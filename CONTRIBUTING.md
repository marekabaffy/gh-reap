# Contributing

Bug reports and pull requests are welcome. For a larger change, open an issue
first so we can agree on the approach. Security issues go through
[SECURITY.md](SECURITY.md), not public issues.

## Layout

- `internal/conn`: every external command (git, lsof, du) and the GitHub GraphQL call,
  behind the `Conn` interface, plus pure parsers for their output. `testdata/` holds
  recorded real output.
- `internal/conn/conntest`: a fake `Conn` that answers from those recordings.
- `internal/reap`: the rules (`DecideBranch`, `DecideTree`: pure functions over facts,
  each returning a verdict and a reason), gathering the facts, removal and the lock.
- `internal/cli`: flags, help, table and JSON output.
- `e2e_test.go`: builds the binary and runs it against throwaway repositories.

## Tests

```
go test -race ./...                                   # unit and integration
go test -tags contract -run Contract ./internal/conn  # real git vs the recordings
```

- Unit tests replay `internal/conn/testdata` through the fake. Each rule has a table row
  per reason.
- Contract tests build a fixture repository with real git (fixed identities and dates, so
  object ids never change) and check every command still prints what was recorded. After a
  deliberate change, re-record with `-update` and review the diff. CI runs them nightly
  against the newest git from the git-core PPA.
- Integration tests serve GitHub from a local server: with `GH_HOST=github.localhost`,
  go-gh calls `http://api.github.localhost/graphql`, and `HTTP_PROXY` routes it to the
  test. They need a git with config-based hooks and `lsof`. Set `E2E_COVERDIR=<dir>` to
  collect the binary's coverage (`go tool covdata percent -i <dir>`).

Before a pull request, lint and format with the configuration in
`.golangci.yml`, and check dependencies for known vulnerabilities:

```
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 fmt ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

## Releases

Push a tag `vX.Y.Z`; the release workflow builds with `script/build.sh` and publishes with
`cli/gh-extension-precompile`.
