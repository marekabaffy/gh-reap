# Security

gh reap deletes local worktrees and branches, so a way to make it remove work it
should keep is a security issue, as is anything that runs code or leaks a token.

Report it privately through
[GitHub's vulnerability reporting](https://github.com/marekabaffy/gh-reap/security/advisories/new),
not in a public issue. Fixes ship in a new release; only the latest release is
supported.

## Verifying a release

Each release binary carries a build attestation. To check that a binary was built
by this repository's release workflow from a given tag, download it and verify it:

```
gh release download vX.Y.Z --repo marekabaffy/gh-reap --pattern darwin-arm64
gh attestation verify darwin-arm64 --repo marekabaffy/gh-reap \
  --signer-workflow marekabaffy/gh-reap/.github/workflows/release.yml \
  --source-ref refs/tags/vX.Y.Z
```
