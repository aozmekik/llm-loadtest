# ADR 0001: Offer open-loop load and measure latency from the scheduled start

- Status: accepted
- Date: 2026-10-18

## Context

The most common way to load-test an inference server is a closed loop: N
workers, each sending its next request as soon as the previous one
finishes. A closed loop answers "how does the server behave with N streams
in flight?", which is useful for capacity planning, but it cannot measure
the latency users see under a given traffic level.

The reason is coordinated omission, described by Gil Tene in "How NOT to
Measure Latency" (2015). When the server slows down, a closed-loop worker
slows down with it: it simply sends fewer requests. The requests that real
users would have sent during the slowdown, and the queueing they would have
experienced, never happen and are never measured. The generator and the
server coordinate to hide the bad periods. The effect is largest exactly
where it matters: near and past saturation.

Real users do not wait for each other. Their arrivals are better modeled as
independent events at some rate, classically a Poisson process.

A second, subtler form of the problem appears inside an open-loop
generator. If latency is timed from when a request was actually sent, any
delay between the planned and the actual send (a saturated generator, an
in-flight cap, GC pauses) is again silently dropped.

## Options considered

1. **Closed loop only.** Simple, and what many tools do. Hides queueing past
   saturation, as above.
2. **Open loop, latency from the actual send time.** Arrivals no longer
   wait for the server, but generator-side delays still vanish from the
   numbers.
3. **Open loop, latency from the scheduled start.** Each request has an
   intended start time taken from the arrival schedule; TTFT and end-to-end
   latency are measured from it. This is the correction used by wrk2 and
   HdrHistogram's recording with expected intervals.
4. **Post-hoc correction** of closed-loop samples (back-filling synthetic
   samples for stalls, as HdrHistogram's `recordValueWithExpectedInterval`
   can do). It works on top of a closed loop but invents samples and needs
   an assumed expected interval.

## Decision

Support both load models, and measure open-loop latency from the scheduled
start (option 3, alongside option 1 for capacity questions).

- `--concurrency` runs a closed loop; a list runs a sweep.
- `--rate` runs an open loop with Poisson (default) or constant arrivals;
  a list runs a sweep.
- Arrival times are computed from the schedule alone: arrival *k* is at
  `start + offset(k)`, where the offsets come from the seeded arrival
  process and never from when earlier requests finished. For constant
  arrivals the offset is computed as `k / rate` rather than accumulated, so
  rounding cannot drift.
- Every request records its scheduled start. TTFT and E2E are measured
  from it in both modes (in closed-loop mode it equals the send time).
- The gap between scheduled and actual send is reported separately as
  **send lag**, so a user can see when the generator itself fell behind.
- `--max-in-flight` (default 1024) caps open-loop concurrency to protect the
  client. Because latency is measured from the scheduled start, a request
  that waits for the cap carries that wait in its latency instead of
  silently shifting the schedule.

## Consequences

- Past saturation, open-loop latencies grow without bound for the length of
  the stage, which is the honest answer to "what happens at this rate?".
  The README demo shows it: against a mock that serves about 4 req/s,
  Poisson arrivals at 6 req/s gave a TTFT p50 of 5.3 s, while a closed
  loop at concurrency 8 reported 120 ms at about the same throughput.
- Open-loop results depend on the stage duration once the server is
  saturated (the queue keeps growing). Reports state the duration, and
  comparisons should use equal durations.
- The tests pin the behaviour down exactly with a fake clock
  (`testing/synctest`): `TestCoordinatedOmission` shows the closed loop
  reporting 90 ms per request while the open loop reports the queue
  growing by 40 ms per request, and `TestMaxInFlightKeepsLatencyHonest`
  shows the in-flight cap's delay appearing as send lag and latency.
- A closed loop remains the right tool for "N concurrent streams"
  questions and is still the default.

## References

- Gil Tene, "How NOT to Measure Latency", Strange Loop 2015.
- wrk2 README, "A constant throughput, correct latency recording variant of
  wrk": https://github.com/giltene/wrk2
- HdrHistogram, `recordValueWithExpectedInterval`:
  https://hdrhistogram.github.io/HdrHistogram/
