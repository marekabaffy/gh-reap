package reap

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/marekabaffy/gh-reap/internal/conn"
	"github.com/marekabaffy/gh-reap/internal/conn/conntest"
)

const R = conntest.Root

// Object ids of the fixture repository (internal/conn/testdata/branches.txt).
var oid = map[string]string{}

// world is the fixture repository as recorded by the contract tests, with PR
// answers chosen to exercise one rule per branch:
//
//	main        main checkout                       KEEP main-checkout
//	wt-detached detached HEAD                       KEEP detached
//	dirty       merged, untracked/modified files    KEEP dirty
//	gone        folder gone (PR open)               worktree REMOVE folder-gone, branch KEEP pr-open
//	locked      merged, git worktree lock           KEEP locked
//	merged      merged, clean                       REMOVE pr-merged
//	unpushed    merged, one commit past the PR      KEEP unpushed-commits
//	kept        merged, marked keep                 KEEP marked-keep
//	level       closed                              KEEP pr-closed
//	nopr        no PR                               KEEP no-pr
//	release.v1  marked keep (dotted name)           KEEP marked-keep
//	squashed    merged, tip is PR head              REMOVE pr-merged
func world(t *testing.T) *conntest.Fake {
	t.Helper()
	f := conntest.New()
	repo := R + "/repo"
	for _, b := range conn.ParseBranches(conntest.Fixture(t, "branches.txt")) {
		oid[b.Name] = b.OID
	}
	for _, dir := range []string{".", repo} {
		f.Set("WorktreeList "+dir, conntest.Fixture(t, "worktree_list.bin"))
		f.Set("CommonDir "+dir, conntest.Fixture(t, "common_dir.txt"))
	}
	f.Set("Branches "+repo, conntest.Fixture(t, "branches.txt"))
	f.Set("KeepMarks "+repo, conntest.Fixture(t, "keep_marks.txt"))
	f.Set("Remotes "+repo, conntest.Fixture(t, "remotes.txt"))
	f.Set("RemoteHead "+repo+" origin", conntest.Fixture(t, "remote_head.txt"))
	f.Set("ProcessCwds", conntest.Fixture(t, "lsof.txt"))
	for _, tree := range []string{"wt-detached", "wt-locked", "wt-merged", "wt-unpushed"} {
		f.Set("Status "+R+"/"+tree, "")
		f.Set("HookList "+R+"/"+tree, conntest.Fixture(t, "hook_list_configured.txt"))
	}
	f.Set("Status "+R+"/wt-dirty", conntest.Fixture(t, "status_dirty.bin"))
	f.Answers["HookList "+R+"/wt-dirty"] = conntest.Fail(1, conntest.Fixture(t, "hook_list_none_stderr.txt"))
	f.Set("DiskUsage "+R+"/wt-merged", "48\t"+R+"/wt-merged\n")
	f.Missing[R+"/wt-gone"] = true

	pushedUnpushed := strings.Repeat("3", 40)
	f.PRs.ByBranch = map[string]conn.PR{
		"merged":   {Number: 1, State: "MERGED", HeadOID: oid["merged"]},
		"unpushed": {Number: 2, State: "MERGED", HeadOID: pushedUnpushed},
		"dirty":    {Number: 3, State: "MERGED", HeadOID: oid["dirty"]},
		"locked":   {Number: 4, State: "MERGED", HeadOID: oid["locked"]},
		"gone":     {Number: 5, State: "OPEN", HeadOID: oid["gone"]},
		"squashed": {Number: 6, State: "MERGED", HeadOID: oid["squashed"]},
		"kept":     {Number: 7, State: "MERGED", HeadOID: oid["kept"]},
		"level":    {Number: 8, State: "CLOSED", HeadOID: oid["level"]},
	}
	f.PRs.DefaultBranch = "main"
	for _, b := range []string{"merged", "dirty", "locked", "squashed", "kept", "level"} {
		f.Bools["HasCommit "+oid[b]] = true
		f.Bools["IsAncestor "+oid[b]+" "+oid[b]] = true
		f.Set("CountUnpushed "+oid[b]+" "+oid[b], "0\n")
	}
	f.Bools["HasCommit "+pushedUnpushed] = true
	f.Set("CountUnpushed "+oid["unpushed"]+" "+pushedUnpushed, conntest.Fixture(t, "count_unpushed.txt"))
	f.Bools["IsAncestor "+oid["unpushed"]+" "+pushedUnpushed] = false
	return f
}

func gather(t *testing.T, f *conntest.Fake, opt GatherOptions) *Inventory {
	t.Helper()
	opt.workers = 1
	inv, err := Gather(context.Background(), f, ".", opt)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

type want struct{ verdict, reason string }

func check(t *testing.T, inv *Inventory, wants map[string]want) {
	t.Helper()
	if len(inv.Items) != len(wants) {
		var refs []string
		for _, it := range inv.Items {
			refs = append(refs, it.Ref)
		}
		t.Errorf("got %d items %v, want %d", len(inv.Items), refs, len(wants))
	}
	for ref, w := range wants {
		it := inv.Find(ref)
		if it == nil {
			t.Errorf("%s: missing", ref)
			continue
		}
		if string(it.Verdict) != w.verdict || it.Reason != w.reason {
			t.Errorf("%s: got %s %s (%s), want %s %s", ref, it.Verdict, it.Reason, it.Detail, w.verdict, w.reason)
		}
	}
}

func everyRule() map[string]want {
	return map[string]want{
		"worktree:" + R + "/repo":        {"KEEP", ReasonMainCheckout},
		"worktree:" + R + "/wt-detached": {"KEEP", ReasonDetached},
		"worktree:" + R + "/wt-dirty":    {"KEEP", ReasonDirty},
		"worktree:" + R + "/wt-gone":     {"REMOVE", ReasonFolderGone},
		"worktree:" + R + "/wt-locked":   {"KEEP", ReasonLocked},
		"worktree:" + R + "/wt-merged":   {"REMOVE", ReasonPRMerged},
		"worktree:" + R + "/wt-unpushed": {"KEEP", ReasonUnpushed},
		"branch:gone":                    {"KEEP", ReasonPROpen},
		"branch:kept":                    {"KEEP", ReasonKeepMark},
		"branch:level":                   {"KEEP", ReasonPRClosed},
		"branch:nopr":                    {"KEEP", ReasonNoPR},
		"branch:release.v1":              {"KEEP", ReasonKeepMark},
		"branch:squashed":                {"REMOVE", ReasonPRMerged},
	}
}

func TestGather_FixtureRepo(t *testing.T) {
	f := world(t)
	inv := gather(t, f, GatherOptions{Sizes: true})
	check(t, inv, everyRule())
	if !inv.Complete || len(inv.Problems) != 0 || inv.GitHub != "acme/widgets" || inv.Root != R+"/repo" || inv.CommonDir != R+"/repo/.git" {
		t.Errorf("inventory: %+v", inv)
	}
	merged := inv.Find("merged")
	if merged.SizeKB == nil || *merged.SizeKB != 48 || merged.Hook == nil || !*merged.Hook || merged.PR.Number != 1 || merged.OID != oid["merged"] {
		t.Errorf("merged: %+v", merged)
	}
	if dirty := inv.Find("worktree:" + R + "/wt-dirty"); dirty.Hook == nil || *dirty.Hook || dirty.Detail != "3 uncommitted or untracked files" {
		t.Errorf("dirty: %+v", dirty)
	}
	if inv.Items[0].Ref != "worktree:"+R+"/repo" || inv.Items[len(inv.Items)-1].Kind != KindBranch {
		t.Error("order: worktrees (main first) before branches")
	}
	if inv.Find("worktree:"+R+"/wt-gone").Hook != nil {
		t.Error("a gone folder has no hook answer")
	}
	// The PR question is asked once, and never sized a KEEP tree.
	n := 0
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "PullRequests") {
			n++
			if c != "PullRequests github.com/acme/widgets" {
				t.Errorf("asked %s", c)
			}
		}
		if strings.HasPrefix(c, "DiskUsage") && c != "DiskUsage "+R+"/wt-merged" {
			t.Errorf("sized %s", c)
		}
	}
	if n != 1 {
		t.Errorf("PullRequests called %d times", n)
	}
}

func TestGather_ClosedState(t *testing.T) {
	inv := gather(t, world(t), GatherOptions{Options: Options{IncludeClosed: true}})
	w := everyRule()
	w["branch:level"] = want{"REMOVE", ReasonPRClosed}
	check(t, inv, w)
}

func TestGather_InUse(t *testing.T) {
	f := world(t)
	lsof := conntest.Fixture(t, "lsof.txt") + "p4242\nR1\nczsh\nfcwd\nn" + R + "/wt-merged/src\np4343\nR1\ncvim\nfcwd\nn" + R + "/wt-merged-sibling\n"
	f.Set("ProcessCwds", lsof)
	inv := gather(t, f, GatherOptions{})
	it := inv.Find("merged")
	if it.Reason != ReasonInUse || !strings.Contains(it.Detail, "zsh (pid 4242)") || strings.Contains(it.Detail, "vim") {
		t.Errorf("got %s %q", it.Reason, it.Detail)
	}
	// gh reap's own processes do not count.
	inv = gather(t, f, GatherOptions{Ignore: func(p conn.Process) bool { return p.PID == 4242 }})
	if it := inv.Find("merged"); it.Verdict != VerdictRemove {
		t.Errorf("ignored holder: %s %s", it.Verdict, it.Detail)
	}
}

func TestGather_Incomplete(t *testing.T) {
	tests := []struct {
		name   string
		edit   func(*conntest.Fake)
		reason string // of the merged worktree
		branch string // of the squashed branch
	}{
		{"lsof missing", func(f *conntest.Fake) {
			f.Answers["ProcessCwds"] = conntest.Answer{Err: &conn.CmdError{Cmd: "lsof", ExitCode: -1, Err: errors.New("executable file not found")}}
		}, ReasonProcessesUnknown, ReasonIncomplete},
		{"GitHub unreachable", func(f *conntest.Fake) { f.PRErr = errors.New("HTTP 502") }, ReasonPRUnknown, ReasonPRUnknown},
		{"no GitHub remote", func(f *conntest.Fake) { f.Set("Remotes "+R+"/repo", "remote.origin.url /srv/git/widgets.git\n") }, ReasonPRUnknown, ReasonPRUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := world(t)
			tt.edit(f)
			inv := gather(t, f, GatherOptions{Sizes: true})
			if inv.Complete || len(inv.Problems) == 0 {
				t.Errorf("complete %v problems %v", inv.Complete, inv.Problems)
			}
			for _, it := range inv.Items {
				if it.Verdict == VerdictRemove {
					t.Errorf("%s is REMOVE in an incomplete list", it.Ref)
				}
			}
			if r := inv.Find("merged").Reason; r != tt.reason {
				t.Errorf("merged: %s, want %s", r, tt.reason)
			}
			if r := inv.Find("squashed").Reason; r != tt.branch {
				t.Errorf("squashed: %s, want %s", r, tt.branch)
			}
			if f.Called("DiskUsage") {
				t.Error("sized a tree nobody may remove")
			}
		})
	}
}

func TestGather_RepoOverrideAndKnownHosts(t *testing.T) {
	f := world(t)
	inv := gather(t, f, GatherOptions{Repo: &conn.Repo{Host: "ghe.example.com", Owner: "corp", Name: "w"}})
	if !f.Called("PullRequests ghe.example.com/corp/w") || inv.GitHub != "corp/w" {
		t.Errorf("override: %v", f.Calls)
	}
	f = world(t)
	inv = gather(t, f, GatherOptions{KnownHost: func(string) bool { return false }})
	if f.Called("PullRequests") || inv.Complete {
		t.Error("asked a host gh does not know")
	}
}

func TestGather_DefaultBranchFromGitHub(t *testing.T) {
	f := world(t)
	f.PRs.DefaultBranch = "nopr" // GitHub says the default is a branch git calls nopr
	inv := gather(t, f, GatherOptions{})
	if it := inv.Find("branch:nopr"); it.Reason != ReasonDefaultBranch {
		t.Errorf("got %s", it.Reason)
	}
}

func TestGather_Errors(t *testing.T) {
	f := conntest.New()
	f.Answers["WorktreeList ."] = conntest.Fail(128, "fatal: not a git repository")
	if _, err := Gather(context.Background(), f, ".", GatherOptions{}); err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("got %v", err)
	}
	f = world(t)
	f.Answers["Branches "+R+"/repo"] = conntest.Fail(128, "fatal: bad")
	if _, err := Gather(context.Background(), f, ".", GatherOptions{}); err == nil {
		t.Error("branch list failure ignored")
	}
}

func TestFind(t *testing.T) {
	inv := &Inventory{Items: []Item{
		{Kind: KindWorktree, Ref: "worktree:/w/a", Branch: "a", Path: "/w/a"},
		{Kind: KindBranch, Ref: "branch:b", Branch: "b"},
	}}
	for ref, want := range map[string]string{
		"worktree:/w/a": "worktree:/w/a", "a": "worktree:/w/a", "/w/a": "worktree:/w/a", "/w/a/": "worktree:/w/a",
		"branch:b": "branch:b", "b": "branch:b",
	} {
		if it := inv.Find(ref); it == nil || it.Ref != want {
			t.Errorf("%s: got %+v", ref, it)
		}
	}
	if inv.Find("c") != nil || inv.Find("branch:a") != nil {
		t.Error("found what is not there")
	}
}
