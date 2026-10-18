# ADR 0002: Exact percentiles per request, a DDSketch for inter-token latency

- Status: accepted
- Date: 2026-10-18

## Context

Most metrics produce one sample per request: TTFT, end-to-end latency,
TPOT, token counts. Even a long run has thousands to hundreds of thousands
of requests, so keeping every sample costs a few megabytes at most and
gives exact percentiles.

Inter-token latency produces one sample per generated token. At 64
concurrent streams, 50 tokens/s each, a 10-minute stage produces about
1.9 million gaps; a sweep multiplies that. Keeping them all as `float64`
costs 8 bytes each, plus a sort per stage, and the cost grows with run
length rather than with the information needed (a handful of percentiles).

## Options considered

1. **Keep every ITL sample.** Exact, simple, memory and sort time grow with
   tokens generated.
2. **Reservoir sampling.** Bounded memory, but tail percentiles (p99, p99.9)
   are exactly what a small uniform reservoir estimates worst.
3. **Fixed-bucket histogram** (e.g. 1 ms buckets). Bounded and simple, but
   the absolute bucket width is wrong for both ends: too coarse for 5 ms
   ITLs, wasteful for multi-second stalls.
4. **HdrHistogram.** Bounded relative error, mergeable, well proven;
   needs a configured value range and significant-digit setting.
5. **DDSketch** (Masson, Rim and Lee, VLDB 2019). Logarithmic buckets give
   a guaranteed relative error for every quantile with no value range to
   configure; memory grows with the log of the value range; sketches merge
   exactly.

## Decision

- Per-request metrics keep every sample and use exact percentiles with
  linear interpolation (Hyndman and Fan definition 7, the NumPy default).
- ITL uses a DDSketch with 1% relative accuracy, implemented from the paper
  in `internal/stats` (about 200 lines including comments, no dependency).
- Quantile estimates interpolate between the two closest ranks, like the
  exact percentiles. Each rank's estimate is within 1% of the true element,
  and a convex combination of such estimates is within 1% of the exact
  interpolated percentile, so the two methods agree within the accuracy
  even for small samples.
- Count, mean, standard deviation (Welford's algorithm, merged with Chan et
  al.'s formula), minimum and maximum are tracked exactly.
- Zero and near-zero gaps (two chunks read in the same instant) go to a
  dedicated zero bucket.

## Consequences

- ITL memory is bounded: a 1e-3 to 1e4 ms value range needs about 806
  buckets (`TestSketchMemoryIsLogarithmic`), regardless of token count.
  `BenchmarkSketchAdd` measured 14.7 to 15.2 ns per sample with no
  allocation on an Apple M1 Pro with Go 1.27.2 (see
  [docs/benchmarks.md](../benchmarks.md) for the exact command).
- ITL percentiles are estimates. Reports say so ("sketch estimates within
  1%"), and `TestSketchRelativeErrorGuarantee` checks the bound against
  exact percentiles on uniform, exponential, lognormal, wide-range and
  zero-heavy data, for sample sizes from 2 to 20,000.
- Sketches are mergeable, which leaves the door open to per-worker sketches
  or distributed load generation without changing the report format.
- The JSON output stores the ITL summary, not the sketch, so ITL cannot be
  re-aggregated across result files. Per-request max ITL is in the records.

## References

- Charles Masson, Jee E. Rim, Homin K. Lee, "DDSketch: A Fast and
  Fully-Mergeable Quantile Sketch with Relative-Error Guarantees",
  PVLDB 12(12), 2019. https://arxiv.org/abs/1908.10693
- Rob J. Hyndman, Yanan Fan, "Sample Quantiles in Statistical Packages",
  The American Statistician 50(4), 1996.
- B. P. Welford, "Note on a Method for Calculating Corrected Sums of Squares
  and Products", Technometrics 4(3), 1962; T. F. Chan, G. H. Golub,
  R. J. LeVeque, "Updating Formulae and a Pairwise Algorithm for Computing
  Sample Variances", 1979.
