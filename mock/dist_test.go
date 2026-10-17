package mock_test

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/aozmekik/llm-loadtest/mock"
)

func TestParseDurationDist(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want mock.Dist[time.Duration]
	}{
		{"50ms", mock.Constant(50 * time.Millisecond)},
		{"const:2s", mock.Constant(2 * time.Second)},
		{" uniform: 20ms , 80ms ", mock.Uniform(20*time.Millisecond, 80*time.Millisecond)},
		{"normal:50ms,10ms", mock.Normal(50*time.Millisecond, 10*time.Millisecond)},
		{"exp:1.5ms", mock.Exponential(1500 * time.Microsecond)},
		{"0s", mock.Constant(time.Duration(0))},
	}
	for _, tt := range tests {
		got, err := mock.ParseDurationDist(tt.in)
		if err != nil {
			t.Errorf("ParseDurationDist(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseDurationDist(%q) = %v, want %v", tt.in, got, tt.want)
		}
		// String output parses back to the same distribution.
		if again, err := mock.ParseDurationDist(got.String()); err != nil || again != got {
			t.Errorf("round trip of %q via %q = %v, %v", tt.in, got.String(), again, err)
		}
	}
	for _, bad := range []string{"", "fast", "50", "uniform:80ms,20ms", "uniform:10ms", "normal:1s", "exp:-5ms", "gamma:1s,2s", "exp:1s,2s"} {
		if _, err := mock.ParseDurationDist(bad); err == nil {
			t.Errorf("ParseDurationDist(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseCountDist(t *testing.T) {
	t.Parallel()
	got, err := mock.ParseCountDist("uniform:64,256")
	if err != nil || got != mock.Uniform(64, 256) {
		t.Fatalf("ParseCountDist = %v, %v", got, err)
	}
	if got.String() != "uniform:64,256" {
		t.Errorf("String() = %q", got.String())
	}
	for _, bad := range []string{"12.5", "uniform:1", "-3", "64ms"} {
		if _, err := mock.ParseCountDist(bad); err == nil {
			t.Errorf("ParseCountDist(%q) succeeded, want an error", bad)
		}
	}
}

func TestDistSampleMoments(t *testing.T) {
	t.Parallel()
	const n = 100_000
	tests := []struct {
		name       string
		dist       mock.Dist[time.Duration]
		mean, sdev float64 // in milliseconds
		lo, hi     time.Duration
	}{
		{"constant", mock.Constant(40 * time.Millisecond), 40, 0, 40 * time.Millisecond, 40 * time.Millisecond},
		{"uniform", mock.Uniform(20*time.Millisecond, 80*time.Millisecond), 50, 60 / math.Sqrt(12), 20 * time.Millisecond, 80 * time.Millisecond},
		{"normal", mock.Normal(100*time.Millisecond, 15*time.Millisecond), 100, 15, 0, time.Hour},
		{"exponential", mock.Exponential(30 * time.Millisecond), 30, 30, 0, time.Hour},
	}
	for _, tt := range tests {
		rng := rand.New(rand.NewPCG(11, 12))
		var sum, sq float64
		for range n {
			v := tt.dist.Sample(rng)
			if v < tt.lo || v > tt.hi {
				t.Fatalf("%s: sample %v outside [%v, %v]", tt.name, v, tt.lo, tt.hi)
			}
			ms := float64(v) / float64(time.Millisecond)
			sum += ms
			sq += ms * ms
		}
		mean := sum / n
		sdev := math.Sqrt(sq/n - mean*mean)
		// Five standard errors: deterministic for this seed, and loose
		// enough to survive a change of seed.
		if tol := 5*tt.sdev/math.Sqrt(n) + 1e-6; math.Abs(mean-tt.mean) > tol {
			t.Errorf("%s: mean %.3fms, want %.3fms +- %.3f", tt.name, mean, tt.mean, tol)
		}
		if math.Abs(sdev-tt.sdev) > 0.02*tt.sdev+1e-3 {
			t.Errorf("%s: stddev %.3fms, want %.3fms", tt.name, sdev, tt.sdev)
		}
		if got := tt.dist.Mean() / float64(time.Millisecond); math.Abs(got-tt.mean) > 1e-9 {
			t.Errorf("%s: Mean() = %v, want %v", tt.name, got, tt.mean)
		}
	}
}

func TestUniformCountIsInclusive(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 1))
	d := mock.Uniform(3, 5)
	seen := map[int]int{}
	for range 30_000 {
		seen[d.Sample(rng)]++
	}
	for v := 3; v <= 5; v++ {
		if seen[v] < 9_000 || seen[v] > 11_000 {
			t.Errorf("value %d drawn %d times, want about 10000 (counts %v)", v, seen[v], seen)
		}
	}
	if len(seen) != 3 {
		t.Errorf("drew values outside [3, 5]: %v", seen)
	}
}

func TestNormalIsClampedAtZero(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(2, 2))
	d := mock.Normal(time.Millisecond, 10*time.Millisecond)
	for range 10_000 {
		if v := d.Sample(rng); v < 0 {
			t.Fatalf("negative sample %v", v)
		}
	}
	var zero mock.Dist[int]
	if zero.Sample(rng) != 0 || zero.String() != "0" {
		t.Error("zero Dist does not yield zero")
	}
}
