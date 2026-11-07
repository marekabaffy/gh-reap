# gh reap

[![CI](https://github.com/marekabaffy/gh-reap/actions/workflows/ci.yml/badge.svg)](https://github.com/marekabaffy/gh-reap/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/marekabaffy/gh-reap)](https://github.com/marekabaffy/gh-reap/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A [GitHub CLI](https://cli.github.com) extension that cleans up the git worktrees and
local branches of merged pull requests, and keeps everything else.

Plain git cannot tell which branches are done, because a squash or rebase merge leaves
them looking unmerged. gh reap asks GitHub instead, checks that nothing local would be
lost, and never forces a removal.

```
$ gh reap
VERDICT  KIND      SIZE  HOOK  WHAT                REASON
KEEP     worktree  -     -     main  ~/app         the main checkout
REMOVE   worktree  12M   yes   login  ~/app-login  PR #41 merged
KEEP     worktree  -     yes   api  ~/app-api      PR #44 is open
KEEP     worktree  -     yes   spike  ~/app-spike  2 uncommitted or untracked files
REMOVE   branch    -     -     typo                PR #39 merged

$ gh reap remove --all
removed      login ~/app-login: worktree and branch removed
removed      typo: branch deleted

2 removed, 0 failed, 0 refused.
```

## Install

```
gh extension install marekabaffy/gh-reap
```

You also need:

- `gh`, signed in (`gh auth login`)
- `lsof`
- git with config-based hooks (tested with 2.56). An older git still removes branches,
  but keeps worktrees.

## Usage

| Command | What it does |
|---|---|
| `gh reap` | List worktrees and branches, each marked REMOVE or KEEP with a reason |
| `gh reap remove --all` | Remove everything marked REMOVE |
| `gh reap remove <name>` | Remove only the branches or worktree paths you name |
| `gh reap keep <branch>` | Never remove this branch (`unkeep` undoes it) |

| Option | What it does |
|---|---|
| `--dry-run` | Show what `remove` would do, change nothing |
| `--state closed` | Also count pull requests closed without merging |
| `--repos <dir>` | List every repository in a folder |
| `--json` | Print JSON, described in [docs/reference.md](docs/reference.md) |
| `--debug` | Print each command gh reap runs |

## How it decides

An item is **REMOVE** only when all of these hold:

- ✅ Its pull request, from this repository or one of your remotes, is merged.
- ✅ Every local commit is in that pull request or on a remote.
- ✅ The worktree is clean, unlocked, holds no other worktree, and no process is
  running inside it.
- ✅ It is not the main checkout, the default branch, or marked keep.

If anything cannot be checked (GitHub is down, `lsof` fails), the item is KEEP.

When you run `remove`, gh reap:

1. checks each item again,
2. runs your [teardown hook](#teardown-hook), then checks again,
3. runs `git worktree remove`,
4. deletes the branch with `git branch -d`. If git refuses only because a squash or
   rebase merge hides it, gh reap deletes it at the exact commit it checked.

If a step fails, it stops and keeps the item. It never deletes remote branches.

> [!NOTE]
> Files git ignores, such as `node_modules` or `.env`, are removed with the worktree, as
> with plain `git worktree remove`. Use the teardown hook to save anything you need.

## Teardown hook

gh reap runs the git hook `pre-worktree-remove` inside a worktree before removing it, with
the worktree's path as `$1`. If the hook fails, the worktree is kept.

```
git config hook.teardown.command /path/to/teardown.sh
git config hook.teardown.event pre-worktree-remove
```

Add `--global` to use it in every repository. For example, to stop a worktree's
containers:

```sh
#!/bin/sh
set -e
if [ -f compose.yaml ]; then
  docker compose -p "$(basename "$1")" down --volumes
fi
```

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
