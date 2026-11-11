<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/wordmark-dark.svg">
    <img src="docs/assets/wordmark-light.svg" alt="gh reap" width="220">
  </picture>
</h1>

Reap what you merged.

[![CI](https://github.com/marekabaffy/gh-reap/actions/workflows/ci.yml/badge.svg)](https://github.com/marekabaffy/gh-reap/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/marekabaffy/gh-reap)](https://github.com/marekabaffy/gh-reap/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A [GitHub CLI](https://cli.github.com) extension that cleans up the git worktrees and
local branches of merged pull requests, and keeps everything else.

Plain git cannot tell which branches are done, because a squash or rebase merge leaves
them looking unmerged. gh reap asks GitHub instead, checks that nothing local would be
lost, and never forces a removal.

<img src="docs/assets/demo.gif" width="720" alt="A terminal recording. gh reap lists two merged items to remove and one blocked by uncommitted files. gh reap remove then asks, removes the worktree and deletes both branches.">

## Install

```
gh extension install marekabaffy/gh-reap
```

Update it later with `gh extension upgrade reap`.

gh reap runs on macOS and Linux. Windows is not supported, because gh reap uses `lsof` to
see what is running inside a worktree. You also need:

- `gh`, signed in (`gh auth login`)
- `lsof`
- git 2.54 or newer, for teardown hooks. An older git still removes branches, but keeps
  worktrees.

## Usage

| Command | What it does |
|---|---|
| `gh reap` | Show what can be removed, and what is blocked and why |
| `gh reap --all` | Also show open PRs, branches without a PR, and kept branches |
| `gh reap remove` | Remove everything under "To remove", after you confirm |
| `gh reap remove <name>...` | Remove only these branches or worktree paths |
| `gh reap keep <branch>` | Never remove this branch (`unkeep` undoes it) |

| Option | What it does |
|---|---|
| `--yes` | `remove` without asking; needed without a terminal |
| `--json` | Print JSON, described in [docs/reference.md](docs/reference.md) |
| `--debug` | Print each command gh reap runs |
| `--version` | Show the gh reap version |

A blocked item goes once you fix what it shows: commit or push the work, or close what
runs inside it. To throw the work away instead, use git (`git worktree remove --force`,
`git branch -D`).

### Many repositories

Run `gh reap` in a folder of repositories to list each one. `remove` works in one
repository at a time, so run it in each:

```sh
for d in ~/src/*/; do (cd "$d" && gh reap remove); done
```

or, with [myrepos](https://myrepos.branchable.com/), `mr run gh reap remove`.

## How it decides

An item is removed only when all of these hold:

- Its pull request, from this repository or one of your remotes, is merged.
- Every local commit is in that pull request or on a remote.
- The worktree is clean, unlocked, holds no other worktree, and no process is
  running inside it.
- It is not the main checkout, the default branch, or marked keep.

If anything cannot be checked (GitHub is down, `lsof` fails), nothing is removed.

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
