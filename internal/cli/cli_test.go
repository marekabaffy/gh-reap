package cli

import (
	"bufio"
	"bytes"
	"flag"
	"io"
	"os"
	"reflect"
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
	}
}

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dry := fs.Bool("dry-run", false, "")
	state := fs.String("state", "", "")
	pos, err := parseInterspersed(fs, []string{"a", "--dry-run", "b", "--state", "closed", "--", "--c"})
	if err != nil || !*dry || *state != "closed" || !reflect.DeepEqual(pos, []string{"a", "b", "--c"}) {
		t.Errorf("pos %v dry %v state %q err %v", pos, *dry, *state, err)
	}
	// After "--", what looks like a flag is a name.
	*dry = false
	pos, err = parseInterspersed(fs, []string{"--", "feature", "--dry-run"})
	if err != nil || *dry || !reflect.DeepEqual(pos, []string{"feature", "--dry-run"}) {
		t.Errorf("after --: pos %v dry %v err %v", pos, *dry, err)
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
  wip       PR #4 open    worktree

Without a PR:
  idea      branch
  notes     branch

Marked keep:
  precious  branch

Other:
  /w/d      worktree      detached HEAD

Remove with: gh reap remove
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
		if got := confirm(&out, bufio.NewReader(strings.NewReader(answer)), items); got != want {
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
	printResult(&out, reap.Result{Ref: "branch:b", Branch: "b", Result: reap.Removed, Detail: "branch deleted"})
	printResult(&out, reap.Result{Ref: "worktree:/w/d", Path: "/w/d", Result: reap.Removed})
	printResult(&out, reap.Result{Ref: "worktree:/w/h", Branch: "h", Path: "/w/h", Result: reap.Failed, Detail: "teardown hook failed: boom"})
	printResult(&out, reap.Result{Ref: "nosuch", Result: reap.Refused, Detail: "not found"})
	printSummary(&out, &errb, &reap.Report{Problems: []string{"p"}, Results: []reap.Result{{Result: reap.Removed}, {Result: reap.Failed}, {Result: reap.Refused}}})
	printSummary(&out, &errb, &reap.Report{})
	printSummary(&out, &errb, &reap.Report{Canceled: true})
	want := `removed  b
removed  /w/d
failed   h: teardown hook failed: boom
refused  nosuch: not found

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
