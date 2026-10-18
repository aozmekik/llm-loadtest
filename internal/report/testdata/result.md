## llm-loadtest results

- Model: `demo` at `http://localhost:8000/v1`
- Prompts: synthetic (~256 input tokens); max_tokens 128; seed 1
- Token counts: approximate for 4 of 8 successful requests: the server sent no usage, so tokens were estimated by counting content chunks (a chunk can hold several tokens)

| Load | OK | Errors | Req/s | Out tok/s | TTFT p50 | TTFT p99 | ITL p50 | ITL p99 | TPOT p50 | E2E p50 | E2E p99 |
|:--|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| concurrency=2 | 4 | 2 | 2.00 | 26.5 | 200 | 298 | 11.1 | 19.7 | 15.0 | 450 | 597 |
| rate=4/s poisson | 4 | 2 | 2.00 | 26.5 | 200 | 298 | 11.1 | 19.7 | 15.0 | 450 | 597 |

Latencies in ms. TTFT and E2E are measured from each request's scheduled start; ITL percentiles are sketch estimates within 1%.

### concurrency=2: 6 requests in 2.0 s

| Metric | Mean | p50 | p90 | p95 | p99 | Max |
|:--|--:|--:|--:|--:|--:|--:|
| TTFT (ms) | 200 | 200 | 280 | 290 | 298 | 300 |
| ITL (ms) | 13.0 | 11.1 | 17.5 | 18.7 | 19.7 | 20.0 |
| TPOT (ms) | 15.0 | 15.0 | 19.0 | 19.5 | 19.9 | 20.0 |
| E2E (ms) | 388 | 450 | 570 | 585 | 597 | 600 |
| Output tokens | 13.2 | 11.0 | 28.0 | 29.5 | 30.7 | 31.0 |
| Input tokens | 60.0 | 60.0 | 68.0 | 69.0 | 69.8 | 70.0 |
| Output tok/s per request | 28.6 | 18.5 | 64.8 | 71.1 | 76.2 | 77.5 |

Errors: 2 x http_500 ("HTTP 500: boom")

### rate=4/s poisson: 6 requests in 2.0 s

| Metric | Mean | p50 | p90 | p95 | p99 | Max |
|:--|--:|--:|--:|--:|--:|--:|
| TTFT (ms) | 200 | 200 | 280 | 290 | 298 | 300 |
| ITL (ms) | 13.0 | 11.1 | 17.5 | 18.7 | 19.7 | 20.0 |
| TPOT (ms) | 15.0 | 15.0 | 19.0 | 19.5 | 19.9 | 20.0 |
| E2E (ms) | 388 | 450 | 570 | 585 | 597 | 600 |
| Output tokens | 13.2 | 11.0 | 28.0 | 29.5 | 30.7 | 31.0 |
| Input tokens | 60.0 | 60.0 | 68.0 | 69.0 | 69.8 | 70.0 |
| Output tok/s per request | 28.6 | 18.5 | 64.8 | 71.1 | 76.2 | 77.5 |
| Send lag (ms) | 0.33 | 0.00 | 1.00 | 1.50 | 1.90 | 2.00 |

Errors: 2 x http_500 ("HTTP 500: boom")
