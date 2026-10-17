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
  o200k_base encodings) and JSONL datasets.
- Warmup, per-request and idle timeouts, extra headers, extra request body
  fields, and an API key read from an environment variable.
- Error counts by class (`http_<status>`, `timeout`, `idle_timeout`,
  `connection`, `stream_error`, `protocol`, `incomplete`, `canceled`).
- Live progress, Markdown report, and JSON, CSV and Markdown files via
  `--out`.
- `llm-loadtest compare` to diff two JSON results, with a regression
  threshold and exit code for CI.
- `llm-loadtest mock` and the `mock` package: an OpenAI-compatible server
  with configurable latency distributions, output lengths, slots and error
  injection.
- The `sse` package: a streaming Server-Sent Events decoder following the
  WHATWG parsing rules.
