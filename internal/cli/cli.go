// Package cli is the gh reap command line: flags, help, and output.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cli/go-gh/v2/pkg/auth"
	"github.com/cli/go-gh/v2/pkg/repository"
	"github.com/marekabaffy/gh-reap/internal/conn"
	"github.com/marekabaffy/gh-reap/internal/reap"
	"golang.org/x/term"
)

// Exit codes.
const (
	ExitOK    = 0
	ExitFail  = 1 // an item failed or was refused, or the list is incomplete
	ExitUsage = 2
)

const usage = `Remove the worktrees and local branches of merged pull requests, safely.

USAGE
  gh reap [--all]              Show what can be removed, and what is blocked
  gh reap remove [<name>...]   Remove it, after you confirm; <name> is a branch or worktree path
  gh reap keep <branch>...     Never remove these branches
  gh reap unkeep <branch>...   Undo keep

FLAGS
  --all     Also show open PRs, branches without a PR, and kept branches
  --yes     remove: do not ask (needed without a terminal)
  --json    Print JSON (see docs/reference.md)
  --debug   Print each command run, to stderr
  -h, --help  Show this help

A branch goes only when its pull request merged and nothing on it is missing from
the PR or a remote; a worktree, also only when it is clean, unlocked and unused.
Its pre-worktree-remove git hook runs first.

EXIT STATUS
  0  success
  1  an item failed or was refused, the checks were incomplete, or another run holds the lock
  2  usage error
`

type flags struct {
	all   bool
	yes   bool
	json  bool
	debug bool
	help  bool
}

// Run runs gh reap with args (without the program name) and returns the exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if cmd == "help" {
		fmt.Fprint(stdout, usage)
		return ExitOK
	}
	var f flags
	fs := flag.NewFlagSet("gh reap", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&f.debug, "debug", false, "")
	fs.BoolVar(&f.help, "help", false, "")
	fs.BoolVar(&f.help, "h", false, "")
	switch cmd {
	case "":
		fs.BoolVar(&f.all, "all", false, "")
		fs.BoolVar(&f.json, "json", false, "")
	case "remove":
		fs.BoolVar(&f.yes, "yes", false, "")
		fs.BoolVar(&f.json, "json", false, "")
	case "keep", "unkeep":
	default:
		return usageError(stderr, "unknown command %q", cmd)
	}
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return usageError(stderr, "%v", err)
	}
	if f.help {
		fmt.Fprint(stdout, usage)
		return ExitOK
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	x := &conn.Exec{}
	if f.debug {
		x.Debug = stderr
	}
	opt, err := gatherOptions()
	if err != nil {
		return usageError(stderr, "%v", err)
	}

	switch cmd {
	case "":
		if len(pos) > 0 {
			return usageError(stderr, "unexpected %q (to remove it: gh reap remove %s)", pos[0], pos[0])
		}
		return runList(ctx, x, f, opt, stdout, stderr)
	case "remove":
		if !f.yes {
			switch {
			case f.json:
				return usageError(stderr, "--json needs --yes")
			case !interactive(stdin, stdout):
				return usageError(stderr, "no terminal to confirm; use --yes")
			}
		}
		return runRemove(ctx, x, f, opt, pos, stdin, stdout, stderr)
	default:
		if len(pos) == 0 {
			return usageError(stderr, "%s needs at least one branch", cmd)
		}
		return runKeep(ctx, x, cmd == "keep", pos, stdout, stderr)
	}
}

// interactive says whether a person can answer a question: stdin and stdout
// are both a terminal.
var interactive = func(stdin io.Reader, stdout io.Writer) bool {
	in, ok := stdin.(*os.File)
	out, ok2 := stdout.(*os.File)
	return ok && ok2 && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

func usageError(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "gh reap: "+format+"\n", a...)
	fmt.Fprintln(stderr, "Run 'gh reap --help' for usage.")
	return ExitUsage
}

// parseInterspersed lets flags follow positional arguments, as gh's own commands
// do. Everything after "--" is positional.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for i, a := range args {
		if a == "--" {
			args, rest = args[:i], args[i+1:]
			break
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return append(pos, rest...), nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

func gatherOptions() (reap.GatherOptions, error) {
	opt := reap.GatherOptions{
		KnownHost: knownHost,
		Ignore:    ignoreSelf(),
		Sizes:     true,
	}
	if v := os.Getenv("GH_REPO"); v != "" {
		r, err := repository.Parse(v)
		if err != nil {
			return opt, fmt.Errorf("GH_REPO: %w", err)
		}
		opt.Repo = &conn.Repo{Host: r.Host, Owner: r.Owner, Name: r.Name}
	}
	return opt, nil
}

func knownHost(host string) bool {
	if host == "github.com" {
		return true
	}
	for _, h := range auth.KnownHosts() {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// ignoreSelf drops gh reap itself, the commands it runs, and the gh that launched
// it from the processes that hold a tree; whoever started gh (a shell) still counts.
func ignoreSelf() func(conn.Process) bool {
	self, parent := os.Getpid(), os.Getppid()
	return func(p conn.Process) bool {
		return p.PID == self || p.PPID == self || (p.PID == parent && p.Command == "gh")
	}
}

func runList(ctx context.Context, c conn.Conn, f flags, opt reap.GatherOptions, stdout, stderr io.Writer) int {
	// Outside a repository, list every repository directly under this folder.
	dirs := []string{"."}
	discovered := false
	if _, err := c.CommonDir(ctx, "."); err != nil {
		found, ferr := reposUnder(ctx, c, ".")
		if ferr != nil || len(found) == 0 {
			fmt.Fprintln(stderr, "gh reap: not in a git repository, and no git repositories directly under this folder")
			return ExitFail
		}
		dirs = found
		discovered = true
	}

	code := ExitOK
	var invs []*reap.Inventory
	for _, d := range dirs {
		inv, err := reap.Gather(ctx, c, d, opt)
		if err != nil {
			fmt.Fprintf(stderr, "gh reap: %s: %v\n", d, err)
			code = ExitFail
			continue
		}
		if !inv.Complete {
			code = ExitFail
		}
		invs = append(invs, inv)
	}
	if f.json {
		if invs == nil {
			invs = []*reap.Inventory{}
		}
		writeJSON(stdout, map[string]any{"repos": invs})
	} else {
		for i, inv := range invs {
			if i > 0 {
				fmt.Fprintln(stdout)
			}
			printInventory(stdout, inv, discovered, f.all)
		}
	}
	return code
}

// reposUnder finds git repositories directly under dir, one per repository even
// when several of its worktrees sit side by side there.
func reposUnder(ctx context.Context, c conn.Conn, dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var dirs []string
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if info, err := os.Stat(p); err != nil || !info.IsDir() { // follows a symlink
			continue
		}
		bare := c.Exists(filepath.Join(p, "HEAD")) && c.Exists(filepath.Join(p, "objects")) && c.Exists(filepath.Join(p, "refs"))
		if !bare && !c.Exists(filepath.Join(p, ".git")) {
			continue
		}
		common, err := c.CommonDir(ctx, p)
		if err != nil || seen[common] {
			continue
		}
		seen[common] = true
		dirs = append(dirs, p)
	}
	sort.Strings(dirs)
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no git repositories directly under %s", dir)
	}
	return dirs, nil
}

func runRemove(ctx context.Context, c conn.Conn, f flags, opt reap.GatherOptions, refs []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ropt := reap.RemoveOptions{GatherOptions: opt, Refs: refs}
	ropt.Sizes = false
	if !f.json {
		ropt.Progress = func(r reap.Result) { printResult(stdout, r) }
	}
	if !f.yes {
		answers := bufio.NewReader(stdin)
		ropt.Confirm = func(items []reap.Item) bool { return confirm(stdout, answers, items) }
	}
	rep, err := reap.Remove(ctx, c, ".", ropt)
	if err != nil {
		fmt.Fprintf(stderr, "gh reap: %v\n", err)
		return ExitFail
	}
	if f.json {
		if rep.Results == nil {
			rep.Results = []reap.Result{}
		}
		writeJSON(stdout, rep)
	} else {
		printSummary(stdout, stderr, rep)
	}
	if rep.Canceled {
		return ExitOK
	}
	if !rep.OK() {
		return ExitFail
	}
	return ExitOK
}

func runKeep(ctx context.Context, c conn.Conn, keep bool, branches []string, stdout, stderr io.Writer) int {
	out, err := c.WorktreeList(ctx, ".")
	if err != nil {
		fmt.Fprintln(stderr, "gh reap: not in a git repository")
		return ExitFail
	}
	root := conn.ParseWorktrees(out)[0].Path
	out, err = c.Branches(ctx, root)
	if err != nil {
		fmt.Fprintf(stderr, "gh reap: %v\n", err)
		return ExitFail
	}
	exists := map[string]bool{}
	for _, b := range conn.ParseBranches(out) {
		exists[b.Name] = true
	}
	code := ExitOK
	for _, b := range branches {
		if !exists[b] {
			fmt.Fprintf(stderr, "gh reap: no local branch %q\n", b)
			code = ExitFail
			continue
		}
		if keep {
			err = c.SetKeep(ctx, root, b)
		} else {
			err = c.UnsetKeep(ctx, root, b)
		}
		if err != nil {
			fmt.Fprintf(stderr, "gh reap: %s: %v\n", b, err)
			code = ExitFail
			continue
		}
		if keep {
			fmt.Fprintf(stdout, "%s: marked keep\n", b)
		} else {
			fmt.Fprintf(stdout, "%s: unmarked\n", b)
		}
	}
	return code
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil && !errors.Is(err, os.ErrClosed) {
		fmt.Fprintln(os.Stderr, "gh reap:", err)
	}
}
