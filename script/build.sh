#!/usr/bin/env bash
# Builds the release binaries. These are the platforms gh-extension-precompile
# builds by default, except Windows, where gh reap's lsof check cannot run.
# The release tag, which gh-extension-precompile passes as $1, becomes --version.
set -euo pipefail

tag="${1:-}"

platforms=(
  darwin-amd64
  darwin-arm64
  freebsd-386
  freebsd-amd64
  freebsd-arm64
  linux-386
  linux-amd64
  linux-arm
  linux-arm64
)

for p in "${platforms[@]}"; do
  GOOS="${p%-*}" GOARCH="${p#*-}" CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/marekabaffy/gh-reap/internal/cli.Version=$tag" -o "dist/$p" .
done
