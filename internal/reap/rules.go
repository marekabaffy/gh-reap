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
	ReasonFolderGone = "folder-gone"

	// KEEP
	ReasonPRClosed         = "pr-closed"
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

// BranchFacts is everything the rules need to know about a local branch.
type BranchFacts struct {
	Name string
	OID  string
	Keep bool
	// Protected is ReasonDefaultBranch or ReasonCurrentBranch, or empty.
	Protected string

	// Remote is the git remote of the GitHub repository, or empty when none is.
	Remote string

	PR    *conn.PR // nil: no PR for this branch
	PRErr error    // the PR read failed: nothing is known

	// Known only for a merged PR, when PRErr == nil.
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
func DecideBranch(b BranchFacts) Decision {
	switch {
	case b.Keep:
		return keep(ReasonKeepMark, "marked keep")
	case b.Protected == ReasonDefaultBranch:
		return keep(ReasonDefaultBranch, "default branch")
	case b.Protected == ReasonCurrentBranch:
		return keep(ReasonCurrentBranch, "checked out in the main checkout")
	case b.PRErr != nil:
		return keep(ReasonPRUnknown, "PR unknown: "+b.PRErr.Error())
	case b.PR == nil:
		return keep(ReasonNoPR, "no PR")
	}
	pr := *b.PR
	switch pr.State {
	case "MERGED":
	case "CLOSED":
		return keep(ReasonPRClosed, fmt.Sprintf("PR #%d closed", pr.Number))
	case "OPEN":
		return keep(ReasonPROpen, fmt.Sprintf("PR #%d open", pr.Number))
	default:
		return keep(ReasonPRUnknown, fmt.Sprintf("PR #%d state %q", pr.Number, pr.State))
	}
	switch {
	case b.CheckErr != nil:
		return keep(ReasonCheckFailed, b.CheckErr.Error())
	case !b.HeadPresent:
		// GitHub keeps the head at refs/pull/<n>/head after the branch is deleted,
		// where a plain git fetch no longer finds it.
		detail := "PR head not fetched"
		if b.Remote != "" {
			detail += fmt.Sprintf(" (git fetch %s pull/%d/head)", b.Remote, pr.Number)
		}
		return keep(ReasonPRHeadMissing, detail)
	case b.Unpushed > 0:
		return keep(ReasonUnpushed, fmt.Sprintf("%d %s not in PR or remote", b.Unpushed, plural(b.Unpushed, "commit", "commits")))
	case !b.Ancestor:
		return keep(ReasonNotOnPR, fmt.Sprintf("tip %s not on PR head %s", short(b.OID), short(pr.HeadOID)))
	}
	return Decision{VerdictRemove, ReasonPRMerged, fmt.Sprintf("PR #%d merged", pr.Number)}
}

// DecideTree applies the worktree rules, then the PR rules to its branch. b is nil
// for a detached or bare worktree.
func DecideTree(t TreeFacts, b *BranchFacts) Decision {
	switch {
	case t.Main:
		return keep(ReasonMainCheckout, "main checkout")
	case t.Locked:
		detail := "locked"
		if t.LockReason != "" {
			detail += ": " + t.LockReason
		}
		return keep(ReasonLocked, detail)
	case t.Missing:
		return Decision{VerdictRemove, ReasonFolderGone, "folder gone"}
	case len(t.Contains) > 0:
		return keep(ReasonContainsTree, "contains worktree "+strings.Join(t.Contains, ", "))
	case t.Detached || b == nil:
		return keep(ReasonDetached, "detached HEAD")
	}
	if d := DecideBranch(*b); d.Verdict == VerdictKeep && (d.Reason == ReasonKeepMark || d.Reason == ReasonDefaultBranch || d.Reason == ReasonCurrentBranch) {
		return d
	}
	switch {
	case t.ProcessesErr != nil:
		return keep(ReasonProcessesUnknown, "processes unknown: "+t.ProcessesErr.Error())
	case len(t.Holders) > 0:
		return keep(ReasonInUse, "in use by "+holders(t.Holders))
	case t.StatusErr != nil:
		return keep(ReasonStatusFailed, "git status failed: "+t.StatusErr.Error())
	case t.Dirty > 0:
		return keep(ReasonDirty, fmt.Sprintf("%d uncommitted %s", t.Dirty, plural(t.Dirty, "file", "files")))
	}
	d := DecideBranch(*b)
	if d.Verdict == VerdictKeep {
		return d
	}
	switch t.Hook {
	case conn.HookUnsupported:
		return keep(ReasonGitTooOld, "git too old for hooks")
	case conn.HookUnknown:
		return keep(ReasonHookUnknown, "hook check failed")
	}
	return d
}

func holders(ps []conn.Process) string {
	var parts []string
	for i, p := range ps {
		if i == 1 {
			parts = append(parts, fmt.Sprintf("%d more", len(ps)-1))
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
