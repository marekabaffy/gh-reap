// Package reap decides which worktrees and branches of finished pull requests can
// go, and removes them. Deciding is pure (Decide*, over facts gathered by Gather);
// every external call goes through conn.Conn.
package reap

import (
	"fmt"
	"strings"

	"github.com/marekabaffy/gh-reap/internal/conn"
)

// Verdict is what list says about an item.
type Verdict string

const (
	VerdictRemove Verdict = "REMOVE"
	VerdictKeep   Verdict = "KEEP"
)

// Reason codes. Each item carries exactly one, the first rule that decided it.
// They are part of the JSON output and documented in docs/reference.md.
const (
	// REMOVE
	ReasonPRMerged   = "pr-merged"
	ReasonPRClosed   = "pr-closed"
	ReasonFolderGone = "folder-gone"

	// KEEP
	ReasonMainCheckout     = "main-checkout"
	ReasonContainsTree     = "contains-worktree"
	ReasonLocked           = "locked"
	ReasonDetached         = "detached"
	ReasonKeepMark         = "marked-keep"
	ReasonDefaultBranch    = "default-branch"
	ReasonCurrentBranch    = "current-branch"
	ReasonInUse            = "in-use"
	ReasonDirty            = "dirty"
	ReasonStatusFailed     = "status-failed"
	ReasonProcessesUnknown = "processes-unknown"
	ReasonNoPR             = "no-pr"
	ReasonPROpen           = "pr-open"
	ReasonPRUnknown        = "pr-unknown"
	ReasonPRHeadMissing    = "pr-head-missing"
	ReasonUnpushed         = "unpushed-commits"
	ReasonNotOnPR          = "not-on-pr-head"
	ReasonCheckFailed      = "check-failed"
	ReasonGitTooOld        = "git-too-old-for-hooks"
	ReasonHookUnknown      = "hook-check-failed"
)

// Options are the user's choices that change verdicts.
type Options struct {
	// IncludeClosed makes a CLOSED (unmerged) PR count as finished, like MERGED.
	IncludeClosed bool
}

// BranchFacts is everything the rules need to know about a local branch.
type BranchFacts struct {
	Name string
	OID  string
	Keep bool
	// Protected is ReasonDefaultBranch or ReasonCurrentBranch, or empty.
	Protected string

	PR    *conn.PR // nil: no PR for this branch
	PRErr error    // the PR read failed: nothing is known

	// Known only when PR != nil and PRErr == nil.
	HeadPresent bool  // the PR head commit exists locally
	Unpushed    int   // commits on the branch in neither the PR head nor any remote
	Ancestor    bool  // the branch tip is the PR head or an ancestor of it
	CheckErr    error // a git command needed above failed
}

// TreeFacts is everything the rules need to know about a registered worktree.
type TreeFacts struct {
	Path       string
	Main       bool // the main checkout (or the bare repository)
	Missing    bool // the folder is gone
	Locked     bool
	LockReason string
	Detached   bool
	// Contains lists other registered worktrees inside this one's folder: removing
	// it would delete them, even when git status sees nothing (an ignored folder).
	Contains []string

	Dirty     int // status entries, tracked and untracked
	StatusErr error

	Holders      []conn.Process
	ProcessesErr error // lsof failed or is missing: nobody can say the tree is free

	Hook conn.HookState
}

// Decision is a verdict and why.
type Decision struct {
	Verdict Verdict
	Reason  string
	Detail  string
}

func keep(reason, detail string) Decision { return Decision{VerdictKeep, reason, detail} }

// DecideBranch applies the PR rules to a branch, whether it stands alone or is
// checked out in a worktree.
func DecideBranch(b BranchFacts, opt Options) Decision {
	switch {
	case b.Keep:
		return keep(ReasonKeepMark, "marked keep (gh reap unkeep "+b.Name+" to undo)")
	case b.Protected == ReasonDefaultBranch:
		return keep(ReasonDefaultBranch, "the repository's default branch")
	case b.Protected == ReasonCurrentBranch:
		return keep(ReasonCurrentBranch, "checked out in the main checkout")
	case b.PRErr != nil:
		return keep(ReasonPRUnknown, "could not read pull requests: "+b.PRErr.Error())
	case b.PR == nil:
		return keep(ReasonNoPR, "no pull request from this repository or your remotes has this branch as its head")
	}
	pr := *b.PR
	switch pr.State {
	case "MERGED":
	case "CLOSED":
		if !opt.IncludeClosed {
			return keep(ReasonPRClosed, fmt.Sprintf("PR #%d closed without merging (--state closed to include)", pr.Number))
		}
	case "OPEN":
		return keep(ReasonPROpen, fmt.Sprintf("PR #%d is open", pr.Number))
	default:
		return keep(ReasonPRUnknown, fmt.Sprintf("PR #%d has unknown state %q", pr.Number, pr.State))
	}
	switch {
	case b.CheckErr != nil:
		return keep(ReasonCheckFailed, b.CheckErr.Error())
	case !b.HeadPresent:
		return keep(ReasonPRHeadMissing, fmt.Sprintf("PR #%d head %s is not in this clone (git fetch, then retry)", pr.Number, short(pr.HeadOID)))
	case b.Unpushed > 0:
		return keep(ReasonUnpushed, fmt.Sprintf("%d %s neither in PR #%d nor on any remote", b.Unpushed, plural(b.Unpushed, "commit", "commits"), pr.Number))
	case !b.Ancestor:
		return keep(ReasonNotOnPR, fmt.Sprintf("branch tip %s is not PR #%d's head %s or behind it", short(b.OID), pr.Number, short(pr.HeadOID)))
	}
	if pr.State == "CLOSED" {
		return Decision{VerdictRemove, ReasonPRClosed, fmt.Sprintf("PR #%d closed", pr.Number)}
	}
	return Decision{VerdictRemove, ReasonPRMerged, fmt.Sprintf("PR #%d merged", pr.Number)}
}

// DecideTree applies the worktree rules, then the PR rules to its branch. b is nil
// for a detached or bare worktree.
func DecideTree(t TreeFacts, b *BranchFacts, opt Options) Decision {
	switch {
	case t.Main:
		return keep(ReasonMainCheckout, "the main checkout")
	case t.Locked:
		detail := "locked with git worktree lock"
		if t.LockReason != "" {
			detail += ": " + t.LockReason
		}
		return keep(ReasonLocked, detail)
	case t.Missing:
		return Decision{VerdictRemove, ReasonFolderGone, "folder is gone; only git's registration is left"}
	case len(t.Contains) > 0:
		return keep(ReasonContainsTree, "contains the worktree "+strings.Join(t.Contains, ", "))
	case t.Detached || b == nil:
		return keep(ReasonDetached, "detached HEAD: no branch to match a PR")
	}
	if d := DecideBranch(*b, opt); d.Verdict == VerdictKeep && (d.Reason == ReasonKeepMark || d.Reason == ReasonDefaultBranch || d.Reason == ReasonCurrentBranch) {
		return d
	}
	switch {
	case t.ProcessesErr != nil:
		return keep(ReasonProcessesUnknown, "cannot see which processes use it: "+t.ProcessesErr.Error())
	case len(t.Holders) > 0:
		return keep(ReasonInUse, "in use by "+holders(t.Holders))
	case t.StatusErr != nil:
		return keep(ReasonStatusFailed, "git status failed: "+t.StatusErr.Error())
	case t.Dirty > 0:
		return keep(ReasonDirty, fmt.Sprintf("%d uncommitted or untracked %s", t.Dirty, plural(t.Dirty, "file", "files")))
	}
	d := DecideBranch(*b, opt)
	if d.Verdict == VerdictKeep {
		return d
	}
	switch t.Hook {
	case conn.HookUnsupported:
		return keep(ReasonGitTooOld, "this git cannot run a pre-worktree-remove hook (needs config-based hooks), so teardown cannot run")
	case conn.HookUnknown:
		return keep(ReasonHookUnknown, "could not check for a pre-worktree-remove hook")
	}
	return d
}

func holders(ps []conn.Process) string {
	var parts []string
	for i, p := range ps {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("%d more", len(ps)-3))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (pid %d)", p.Command, p.PID))
	}
	return strings.Join(parts, ", ")
}

func short(oid string) string {
	if len(oid) > 8 {
		return oid[:8]
	}
	if oid == "" {
		return "(none)"
	}
	return oid
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
