package cli

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aozmekik/llm-loadtest/mock"
)

// intList is a comma-separated list of positive integers, e.g. "1,2,4,8".
type intList []int

func (l *intList) String() string { return joinValues(*l, strconv.Itoa) }

func (l *intList) Set(s string) error {
	var out []int
	for field := range strings.SplitSeq(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || n < 1 {
			return fmt.Errorf("%q is not a positive integer", field)
		}
		out = append(out, n)
	}
	*l = out
	return nil
}

// floatList is a comma-separated list of positive numbers, e.g. "0.5,1,2".
type floatList []float64

func (l *floatList) String() string {
	return joinValues(*l, func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) })
}

func (l *floatList) Set(s string) error {
	var out []float64
	for field := range strings.SplitSeq(s, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(field), 64)
		if err != nil || !(v > 0) || math.IsInf(v, 0) {
			return fmt.Errorf("%q is not a positive number", field)
		}
		out = append(out, v)
	}
	*l = out
	return nil
}

func joinValues[T any](values []T, format func(T) string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = format(v)
	}
	return strings.Join(parts, ",")
}

// stringList collects a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ", ") }

func (l *stringList) Set(s string) error {
	*l = append(*l, s)
	return nil
}

// headerList collects repeatable "Name: value" headers.
type headerList struct{ h http.Header }

func (l *headerList) String() string {
	if l.h == nil {
		return ""
	}
	return strings.Join(headerNames(l.h), ", ")
}

func (l *headerList) Set(s string) error {
	name, value, ok := strings.Cut(s, ":")
	name = strings.TrimSpace(name)
	if !ok || name == "" || strings.ContainsAny(name, " \t") {
		return errors.New(`want "Name: value"`)
	}
	if l.h == nil {
		l.h = http.Header{}
	}
	l.h.Add(name, strings.TrimSpace(value))
	return nil
}

// optionalFloat is a float flag that records whether it was set.
type optionalFloat struct{ v *float64 }

func (o *optionalFloat) String() string {
	if o.v == nil {
		return ""
	}
	return strconv.FormatFloat(*o.v, 'g', -1, 64)
}

func (o *optionalFloat) Set(s string) error {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return errors.New("not a number")
	}
	o.v = &v
	return nil
}

// distFlag is a latency or token-count distribution such as
// "normal:50ms,10ms".
type distFlag[T mock.Number] struct {
	d     *mock.Dist[T]
	parse func(string) (mock.Dist[T], error)
}

func (f distFlag[T]) String() string {
	if f.d == nil {
		return ""
	}
	return f.d.String()
}

func (f distFlag[T]) Set(s string) error {
	d, err := f.parse(s)
	if err != nil {
		return err
	}
	*f.d = d
	return nil
}

func durationDistVar(fs *flag.FlagSet, p *mock.Dist[time.Duration], name string, def mock.Dist[time.Duration], usage string) {
	*p = def
	fs.Var(distFlag[time.Duration]{d: p, parse: mock.ParseDurationDist}, name, usage)
}

func countDistVar(fs *flag.FlagSet, p *mock.Dist[int], name string, def mock.Dist[int], usage string) {
	*p = def
	fs.Var(distFlag[int]{d: p, parse: mock.ParseCountDist}, name, usage)
}

// parseInterleaved parses flags that may appear before, between or after
// positional arguments, which the flag package alone does not allow.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...), nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}
