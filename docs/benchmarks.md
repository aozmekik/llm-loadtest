# Benchmarks

Every number on this page comes from the built-in **mock server**, not from
a model. The runs show what llm-loadtest measures and how much it adds on
top of the server; they say nothing about any inference engine. For
real-engine numbers, run the same commands against your own server.

## Environment

- Apple M1 Pro (8 cores), 16 GB RAM, macOS 26.6.2, Go 1.27.2.
- Client and mock on the same machine, over loopback.
- Binary built from commit `aee1c21` with `go build ./cmd/llm-loadtest`.
  Later commits change error handling, reporting and documentation, not
  the measurement path.
- Other jobs were running on the machine throughout: load averages between
  7 and 35 (`uptime` before and after each run), so expect noise,
  especially in the tails.

## Measurement overhead

A mock with constant 50 ms TTFT, 10 ms ITL and 64 output tokens, so the
expected E2E is 50 + 63 x 10 = 680 ms:

```sh
llm-loadtest mock --addr 127.0.0.1:18300 --ttft 50ms --itl 10ms --output-tokens 64
llm-loadtest run --base-url http://127.0.0.1:18300/v1 --model mock --max-tokens 64 \
  --concurrency 1,8,32 --duration 20s --warmup 8
```

| Concurrency | Requests | TTFT p50 | TTFT p99 | ITL p50 | TPOT p50 | E2E p50 | E2E p99 |
|--:|--:|--:|--:|--:|--:|--:|--:|
| 1 | 30 | 51.4 ms | 56.4 ms | 10.1 ms | 10.0 ms | 681 ms | 682 ms |
| 8 | 240 | 51.3 ms | 54.2 ms | 10.1 ms | 10.0 ms | 681 ms | 684 ms |
| 32 | 960 | 51.0 ms | 56.2 ms | 10.1 ms | 10.0 ms | 681 ms | 688 ms |

Measured TTFT p50 is 1.0 to 1.4 ms above the configured value, which covers
the mock's timer, the loopback round trip and the client. ITL p50 reads
10.1 ms, within the sketch's 1% accuracy of the configured 10 ms; TPOT,
which averages over each request's gaps exactly, reads 10.0 ms. The p99
values carry the machine's load.

## Generator ceiling

A mock with zero latency and 64 output tokens, so the client and the mock,
sharing the same CPUs, are the bottleneck:

```sh
llm-loadtest mock --addr 127.0.0.1:18301 --ttft 0s --itl 0s --output-tokens 64
llm-loadtest run --base-url http://127.0.0.1:18301/v1 --model mock --max-tokens 64 \
  --concurrency 1,16,64 --duration 10s --warmup 16 --json-records=false
```

| Concurrency | Requests | Req/s | Output tok/s | E2E p50 | E2E p99 |
|--:|--:|--:|--:|--:|--:|
| 1 | 21,215 | 2,121 | 135,771 | 0.45 ms | 1.10 ms |
| 16 | 56,802 | 5,679 | 363,453 | 2.04 ms | 12.5 ms |
| 64 | 52,850 | 5,280 | 337,944 | 4.29 ms | 67.0 ms |

Throughput flattens at about 5,300 to 5,700 req/s on this shared machine.
Real servers are orders of magnitude slower per request, so the generator
is unlikely to be the limit; if send lag grows in an open-loop run, it is.

## Saturation and coordinated omission

From [examples/demo.sh](../examples/demo.sh) with `DURATION=20s`: a mock
with 8 slots where each request takes about 2 s (120 ms TTFT, 15 ms per
token, 64 to 192 tokens), so its capacity is about 4 req/s.

Closed-loop sweep (`--concurrency 1,2,4,8,16 --duration 20s --warmup 8`):

| Load | OK | Req/s | Out tok/s | TTFT p50 | TTFT p99 | ITL p50 | E2E p50 | E2E p99 |
|:--|--:|--:|--:|--:|--:|--:|--:|--:|
| concurrency=1 | 10 | 0.50 | 62.6 | 115 | 146 | 15.3 | 1908 | 2782 |
| concurrency=2 | 23 | 1.12 | 125 | 117 | 158 | 15.0 | 1695 | 2768 |
| concurrency=4 | 43 | 2.00 | 244 | 122 | 163 | 15.0 | 1883 | 2976 |
| concurrency=8 | 87 | 3.95 | 494 | 120 | 164 | 15.0 | 2086 | 2978 |
| concurrency=16 | 92 | 3.78 | 477 | 2071 | 2710 | 15.0 | 3939 | 4937 |

Throughput saturates at 8 streams; at 16 the extra requests wait for a
slot and TTFT jumps from 120 ms to 2 s. The same sweep against a mock with
16 slots (with `--seed 2`), compared with `llm-loadtest compare`, shows the
concurrency=16 stage improving by +101.4% in req/s and -94.3% in TTFT p50.

Open loop at Poisson rates below and above capacity (`--rate 2,6
--duration 20s`, prompts from `examples/prompts.jsonl`):

| Load | OK | Req/s | TTFT p50 | TTFT p99 | E2E p50 | E2E p99 | Stage length |
|:--|--:|--:|--:|--:|--:|--:|--:|
| rate=2/s poisson | 45 | 2.05 | 120 | 158 | 1522 | 2824 | 21.9 s |
| rate=6/s poisson | 127 | 4.16 | 5283 | 9044 | 7467 | 11286 | 30.6 s |

Latencies in ms. The closed loop at concurrency 8 reported a TTFT p99 of
164 ms at 3.95 req/s. Offered 6 req/s, the same server completes about
4 req/s and its queue grows for the whole stage: TTFT p50 is 5.3 s, and
the stage needs 10.6 s after its 20 s schedule to drain. A closed loop
cannot show this, because it never offers more than the server completes.

## Microbenchmarks

```sh
go test -run='^$' -bench=. -benchtime=2s -count=3 ./sse ./internal/stats
```

| Benchmark | Result | Allocations |
|---|---|---|
| SSE decoder, 1,001 OpenAI-style events (157 KiB) | 1,070 to 1,174 MB/s | 1,004 allocs per stream |
| DDSketch `Add` | 14.7 to 15.2 ns per value | 0 |

Three iterations each, at commit `461c229` (the same `sse` and
`internal/stats` code as `aee1c21`), at a load average of about 19 to 24.
