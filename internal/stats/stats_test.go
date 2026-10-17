package stats

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

func approxEqual(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestQuantileKnownValues(t *testing.T) {
	t.Parallel()
	oneToTen := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	tests := []struct {
		name   string
		sorted []float64
		q      float64
		want   float64
	}{
		// Expected values match numpy.percentile(x, 100*q) (method="linear").
		{"median even", oneToTen, 0.5, 5.5},
		{"p90", oneToTen, 0.9, 9.1},
		{"p95", oneToTen, 0.95, 9.55},
		{"p99", oneToTen, 0.99, 9.91},
		{"p0", oneToTen, 0, 1},
		{"p100", oneToTen, 1, 10},
		{"clamped low", oneToTen, -0.5, 1},
		{"clamped high", oneToTen, 1.5, 10},
		{"median odd", []float64{1, 3, 7}, 0.5, 3},
		{"interpolated p40", []float64{15, 20, 35, 40, 50}, 0.4, 29},
		{"single value", []float64{42}, 0.99, 42},
		{"repeated values", []float64{2, 2, 2, 2}, 0.75, 2},
	}
	for _, tt := range tests {
		if got := Quantile(tt.sorted, tt.q); !approxEqual(got, tt.want, 1e-12) {
			t.Errorf("%s: Quantile(%v, %v) = %v, want %v", tt.name, tt.sorted, tt.q, got, tt.want)
		}
	}
	if got := Quantile(nil, 0.5); !math.IsNaN(got) {
		t.Errorf("Quantile(nil) = %v, want NaN", got)
	}
}

func TestSummarize(t *testing.T) {
	t.Parallel()
	values := []float64{7, 3, 10, 1, 5, 9, 2, 8, 4, 6} // 1..10 shuffled
	input := slices.Clone(values)
	got := Summarize(values)
	want := Summary{
		Count: 10, Mean: 5.5, Stddev: math.Sqrt(82.5 / 9),
		Min: 1, P50: 5.5, P90: 9.1, P95: 9.55, P99: 9.91, Max: 10,
	}
	if !summariesClose(got, want, 1e-12) {
		t.Errorf("Summarize = %+v, want %+v", got, want)
	}
	if !slices.Equal(values, input) {
		t.Error("Summarize modified its input")
	}
}

func TestSummarizeEdgeCases(t *testing.T) {
	t.Parallel()
	if got := Summarize(nil); got != (Summary{}) {
		t.Errorf("Summarize(nil) = %+v, want zero Summary", got)
	}
	if got := Summarize([]float64{math.NaN()}); got != (Summary{}) {
		t.Errorf("Summarize([NaN]) = %+v, want zero Summary", got)
	}
	got := Summarize([]float64{42, math.NaN()})
	want := Summary{Count: 1, Mean: 42, Min: 42, P50: 42, P90: 42, P95: 42, P99: 42, Max: 42}
	if got != want {
		t.Errorf("Summarize([42, NaN]) = %+v, want %+v", got, want)
	}
}

func summariesClose(a, b Summary, tol float64) bool {
	return a.Count == b.Count &&
		approxEqual(a.Mean, b.Mean, tol) && approxEqual(a.Stddev, b.Stddev, tol) &&
		approxEqual(a.Min, b.Min, tol) && approxEqual(a.Max, b.Max, tol) &&
		approxEqual(a.P50, b.P50, tol) && approxEqual(a.P90, b.P90, tol) &&
		approxEqual(a.P95, b.P95, tol) && approxEqual(a.P99, b.P99, tol)
}

// lowerQuantile is the element the sketch guarantees to approximate.
func lowerQuantile(sorted []float64, q float64) float64 {
	return sorted[int(q*float64(len(sorted)-1))]
}

func TestSketchRelativeErrorGuarantee(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	distributions := map[string]func() float64{
		"uniform":     func() float64 { return 1 + 99*rng.Float64() },
		"exponential": func() float64 { return 20 * rng.ExpFloat64() },
		"lognormal":   func() float64 { return math.Exp(3 + 1.5*rng.NormFloat64()) },
		"wide range":  func() float64 { return math.Pow(10, -6+12*rng.Float64()) },
		"with zeros": func() float64 {
			if rng.IntN(10) == 0 {
				return 0
			}
			return 5 + rng.NormFloat64()
		},
	}
	quantiles := []float64{0.01, 0.1, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99, 0.999}
	for _, alpha := range []float64{0.01, 0.05} {
		for name, gen := range distributions {
			s := NewSketch(alpha)
			values := make([]float64, 20_000)
			for i := range values {
				values[i] = gen()
				s.Add(values[i])
			}
			slices.Sort(values)
			for _, q := range quantiles {
				exact := lowerQuantile(values, q)
				got := s.Quantile(q)
				if exact <= minIndexable {
					if got != max(0, values[0]) {
						t.Errorf("%s alpha=%v q=%v: got %v, want zero bucket", name, alpha, q, got)
					}
					continue
				}
				if relErr := math.Abs(got-exact) / exact; relErr > alpha*(1+1e-9) {
					t.Errorf("%s alpha=%v q=%v: estimate %v, exact %v, relative error %.4f > %v",
						name, alpha, q, got, exact, relErr, alpha)
				}
			}
		}
	}
}

func TestSketchExactMoments(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(3, 4))
	s := NewSketch(0.01)
	values := make([]float64, 5000)
	for i := range values {
		values[i] = 10 + 3*rng.NormFloat64()
		s.Add(values[i])
	}
	got, want := s.Summary(), Summarize(values)
	if got.Count != want.Count || got.Min != want.Min || got.Max != want.Max {
		t.Errorf("count/min/max = %d/%v/%v, want %d/%v/%v", got.Count, got.Min, got.Max, want.Count, want.Min, want.Max)
	}
	if !approxEqual(got.Mean, want.Mean, 1e-9) || !approxEqual(got.Stddev, want.Stddev, 1e-9) {
		t.Errorf("mean/stddev = %v/%v, want %v/%v", got.Mean, got.Stddev, want.Mean, want.Stddev)
	}
	if s.Count() != len(values) {
		t.Errorf("Count() = %d, want %d", s.Count(), len(values))
	}
}

func TestSketchMerge(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(5, 6))
	whole, a, b := NewSketch(0.02), NewSketch(0.02), NewSketch(0.02)
	values := make([]float64, 0, 10_000)
	for i := range 10_000 {
		v := 50 * rng.ExpFloat64()
		if i%7 == 0 {
			v = 0
		}
		values = append(values, v)
		whole.Add(v)
		if i%3 == 0 {
			a.Add(v)
		} else {
			b.Add(v)
		}
	}
	if err := a.Merge(b); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	for _, q := range []float64{0, 0.1, 0.5, 0.9, 0.99, 1} {
		if got, want := a.Quantile(q), whole.Quantile(q); got != want {
			t.Errorf("merged Quantile(%v) = %v, want %v", q, got, want)
		}
	}
	got, want := a.Summary(), Summarize(values)
	if got.Count != want.Count || !approxEqual(got.Mean, want.Mean, 1e-9) || !approxEqual(got.Stddev, want.Stddev, 1e-9) {
		t.Errorf("merged moments = %+v, want count/mean/stddev of %+v", got, want)
	}

	empty := NewSketch(0.02)
	if err := empty.Merge(whole); err != nil {
		t.Fatalf("Merge into empty: %v", err)
	}
	if empty.Quantile(0.5) != whole.Quantile(0.5) || empty.Count() != whole.Count() {
		t.Error("merging into an empty sketch did not copy its contents")
	}
	if err := whole.Merge(NewSketch(0.02)); err != nil || whole.Count() != len(values) {
		t.Errorf("merging an empty sketch changed the count or failed: %v", err)
	}
	if err := a.Merge(NewSketch(0.05)); err == nil {
		t.Error("Merge with different accuracy succeeded, want ErrAccuracyMismatch")
	}
}

func TestSketchEdgeCases(t *testing.T) {
	t.Parallel()
	s := NewSketch(0.01)
	if !math.IsNaN(s.Quantile(0.5)) {
		t.Error("empty sketch Quantile is not NaN")
	}
	if s.Summary() != (Summary{}) {
		t.Error("empty sketch Summary is not zero")
	}
	s.Add(math.NaN())
	s.Add(math.Inf(1))
	if s.Count() != 0 {
		t.Errorf("non-finite values were counted: Count() = %d", s.Count())
	}
	for range 3 {
		s.Add(0)
	}
	if got := s.Quantile(0.5); got != 0 {
		t.Errorf("all-zero sketch Quantile(0.5) = %v, want 0", got)
	}
	s.Add(-1)
	if got := s.Quantile(0); got != -1 {
		t.Errorf("Quantile(0) = %v, want exact minimum -1", got)
	}
	single := NewSketch(0.01)
	single.Add(12.5)
	if got := single.Quantile(0.99); got != 12.5 {
		t.Errorf("single-value Quantile = %v, want 12.5 (clamped to min/max)", got)
	}
	if single.RelativeAccuracy() != 0.01 {
		t.Errorf("RelativeAccuracy() = %v", single.RelativeAccuracy())
	}
}

func TestSketchMemoryIsLogarithmic(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(7, 8))
	s := NewSketch(0.01)
	for range 200_000 {
		s.Add(math.Pow(10, -3+7*rng.Float64())) // 1e-3 .. 1e4
	}
	// ln(1e7)/ln(1.0202) is about 806 buckets.
	if len(s.bins) > 820 {
		t.Errorf("sketch uses %d buckets for a 1e7 value range, want about 806", len(s.bins))
	}
}

func TestNewSketchPanicsOnInvalidAccuracy(t *testing.T) {
	t.Parallel()
	for _, a := range []float64{0, 1, -0.1, 2, math.NaN()} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewSketch(%v) did not panic", a)
				}
			}()
			NewSketch(a)
		}()
	}
}

func BenchmarkSketchAdd(b *testing.B) {
	rng := rand.New(rand.NewPCG(9, 10))
	values := make([]float64, 4096)
	for i := range values {
		values[i] = 20 * rng.ExpFloat64()
	}
	s := NewSketch(0.01)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		s.Add(values[i&4095])
		i++
	}
}
