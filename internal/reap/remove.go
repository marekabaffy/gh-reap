package reap

import (
	"context"
	"fmt"

	"github.com/marekabaffy/gh-reap/internal/conn"
)

// Outcomes of one removal.
const (
	Removed = "removed"
	Failed  = "failed"
	Refused = "refused"
)

// Reason codes of a failed or refused removal (besides the list's KEEP reasons).
const (
	ReasonNotFound     = "not-found"
	ReasonChanged      = "changed-since-list"
	ReasonHookFailed   = "hook-failed"
	ReasonRemoveFailed = "worktree-remove-failed"
	ReasonBranchKept   = "branch-delete-failed"
	ReasonCanceled     = "canceled"
)

// Result is what happened to one chosen item.
type Result struct {
	Ref    string `json:"ref"`
	Kind   string `json:"kind,omitempty"`
	Branch string `json:"branch,omitempty"`
	Path   string `json:"path,omitempty"`
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// RemoveOptions choose what Remove acts on.
type RemoveOptions struct {
	GatherOptions
	// Refs names the items to remove; empty means every REMOVE item.
	Refs []string
	// Confirm, when set, sees the chosen items before anything is removed; false
	// cancels the run.
	Confirm func([]Item) bool
	// Progress, when set, receives each result as it happens.
	Progress func(Result)
}

// Report is the outcome of one Remove.
type Report struct {
	Root     string   `json:"repo"`
	Problems []string `json:"problems,omitempty"`
	Results  []Result `json:"results"`
	Canceled bool     `json:"-"` // Confirm said no, or an interrupt answered for it
}

// OK is true when every chosen item was removed and the list was complete.
func (r *Report) OK() bool {
	if len(r.Problems) > 0 {
		return false
	}
	for _, res := range r.Results {
		if res.Result != Removed {
			return false
		}
	}
	return true
}

// Remove lists the repository again, then removes the chosen REMOVE items one by
// one, checking each again right before it acts. Nothing is forced and nothing is
// retried another way: a failure leaves the item and is reported.
func Remove(ctx context.Context, c conn.Conn, dir string, opt RemoveOptions) (*Report, error) {
	common, err := c.CommonDir(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	unlock, err := Lock(common)
	if err != nil {
		return nil, err
	}
	defer unlock()
	inv, err := Gather(ctx, c, dir, opt.GatherOptions)
	if err != nil {
		return nil, err
	}
	rep := &Report{Root: inv.Root}
	if !inv.Complete {
		rep.Problems = inv.Problems
	}
	add := func(r Result) {
		rep.Results = append(rep.Results, r)
		if opt.Progress != nil {
			opt.Progress(r)
		}
	}

	var chosen []Item
	seen := map[string]bool{}
	pick := func(it Item) {
		if !seen[it.Ref] {
			seen[it.Ref] = true
			chosen = append(chosen, it)
		}
	}
	for _, ref := range opt.Refs {
		it := inv.Find(ref)
		switch {
		case it == nil:
			add(Result{Ref: ref, Result: Refused, Reason: ReasonNotFound, Detail: "not found"})
		case it.Verdict != VerdictRemove:
			add(Result{Ref: it.Ref, Kind: it.Kind, Branch: it.Branch, Path: it.Path, Result: Refused, Reason: it.Reason, Detail: it.Detail})
		default:
			pick(*it)
		}
	}
	if len(opt.Refs) == 0 {
		for _, it := range inv.Items {
			if it.Verdict == VerdictRemove {
				pick(it)
			}
		}
	}

	// Registrations of gone folders first, then worktrees (each takes its branch),
	// then branches no worktree holds.
	order := func(it Item) int {
		switch {
		case it.Reason == ReasonFolderGone:
			return 0
		case it.Kind == KindWorktree:
			return 1
		}
		return 2
	}
	if len(chosen) > 0 && opt.Confirm != nil && !opt.Confirm(chosen) {
		rep.Canceled = true
		return rep, nil
	}
	for pass := 0; pass < 3; pass++ {
		for _, it := range chosen {
			if order(it) != pass {
				continue
			}
			if ctx.Err() != nil {
				add(result(it, Failed, ReasonCanceled, "interrupted"))
				continue
			}
			add(removeOne(ctx, c, inv, it, opt))
		}
	}
	return rep, nil
}

func result(it Item, outcome, reason, detail string) Result {
	return Result{Ref: it.Ref, Kind: it.Kind, Branch: it.Branch, Path: it.Path, Result: outcome, Reason: reason, Detail: detail}
}

// recheck lists the item again, right now: fresh processes, status, branch and
// tip. It returns the fresh item, or the reason and detail why it may no longer
// be removed as listed.
func recheck(ctx context.Context, c conn.Conn, inv *Inventory, it Item, opt RemoveOptions) (*Item, string, string) {
	again := opt.GatherOptions
	again.only, again.reuse, again.Sizes = it.Ref, inv, false
	fresh, err := Gather(ctx, c, inv.Root, again)
	if err != nil {
		return nil, ReasonChanged, err.Error()
	}
	now := fresh.Find(it.Ref)
	switch {
	case now == nil:
		return nil, ReasonChanged, "no longer there"
	case now.Verdict != VerdictRemove:
		return nil, now.Reason, now.Detail
	case now.OID != it.OID || now.Branch != it.Branch:
		return nil, ReasonChanged, fmt.Sprintf("now at %s on %q, listed at %s on %q", short(now.OID), now.Branch, short(it.OID), it.Branch)
	case now.Reason != it.Reason:
		// e.g. a gone folder came back: act only on what was listed.
		return nil, ReasonChanged, now.Detail
	}
	return now, "", ""
}

func removeOne(ctx context.Context, c conn.Conn, inv *Inventory, it Item, opt RemoveOptions) Result {
	now, reason, why := recheck(ctx, c, inv, it, opt)
	if now == nil {
		if ctx.Err() != nil {
			return result(it, Failed, ReasonCanceled, "interrupted")
		}
		return result(it, Refused, reason, "changed: "+why)
	}
	it = *now

	force := it.PR != nil && it.PR.State == "MERGED"

	if it.Kind == KindWorktree {
		if it.Reason != ReasonFolderGone {
			if out, err := c.RunHook(ctx, it.Path); err != nil {
				if ctx.Err() != nil {
					return result(it, Failed, ReasonCanceled, "interrupted")
				}
				last := conn.LastLine(out)
				if conn.OutputHeld(err) {
					last = "a process it started still holds its output (redirect it: >/dev/null 2>&1)"
				}
				if last == "" {
					last = err.Error()
				}
				return result(it, Failed, ReasonHookFailed, "teardown hook failed: "+last)
			}
			// The hook may have changed the tree: left a process running, made it
			// dirty, moved or detached its HEAD. Check everything again.
			if now, reason, why := recheck(ctx, c, inv, it, opt); now == nil {
				if ctx.Err() != nil {
					return result(it, Failed, ReasonCanceled, "interrupted")
				}
				return result(it, Failed, reason, "changed by teardown hook: "+why)
			}
		}
		if out, err := c.RemoveWorktree(ctx, inv.Root, it.Path); err != nil {
			return result(it, Failed, ReasonRemoveFailed, "git worktree remove: "+lastOr(out, err))
		}
		if it.Reason == ReasonFolderGone {
			return result(it, Removed, it.Reason, "registration removed")
		}
	}
	if out, err := c.DeleteBranch(ctx, inv.Root, it.Branch); err != nil {
		// Only git's "not fully merged" refusal may be overridden; any other (the
		// branch is checked out somewhere) stands.
		if !force || !conn.NotFullyMerged(out) {
			return result(it, Failed, ReasonBranchKept, removedPrefix(it)+"branch kept: "+lastOr(out, err))
		}
		// A squash or rebase merge leaves the branch unmerged to git; the PR merged
		// with exactly this tip, so deleting it loses nothing. The delete is atomic
		// on that tip: if the branch has moved since, git refuses.
		if out, err := c.DeleteBranchAt(ctx, inv.Root, it.Branch, it.OID); err != nil {
			return result(it, Failed, ReasonBranchKept, removedPrefix(it)+"branch kept: "+lastOr(out, err))
		}
	}
	detail := "branch deleted"
	if it.Kind == KindWorktree {
		detail = "worktree and branch removed"
	}
	return result(it, Removed, it.Reason, detail)
}

func removedPrefix(it Item) string {
	if it.Kind == KindWorktree {
		return "worktree removed, "
	}
	return ""
}

func lastOr(out string, err error) string {
	if l := conn.LastLine(out); l != "" {
		return l
	}
	return err.Error()
}
