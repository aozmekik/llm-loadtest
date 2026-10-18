package report

import (
	"bufio"
	"cmp"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/aozmekik/llm-loadtest/internal/loadgen"
	"github.com/aozmekik/llm-loadtest/internal/stats"
)

// WriteMarkdown renders the result as a summary table with one row per
// stage, followed by a percentile table for each stage.
func WriteMarkdown(w io.Writer, r *Result) error {
	bw := bufio.NewWriter(w)
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(bw, format, args...) }

	p("## llm-loadtest results\n\n")
	p("- Model: `%s` at `%s`\n", r.Config.Model, r.Config.BaseURL)
	p("- Prompts: %s", r.Config.Prompts)
	if r.Config.MaxTokens > 0 {
		p("; %s %d", cmp.Or(r.Config.MaxTokensField, "max_tokens"), r.Config.MaxTokens)
	}
	p("; seed %d", r.Config.Seed)
	p("\n- Token counts: %s\n", tokenCountNote(r.Stages))
	if r.Interrupted {
		p("- **Interrupted**: the run was stopped early; the last stage is partial")
		if n := canceled(r.Stages); n > 0 {
			p(", and the %d requests in flight were canceled and left out of the counts and distributions", n)
		}
		p(".\n")
	}

	p("\n| Load | OK | Errors | Req/s | Out tok/s | TTFT p50 | TTFT p99 | ITL p50 | ITL p99 | TPOT p50 | E2E p50 | E2E p99 |\n")
	p("|:--|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|\n")
	for i := range r.Stages {
		s := &r.Stages[i]
		p("| %s | %d | %d | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			s.Load, s.Succeeded, s.Failed, num(s.RequestsPerSecond), num(s.OutputTokensPerSecond),
			stat(&s.TTFT, s.TTFT.P50), stat(&s.TTFT, s.TTFT.P99), stat(&s.ITL, s.ITL.P50), stat(&s.ITL, s.ITL.P99),
			stat(&s.TPOT, s.TPOT.P50), stat(&s.E2E, s.E2E.P50), stat(&s.E2E, s.E2E.P99))
	}
	p("\nLatencies in ms. TTFT and E2E are measured from each request's scheduled start; " +
		"ITL percentiles are sketch estimates within 1%%.\n")

	for i := range r.Stages {
		s := &r.Stages[i]
		p("\n### %s: %d requests in %.1f s\n\n", s.Load, s.Requests, s.DurationSeconds)
		p("| Metric | Mean | p50 | p90 | p95 | p99 | Max |\n")
		p("|:--|--:|--:|--:|--:|--:|--:|\n")
		row := func(name string, sm *stats.Summary) {
			if sm.Count == 0 {
				p("| %s | - | - | - | - | - | - |\n", name)
				return
			}
			p("| %s | %s | %s | %s | %s | %s | %s |\n", name,
				num(sm.Mean), num(sm.P50), num(sm.P90), num(sm.P95), num(sm.P99), num(sm.Max))
		}
		row("TTFT (ms)", &s.TTFT)
		row("ITL (ms)", &s.ITL)
		row("TPOT (ms)", &s.TPOT)
		row("E2E (ms)", &s.E2E)
		row("Output tokens", &s.OutputTokens)
		if s.InputTokens.Count > 0 {
			row("Input tokens", &s.InputTokens)
		}
		row("Output tok/s per request", &s.RequestTokensPerSecond)
		if s.Load.Mode == loadgen.Open {
			row("Send lag (ms)", &s.SendLag)
		}
		if s.Failed > 0 {
			parts := make([]string, 0, len(s.Errors))
			for _, class := range s.ErrorClasses() {
				parts = append(parts, fmt.Sprintf("%d x %s (%q)", s.Errors[class], class, s.ErrorSamples[class]))
			}
			p("\nErrors: %s\n", strings.Join(parts, ", "))
		}
		if n := s.TimedOut(); n > 0 {
			p("\nThe %d timed-out requests are not in the distributions above, so they understate the tail: "+
				"those requests took at least as long as the timeout.\n", n)
		}
		if s.Canceled > 0 {
			p("\nCanceled by the interrupt: %d requests in flight, not counted as errors.\n", s.Canceled)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("write Markdown: %w", err)
	}
	return nil
}

// stat formats one statistic of sm, or "-" when sm has no samples.
func stat(sm *stats.Summary, v float64) string {
	if sm.Count == 0 {
		return "-"
	}
	return num(v)
}

func canceled(stages []Stage) int {
	n := 0
	for i := range stages {
		n += stages[i].Canceled
	}
	return n
}

// tokenCountNote says how output tokens were counted across all stages.
func tokenCountNote(stages []Stage) string {
	var exact, approx int
	for i := range stages {
		exact += stages[i].TokenCounts.Exact
		approx += stages[i].TokenCounts.Approximate
	}
	total := exact + approx
	switch {
	case total == 0:
		return "no successful requests"
	case approx == 0:
		return fmt.Sprintf("exact (server-reported usage) for all %d successful requests", total)
	default:
		return fmt.Sprintf("approximate for %d of %d successful requests: the server sent no usage, "+
			"so tokens were estimated by counting content chunks (a chunk can hold several tokens)", approx, total)
	}
}

// num formats a value with precision suited to its magnitude.
func num(v float64) string {
	switch a := math.Abs(v); {
	case a >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case a >= 10:
		return strconv.FormatFloat(v, 'f', 1, 64)
	default:
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
}

var csvHeader = []string{
	"stage", "load", "seq", "scheduled_ms", "send_lag_ms", "ttft_ms", "e2e_ms", "tpot_ms", "max_itl_ms",
	"output_tokens", "input_tokens", "token_source", "chunks", "finish_reason", "error", "error_message",
}

// WriteCSV writes one row per request across all stages.
func WriteCSV(w io.Writer, r *Result) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return fmt.Errorf("write CSV: %w", err)
	}
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	opt := func(v *float64) string {
		if v == nil {
			return ""
		}
		return f(*v)
	}
	for i := range r.Stages {
		s := &r.Stages[i]
		for j := range s.Records {
			rec := &s.Records[j]
			row := []string{
				strconv.Itoa(i), s.Load.String(), strconv.FormatUint(rec.Seq, 10),
				f(rec.ScheduledMs), f(rec.SendLagMs), opt(rec.TTFTMs), f(rec.E2EMs), opt(rec.TPOTMs), f(rec.MaxITLMs),
				strconv.Itoa(rec.OutputTokens), strconv.Itoa(rec.InputTokens), rec.TokenSource,
				strconv.Itoa(rec.Chunks), rec.FinishReason, rec.Error, rec.ErrorMessage,
			}
			if err := cw.Write(row); err != nil {
				return fmt.Errorf("write CSV: %w", err)
			}
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("write CSV: %w", err)
	}
	return nil
}
