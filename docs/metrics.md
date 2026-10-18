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
| first token | When the first chunk carrying generated output arrived: non-empty `delta.content`, `delta.reasoning_content`, `delta.reasoning` or `delta.tool_calls`. A leading chunk that only announces `"role": "assistant"` (with empty content and no or empty `tool_calls`) is not a token. |
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
| requests | Requests issued in the stage: OK + errors + canceled. |
| OK / errors | Successful and failed requests. Errors are counted by class. |
| canceled | Requests in flight when the run was interrupted (Ctrl-C). They are neither successes nor errors. |
| error rate | Errors / (OK + errors), so an interrupt does not raise it. |
| req/s | Successful requests / stage duration. |
| out tok/s | Output tokens of successful requests / stage duration. |
| stage duration | From the stage start until the last request finished. A stage stops *issuing* requests when `--duration` elapses or `--requests` have been issued, then waits for requests in flight, so the duration includes that drain. In open-loop mode `--duration` bounds the *schedule*: every arrival scheduled within it is sent, also after the duration when `--max-in-flight` held it back (see below). |
| p50, p90, p95, p99 | Percentiles over successful requests (ITL: over all gaps of successful requests). |

Distributions (TTFT, ITL, TPOT, E2E, token counts) include successful
requests only, so a fast-failing error does not make latency look better.
Send lag includes every request. A statistic with no samples, such as the
TTFT of a stage in which every request failed, is shown as `-` and stored
in JSON as a summary with `"count": 0` whose other fields are zero.

**Timeouts censor the tail.** A request that hits `--timeout` or
`--idle-timeout` is an error, so it is not in the distributions, and those
are exactly the slowest requests. Under overload the percentiles can then
look healthy while most requests time out: a stage's E2E p99 can never
exceed the timeout. Read the error counts next to the percentiles; the
report adds a note to every stage with timeouts. Raising `--timeout` well
above the expected latency keeps the tail measurable.

**The open-loop backlog.** When `--max-in-flight` is reached, arrivals wait
for a free slot, and the wait counts in their latency (it is reported as
send lag). Arrivals scheduled before the end of `--duration` are still sent
after it, so a saturated stage can run much longer than `--duration`: at
10 req/s for 1 s with one slot and 1 s per request, the stage sends its
10 requests over 10 s. Dropping the late arrivals would hide the queueing
the open loop exists to measure. Raise `--max-in-flight`, or lower the
rate, if send lag grows.

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

The token limit is sent as `max_tokens` (`--max-tokens`, or a dataset
row's `max_tokens`). The OpenAI API reference marks that field as
deprecated in favour of `max_completion_tokens` and not compatible with
its o-series reasoning models; `--max-tokens-field max_completion_tokens`
sends the same limit in that field.

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
| `http_<status>` | The server answered with a non-200 status, e.g. `http_429`. Redirects are not followed: a 3xx is recorded as `http_3xx` with the redirect target in the message. |
| `timeout` | The request did not finish within `--timeout`. |
| `idle_timeout` | No bytes arrived for `--idle-timeout`. |
| `connection` | Connecting failed, or the connection broke mid-stream. |
| `stream_error` | The server sent an error payload in the stream, either `{"error": ...}`, `{"object": "error", ...}` or an `event: error`. |
| `protocol` | The response broke the streaming protocol: invalid JSON, an event over 1 MiB, or `application/json` instead of an event stream. |
| `incomplete` | The stream ended without `[DONE]` or a `finish_reason`. |
| `canceled` | The run was interrupted (Ctrl-C) while the request was in flight. Counted separately, not as an error. |

Each class keeps the first error message as a sample, shortened to about
300 bytes. Messages never contain the request URL, and configured secrets
are replaced with `[REDACTED]` even when the server echoes them: the API key
and the base URL's password whatever their length, and header values, the
URL's username and query values of 8 bytes or more, each also in its
URL-escaped forms. A server that echoes only part of a secret is not
caught. See [ADR 0005](adr/0005-credentials-go-only-where-pointed.md).
