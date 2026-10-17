# ADR 0003: Build on the Go standard library only

- Status: accepted
- Date: 2026-10-18

## Context

The tool needs a command line with subcommands, an HTTP client tuned for
streaming, an SSE parser, statistics, JSON and CSV output, and a mock
server. Popular choices exist for each: cobra or urfave/cli for the
command line, an SSE client library, an HdrHistogram port, testify for
assertions.

A load generator sits in the measurement path. Anything between the socket
and the timestamp (buffering, decompression, background goroutines) shows
up as latency, so every layer there has to be understood. It also has to be
easy to audit and to install with `go install`.

## Options considered

1. **cobra + an SSE library + HdrHistogram + testify.** Familiar, less code
   to write; four dependency trees to track, and an SSE library whose
   buffering and line handling sit inside the timing path.
2. **Standard library only.** More code to own: an SSE decoder, a quantile
   sketch, flag handling for subcommands.
3. **Mixed:** only cobra, because the CLI is not in the timing path.

## Decision

Use only the standard library (option 2).

- `flag.FlagSet` per subcommand gives `run`, `mock`, `compare` and
  `version` with generated help. A small helper allows flags after
  positional arguments for `compare`.
- The SSE decoder (`sse`) is written from the WHATWG specification, with
  the property that matters for timing: it dispatches an event as soon as
  its terminating line is read and never waits for lookahead. It is
  fuzz-tested against a naive reference parser.
- Statistics (`internal/stats`) implement exact percentiles and a DDSketch
  from the paper (ADR 0002).
- Tests use `testing`, `net/http/httptest` and `testing/synctest`.

## Consequences

- `go.mod` has no `require` block; `go install` fetches one module.
- About 4,100 lines of non-test Go, comments included, to maintain; the
  SSE decoder is about 260 of them and the sketch about 200.
- `flag` accepts both `-flag` and `--flag` but prints help with a single
  dash and has no shell completion. Acceptable for a tool with four
  commands.
- Upgrading Go is the only dependency update; Dependabot still watches
  GitHub Actions and would pick up future Go modules.
