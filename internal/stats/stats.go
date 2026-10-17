// Package stats computes summary statistics: exact percentiles for small
// per-request samples, and a mergeable relative-error quantile sketch for
// high-volume samples such as inter-token latencies.
package stats

import (
	"math"
	"slices"
)

// Summary describes the distribution of a sample. All fields are zero for an
// empty sample, so a Summary always encodes to valid JSON.
type Summary struct {
	Count  int     `json:"count"`
	Mean   float64 `json:"mean"`
	Stddev float64 `json:"stddev"`
	Min    float64 `json:"min"`
	P50    float64 `json:"p50"`
	P90    float64 `json:"p90"`
	P95    float64 `json:"p95"`
	P99    float64 `json:"p99"`
	Max    float64 `json:"max"`
}

// Summarize returns exact statistics for values. Percentiles use linear
// interpolation between closest ranks (Hyndman and Fan definition 7, the
// default of NumPy and R). Stddev is the sample standard deviation. NaN
// values are ignored. values is not modified.
func Summarize(values []float64) Summary {
	sorted := make([]float64, 0, len(values))
	for _, v := range values {
		if !math.IsNaN(v) {
			sorted = append(sorted, v)
		}
	}
	if len(sorted) == 0 {
		return Summary{}
	}
	slices.Sort(sorted)

	var sum float64
	for _, v := range sorted {
		sum += v
	}
	mean := sum / float64(len(sorted))
	var sq float64
	for _, v := range sorted {
		sq += (v - mean) * (v - mean)
	}
	var stddev float64
	if len(sorted) > 1 {
		stddev = math.Sqrt(sq / float64(len(sorted)-1))
	}
	return Summary{
		Count:  len(sorted),
		Mean:   mean,
		Stddev: stddev,
		Min:    sorted[0],
		P50:    Quantile(sorted, 0.50),
		P90:    Quantile(sorted, 0.90),
		P95:    Quantile(sorted, 0.95),
		P99:    Quantile(sorted, 0.99),
		Max:    sorted[len(sorted)-1],
	}
}

// Quantile returns the q-quantile (0 <= q <= 1) of an ascending sorted
// sample using linear interpolation between closest ranks (Hyndman and Fan
// definition 7). It returns NaN for an empty sample; q is clamped to [0, 1].
func Quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return math.NaN()
	}
	q = min(max(q, 0), 1)
	h := q * float64(n-1)
	lo := int(math.Floor(h))
	hi := min(lo+1, n-1)
	return sorted[lo] + (h-float64(lo))*(sorted[hi]-sorted[lo])
}
