package report_test

import (
	"bytes"
	"math"
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
		TTFT:              stats.Summary{P50: 50, P99: ttftP99},
		E2E:               stats.Summary{P50: 500, P99: 900},
	}
}

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
	c := report.Compare(base, head, 10)
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
	if got := report.Compare(base, head, 0).Regressions(); got != 0 {
		t.Errorf("threshold 0 flagged %d regressions, want none", got)
	}
	if got := report.Compare(base, head, 2).Regressions(); got < 3 {
		t.Errorf("threshold 2%% flagged %d regressions, want at least 3", got)
	}
}

func TestCompareErrorRateInPoints(t *testing.T) {
	t.Parallel()
	c := report.Compare(result(closedStage(1, 10, 100, 1)), result(closedStage(1, 10, 100, 12)), 5)
	if m := metric(t, &c.Stages[0], "Error rate (%)"); !m.Regression || math.Abs(m.Change()-11) > 1e-9 {
		t.Errorf("error rate 1%% -> 12%%: change %v pp, regression %v; want +11 pp flagged", m.Change(), m.Regression)
	}
}

func TestCompareZeroBase(t *testing.T) {
	t.Parallel()
	b := closedStage(1, 0, 0, 0)
	h := closedStage(1, 5, 0, 0)
	c := report.Compare(result(b), result(h), 10)
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
	c := report.Compare(base, head, 0)
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

func TestComparisonMarkdown(t *testing.T) {
	t.Parallel()
	base := result(closedStage(8, 50, 200, 0), closedStage(16, 60, 300, 0))
	head := result(closedStage(8, 40, 150, 3))
	var buf bytes.Buffer
	if err := report.Compare(base, head, 10).WriteMarkdown(&buf, "base.json", "head.json"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"- Base: `base.json`", "### concurrency=8", "| Req/s | 50.0 | 40.0 | **-20.0% regression** |",
		"| TTFT p99 (ms) | 200 | 150 | -25.0% |", "| Error rate (%) | 0.00 | 3.00 | +3.00 pp |",
		"Only in base: concurrency=16", "1 regression(s) beyond the threshold.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("comparison lacks %q:\n%s", want, out)
		}
	}
	buf.Reset()
	if err := report.Compare(base, result(), 0).WriteMarkdown(&buf, "a", "b"); err != nil || !strings.Contains(buf.String(), "No stage loads match") {
		t.Errorf("unmatched comparison: %v\n%s", err, buf.String())
	}
}
