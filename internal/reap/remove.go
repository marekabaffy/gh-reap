package reap

import (
	"context"
	"fmt"
	"strings"

	"github.com/marekabaffy/gh-reap/internal/conn"
)

// Outcomes of one removal.
const (
	Removed     = "removed"
	WouldRemove = "would-remove"
	Failed      = "failed"
	Refused     = "refused"
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
	Refs   []string
	All    bool // every REMOVE item
	DryRun bool
	// Progress, when set, receives each result as it happens.
	Progress func(Result)
}

// Report is the outcome of one Remove.
type Report struct {
	Root     string   `json:"repo"`
	DryRun   bool     `json:"dry_run"`
	Problems []string `json:"problems,omitempty"`
	Results  []Result `json:"results"`
}

// OK is true when every chosen item was removed (or would be) and the list was complete.
func (r *Report) OK() bool {
	if len(r.Problems) > 0 {
		return false
	}
	for _, res := range r.Results {
		if res.Result != Removed && res.Result != WouldRemove {
			return false
		}
	}
	return true
}

// Remove lists the repository again, then removes the chosen REMOVE items one by
// one, checking each again right before it acts. Nothing is forced and nothing is
// retried another way: a failure leaves the item and is reported.
func Remove(ctx context.Context, c conn.Conn, dir string, opt RemoveOptions) (*Report, error) {
	if !opt.DryRun {
		common, err := c.CommonDir(ctx, dir)
		if err != nil {
			return nil, fmt.Errorf("not a git repository: %w", err)
		}
		unlock, err := Lock(common)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	inv, err := Gather(ctx, c, dir, opt.GatherOptions)
	if err != nil {
		return nil, err
	}
	rep := &Report{Root: inv.Root, DryRun: opt.DryRun}
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
			add(Result{Ref: ref, Result: Refused, Reason: ReasonNotFound, Detail: "no worktree or local branch by that name or path"})
		case it.Verdict != VerdictRemove:
			add(Result{Ref: it.Ref, Kind: it.Kind, Branch: it.Branch, Path: it.Path, Result: Refused, Reason: it.Reason, Detail: "listed KEEP: " + it.Detail})
		default:
			pick(*it)
		}
	}
	if opt.All {
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
	for pass := 0; pass < 3; pass++ {
		for _, it := range chosen {
			if order(it) != pass {
				continue
			}
			if ctx.Err() != nil {
				add(result(it, Failed, ReasonCanceled, "interrupted before this item"))
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
		return result(it, Refused, reason, "changed since the list: "+why)
	}
	it = *now

	force := it.PR != nil && it.PR.State == "MERGED"
	if opt.DryRun {
		var steps []string
		if it.Kind == KindWorktree {
			if it.Hook != nil && *it.Hook {
				steps = append(steps, "run the pre-worktree-remove hook")
			}
			steps = append(steps, "git worktree remove")
		}
		if it.Branch != "" && it.Reason != ReasonFolderGone {
			step := "git branch -d " + it.Branch
			if force {
				step += " (or delete it at the PR head if git does not see it merged)"
			}
			steps = append(steps, step)
		}
		return result(it, WouldRemove, it.Reason, strings.Join(steps, ", then "))
	}

	if it.Kind == KindWorktree {
		if it.Reason != ReasonFolderGone {
			if out, err := c.RunHook(ctx, it.Path); err != nil {
				if ctx.Err() != nil {
					return result(it, Failed, ReasonCanceled, "interrupted while the hook ran, tree kept")
				}
				last := conn.LastLine(out)
				if conn.OutputHeld(err) {
					last = "a process it started still holds its output (redirect it: >/dev/null 2>&1)"
				}
				if last == "" {
					last = err.Error()
				}
				return result(it, Failed, ReasonHookFailed, "pre-worktree-remove hook failed, tree kept: "+last)
			}
			// The hook may have changed the tree: left a process running, made it
			// dirty, moved or detached its HEAD. Check everything again.
			if now, reason, why := recheck(ctx, c, inv, it, opt); now == nil {
				return result(it, Failed, reason, "after the hook: "+why+"; tree kept")
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
