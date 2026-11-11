package main_test

// Integration tests: build the gh-reap binary and run it against throwaway
// repositories under a temp dir. GitHub is a local server: with GH_HOST set to
// github.localhost, go-gh talks plain HTTP to api.github.localhost, and HTTP_PROXY
// routes that to the test server. Nothing in the binary knows it is being tested.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gh-reap-bin")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "gh-reap")
	// With E2E_COVERDIR set, the binary records coverage there (go tool covdata).
	args := []string{"build", "-o", binary}
	if os.Getenv("E2E_COVERDIR") != "" {
		args = append(args, "-cover", "-coverpkg=./...")
	}
	if out, err := exec.Command("go", append(args, ".")...).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	// Removal needs config-based hooks; without them every worktree is kept.
	if out, err := exec.Command("git", "hook", "list", "--allow-unknown-hook-name", "x").CombinedOutput(); err != nil &&
		!strings.Contains(string(out), "no hooks found") {
		v, _ := exec.Command("git", "--version").Output()
		fmt.Fprintf(os.Stderr, "these tests need a git with config-based hooks (git hook list --allow-unknown-hook-name); found %s%s", v, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.Remove(binary)
	_ = os.Remove(dir)
	os.Exit(code)
}

// pr is what the fake GitHub says about a branch.
type pr struct {
	Number  int
	State   string
	HeadOID string
	Head    string // head repository, OWNER/REPO; empty means acme/widgets
}

type fakeGitHub struct {
	mu       sync.Mutex
	prs      map[string][]pr // newest first
	requests atomic.Int32
	fail     atomic.Bool
}

func (f *fakeGitHub) set(branch string, p pr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs[branch] = []pr{p}
}

// newer adds a PR from branch, newer than the ones already there.
func (f *fakeGitHub) newer(branch string, p pr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs[branch] = append([]pr{p}, f.prs[branch]...)
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if r.URL.Path != "/graphql" || r.Host != "api.github.localhost" {
		http.Error(w, "unexpected "+r.Host+r.URL.Path, http.StatusNotFound)
		return
	}
	if f.fail.Load() {
		http.Error(w, "boom", http.StatusBadGateway)
		return
	}
	var body struct {
		Query     string
		Variables map[string]any
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	repo := map[string]any{"defaultBranchRef": map[string]string{"name": "main"}}
	for k, v := range body.Variables {
		if !strings.HasPrefix(k, "b") {
			continue
		}
		nodes := []any{}
		for _, p := range f.prs[v.(string)] {
			head := p.Head
			if head == "" {
				head = "acme/widgets"
			}
			nodes = append(nodes, map[string]any{
				"number": p.Number, "state": p.State, "headRefOid": p.HeadOID,
				"url":            fmt.Sprintf("https://github.localhost/acme/widgets/pull/%d", p.Number),
				"headRepository": map[string]string{"nameWithOwner": head},
			})
		}
		repo[k] = map[string]any{"nodes": nodes}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": repo}})
}

// env is one test's world: a fake GitHub, a bare "origin", and its clone.
type env struct {
	t      *testing.T
	root   string // temp dir holding everything
	origin string
	main   string
	gh     *fakeGitHub
	srv    *httptest.Server
	path   string // PATH for the binary
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, root: root, gh: &fakeGitHub{prs: map[string][]pr{}}, path: os.Getenv("PATH")}
	e.srv = httptest.NewServer(e.gh)
	t.Cleanup(e.srv.Close)
	if err := os.WriteFile(filepath.Join(root, "gitconfig"), []byte("[init]\n\tdefaultBranch = main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.origin = filepath.Join(root, "origin.git")
	e.main = filepath.Join(root, "widgets")
	e.git(root, "init", "-q", "--bare", e.origin)
	e.git(root, "init", "-q", e.main)
	e.git(e.main, "remote", "add", "origin", "https://github.localhost/acme/widgets.git")
	e.git(e.main, "config", "url."+e.origin+".insteadOf", "https://github.localhost/acme/widgets.git")
	e.commit(e.main, "README", "hello\n")
	e.git(e.main, "push", "-q", "-u", "origin", "main")
	e.git(e.main, "remote", "set-head", "origin", "main")
	return e
}

func (e *env) environ() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "GH_HOST", "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_REPO",
			"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "GH_CONFIG_DIR", "PATH", "GIT_DIR", "GIT_WORK_TREE", "GOCOVERDIR",
			"NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "GH_FORCE_TTY":
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GH_HOST=github.localhost", "GH_TOKEN=test-token", "GH_CONFIG_DIR="+filepath.Join(e.root, "ghconfig"),
		"HTTP_PROXY="+e.srv.URL, "http_proxy="+e.srv.URL, "NO_PROXY=", "no_proxy=",
		"PATH="+e.path,
		"GIT_CONFIG_GLOBAL="+filepath.Join(e.root, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GOCOVERDIR="+os.Getenv("E2E_COVERDIR"),
	)
}

func (e *env) git(dir string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = e.environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (e *env) commit(dir, file, content string) string {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.git(dir, "add", file)
	e.git(dir, "commit", "-q", "-m", "change "+file)
	return e.git(dir, "rev-parse", "HEAD")
}

// worktree adds a worktree on a new branch with one pushed commit, and returns its path and tip.
func (e *env) worktree(branch string) (string, string) {
	e.t.Helper()
	path := filepath.Join(e.root, "wt-"+branch)
	e.git(e.main, "worktree", "add", "-q", "-b", branch, path, "main")
	tip := e.commit(path, branch+".txt", branch+"\n")
	e.git(path, "push", "-q", "origin", branch)
	return path, tip
}

// branch adds a branch (no worktree) with one pushed commit and returns its tip.
func (e *env) branch(name string) string {
	e.t.Helper()
	path, tip := e.worktree(name)
	e.git(e.main, "worktree", "remove", path)
	return tip
}

type run struct {
	code   int
	stdout string
	stderr string
}

func (e *env) run(dir string, args ...string) run {
	e.t.Helper()
	return e.runEnv(dir, nil, args...)
}

func (e *env) runEnv(dir string, extra []string, args ...string) run {
	e.t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = append(e.environ(), extra...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return run{code, stdout.String(), stderr.String()}
}

// inTerminal runs gh reap under a pseudo-terminal (script(1)) and, like a
// person, types answer once the [y/N] prompt is on screen.
func (e *env) inTerminal(dir, answer string, args ...string) run {
	e.t.Helper()
	command := append([]string{binary}, args...)
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("script", append([]string{"-q", "/dev/null"}, command...)...)
	} else {
		cmd = exec.Command("script", "-qec", strings.Join(command, " "), "/dev/null")
	}
	cmd.Dir = dir
	cmd.Env = e.environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		e.t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		e.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	var out strings.Builder
	buf := make([]byte, 4096)
	typed := false
	for {
		n, rerr := stdout.Read(buf)
		out.Write(buf[:n])
		if !typed && strings.Contains(out.String(), "[y/N] ") {
			_, _ = io.WriteString(stdin, answer)
			typed = true
		}
		if rerr != nil {
			break
		}
	}
	_ = stdin.Close()
	err = cmd.Wait()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return run{code, strings.ReplaceAll(out.String(), "\r\n", "\n"), ""}
}

type item struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail"`
	Branch  string `json:"branch"`
	OID     string `json:"oid"`
	Path    string `json:"path"`
	PR      *struct {
		Number  int    `json:"number"`
		State   string `json:"state"`
		HeadOID string `json:"head_oid"`
		URL     string `json:"url"`
	} `json:"pr"`
	Hook   *bool  `json:"hook"`
	SizeKB *int64 `json:"size_kb"`
}

type listing struct {
	Repos []struct {
		Path     string   `json:"path"`
		GitHub   string   `json:"github"`
		Complete bool     `json:"complete"`
		Problems []string `json:"problems"`
		Items    []item   `json:"items"`
	} `json:"repos"`
}

func (e *env) list(dir string) (listing, run) {
	e.t.Helper()
	r := e.run(dir, "--json")
	var l listing
	if err := json.Unmarshal([]byte(r.stdout), &l); err != nil {
		e.t.Fatalf("list --json: %v\nstdout: %s\nstderr: %s", err, r.stdout, r.stderr)
	}
	return l, r
}

func byName(l listing) map[string]item {
	m := map[string]item{}
	for _, repo := range l.Repos {
		for _, it := range repo.Items {
			key := it.Branch
			if it.Kind == "worktree" && (it.Branch == "" || it.Reason == "folder-gone") {
				key = it.Path
			}
			m[key] = it
		}
	}
	return m
}

type report struct {
	Repo     string   `json:"repo"`
	Problems []string `json:"problems"`
	Results  []struct {
		Ref    string `json:"ref"`
		Kind   string `json:"kind"`
		Branch string `json:"branch"`
		Path   string `json:"path"`
		Result string `json:"result"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	} `json:"results"`
}

func (e *env) remove(dir string, args ...string) (report, run) {
	e.t.Helper()
	r := e.run(dir, append([]string{"remove", "--json", "--yes"}, args...)...)
	var rep report
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		e.t.Fatalf("remove --json: %v\nstdout: %s\nstderr: %s", err, r.stdout, r.stderr)
	}
	return rep, r
}

func (e *env) exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (e *env) hasBranch(name string) bool {
	cmd := exec.Command("git", "-C", e.main, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	cmd.Env = e.environ()
	return cmd.Run() == nil
}

// hook registers a pre-worktree-remove hook running script (sh) through git config.
func (e *env) hook(script string) {
	e.t.Helper()
	path := filepath.Join(e.root, "teardown.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		e.t.Fatal(err)
	}
	e.git(e.main, "config", "hook.teardown.command", path)
	e.git(e.main, "config", "hook.teardown.event", "pre-worktree-remove")
}

func expect(t *testing.T, it item, verdict, reason string) {
	t.Helper()
	if it.Verdict != verdict || it.Reason != reason {
		t.Errorf("%s %s: got %s %s (%s), want %s %s", it.Kind, it.Branch, it.Verdict, it.Reason, it.Detail, verdict, reason)
	}
}

func TestE2E_ListEveryRule(t *testing.T) {
	e := newEnv(t)

	_, mergedTip := e.worktree("merged")
	e.gh.set("merged", pr{1, "MERGED", mergedTip, ""})

	squashTip := e.branch("squashed")
	e.gh.set("squashed", pr{2, "MERGED", squashTip, ""})

	_, openTip := e.worktree("open")
	e.gh.set("open", pr{3, "OPEN", openTip, ""})

	dirtyPath, dirtyTip := e.worktree("dirty")
	e.gh.set("dirty", pr{4, "MERGED", dirtyTip, ""})
	if err := os.WriteFile(filepath.Join(dirtyPath, "scratch.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}

	unpushedPath, unpushedHead := e.worktree("unpushed")
	e.gh.set("unpushed", pr{5, "MERGED", unpushedHead, ""})
	e.commit(unpushedPath, "more.txt", "not pushed\n")

	closedTip := e.branch("closed")
	e.gh.set("closed", pr{6, "CLOSED", closedTip, ""})

	keptTip := e.branch("kept")
	e.gh.set("kept", pr{7, "MERGED", keptTip, ""})
	if r := e.run(e.main, "keep", "kept"); r.code != 0 {
		t.Fatalf("keep: %+v", r)
	}

	lockedPath, lockedTip := e.worktree("locked")
	e.gh.set("locked", pr{8, "MERGED", lockedTip, ""})
	e.git(e.main, "worktree", "lock", "--reason", "pinned", lockedPath)

	busyPath, busyTip := e.worktree("busy")
	e.gh.set("busy", pr{9, "MERGED", busyTip, ""})
	sleeper := exec.Command("sleep", "60")
	sleeper.Dir = busyPath
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })

	gonePath, goneTip := e.worktree("gone")
	e.gh.set("gone", pr{10, "OPEN", goneTip, ""})
	if err := os.Rename(gonePath, gonePath+"-moved"); err != nil {
		t.Fatal(err)
	}

	e.branch("nopr")

	missingTip := e.branch("headmissing")
	e.gh.set("headmissing", pr{11, "MERGED", strings.Repeat("ab", 20), ""})
	_ = missingTip

	behindPath, behindOld := e.worktree("behind")
	behindNew := e.commit(behindPath, "later.txt", "pushed later\n")
	e.git(behindPath, "push", "-q", "origin", "behind")
	e.git(behindPath, "reset", "-q", "--hard", behindOld)
	e.gh.set("behind", pr{12, "MERGED", behindNew, ""})

	divergedPath, _ := e.worktree("diverged")
	other := e.commit(divergedPath, "other.txt", "on a remote but not in the PR\n")
	e.git(divergedPath, "push", "-q", "origin", "diverged:refs/heads/elsewhere")
	e.git(divergedPath, "reset", "-q", "--hard", "HEAD~1")
	prHead := e.commit(divergedPath, "pr.txt", "the PR's own commit\n")
	e.git(divergedPath, "push", "-q", "-f", "origin", "diverged")
	e.git(divergedPath, "reset", "-q", "--hard", other)
	e.gh.set("diverged", pr{13, "MERGED", prHead, ""})

	l, r := e.list(e.main)
	if r.code != 0 {
		t.Fatalf("list exit %d: %s", r.code, r.stderr)
	}
	if got := e.gh.requests.Load(); got != 1 {
		t.Errorf("GitHub requests: got %d, want 1 batched query", got)
	}
	if l.Repos[0].GitHub != "acme/widgets" {
		t.Errorf("github: got %q", l.Repos[0].GitHub)
	}
	items := byName(l)
	expect(t, items["main"], "KEEP", "main-checkout")
	expect(t, items["merged"], "REMOVE", "pr-merged")
	expect(t, items["squashed"], "REMOVE", "pr-merged")
	expect(t, items["open"], "KEEP", "pr-open")
	expect(t, items["dirty"], "KEEP", "dirty")
	expect(t, items["unpushed"], "KEEP", "unpushed-commits")
	expect(t, items["closed"], "KEEP", "pr-closed")
	expect(t, items["kept"], "KEEP", "marked-keep")
	expect(t, items["locked"], "KEEP", "locked")
	expect(t, items["busy"], "KEEP", "in-use")
	expect(t, items[gonePath], "REMOVE", "folder-gone")
	expect(t, items["gone"], "KEEP", "pr-open") // its branch, judged alone
	expect(t, items["nopr"], "KEEP", "no-pr")
	expect(t, items["headmissing"], "KEEP", "pr-head-missing")
	expect(t, items["behind"], "REMOVE", "pr-merged")
	expect(t, items["diverged"], "KEEP", "not-on-pr-head")
	if !strings.Contains(items["busy"].Detail, "sleep") {
		t.Errorf("busy detail should name the process: %q", items["busy"].Detail)
	}
	if it := items["merged"]; it.Hook == nil || *it.Hook || it.SizeKB == nil || it.PR == nil || it.PR.Number != 1 {
		t.Errorf("merged item fields: %+v", it)
	}
	if it := items["merged"]; it.Ref != "worktree:"+filepath.Join(e.root, "wt-merged") {
		t.Errorf("ref: %q", it.Ref)
	}

	// The same list from inside a worktree.
	l, _ = e.list(filepath.Join(e.root, "wt-open"))
	if got := byName(l)["merged"]; got.Verdict != "REMOVE" || l.Repos[0].Path != e.main {
		t.Errorf("from a worktree: path %q, merged %s", l.Repos[0].Path, got.Verdict)
	}
	// ...where the tree you stand in is in use (by this test's process? no: by nobody
	// but gh reap itself, which does not count), so it is judged on its PR alone.
	expect(t, byName(l)["open"], "KEEP", "pr-open")

	// The text list: what can go first, the rest counted; --all lists the rest.
	r = e.run(e.main)
	for _, want := range []string{"To remove:", "merged", "squashed", "Not shown:", "Remove with: gh reap remove"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("list lacks %q:\n%s", want, r.stdout)
		}
	}
	if r = e.run(e.main, "--all"); !strings.Contains(r.stdout, "PR #3 open") {
		t.Errorf("--all lacks the open PR:\n%s", r.stdout)
	}
}

func TestE2E_RemoveAll(t *testing.T) {
	e := newEnv(t)
	mergedPath, mergedTip := e.worktree("merged")
	e.gh.set("merged", pr{1, "MERGED", mergedTip, ""})
	squashTip := e.branch("squashed") // never merged in git: needs -D
	e.gh.set("squashed", pr{2, "MERGED", squashTip, ""})
	openPath, openTip := e.worktree("open")
	e.gh.set("open", pr{3, "OPEN", openTip, ""})
	closedTip := e.branch("closed")
	e.gh.set("closed", pr{4, "CLOSED", closedTip, ""})
	gonePath, _ := e.worktree("gone")
	if err := os.Rename(gonePath, gonePath+"-moved"); err != nil {
		t.Fatal(err)
	}

	rep, r := e.remove(e.main)
	if r.code != 0 {
		t.Fatalf("remove exit %d: %+v %s", r.code, rep, r.stderr)
	}
	for _, res := range rep.Results {
		if res.Result != "removed" {
			t.Errorf("%s: %s %s", res.Ref, res.Result, res.Detail)
		}
	}
	if e.exists(mergedPath) || e.hasBranch("merged") || e.hasBranch("squashed") {
		t.Error("merged worktree or branches still there")
	}
	if !e.exists(openPath) || !e.hasBranch("open") || !e.hasBranch("closed") {
		t.Error("removed something it should keep")
	}
	if strings.Contains(e.git(e.main, "worktree", "list", "--porcelain"), gonePath+"\n") {
		t.Error("registration of the gone folder is still there")
	}
	if e.exists(filepath.Join(e.main, ".git", "gh-reap.lock")) {
		t.Error("lock left behind")
	}
}

func TestE2E_RemoveByRef(t *testing.T) {
	e := newEnv(t)
	aPath, aTip := e.worktree("a")
	e.gh.set("a", pr{1, "MERGED", aTip, ""})
	bTip := e.branch("b")
	e.gh.set("b", pr{2, "MERGED", bTip, ""})
	_, openTip := e.worktree("open")
	e.gh.set("open", pr{3, "OPEN", openTip, ""})

	// A KEEP item and an unknown ref are refused; the named REMOVE item goes.
	rep, r := e.remove(e.main, "open", "nosuch", aPath)
	if r.code != 1 {
		t.Errorf("exit %d, want 1", r.code)
	}
	got := map[string]string{}
	for _, res := range rep.Results {
		got[res.Ref] = res.Result + " " + res.Reason
	}
	want := map[string]string{
		"worktree:" + filepath.Join(e.root, "wt-open"): "refused pr-open",
		"nosuch":            "refused not-found",
		"worktree:" + aPath: "removed pr-merged",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
	if e.exists(aPath) || e.hasBranch("a") {
		t.Error("a still there")
	}
	// A branch item by its listed ref, flags after the ref.
	rep, r = e.remove(e.main, "branch:b", "--json")
	if r.code != 0 || rep.Results[0].Result != "removed" || e.hasBranch("b") {
		t.Errorf("branch:b: exit %d %+v", r.code, rep)
	}
}

// A branch whose worktree folder was deleted by hand goes by name in one run:
// the stale registration, then the branch.
func TestE2E_RemoveBranchWhoseFolderIsGone(t *testing.T) {
	e := newEnv(t)
	path, tip := e.worktree("gone")
	e.gh.set("gone", pr{1, "MERGED", tip, ""})
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	rep, r := e.remove(e.main, "gone")
	if r.code != 0 || len(rep.Results) != 2 {
		t.Fatalf("exit %d: %+v\n%s", r.code, rep.Results, r.stderr)
	}
	for _, res := range rep.Results {
		if res.Result != "removed" {
			t.Errorf("%s: %s %s (%s)", res.Ref, res.Result, res.Reason, res.Detail)
		}
	}
	if e.hasBranch("gone") || strings.Contains(e.git(e.main, "worktree", "list"), "wt-gone") {
		t.Error("the branch or the registration is still there")
	}
}

func TestE2E_RevalidatesBeforeActing(t *testing.T) {
	e := newEnv(t)
	path, tip := e.worktree("moving")
	e.gh.set("moving", pr{1, "MERGED", tip, ""})
	// The hook makes the tree dirty: the check after the hook keeps it.
	e.hook("echo late > \"$1/late.txt\"\n")
	rep, r := e.remove(e.main, "moving")
	if r.code != 1 || rep.Results[0].Reason != "dirty" || !strings.Contains(rep.Results[0].Detail, "changed by teardown hook") || !e.exists(path) {
		t.Fatalf("got exit %d %+v", r.code, rep)
	}
	// Now the tree is dirty: the fresh list refuses before anything runs.
	rep, _ = e.remove(e.main, "moving")
	if rep.Results[0].Result != "refused" || rep.Results[0].Reason != "dirty" {
		t.Errorf("dirty: %+v", rep.Results[0])
	}
}

func TestE2E_Hook(t *testing.T) {
	e := newEnv(t)
	log := filepath.Join(e.root, "hook.log")
	okPath, okTip := e.worktree("ok")
	e.gh.set("ok", pr{1, "MERGED", okTip, ""})
	failPath, failTip := e.worktree("fails")
	e.gh.set("fails", pr{2, "MERGED", failTip, ""})
	e.hook(`echo "ran in $(pwd -P) for $1" >> ` + log + `
case "$1" in *fails) echo "first line"; echo "database still attached" >&2; exit 3;; esac
`)

	l, _ := e.list(e.main)
	if h := byName(l)["ok"].Hook; h == nil || !*h {
		t.Errorf("hook should show yes: %+v", byName(l)["ok"])
	}
	rep, r := e.remove(e.main)
	if r.code != 1 {
		t.Errorf("exit %d, want 1 (one hook failed)", r.code)
	}
	res := map[string]string{}
	for _, x := range rep.Results {
		res[x.Branch] = x.Result + " " + x.Reason + " " + x.Detail
	}
	if !strings.HasPrefix(res["ok"], "removed") {
		t.Errorf("ok: %s", res["ok"])
	}
	if !strings.HasPrefix(res["fails"], "failed hook-failed") || !strings.Contains(res["fails"], "database still attached") {
		t.Errorf("fails: %s", res["fails"])
	}
	if e.exists(okPath) || !e.exists(failPath) || !e.hasBranch("fails") {
		t.Error("wrong trees removed")
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "ran in "+okPath+" for "+okPath) {
		t.Errorf("hook should run inside the tree with its path: %s", data)
	}
}

func TestE2E_HookLeavesAProcessRunning(t *testing.T) {
	e := newEnv(t)
	path, tip := e.worktree("server")
	e.gh.set("server", pr{1, "MERGED", tip, ""})
	pidFile := filepath.Join(e.root, "server.pid")
	e.hook("cd \"$1\" && (sleep 60 </dev/null >/dev/null 2>&1 & echo $! > " + pidFile + ")\n")
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			_ = exec.Command("kill", strings.TrimSpace(string(data))).Run()
		}
	})
	rep, r := e.remove(e.main, "server")
	if r.code != 1 || rep.Results[0].Reason != "in-use" || !strings.Contains(rep.Results[0].Detail, "sleep") || !e.exists(path) {
		t.Errorf("got exit %d %+v", r.code, rep)
	}
}

func TestE2E_HookCommitsOnADetachedHead(t *testing.T) {
	e := newEnv(t)
	path, tip := e.worktree("topic")
	e.gh.set("topic", pr{1, "MERGED", tip, ""})
	// The hook saves work as a commit on a detached HEAD: only the worktree's
	// HEAD would know it, so removing the tree would lose the commit.
	e.hook("cd \"$1\" && git switch -q --detach && git -c user.name=t -c user.email=t@t commit -q --allow-empty -m saved\n")
	rep, r := e.remove(e.main, "topic")
	if r.code != 1 || rep.Results[0].Reason != "detached" || !e.exists(path) || !e.hasBranch("topic") {
		t.Errorf("got exit %d %+v", r.code, rep)
	}
}

func TestE2E_WorktreeInsideAnother(t *testing.T) {
	e := newEnv(t)
	outer, tip := e.worktree("outer")
	e.gh.set("outer", pr{1, "MERGED", tip, ""})
	// The outer tree ignores .worktrees/, so git status sees the inner tree as
	// nothing, and git worktree remove would delete it with its work.
	e.commit(outer, ".gitignore", ".worktrees/\n")
	e.git(outer, "push", "-q", "origin", "outer")
	tip = e.git(outer, "rev-parse", "HEAD")
	e.gh.set("outer", pr{1, "MERGED", tip, ""})
	inner := filepath.Join(outer, ".worktrees", "inner")
	e.git(e.main, "worktree", "add", "-q", "-b", "inner", inner, "main")
	wip := filepath.Join(inner, "wip.txt")
	if err := os.WriteFile(wip, []byte("unsaved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, _ := e.list(e.main)
	expect(t, byName(l)["outer"], "KEEP", "contains-worktree")
	e.remove(e.main)
	if !e.exists(outer) || !e.exists(wip) {
		t.Error("removing the outer tree took the inner one")
	}
}

func TestE2E_HookLeavesItsOutputOpen(t *testing.T) {
	e := newEnv(t)
	path, tip := e.worktree("daemon")
	e.gh.set("daemon", pr{1, "MERGED", tip, ""})
	// A background process outside the tree that keeps the hook's output open:
	// gh reap must not wait for it to exit.
	pidFile := filepath.Join(e.root, "daemon.pid")
	e.hook("cd / && sleep 60 & echo $! > " + pidFile + "\n")
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			_ = exec.Command("kill", strings.TrimSpace(string(data))).Run()
		}
	})
	start := time.Now()
	rep, r := e.remove(e.main, "daemon")
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("waited %v for the hook's leftover process", took)
	}
	if r.code != 1 || rep.Results[0].Reason != "hook-failed" || !strings.Contains(rep.Results[0].Detail, "holds its output") || !e.exists(path) {
		t.Errorf("got exit %d %+v", r.code, rep)
	}
}

func TestE2E_ForkPRWithTheSameBranchName(t *testing.T) {
	e := newEnv(t)
	_, tip := e.worktree("patch-1")
	e.gh.set("patch-1", pr{1, "OPEN", tip, ""})
	// Someone else's fork opened, and got merged, a newer PR from their own
	// patch-1 that happens to contain this tip. It is not this branch's PR.
	e.gh.newer("patch-1", pr{2, "MERGED", tip, "stranger/widgets"})
	l, _ := e.list(e.main)
	expect(t, byName(l)["patch-1"], "KEEP", "pr-open")
	// From a fork that is one of our remotes, the PR is ours.
	e.git(e.main, "remote", "add", "fork", "https://github.localhost/stranger/widgets.git")
	l, _ = e.list(e.main)
	expect(t, byName(l)["patch-1"], "REMOVE", "pr-merged")
}

func TestE2E_RemoveAsks(t *testing.T) {
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("no script(1) to give gh reap a terminal")
	}
	e := newEnv(t)
	path, tip := e.worktree("done")
	e.gh.set("done", pr{1, "MERGED", tip, ""})

	r := e.inTerminal(e.main, "n\n", "remove")
	if !strings.Contains(r.stdout, "\x1b[1mTo remove:\x1b[0m") {
		t.Errorf("a terminal gets color:\n%q", r.stdout)
	}
	shown := ansiCode.ReplaceAllString(r.stdout, "")
	if r.code != 0 || !strings.Contains(shown, "Remove 1 worktree and 1 branch? [y/N]") || !strings.Contains(shown, "Canceled.") {
		t.Errorf("declined: exit %d\n%q", r.code, r.stdout)
	}
	if !e.exists(path) || !e.hasBranch("done") {
		t.Fatal("declined, but removed")
	}

	r = e.inTerminal(e.main, "y\n", "remove")
	shown = ansiCode.ReplaceAllString(r.stdout, "")
	if r.code != 0 || !strings.Contains(shown, "✓ Removed worktree") || !strings.Contains(shown, "✓ Deleted local branch done") || !strings.Contains(shown, "1 removed.") {
		t.Errorf("accepted: exit %d\n%q", r.code, r.stdout)
	}
	if e.exists(path) || e.hasBranch("done") {
		t.Error("accepted, but still there")
	}
}

func TestE2E_FlagsAfterNames(t *testing.T) {
	e := newEnv(t)
	tip := e.branch("done")
	e.gh.set("done", pr{1, "MERGED", tip, ""})

	// Flags may follow names, as in gh's own commands.
	if r := e.run(e.main, "remove", "done", "--yes", "--json"); r.code != 0 || !strings.Contains(r.stdout, `"result": "removed"`) {
		t.Errorf("flags after a name: %+v", r)
	}
	if e.hasBranch("done") {
		t.Error("still there")
	}
	// After "--", what looks like a flag is a name.
	if r := e.run(e.main, "keep", "--", "--help"); r.code != 1 || !strings.Contains(r.stderr, `no local branch "--help"`) {
		t.Errorf("after --: %+v", r)
	}
}

var ansiCode = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestE2E_Color(t *testing.T) {
	e := newEnv(t)
	tip := e.branch("done")
	e.gh.set("done", pr{1, "MERGED", tip, ""})

	piped := e.run(e.main)
	if strings.Contains(piped.stdout, "\x1b[") || !strings.Contains(piped.stdout, "To remove:") {
		t.Errorf("piped output is plain: %q", piped.stdout)
	}
	forced := e.runEnv(e.main, []string{"CLICOLOR_FORCE=1"})
	if !strings.Contains(forced.stdout, "\x1b[1mTo remove:\x1b[0m") {
		t.Errorf("CLICOLOR_FORCE colors a pipe: %q", forced.stdout)
	}
	if got := ansiCode.ReplaceAllString(forced.stdout, ""); got != piped.stdout {
		t.Errorf("color changed the text:\n%q\n%q", got, piped.stdout)
	}
	if r := e.runEnv(e.main, []string{"CLICOLOR_FORCE=1"}, "--json"); strings.Contains(r.stdout, "\x1b[") {
		t.Errorf("JSON is never colored: %q", r.stdout)
	}
}

func TestE2E_KeepAndUnkeep(t *testing.T) {
	e := newEnv(t)
	path, tip := e.worktree("precious")
	e.gh.set("precious", pr{1, "MERGED", tip, ""})
	if r := e.run(e.main, "keep", "precious"); r.code != 0 || !strings.Contains(r.stdout, "marked keep") {
		t.Fatalf("keep: %+v", r)
	}
	if v := e.git(e.main, "config", "branch.precious.reap-keep"); v != "true" {
		t.Errorf("config: %q", v)
	}
	l, _ := e.list(e.main)
	expect(t, byName(l)["precious"], "KEEP", "marked-keep")
	rep, r := e.remove(e.main)
	if r.code != 0 || len(rep.Results) != 0 || !e.exists(path) {
		t.Errorf("remove --all touched a kept tree: %+v", rep)
	}
	if r := e.run(e.main, "unkeep", "precious"); r.code != 0 {
		t.Fatalf("unkeep: %+v", r)
	}
	l, _ = e.list(e.main)
	expect(t, byName(l)["precious"], "REMOVE", "pr-merged")
	if r := e.run(e.main, "keep", "no-such-branch"); r.code != 1 {
		t.Errorf("keep of a missing branch: exit %d", r.code)
	}
	// A revision is not a branch: keep must not report success for it.
	if r := e.run(e.main, "keep", "precious~1"); r.code != 1 {
		t.Errorf("keep precious~1: exit %d %s", r.code, r.stdout)
	}
}

// waitFor polls until the file exists.
func (e *env) waitFor(path, what string) {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !e.exists(path) {
		if time.Now().After(deadline) {
			e.t.Fatalf("%s never happened", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A run that is killed outright cannot clean up, and still locks nobody out:
// the kernel drops its lock, and the file it leaves means nothing.
func TestE2E_KilledRunLeavesNoLock(t *testing.T) {
	e := newEnv(t)
	path, tip := e.worktree("a")
	e.gh.set("a", pr{1, "MERGED", tip, ""})
	started := filepath.Join(e.root, "started")
	release := filepath.Join(e.root, "release")
	e.hook("touch " + started + "\nwhile [ ! -e " + release + " ]; do sleep 0.05; done\n")

	first := exec.Command(binary, "remove", "--yes")
	first.Dir = e.main
	first.Env = e.environ()
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	e.waitFor(started, "the first run's hook")
	_ = first.Process.Kill()
	_ = first.Wait()
	lock := filepath.Join(e.main, ".git", "gh-reap.lock")
	if !e.exists(lock) {
		t.Fatal("the killed run left no lock file: this test proves nothing")
	}
	// The hook it started outlives it, and must not hold the lock either.
	if r := e.run(e.main, "remove", "--yes"); strings.Contains(r.stderr, "lock") {
		t.Errorf("locked out by a dead run: %+v", r)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.hook("exit 0\n")
	deadline := time.Now().Add(30 * time.Second)
	// Until the orphaned hook has left the tree, the tree is in use and a run
	// finds nothing to remove, which is no failure: wait for the removal itself.
	for e.exists(path) {
		r := e.run(e.main, "remove", "--yes")
		if strings.Contains(r.stderr, "lock") {
			t.Fatalf("locked out by a dead run: %+v", r)
		}
		if time.Now().After(deadline) {
			t.Fatalf("never removed after the kill: %+v", r)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if e.hasBranch("a") || e.exists(lock) {
		t.Error("the run after the kill left the branch or the lock file")
	}
	// list never takes the lock.
	if _, r := e.list(e.main); r.code != 0 {
		t.Errorf("list: %+v", r)
	}
}

// Ctrl-C at the question answers no at once, without waiting for Enter.
func TestE2E_InterruptAtThePrompt(t *testing.T) {
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("no script(1) to give gh reap a terminal")
	}
	e := newEnv(t)
	path, tip := e.worktree("done")
	e.gh.set("done", pr{1, "MERGED", tip, ""})

	finished := make(chan run, 1)
	go func() { finished <- e.inTerminal(e.main, "\x03", "remove") }()
	select {
	case r := <-finished:
		if r.code != 130 || strings.Contains(r.stdout, "removed") {
			t.Errorf("exit %d\n%q", r.code, r.stdout)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("still waiting for an answer after Ctrl-C")
	}
	if !e.exists(path) || !e.hasBranch("done") {
		t.Error("interrupted, but removed")
	}
	if e.exists(filepath.Join(e.main, ".git", "gh-reap.lock")) {
		t.Error("interrupted, and left the lock file")
	}
}

// An interrupt while listing stops quietly: no list built from killed commands.
func TestE2E_InterruptWhileListing(t *testing.T) {
	e := newEnv(t)
	path, tip := e.worktree("done")
	e.gh.set("done", pr{1, "MERGED", tip, ""})
	bin := filepath.Join(e.root, "bin")
	started := filepath.Join(e.root, "lsof-started")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "lsof"), []byte("#!/bin/sh\ntouch "+started+"\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.path = bin + string(os.PathListSeparator) + e.path

	for _, args := range [][]string{{}, {"--json"}, {"remove", "--yes"}} {
		_ = os.Remove(started)
		cmd := exec.Command(binary, args...)
		cmd.Dir = e.main
		cmd.Env = e.environ()
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		e.waitFor(started, "lsof")
		_ = cmd.Process.Signal(os.Interrupt)
		err := cmd.Wait()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 130 || stdout.Len() > 0 || stderr.Len() > 0 {
			t.Errorf("%v: %v\nstdout: %s\nstderr: %s", args, err, stdout.String(), stderr.String())
		}
	}
	if !e.exists(path) || !e.hasBranch("done") || e.exists(filepath.Join(e.main, ".git", "gh-reap.lock")) {
		t.Error("interrupted, but something was removed or the lock file left")
	}
}

func TestE2E_ConcurrentRunsRefused(t *testing.T) {
	e := newEnv(t)
	_, tip := e.worktree("slow")
	e.gh.set("slow", pr{1, "MERGED", tip, ""})
	started := filepath.Join(e.root, "started")
	release := filepath.Join(e.root, "release")
	e.hook("touch " + started + "\nwhile [ ! -e " + release + " ]; do sleep 0.05; done\n")

	first := exec.Command(binary, "remove", "--yes")
	first.Dir = e.main
	first.Env = e.environ()
	var out strings.Builder
	first.Stdout, first.Stderr = &out, &out
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !e.exists(started) {
		if time.Now().After(deadline) {
			t.Fatal("first run never reached its hook")
		}
		time.Sleep(20 * time.Millisecond)
	}
	second := e.run(e.main, "remove", "--yes")
	// From a worktree, the lock is the same one.
	third := e.run(filepath.Join(e.root, "wt-slow"), "remove", "--yes")
	_ = os.WriteFile(release, nil, 0o644)
	err := first.Wait()
	lock := filepath.Join(e.main, ".git", "gh-reap.lock")
	for _, r := range []run{second, third} {
		if r.code != 1 || !strings.Contains(r.stderr, "another gh reap run holds the lock: "+lock) || !strings.Contains(r.stderr, "wait for it to finish") {
			t.Errorf("run during another: %+v", r)
		}
	}
	if err != nil {
		t.Errorf("first run: %v\n%s", err, out.String())
	}
	if e.hasBranch("slow") {
		t.Error("first run did not finish its removal")
	}
}

func TestE2E_IncompleteInventory(t *testing.T) {
	e := newEnv(t)
	_, tip := e.worktree("a")
	e.gh.set("a", pr{1, "MERGED", tip, ""})
	squash := e.branch("b")
	e.gh.set("b", pr{2, "MERGED", squash, ""})

	t.Run("no lsof", func(t *testing.T) {
		bin := filepath.Join(e.root, "bin")
		_ = os.MkdirAll(bin, 0o755)
		gitPath, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Symlink(gitPath, filepath.Join(bin, "git"))
		old := e.path
		e.path = bin
		defer func() { e.path = old }()
		l, r := e.list(e.main)
		if r.code != 1 || l.Repos[0].Complete {
			t.Errorf("exit %d complete %v", r.code, l.Repos[0].Complete)
		}
		for _, it := range l.Repos[0].Items {
			if it.Verdict == "REMOVE" {
				t.Errorf("%s REMOVE without lsof", it.Ref)
			}
		}
		expect(t, byName(l)["b"], "KEEP", "inventory-incomplete")
		rep, r := e.remove(e.main)
		if r.code != 1 || len(rep.Results) != 0 || !e.hasBranch("b") {
			t.Errorf("remove without lsof: exit %d %+v", r.code, rep)
		}
	})

	t.Run("GitHub unreachable", func(t *testing.T) {
		e.gh.fail.Store(true)
		defer e.gh.fail.Store(false)
		l, r := e.list(e.main)
		if r.code != 1 || l.Repos[0].Complete {
			t.Errorf("exit %d", r.code)
		}
		expect(t, byName(l)["a"], "KEEP", "pr-unknown")
		expect(t, byName(l)["b"], "KEEP", "pr-unknown")
	})
}

func TestE2E_GitTooOldForHooks(t *testing.T) {
	e := newEnv(t)
	path, tip := e.worktree("a")
	e.gh.set("a", pr{1, "MERGED", tip, ""})
	squash := e.branch("b")
	e.gh.set("b", pr{2, "MERGED", squash, ""})
	// A git without config-based hooks answers `git hook list` like this (Apple git 2.50).
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(e.root, "oldgit")
	_ = os.MkdirAll(bin, 0o755)
	wrapper := "#!/bin/sh\nif [ \"$3\" = hook ] && [ \"$4\" = list ]; then echo \"error: unknown subcommand: \\`list'\" >&2; exit 129; fi\nexec " + gitPath + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	e.path = bin + string(os.PathListSeparator) + e.path

	l, r := e.list(e.main)
	if r.code != 0 {
		t.Errorf("exit %d: %s", r.code, r.stderr)
	}
	expect(t, byName(l)["a"], "KEEP", "git-too-old-for-hooks")
	if h := byName(l)["a"].Hook; h != nil {
		t.Errorf("hook should be unknown, not %v", *h)
	}
	expect(t, byName(l)["b"], "REMOVE", "pr-merged") // a branch needs no teardown
	rep, _ := e.remove(e.main, "a")
	if rep.Results[0].Result != "refused" || !e.exists(path) {
		t.Errorf("removed a tree without its teardown: %+v", rep)
	}
}

func TestE2E_FolderOfRepos(t *testing.T) {
	e := newEnv(t)
	_, tip := e.worktree("a")
	e.gh.set("a", pr{1, "MERGED", tip, ""})
	// A second repository, reached through a symlink, with no GitHub remote:
	// listed, but incomplete. The bare origin.git beside them is listed too.
	other := filepath.Join(e.root, "elsewhere", "other")
	e.git(e.root, "init", "-q", other)
	e.commit(other, "x", "x\n")
	if err := os.Symlink(other, filepath.Join(e.root, "other-link")); err != nil {
		t.Fatal(err)
	}

	l, r := e.list(e.root)
	if len(l.Repos) != 3 {
		t.Fatalf("repos: %+v (stderr %s)", l, r.stderr)
	}
	if r.code != 1 {
		t.Errorf("exit %d, want 1 (one repo has no GitHub remote)", r.code)
	}
	expect(t, byName(l)["a"], "REMOVE", "pr-merged")

	// remove runs in one repository only.
	if r := e.run(e.root, "remove", "--yes"); r.code != 1 {
		t.Errorf("remove outside a repo: %+v", r)
	}
	if r := e.run(e.root); !strings.Contains(r.stdout, "widgets (acme/widgets)") {
		t.Errorf("each repo should have a heading: %s", r.stdout)
	}
}

func TestE2E_SingleChildRepoHint(t *testing.T) {
	e := newEnv(t)
	tip := e.branch("done")
	e.gh.set("done", pr{1, "MERGED", tip, ""})
	parent := filepath.Join(e.root, "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.main, filepath.Join(parent, "only-repo")); err != nil {
		t.Fatal(err)
	}
	r := e.run(parent)
	if r.code != 0 || !strings.Contains(r.stdout, e.main+" (acme/widgets)") || !strings.Contains(r.stdout, "Remove with: cd '"+e.main+"' && gh reap remove") {
		t.Fatalf("single discovered repo needs its own heading and removal directory: %+v", r)
	}
}

func TestE2E_Usage(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{{"bogus"}, {"remove"}, {"list"}, {"--state", "closed"}, {"--nope"}, {"keep"}, {"x"}, {"remove", "--all"}} {
		if r := e.run(e.main, args...); r.code != 2 {
			t.Errorf("%v: exit %d, want 2", args, r.code)
		}
	}
	r := e.run(e.main, "--help")
	if r.code != 0 || !strings.Contains(r.stdout, "gh reap remove [<name>...]") {
		t.Errorf("help: %+v", r)
	}
}
