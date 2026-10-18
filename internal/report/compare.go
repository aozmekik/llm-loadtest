package report

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/aozmekik/llm-loadtest/internal/stats"
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
	// BaseEmpty and HeadEmpty report that the metric had no samples on that
	// side, such as the latencies of a stage without a successful request.
	// The value is then meaningless, and the change is undefined.
	BaseEmpty, HeadEmpty bool
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
// the absolute change in percentage points. It is NaN when the change is
// undefined: a side without samples, or a relative change from zero.
func (d *MetricDiff) Change() float64 {
	switch {
	case d.BaseEmpty || d.HeadEmpty:
		return math.NaN()
	case d.Points:
		return (d.Head - d.Base) * 100
	case d.Base == 0 && d.Head == 0:
		return 0
	case d.Base == 0:
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

// Thresholds configures regression detection.
type Thresholds struct {
	// Relative is how many percent worse a throughput or latency metric
	// may get before it counts as a regression. Zero disables regression
	// detection, and Failures then reports nothing.
	Relative float64
	// ErrorRate is how many percentage points the error rate may rise
	// before it counts as a regression. It applies only when Relative is
	// set.
	ErrorRate float64
}

// Enabled reports whether the thresholds detect regressions.
func (t Thresholds) Enabled() bool { return t.Relative > 0 }

// StageDiff compares two stages run at the same load.
type StageDiff struct {
	// Load names the matched load, e.g. "concurrency=8".
	Load string
	// BaseSucceeded and HeadSucceeded count the successful requests.
	BaseSucceeded, HeadSucceeded int
	// Metrics holds the compared metrics in display order.
	Metrics []MetricDiff
}

// Comparison is the result of Compare.
type Comparison struct {
	// Thresholds is the regression gate the comparison was made with.
	Thresholds Thresholds
	// Stages holds the stages present in both results, in base order.
	Stages []StageDiff
	// OnlyBase and OnlyHead list loads present in one result only.
	OnlyBase, OnlyHead []string
	// BaseInterrupted and HeadInterrupted report runs that were stopped
	// early, whose last stage is partial.
	BaseInterrupted, HeadInterrupted bool
	// ConfigDiffs lists the run settings that differ, such as
	// "max_tokens: 256 -> 128". They are reported but never fail the
	// comparison, since comparing two settings can be the point.
	ConfigDiffs []string
	// SamePrompts reports that both runs sent the same prompt sequence (same
	// source and seed) to the same base URL, so a server-side prefix cache
	// may have served head's prompts and flattered its time to first token.
	SamePrompts bool
}

// Compare matches stages of base and head by load and compares their key
// metrics. With thresholds enabled, a metric is a regression when head is
// worse than base by more than Thresholds.Relative percent, or for the
// error rate by more than Thresholds.ErrorRate percentage points.
func Compare(base, head *Result, t Thresholds) *Comparison {
	c := &Comparison{
		Thresholds:      t,
		BaseInterrupted: base.Interrupted,
		HeadInterrupted: head.Interrupted,
		ConfigDiffs:     configDiffs(&base.Config, &head.Config),
		SamePrompts: base.Config.BaseURL != "" && base.Config.BaseURL == head.Config.BaseURL &&
			base.Config.Prompts == head.Config.Prompts && base.Config.Seed == head.Config.Seed,
	}
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
		c.Stages = append(c.Stages, diffStages(key, b, &head.Stages[j], t))
	}
	for i := range head.Stages {
		if !matched[i] {
			c.OnlyHead = append(c.OnlyHead, head.Stages[i].Load.String())
		}
	}
	return c
}

func diffStages(load string, b, h *Stage, t Thresholds) StageDiff {
	summary := func(name string, bs, hs *stats.Summary, pick func(*stats.Summary) float64) MetricDiff {
		return MetricDiff{Name: name, Base: pick(bs), Head: pick(hs), BaseEmpty: bs.Count == 0, HeadEmpty: hs.Count == 0}
	}
	p50 := func(s *stats.Summary) float64 { return s.P50 }
	p99 := func(s *stats.Summary) float64 { return s.P99 }
	metrics := []MetricDiff{
		{Name: "Req/s", Base: b.RequestsPerSecond, Head: h.RequestsPerSecond, Direction: HigherIsBetter},
		{Name: "Output tok/s", Base: b.OutputTokensPerSecond, Head: h.OutputTokensPerSecond, Direction: HigherIsBetter},
		summary("TTFT p50 (ms)", &b.TTFT, &h.TTFT, p50),
		summary("TTFT p99 (ms)", &b.TTFT, &h.TTFT, p99),
		summary("ITL p50 (ms)", &b.ITL, &h.ITL, p50),
		summary("ITL p99 (ms)", &b.ITL, &h.ITL, p99),
		summary("TPOT p50 (ms)", &b.TPOT, &h.TPOT, p50),
		summary("E2E p50 (ms)", &b.E2E, &h.E2E, p50),
		summary("E2E p99 (ms)", &b.E2E, &h.E2E, p99),
		{
			Name: "Error rate (%)", Base: b.ErrorRate(), Head: h.ErrorRate(), Points: true,
			BaseEmpty: b.Succeeded+b.Failed == 0, HeadEmpty: h.Succeeded+h.Failed == 0,
		},
	}
	if t.Enabled() {
		for i := range metrics {
			m := &metrics[i]
			limit := t.Relative
			if m.Points {
				limit = t.ErrorRate
			}
			worse := m.worseBy()
			// An undefined change cannot be judged; a head stage without a
			// successful request fails the comparison on its own.
			m.Regression = !math.IsNaN(worse) && worse > limit
		}
	}
	return StageDiff{Load: load, BaseSucceeded: b.Succeeded, HeadSucceeded: h.Succeeded, Metrics: metrics}
}

// configDiffs lists the settings that shape results and differ between
// the two runs. The base URL and the seed are left out: comparing two
// servers is the common case, and runs against one server should use
// different seeds.
func configDiffs(b, h *Config) []string {
	var diffs []string
	add := func(name, bv, hv string) {
		if bv != hv {
			diffs = append(diffs, fmt.Sprintf("%s: %s -> %s", name, orDash(bv), orDash(hv)))
		}
	}
	itoa := func(n int) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	temp := func(v *float64) string {
		if v == nil {
			return ""
		}
		return strconv.FormatFloat(*v, 'g', -1, 64)
	}
	add("model", b.Model, h.Model)
	add("prompts", b.Prompts, h.Prompts)
	add("max_tokens", itoa(b.MaxTokens), itoa(h.MaxTokens))
	add("max_tokens_field", b.MaxTokensField, h.MaxTokensField)
	add("temperature", temp(b.Temperature), temp(h.Temperature))
	add("include_usage", strconv.FormatBool(b.IncludeUsage), strconv.FormatBool(h.IncludeUsage))
	add("extra_body_keys", strings.Join(b.ExtraBody, ","), strings.Join(h.ExtraBody, ","))
	add("stage_duration", b.Duration, h.Duration)
	add("stage_requests", itoa(b.Requests), itoa(h.Requests))
	add("warmup_requests", itoa(b.Warmup), itoa(h.Warmup))
	add("timeout", b.Timeout, h.Timeout)
	add("idle_timeout", b.IdleTimeout, h.IdleTimeout)
	add("max_in_flight", itoa(b.MaxInFlight), itoa(h.MaxInFlight))
	return diffs
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
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

// Failures lists the reasons the comparison fails its thresholds: regressed
// metrics, and anything that keeps it from covering the base run, namely
// base stages missing from head, no matched stage at all, a head stage
// without a successful request, or an interrupted run. It is empty when
// the thresholds are disabled.
func (c *Comparison) Failures() []string {
	if !c.Thresholds.Enabled() {
		return nil
	}
	var f []string
	if n := c.Regressions(); n > 0 {
		f = append(f, fmt.Sprintf("%d %s regressed beyond the thresholds", n, plural(n, "metric", "metrics")))
	}
	if len(c.Stages) == 0 {
		f = append(f, "no stage loads match")
	}
	for _, load := range c.OnlyBase {
		f = append(f, "base stage "+load+" is missing from head")
	}
	for i := range c.Stages {
		if c.Stages[i].HeadSucceeded == 0 {
			f = append(f, "head stage "+c.Stages[i].Load+" has no successful request")
		}
	}
	if c.BaseInterrupted {
		f = append(f, "the base run was interrupted")
	}
	if c.HeadInterrupted {
		f = append(f, "the head run was interrupted")
	}
	return f
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// WriteMarkdown renders the comparison with one table per matched stage,
// followed by notes and, with thresholds enabled, the verdict.
func (c *Comparison) WriteMarkdown(w io.Writer, baseName, headName string) error {
	bw := bufio.NewWriter(w)
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(bw, format, args...) }
	p("## llm-loadtest comparison\n\n- Base: `%s`\n- Head: `%s`\n", baseName, headName)
	if t := c.Thresholds; t.Enabled() {
		p("- Regression thresholds: %g%% for throughput and latency, %g percentage points for the error rate\n",
			t.Relative, t.ErrorRate)
	}
	for _, s := range c.Stages {
		p("\n### %s\n\n| Metric | Base | Head | Change |\n|:--|--:|--:|--:|\n", s.Load)
		for i := range s.Metrics {
			m := &s.Metrics[i]
			p("| %s | %s | %s | %s |\n", m.Name, m.value(m.Base, m.BaseEmpty), m.value(m.Head, m.HeadEmpty), formatChange(m))
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
	if notes := c.notes(); len(notes) > 0 {
		p("\nNotes:\n\n")
		for _, n := range notes {
			p("- %s\n", n)
		}
	}
	if c.Thresholds.Enabled() {
		if failures := c.Failures(); len(failures) > 0 {
			p("\n**Result: fail**\n\n")
			for _, f := range failures {
				p("- %s\n", f)
			}
		} else {
			p("\nResult: pass, no metric regressed beyond the thresholds.\n")
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("write comparison: %w", err)
	}
	return nil
}

// notes explains what can make the comparison misleading.
func (c *Comparison) notes() []string {
	var notes []string
	for _, side := range []struct {
		name        string
		interrupted bool
	}{{"base", c.BaseInterrupted}, {"head", c.HeadInterrupted}} {
		if side.interrupted {
			notes = append(notes, "The "+side.name+" run was interrupted; its last stage is partial.")
		}
	}
	for i := range c.Stages {
		switch s := &c.Stages[i]; {
		case s.HeadSucceeded == 0:
			notes = append(notes, s.Load+": head has no successful request, so its latencies cannot be compared.")
		case s.BaseSucceeded == 0:
			notes = append(notes, s.Load+": base has no successful request, so its latencies cannot be compared.")
		}
	}
	if len(c.ConfigDiffs) > 0 {
		notes = append(notes, "The runs used different settings ("+strings.Join(c.ConfigDiffs, "; ")+
			"). These do not fail the comparison.")
	}
	if c.SamePrompts {
		notes = append(notes, "Both runs sent the same prompts (same source and seed) to the same base URL. "+
			"If that server has a prefix cache, it may have served head's prompts and lowered its TTFT; "+
			"give each run its own --seed, or restart the server or reset its cache between runs.")
	}
	return notes
}

// value formats one side of a metric, or "-" when it had no samples.
func (d *MetricDiff) value(v float64, empty bool) string {
	switch {
	case empty:
		return "-"
	case d.Points:
		return num(v * 100)
	}
	return num(v)
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
