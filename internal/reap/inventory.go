package reap

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/marekabaffy/gh-reap/internal/conn"
)

// Kinds of item.
const (
	KindWorktree = "worktree"
	KindBranch   = "branch"
)

// ReasonIncomplete replaces REMOVE when the inventory could not be completed.
const ReasonIncomplete = "inventory-incomplete"

// Item is one line of the list: a worktree (with the branch it holds) or a local
// branch no worktree holds. Its JSON form is documented in docs/reference.md.
type Item struct {
	Kind    string   `json:"kind"`
	Ref     string   `json:"ref"`
	Verdict Verdict  `json:"verdict"`
	Reason  string   `json:"reason"`
	Detail  string   `json:"detail"`
	Branch  string   `json:"branch,omitempty"`
	OID     string   `json:"oid,omitempty"`
	Path    string   `json:"path,omitempty"`
	PR      *conn.PR `json:"pr,omitempty"`
	Hook    *bool    `json:"hook,omitempty"`
	SizeKB  *int64   `json:"size_kb,omitempty"`
}

// Inventory is the list for one repository.
type Inventory struct {
	Root      string   `json:"path"`
	GitHub    string   `json:"github,omitempty"`
	Complete  bool     `json:"complete"`
	Problems  []string `json:"problems,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	Items     []Item   `json:"items"`
	CommonDir string   `json:"-"`

	prs   *conn.PRResult
	prErr error
}

// GatherOptions control one Gather.
type GatherOptions struct {
	// Repo overrides the repository found from the remotes (GH_REPO).
	Repo *conn.Repo
	// KnownHost says whether a remote's host is a GitHub host gh can talk to.
	KnownHost func(host string) bool
	// Ignore drops processes that are gh reap itself (and the gh that launched it).
	Ignore func(conn.Process) bool
	// Sizes measures the folders of worktrees marked REMOVE.
	Sizes bool

	// only limits the gather to one item, reusing PR data from a previous one.
	only    string
	reuse   *Inventory
	workers int
}

// RefOf is the ref of a worktree path or branch name.
func RefOf(kind, name string) string { return kind + ":" + name }

// Gather lists the repository containing dir.
func Gather(ctx context.Context, c conn.Conn, dir string, opt GatherOptions) (*Inventory, error) {
	out, err := c.WorktreeList(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("not a git repository (or git is too old for worktree list -z): %w", err)
	}
	trees := conn.ParseWorktrees(out)
	if len(trees) == 0 {
		return nil, errors.New("git worktree list returned nothing")
	}
	inv := &Inventory{Root: trees[0].Path, Complete: true}
	root := inv.Root
	if inv.CommonDir, err = c.CommonDir(ctx, dir); err != nil {
		return nil, err
	}

	out, err = c.Branches(ctx, root)
	if err != nil {
		return nil, err
	}
	branches := conn.ParseBranches(out)
	out, err = c.KeepMarks(ctx, root)
	if err != nil {
		return nil, err
	}
	keepMarks := conn.ParseKeepMarks(out)

	// Which branch each tree holds; a tree whose folder is gone holds nothing,
	// so its branch is judged on its own once the registration is pruned.
	held := map[string]bool{}
	missing := map[string]bool{}
	for i, t := range trees {
		if i > 0 && !t.Locked && (t.Prunable || !c.Exists(t.Path)) {
			missing[t.Path] = true
			continue
		}
		if t.Branch != "" {
			held[t.Branch] = true
		}
	}

	// Protected branches: the main checkout's own, and the default branch as both
	// the local remote HEAD and GitHub know it.
	protected := map[string]string{}
	if trees[0].Branch != "" {
		protected[trees[0].Branch] = ReasonCurrentBranch
	}

	// Pull requests: one batched query, or reuse the previous list's answer.
	repo, remoteName := opt.Repo, ""
	var remotes []conn.Remote
	if out, err := c.Remotes(ctx, root); err == nil {
		remotes = conn.ParseRemotes(out)
		if repo == nil {
			known := opt.KnownHost
			if known == nil {
				known = func(h string) bool { return h == "github.com" }
			}
			if r, ok := conn.PickRepo(remotes, known); ok {
				repo = &r
			}
		}
		if repo != nil {
			remoteName = conn.RemoteOf(remotes, *repo)
		}
	}
	if remoteName != "" {
		if ref, err := c.RemoteHead(ctx, root, remoteName); err == nil && ref != "" {
			protected[strings.TrimPrefix(ref, "refs/remotes/"+remoteName+"/")] = ReasonDefaultBranch
		}
	}
	switch {
	case opt.reuse != nil:
		inv.prs, inv.prErr, inv.GitHub = opt.reuse.prs, opt.reuse.prErr, opt.reuse.GitHub
	case repo == nil:
		inv.prErr = errors.New("no remote points to a known GitHub host (set GH_REPO=owner/repo to choose)")
	default:
		inv.GitHub = repo.String()
		var names []string
		for _, b := range branches {
			names = append(names, b.Name)
		}
		res, err := c.PullRequests(ctx, *repo, conn.HeadRepos(*repo, remotes), names)
		if err != nil {
			inv.prErr = err
		} else {
			inv.prs = &res
		}
	}
	if inv.prErr != nil {
		inv.problem("pull requests unknown: %v", inv.prErr)
	}
	if inv.prs != nil && inv.prs.DefaultBranch != "" {
		protected[inv.prs.DefaultBranch] = ReasonDefaultBranch
	}

	wanted := func(ref string) bool { return opt.only == "" || opt.only == ref }

	// Branch facts, for every branch a wanted item needs.
	facts := map[string]*BranchFacts{}
	var jobs []func()
	for _, b := range branches {
		ref := RefOf(KindBranch, b.Name)
		if held[b.Name] {
			ref = ""
			for _, t := range trees {
				if t.Branch == b.Name && !missing[t.Path] {
					ref = RefOf(KindWorktree, t.Path)
				}
			}
		}
		if !wanted(ref) {
			continue
		}
		f := &BranchFacts{Name: b.Name, OID: b.OID, Keep: keepMarks[b.Name], Protected: protected[b.Name], PRErr: inv.prErr}
		facts[b.Name] = f
		if inv.prs == nil {
			continue
		}
		if pr, ok := inv.prs.ByBranch[b.Name]; ok {
			f.PR = &pr
			if pr.State == "MERGED" { // the only state whose commits the rules compare
				jobs = append(jobs, func() { branchChecks(ctx, c, root, f) })
			}
		}
	}

	// Processes, once for the whole repository.
	var procs []conn.Process
	procsOut, procsErr := c.ProcessCwds(ctx)
	if procsErr != nil {
		procsErr = fmt.Errorf("lsof: %w", procsErr)
		inv.problem("cannot see which processes use the worktrees (%v); nothing is marked REMOVE", procsErr)
		inv.Complete = false
	} else if all, err := conn.ParseProcesses(procsOut); err != nil {
		procsErr = fmt.Errorf("lsof: %w", err)
		inv.problem("cannot read which processes use the worktrees (%v); nothing is marked REMOVE", procsErr)
		inv.Complete = false
	} else {
		for _, p := range all {
			if opt.Ignore == nil || !opt.Ignore(p) {
				procs = append(procs, p)
			}
		}
	}

	tfacts := make([]*TreeFacts, len(trees))
	for i, t := range trees {
		ref := RefOf(KindWorktree, t.Path)
		if !wanted(ref) {
			continue
		}
		tf := &TreeFacts{
			Path: t.Path, Main: i == 0, Missing: missing[t.Path], Locked: t.Locked,
			LockReason: t.LockReason, Detached: t.Detached || t.Bare, ProcessesErr: procsErr,
		}
		tfacts[i] = tf
		if i > 0 && !tf.Missing {
			tf.Contains = treesInside(trees, missing, i)
		}
		if i == 0 || tf.Missing || !c.Exists(t.Path) {
			continue
		}
		tf.Holders = holdersOf(procs, t.Path)
		jobs = append(jobs, func() {
			out, err := c.Status(ctx, tf.Path)
			hookOut, hookErr := c.HookList(ctx, tf.Path)
			tf.StatusErr = err
			tf.Dirty = conn.ParseStatus(out)
			tf.Hook = conn.ParseHookList(hookOut, hookErr)
		})
	}
	parallel(jobs, opt.workers)

	unsupported, unknown := false, false
	for i, tf := range tfacts {
		if i == 0 || tf == nil || tf.Missing {
			continue
		}
		unsupported = unsupported || tf.Hook == conn.HookUnsupported
		unknown = unknown || tf.Hook == conn.HookUnknown
	}
	if unsupported {
		inv.Warnings = append(inv.Warnings, "git cannot list teardown hooks; worktree removal requires Git with config-based hooks (check which git is on PATH)")
	}
	if unknown {
		inv.Warnings = append(inv.Warnings, "teardown hooks could not be checked for some worktrees; those worktrees cannot be removed")
	}
	for i, t := range trees {
		tf := tfacts[i]
		if tf == nil {
			continue
		}
		item := Item{Kind: KindWorktree, Ref: RefOf(KindWorktree, t.Path), Path: t.Path, OID: t.HEAD}
		var bf *BranchFacts
		if !t.Detached && !t.Bare && t.Branch != "" {
			item.Branch = t.Branch
			if !tf.Missing {
				bf = facts[t.Branch]
				if bf == nil { // the branch was deleted under a live tree
					bf = &BranchFacts{Name: t.Branch, OID: t.HEAD, PRErr: errors.New("branch ref not found")}
				}
				// The branch tip, which removal compares before it deletes the branch.
				item.PR, item.OID = bf.PR, bf.OID
			}
		}
		if i > 0 && !tf.Missing && (tf.Hook == conn.HookConfigured || tf.Hook == conn.HookNone) {
			has := tf.Hook == conn.HookConfigured
			item.Hook = &has
		}
		d := DecideTree(*tf, bf)
		item.Verdict, item.Reason, item.Detail = d.Verdict, d.Reason, d.Detail
		inv.Items = append(inv.Items, item)
	}
	for _, b := range branches {
		f := facts[b.Name]
		if f == nil || held[b.Name] {
			continue
		}
		d := DecideBranch(*f)
		inv.Items = append(inv.Items, Item{
			Kind: KindBranch, Ref: RefOf(KindBranch, b.Name), Branch: b.Name,
			OID: b.OID, PR: f.PR, Verdict: d.Verdict, Reason: d.Reason, Detail: d.Detail,
		})
	}
	if inv.prErr != nil {
		inv.Complete = false
	}
	if !inv.Complete {
		for i := range inv.Items {
			if inv.Items[i].Verdict == VerdictRemove {
				inv.Items[i].Verdict, inv.Items[i].Reason = VerdictKeep, ReasonIncomplete
				inv.Items[i].Detail = "checks incomplete"
			}
		}
	}
	if opt.Sizes {
		inv.measure(ctx, c, opt.workers)
	}
	sort.SliceStable(inv.Items, func(i, j int) bool {
		a, b := inv.Items[i], inv.Items[j]
		if a.Kind != b.Kind {
			return a.Kind == KindWorktree
		}
		return false // worktrees keep git's order (main first); branches come sorted from git
	})
	// An interrupt kills the commands above mid-answer: what they said is no list.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return inv, nil
}

func (inv *Inventory) problem(format string, a ...any) {
	inv.Problems = append(inv.Problems, fmt.Sprintf(format, a...))
}

// Find returns the item a user's ref names: a ref as listed, a branch name, or a
// worktree path.
func (inv *Inventory) Find(ref string) *Item {
	for i, it := range inv.Items {
		if it.Ref == ref || (it.Branch != "" && it.Branch == ref) {
			return &inv.Items[i]
		}
	}
	if abs, err := filepath.Abs(ref); err == nil {
		abs = canonical(abs)
		for i, it := range inv.Items {
			if it.Path != "" && canonical(it.Path) == abs {
				return &inv.Items[i]
			}
		}
	}
	return nil
}

func branchChecks(ctx context.Context, c conn.Conn, dir string, f *BranchFacts) {
	head := f.PR.HeadOID
	ok, err := c.HasCommit(ctx, dir, head)
	if err != nil || !ok {
		f.CheckErr = err
		return
	}
	f.HeadPresent = true
	out, err := c.CountUnpushed(ctx, dir, f.OID, head)
	if err != nil {
		f.CheckErr = err
		return
	}
	if f.Unpushed, err = conn.ParseCount(out); err != nil {
		f.CheckErr = err
		return
	}
	f.Ancestor, f.CheckErr = c.IsAncestor(ctx, dir, f.OID, head)
}

func (inv *Inventory) measure(ctx context.Context, c conn.Conn, workers int) {
	var jobs []func()
	for i := range inv.Items {
		it := &inv.Items[i]
		if it.Kind != KindWorktree || it.Verdict != VerdictRemove || it.Reason == ReasonFolderGone {
			continue
		}
		jobs = append(jobs, func() {
			if out, err := c.DiskUsage(ctx, it.Path); err == nil {
				if kb, err := conn.ParseDiskUsage(out); err == nil {
					it.SizeKB = &kb
				}
			}
		})
	}
	parallel(jobs, workers)
}

// treesInside returns the other registered worktrees whose folders are inside
// trees[i]'s folder.
func treesInside(trees []conn.Worktree, missing map[string]bool, i int) []string {
	outer := canonical(trees[i].Path)
	var inside []string
	for j, o := range trees {
		if j != i && !missing[o.Path] && isInside(canonical(o.Path), outer) {
			inside = append(inside, o.Path)
		}
	}
	return inside
}

func isInside(p, dir string) bool { return strings.HasPrefix(p, dir+"/") }

// holdersOf returns the processes whose working directory is the tree or inside it.
func holdersOf(procs []conn.Process, tree string) []conn.Process {
	tree = canonical(tree)
	var hs []conn.Process
	for _, p := range procs {
		if p.Cwd == tree || isInside(p.Cwd, tree) {
			hs = append(hs, p)
		}
	}
	return hs
}

func parallel(jobs []func(), workers int) {
	if workers <= 0 {
		workers = 8
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			job()
		}()
	}
	wg.Wait()
}
