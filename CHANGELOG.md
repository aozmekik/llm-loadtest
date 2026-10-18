# Changelog

All notable changes to this project are documented in this file. The format
is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While the version is below 1.0, flags, output formats and the Go API may
change between minor versions.

## [Unreleased]

## [0.1.0] - 2026-10-18

### Added

- `llm-loadtest run`: load tests for OpenAI-compatible streaming
  chat-completion endpoints, reporting TTFT, inter-token latency, TPOT,
  end-to-end latency, token counts and throughput with p50/p90/p95/p99.
- Closed-loop load at fixed concurrency, concurrency sweeps, and open-loop
  Poisson or constant arrivals with latency measured from the scheduled
  start to avoid coordinated omission.
- Exact token counts via `stream_options.include_usage`, with a labelled
  chunk-count fallback.
- Synthetic prompts of a target length (exact for the cl100k_base and
  o200k_base encodings, checked in CI with tiktoken; at least 18 tokens)
  and JSONL datasets.
- Warmup, per-request and idle timeouts, extra headers, extra request body
  fields, and `--max-tokens-field` to send the limit as
  `max_completion_tokens`.
- An API key read from an environment variable: `OPENAI_API_KEY` by
  default, sent only to api.openai.com; any other variable named with
  `--api-key-env`. Redirects are not followed, and configured secrets are
  scrubbed from error messages.
- Error counts by class (`http_<status>`, `timeout`, `idle_timeout`,
  `connection`, `stream_error`, `protocol`, `incomplete`), with requests
  canceled by an interrupt counted separately.
- Live progress, Markdown report, and JSON, CSV and Markdown files via
  `--out`.
- `llm-loadtest compare` to diff two JSON results. With `--threshold` (and
  `--error-threshold` for the error rate) it exits 1 on a regression, a
  missing or failed stage, or an interrupted run, for use as a CI gate.
- `llm-loadtest mock` and the `mock` package: an OpenAI-compatible server
  with configurable latency distributions, output lengths, slots and error
  injection.
- The `sse` package: a streaming Server-Sent Events decoder following the
  WHATWG parsing rules.

[Unreleased]: https://github.com/aozmekik/llm-loadtest/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/aozmekik/llm-loadtest/releases/tag/v0.1.0
