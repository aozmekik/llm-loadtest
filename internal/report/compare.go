package report

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strings"
)

// Direction says which way a metric improves.
type Direction int

// Metric directions.
const (
	// LowerIsBetter applies to latencies and error rates.
	LowerIsBetter Direction = iota
	// HigherIsBetter applies to throughput.
	HigherIsBetter
)

// MetricDiff compares one metric between two stages.
type MetricDiff struct {
	// Name labels the metric, e.g. "TTFT p99 (ms)".
	Name string
	// Base and Head are the metric's values in the two results.
	Base, Head float64
	// Direction says which way the metric improves.
	Direction Direction
	// Points marks a metric compared in percentage points (error rate)
	// rather than relative change.
	Points bool
	// Regression reports that Head is worse than Base by more than the
	// threshold.
	Regression bool
}

// Change returns the relative change in percent, or for a Points metric
// the absolute change in percentage points. It is NaN when the relative
// change is undefined because Base is zero.
func (d *MetricDiff) Change() float64 {
	if d.Points {
		return (d.Head - d.Base) * 100
	}
	if d.Base == 0 {
		if d.Head == 0 {
			return 0
		}
		return math.NaN()
	}
	return (d.Head - d.Base) / math.Abs(d.Base) * 100
}

// worseBy returns how much worse Head is than Base, in the units of Change;
// negative values are improvements.
func (d *MetricDiff) worseBy() float64 {
	c := d.Change()
	if d.Direction == HigherIsBetter {
		return -c
	}
	return c
}

// StageDiff compares two stages run at the same load.
type StageDiff struct {
	// Load names the matched load, e.g. "concurrency=8".
	Load    string
	Metrics []MetricDiff
}

// Comparison is the result of Compare.
type Comparison struct {
	// Threshold is the regression threshold in percent (percentage points
	// for error rate); zero disables regression detection.
	Threshold float64
	// Stages holds the stages present in both results, in base order.
	Stages []StageDiff
	// OnlyBase and OnlyHead list loads present in one result only.
	OnlyBase, OnlyHead []string
}

// Compare matches stages of base and head by load and compares their key
// metrics. A metric is a regression when head is worse by more than
// threshold percent (percentage points for the error rate). A threshold of
// zero reports changes without flagging regressions.
func Compare(base, head *Result, threshold float64) *Comparison {
	c := &Comparison{Threshold: threshold}
	unmatched := map[string][]int{} // head stage indices by load, in order
	for i := range head.Stages {
		key := head.Stages[i].Load.String()
		unmatched[key] = append(unmatched[key], i)
	}
	matched := make([]bool, len(head.Stages))
	for i := range base.Stages {
		b := &base.Stages[i]
		key := b.Load.String()
		candidates := unmatched[key]
		if len(candidates) == 0 {
			c.OnlyBase = append(c.OnlyBase, key)
			continue
		}
		j := candidates[0]
		unmatched[key] = candidates[1:]
		matched[j] = true
		c.Stages = append(c.Stages, diffStages(key, b, &head.Stages[j], threshold))
	}
	for i := range head.Stages {
		if !matched[i] {
			c.OnlyHead = append(c.OnlyHead, head.Stages[i].Load.String())
		}
	}
	return c
}

func diffStages(load string, b, h *Stage, threshold float64) StageDiff {
	metrics := []MetricDiff{
		{Name: "Req/s", Base: b.RequestsPerSecond, Head: h.RequestsPerSecond, Direction: HigherIsBetter},
		{Name: "Output tok/s", Base: b.OutputTokensPerSecond, Head: h.OutputTokensPerSecond, Direction: HigherIsBetter},
		{Name: "TTFT p50 (ms)", Base: b.TTFT.P50, Head: h.TTFT.P50},
		{Name: "TTFT p99 (ms)", Base: b.TTFT.P99, Head: h.TTFT.P99},
		{Name: "ITL p50 (ms)", Base: b.ITL.P50, Head: h.ITL.P50},
		{Name: "ITL p99 (ms)", Base: b.ITL.P99, Head: h.ITL.P99},
		{Name: "TPOT p50 (ms)", Base: b.TPOT.P50, Head: h.TPOT.P50},
		{Name: "E2E p50 (ms)", Base: b.E2E.P50, Head: h.E2E.P50},
		{Name: "E2E p99 (ms)", Base: b.E2E.P99, Head: h.E2E.P99},
		{Name: "Error rate (%)", Base: b.ErrorRate(), Head: h.ErrorRate(), Points: true},
	}
	if threshold > 0 {
		for i := range metrics {
			m := &metrics[i]
			worse := m.worseBy()
			// An undefined relative change (zero base) cannot be judged.
			m.Regression = !math.IsNaN(worse) && worse > threshold
		}
	}
	return StageDiff{Load: load, Metrics: metrics}
}

// Regressions returns the number of regressed metrics across all stages.
func (c *Comparison) Regressions() int {
	n := 0
	for _, s := range c.Stages {
		for _, m := range s.Metrics {
			if m.Regression {
				n++
			}
		}
	}
	return n
}

// WriteMarkdown renders the comparison with one table per matched stage.
func (c *Comparison) WriteMarkdown(w io.Writer, baseName, headName string) error {
	bw := bufio.NewWriter(w)
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(bw, format, args...) }
	p("## llm-loadtest comparison\n\n- Base: `%s`\n- Head: `%s`\n", baseName, headName)
	if c.Threshold > 0 {
		p("- Regression threshold: %g%% (percentage points for error rate)\n", c.Threshold)
	}
	for _, s := range c.Stages {
		p("\n### %s\n\n| Metric | Base | Head | Change |\n|:--|--:|--:|--:|\n", s.Load)
		for i := range s.Metrics {
			m := &s.Metrics[i]
			base, head := num(m.Base), num(m.Head)
			if m.Points {
				base, head = num(m.Base*100), num(m.Head*100)
			}
			p("| %s | %s | %s | %s |\n", m.Name, base, head, formatChange(m))
		}
	}
	if len(c.OnlyBase) > 0 {
		p("\nOnly in base: %s\n", strings.Join(c.OnlyBase, ", "))
	}
	if len(c.OnlyHead) > 0 {
		p("\nOnly in head: %s\n", strings.Join(c.OnlyHead, ", "))
	}
	if len(c.Stages) == 0 {
		p("\nNo stage loads match between the two results.\n")
	}
	if c.Threshold > 0 {
		p("\n%d regression(s) beyond the threshold.\n", c.Regressions())
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("write comparison: %w", err)
	}
	return nil
}

func formatChange(m *MetricDiff) string {
	ch := m.Change()
	var s string
	switch {
	case math.IsNaN(ch):
		return "n/a"
	case m.Points:
		s = fmt.Sprintf("%+.2f pp", positiveZero(math.Round(ch*100)/100))
	default:
		s = fmt.Sprintf("%+.1f%%", positiveZero(math.Round(ch*10)/10))
	}
	if m.Regression {
		s = "**" + s + " regression**"
	}
	return s
}

// positiveZero maps -0 to 0 so that tiny decreases print as +0.0%.
func positiveZero(v float64) float64 {
	if v == 0 {
		return 0
	}
	return v
}
