// gh reap removes the git worktrees and local branches of finished GitHub pull
// requests, safely. See README.md.
package main

import (
	"os"

	"github.com/marekabaffy/gh-reap/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
