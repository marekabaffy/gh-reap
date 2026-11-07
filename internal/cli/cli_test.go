package cli

import (
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
		{"remove"},
		{"list", "--state", "open"},
		{"list", "--nope"},
		{"keep"},
		{"unkeep"},
		{"list", "extra"},
		{"remove", "--repos", "x"},
		{"list", "--all"},
	}
	for _, args := range tests {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code != ExitUsage {
			t.Errorf("%v: exit %d, want %d (stderr %q)", args, code, ExitUsage, errb.String())
		}
		if !strings.Contains(errb.String(), "gh reap --help") {
			t.Errorf("%v: no pointer to help: %q", args, errb.String())
		}
	}
}

func TestRun_Help(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"remove", "--help"}, {"list", "-h"}} {
		var out bytes.Buffer
		if code := Run(args, &out, io.Discard); code != ExitOK || !strings.Contains(out.String(), "EXIT STATUS") {
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

func TestSizeHookTilde(t *testing.T) {
	kb := func(n int64) *int64 { return &n }
	for in, want := range map[int64]string{0: "0K", 512: "512K", 2048: "2.0M", 50 * 1024: "50M", 3 * 1024 * 1024: "3.0G", 4096 * 1024 * 1024: "4096G"} {
		if got := size(kb(in)); got != want {
			t.Errorf("size(%d) = %q, want %q", in, got, want)
		}
	}
	yes, no := true, false
	if size(nil) != "-" || hook(nil) != "-" || hook(&yes) != "yes" || hook(&no) != "no" {
		t.Error("placeholders")
	}
	home, _ := os.UserHomeDir()
	if tilde(home+"/w/x") != "~/w/x" || tilde("/elsewhere") != "/elsewhere" || tilde(home+"x") != home+"x" {
		t.Error("tilde")
	}
}

func TestPrintInventory(t *testing.T) {
	kb := int64(2048)
	yes := true
	inv := &reap.Inventory{Root: "/r", GitHub: "acme/w", Problems: []string{"lsof missing"}, Items: []reap.Item{
		{Kind: reap.KindWorktree, Verdict: reap.VerdictRemove, Branch: "a", Path: "/w/a", SizeKB: &kb, Hook: &yes, Detail: "PR #1 merged"},
		{Kind: reap.KindWorktree, Verdict: reap.VerdictKeep, Path: "/w/d", Detail: "detached HEAD"},
		{Kind: reap.KindBranch, Verdict: reap.VerdictKeep, Branch: "b", Detail: "no pull request"},
	}}
	var out bytes.Buffer
	printInventory(&out, inv, true)
	for _, want := range []string{"== /r  (acme/w)", "! lsof missing", "REMOVE   worktree  2.0M  yes   a  /w/a", "(detached) /w/d", "1 to remove, 2 to keep", "gh reap remove"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}
}

func TestPrintResultAndSummary(t *testing.T) {
	var out, errb bytes.Buffer
	printResult(&out, reap.Result{Ref: "branch:b", Branch: "b", Result: reap.Removed, Detail: "branch deleted"})
	printResult(&out, reap.Result{Ref: "nosuch", Result: reap.Refused, Detail: "no worktree"})
	printSummary(&out, &errb, &reap.Report{Problems: []string{"p"}, Results: []reap.Result{{Result: reap.Removed}, {Result: reap.Refused}}})
	printSummary(&out, &errb, &reap.Report{DryRun: true, Results: []reap.Result{{Result: reap.WouldRemove}}})
	printSummary(&out, &errb, &reap.Report{})
	for _, want := range []string{"removed      b: branch deleted", "refused      nosuch: no worktree", "1 removed, 0 failed, 1 refused", "1 would be removed", "Nothing to remove"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}
	if errb.String() != "! p\n" {
		t.Errorf("problems go to stderr: %q", errb.String())
	}
}
