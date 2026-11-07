package conn

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Worktree is one entry of `git worktree list --porcelain -z`.
type Worktree struct {
	Path       string
	HEAD       string
	Branch     string // short name; empty when detached or bare
	Bare       bool
	Detached   bool
	Locked     bool
	LockReason string
	Prunable   bool
}

// ParseWorktrees reads `git worktree list --porcelain -z`: attributes separated by
// NUL, records by an empty attribute. The first record is the main worktree.
func ParseWorktrees(out string) []Worktree {
	var trees []Worktree
	var cur *Worktree
	for _, field := range strings.Split(out, "\x00") {
		key, value, _ := strings.Cut(field, " ")
		switch key {
		case "":
			cur = nil
		case "worktree":
			trees = append(trees, Worktree{Path: value})
			cur = &trees[len(trees)-1]
		case "HEAD":
			if cur != nil {
				cur.HEAD = value
			}
		case "branch":
			if cur != nil {
				cur.Branch = strings.TrimPrefix(value, "refs/heads/")
			}
		case "bare":
			if cur != nil {
				cur.Bare = true
			}
		case "detached":
			if cur != nil {
				cur.Detached = true
			}
		case "locked":
			if cur != nil {
				cur.Locked, cur.LockReason = true, value
			}
		case "prunable":
			if cur != nil {
				cur.Prunable = true
			}
		}
	}
	return trees
}

// Branch is a local branch and the commit it points to.
type Branch struct {
	Name string
	OID  string
}

// ParseBranches reads "name<TAB>oid" lines. Ref names cannot hold a tab or newline.
func ParseBranches(out string) []Branch {
	var bs []Branch
	for _, line := range lines(out) {
		name, oid, ok := strings.Cut(line, "\t")
		if ok {
			bs = append(bs, Branch{Name: name, OID: oid})
		}
	}
	return bs
}

// ParseKeepMarks reads `git config --type=bool --get-regexp` output for
// branch.<name>.reap-keep. A branch name may itself contain dots.
func ParseKeepMarks(out string) map[string]bool {
	marks := map[string]bool{}
	for _, line := range lines(out) {
		key, value, _ := strings.Cut(line, " ")
		name, ok := strings.CutPrefix(key, "branch.")
		if !ok {
			continue
		}
		name, ok = strings.CutSuffix(name, ".reap-keep")
		if ok && value == "true" {
			marks[name] = true
		}
	}
	return marks
}

// Remote is a git remote, with the repository gh resolved it to, if any.
type Remote struct {
	Name     string
	URL      string
	Resolved string // remote.<name>.gh-resolved: "base" or OWNER/REPO
}

// ParseRemotes reads `git config --get-regexp '^remote\..*\.(url|gh-resolved)$'`,
// keeping each remote's first url.
func ParseRemotes(out string) []Remote {
	byName := map[string]*Remote{}
	var order []string
	for _, line := range lines(out) {
		key, value, _ := strings.Cut(line, " ")
		rest, ok := strings.CutPrefix(key, "remote.")
		if !ok {
			continue
		}
		dot := strings.LastIndex(rest, ".")
		if dot < 0 {
			continue
		}
		name, attr := rest[:dot], rest[dot+1:]
		r := byName[name]
		if r == nil {
			r = &Remote{Name: name}
			byName[name] = r
			order = append(order, name)
		}
		switch attr {
		case "url":
			if r.URL == "" {
				r.URL = value
			}
		case "gh-resolved":
			r.Resolved = value
		}
	}
	remotes := make([]Remote, 0, len(order))
	for _, name := range order {
		remotes = append(remotes, *byName[name])
	}
	return remotes
}

var scpLike = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// ParseRemoteURL turns a git remote URL (https, ssh, git, or scp-like
// user@host:owner/repo) into a host and OWNER/REPO.
func ParseRemoteURL(raw string) (Repo, bool) {
	if !strings.Contains(raw, "://") {
		m := scpLike.FindStringSubmatch(raw)
		if m == nil {
			return Repo{}, false
		}
		raw = "ssh://" + m[1] + "/" + strings.TrimPrefix(m[2], "/")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return Repo{}, false
	}
	path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	owner, name, ok := strings.Cut(path, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Repo{}, false
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "ssh.")
	return Repo{Host: host, Owner: owner, Name: name}, true
}

// PickRepo chooses the remote gh would: one marked by `gh repo set-default`, then
// upstream, github, origin, then the rest by name. Only remotes on a known host count.
func PickRepo(remotes []Remote, known func(host string) bool) (Repo, bool) {
	rank := func(r Remote) int {
		switch {
		case r.Resolved != "":
			return 0
		case r.Name == "upstream":
			return 1
		case r.Name == "github":
			return 2
		case r.Name == "origin":
			return 3
		}
		return 4
	}
	sorted := append([]Remote(nil), remotes...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, rj := rank(sorted[i]), rank(sorted[j])
		if ri != rj {
			return ri < rj
		}
		return sorted[i].Name < sorted[j].Name
	})
	for _, r := range sorted {
		repo, ok := ParseRemoteURL(r.URL)
		if !ok || !known(repo.Host) {
			continue
		}
		if r.Resolved != "" && r.Resolved != "base" {
			if owner, name, ok := strings.Cut(r.Resolved, "/"); ok {
				repo.Owner, repo.Name = owner, name
			}
		}
		return repo, true
	}
	return Repo{}, false
}

// HeadRepos are the repositories whose pull requests are yours: repo itself and
// every remote on its host (your fork).
func HeadRepos(repo Repo, remotes []Remote) []Repo {
	heads := []Repo{repo}
	for _, r := range remotes {
		if got, ok := ParseRemoteURL(r.URL); ok && got.Host == repo.Host {
			heads = append(heads, got)
		}
	}
	return heads
}

// RemoteOf is the remote name of the chosen repository, for reading its HEAD.
func RemoteOf(remotes []Remote, repo Repo) string {
	for _, r := range remotes {
		if got, ok := ParseRemoteURL(r.URL); ok && got.Host == repo.Host &&
			strings.EqualFold(got.Owner, repo.Owner) && strings.EqualFold(got.Name, repo.Name) {
			return r.Name
		}
	}
	return ""
}

// ParseStatus counts entries in `git status --porcelain -z`. A rename or copy
// carries its source path as one extra NUL-separated field.
func ParseStatus(out string) int {
	n := 0
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 3 {
			continue
		}
		n++
		if f[0] == 'R' || f[0] == 'C' {
			i++
		}
	}
	return n
}

// Process is a running process and its working directory.
type Process struct {
	PID     int
	PPID    int
	Command string
	Cwd     string
}

// ParseProcesses reads `lsof -FpcRn` field output: p<pid>, R<parent pid>,
// c<command>, f<fd>, n<name>, one field per line, a new p line starting each process.
// Output it cannot read is an error, never a process quietly dropped: a dropped
// process could be the one working inside a tree.
func ParseProcesses(out string) ([]Process, error) {
	var procs []Process
	var cur Process
	for _, line := range lines(out) {
		if line == "" {
			continue
		}
		var err error
		switch line[0] {
		case 'p':
			cur = Process{}
			if cur.PID, err = strconv.Atoi(line[1:]); err == nil && cur.PID <= 0 {
				err = errors.New("not a pid")
			}
		case 'R':
			cur.PPID, err = strconv.Atoi(line[1:])
		case 'c':
			cur.Command = line[1:]
		case 'n':
			if cur.PID == 0 {
				err = errors.New("a name before any process")
			}
			p := cur
			p.Cwd = line[1:]
			procs = append(procs, p)
		}
		if err != nil {
			return nil, fmt.Errorf("unreadable lsof line %q: %w", line, err)
		}
	}
	return procs, nil
}

// HookState is whether a worktree has a pre-worktree-remove hook.
type HookState int

const (
	HookNone        HookState = iota // nothing registered
	HookConfigured                   // at least one hook registered
	HookUnsupported                  // this git cannot run config-based hooks by event name
	HookUnknown                      // the check failed for another reason
)

// ParseHookList reads the result of `git hook list --allow-unknown-hook-name <event>`:
// exit 0 lists hooks, exit 1 with "no hooks found" means none. Anything else (an
// unknown option, an unknown command) means this git is too old.
func ParseHookList(out string, err error) HookState {
	if err == nil {
		if strings.TrimSpace(out) == "" {
			return HookNone
		}
		return HookConfigured
	}
	var ce *CmdError
	if !errors.As(err, &ce) {
		return HookUnknown
	}
	switch {
	case ce.ExitCode == 1 && strings.Contains(ce.Stderr, "no hooks found"):
		return HookNone
	case ce.ExitCode == 129, strings.Contains(ce.Stderr, "unknown option"),
		strings.Contains(ce.Stderr, "is not a git command"), strings.Contains(ce.Stderr, "unknown subcommand"):
		return HookUnsupported
	}
	return HookUnknown
}

// ParseDiskUsage reads `du -sk`: kilobytes, a tab, the path.
func ParseDiskUsage(out string) (int64, error) {
	f := strings.Fields(out)
	if len(f) == 0 {
		return 0, fmt.Errorf("empty du output")
	}
	return strconv.ParseInt(f[0], 10, 64)
}

// ParseCount reads a single integer, as printed by `git rev-list --count`.
func ParseCount(out string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(out))
}

func lines(out string) []string {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}
