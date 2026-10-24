# Contributing

Issues and pull requests are welcome.

## Before you open a pull request

1. Run `gofmt -l .` (it must print nothing), `go vet ./...` and `go test -race ./...`.
2. Add a test for each change in behaviour. The tests run against a fake CLI (`fake_test.go`): the
   test binary runs itself again as a scripted `claude`, so no login and no network are needed.
3. Keep the module free of dependencies outside the standard library.

## Changes to the protocol

The stream-json protocol is not a documented stable API. A change that depends on a new CLI
behaviour must say which CLI version it was checked on and how. If you raise `TestedVersion`,
follow "Re-verifying on a CLI upgrade" in [docs/protocol.md](docs/protocol.md), run `TestRealCLI`
against the new version, and update the version at the top of that page.

`TestRealCLI` uses your own Claude login and a model call (about one cent). It only runs with
`CCGO_CLAUDE_IT=1`.

## Licence

By contributing you agree that your contribution is licensed under the MIT licence in
[LICENSE](LICENSE).
