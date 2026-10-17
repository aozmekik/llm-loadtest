package stats

import (
	"errors"
	"math"
)

// minIndexable is the smallest value given its own logarithmic bucket.
// Smaller values, including zero and negatives, go to the zero bucket.
const minIndexable = 1e-9

// ErrAccuracyMismatch is returned by Merge when the sketches were created
// with different relative accuracies.
var ErrAccuracyMismatch = errors.New("stats: cannot merge sketches with different relative accuracy")

// Sketch is a quantile sketch with a relative-error guarantee, following
// DDSketch (Masson, Rim and Lee, "DDSketch: A Fast and Fully-Mergeable
// Quantile Sketch with Relative-Error Guarantees", VLDB 2019).
//
// Values are counted in logarithmically sized buckets, so every element
// can be recovered to within a factor (1 ± a) for relative accuracy a.
// Quantile interpolates between the two closest ranks exactly as the exact
// Quantile function does, so for non-negative data its estimate is within
// a factor (1 ± a) of the exact interpolated quantile. Memory grows with
// the logarithm of the value range, not with the number of values, and
// Count, Mean, Stddev, Min and Max are exact.
//
// Values at or below 1e-9, including zero and negative values, are counted
// in a dedicated zero bucket and estimated as zero (clamped to Min). The
// zero value of Sketch is not usable; create one with NewSketch. A Sketch is
// not safe for concurrent use.
type Sketch struct {
	alpha    float64
	logGamma float64
	bins     []int
	offset   int // bins[i] counts values whose bucket index is offset+i
	zeros    int
	n        int
	mean, m2 float64 // running moments (Welford)
	min, max float64
}

// NewSketch returns an empty sketch with the given relative accuracy, which
// must be in the open interval (0, 1); 0.01 means quantiles are accurate to
// within 1%. It panics on an invalid accuracy.
func NewSketch(relativeAccuracy float64) *Sketch {
	if !(relativeAccuracy > 0 && relativeAccuracy < 1) {
		panic("stats: relative accuracy must be in (0, 1)")
	}
	gamma := (1 + relativeAccuracy) / (1 - relativeAccuracy)
	return &Sketch{alpha: relativeAccuracy, logGamma: math.Log(gamma)}
}

// RelativeAccuracy returns the accuracy the sketch was created with.
func (s *Sketch) RelativeAccuracy() float64 { return s.alpha }

// Count returns the number of values added.
func (s *Sketch) Count() int { return s.n }

// Add records one value. NaN and infinite values are ignored.
func (s *Sketch) Add(v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	if s.n == 0 {
		s.min, s.max = v, v
	} else {
		s.min, s.max = min(s.min, v), max(s.max, v)
	}
	s.n++
	delta := v - s.mean
	s.mean += delta / float64(s.n)
	s.m2 += delta * (v - s.mean)

	if v <= minIndexable {
		s.zeros++
		return
	}
	s.increment(s.index(v), 1)
}

// Merge adds every value recorded in other to s. Both sketches must have the
// same relative accuracy.
func (s *Sketch) Merge(other *Sketch) error {
	if other.alpha != s.alpha {
		return ErrAccuracyMismatch
	}
	if other.n == 0 {
		return nil
	}
	if s.n == 0 {
		s.min, s.max = other.min, other.max
	} else {
		s.min, s.max = min(s.min, other.min), max(s.max, other.max)
	}
	na, nb := float64(s.n), float64(other.n)
	total := na + nb
	delta := other.mean - s.mean
	s.mean += delta * nb / total
	s.m2 += other.m2 + delta*delta*na*nb/total
	s.n += other.n

	s.zeros += other.zeros
	for i, c := range other.bins {
		if c > 0 {
			s.increment(other.offset+i, c)
		}
	}
	return nil
}

// Quantile returns an estimate of the q-quantile, 0 <= q <= 1, or NaN if the
// sketch is empty. Like the exact Quantile function it interpolates
// linearly between closest ranks (Hyndman and Fan definition 7).
// Quantile(0) and Quantile(1) return the exact minimum and maximum.
func (s *Sketch) Quantile(q float64) float64 {
	if s.n == 0 {
		return math.NaN()
	}
	switch {
	case q <= 0:
		return s.min
	case q >= 1:
		return s.max
	}
	h := q * float64(s.n-1)
	lo := int(math.Floor(h))
	frac := h - float64(lo)
	vlo := s.valueAtRank(lo)
	if frac == 0 || lo+1 >= s.n {
		return vlo
	}
	return vlo + frac*(s.valueAtRank(lo+1)-vlo)
}

// valueAtRank estimates the element of 0-based rank r in sorted order.
func (s *Sketch) valueAtRank(r int) float64 {
	if r < s.zeros {
		return s.clamp(0)
	}
	cumulative := s.zeros
	for i, c := range s.bins {
		cumulative += c
		if cumulative > r {
			return s.clamp(s.value(s.offset + i))
		}
	}
	return s.max
}

// Summary returns the sketch's statistics. Percentiles are sketch
// estimates; the other fields are exact.
func (s *Sketch) Summary() Summary {
	if s.n == 0 {
		return Summary{}
	}
	var stddev float64
	if s.n > 1 {
		stddev = math.Sqrt(max(s.m2, 0) / float64(s.n-1))
	}
	return Summary{
		Count:  s.n,
		Mean:   s.mean,
		Stddev: stddev,
		Min:    s.min,
		P50:    s.Quantile(0.50),
		P90:    s.Quantile(0.90),
		P95:    s.Quantile(0.95),
		P99:    s.Quantile(0.99),
		Max:    s.max,
	}
}

// index returns the bucket k with gamma^(k-1) < v <= gamma^k.
func (s *Sketch) index(v float64) int {
	return int(math.Ceil(math.Log(v) / s.logGamma))
}

// value returns the estimate for bucket k: the point whose relative
// distance to both bucket bounds equals the accuracy.
func (s *Sketch) value(k int) float64 {
	gamma := math.Exp(s.logGamma)
	return 2 * math.Exp(float64(k)*s.logGamma) / (gamma + 1)
}

func (s *Sketch) clamp(v float64) float64 { return min(max(v, s.min), s.max) }

func (s *Sketch) increment(k, c int) {
	switch {
	case len(s.bins) == 0:
		s.offset = k
		s.bins = append(s.bins, 0)
	case k < s.offset:
		grown := make([]int, s.offset-k+len(s.bins))
		copy(grown[s.offset-k:], s.bins)
		s.bins, s.offset = grown, k
	case k >= s.offset+len(s.bins):
		s.bins = append(s.bins, make([]int, k-s.offset-len(s.bins)+1)...)
	}
	s.bins[k-s.offset] += c
}
