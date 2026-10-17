# Contributing

Thanks for taking a look. Issues and pull requests are welcome; for larger
changes, please open an issue first to agree on the approach.

## Prerequisites

- Go 1.26 or later (CI tests the two latest Go releases).
- [golangci-lint](https://golangci-lint.run/) v2.15 for linting.
- `curl` and `bash` for `examples/demo.sh`.

The module has no third-party dependencies (see
[ADR 0003](docs/adr/0003-standard-library-only.md)); please discuss before
adding one.

## Build, test, lint

```sh
go build ./...                          # everything
go build -o llm-loadtest ./cmd/llm-loadtest

go test ./...                           # unit, end-to-end and example tests
go test -race -shuffle=on -count=1 ./...  # what CI runs
go test -run='^$' -fuzz=FuzzDecoder -fuzztime=60s ./sse

golangci-lint run ./...                 # lint and format check (.golangci.yml)
golangci-lint fmt ./...                 # apply gofumpt and goimports
go vet ./...

go run ./examples/library               # library example
DURATION=3s examples/demo.sh            # CLI demo against the mock
```

Markdown and CSV rendering are checked against golden files in
`internal/report/testdata`. After an intentional output change, regenerate
them and review the diff:

```sh
go test ./internal/report -update
git diff internal/report/testdata
```

Microbenchmarks:

```sh
go test -run='^$' -bench=. -benchtime=2s -count=3 ./sse ./internal/stats
```

## Writing tests

- Timing assertions belong inside `testing/synctest` bubbles, connected to
  the mock through `internal/inmem`, where the clock is fake and expected
  durations are exact. Use real-network tests only for network behaviour,
  and give them one-sided or robust assertions. See
  [ADR 0004](docs/adr/0004-deterministic-timing-tests.md).
- Statistical tests use fixed seeds and bounds of several standard errors.
- No test may depend on wall-clock speed.

## Changes

- Commits follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `docs:`, `test:`, `ci:`, `chore:`).
- User-visible changes get a line in [CHANGELOG.md](CHANGELOG.md) under
  "Unreleased".
- Changes to how a metric is measured or defined need a matching update to
  [docs/metrics.md](docs/metrics.md), and a new ADR in `docs/adr` if they
  revisit a recorded decision.
- Numbers in the documentation must come from a command you ran, stated
  next to the number with the hardware and Go version.
