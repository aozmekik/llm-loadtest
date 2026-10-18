package report_test

import (
	"bytes"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/aozmekik/llm-loadtest/internal/loadgen"
	"github.com/aozmekik/llm-loadtest/internal/report"
	"github.com/aozmekik/llm-loadtest/internal/stats"
)

func closedStage(c int, rps, ttftP99 float64, failed int) report.Stage {
	return report.Stage{
		Load:              loadgen.Load{Mode: loadgen.Closed, Concurrency: c},
		Requests:          100,
		Succeeded:         100 - failed,
		Failed:            failed,
		RequestsPerSecond: rps,
		TTFT:              stats.Summary{Count: 100 - failed, P50: 50, P99: ttftP99},
		E2E:               stats.Summary{Count: 100 - failed, P50: 500, P99: 900},
	}
}

// failedStage is a stage in which every request failed.
func failedStage(c int) report.Stage {
	return report.Stage{
		Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: c}, Requests: 5, Failed: 5,
		Errors: map[string]int{"http_500": 5},
	}
}

func gate(pct float64) report.Thresholds { return report.Thresholds{Relative: pct, ErrorRate: pct} }

func result(stages ...report.Stage) *report.Result {
	return &report.Result{SchemaVersion: report.SchemaVersion, Stages: stages}
}

func metric(t *testing.T, s *report.StageDiff, name string) *report.MetricDiff {
	t.Helper()
	for i := range s.Metrics {
		if s.Metrics[i].Name == name {
			return &s.Metrics[i]
		}
	}
	t.Fatalf("metric %q not found", name)
	return nil
}

func TestCompareRegressions(t *testing.T) {
	t.Parallel()
	base := result(closedStage(1, 10, 100, 0), closedStage(8, 50, 200, 0))
	head := result(closedStage(1, 10.5, 109, 0), closedStage(8, 40, 150, 3))
	c := report.Compare(base, head, gate(10))
	if len(c.Stages) != 2 {
		t.Fatalf("matched %d stages, want 2", len(c.Stages))
	}
	c1, c8 := &c.Stages[0], &c.Stages[1]

	if m := metric(t, c1, "TTFT p99 (ms)"); m.Regression || math.Abs(m.Change()-9) > 1e-9 {
		t.Errorf("c=1 TTFT p99: change %v, regression %v; want +9%%, no regression", m.Change(), m.Regression)
	}
	if m := metric(t, c8, "Req/s"); !m.Regression || m.Change() != -20 {
		t.Errorf("c=8 Req/s: change %v, regression %v; want -20%% flagged", m.Change(), m.Regression)
	}
	if m := metric(t, c8, "TTFT p99 (ms)"); m.Regression || m.Change() != -25 {
		t.Errorf("c=8 TTFT p99 improved: change %v, regression %v", m.Change(), m.Regression)
	}
	// Error rate moves 0% -> 3%: 3 percentage points, under the 10 pp threshold.
	if m := metric(t, c8, "Error rate (%)"); m.Regression || math.Abs(m.Change()-3) > 1e-9 {
		t.Errorf("c=8 error rate: change %v pp, regression %v", m.Change(), m.Regression)
	}
	if got := c.Regressions(); got != 1 {
		t.Errorf("Regressions() = %d, want 1", got)
	}
	if c := report.Compare(base, head, report.Thresholds{}); c.Regressions() != 0 || c.Failures() != nil {
		t.Errorf("disabled thresholds flagged %d regressions and failures %v, want none", c.Regressions(), c.Failures())
	}
	if got := report.Compare(base, head, gate(2)).Regressions(); got < 3 {
		t.Errorf("threshold 2%% flagged %d regressions, want at least 3", got)
	}
	// The error rate has its own threshold, in percentage points.
	strict := report.Compare(base, head, report.Thresholds{Relative: 50, ErrorRate: 1})
	if m := metric(t, &strict.Stages[1], "Error rate (%)"); !m.Regression || strict.Regressions() != 1 {
		t.Errorf("error rate +3 pp with a 1 pp threshold: regression %v, %d regressions", m.Regression, strict.Regressions())
	}
}

func TestCompareErrorRateInPoints(t *testing.T) {
	t.Parallel()
	c := report.Compare(result(closedStage(1, 10, 100, 1)), result(closedStage(1, 10, 100, 12)), gate(5))
	if m := metric(t, &c.Stages[0], "Error rate (%)"); !m.Regression || math.Abs(m.Change()-11) > 1e-9 {
		t.Errorf("error rate 1%% -> 12%%: change %v pp, regression %v; want +11 pp flagged", m.Change(), m.Regression)
	}
}

func TestCompareZeroBase(t *testing.T) {
	t.Parallel()
	b := closedStage(1, 0, 0, 0)
	h := closedStage(1, 5, 0, 0)
	c := report.Compare(result(b), result(h), gate(10))
	if m := metric(t, &c.Stages[0], "Req/s"); !math.IsNaN(m.Change()) || m.Regression {
		t.Errorf("change from zero = %v (regression %v), want NaN and not judged", m.Change(), m.Regression)
	}
	if m := metric(t, &c.Stages[0], "TTFT p99 (ms)"); m.Change() != 0 {
		t.Errorf("zero to zero change = %v, want 0", m.Change())
	}
}

func TestCompareMatching(t *testing.T) {
	t.Parallel()
	open := report.Stage{Load: loadgen.Load{Mode: loadgen.Open, Rate: 2, Arrival: loadgen.Poisson}}
	base := result(closedStage(1, 10, 100, 0), closedStage(4, 10, 100, 0), closedStage(4, 20, 100, 0))
	head := result(closedStage(4, 11, 100, 0), open, closedStage(4, 22, 100, 0), closedStage(4, 30, 100, 0))
	c := report.Compare(base, head, report.Thresholds{})
	if len(c.Stages) != 2 || c.Stages[0].Load != "concurrency=4" {
		t.Fatalf("matched stages = %+v", c.Stages)
	}
	// Repeated loads are matched in order.
	if m := metric(t, &c.Stages[1], "Req/s"); m.Base != 20 || m.Head != 22 {
		t.Errorf("second concurrency=4 pair = %v -> %v, want 20 -> 22", m.Base, m.Head)
	}
	if strings.Join(c.OnlyBase, ",") != "concurrency=1" {
		t.Errorf("OnlyBase = %v", c.OnlyBase)
	}
	if strings.Join(c.OnlyHead, ",") != "rate=2/s poisson,concurrency=4" {
		t.Errorf("OnlyHead = %v", c.OnlyHead)
	}
}

// TestCompareGate checks that a comparison with thresholds fails whenever
// it does not cover the base run, not only when a metric regressed.
func TestCompareGate(t *testing.T) {
	t.Parallel()
	healthy := func() *report.Result { return result(closedStage(1, 10, 100, 0), closedStage(8, 50, 200, 0)) }
	interrupted := healthy()
	interrupted.Interrupted = true
	tests := []struct {
		name       string
		base, head *report.Result
		want       []string
	}{
		{"identical", healthy(), healthy(), nil},
		{"extra head stage", result(closedStage(1, 10, 100, 0)), healthy(), nil},
		{"missing stage", healthy(), result(closedStage(1, 10, 100, 0)), []string{"base stage concurrency=8 is missing from head"}},
		{"no match", healthy(), result(closedStage(99, 10, 100, 0)), []string{
			"no stage loads match", "base stage concurrency=1 is missing from head", "base stage concurrency=8 is missing from head",
		}},
		{"head interrupted", healthy(), interrupted, []string{"the head run was interrupted"}},
		{"base interrupted", interrupted, healthy(), []string{"the base run was interrupted"}},
		{"head failed entirely", result(closedStage(1, 10, 100, 0)), result(failedStage(1)), []string{
			"1 metric regressed beyond the thresholds", "head stage concurrency=1 has no successful request",
		}},
	}
	for _, tt := range tests {
		// A generous threshold: only the structural checks can fail.
		c := report.Compare(tt.base, tt.head, report.Thresholds{Relative: 1000, ErrorRate: 50})
		if got := c.Failures(); !slices.Equal(got, tt.want) {
			t.Errorf("%s: Failures() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestCompareEmptySummaries checks that a stage without successful requests
// shows its latencies as undefined rather than as a -100% improvement.
func TestCompareEmptySummaries(t *testing.T) {
	t.Parallel()
	c := report.Compare(result(closedStage(1, 10, 100, 0)), result(failedStage(1)), gate(10))
	s := &c.Stages[0]
	for _, name := range []string{"TTFT p50 (ms)", "TTFT p99 (ms)", "ITL p99 (ms)", "E2E p99 (ms)"} {
		if m := metric(t, s, name); !m.HeadEmpty || !math.IsNaN(m.Change()) || m.Regression {
			t.Errorf("%s: head empty %v, change %v, regression %v; want an undefined change", name, m.HeadEmpty, m.Change(), m.Regression)
		}
	}
	// Throughput and the error rate are defined and do regress.
	if m := metric(t, s, "Req/s"); !m.Regression || m.Change() != -100 {
		t.Errorf("Req/s: change %v, regression %v", m.Change(), m.Regression)
	}
	if m := metric(t, s, "Error rate (%)"); !m.Regression || m.Change() != 100 {
		t.Errorf("error rate: change %v, regression %v", m.Change(), m.Regression)
	}
	var buf bytes.Buffer
	if err := c.WriteMarkdown(&buf, "a", "b"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"| TTFT p50 (ms) | 50.0 | - | n/a |", "concurrency=1: head has no successful request"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("comparison lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestCompareNotes(t *testing.T) {
	t.Parallel()
	temp := 0.7
	base, head := result(closedStage(1, 10, 100, 0)), result(closedStage(1, 10, 100, 0))
	base.Config = report.Config{BaseURL: "http://gpu:8000/v1", Model: "m", Prompts: "synthetic (~256 input tokens)", MaxTokens: 256, Seed: 1, Duration: "30s"}
	head.Config = base.Config
	c := report.Compare(base, head, report.Thresholds{})
	if !c.SamePrompts || c.ConfigDiffs != nil {
		t.Errorf("same settings: SamePrompts %v, ConfigDiffs %q", c.SamePrompts, c.ConfigDiffs)
	}
	head.Config.Seed, head.Config.MaxTokens, head.Config.Duration, head.Config.Temperature = 2, 0, "10s", &temp
	c = report.Compare(base, head, report.Thresholds{})
	want := []string{"max_tokens: 256 -> -", "temperature: - -> 0.7", "stage_duration: 30s -> 10s"}
	if c.SamePrompts || !slices.Equal(c.ConfigDiffs, want) {
		t.Errorf("SamePrompts %v, ConfigDiffs %q; want false and %q", c.SamePrompts, c.ConfigDiffs, want)
	}
	if len(c.Failures()) != 0 {
		t.Error("differing settings alone must not fail a comparison")
	}
}

func TestComparisonMarkdown(t *testing.T) {
	t.Parallel()
	base := result(closedStage(8, 50, 200, 0), closedStage(16, 60, 300, 0))
	head := result(closedStage(8, 40, 150, 3))
	head.Interrupted = true
	var buf bytes.Buffer
	if err := report.Compare(base, head, gate(10)).WriteMarkdown(&buf, "base.json", "head.json"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"- Base: `base.json`", "- Regression thresholds: 10% for throughput and latency, 10 percentage points for the error rate",
		"### concurrency=8", "| Req/s | 50.0 | 40.0 | **-20.0% regression** |",
		"| TTFT p99 (ms) | 200 | 150 | -25.0% |", "| Error rate (%) | 0.00 | 3.00 | +3.00 pp |",
		"Only in base: concurrency=16", "- The head run was interrupted; its last stage is partial.",
		"**Result: fail**", "- 1 metric regressed beyond the thresholds", "- base stage concurrency=16 is missing from head",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("comparison lacks %q:\n%s", want, out)
		}
	}
	buf.Reset()
	if err := report.Compare(base, base, gate(10)).WriteMarkdown(&buf, "a", "b"); err != nil || !strings.Contains(buf.String(), "Result: pass") {
		t.Errorf("passing comparison: %v\n%s", err, buf.String())
	}
	buf.Reset()
	tiny := report.Compare(result(closedStage(1, 1000, 100, 0)), result(closedStage(1, 999.9, 100, 0)), report.Thresholds{})
	if err := tiny.WriteMarkdown(&buf, "a", "b"); err != nil || !strings.Contains(buf.String(), "| Req/s | 1000 | 1000 | +0.0% |") {
		t.Errorf("a -0.01%% change should print as +0.0%%:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "Result:") {
		t.Errorf("a comparison without thresholds gives no verdict:\n%s", buf.String())
	}
	buf.Reset()
	if err := report.Compare(base, result(), report.Thresholds{}).WriteMarkdown(&buf, "a", "b"); err != nil || !strings.Contains(buf.String(), "No stage loads match") {
		t.Errorf("unmatched comparison: %v\n%s", err, buf.String())
	}
}
