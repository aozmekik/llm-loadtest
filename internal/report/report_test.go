package report_test

import (
	"bytes"
	"encoding/csv"
	"flag"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aozmekik/llm-loadtest/internal/client"
	"github.com/aozmekik/llm-loadtest/internal/loadgen"
	"github.com/aozmekik/llm-loadtest/internal/report"
	"github.com/aozmekik/llm-loadtest/internal/stats"
)

var update = flag.Bool("update", false, "rewrite golden files")

const ms = time.Millisecond

// sampleStage builds a stage with known measurements: three successes
// (two with server usage), one success without content, and two failures.
func sampleStage(load loadgen.Load) *loadgen.StageResult {
	itl := stats.NewSketch(loadgen.ITLAccuracy)
	for _, v := range []float64{10, 10, 12, 20} {
		itl.Add(v)
	}
	return &loadgen.StageResult{
		Stage:   loadgen.Stage{Load: load, Requests: 6},
		Elapsed: 2 * time.Second,
		ITL:     itl,
		Records: []loadgen.Record{
			{Seq: 0, TTFT: 100 * ms, E2E: 400 * ms, TPOT: 10 * ms, MaxITL: 12 * ms, OutputTokens: 31, PromptTokens: 50, UsageReported: true, Chunks: 31, FinishReason: "length"},
			{Seq: 1, Scheduled: 100 * ms, SendLag: 2 * ms, TTFT: 200 * ms, E2E: 600 * ms, TPOT: 20 * ms, MaxITL: 20 * ms, OutputTokens: 21, PromptTokens: 70, UsageReported: true, Chunks: 21, FinishReason: "stop"},
			{Seq: 2, Scheduled: 200 * ms, TTFT: 300 * ms, E2E: 500 * ms, OutputTokens: 1, Chunks: 1, FinishReason: "stop"},
			{Seq: 3, Scheduled: 300 * ms, E2E: 50 * ms, FinishReason: "stop"},
			{Seq: 4, Scheduled: 400 * ms, E2E: 5 * ms, Err: &client.Error{Class: "http_500", Message: "HTTP 500: boom"}},
			{Seq: 5, Scheduled: 500 * ms, E2E: 1 * ms, Err: &client.Error{Class: "http_500", Message: "HTTP 500: second"}},
		},
	}
}

func TestNewStage(t *testing.T) {
	t.Parallel()
	st := report.NewStage(sampleStage(loadgen.Load{Mode: loadgen.Closed, Concurrency: 2}))
	if st.Requests != 6 || st.Succeeded != 4 || st.Failed != 2 {
		t.Errorf("requests/succeeded/failed = %d/%d/%d, want 6/4/2", st.Requests, st.Succeeded, st.Failed)
	}
	if st.Errors["http_500"] != 2 || st.ErrorSamples["http_500"] != "HTTP 500: boom" {
		t.Errorf("errors = %v, samples = %v; want 2 x http_500 sampled from the first", st.Errors, st.ErrorSamples)
	}
	if st.TokenCounts != (report.TokenCounts{Exact: 2, Approximate: 2}) {
		t.Errorf("token counts = %+v", st.TokenCounts)
	}
	// 4 successes and 31+21+1+0 tokens over 2 seconds.
	if st.RequestsPerSecond != 2 || st.OutputTokensPerSecond != 26.5 {
		t.Errorf("throughput = %v req/s, %v tok/s; want 2 and 26.5", st.RequestsPerSecond, st.OutputTokensPerSecond)
	}
	// TTFT only where content arrived; TPOT only with two or more tokens.
	if st.TTFT.Count != 3 || st.TTFT.P50 != 200 || st.TPOT.Count != 2 || st.TPOT.Mean != 15 {
		t.Errorf("TTFT %+v, TPOT %+v", st.TTFT, st.TPOT)
	}
	if st.E2E.Count != 4 || st.E2E.Max != 600 || st.InputTokens.Count != 2 || st.InputTokens.Mean != 60 {
		t.Errorf("E2E %+v, input tokens %+v", st.E2E, st.InputTokens)
	}
	if st.SendLag.Count != 6 || st.SendLag.Max != 2 {
		t.Errorf("send lag %+v, want all six requests", st.SendLag)
	}
	if got := st.RequestTokensPerSecond.Max; got != 77.5 { // 31 tokens / 0.4 s
		t.Errorf("max per-request tok/s = %v, want 77.5", got)
	}
	if st.ITL.Count != 4 || st.ITL.Min != 10 || st.ITL.Max != 20 {
		t.Errorf("ITL %+v", st.ITL)
	}
	if math.Abs(st.ErrorRate()-1.0/3) > 1e-12 {
		t.Errorf("ErrorRate = %v, want 1/3", st.ErrorRate())
	}
	recs := st.Records
	if recs[0].TokenSource != report.SourceUsage || recs[2].TokenSource != report.SourceChunks {
		t.Errorf("token sources = %q, %q", recs[0].TokenSource, recs[2].TokenSource)
	}
	if recs[3].TTFTMs != nil || recs[2].TPOTMs != nil || recs[0].TPOTMs == nil || *recs[0].TTFTMs != 100 {
		t.Error("undefined TTFT/TPOT must be null and defined ones set")
	}
	if recs[4].Error != "http_500" || recs[4].ErrorMessage == "" {
		t.Errorf("failed record = %+v", recs[4])
	}
}

// TestCanceledAndTimedOut checks the two kinds of request the report keeps
// out of its distributions but must still account for.
func TestCanceledAndTimedOut(t *testing.T) {
	t.Parallel()
	res := &loadgen.StageResult{
		Stage:   loadgen.Stage{Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: 4}, Requests: 4},
		Elapsed: time.Second,
		ITL:     stats.NewSketch(loadgen.ITLAccuracy),
		Records: []loadgen.Record{
			{Seq: 0, TTFT: 100 * ms, E2E: 200 * ms, OutputTokens: 1, Chunks: 1, FinishReason: "stop"},
			{Seq: 1, E2E: time.Second, Err: &client.Error{Class: client.ClassTimeout, Message: "no complete response within 1s"}},
			{Seq: 2, E2E: 900 * ms, Err: &client.Error{Class: client.ClassCanceled, Message: "request canceled"}},
			{Seq: 3, E2E: 900 * ms, Err: &client.Error{Class: client.ClassCanceled, Message: "request canceled"}},
		},
	}
	st := report.NewStage(res)
	if st.Requests != 4 || st.Succeeded != 1 || st.Failed != 1 || st.Canceled != 2 || st.Errors["canceled"] != 0 {
		t.Errorf("requests/succeeded/failed/canceled = %d/%d/%d/%d, errors %v", st.Requests, st.Succeeded, st.Failed, st.Canceled, st.Errors)
	}
	if st.ErrorRate() != 0.5 || st.TimedOut() != 1 {
		t.Errorf("ErrorRate = %v, TimedOut = %d; want 0.5 (canceled left out) and 1", st.ErrorRate(), st.TimedOut())
	}
	var buf bytes.Buffer
	r := &report.Result{SchemaVersion: report.SchemaVersion, Interrupted: true, Stages: []report.Stage{st}}
	if err := report.WriteMarkdown(&buf, r); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"the 2 requests in flight were canceled", "Canceled by the interrupt: 2 requests",
		"The 1 timed-out requests are not in the distributions above",
		// One request with one token has no ITL gap and no TPOT.
		"| concurrency=4 | 1 | 1 | 1.00 | 1.00 | 100 | 100 | - | - | - | 200 | 200 |",
		"| ITL (ms) | - | - | - | - | - | - |",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("Markdown lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestErrorClassesOrder(t *testing.T) {
	t.Parallel()
	s := report.Stage{Errors: map[string]int{"timeout": 1, "http_500": 3, "connection": 1}}
	if got := s.ErrorClasses(); !reflect.DeepEqual(got, []string{"http_500", "connection", "timeout"}) {
		t.Errorf("ErrorClasses = %v", got)
	}
	if (&report.Stage{}).ErrorRate() != 0 {
		t.Error("ErrorRate of an empty stage is not 0")
	}
}

func sampleResult() *report.Result {
	temp := 0.0
	return &report.Result{
		SchemaVersion: report.SchemaVersion,
		Tool:          "llm-loadtest test",
		StartedAt:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Config: report.Config{
			BaseURL: "http://localhost:8000/v1", Model: "demo", Prompts: "synthetic (~256 input tokens)",
			MaxTokens: 128, Temperature: &temp, IncludeUsage: true, HeaderNames: []string{"X-Tenant"}, Seed: 1,
		},
		Stages: []report.Stage{
			report.NewStage(sampleStage(loadgen.Load{Mode: loadgen.Closed, Concurrency: 2})),
			report.NewStage(sampleStage(loadgen.Load{Mode: loadgen.Open, Rate: 4, Arrival: loadgen.Poisson})),
		},
	}
}

func TestJSONRoundTrip(t *testing.T) {
	t.Parallel()
	want := sampleResult()
	var buf bytes.Buffer
	if err := report.WriteJSON(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := report.ReadJSON(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the result:\n got  %+v\n want %+v", got, want)
	}
}

func TestReadJSONErrors(t *testing.T) {
	t.Parallel()
	for name, input := range map[string]string{
		"not JSON":       "{",
		"no schema":      `{"stages": []}`,
		"future schema":  `{"schema_version": 99}`,
		"wrong document": `[1, 2, 3]`,
	} {
		if _, err := report.ReadJSON(strings.NewReader(input)); err == nil {
			t.Errorf("%s: ReadJSON succeeded", name)
		}
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from golden file; run go test ./internal/report -update and review the diff\n--- got ---\n%s", name, got)
	}
}

func TestWriteMarkdownGolden(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := report.WriteMarkdown(&buf, sampleResult()); err != nil {
		t.Fatal(err)
	}
	golden(t, "result.md", buf.Bytes())
}

func TestWriteMarkdownNotes(t *testing.T) {
	t.Parallel()
	r := sampleResult()
	r.Interrupted = true
	r.Stages[0].TokenCounts = report.TokenCounts{Exact: 4}
	r.Stages[1].TokenCounts = report.TokenCounts{Exact: 4}
	var buf bytes.Buffer
	if err := report.WriteMarkdown(&buf, r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"**Interrupted**", "exact (server-reported usage) for all 8"} {
		if !strings.Contains(out, want) {
			t.Errorf("Markdown lacks %q:\n%s", want, out)
		}
	}
	buf.Reset()
	if err := report.WriteMarkdown(&buf, &report.Result{SchemaVersion: 1}); err != nil || !strings.Contains(buf.String(), "no successful requests") {
		t.Errorf("empty result rendering: %v\n%s", err, buf.String())
	}
}

func TestWriteCSV(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := report.WriteCSV(&buf, sampleResult()); err != nil {
		t.Fatal(err)
	}
	golden(t, "result.csv", buf.Bytes())
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1+12 {
		t.Fatalf("got %d rows, want a header and 12 records", len(rows))
	}
	if rows[0][5] != "ttft_ms" || rows[4][5] != "" || rows[1][5] != "100" {
		t.Errorf("ttft column: header %q, null %q, value %q", rows[0][5], rows[4][5], rows[1][5])
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func TestWritersReportErrors(t *testing.T) {
	t.Parallel()
	r := sampleResult()
	if report.WriteJSON(failingWriter{}, r) == nil {
		t.Error("WriteJSON ignored a write error")
	}
	if report.WriteMarkdown(failingWriter{}, r) == nil {
		t.Error("WriteMarkdown ignored a write error")
	}
	if report.WriteCSV(failingWriter{}, r) == nil {
		t.Error("WriteCSV ignored a write error")
	}
	if report.Compare(r, r, 0).WriteMarkdown(failingWriter{}, "a", "b") == nil {
		t.Error("Comparison.WriteMarkdown ignored a write error")
	}
}
