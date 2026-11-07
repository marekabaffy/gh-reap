package reap

import (
	"errors"
	"strings"
	"testing"

	"github.com/marekabaffy/gh-reap/internal/conn"
)

const (
	tip  = "1111111111111111111111111111111111111111"
	head = "2222222222222222222222222222222222222222"
)

// finished is a branch whose merged PR has exactly its tip as head: REMOVE.
func finished() BranchFacts {
	return BranchFacts{
		Name: "feat", OID: tip, PR: &conn.PR{Number: 7, State: "MERGED", HeadOID: tip},
		HeadPresent: true, Ancestor: true,
	}
}

func TestDecideBranch(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(*BranchFacts)
		opt     Options
		verdict Verdict
		reason  string
		detail  string
	}{
		{"merged, tip is the PR head", nil, Options{}, VerdictRemove, ReasonPRMerged, "PR #7 merged"},
		{"merged, tip behind the PR head", func(b *BranchFacts) { b.PR.HeadOID = head }, Options{}, VerdictRemove, ReasonPRMerged, ""},
		{"closed, default", func(b *BranchFacts) { b.PR.State = "CLOSED" }, Options{}, VerdictKeep, ReasonPRClosed, "--state closed"},
		{"closed, --state closed", func(b *BranchFacts) { b.PR.State = "CLOSED" }, Options{IncludeClosed: true}, VerdictRemove, ReasonPRClosed, "PR #7 closed"},
		{"open", func(b *BranchFacts) { b.PR.State = "OPEN" }, Options{IncludeClosed: true}, VerdictKeep, ReasonPROpen, "PR #7 is open"},
		{"unknown state", func(b *BranchFacts) { b.PR.State = "DRAFTY" }, Options{}, VerdictKeep, ReasonPRUnknown, "DRAFTY"},
		{"no PR", func(b *BranchFacts) { b.PR = nil }, Options{}, VerdictKeep, ReasonNoPR, ""},
		{"PR read failed", func(b *BranchFacts) { b.PRErr = errors.New("HTTP 502") }, Options{}, VerdictKeep, ReasonPRUnknown, "HTTP 502"},
		{"keep mark beats a merged PR", func(b *BranchFacts) { b.Keep = true }, Options{}, VerdictKeep, ReasonKeepMark, "unkeep feat"},
		{"default branch", func(b *BranchFacts) { b.Protected = ReasonDefaultBranch }, Options{}, VerdictKeep, ReasonDefaultBranch, ""},
		{"main checkout's branch", func(b *BranchFacts) { b.Protected = ReasonCurrentBranch }, Options{}, VerdictKeep, ReasonCurrentBranch, ""},
		{"PR head not in this clone", func(b *BranchFacts) { b.HeadPresent, b.Ancestor = false, false }, Options{}, VerdictKeep, ReasonPRHeadMissing, "git fetch"},
		{"commits past the PR head, unpushed", func(b *BranchFacts) { b.Unpushed, b.Ancestor = 2, false }, Options{}, VerdictKeep, ReasonUnpushed, "2 commits"},
		{"one unpushed commit (merge commits count)", func(b *BranchFacts) { b.Unpushed, b.Ancestor = 1, false }, Options{}, VerdictKeep, ReasonUnpushed, "1 commit "},
		{"tip on a remote but not the PR", func(b *BranchFacts) { b.Ancestor = false }, Options{}, VerdictKeep, ReasonNotOnPR, "not PR #7's head"},
		{"a check failed", func(b *BranchFacts) { b.CheckErr = errors.New("git died") }, Options{}, VerdictKeep, ReasonCheckFailed, "git died"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := finished()
			pr := *b.PR
			b.PR = &pr
			if tt.edit != nil {
				tt.edit(&b)
			}
			d := DecideBranch(b, tt.opt)
			if d.Verdict != tt.verdict || d.Reason != tt.reason || !strings.Contains(d.Detail, tt.detail) {
				t.Errorf("got %s %s %q, want %s %s containing %q", d.Verdict, d.Reason, d.Detail, tt.verdict, tt.reason, tt.detail)
			}
		})
	}
}

func TestDecideTree(t *testing.T) {
	tests := []struct {
		name    string
		tree    func(*TreeFacts)
		branch  func(*BranchFacts) // nil branch facts when it sets name to ""
		verdict Verdict
		reason  string
		detail  string
	}{
		{"clean, unused, merged", nil, nil, VerdictRemove, ReasonPRMerged, ""},
		{"main checkout", func(t *TreeFacts) { t.Main = true }, nil, VerdictKeep, ReasonMainCheckout, ""},
		{"locked", func(t *TreeFacts) { t.Locked, t.LockReason = true, "usb disk" }, nil, VerdictKeep, ReasonLocked, "usb disk"},
		{"locked and gone stays", func(t *TreeFacts) { t.Locked, t.Missing = true, true }, nil, VerdictKeep, ReasonLocked, ""},
		{"folder gone", func(t *TreeFacts) { t.Missing = true }, func(b *BranchFacts) { b.PR.State = "OPEN" }, VerdictRemove, ReasonFolderGone, ""},
		{"detached", func(t *TreeFacts) { t.Detached = true }, nil, VerdictKeep, ReasonDetached, ""},
		{"contains another worktree", func(t *TreeFacts) { t.Contains = []string{"/w/feat/.worktrees/x"} }, nil, VerdictKeep, ReasonContainsTree, "/w/feat/.worktrees/x"},
		{"no branch facts", nil, func(b *BranchFacts) { b.Name = "" }, VerdictKeep, ReasonDetached, ""},
		{"marked keep", nil, func(b *BranchFacts) { b.Keep = true }, VerdictKeep, ReasonKeepMark, ""},
		{"default branch checked out", nil, func(b *BranchFacts) { b.Protected = ReasonDefaultBranch }, VerdictKeep, ReasonDefaultBranch, ""},
		{"in use", func(t *TreeFacts) {
			t.Holders = []conn.Process{{PID: 4, Command: "zsh"}, {PID: 5, Command: "vim"}, {PID: 6, Command: "node"}, {PID: 7, Command: "x"}}
		}, nil, VerdictKeep, ReasonInUse, "zsh (pid 4), vim (pid 5), node (pid 6), 1 more"},
		{"processes unknown", func(t *TreeFacts) { t.ProcessesErr = errors.New("lsof: not found") }, nil, VerdictKeep, ReasonProcessesUnknown, "lsof"},
		{"dirty", func(t *TreeFacts) { t.Dirty = 1 }, nil, VerdictKeep, ReasonDirty, "1 uncommitted or untracked file"},
		{"status failed", func(t *TreeFacts) { t.StatusErr = errors.New("index corrupt") }, nil, VerdictKeep, ReasonStatusFailed, "index corrupt"},
		{"PR open", nil, func(b *BranchFacts) { b.PR.State = "OPEN" }, VerdictKeep, ReasonPROpen, ""},
		{"unpushed", nil, func(b *BranchFacts) { b.Unpushed, b.Ancestor = 3, false }, VerdictKeep, ReasonUnpushed, ""},
		{"hook configured", func(t *TreeFacts) { t.Hook = conn.HookConfigured }, nil, VerdictRemove, ReasonPRMerged, ""},
		{"git too old for hooks", func(t *TreeFacts) { t.Hook = conn.HookUnsupported }, nil, VerdictKeep, ReasonGitTooOld, ""},
		{"hook check failed", func(t *TreeFacts) { t.Hook = conn.HookUnknown }, nil, VerdictKeep, ReasonHookUnknown, ""},
		{"too-old git does not hide a KEEP reason", func(t *TreeFacts) { t.Hook = conn.HookUnsupported }, func(b *BranchFacts) { b.PR.State = "OPEN" }, VerdictKeep, ReasonPROpen, ""},
		{"in use is reported before dirty", func(t *TreeFacts) { t.Dirty = 2; t.Holders = []conn.Process{{PID: 1, Command: "sh"}} }, nil, VerdictKeep, ReasonInUse, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tf := TreeFacts{Path: "/w/feat"}
			if tt.tree != nil {
				tt.tree(&tf)
			}
			b := finished()
			pr := *b.PR
			b.PR = &pr
			if tt.branch != nil {
				tt.branch(&b)
			}
			bp := &b
			if b.Name == "" {
				bp = nil
			}
			d := DecideTree(tf, bp, Options{})
			if d.Verdict != tt.verdict || d.Reason != tt.reason || !strings.Contains(d.Detail, tt.detail) {
				t.Errorf("got %s %s %q, want %s %s containing %q", d.Verdict, d.Reason, d.Detail, tt.verdict, tt.reason, tt.detail)
			}
		})
	}
}

func TestShortAndPlural(t *testing.T) {
	if short("") != "(none)" || short("abc") != "abc" || short(tip) != "11111111" {
		t.Error("short")
	}
	if plural(1, "a", "b") != "a" || plural(0, "a", "b") != "b" {
		t.Error("plural")
	}
}
