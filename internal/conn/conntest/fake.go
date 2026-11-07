// Package conntest is a fake conn.Conn for unit tests. It answers from recorded
// command output (internal/conn/testdata, kept honest by the contract tests) and
// from answers a test sets, and records every call.
package conntest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/marekabaffy/gh-reap/internal/conn"
)

// Root is the placeholder for the fixture repository's folder in recorded output.
const Root = "$ROOT"

// Fixture reads a recorded output from internal/conn/testdata.
func Fixture(t testing.TB, name string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Answer is a command's output and error.
type Answer struct {
	Out string
	Err error
}

// Fail is an Answer for a command that exited with code and stderr.
func Fail(code int, stderr string) Answer {
	return Answer{Err: &conn.CmdError{Cmd: "fake", ExitCode: code, Stderr: stderr, Err: fmt.Errorf("exit status %d", code)}}
}

// Fake implements conn.Conn. Answers are keyed by the method name and its
// arguments joined by spaces, e.g. "Status $ROOT/wt-dirty". An unexpected call
// gets an error, so a test notices what it did not plan for.
type Fake struct {
	mu      sync.Mutex
	Answers map[string]Answer
	Bools   map[string]bool // HasCommit and IsAncestor
	Missing map[string]bool // paths Exists reports as gone
	PRs     conn.PRResult
	PRErr   error
	Calls   []string
	// OnCall, when set, runs on every call (to change the world between calls).
	OnCall func(key string)
}

// New returns an empty fake.
func New() *Fake {
	return &Fake{
		Answers: map[string]Answer{}, Bools: map[string]bool{}, Missing: map[string]bool{},
		PRs: conn.PRResult{ByBranch: map[string]conn.PR{}},
	}
}

// Set answers key with out.
func (f *Fake) Set(key, out string) { f.Answers[key] = Answer{Out: out} }

// Called reports whether a call with this key prefix happened.
func (f *Fake) Called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *Fake) answer(parts ...string) (string, error) {
	key := strings.Join(parts, " ")
	f.mu.Lock()
	f.Calls = append(f.Calls, key)
	hook := f.OnCall
	f.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.Answers[key]
	if !ok {
		return "", &conn.CmdError{Cmd: key, ExitCode: -1, Err: fmt.Errorf("fake: no answer for %q", key)}
	}
	return a.Out, a.Err
}

func (f *Fake) boolean(parts ...string) (bool, error) {
	key := strings.Join(parts, " ")
	f.mu.Lock()
	f.Calls = append(f.Calls, key)
	defer f.mu.Unlock()
	v, ok := f.Bools[key]
	if !ok {
		return false, fmt.Errorf("fake: no answer for %q", key)
	}
	return v, nil
}

func (f *Fake) WorktreeList(_ context.Context, dir string) (string, error) {
	return f.answer("WorktreeList", dir)
}

func (f *Fake) CommonDir(_ context.Context, dir string) (string, error) {
	return f.answer("CommonDir", dir)
}

func (f *Fake) Branches(_ context.Context, dir string) (string, error) {
	return f.answer("Branches", dir)
}

func (f *Fake) KeepMarks(_ context.Context, dir string) (string, error) {
	return f.answer("KeepMarks", dir)
}

func (f *Fake) SetKeep(_ context.Context, dir, b string) error {
	_, err := f.answer("SetKeep", dir, b)
	return err
}

func (f *Fake) UnsetKeep(_ context.Context, dir, b string) error {
	_, err := f.answer("UnsetKeep", dir, b)
	return err
}

func (f *Fake) Remotes(_ context.Context, dir string) (string, error) {
	return f.answer("Remotes", dir)
}

func (f *Fake) RemoteHead(_ context.Context, dir, remote string) (string, error) {
	return f.answer("RemoteHead", dir, remote)
}

func (f *Fake) Status(_ context.Context, tree string) (string, error) {
	return f.answer("Status", tree)
}

func (f *Fake) HasCommit(_ context.Context, dir, oid string) (bool, error) {
	return f.boolean("HasCommit", oid)
}

func (f *Fake) CountUnpushed(_ context.Context, dir, tip, base string) (string, error) {
	return f.answer("CountUnpushed", tip, base)
}

func (f *Fake) IsAncestor(_ context.Context, dir, a, b string) (bool, error) {
	return f.boolean("IsAncestor", a, b)
}

func (f *Fake) HookList(_ context.Context, tree string) (string, error) {
	return f.answer("HookList", tree)
}

func (f *Fake) RunHook(_ context.Context, tree string) (string, error) {
	return f.answer("RunHook", tree)
}

func (f *Fake) RemoveWorktree(_ context.Context, dir, path string) (string, error) {
	return f.answer("RemoveWorktree", path)
}

func (f *Fake) DeleteBranch(_ context.Context, dir, b string) (string, error) {
	return f.answer("DeleteBranch", "-d", b)
}

func (f *Fake) DeleteBranchAt(_ context.Context, dir, b, oid string) (string, error) {
	return f.answer("DeleteBranchAt", b, oid)
}

func (f *Fake) ProcessCwds(_ context.Context) (string, error) {
	return f.answer("ProcessCwds")
}

func (f *Fake) DiskUsage(_ context.Context, path string) (string, error) {
	return f.answer("DiskUsage", path)
}

func (f *Fake) Exists(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.Missing[path]
}

func (f *Fake) PullRequests(_ context.Context, repo conn.Repo, heads []conn.Repo, branches []string) (conn.PRResult, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, "PullRequests "+repo.Host+"/"+repo.String())
	defer f.mu.Unlock()
	return f.PRs, f.PRErr
}
