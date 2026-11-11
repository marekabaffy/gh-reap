package cli

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/marekabaffy/gh-reap/internal/conn"
	"github.com/marekabaffy/gh-reap/internal/reap"
)

func TestRun_UsageErrors(t *testing.T) {
	tests := [][]string{
		{"bogus"},
		{"list"},
		{"--nope"},
		{"--state", "closed"},
		{"--yes"},
		{"extra"},
		{"keep"},
		{"unkeep"},
		{"remove", "--all"},
		{"remove", "--dry-run"},
		{"remove", "--json"}, // --json needs --yes
		{"remove"},           // no terminal here to confirm
	}
	for _, args := range tests {
		var out, errb bytes.Buffer
		if code := Run(args, strings.NewReader(""), &out, &errb); code != ExitUsage {
			t.Errorf("%v: exit %d, want %d (stderr %q)", args, code, ExitUsage, errb.String())
		}
		if !strings.Contains(errb.String(), "gh reap --help") {
			t.Errorf("%v: no pointer to help: %q", args, errb.String())
		}
	}
}

func TestRun_Help(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"remove", "--help"}} {
		var out bytes.Buffer
		if code := Run(args, strings.NewReader(""), &out, io.Discard); code != ExitOK || !strings.Contains(out.String(), "EXIT STATUS") {
			t.Errorf("%v: exit %d", args, code)
		}
		for _, want := range []string{"EXAMPLES", "$ gh reap remove fix-login", "https://github.com/marekabaffy/gh-reap/blob/main/docs/reference.md"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%v: help lacks %q", args, want)
			}
		}
	}
}

// Styled output is the plain output plus escape codes, and nothing else:
// the text and the alignment do not change.
func TestRun_Version(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"remove", "--version"}} {
		var out bytes.Buffer
		if code := Run(args, strings.NewReader(""), &out, io.Discard); code != ExitOK || !regexp.MustCompile(`^gh reap version \S+\n$`).MatchString(out.String()) {
			t.Errorf("%v: exit %d, %q", args, code, out.String())
		}
	}
}

func TestColor_OnlyAddsEscapes(t *testing.T) {
	yes, no := bufio.NewReader(strings.NewReader("y\n")), bufio.NewReader(strings.NewReader("n\n"))
	render := func(answers *bufio.Reader) string {
		var out bytes.Buffer
		printInventory(&out, inventory(), true, true)
		printInventory(&out, inventory(), false, false)
		printInventory(&out, &reap.Inventory{Root: "/r", Warnings: []string{"w"}, Problems: []string{"p"}}, false, false)
		confirm(context.Background(), &out, answers, []reap.Item{{Kind: reap.KindBranch, Branch: "b"}, {Kind: reap.KindWorktree, Branch: "longer"}})
		printResult(&out, reap.Result{Branch: "b", Result: reap.Removed})
		printResult(&out, reap.Result{Branch: "h", Result: reap.Failed, Detail: "boom"})
		printSummary(&out, io.Discard, &reap.Report{Results: []reap.Result{{Result: reap.Removed}, {Result: reap.Failed}}})
		printHelp(&out)
		return out.String()
	}
	plain := render(yes)
	if strings.Contains(plain, "\x1b") {
		t.Fatalf("plain output has escape codes: %q", plain)
	}
	color = true
	t.Cleanup(func() { color = false })
	styled := render(no)
	if styled == plain {
		t.Fatal("styled output has no escape codes")
	}
	if stripped := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(styled, ""); strings.ReplaceAll(stripped, "Canceled.\n", "") != plain {
		t.Errorf("stripped\n%q\nplain\n%q", stripped, plain)
	}
}

func TestColor_Styles(t *testing.T) {
	color = true
	t.Cleanup(func() { color = false })
	var out bytes.Buffer
	printInventory(&out, inventory(), false, false)
	for _, want := range []string{
		"\x1b[1mTo remove:\x1b[0m\n",
		"  \x1b[36mlogin\x1b[0m  \x1b[35mPR #1 merged\x1b[0m  \x1b[2mworktree and branch, 2.0M, teardown hook\x1b[0m\n",
		"\x1b[1mBlocked:\x1b[0m\n",
		"  \x1b[36mspike\x1b[0m  \x1b[35mPR #3 merged\x1b[0m  \x1b[33m3 uncommitted files\x1b[0m\n",
		"\x1b[2mRemove with:\x1b[0m \x1b[1mgh reap remove\x1b[0m\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%q", want, out.String())
		}
	}

	out.Reset()
	printInventory(&out, &reap.Inventory{Warnings: []string{"careful"}, Complete: true}, false, false)
	if want := "\x1b[33m!\x1b[0m careful\n"; !strings.Contains(out.String(), want) {
		t.Errorf("warning lacks %q: %q", want, out.String())
	}

	out.Reset()
	printResult(&out, reap.Result{Kind: reap.KindWorktree, Branch: "b", Path: "/w/b", Result: reap.Removed})
	printResult(&out, reap.Result{Branch: "c", Result: reap.Failed, Detail: "boom"})
	want := "\x1b[32m✓\x1b[0m Removed worktree /w/b\n\x1b[32m✓\x1b[0m Deleted local branch \x1b[36mb\x1b[0m\n\x1b[31mX\x1b[0m \x1b[36mc\x1b[0m: boom\n"
	if out.String() != want {
		t.Errorf("results\n%q\nwant\n%q", out.String(), want)
	}

	out.Reset()
	printHelp(&out)
	for _, want := range []string{"\x1b[1mUSAGE\x1b[0m\n", "\x1b[1mEXIT STATUS\x1b[0m\n", "\x1b[1mLEARN MORE\x1b[0m\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

func TestHelp_ExplainsBeforeUsage(t *testing.T) {
	if strings.Index(usage, "A branch goes only") > strings.Index(usage, "USAGE") {
		t.Error("the explanation comes before USAGE")
	}
}

// Flag errors read as they do in gh, which also parses with pflag.
func TestRun_FlagErrors(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"--force"}, "gh reap: unknown flag: --force\n"},
		{[]string{"-all"}, "gh reap: unknown shorthand flag: 'a' in -all\n"},
		{[]string{"remove", "--all"}, "gh reap: unknown flag: --all\n"},
	} {
		var errb bytes.Buffer
		if code := Run(tt.args, strings.NewReader(""), io.Discard, &errb); code != ExitUsage || !strings.Contains(errb.String(), tt.want) {
			t.Errorf("%v: exit %d, stderr %q, want %q", tt.args, code, errb.String(), tt.want)
		}
	}
}

func TestColorEnabled(t *testing.T) {
	clear := func(t *testing.T) {
		for _, k := range []string{"NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "GH_FORCE_TTY"} {
			t.Setenv(k, "")
		}
	}
	for _, tt := range []struct {
		name string
		env  []string
		out  io.Writer
		want bool
	}{
		{"forced", []string{"CLICOLOR_FORCE", "1"}, os.Stdout, true},
		{"forced tty", []string{"GH_FORCE_TTY", "1"}, os.Stdout, true},
		{"NO_COLOR beats a forced tty", []string{"GH_FORCE_TTY", "1", "NO_COLOR", "1"}, os.Stdout, false},
		{"CLICOLOR=0 beats a forced tty", []string{"GH_FORCE_TTY", "1", "CLICOLOR", "0"}, os.Stdout, false},
		{"not standard output", []string{"CLICOLOR_FORCE", "1"}, &bytes.Buffer{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clear(t)
			for i := 0; i < len(tt.env); i += 2 {
				t.Setenv(tt.env[i], tt.env[i+1])
			}
			if got := colorEnabled(tt.out); got != tt.want {
				t.Errorf("colorEnabled = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIgnoreSelf(t *testing.T) {
	ignore := ignoreSelf()
	self, parent := os.Getpid(), os.Getppid()
	tests := []struct {
		p    conn.Process
		want bool
	}{
		{conn.Process{PID: self}, true},
		{conn.Process{PID: 99999999, PPID: self, Command: "lsof"}, true},
		{conn.Process{PID: parent, Command: "gh"}, true},
		{conn.Process{PID: parent, Command: "zsh"}, false}, // started directly from a shell
		{conn.Process{PID: 99999999, PPID: 1, Command: "vim"}, false},
	}
	for _, tt := range tests {
		if got := ignore(tt.p); got != tt.want {
			t.Errorf("%+v: %v", tt.p, got)
		}
	}
}

func TestSizeTildeName(t *testing.T) {
	for in, want := range map[int64]string{0: "0K", 512: "512K", 2048: "2.0M", 50 * 1024: "50M", 3 * 1024 * 1024: "3.0G", 4096 * 1024 * 1024: "4096G"} {
		if got := size(in); got != want {
			t.Errorf("size(%d) = %q, want %q", in, got, want)
		}
	}
	home, _ := os.UserHomeDir()
	if tilde(home+"/w/x") != "~/w/x" || tilde("/elsewhere") != "/elsewhere" || tilde(home+"x") != home+"x" {
		t.Error("tilde")
	}
	long := strings.Repeat("x", 50)
	if got := name(reap.Item{Branch: long}); got != strings.Repeat("x", 39)+"…" {
		t.Errorf("name: %q", got)
	}
	if got := name(reap.Item{Path: "/w/d"}); got != "/w/d" {
		t.Errorf("name of a detached tree: %q", got)
	}
}

// inventory has one item of every group.
func inventory() *reap.Inventory {
	kb, yes := int64(2048), true
	merged := &conn.PR{Number: 1, State: "MERGED"}
	return &reap.Inventory{Root: "/r", GitHub: "acme/w", Complete: true, Items: []reap.Item{
		{Kind: reap.KindWorktree, Verdict: reap.VerdictKeep, Reason: reap.ReasonMainCheckout, Branch: "main", Path: "/r", Detail: "main checkout"},
		{Kind: reap.KindWorktree, Verdict: reap.VerdictRemove, Reason: reap.ReasonPRMerged, Branch: "login", Path: "/w/login", PR: merged, SizeKB: &kb, Hook: &yes, Detail: "PR #1 merged"},
		{Kind: reap.KindWorktree, Verdict: reap.VerdictKeep, Reason: reap.ReasonDirty, Branch: "spike", Path: "/w/spike", PR: &conn.PR{Number: 3, State: "MERGED"}, Detail: "3 uncommitted files"},
		{Kind: reap.KindWorktree, Verdict: reap.VerdictKeep, Reason: reap.ReasonDirty, Branch: "wip", Path: "/w/wip", PR: &conn.PR{Number: 4, State: "OPEN"}, Detail: "1 uncommitted file"},
		{Kind: reap.KindWorktree, Verdict: reap.VerdictKeep, Reason: reap.ReasonDetached, Path: "/w/d", Detail: "detached HEAD"},
		{Kind: reap.KindBranch, Verdict: reap.VerdictRemove, Reason: reap.ReasonPRMerged, Branch: "typo", PR: &conn.PR{Number: 2, State: "MERGED"}, Detail: "PR #2 merged"},
		{Kind: reap.KindBranch, Verdict: reap.VerdictKeep, Reason: reap.ReasonNoPR, Branch: "idea", Detail: "no PR"},
		{Kind: reap.KindBranch, Verdict: reap.VerdictKeep, Reason: reap.ReasonNoPR, Branch: "notes", Detail: "no PR"},
		{Kind: reap.KindBranch, Verdict: reap.VerdictKeep, Reason: reap.ReasonKeepMark, Branch: "precious", PR: merged, Detail: "marked keep"},
		{Kind: reap.KindBranch, Verdict: reap.VerdictKeep, Reason: reap.ReasonDefaultBranch, Branch: "main", Detail: "default branch"},
	}}
}

func TestPrintInventory(t *testing.T) {
	var out bytes.Buffer
	printInventory(&out, inventory(), false, false)
	want := `To remove:
  login  PR #1 merged  worktree and branch, 2.0M, teardown hook
  typo   PR #2 merged  branch

Blocked:
  spike  PR #3 merged  3 uncommitted files

Not shown: 1 open PR, 2 without a PR, 1 marked keep, 1 other (gh reap --all)
Remove with: gh reap remove
`
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
}

func TestPrintInventory_All(t *testing.T) {
	var out bytes.Buffer
	printInventory(&out, inventory(), true, true)
	want := `/r (acme/w)
To remove:
  login     PR #1 merged  worktree and branch, 2.0M, teardown hook
  typo      PR #2 merged  branch

Blocked:
  spike     PR #3 merged  3 uncommitted files

Open PR:
  wip       PR #4 open    worktree, 1 uncommitted file

Without a PR:
  idea      branch
  notes     branch

Marked keep:
  precious  branch

Other:
  /w/d      worktree      detached HEAD

Remove with: cd '/r' && gh reap remove
`
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
}

func TestPrintInventory_EmptyAndIncomplete(t *testing.T) {
	var out bytes.Buffer
	printInventory(&out, &reap.Inventory{Complete: true}, false, false)
	if out.String() != "Nothing to remove.\n" {
		t.Errorf("empty: %q", out.String())
	}
	out.Reset()
	inv := &reap.Inventory{Problems: []string{"pull requests unknown: HTTP 502"}, Items: []reap.Item{
		{Kind: reap.KindBranch, Verdict: reap.VerdictKeep, Reason: reap.ReasonIncomplete, Branch: "typo", PR: &conn.PR{Number: 2, State: "MERGED"}, Detail: "checks incomplete"},
	}}
	printInventory(&out, inv, false, false)
	want := `! pull requests unknown: HTTP 502
Blocked:
  typo  PR #2 merged  checks incomplete

Checks incomplete: nothing can be removed.
`
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
}

func TestConfirm(t *testing.T) {
	inv := inventory()
	items := []reap.Item{inv.Items[1], inv.Items[5]} // the login worktree, the typo branch
	for answer, want := range map[string]bool{"y\n": true, "YES\n": true, "n\n": false, "\n": false, "": false} {
		var out bytes.Buffer
		if got := confirm(context.Background(), &out, bufio.NewReader(strings.NewReader(answer)), items); got != want {
			t.Errorf("%q: %v", answer, got)
		}
		prompt := `To remove:
  login  worktree and branch, teardown hook
  typo   branch

Remove 1 worktree and 2 branches? [y/N] `
		if !want {
			prompt += "Canceled.\n"
		}
		if out.String() != prompt {
			t.Errorf("%q: got\n%s\nwant\n%s", answer, out.String(), prompt)
		}
	}
}

// An interrupt answers no, even though nobody typed anything.
func TestConfirm_Interrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	never, w := io.Pipe()
	defer w.Close()
	var out bytes.Buffer
	if confirm(ctx, &out, bufio.NewReader(never), inventory().Items[1:2]) {
		t.Error("an interrupted prompt said yes")
	}
	if !strings.HasSuffix(out.String(), "[y/N] \n") {
		t.Errorf("got %q", out.String())
	}
}

func TestScope(t *testing.T) {
	tree := reap.Item{Kind: reap.KindWorktree, Branch: "a"}
	gone := reap.Item{Kind: reap.KindWorktree, Branch: "g", Reason: reap.ReasonFolderGone}
	branch := reap.Item{Kind: reap.KindBranch, Branch: "b"}
	for want, items := range map[string][]reap.Item{
		"1 worktree and 1 branch":   {tree},
		"2 worktrees and 1 branch":  {tree, gone},
		"1 branch":                  {branch},
		"1 worktree and 2 branches": {tree, branch},
	} {
		if got := scope(items); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestPrintResultAndSummary(t *testing.T) {
	var out, errb bytes.Buffer
	printResult(&out, reap.Result{Ref: "branch:b", Kind: reap.KindBranch, Branch: "b", Result: reap.Removed, Detail: "branch deleted"})
	printResult(&out, reap.Result{Ref: "worktree:/w/d", Kind: reap.KindWorktree, Path: "/w/d", Result: reap.Removed})
	printResult(&out, reap.Result{Ref: "worktree:/w/g", Kind: reap.KindWorktree, Branch: "g", Path: "/w/g", Result: reap.Removed, Reason: reap.ReasonFolderGone})
	printResult(&out, reap.Result{Ref: "worktree:/w/h", Branch: "h", Path: "/w/h", Result: reap.Failed, Detail: "teardown hook failed: boom"})
	printResult(&out, reap.Result{Ref: "nosuch", Result: reap.Refused, Detail: "not found"})
	printSummary(&out, &errb, &reap.Report{Problems: []string{"p"}, Results: []reap.Result{{Result: reap.Removed}, {Result: reap.Failed}, {Result: reap.Refused}}})
	printSummary(&out, &errb, &reap.Report{})
	printSummary(&out, &errb, &reap.Report{Canceled: true})
	want := `✓ Deleted local branch b
✓ Removed worktree /w/d
✓ Pruned stale worktree metadata for /w/g
X h: teardown hook failed: boom
X nosuch: not found

1 removed, 1 failed, 1 refused.
Nothing to remove.
`
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
	if errb.String() != "! p\n" {
		t.Errorf("problems go to stderr: %q", errb.String())
	}
}

func TestPrintInventory_QuotedRoot(t *testing.T) {
	inv := inventory()
	inv.Root = "/tmp/repo's $(touch unwanted)"
	var out bytes.Buffer
	printInventory(&out, inv, true, false)
	if !strings.Contains(out.String(), `Remove with: cd '/tmp/repo'"'"'s $(touch unwanted)' && gh reap remove`) {
		t.Fatalf("unsafe removal hint: %s", out.String())
	}
}

func TestRowFor_RetainedWorktrees(t *testing.T) {
	for _, state := range []string{"", "OPEN", "CLOSED"} {
		it := reap.Item{Kind: reap.KindWorktree, Verdict: reap.VerdictKeep, Reason: reap.ReasonInUse, Detail: "in use by zsh (pid 42)"}
		if state != "" {
			it.PR = &conn.PR{State: state}
		}
		r := rowFor(group(it), it)
		if !strings.Contains(r.info, it.Detail) {
			t.Errorf("state %q hides reason: %+v", state, r)
		}
	}
}

func TestPrintInventory_Warnings(t *testing.T) {
	inv := inventory()
	inv.Warnings = []string{"git cannot list teardown hooks"}
	var out bytes.Buffer
	printInventory(&out, inv, false, false)
	if !strings.Contains(out.String(), "! git cannot list teardown hooks") || !strings.Contains(out.String(), "Remove with:") {
		t.Fatalf("warning should be visible without hiding branch cleanup: %s", out.String())
	}
}
