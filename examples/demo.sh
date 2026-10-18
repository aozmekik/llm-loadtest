#!/usr/bin/env bash
# Rehearses a benchmark end to end against the built-in mock server; no GPU
# or API key needed. All numbers it prints describe the mock, not a model.
#
#   1. a closed-loop concurrency sweep against a mock with 8 slots,
#   2. the same sweep against a mock with 16 slots, and a comparison,
#   3. open-loop Poisson arrivals below and above the mock's capacity,
#      showing the queueing a closed loop hides (coordinated omission).
#
# Usage: examples/demo.sh            (DURATION=10s per stage by default)
#        DURATION=3s examples/demo.sh
set -euo pipefail

cd "$(dirname "$0")/.."
DURATION="${DURATION:-10s}"
PORT="${PORT:-18000}"
OUT="examples/out"
BASE_URL="http://127.0.0.1:${PORT}/v1"
mkdir -p "$OUT"

go build -o "$OUT/llm-loadtest" ./cmd/llm-loadtest
bin="$OUT/llm-loadtest"

mock_pid=""
stop_mock() {
  if [[ -n "$mock_pid" ]]; then
    kill "$mock_pid" 2>/dev/null || true
    wait "$mock_pid" 2>/dev/null || true
    mock_pid=""
  fi
}
trap stop_mock EXIT

# start_mock SLOTS: serves a mock whose requests take about 2 s each
# (120 ms to the first token, then 15 ms per token for 64-192 tokens).
start_mock() {
  stop_mock
  "$bin" mock --addr "127.0.0.1:${PORT}" --slots "$1" --seed 7 \
    --ttft normal:120ms,20ms --itl normal:15ms,3ms --output-tokens uniform:64,192 &
  mock_pid=$!
  for _ in $(seq 100); do
    if curl -fsS "${BASE_URL}/models" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  echo "mock did not start" >&2
  return 1
}

echo "== 1. Concurrency sweep, mock with 8 slots"
start_mock 8
"$bin" run --base-url "$BASE_URL" --model mock --max-tokens 192 \
  --concurrency 1,2,4,8,16 --duration "$DURATION" --warmup 8 \
  --out "$OUT/sweep-8-slots.json" --out "$OUT/sweep-8-slots.csv" --out "$OUT/sweep-8-slots.md"

echo "== 2. Same sweep, mock with 16 slots, compared with step 1"
# A second run against the same address gets its own --seed, as it should
# against a real server, whose prefix cache could serve repeated prompts.
start_mock 16
"$bin" run --base-url "$BASE_URL" --model mock --max-tokens 192 \
  --concurrency 1,2,4,8,16 --duration "$DURATION" --warmup 8 --seed 2 \
  --out "$OUT/sweep-16-slots.json" >/dev/null
"$bin" compare "$OUT/sweep-8-slots.json" "$OUT/sweep-16-slots.json"

echo "== 3. Open loop below and above capacity (8 slots, about 4 req/s)"
start_mock 8
"$bin" run --base-url "$BASE_URL" --model mock --max-tokens 192 \
  --dataset examples/prompts.jsonl --rate 2,6 --arrival poisson --duration "$DURATION" \
  --out "$OUT/open-loop.json"
