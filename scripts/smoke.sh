#!/bin/sh
# Smoke-tests an installed llm-loadtest: prints the version, starts the mock
# and runs a short load test against it. Used by CI on the .deb, .rpm, .apk
# and snap builds, so it needs only a POSIX shell.
#
# Usage: scripts/smoke.sh [binary]      (default: llm-loadtest on PATH)
set -eu

bin="${1:-llm-loadtest}"
port="${PORT:-18123}"

"$bin" version

"$bin" mock --addr "127.0.0.1:${port}" --slots 2 --seed 1 \
  --ttft normal:20ms,5ms --itl normal:2ms,1ms --output-tokens uniform:8,16 &
mock_pid=$!
trap 'kill "$mock_pid" 2>/dev/null || true' EXIT

# No curl in the minimal images, so give the mock a moment to start listening.
sleep 1

"$bin" run --base-url "http://127.0.0.1:${port}/v1" --model mock \
  --max-tokens 16 --concurrency 2 --duration 2s
