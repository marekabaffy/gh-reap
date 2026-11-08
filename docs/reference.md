# Reference

## `gh reap --json`

```json
{
  "repos": [
    {
      "path": "/home/me/src/widgets",
      "github": "acme/widgets",
      "complete": true,
      "items": [
        {
          "kind": "worktree",
          "ref": "worktree:/home/me/src/widgets-fix-login",
          "verdict": "REMOVE",
          "reason": "pr-merged",
          "detail": "PR #41 merged",
          "branch": "fix-login",
          "oid": "4038e29f7b6045521864de786bc46075e9ad791c",
          "path": "/home/me/src/widgets-fix-login",
          "pr": { "number": 41, "state": "MERGED", "head_oid": "4038e29f7b6045521864de786bc46075e9ad791c", "head_repo": "acme/widgets", "url": "https://github.com/acme/widgets/pull/41" },
          "hook": true,
          "size_kb": 90112
        }
      ]
    }
  ]
}
```

Repository fields: `path` (the main checkout, or the bare repository), `github`
(OWNER/REPO, absent when no GitHub remote was found), `complete` (false when `lsof` failed
or pull requests could not be read; then nothing is REMOVE), `problems` (absent when
there are none), `warnings` (optional diagnostics that do not make the inventory
incomplete, such as unsupported teardown hooks), `items`.

| Item field | Present | Meaning |
|---|---|---|
| `kind` | always | `worktree`, or `branch` for a local branch no worktree holds |
| `ref` | always | `worktree:<path>` or `branch:<name>`; pass it to `gh reap remove` |
| `verdict` | always | `REMOVE` or `KEEP` |
| `reason` | always | a reason code, below |
| `detail` | always | a few words for people; the wording may change |
| `branch` | unless detached | the branch name |
| `oid` | except the bare repository | the branch tip (a detached worktree: its HEAD) |
| `path` | worktrees | the worktree folder |
| `pr` | when a PR was found | `number`, `state` (`OPEN`, `MERGED`, `CLOSED`), `head_oid`, `head_repo` (OWNER/REPO), `url` |
| `hook` | worktrees, except the main checkout, gone folders, and when git cannot list hooks | whether a `pre-worktree-remove` hook is registered |
| `size_kb` | REMOVE worktrees whose folder exists, when `du` succeeds | `du -sk` of the folder |

## `gh reap remove --json`

Needs `--yes`. Prints `repo`, `problems` (when the list was incomplete) and `results`, one
per item: `ref`, `kind`, `branch`, `path`, `result` (`removed`, `failed`, `refused`),
`reason`, `detail`.

## Reason codes

| Code | Verdict | Meaning |
|---|---|---|
| `pr-merged` | REMOVE | its PR merged and every rule holds |
| `pr-closed` | KEEP | its PR was closed without merging |
| `folder-gone` | REMOVE | a registered worktree whose folder no longer exists |
| `main-checkout` | KEEP | the main checkout |
| `locked` | KEEP | locked with `git worktree lock` |
| `contains-worktree` | KEEP | another worktree sits inside its folder; removing it would delete that one too |
| `detached` | KEEP | a worktree on a detached HEAD |
| `marked-keep` | KEEP | marked with `gh reap keep` |
| `default-branch` | KEEP | the repository's default branch |
| `current-branch` | KEEP | checked out in the main checkout |
| `in-use` | KEEP | a process works inside the tree |
| `processes-unknown` | KEEP | `lsof` failed, is missing, or printed output gh reap cannot read |
| `dirty` | KEEP | uncommitted or untracked files |
| `status-failed` | KEEP | `git status` failed |
| `no-pr` | KEEP | no PR from this repository or one of your remotes has this branch as its head |
| `pr-open` | KEEP | its PR is open |
| `pr-unknown` | KEEP | pull requests could not be read, or GitHub's answer lacked the default branch |
| `pr-head-missing` | KEEP | the PR head commit is not in the local clone (fetch, then retry) |
| `unpushed-commits` | KEEP | commits on the branch in neither the PR nor any remote |
| `not-on-pr-head` | KEEP | the branch tip is not the PR head or behind it |
| `check-failed` | KEEP | a git command needed for the checks failed |
| `git-too-old-for-hooks` | KEEP | this git cannot run the teardown hook |
| `hook-check-failed` | KEEP | listing the hook failed |
| `inventory-incomplete` | KEEP | would be REMOVE, but the checks were incomplete |

`remove` results add:

| Code | Meaning |
|---|---|
| `not-found` | no worktree or local branch by that name or path |
| `changed-since-list` | the item changed between the list and the check before acting |
| `hook-failed` | the teardown hook failed; the tree is kept |
| any KEEP reason, after the hook | the hook changed the tree (left a process running, files, a moved or detached HEAD); the tree is kept |
| `worktree-remove-failed` | `git worktree remove` refused |
| `branch-delete-failed` | the branch was kept; `detail` says whether its worktree was removed |
| `canceled` | interrupted (Ctrl-C) |

## Exit status

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | an item failed or was refused, the checks were incomplete, or another `remove` holds the lock |
| 2 | usage error |

Listing a folder of repositories, the exit status covers every repository.

## Which GitHub repository

gh reap picks the remote the way `gh` does: the one set with `gh repo set-default`, then
`upstream`, `github`, `origin`, then the rest by name. Set `GH_REPO=OWNER/REPO` to choose.
