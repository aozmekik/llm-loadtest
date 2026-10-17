# Metrics

This page defines every number llm-loadtest reports. Different benchmarking
tools define these metrics slightly differently, so compare numbers across
tools only after checking the definitions.

## Timestamps

For each request the client records:

| Timestamp | Meaning |
|---|---|
| scheduled | When the load model wanted the request to start. In closed-loop mode this is the moment a worker becomes free. In open-loop mode it is the arrival time drawn from the schedule. |
| sent | When the request was handed to Go's HTTP client. |
| first token | When the first chunk carrying generated output arrived: non-empty `delta.content`, `delta.reasoning_content`, `delta.reasoning` or `delta.tool_calls`. A leading chunk that only announces `"role": "assistant"` is not a token. |
| chunk *i* | When the *i*-th chunk carrying generated output arrived. |
| end | When `data: [DONE]` arrived (or end of stream after a `finish_reason`), or when the request failed. |

"Arrived" means the SSE decoder dispatched the event, so it includes reading
from the socket and parsing the event, but not JSON decoding of later
events. The decoder never waits for more input than it needs; see
[`sse`](../sse/sse.go).

## Per-request metrics

| Metric | Definition | Notes |
|---|---|---|
| TTFT | first token - scheduled | Undefined (null) when no output arrived. |
| E2E | end - scheduled | End-to-end latency. |
| ITL | chunk *i+1* - chunk *i* | One sample per gap, so a request with *n* chunks contributes *n-1* samples. |
| TPOT | (end - first token) / (output tokens - 1) | Time per output token after the first. Undefined below two tokens. |
| max ITL | the longest ITL gap of the request | Shows stalls that percentiles over all gaps can hide. |
| output tokens | `usage.completion_tokens` if the server sent usage, else the number of output chunks | See [token counts](#token-counts). |
| input tokens | `usage.prompt_tokens` | Only when the server sent usage. |
| output tok/s per request | output tokens / E2E | Includes the time to first token. |
| send lag | sent - scheduled | Near zero unless the generator fell behind or `--max-in-flight` was reached. |

TTFT and E2E are measured from the **scheduled** start rather than from the
send time. In closed-loop mode the two are the same to within microseconds.
In open-loop mode, any delay between the scheduled and the actual send is
time a real user would have spent waiting, so it belongs in the latency;
see [ADR 0001](adr/0001-open-loop-latency-from-scheduled-time.md).

ITL is measured between **chunks**, not tokens. Engines commonly stream
one token per chunk, but some servers, settings and proxies batch several
tokens into one chunk. When they do, ITL describes the chunk cadence, and
TPOT, which uses the exact token count, is the per-token figure. Comparing
output tokens with the chunk count in the CSV shows which case applies.

## Stage aggregates

| Metric | Definition |
|---|---|
| requests | Requests issued in the stage, successful or not. |
| OK / errors | Successful and failed requests. Errors are counted by class. |
| req/s | Successful requests / stage duration. |
| out tok/s | Output tokens of successful requests / stage duration. |
| stage duration | From the stage start until the last request finished. A stage stops *issuing* requests when `--duration` elapses or `--requests` have been issued, then waits for requests in flight, so the duration includes that drain. |
| p50, p90, p95, p99 | Percentiles over successful requests (ITL: over all gaps of successful requests). |

Distributions (TTFT, ITL, TPOT, E2E, token counts) include successful
requests only, so a fast-failing error does not make latency look better.
Send lag includes every request.

### Percentile method

Per-request metrics keep every sample and use exact percentiles with linear
interpolation between the closest ranks (Hyndman and Fan definition 7, the
default of NumPy and R).

ITL produces one sample per token, which can be millions per stage. It is
summarized with a [DDSketch](https://arxiv.org/abs/1908.10693) quantile
sketch with 1% relative accuracy, using the same interpolation, so ITL
percentiles are within 1% of the exact values. Count, mean, standard
deviation, minimum and maximum are exact. See
[ADR 0002](adr/0002-ddsketch-for-inter-token-latency.md).

## Token counts

By default every request sets `"stream_options": {"include_usage": true}`,
which asks the server for a final chunk with exact `prompt_tokens` and
`completion_tokens`. The field is part of the OpenAI API and many
OpenAI-compatible servers implement it; check your server's documentation,
or simply look at the report. If a server does not send usage, output
tokens are approximated
by counting output chunks, and the report says so in its "Token counts"
line and per request (`token_source`: `usage` or `chunks`). Servers that
reject the field can be run with `--include-usage=false`.

## Error classes

| Class | Meaning |
|---|---|
| `http_<status>` | The server answered with a non-200 status, e.g. `http_429`. |
| `timeout` | The request did not finish within `--timeout`. |
| `idle_timeout` | No bytes arrived for `--idle-timeout`. |
| `connection` | Connecting failed, or the connection broke mid-stream. |
| `stream_error` | The server sent an error payload in the stream, either `{"error": ...}`, `{"object": "error", ...}` or an `event: error`. |
| `protocol` | The response broke the streaming protocol: invalid JSON, an event over 1 MiB, or `application/json` instead of an event stream. |
| `incomplete` | The stream ended without `[DONE]` or a `finish_reason`. |
| `canceled` | The run was interrupted (Ctrl-C) while the request was in flight. |

Each class keeps the first error message as a sample. Messages never contain
the API key, header values or base-URL credentials and query values, even
if the server echoes them back.
