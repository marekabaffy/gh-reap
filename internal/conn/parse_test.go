package conn

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestParseWorktrees(t *testing.T) {
	got := ParseWorktrees(fixture(t, "worktree_list.bin"))
	want := []Worktree{
		{Path: "$ROOT/repo", HEAD: "d2199bdc0ab884f8bd2a434b3b986ebca248b759", Branch: "main"},
		{Path: "$ROOT/wt-detached", HEAD: "d2199bdc0ab884f8bd2a434b3b986ebca248b759", Detached: true},
		{Path: "$ROOT/wt-dirty", HEAD: "dd9b46c44b3f0e667b4ba127a5fd321ad6903012", Branch: "dirty"},
		{Path: "$ROOT/wt-gone", HEAD: "be02e077081761b4cbb5331b00637c9f6fa03600", Branch: "gone", Prunable: true},
		{Path: "$ROOT/wt-locked", HEAD: "f5cc5ad49a903e1e05c5a07ee591fc9f58583e1d", Branch: "locked", Locked: true, LockReason: "on a removable disk"},
		{Path: "$ROOT/wt-merged", HEAD: "4038e29f7b6045521864de786bc46075e9ad791c", Branch: "merged"},
		{Path: "$ROOT/wt-unpushed", HEAD: "790482555e30fe591ce9a5a492f44d8c0d2385b3", Branch: "unpushed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestParseWorktrees_Cases(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Worktree
	}{
		{"empty", "", nil},
		{"bare main", "worktree /r.git\x00bare\x00\x00", []Worktree{{Path: "/r.git", Bare: true}}},
		{
			"locked without reason", "worktree /a\x00HEAD 1\x00branch refs/heads/x\x00locked\x00\x00",
			[]Worktree{{Path: "/a", HEAD: "1", Branch: "x", Locked: true}},
		},
		{
			"path with spaces and newline", "worktree /a b\nc\x00HEAD 1\x00detached\x00\x00",
			[]Worktree{{Path: "/a b\nc", HEAD: "1", Detached: true}},
		},
		{
			"branch with slashes", "worktree /a\x00HEAD 1\x00branch refs/heads/feat/x-1\x00\x00",
			[]Worktree{{Path: "/a", HEAD: "1", Branch: "feat/x-1"}},
		},
		{"unknown attribute ignored", "worktree /a\x00HEAD 1\x00future thing\x00\x00", []Worktree{{Path: "/a", HEAD: "1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseWorktrees(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseBranches(t *testing.T) {
	got := ParseBranches(fixture(t, "branches.txt"))
	if len(got) != 11 {
		t.Fatalf("got %d branches: %+v", len(got), got)
	}
	if got[0] != (Branch{"dirty", "dd9b46c44b3f0e667b4ba127a5fd321ad6903012"}) {
		t.Errorf("first: %+v", got[0])
	}
	names := map[string]bool{}
	for _, b := range got {
		names[b.Name] = true
		if len(b.OID) != 40 {
			t.Errorf("%s: oid %q", b.Name, b.OID)
		}
	}
	for _, n := range []string{"main", "release.v1", "squashed", "level"} {
		if !names[n] {
			t.Errorf("missing %s", n)
		}
	}
	if ParseBranches("") != nil {
		t.Error("empty output should give no branches")
	}
}

func TestParseKeepMarks(t *testing.T) {
	got := ParseKeepMarks(fixture(t, "keep_marks.txt"))
	want := map[string]bool{"kept": true, "release.v1": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := ParseKeepMarks("branch.x.reap-keep false\nother.y true\n"); len(got) != 0 {
		t.Errorf("false and foreign keys: %v", got)
	}
}

func TestParseRemotes(t *testing.T) {
	got := ParseRemotes(fixture(t, "remotes.txt"))
	want := []Remote{{Name: "origin", URL: "https://github.com/acme/widgets.git", Resolved: "base"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	got = ParseRemotes("remote.my.fork.url git@github.com:me/w.git\nremote.my.fork.url second\nremote.up.url https://x/y/z\n")
	want = []Remote{{Name: "my.fork", URL: "git@github.com:me/w.git"}, {Name: "up", URL: "https://x/y/z"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dotted name: got %+v", got)
	}
}

func TestParseRemoteURL(t *testing.T) {
	tests := []struct {
		url  string
		want Repo
		ok   bool
	}{
		{"https://github.com/acme/widgets.git", Repo{"github.com", "acme", "widgets"}, true},
		{"https://github.com/acme/widgets", Repo{"github.com", "acme", "widgets"}, true},
		{"https://github.com/acme/widgets/", Repo{"github.com", "acme", "widgets"}, true},
		{"https://user:tok@GitHub.com/acme/widgets.git", Repo{"github.com", "acme", "widgets"}, true},
		{"git@github.com:acme/widgets.git", Repo{"github.com", "acme", "widgets"}, true},
		{"github.com:acme/widgets", Repo{"github.com", "acme", "widgets"}, true},
		{"ssh://git@github.com/acme/widgets.git", Repo{"github.com", "acme", "widgets"}, true},
		{"ssh://git@ssh.github.com:443/acme/widgets.git", Repo{"github.com", "acme", "widgets"}, true},
		{"git://github.example.com/acme/widgets", Repo{"github.example.com", "acme", "widgets"}, true},
		{"/srv/git/widgets.git", Repo{}, false},
		{"https://github.com/acme", Repo{}, false},
		{"https://gitlab.com/a/b/c", Repo{}, false},
		{"", Repo{}, false},
	}
	for _, tt := range tests {
		got, ok := ParseRemoteURL(tt.url)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%q: got %+v %v, want %+v %v", tt.url, got, ok, tt.want, tt.ok)
		}
	}
}

func TestHeadRepos(t *testing.T) {
	repo := Repo{Host: "github.com", Owner: "acme", Name: "widgets"}
	remotes := []Remote{
		{Name: "origin", URL: "git@github.com:me/widgets.git"},
		{Name: "upstream", URL: "https://github.com/acme/widgets"},
		{Name: "work", URL: "https://ghe.example.com/acme/widgets.git"}, // another host
		{Name: "local", URL: "/srv/git/widgets.git"},                    // not GitHub
	}
	got := HeadRepos(repo, remotes)
	want := []Repo{repo, {Host: "github.com", Owner: "me", Name: "widgets"}, repo}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestPickRepo(t *testing.T) {
	gh := func(h string) bool { return h == "github.com" }
	tests := []struct {
		name    string
		remotes []Remote
		want    string
		ok      bool
	}{
		{"origin", []Remote{{Name: "origin", URL: "git@github.com:me/w.git"}}, "me/w", true},
		{"upstream before origin", []Remote{{Name: "origin", URL: "git@github.com:me/w.git"}, {Name: "upstream", URL: "https://github.com/acme/w"}}, "acme/w", true},
		{"set-default wins", []Remote{{Name: "upstream", URL: "https://github.com/acme/w"}, {Name: "origin", URL: "git@github.com:me/w.git", Resolved: "base"}}, "me/w", true},
		{"resolved names a repo", []Remote{{Name: "origin", URL: "git@github.com:me/w.git", Resolved: "acme/real"}}, "acme/real", true},
		{"unknown host skipped", []Remote{{Name: "origin", URL: "git@gitlab.com:me/w.git"}, {Name: "zz", URL: "https://github.com/a/b"}}, "a/b", true},
		{"none", []Remote{{Name: "origin", URL: "/srv/w.git"}}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := PickRepo(tt.remotes, gh)
			if ok != tt.ok || (ok && got.String() != tt.want) {
				t.Errorf("got %v %v, want %v %v", got, ok, tt.want, tt.ok)
			}
		})
	}
	remotes := []Remote{{Name: "origin", URL: "git@github.com:Me/W.git"}}
	if RemoteOf(remotes, Repo{"github.com", "me", "w"}) != "origin" || RemoteOf(remotes, Repo{"github.com", "x", "w"}) != "" {
		t.Error("RemoteOf")
	}
}

func TestParseStatus(t *testing.T) {
	// A rename carries its source path: three entries, not four.
	if got := ParseStatus(fixture(t, "status_dirty.bin")); got != 3 {
		t.Errorf("dirty fixture: %d entries, want 3", got)
	}
	tests := map[string]int{
		"":                               0,
		"?? a\x00":                       1,
		" M a b\x00?? c\x00":             2,
		"C  new\x00old\x00 D gone\x00":   2,
		"!! ignored\x00":                 1, // gh reap never asks for ignored files
		"RM renamed\x00from\x00?? x\x00": 2,
	}
	for in, want := range tests {
		if got := ParseStatus(in); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}

func TestParseProcesses(t *testing.T) {
	got, err := ParseProcesses(fixture(t, "lsof.txt"))
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Command != "sleep" || got[0].Cwd != "$ROOT" || got[1].Cwd != "$ROOT/sub dir" || got[0].PPID == 0 || got[0].PID == 0 {
		t.Errorf("got %+v", got)
	}
	// A process without a cwd entry (unreadable) is skipped.
	got, err = ParseProcesses("p1\nR0\nclaunchd\np2\nR1\ncsh\nfcwd\nn/home\n")
	if err != nil || len(got) != 1 || got[0] != (Process{PID: 2, PPID: 1, Command: "sh", Cwd: "/home"}) {
		t.Errorf("got %+v %v", got, err)
	}
	// Output it cannot read fails the whole list rather than dropping a process.
	for _, bad := range []string{"pINVALID\nczsh\nn/w\n", "p0\nczsh\nn/w\n", "p5\nRx\nczsh\nn/w\n", "czsh\nn/w\n"} {
		if got, err := ParseProcesses(bad); err == nil {
			t.Errorf("%q: accepted as %+v", bad, got)
		}
	}
}

func TestParseHookList(t *testing.T) {
	none := fixture(t, "hook_list_none_stderr.txt")
	tests := []struct {
		name string
		out  string
		err  error
		want HookState
	}{
		{"configured", fixture(t, "hook_list_configured.txt"), nil, HookConfigured},
		{"none", "", &CmdError{ExitCode: 1, Stderr: none}, HookNone},
		{"old git: unknown option", "", &CmdError{ExitCode: 129, Stderr: "error: unknown option `allow-unknown-hook-name'"}, HookUnsupported},
		{"older git: no hook command", "", &CmdError{ExitCode: 1, Stderr: "git: 'hook' is not a git command. See 'git --help'."}, HookUnsupported},
		{"git without list", "", &CmdError{ExitCode: 129, Stderr: "usage: git hook run"}, HookUnsupported},
		{"other failure", "", &CmdError{ExitCode: 128, Stderr: "fatal: not a git repository"}, HookUnknown},
		{"not run", "", errors.New("exec: git: not found"), HookUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseHookList(tt.out, tt.err); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseDiskUsageAndCount(t *testing.T) {
	if kb, err := ParseDiskUsage("1234\t/some path\n"); kb != 1234 || err != nil {
		t.Errorf("du: %d %v", kb, err)
	}
	if _, err := ParseDiskUsage(""); err == nil {
		t.Error("du: empty should fail")
	}
	if n, err := ParseCount(fixture(t, "count_unpushed.txt")); n != 1 || err != nil {
		t.Errorf("count: %d %v", n, err)
	}
}

// cliRepos are the head repositories counted as yours in the recorded cli/cli answer.
var cliRepos = []Repo{{Host: "github.com", Owner: "cli", Name: "cli"}}

func TestPRQueryAndResponse(t *testing.T) {
	q := PRQuery(2)
	for _, want := range []string{
		"$b0: String!", "$b1: String!", "b1: pullRequests(headRefName: $b1, first: 10",
		"orderBy: {field: CREATED_AT, direction: DESC}", "defaultBranchRef { name }", "headRefOid", "headRepository { nameWithOwner }",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q:\n%s", want, q)
		}
	}
	var resp struct{ Data json.RawMessage }
	if err := json.Unmarshal([]byte(fixture(t, "graphql_response.json")), &resp); err != nil {
		t.Fatal(err)
	}
	branches := []string{"bagtoad/triage-ghmock-skill", "probe", "dependabot/github_actions/codeql-actions-92276e7fb0", "no-such-branch-xyz"}
	res := PRResult{ByBranch: map[string]PR{}}
	if err := ParsePRResponse(resp.Data, branches, cliRepos, &res); err != nil {
		t.Fatal(err)
	}
	// probe's only PR comes from someone's fork: not cli/cli's.
	if res.DefaultBranch != "trunk" || len(res.ByBranch) != 2 {
		t.Errorf("got %+v", res)
	}
	if _, ok := res.ByBranch["probe"]; ok {
		t.Errorf("a fork's PR counted as ours: %+v", res.ByBranch["probe"])
	}
	// Several PRs from this head branch: the newest wins.
	if pr := res.ByBranch[branches[0]]; pr.Number != 14976 || pr.State != "MERGED" || pr.HeadRepo != "cli/cli" {
		t.Errorf("%s: %+v", branches[0], pr)
	}
	if res.ByBranch[branches[2]].State != "OPEN" {
		t.Errorf("states: %+v", res.ByBranch)
	}
	// With that fork as one of our remotes, its PR is ours.
	fork := append(slices.Clone(cliRepos), Repo{Host: "github.com", Owner: "EscaleraAlCielo", Name: "tmp-cli-fork-delete-me"})
	res = PRResult{ByBranch: map[string]PR{}}
	if err := ParsePRResponse(resp.Data, branches, fork, &res); err != nil {
		t.Fatal(err)
	}
	if pr := res.ByBranch["probe"]; pr.State != "CLOSED" || pr.Number != 14974 || pr.HeadOID != "f74588630208e529fb09d898eccfad4fc02fc927" {
		t.Errorf("probe from our fork: %+v", pr)
	}
	// A missing alias is an error, not "no PR".
	if err := ParsePRResponse(resp.Data, append(branches, "fifth"), cliRepos, &PRResult{ByBranch: map[string]PR{}}); err == nil {
		t.Error("missing alias accepted")
	}
	if err := ParsePRResponse([]byte(`{"repository":null}`), nil, cliRepos, &PRResult{ByBranch: map[string]PR{}}); err == nil {
		t.Error("null repository accepted")
	}
	// Without the default branch, its protection would be lost: an error.
	for _, bad := range []string{`{"repository":{}}`, `{"repository":{"defaultBranchRef":null}}`, `{"repository":{"defaultBranchRef":{"name":""}}}`, `{"repository":{"defaultBranchRef":7}}`} {
		if err := ParsePRResponse([]byte(bad), nil, cliRepos, &PRResult{ByBranch: map[string]PR{}}); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestCmdError(t *testing.T) {
	err := &CmdError{Cmd: "git x", ExitCode: 1, Stderr: "line one\nfatal: the reason\n", Err: errors.New("exit status 1")}
	if err.Error() != "git x: fatal: the reason" || ExitCode(err) != 1 || ExitCode(nil) != 0 || ExitCode(errors.New("x")) != -1 {
		t.Errorf("%v", err)
	}
	if (&CmdError{Cmd: "lsof", ExitCode: -1, Err: errors.New("not found")}).Error() != "lsof: not found" {
		t.Error("no stderr")
	}
}
