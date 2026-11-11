package reap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/marekabaffy/gh-reap/internal/conn"
	"github.com/marekabaffy/gh-reap/internal/conn/conntest"
)

// removable is the fixture world with a real folder for the lock, and git
// answering every removal step successfully.
func removable(t *testing.T) *conntest.Fake {
	f := world(t)
	f.Set("CommonDir .", t.TempDir())
	f.Set("RunHook "+R+"/wt-merged", "teardown for "+R+"/wt-merged\n")
	f.Set("RemoveWorktree "+R+"/wt-merged", "")
	f.Set("RemoveWorktree "+R+"/wt-gone", "")
	f.Set("DeleteBranch -d merged", "Deleted branch merged\n")
	f.Set("DeleteBranch -d squashed", "Deleted branch squashed\n")
	return f
}

func remove(t *testing.T, f *conntest.Fake, opt RemoveOptions) *Report {
	t.Helper()
	opt.workers = 1
	rep, err := Remove(context.Background(), f, ".", opt)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// mutations lists the calls that change something, in order.
func mutations(f *conntest.Fake) []string {
	var out []string
	for _, c := range f.Calls {
		for _, p := range []string{"RunHook", "RemoveWorktree", "DeleteBranch", "DeleteBranchAt", "SetKeep", "UnsetKeep"} {
			if strings.HasPrefix(c, p+" ") {
				out = append(out, c)
			}
		}
	}
	return out
}

func outcomes(rep *Report) map[string]string {
	m := map[string]string{}
	for _, r := range rep.Results {
		m[r.Ref] = r.Result + " " + r.Reason
	}
	return m
}

func TestRemove_All(t *testing.T) {
	f := removable(t)
	rep := remove(t, f, RemoveOptions{})
	want := map[string]string{
		"worktree:" + R + "/wt-gone":   "removed folder-gone",
		"worktree:" + R + "/wt-merged": "removed pr-merged",
		"branch:squashed":              "removed pr-merged",
	}
	if got := outcomes(rep); !reflect.DeepEqual(got, want) || !rep.OK() {
		t.Errorf("got %v", got)
	}
	// Gone registrations first, then trees (hook, tree, branch), then branches.
	wantCalls := []string{
		"RemoveWorktree " + R + "/wt-gone",
		"RunHook " + R + "/wt-merged", "RemoveWorktree " + R + "/wt-merged", "DeleteBranch -d merged",
		"DeleteBranch -d squashed",
	}
	if got := mutations(f); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("calls\n got %v\nwant %v", got, wantCalls)
	}
	// One lsof for the list, one per item checked again, one after the hook.
	n := 0
	for _, c := range f.Calls {
		if c == "ProcessCwds" {
			n++
		}
	}
	if n != 5 {
		t.Errorf("lsof ran %d times, want 5", n)
	}
}

func TestRemove_Confirm(t *testing.T) {
	// Declining: Confirm sees every chosen item, and nothing is touched.
	f := removable(t)
	var seen []string
	rep := remove(t, f, RemoveOptions{Confirm: func(items []Item) bool {
		for _, it := range items {
			seen = append(seen, it.Ref)
		}
		return false
	}})
	if !rep.Canceled || len(rep.Results) != 0 || len(mutations(f)) != 0 || len(seen) != 3 {
		t.Errorf("declined: seen %v, report %+v, calls %v", seen, rep, mutations(f))
	}
	// Accepting: everything chosen is removed.
	f = removable(t)
	rep = remove(t, f, RemoveOptions{Confirm: func([]Item) bool { return true }})
	if rep.Canceled || !rep.OK() || len(rep.Results) != 3 {
		t.Errorf("accepted: %+v", rep)
	}
}

func TestRemove_ByRef(t *testing.T) {
	f := removable(t)
	rep := remove(t, f, RemoveOptions{Refs: []string{"squashed", "nopr", "missing", "branch:squashed"}})
	want := map[string]string{
		"branch:squashed": "removed pr-merged",
		"branch:nopr":     "refused no-pr",
		"missing":         "refused not-found",
	}
	if got := outcomes(rep); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
	if rep.OK() {
		t.Error("a refused ref must fail the run")
	}
	if len(rep.Results) != 3 {
		t.Errorf("duplicate ref acted on twice: %+v", rep.Results)
	}
}

func TestRemove_Failures(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(*conntest.Fake)
		ref     string
		outcome string
		detail  string
		calls   []string
	}{
		{
			name: "hook fails: tree and branch kept",
			edit: func(f *conntest.Fake) {
				f.Answers["RunHook "+R+"/wt-merged"] = conntest.Answer{Out: "dumping db\nerror: database busy\n", Err: errors.New("exit status 1")}
			},
			ref: "merged", outcome: "failed hook-failed", detail: "error: database busy",
			calls: []string{"RunHook " + R + "/wt-merged"},
		},
		{
			name: "git worktree remove refuses: branch kept",
			edit: func(f *conntest.Fake) {
				f.Answers["RemoveWorktree "+R+"/wt-merged"] = conntest.Answer{Out: "fatal: '" + R + "/wt-merged' contains modified or untracked files, use --force to delete it\n", Err: errors.New("exit status 128")}
			},
			ref: "merged", outcome: "failed worktree-remove-failed", detail: "contains modified or untracked files",
			calls: []string{"RunHook " + R + "/wt-merged", "RemoveWorktree " + R + "/wt-merged"},
		},
		{
			name: "squash merge: -d refuses, PR merged at this tip, so -D",
			edit: func(f *conntest.Fake) {
				f.Answers["DeleteBranch -d squashed"] = conntest.Answer{Out: conntest.Fixture(t, "delete_branch_unmerged.txt"), Err: errors.New("exit status 1")}
				f.Set("DeleteBranchAt squashed "+oid["squashed"], "")
			},
			ref: "squashed", outcome: "removed pr-merged",
			calls: []string{"DeleteBranch -d squashed", "DeleteBranchAt squashed " + oid["squashed"]},
		},
		{
			name: "tip moved after the check: the atomic delete refuses",
			edit: func(f *conntest.Fake) {
				f.Answers["DeleteBranch -d squashed"] = conntest.Answer{Out: conntest.Fixture(t, "delete_branch_unmerged.txt"), Err: errors.New("exit status 1")}
				f.Answers["DeleteBranchAt squashed "+oid["squashed"]] = conntest.Answer{Out: conntest.Fixture(t, "delete_branch_moved.txt"), Err: errors.New("exit status 1")}
			},
			ref: "squashed", outcome: "failed branch-delete-failed", detail: "but expected",
			calls: []string{"DeleteBranch -d squashed", "DeleteBranchAt squashed " + oid["squashed"]},
		},
		{
			name: "tree removed but branch delete fails: said so",
			edit: func(f *conntest.Fake) {
				f.Answers["DeleteBranch -d merged"] = conntest.Answer{Out: "error: the branch 'merged' is not fully merged\n", Err: errors.New("exit status 1")}
				f.Answers["DeleteBranchAt merged "+oid["merged"]] = conntest.Answer{Out: "error: still no\n", Err: errors.New("exit status 1")}
			},
			ref: "merged", outcome: "failed branch-delete-failed", detail: "worktree removed, branch kept: error: still no",
			calls: []string{"RunHook " + R + "/wt-merged", "RemoveWorktree " + R + "/wt-merged", "DeleteBranch -d merged", "DeleteBranchAt merged " + oid["merged"]},
		},
		{
			name: "checked out elsewhere after the check: -d refuses and that stands",
			edit: func(f *conntest.Fake) {
				f.Answers["DeleteBranch -d squashed"] = conntest.Answer{Out: "error: cannot delete branch 'squashed' used by worktree at '/elsewhere'\n", Err: errors.New("exit status 1")}
			},
			ref: "squashed", outcome: "failed branch-delete-failed", detail: "used by worktree",
			calls: []string{"DeleteBranch -d squashed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := removable(t)
			tt.edit(f)
			rep := remove(t, f, RemoveOptions{Refs: []string{tt.ref}})
			r := rep.Results[0]
			if got := r.Result + " " + r.Reason; got != tt.outcome || !strings.Contains(r.Detail, tt.detail) {
				t.Errorf("got %s %q, want %s containing %q", got, r.Detail, tt.outcome, tt.detail)
			}
			if got := mutations(f); !reflect.DeepEqual(got, tt.calls) {
				t.Errorf("calls\n got %v\nwant %v", got, tt.calls)
			}
			if rep.OK() != strings.HasPrefix(tt.outcome, "removed") {
				t.Errorf("OK() = %v", rep.OK())
			}
		})
	}
}

func TestRemove_RevalidatesEachItem(t *testing.T) {
	// Each change happens after the list and before the item is checked again.
	tests := []struct {
		name    string
		change  func(f *conntest.Fake)
		outcome string
	}{
		{"tree became dirty", func(f *conntest.Fake) {
			f.Set("Status "+R+"/wt-merged", "?? new.txt\x00")
		}, "refused dirty"},
		{"someone opened a shell in it", func(f *conntest.Fake) {
			f.Set("ProcessCwds", "p77\nR1\nczsh\nfcwd\nn"+R+"/wt-merged\n")
		}, "refused in-use"},
		{"branch moved to another commit, still behind the PR head", func(f *conntest.Fake) {
			moved := strings.Repeat("4", 40)
			f.Set("Branches "+R+"/repo", strings.Replace(conntest.Fixture(t, "branches.txt"), oid["merged"], moved, 1))
			f.Bools["IsAncestor "+moved+" "+oid["merged"]] = true
			f.Set("CountUnpushed "+moved+" "+oid["merged"], "0")
		}, "refused changed-since-list"},
		{"locked meanwhile", func(f *conntest.Fake) {
			f.Set("WorktreeList "+R+"/repo", strings.Replace(conntest.Fixture(t, "worktree_list.bin"),
				"branch refs/heads/merged\x00", "branch refs/heads/merged\x00locked\x00", 1))
		}, "refused locked"},
		{"tree removed by someone else", func(f *conntest.Fake) {
			f.Set("WorktreeList "+R+"/repo", strings.Replace(conntest.Fixture(t, "worktree_list.bin"),
				"worktree "+R+"/wt-merged\x00", "worktree "+R+"/wt-elsewhere\x00", 1))
		}, "refused changed-since-list"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := removable(t)
			var once sync.Once
			// The first list runs from "."; the check before acting runs from the
			// main checkout, so its first call marks the moment in between.
			f.OnCall = func(key string) {
				if key == "WorktreeList "+R+"/repo" {
					once.Do(func() { tt.change(f) })
				}
			}
			rep := remove(t, f, RemoveOptions{Refs: []string{"merged"}})
			if got := outcomes(rep)["worktree:"+R+"/wt-merged"]; got != tt.outcome {
				t.Errorf("got %q, want %q (%+v)", got, tt.outcome, rep.Results)
			}
			if len(mutations(f)) != 0 {
				t.Errorf("acted anyway: %v", mutations(f))
			}
		})
	}
}

func TestRemove_HookChangesTheTree(t *testing.T) {
	// Each change is made by the hook; the tree must be kept.
	tests := []struct {
		name    string
		change  func(f *conntest.Fake)
		outcome string
		detail  string
	}{
		{"starts a server inside", func(f *conntest.Fake) {
			f.Set("ProcessCwds", "p88\nR1\ncnode\nfcwd\nn"+R+"/wt-merged\n")
		}, "failed in-use", "node (pid 88)"},
		{"leaves an untracked file", func(f *conntest.Fake) {
			f.Set("Status "+R+"/wt-merged", "?? dump.sql\x00")
		}, "failed dirty", "1 uncommitted"},
		{"detaches HEAD (and may commit there)", func(f *conntest.Fake) {
			f.Set("WorktreeList "+R+"/repo", strings.Replace(conntest.Fixture(t, "worktree_list.bin"),
				"branch refs/heads/merged\x00", "detached\x00", 1))
		}, "failed detached", "changed by teardown hook"},
		{"commits on the branch", func(f *conntest.Fake) {
			moved := strings.Repeat("5", 40)
			f.Set("Branches "+R+"/repo", strings.Replace(conntest.Fixture(t, "branches.txt"), oid["merged"], moved, 1))
		}, "failed", "changed by teardown hook"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := removable(t)
			f.OnCall = func(key string) {
				if key == "RunHook "+R+"/wt-merged" {
					tt.change(f)
				}
			}
			rep := remove(t, f, RemoveOptions{Refs: []string{"merged"}})
			r := rep.Results[0]
			if !strings.HasPrefix(r.Result+" "+r.Reason, tt.outcome) || !strings.Contains(r.Detail, tt.detail) || f.Called("RemoveWorktree") {
				t.Errorf("got %+v, calls %v", r, mutations(f))
			}
		})
	}
}

func TestRemove_GoneFolderCameBack(t *testing.T) {
	f := removable(t)
	f.PRs.ByBranch["gone"] = conn.PR{Number: 5, State: "MERGED", HeadOID: oid["gone"]}
	f.Bools["HasCommit "+oid["gone"]] = true
	f.Bools["IsAncestor "+oid["gone"]+" "+oid["gone"]] = true
	f.Set("CountUnpushed "+oid["gone"]+" "+oid["gone"], "0")
	f.Set("Status "+R+"/wt-gone", "")
	f.Set("HookList "+R+"/wt-gone", "teardown\n")
	f.OnCall = func(key string) {
		if key == "WorktreeList "+R+"/repo" { // restored, clean, before the check
			f.Missing[R+"/wt-gone"] = false
			f.Set("WorktreeList "+R+"/repo", strings.Replace(conntest.Fixture(t, "worktree_list.bin"),
				"prunable gitdir file points to non-existent location\x00", "", 1))
		}
	}
	rep := remove(t, f, RemoveOptions{Refs: []string{"worktree:" + R + "/wt-gone"}})
	if got := outcomes(rep)["worktree:"+R+"/wt-gone"]; got != "refused changed-since-list" || len(mutations(f)) != 0 {
		t.Errorf("got %q, calls %v", got, mutations(f))
	}
}

func TestRemove_IncompleteListRemovesNothing(t *testing.T) {
	f := removable(t)
	f.PRErr = errors.New("HTTP 502")
	rep := remove(t, f, RemoveOptions{Refs: []string{"merged"}})
	if rep.OK() || len(rep.Problems) == 0 || len(mutations(f)) != 0 {
		t.Errorf("report %+v, calls %v", rep, mutations(f))
	}
	if got := outcomes(rep)["worktree:"+R+"/wt-merged"]; got != "refused pr-unknown" {
		t.Errorf("got %q", got)
	}
}

func TestRemove_Lock(t *testing.T) {
	f := removable(t)
	dir, _ := f.CommonDir(context.Background(), ".")
	release, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	f.Calls = nil
	_, err = Remove(context.Background(), f, ".", RemoveOptions{})
	if !errors.Is(err, ErrLocked) || len(mutations(f)) != 0 {
		t.Errorf("got %v, calls %v", err, f.Calls)
	}
	// The lock is released after a run.
	release()
	remove(t, f, RemoveOptions{})
	if _, err := os.Stat(filepath.Join(dir, LockName)); !os.IsNotExist(err) {
		t.Error("lock not released")
	}
}

func TestRemove_Canceled(t *testing.T) {
	f := removable(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.OnCall = func(key string) {
		if key == "DeleteBranch -d merged" {
			cancel()
		}
	}
	rep, err := Remove(ctx, f, ".", RemoveOptions{GatherOptions: GatherOptions{workers: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(rep)["branch:squashed"]; got != "failed canceled" {
		t.Errorf("after interrupt: %q", got)
	}
}

// An interrupt while listing kills commands mid-answer: there is no list to act on
// or to report, only the interrupt.
func TestRemove_CanceledWhileListing(t *testing.T) {
	f := removable(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.OnCall = func(key string) {
		if key == "ProcessCwds" {
			cancel()
		}
	}
	rep, err := Remove(ctx, f, ".", RemoveOptions{GatherOptions: GatherOptions{workers: 1}})
	if !errors.Is(err, context.Canceled) || rep != nil || len(mutations(f)) != 0 {
		t.Errorf("got %v, %v, calls %v", rep, err, mutations(f))
	}
}

// An interrupt during the check right before acting is an interrupt, not a change.
func TestRemove_CanceledDuringRecheck(t *testing.T) {
	f := removable(t)
	ctx, cancel := context.WithCancel(context.Background())
	lists := 0
	f.OnCall = func(key string) {
		if key == "ProcessCwds" {
			if lists++; lists == 2 {
				cancel()
			}
		}
	}
	rep, err := Remove(ctx, f, ".", RemoveOptions{Refs: []string{"merged"}, GatherOptions: GatherOptions{workers: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(rep)["worktree:"+R+"/wt-merged"]; got != "failed canceled" || len(mutations(f)) != 0 {
		t.Errorf("got %q, calls %v", got, mutations(f))
	}
}

func TestRemove_CanceledDuringHook(t *testing.T) {
	f := removable(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.Answers["RunHook "+R+"/wt-merged"] = conntest.Answer{Err: errors.New("signal: killed")}
	f.OnCall = func(key string) {
		if key == "RunHook "+R+"/wt-merged" {
			cancel()
		}
	}
	rep, err := Remove(ctx, f, ".", RemoveOptions{Refs: []string{"merged"}, GatherOptions: GatherOptions{workers: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(rep)["worktree:"+R+"/wt-merged"]; got != "failed canceled" || f.Called("RemoveWorktree") {
		t.Errorf("got %q, calls %v", got, mutations(f))
	}
}

func TestReportOK(t *testing.T) {
	ok := &Report{Results: []Result{{Result: Removed}, {Result: Removed}}}
	if !ok.OK() || (&Report{Problems: []string{"x"}}).OK() || (&Report{Results: []Result{{Result: Failed}}}).OK() {
		t.Error("OK")
	}
}
