# ADR 0004: Test timing with a fake clock and an in-memory transport

- Status: accepted
- Date: 2026-10-18

## Context

The core claims of a latency tool are timing claims: TTFT matches what the
server did, the open-loop schedule holds when the server is slow, queueing
shows up in latency. Tests of such claims against real time are slow and
flaky: `time.Sleep(10 * time.Millisecond)` can take 11 ms or 30 ms on a
busy CI runner, so assertions need tolerances wide enough to hide real
bugs.

Go 1.25 made `testing/synctest` stable. Inside a synctest "bubble", the
`time` package uses a fake clock that advances only when every goroutine in
the bubble is durably blocked, so sleeps, timers and context deadlines
complete instantly and exactly. Network sockets are not durably blocking,
so a loopback `httptest.Server` cannot be used inside a bubble.

## Options considered

1. **Real time with tolerances.** Simple; slow, flaky, and weak.
2. **An injected `Clock` interface** in the scheduler and client. Exact,
   but every timing call site in non-test code goes through an
   abstraction that exists for tests, and the mock server and HTTP stack
   would need it too.
3. **`testing/synctest` plus an in-memory HTTP transport.** Library code
   calls `time` directly; tests run the real client, load generator and
   mock server in a bubble, connected by an `http.RoundTripper` that calls
   the handler directly and streams the body through `io.Pipe`.

## Decision

Option 3, keeping a small number of real-network tests.

- `internal/inmem.Transport` serves requests by calling an `http.Handler`
  in a goroutine. Header writes, streaming, `http.ErrAbortHandler`
  (dropped connections) and context cancellation behave like a real
  connection from the client's point of view.
- Timing tests run inside `synctest.Test` with the mock server, so expected
  values are exact: a mock with 100 ms TTFT and 10 ms ITL yields a measured
  TTFT of exactly 100 ms and an end-to-end latency of exactly 190 ms for
  10 tokens.
- Statistical tests (Poisson arrivals, mock distributions) use fixed seeds
  and bounds of five standard errors, so they are deterministic for the
  seed and would hold for nearly any other.
- Real-network tests remain where the network path is the point:
  `httptest` servers for protocol handling, fragmented writes and dropped
  connections, and `TestTimingOverLoopback`, which asserts only one-sided
  or robust properties (TTFT is never early; the mean chunk gap is pinned
  by the mock's absolute schedule).

## Consequences

- The scheduler test, the coordinated-omission test and the timeout tests
  assert exact durations and run in milliseconds of wall time.
- The in-memory transport is test infrastructure that must stay faithful to
  `net/http` semantics; it has its own tests.
- The approach requires Go 1.25 or later, which the module's `go 1.26`
  directive already guarantees.
