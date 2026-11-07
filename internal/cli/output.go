package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/marekabaffy/gh-reap/internal/reap"
)

func printInventory(w io.Writer, inv *reap.Inventory, header bool) {
	title := tilde(inv.Root)
	if inv.GitHub != "" {
		title += "  (" + inv.GitHub + ")"
	}
	if header {
		fmt.Fprintln(w, "==", title)
	}
	for _, p := range inv.Problems {
		fmt.Fprintln(w, "! "+p)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VERDICT\tKIND\tSIZE\tHOOK\tWHAT\tREASON")
	removable := 0
	for _, it := range inv.Items {
		if it.Verdict == reap.VerdictRemove {
			removable++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", it.Verdict, it.Kind, size(it.SizeKB), hook(it.Hook), what(it), it.Detail)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\n%d to remove, %d to keep.", removable, len(inv.Items)-removable)
	if removable > 0 {
		fmt.Fprint(w, " Remove with: gh reap remove <branch|path>... or gh reap remove --all")
	}
	fmt.Fprintln(w)
}

func what(it reap.Item) string {
	switch {
	case it.Kind == reap.KindBranch:
		return it.Branch
	case it.Branch == "":
		return "(detached) " + tilde(it.Path)
	}
	return it.Branch + "  " + tilde(it.Path)
}

func printResult(w io.Writer, r reap.Result) {
	name := r.Branch
	if r.Path != "" {
		name = strings.TrimSpace(r.Branch + " " + tilde(r.Path))
	}
	if name == "" {
		name = r.Ref
	}
	fmt.Fprintf(w, "%-12s %s: %s\n", r.Result, name, r.Detail)
}

func printSummary(stdout, stderr io.Writer, rep *reap.Report) {
	for _, p := range rep.Problems {
		fmt.Fprintln(stderr, "! "+p)
	}
	counts := map[string]int{}
	for _, r := range rep.Results {
		counts[r.Result]++
	}
	if rep.DryRun {
		fmt.Fprintf(stdout, "\ndry run: %d would be removed, %d refused. Nothing was changed.\n", counts[reap.WouldRemove], counts[reap.Refused])
		return
	}
	if len(rep.Results) == 0 {
		fmt.Fprintln(stdout, "Nothing to remove.")
		return
	}
	fmt.Fprintf(stdout, "\n%d removed, %d failed, %d refused.\n", counts[reap.Removed], counts[reap.Failed], counts[reap.Refused])
}

func size(kb *int64) string {
	if kb == nil {
		return "-"
	}
	v := float64(*kb)
	for _, unit := range []string{"K", "M", "G"} {
		if v < 1024 || unit == "G" {
			if v < 10 && unit != "K" {
				return fmt.Sprintf("%.1f%s", v, unit)
			}
			return fmt.Sprintf("%.0f%s", v, unit)
		}
		v /= 1024
	}
	return "-"
}

func hook(h *bool) string {
	switch {
	case h == nil:
		return "-"
	case *h:
		return "yes"
	}
	return "no"
}

func tilde(p string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + p[len(home):]
	}
	return p
}
