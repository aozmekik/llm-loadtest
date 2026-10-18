# Contributing

Thanks for taking a look. Issues and pull requests are welcome; for larger
changes, please open an issue first to agree on the approach.

## Prerequisites

- Go 1.26 or later (CI tests the two latest Go releases).
- [golangci-lint](https://golangci-lint.run/) v2.15 for linting.
- `curl` and `bash` for `examples/demo.sh`.
- Python 3.11 or later, only for the prompt-length check below.

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

If fuzzing finds a failure, Go writes the input to
`sse/testdata/fuzz/FuzzDecoder/`. Commit that file with the fix: it then
runs as a regular test case.

Synthetic prompts must stay exactly `--input-tokens` long in the
`cl100k_base` and `o200k_base` encodings. After changing the vocabulary or
the instruction in `internal/prompt`, count them with tiktoken (CI does the
same):

```sh
python3 -m venv .venv
.venv/bin/pip install -r scripts/prompttokens/requirements.txt
go run ./scripts/prompttokens -seeds 10 -count 50 | .venv/bin/python scripts/prompttokens/check.py
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

## Releasing

Nothing has been released yet. A release is a tag on `main`; the
`.goreleaser.yaml` builds the binaries and a draft GitHub release:

```sh
# Move the "Unreleased" entries under a new version heading in CHANGELOG.md first.
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
goreleaser release --clean   # or from CI
```

Until a tag exists, `go install ...@latest` installs a pseudo-version such
as `0.0.0-20261018...`, and `llm-loadtest version` prints it.
