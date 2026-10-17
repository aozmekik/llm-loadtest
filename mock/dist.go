package mock

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// Number is the set of value types a Dist can produce: durations for
// latencies and integers for token counts.
type Number interface{ ~int | ~int64 }

type distKind uint8

const (
	kindConstant distKind = iota
	kindUniform
	kindNormal
	kindExponential
)

// Dist is a probability distribution over non-negative values of type T.
// The zero value always yields zero. Create one with Constant, Uniform,
// Normal or Exponential, or parse one with ParseDurationDist or
// ParseCountDist.
type Dist[T Number] struct {
	kind distKind
	a, b T
}

// Constant returns a distribution that always yields v.
func Constant[T Number](v T) Dist[T] { return Dist[T]{kind: kindConstant, a: v} }

// Uniform returns a distribution uniform on [lo, hi]. For integer counts
// every value in the range is equally likely.
func Uniform[T Number](lo, hi T) Dist[T] {
	return Dist[T]{kind: kindUniform, a: min(lo, hi), b: max(lo, hi)}
}

// Normal returns a normal distribution with the given mean and standard
// deviation. Samples below zero are clamped to zero, so keep the mean a few
// standard deviations above zero for an unbiased mean.
func Normal[T Number](mean, stddev T) Dist[T] {
	return Dist[T]{kind: kindNormal, a: mean, b: stddev}
}

// Exponential returns an exponential distribution with the given mean.
func Exponential[T Number](mean T) Dist[T] { return Dist[T]{kind: kindExponential, a: mean} }

// Sample draws one value from r. The result is never negative.
func (d Dist[T]) Sample(r *rand.Rand) T {
	var v float64
	switch d.kind {
	case kindConstant:
		return max(d.a, 0)
	case kindUniform:
		// Floor of a continuous draw on [lo, hi+1) is uniform over the
		// integers lo..hi (nanoseconds, for durations).
		v = math.Floor(float64(d.a) + r.Float64()*(float64(d.b-d.a)+1))
	case kindNormal:
		v = math.Round(float64(d.a) + r.NormFloat64()*float64(d.b))
	case kindExponential:
		v = math.Round(r.ExpFloat64() * float64(d.a))
	}
	return T(max(v, 0))
}

// Mean returns the distribution's mean, ignoring the clamping of negative
// normal samples.
func (d Dist[T]) Mean() float64 {
	switch d.kind {
	case kindUniform:
		return (float64(d.a) + float64(d.b)) / 2
	case kindConstant, kindNormal, kindExponential:
		return float64(d.a)
	}
	return 0
}

// String formats the distribution in the syntax accepted by the parse
// functions, for example "normal:50ms,10ms".
func (d Dist[T]) String() string {
	switch d.kind {
	case kindUniform:
		return fmt.Sprintf("uniform:%v,%v", d.a, d.b)
	case kindNormal:
		return fmt.Sprintf("normal:%v,%v", d.a, d.b)
	case kindExponential:
		return fmt.Sprintf("exp:%v", d.a)
	case kindConstant:
	}
	return fmt.Sprint(d.a)
}

// ParseDurationDist parses a latency distribution. Accepted forms, with
// values in time.ParseDuration syntax:
//
//	50ms                 constant
//	uniform:20ms,80ms    uniform between the two bounds
//	normal:50ms,10ms     normal with mean and standard deviation
//	exp:50ms             exponential with the given mean
func ParseDurationDist(s string) (Dist[time.Duration], error) {
	return parseDist(s, time.ParseDuration)
}

// ParseCountDist parses a token-count distribution in the same forms as
// ParseDurationDist, with integer values: "128", "uniform:64,256",
// "normal:128,32" or "exp:100".
func ParseCountDist(s string) (Dist[int], error) {
	return parseDist(s, strconv.Atoi)
}

var errDistSyntax = errors.New("want V, uniform:LO,HI, normal:MEAN,STDDEV or exp:MEAN")

func parseDist[T Number](s string, parse func(string) (T, error)) (Dist[T], error) {
	name, args, hasKind := strings.Cut(strings.TrimSpace(s), ":")
	if !hasKind {
		name, args = "const", name
	}
	var values []T
	for field := range strings.SplitSeq(args, ",") {
		v, err := parse(strings.TrimSpace(field))
		if err != nil {
			return Dist[T]{}, fmt.Errorf("distribution %q: %w", s, err)
		}
		if v < 0 {
			return Dist[T]{}, fmt.Errorf("distribution %q: values must not be negative", s)
		}
		values = append(values, v)
	}
	want := map[string]int{"const": 1, "uniform": 2, "normal": 2, "exp": 1}[name]
	if want == 0 || len(values) != want {
		return Dist[T]{}, fmt.Errorf("distribution %q: %w", s, errDistSyntax)
	}
	switch name {
	case "uniform":
		if values[0] > values[1] {
			return Dist[T]{}, fmt.Errorf("distribution %q: lower bound exceeds upper bound", s)
		}
		return Uniform(values[0], values[1]), nil
	case "normal":
		return Normal(values[0], values[1]), nil
	case "exp":
		return Exponential(values[0]), nil
	}
	return Constant(values[0]), nil
}
