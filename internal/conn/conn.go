// Package conn holds every external command gh reap runs (git, lsof, du) and its
// one GitHub API call, behind the Conn interface. Parsers for their output are pure
// functions in parse.go, tested against recorded output in testdata/.
package conn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Conn is everything gh reap asks of the outside world. Exec is the real one;
// tests use a fake that answers from fixtures.
type Conn interface {
	// WorktreeList is `git worktree list --porcelain -z`.
	WorktreeList(ctx context.Context, dir string) (string, error)
	// CommonDir is the absolute git common dir (where the lock lives).
	CommonDir(ctx context.Context, dir string) (string, error)
	// Branches lists local branches as "name<TAB>oid" lines.
	Branches(ctx context.Context, dir string) (string, error)
	// KeepMarks lists `branch.<name>.reap-keep` config entries, normalised to true/false.
	KeepMarks(ctx context.Context, dir string) (string, error)
	// SetKeep sets branch.<name>.reap-keep=true; UnsetKeep removes it.
	SetKeep(ctx context.Context, dir, branch string) error
	UnsetKeep(ctx context.Context, dir, branch string) error
	// Remotes lists remote URLs and gh-resolved markers from git config.
	Remotes(ctx context.Context, dir string) (string, error)
	// RemoteHead is the branch a remote's HEAD points to, e.g. "refs/remotes/origin/main".
	RemoteHead(ctx context.Context, dir, remote string) (string, error)
	// Status is `git status --porcelain -z` including untracked files.
	Status(ctx context.Context, tree string) (string, error)
	// HasCommit reports whether a commit object exists locally.
	HasCommit(ctx context.Context, dir, oid string) (bool, error)
	// CountUnpushed counts commits reachable from tip but not from base or any remote.
	CountUnpushed(ctx context.Context, dir, tip, base string) (string, error)
	// IsAncestor reports whether a is an ancestor of (or equal to) b.
	IsAncestor(ctx context.Context, dir, a, b string) (bool, error)
	// HookList is `git hook list` for the pre-worktree-remove event, run in tree.
	HookList(ctx context.Context, tree string) (string, error)
	// RunHook runs the pre-worktree-remove hook inside tree with tree as its argument.
	RunHook(ctx context.Context, tree string) (string, error)
	// RemoveWorktree is `git worktree remove <path>`, never forced.
	RemoveWorktree(ctx context.Context, dir, path string) (string, error)
	// DeleteBranch is `git branch -d`, which refuses a branch git does not see merged
	// or one checked out in a worktree.
	DeleteBranch(ctx context.Context, dir, branch string) (string, error)
	// DeleteBranchAt deletes a branch only if it still points to oid: an atomic
	// `git update-ref -d refs/heads/<branch> <oid>`, then its config section, as -D would.
	DeleteBranchAt(ctx context.Context, dir, branch, oid string) (string, error)
	// ProcessCwds is `lsof -w -a -d cwd -FpcRn`: the working directory of every process.
	ProcessCwds(ctx context.Context) (string, error)
	// DiskUsage is `du -sk <path>`.
	DiskUsage(ctx context.Context, path string) (string, error)
	// Exists reports whether a path exists on disk.
	Exists(path string) bool
	// PullRequests asks GitHub for the default branch and, for each branch, the
	// newest PR whose head is that branch in one of heads.
	PullRequests(ctx context.Context, repo Repo, heads []Repo, branches []string) (PRResult, error)
}

// CmdError is a command that ran and exited non-zero, or could not start.
type CmdError struct {
	Cmd      string
	ExitCode int // -1 when the command could not start
	Stderr   string
	Err      error
}

func (e *CmdError) Error() string {
	if msg := LastLine(e.Stderr); msg != "" {
		return fmt.Sprintf("%s: %s", e.Cmd, msg)
	}
	return fmt.Sprintf("%s: %v", e.Cmd, e.Err)
}

func (e *CmdError) Unwrap() error { return e.Err }

// ExitCode returns the exit code of a failed command, 0 for nil and -1 when it did not run.
func ExitCode(err error) int {
	var ce *CmdError
	if errors.As(err, &ce) {
		return ce.ExitCode
	}
	if err != nil {
		return -1
	}
	return 0
}

// LastLine is the last non-empty line of s, trimmed, skipping git's "hint:" lines
// so the line shown is the error itself.
func LastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "hint:") {
			return l
		}
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// Exec runs real commands. Debug, when set, receives one line per command.
type Exec struct {
	Debug io.Writer
	// LookPath finds a program; nil means exec.LookPath.
	LookPath func(string) (string, error)
}

// HookEvent is the git hook event gh reap fires before it removes a worktree.
const HookEvent = "pre-worktree-remove"

// OutputHeld reports whether a command exited but a process it started kept its
// output open past waitDelay.
func OutputHeld(err error) bool { return errors.Is(err, exec.ErrWaitDelay) }

// waitDelay is how long a command's output is waited for once it has exited or
// been canceled.
const waitDelay = 5 * time.Second

// Env added to commands whose messages gh reap parses, so they are not translated.
var cLocale = []string{"LC_ALL=C", "LANG=C"}

// command is one external command to run.
type command struct {
	name     string
	args     []string
	dir      string   // working directory; empty means the current one
	env      []string // added to the environment
	combined bool     // stdout and stderr in one stream, for output shown to the user
}

func (x *Exec) run(ctx context.Context, c command) (string, error) {
	look := x.LookPath
	if look == nil {
		look = exec.LookPath
	}
	display := c.name + " " + strings.Join(c.args, " ")
	path, err := look(c.name)
	if err != nil {
		x.debugf("%s: not found", display)
		return "", &CmdError{Cmd: display, ExitCode: -1, Err: err}
	}
	cmd := exec.CommandContext(ctx, path, c.args...)
	// Once the command exits or Ctrl-C kills it, stop waiting for its output after
	// a while, even if a child (a hook's leftover server) still holds the pipes.
	cmd.WaitDelay = waitDelay
	cmd.Dir = c.dir
	if c.env != nil {
		cmd.Env = append(cmd.Environ(), c.env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if c.combined {
		cmd.Stderr = &stdout
	}
	start := time.Now()
	err = cmd.Run()
	x.debugf("%s (%s, exit %d)", display, time.Since(start).Round(time.Millisecond), cmd.ProcessState.ExitCode())
	if err == nil {
		return stdout.String(), nil
	}
	code := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	msg := stderr.String()
	if c.combined {
		msg = stdout.String()
	}
	return stdout.String(), &CmdError{Cmd: display, ExitCode: code, Stderr: msg, Err: err}
}

func (x *Exec) debugf(format string, a ...any) {
	if x.Debug != nil {
		fmt.Fprintf(x.Debug, "+ "+format+"\n", a...)
	}
}

// git runs git in dir, in the C locale so its output can be parsed.
func (x *Exec) git(ctx context.Context, dir string, args ...string) (string, error) {
	return x.run(ctx, command{name: "git", args: append([]string{"-C", dir}, args...), env: cLocale})
}

// gitShown runs git with stdout and stderr in one stream, for output shown to
// the user; env nil keeps the user's locale.
func (x *Exec) gitShown(ctx context.Context, env []string, args ...string) (string, error) {
	return x.run(ctx, command{name: "git", args: args, env: env, combined: true})
}

func (x *Exec) WorktreeList(ctx context.Context, dir string) (string, error) {
	return x.git(ctx, dir, "worktree", "list", "--porcelain", "-z")
}

func (x *Exec) CommonDir(ctx context.Context, dir string) (string, error) {
	out, err := x.git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	return strings.TrimSpace(out), err
}

func (x *Exec) Branches(ctx context.Context, dir string) (string, error) {
	return x.git(ctx, dir, "for-each-ref", "--format=%(refname:lstrip=2)%09%(objectname)", "refs/heads")
}

func (x *Exec) KeepMarks(ctx context.Context, dir string) (string, error) {
	out, err := x.git(ctx, dir, "config", "--type=bool", "--get-regexp", `^branch\..*\.reap-keep$`)
	if ExitCode(err) == 1 { // no entries
		return "", nil
	}
	return out, err
}

func (x *Exec) SetKeep(ctx context.Context, dir, branch string) error {
	_, err := x.git(ctx, dir, "config", "branch."+branch+".reap-keep", "true")
	return err
}

func (x *Exec) UnsetKeep(ctx context.Context, dir, branch string) error {
	_, err := x.git(ctx, dir, "config", "--unset", "branch."+branch+".reap-keep")
	if ExitCode(err) == 5 { // was not set
		return nil
	}
	return err
}

func (x *Exec) Remotes(ctx context.Context, dir string) (string, error) {
	out, err := x.git(ctx, dir, "config", "--get-regexp", `^remote\..*\.(url|gh-resolved)$`)
	if ExitCode(err) == 1 {
		return "", nil
	}
	return out, err
}

func (x *Exec) RemoteHead(ctx context.Context, dir, remote string) (string, error) {
	out, err := x.git(ctx, dir, "symbolic-ref", "--quiet", "refs/remotes/"+remote+"/HEAD")
	return strings.TrimSpace(out), err
}

func (x *Exec) Status(ctx context.Context, tree string) (string, error) {
	return x.git(ctx, tree, "status", "--porcelain", "-z", "--untracked-files=all", "--ignore-submodules=none")
}

func (x *Exec) HasCommit(ctx context.Context, dir, oid string) (bool, error) {
	_, err := x.git(ctx, dir, "cat-file", "-e", oid+"^{commit}")
	switch code := ExitCode(err); {
	case code == 0:
		return true, nil
	case code > 0:
		return false, nil
	default:
		return false, err
	}
}

func (x *Exec) CountUnpushed(ctx context.Context, dir, tip, base string) (string, error) {
	out, err := x.git(ctx, dir, "rev-list", "--count", tip, "--not", base, "--remotes")
	return strings.TrimSpace(out), err
}

func (x *Exec) IsAncestor(ctx context.Context, dir, a, b string) (bool, error) {
	_, err := x.git(ctx, dir, "merge-base", "--is-ancestor", a, b)
	switch ExitCode(err) {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, err
	}
}

func (x *Exec) HookList(ctx context.Context, tree string) (string, error) {
	return x.git(ctx, tree, "hook", "list", "--allow-unknown-hook-name", HookEvent)
}

func (x *Exec) RunHook(ctx context.Context, tree string) (string, error) {
	// The hook's own output is shown to the user, in their locale.
	return x.gitShown(ctx, nil, "-C", tree, "hook", "run", "--allow-unknown-hook-name", "--ignore-missing", HookEvent, "--", tree)
}

func (x *Exec) RemoveWorktree(ctx context.Context, dir, path string) (string, error) {
	return x.gitShown(ctx, cLocale, "-C", dir, "worktree", "remove", path)
}

// NotFullyMerged reports whether `git branch -d` (run in the C locale) refused a
// branch only because git does not see it merged.
func NotFullyMerged(out string) bool { return strings.Contains(out, "is not fully merged") }

func (x *Exec) DeleteBranch(ctx context.Context, dir, branch string) (string, error) {
	return x.gitShown(ctx, cLocale, "-C", dir, "branch", "-d", "--", branch)
}

func (x *Exec) DeleteBranchAt(ctx context.Context, dir, branch, oid string) (string, error) {
	if out, err := x.gitShown(ctx, cLocale, "-C", dir, "update-ref", "-d", "refs/heads/"+branch, oid); err != nil {
		return out, err
	}
	// The ref is gone; its config (upstream, a keep mark) goes with it, as with branch -D.
	_, _ = x.git(ctx, dir, "config", "--remove-section", "branch."+branch)
	return "", nil
}

func (x *Exec) ProcessCwds(ctx context.Context) (string, error) {
	// Run from / so lsof's own working directory holds no tree.
	// Any failure, even with partial output, leaves the list incomplete.
	return x.run(ctx, command{name: "lsof", args: []string{"-w", "-a", "-d", "cwd", "-FpcRn"}, dir: "/"})
}

func (x *Exec) Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (x *Exec) DiskUsage(ctx context.Context, path string) (string, error) {
	return x.run(ctx, command{name: "du", args: []string{"-sk", path}})
}
