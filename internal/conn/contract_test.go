//go:build contract

package conn

// Contract tests: run the real commands against a fixture repository and check
// their output still matches what the unit tests replay from testdata/. Run them
// with `go test -tags contract ./internal/conn`; re-record with `-update`.
// CI runs them nightly against the newest git.

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "re-record testdata from real commands")

// fixtureRepo builds the repository every fixture is recorded from, under root:
//
//	origin.git    bare remote (reached through url.insteadOf from a github.com URL)
//	repo          main checkout on main; hook "teardown" registered
//	wt-merged     worktree, branch merged, pushed
//	wt-unpushed   worktree, branch unpushed, one commit past what was pushed
//	wt-dirty      worktree, branch dirty: modified, untracked and renamed files
//	wt-locked     worktree, branch locked, locked with a reason
//	wt-gone       worktree, branch gone, folder moved away
//	wt-detached   worktree on a detached HEAD
//	branches      squashed, kept and release.v1 (both marked keep), nopr, level (= main)
//	plain         a second repository with no hook
//
// Fixed identities and dates make every object id the same on every run.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitconfig := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(gitconfig, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": gitconfig, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.com", "GIT_AUTHOR_DATE": "2024-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME": "Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.com", "GIT_COMMITTER_DATE": "2024-01-01T00:00:00Z",
	} {
		t.Setenv(k, v)
	}
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(dir, file, content string) {
		t.Helper()
		write(filepath.Join(dir, file), content)
		git(dir, "add", file)
		git(dir, "commit", "-q", "-m", "add "+file)
	}
	repo := filepath.Join(root, "repo")
	origin := filepath.Join(root, "origin.git")
	git(root, "init", "-q", "--bare", "--object-format=sha1", "-b", "main", origin)
	git(root, "init", "-q", "--object-format=sha1", "-b", "main", repo)
	git(repo, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	git(repo, "config", "url."+origin+".insteadOf", "https://github.com/acme/widgets.git")
	git(repo, "config", "remote.origin.gh-resolved", "base")
	commit(repo, "README", "widgets\n")
	git(repo, "push", "-q", "-u", "origin", "main")
	git(repo, "remote", "set-head", "origin", "main")

	tree := func(branch string) string {
		t.Helper()
		path := filepath.Join(root, "wt-"+branch)
		git(repo, "worktree", "add", "-q", "-b", branch, path, "main")
		commit(path, branch+".txt", branch+"\n")
		git(path, "push", "-q", "origin", branch)
		return path
	}
	tree("merged")
	commit(tree("unpushed"), "later.txt", "not pushed\n")
	dirty := tree("dirty")
	write(filepath.Join(dirty, "dirty.txt"), "changed\n")
	write(filepath.Join(dirty, "new file.txt"), "untracked\n")
	git(dirty, "mv", "README", "README.md")
	git(repo, "worktree", "lock", "--reason", "on a removable disk", tree("locked"))
	if err := os.Rename(tree("gone"), filepath.Join(root, "moved-away")); err != nil {
		t.Fatal(err)
	}
	git(repo, "worktree", "add", "-q", "--detach", filepath.Join(root, "wt-detached"), "main")
	for _, b := range []string{"squashed", "kept", "release.v1", "nopr"} {
		git(repo, "worktree", "remove", tree(b))
	}
	git(repo, "branch", "level", "main")
	git(repo, "config", "branch.kept.reap-keep", "true")
	git(repo, "config", "branch.release.v1.reap-keep", "yes")

	hook := filepath.Join(root, "teardown.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho \"teardown for $1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(repo, "config", "hook.teardown.command", hook)
	git(repo, "config", "hook.teardown.event", HookEvent)

	plain := filepath.Join(root, "plain")
	git(root, "init", "-q", "--object-format=sha1", "-b", "main", plain)
	commit(plain, "x", "x\n")
	return root
}

// golden compares got (with the fixture root replaced by $ROOT) to testdata/name,
// or writes it there with -update.
func golden(t *testing.T, root, name, got string) {
	t.Helper()
	got = strings.ReplaceAll(got, root, "$ROOT")
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s: real output no longer matches the fixture\n got: %q\nwant: %q", name, got, want)
	}
}

// must returns a command's output, failing the test on an error.
func must(t *testing.T) func(string, error) string {
	return func(out string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
}

func TestContract_Git(t *testing.T) {
	root := fixtureRepo(t)
	repo := filepath.Join(root, "repo")
	ctx := context.Background()
	x := &Exec{}

	golden(t, root, "worktree_list.bin", must(t)(x.WorktreeList(ctx, filepath.Join(root, "wt-merged"))))
	golden(t, root, "common_dir.txt", must(t)(x.CommonDir(ctx, filepath.Join(root, "wt-merged"))))
	golden(t, root, "branches.txt", must(t)(x.Branches(ctx, repo)))
	golden(t, root, "keep_marks.txt", must(t)(x.KeepMarks(ctx, repo)))
	golden(t, root, "remotes.txt", must(t)(x.Remotes(ctx, repo)))
	golden(t, root, "remote_head.txt", must(t)(x.RemoteHead(ctx, repo, "origin")))
	golden(t, root, "status_dirty.bin", must(t)(x.Status(ctx, filepath.Join(root, "wt-dirty"))))
	if out := must(t)(x.Status(ctx, filepath.Join(root, "wt-merged"))); out != "" {
		t.Errorf("clean tree status: %q", out)
	}

	branches := map[string]string{}
	for _, b := range ParseBranches(must(t)(x.Branches(ctx, repo))) {
		branches[b.Name] = b.OID
	}
	tip, err := exec.Command("git", "-C", repo, "rev-parse", "refs/remotes/origin/unpushed").Output()
	if err != nil {
		t.Fatal(err)
	}
	pushed := strings.TrimSpace(string(tip))
	golden(t, root, "count_unpushed.txt", must(t)(x.CountUnpushed(ctx, repo, branches["unpushed"], pushed)))
	if n := must(t)(x.CountUnpushed(ctx, repo, branches["merged"], branches["merged"])); n != "0" {
		t.Errorf("count at the PR head: %q", n)
	}

	if ok, err := x.HasCommit(ctx, repo, branches["merged"]); !ok || err != nil {
		t.Errorf("HasCommit(present) = %v, %v", ok, err)
	}
	if ok, err := x.HasCommit(ctx, repo, strings.Repeat("ab", 20)); ok || err != nil {
		t.Errorf("HasCommit(absent) = %v, %v", ok, err)
	}
	if ok, err := x.IsAncestor(ctx, repo, pushed, branches["unpushed"]); !ok || err != nil {
		t.Errorf("IsAncestor(behind) = %v, %v", ok, err)
	}
	if ok, err := x.IsAncestor(ctx, repo, branches["unpushed"], pushed); ok || err != nil {
		t.Errorf("IsAncestor(ahead) = %v, %v", ok, err)
	}

	// Hooks: listed, run inside the tree with its path, and absent elsewhere.
	hooks, err := x.HookList(ctx, filepath.Join(root, "wt-merged"))
	golden(t, root, "hook_list_configured.txt", must(t)(hooks, err))
	if got := ParseHookList(hooks, err); got != HookConfigured {
		t.Errorf("configured hook read as %v", got)
	}
	out, err := x.HookList(ctx, filepath.Join(root, "plain"))
	var stderr string
	var ce *CmdError
	if errors.As(err, &ce) {
		stderr = ce.Stderr
	}
	golden(t, root, "hook_list_none_stderr.txt", stderr)
	if got := ParseHookList(out, err); got != HookNone {
		t.Errorf("no hook read as %v (%v)", got, err)
	}
	golden(t, root, "run_hook.txt", must(t)(x.RunHook(ctx, filepath.Join(root, "wt-merged"))))
	if _, err := x.RunHook(ctx, filepath.Join(root, "plain")); err != nil {
		t.Errorf("hook run with nothing registered: %v", err)
	}

	// Refusals gh reap relies on: git keeps a dirty tree and an unmerged branch.
	out, err = x.RemoveWorktree(ctx, repo, filepath.Join(root, "wt-dirty"))
	if err == nil {
		t.Error("git worktree remove took a dirty tree")
	}
	golden(t, root, "remove_worktree_dirty.txt", out)
	out, err = x.DeleteBranch(ctx, repo, "squashed")
	if err == nil {
		t.Error("git branch -d took an unmerged branch")
	}
	golden(t, root, "delete_branch_unmerged.txt", out)
	if !NotFullyMerged(out) {
		t.Errorf("NotFullyMerged does not recognise git's refusal: %q", out)
	}

	// What gh reap does run: remove a clean tree, a gone tree's registration, and
	// force-delete the squash-merged branch.
	if out, err := x.RemoveWorktree(ctx, repo, filepath.Join(root, "wt-gone")); err != nil {
		t.Errorf("remove a gone tree's registration: %v %s", err, out)
	}
	// -d refuses a branch checked out in a worktree, and not as "not fully merged":
	// gh reap must not delete past that refusal.
	out, err = x.DeleteBranch(ctx, repo, "merged")
	if err == nil || NotFullyMerged(out) {
		t.Errorf("branch -d on a checked-out branch: %v %q", err, out)
	}
	if out, err := x.RemoveWorktree(ctx, repo, filepath.Join(root, "wt-merged")); err != nil {
		t.Errorf("remove a clean tree: %v %s", err, out)
	}
	// -d refuses a branch merged on GitHub but not into the local main: hence -D,
	// only for a merged PR whose head is the branch tip.
	if _, err := x.DeleteBranch(ctx, repo, "merged"); err == nil {
		t.Error("branch -d took a branch not merged locally")
	}
	if out, err := x.DeleteBranch(ctx, repo, "level"); err != nil {
		t.Errorf("branch -d level: %v %s", err, out)
	}
	// The forced delete is atomic on the checked tip: a moved branch is refused.
	out, err = x.DeleteBranchAt(ctx, repo, "squashed", branches["merged"])
	if err == nil {
		t.Error("deleted a branch at a tip other than the one checked")
	}
	golden(t, root, "delete_branch_moved.txt", out)
	if err := x.SetKeep(ctx, repo, "squashed"); err != nil {
		t.Fatal(err)
	}
	if out, err := x.DeleteBranchAt(ctx, repo, "squashed", branches["squashed"]); err != nil {
		t.Errorf("delete squashed at its tip: %v %s", err, out)
	}
	if strings.Contains(must(t)(x.Branches(ctx, repo)), "squashed\t") {
		t.Error("squashed still there")
	}
	if ParseKeepMarks(must(t)(x.KeepMarks(ctx, repo)))["squashed"] {
		t.Error("the deleted branch's config section is left behind")
	}
	if _, err := os.Stat(filepath.Join(root, "wt-merged")); !os.IsNotExist(err) {
		t.Error("wt-merged still on disk")
	}

	// Keep marks round-trip.
	if err := x.SetKeep(ctx, repo, "nopr"); err != nil {
		t.Fatal(err)
	}
	if !ParseKeepMarks(must(t)(x.KeepMarks(ctx, repo)))["nopr"] {
		t.Error("SetKeep not visible")
	}
	if err := x.UnsetKeep(ctx, repo, "nopr"); err != nil {
		t.Fatal(err)
	}
	if err := x.UnsetKeep(ctx, repo, "nopr"); err != nil {
		t.Errorf("unsetting twice: %v", err)
	}

	if kb, err := ParseDiskUsage(must(t)(x.DiskUsage(ctx, filepath.Join(root, "plain")))); err != nil || kb <= 0 {
		t.Errorf("du: %d %v", kb, err)
	}
}

// TestContract_Lsof checks lsof sees a process's working directory and parent. Its
// full output differs on every machine, so it is checked, not compared.
func TestContract_Lsof(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub dir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	sleep := exec.Command("sleep", "30")
	sleep.Dir = dir
	nested := exec.Command("sleep", "30")
	nested.Dir = sub
	for _, c := range []*exec.Cmd{sleep, nested} {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Process.Kill(); _ = c.Wait() }()
	}
	time.Sleep(100 * time.Millisecond)

	out, err := (&Exec{}).ProcessCwds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found *Process
	procs, err := ParseProcesses(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.PID == sleep.Process.Pid {
			found = &p
		}
	}
	if found == nil {
		t.Fatalf("lsof did not list the sleep (pid %d)", sleep.Process.Pid)
	}
	if found.Cwd != dir || found.PPID != os.Getpid() || found.Command != "sleep" {
		t.Errorf("lsof read %+v, want cwd %s, parent %d, command sleep", *found, dir, os.Getpid())
	}
	if *update {
		// Keep the two sleeps: real output, small, and with no user's paths.
		var keep []string
		var cur []string
		flush := func() {
			if len(cur) > 0 && (cur[0] == "p"+strconv.Itoa(sleep.Process.Pid) || cur[0] == "p"+strconv.Itoa(nested.Process.Pid)) {
				keep = append(keep, cur...)
			}
			cur = nil
		}
		for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			if strings.HasPrefix(line, "p") {
				flush()
			}
			cur = append(cur, line)
		}
		flush()
		golden(t, dir, "lsof.txt", strings.Join(keep, "\n")+"\n")
	}
}
