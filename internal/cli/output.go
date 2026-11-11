package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	ghterm "github.com/cli/go-gh/v2/pkg/term"
	"github.com/marekabaffy/gh-reap/internal/conn"
	"github.com/marekabaffy/gh-reap/internal/reap"
)

// The list groups items by what you can do about them: remove them; unblock
// them (the PR merged, but something you can fix holds them); or nothing yet,
// which --all shows and the list otherwise only counts.
const (
	gRemove = iota
	gBlocked
	gOpen
	gClosed
	gNoPR
	gKeep
	gLocked
	gOther
	nGroups
)

var groupNames = [nGroups]struct{ title, one, many string }{
	gRemove:  {"To remove", "", ""},
	gBlocked: {"Blocked", "", ""},
	gOpen:    {"Open PR", "open PR", "open PRs"},
	gClosed:  {"Closed PR", "closed PR", "closed PRs"},
	gNoPR:    {"Without a PR", "without a PR", "without a PR"},
	gKeep:    {"Marked keep", "marked keep", "marked keep"},
	gLocked:  {"Locked", "locked", "locked"},
	gOther:   {"Other", "other", "other"},
}

// ANSI styles, from the 16 colors every terminal theme defines. They mean what
// they mean in gh: magenta merged, green open or done, red closed or failed,
// yellow a warning, cyan a branch.
const (
	bold    = "1"
	dim     = "2"
	red     = "31"
	green   = "32"
	yellow  = "33"
	magenta = "35"
	cyan    = "36"
)

type sectionStyle struct{ status, info string }

var groupStyles = [nGroups]sectionStyle{
	gRemove:  {magenta, dim},
	gBlocked: {magenta, yellow},
	gOpen:    {green, dim},
	gClosed:  {red, dim},
	gNoPR:    {dim, dim},
	gKeep:    {dim, dim},
	gLocked:  {dim, dim},
	gOther:   {dim, dim},
}

// color says whether standard output is styled. Run sets it once.
var color bool

// colorEnabled follows gh: color goes only to a terminal, unless the
// environment says otherwise (NO_COLOR, CLICOLOR, CLICOLOR_FORCE, GH_FORCE_TTY).
var colorEnabled = func(stdout io.Writer) bool {
	return stdout == io.Writer(os.Stdout) && ghterm.FromEnv().IsColorEnabled()
}

// fit says whether names are cut to fit a column: only on a terminal. A pipe gets
// every name whole, so a script can pass it back to gh reap remove. Run sets it once.
var fit bool

var terminalOutput = func(stdout io.Writer) bool {
	return stdout == io.Writer(os.Stdout) && ghterm.FromEnv().IsTerminalOutput()
}

func style(code, s string) string {
	if !color || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// padded styles s and fills it to width with plain spaces, so alignment never
// counts the escape codes.
func padded(code, s string, width int) string {
	return style(code, s) + strings.Repeat(" ", width-len([]rune(s)))
}

// printHelp prints the usage text, with its section headings in bold as gh does.
func printHelp(w io.Writer) {
	for _, line := range strings.SplitAfter(usage, "\n") {
		if heading := strings.TrimSuffix(line, "\n"); heading != "" && heading[0] != ' ' && heading == strings.ToUpper(heading) {
			line = style(bold, heading) + "\n"
		}
		fmt.Fprint(w, line)
	}
}

// group places an item, or returns -1 for one never worth showing: the main
// checkout and the default branch.
func group(it reap.Item) int {
	switch {
	case it.Verdict == reap.VerdictRemove:
		return gRemove
	case it.Reason == reap.ReasonMainCheckout, it.Reason == reap.ReasonDefaultBranch:
		return -1
	case it.Reason == reap.ReasonKeepMark:
		return gKeep
	case it.Reason == reap.ReasonLocked:
		return gLocked
	case it.PR == nil && (it.Reason == reap.ReasonDetached || it.Reason == reap.ReasonPRUnknown):
		return gOther
	case it.PR == nil:
		return gNoPR
	}
	// By PR state, not by reason: a dirty tree whose PR is still open is not blocked.
	switch it.PR.State {
	case "MERGED":
		return gBlocked
	case "OPEN":
		return gOpen
	case "CLOSED":
		return gClosed
	}
	return gOther
}

type row struct{ name, status, info string }

func rowFor(g int, it reap.Item) row {
	r := row{name: name(it)}
	switch g {
	case gRemove:
		r.status, r.info = it.Detail, removeInfo(it, true)
	case gBlocked:
		r.status, r.info = prText(it.PR), it.Detail
	case gOpen, gClosed:
		r.status, r.info = prText(it.PR), it.Kind
		if it.Kind == reap.KindWorktree && it.Reason != reap.ReasonPROpen && it.Reason != reap.ReasonPRClosed {
			r.info += ", " + it.Detail
		}
	case gNoPR, gKeep:
		r.status = it.Kind
		if g == gNoPR && it.Kind == reap.KindWorktree && it.Reason != reap.ReasonNoPR {
			r.info = it.Detail
		}
	default:
		r.status, r.info = it.Kind, it.Detail
	}
	return r
}

func printInventory(w io.Writer, inv *reap.Inventory, header, all bool) {
	if header {
		title := tilde(inv.Root)
		if inv.GitHub != "" {
			title += " (" + inv.GitHub + ")"
		}
		fmt.Fprintln(w, style(bold, title))
	}
	for _, warning := range inv.Warnings {
		fmt.Fprintln(w, style(yellow, "!")+" "+warning)
	}
	for _, p := range inv.Problems {
		fmt.Fprintln(w, style(yellow, "!")+" "+p)
	}
	var groups [nGroups][]row
	for _, it := range inv.Items {
		if g := group(it); g >= 0 {
			groups[g] = append(groups[g], rowFor(g, it))
		}
	}
	shown := []int{gRemove, gBlocked}
	if all {
		shown = []int{gRemove, gBlocked, gOpen, gClosed, gNoPR, gKeep, gLocked, gOther}
	}
	var sections []section
	for _, g := range shown {
		if len(groups[g]) > 0 {
			sections = append(sections, section{groupNames[g].title, groupStyles[g], groups[g]})
		}
	}
	printSections(w, sections)

	var footer []string
	switch {
	case !inv.Complete:
		footer = append(footer, style(red, "Checks incomplete: nothing can be removed."))
	case len(groups[gRemove]) == 0 && len(groups[gBlocked]) == 0:
		footer = append(footer, "Nothing to remove.")
	}
	if !all {
		var hidden []string
		for g := gOpen; g < nGroups; g++ {
			if n := len(groups[g]); n > 0 {
				noun := groupNames[g].many
				if n == 1 {
					noun = groupNames[g].one
				}
				hidden = append(hidden, fmt.Sprintf("%d %s", n, noun))
			}
		}
		if len(hidden) > 0 {
			footer = append(footer, style(dim, "Not shown: "+strings.Join(hidden, ", ")+" (gh reap --all)"))
		}
	}
	if inv.Complete && len(groups[gRemove]) > 0 {
		command := "gh reap remove"
		if header {
			command = "cd " + shellQuote(inv.Root) + " && " + command
		}
		footer = append(footer, style(dim, "Remove with:")+" "+style(bold, command))
	}
	if len(footer) > 0 {
		if len(sections) > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, strings.Join(footer, "\n"))
	}
}

type section struct {
	title string
	style sectionStyle
	rows  []row
}

// printSections prints each section under its title, with columns aligned
// across all of them.
func printSections(w io.Writer, sections []section) {
	var nameW, statusW int
	for _, s := range sections {
		for _, r := range s.rows {
			nameW = max(nameW, len([]rune(r.name)))
			statusW = max(statusW, len(r.status))
		}
	}
	for i, s := range sections {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, style(bold, s.title+":"))
		for _, r := range s.rows {
			line := "  " + padded(cyan, r.name, nameW) + "  " + padded(s.style.status, r.status, statusW) + "  " + style(s.style.info, r.info)
			fmt.Fprintln(w, strings.TrimRight(line, " "))
		}
	}
}

// confirm shows what remove is about to remove and asks; only "y" or "yes" says
// yes. An interrupt while it waits says no.
func confirm(ctx context.Context, w io.Writer, answers *bufio.Reader, items []reap.Item) bool {
	rows := make([]row, 0, len(items))
	for _, it := range items {
		rows = append(rows, row{name: name(it), status: removeInfo(it, false)})
	}
	printSections(w, []section{{"To remove", sectionStyle{dim, dim}, rows}})
	fmt.Fprintf(w, "\n%s [y/N] ", style(bold, "Remove "+scope(items)+"?"))
	typed := make(chan string, 1)
	go func() {
		answer, _ := answers.ReadString('\n')
		typed <- answer
	}()
	var answer string
	select {
	case answer = <-typed:
	case <-ctx.Done():
		fmt.Fprintln(w) // end the prompt's line, after the terminal's ^C
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	fmt.Fprintln(w, "Canceled.")
	return false
}

// scope counts what removing items deletes: a worktree takes its branch with it.
func scope(items []reap.Item) string {
	var trees, branches int
	for _, it := range items {
		if it.Kind == reap.KindWorktree {
			trees++
		}
		if it.Branch != "" && it.Reason != reap.ReasonFolderGone {
			branches++
		}
	}
	var parts []string
	if trees > 0 {
		parts = append(parts, count(trees, "worktree", "worktrees"))
	}
	if branches > 0 {
		parts = append(parts, count(branches, "branch", "branches"))
	}
	return strings.Join(parts, " and ")
}

// removeInfo says what removing it deletes.
func removeInfo(it reap.Item, withSize bool) string {
	switch {
	case it.Reason == reap.ReasonFolderGone:
		return "worktree registration"
	case it.Kind == reap.KindBranch:
		return "branch"
	}
	parts := []string{"worktree and branch"}
	if withSize && it.SizeKB != nil {
		parts = append(parts, size(*it.SizeKB))
	}
	if it.Hook != nil && *it.Hook {
		parts = append(parts, "teardown hook")
	}
	return strings.Join(parts, ", ")
}

// printResult says what happened to one item, in the words gh pr merge uses
// for the same work; the reason follows when it was not removed.
func printResult(w io.Writer, r reap.Result) {
	if r.Result != reap.Removed {
		n := r.Branch
		if n == "" {
			n = tilde(r.Path)
		}
		if n == "" {
			n = r.Ref
		}
		fmt.Fprintf(w, "%s %s: %s\n", style(red, "X"), style(cyan, n), r.Detail)
		return
	}
	done := style(green, "✓")
	if r.Reason == reap.ReasonFolderGone {
		fmt.Fprintf(w, "%s Pruned stale worktree metadata for %s\n", done, tilde(r.Path))
		return
	}
	if r.Kind == reap.KindWorktree {
		fmt.Fprintf(w, "%s Removed worktree %s\n", done, tilde(r.Path))
	}
	if r.Branch != "" {
		fmt.Fprintf(w, "%s Deleted local branch %s\n", done, style(cyan, r.Branch))
	}
}

// printSummary prints the problems, then the counts that are not zero.
func printSummary(stdout, stderr io.Writer, rep *reap.Report) {
	for _, p := range rep.Problems {
		fmt.Fprintln(stderr, "! "+p)
	}
	switch {
	case rep.Canceled:
		return
	case len(rep.Results) == 0:
		fmt.Fprintln(stdout, "Nothing to remove.")
		return
	}
	counts := map[string]int{}
	for _, r := range rep.Results {
		counts[r.Result]++
	}
	var parts []string
	for _, result := range []string{reap.Removed, reap.Failed, reap.Refused} {
		if n := counts[result]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, result))
		}
	}
	fmt.Fprintf(stdout, "\n%s\n", style(bold, strings.Join(parts, ", ")+"."))
}

// name is how an item is shown: its branch, or its folder when it has none,
// cut to fit a column on a terminal.
func name(it reap.Item) string {
	n := it.Branch
	if n == "" {
		n = tilde(it.Path)
	}
	const width = 40
	if r := []rune(n); fit && len(r) > width {
		return string(r[:width-1]) + "…"
	}
	return n
}

func prText(pr *conn.PR) string {
	return fmt.Sprintf("PR #%d %s", pr.Number, strings.ToLower(pr.State))
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func size(kb int64) string {
	v := float64(kb)
	for _, unit := range []string{"K", "M", "G"} {
		if v < 1024 || unit == "G" {
			if v < 10 && unit != "K" {
				return fmt.Sprintf("%.1f%s", v, unit)
			}
			return fmt.Sprintf("%.0f%s", v, unit)
		}
		v /= 1024
	}
	return ""
}

func tilde(p string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + p[len(home):]
	}
	return p
}

// shellQuote keeps paths literal in a POSIX shell, including embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
