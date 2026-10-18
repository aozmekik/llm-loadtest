// Package report turns stage measurements into results and renders them as
// JSON, per-request CSV and Markdown, and compares two results.
package report

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/aozmekik/llm-loadtest/internal/client"
	"github.com/aozmekik/llm-loadtest/internal/loadgen"
	"github.com/aozmekik/llm-loadtest/internal/stats"
)

// SchemaVersion is the version of the JSON result format. It changes when
// a field is removed or its meaning changes.
const SchemaVersion = 1

// Result is the outcome of one run: its configuration and one entry per
// stage.
type Result struct {
	SchemaVersion int       `json:"schema_version"`
	Tool          string    `json:"tool"`
	StartedAt     time.Time `json:"started_at"`
	Interrupted   bool      `json:"interrupted,omitempty"`
	Config        Config    `json:"config"`
	Stages        []Stage   `json:"stages"`
}

// Config records the run settings that shape the results. It deliberately
// holds no secrets: the API key is never stored, the base URL is redacted
// and only header names and extra body keys are kept.
type Config struct {
	// BaseURL is the API root, with credentials and query values redacted.
	BaseURL string `json:"base_url"`
	// Model is the requested model name.
	Model string `json:"model"`
	// Prompts describes the prompt source: synthetic prompts and their
	// length, or a dataset file and its size.
	Prompts string `json:"prompts"`
	// MaxTokens is the token limit of each request; zero if omitted.
	MaxTokens int `json:"max_tokens,omitempty"`
	// MaxTokensField is the request field that carried MaxTokens, when it
	// was not "max_tokens".
	MaxTokensField string `json:"max_tokens_field,omitempty"`
	// Temperature is the sampling temperature, or nil for the server's.
	Temperature *float64 `json:"temperature,omitempty"`
	// IncludeUsage reports whether stream_options.include_usage was sent.
	IncludeUsage bool `json:"include_usage"`
	// HeaderNames lists the extra request headers, without their values.
	HeaderNames []string `json:"header_names,omitempty"`
	// ExtraBody lists the keys merged into the request body.
	ExtraBody []string `json:"extra_body_keys,omitempty"`
	// Duration is each stage's issuing period, e.g. "30s".
	Duration string `json:"stage_duration,omitempty"`
	// Requests is each stage's request limit.
	Requests int `json:"stage_requests,omitempty"`
	// Warmup is the number of warmup requests, which are not in Stages.
	Warmup int `json:"warmup_requests,omitempty"`
	// Timeout and IdleTimeout are the per-request timeouts.
	Timeout     string `json:"timeout,omitempty"`
	IdleTimeout string `json:"idle_timeout,omitempty"`
	// MaxInFlight is the open-loop concurrency cap.
	MaxInFlight int `json:"max_in_flight,omitempty"`
	// Seed seeded the synthetic prompts, dataset shuffling and Poisson
	// arrivals.
	Seed uint64 `json:"seed"`
}

// TokenCounts tells how output token counts were obtained.
type TokenCounts struct {
	// Exact counts successful requests whose server reported usage.
	Exact int `json:"exact"`
	// Approximate counts successful requests whose tokens were estimated
	// by counting content chunks.
	Approximate int `json:"approximate"`
}

// Stage is the aggregated result of one stage. Latencies are in
// milliseconds and, except ITL and TPOT, measured from each request's
// scheduled start. Distributions cover successful requests only; SendLag
// covers all requests. A summary with Count zero is empty, and its other
// fields are zero rather than measurements.
type Stage struct {
	Load            loadgen.Load `json:"load"`
	DurationSeconds float64      `json:"duration_s"`
	// Requests counts every request issued: Succeeded + Failed + Canceled.
	Requests  int `json:"requests"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	// Canceled counts requests that were in flight when the run was
	// interrupted. They are neither successes nor errors.
	Canceled int `json:"canceled,omitempty"`
	// Errors counts failed requests by class, and ErrorSamples holds the
	// first message of each class.
	Errors       map[string]int    `json:"errors,omitempty"`
	ErrorSamples map[string]string `json:"error_samples,omitempty"`
	TokenCounts  TokenCounts       `json:"token_counts"`
	// RequestsPerSecond is successful requests per second of stage time.
	RequestsPerSecond float64 `json:"requests_per_s"`
	// OutputTokensPerSecond is the output tokens of successful requests
	// per second of stage time.
	OutputTokensPerSecond float64       `json:"output_tokens_per_s"`
	TTFT                  stats.Summary `json:"ttft_ms"`
	ITL                   stats.Summary `json:"itl_ms"`
	TPOT                  stats.Summary `json:"tpot_ms"`
	E2E                   stats.Summary `json:"e2e_ms"`
	OutputTokens          stats.Summary `json:"output_tokens"`
	InputTokens           stats.Summary `json:"input_tokens"`
	// RequestTokensPerSecond is each request's output tokens divided by
	// its end-to-end latency.
	RequestTokensPerSecond stats.Summary `json:"request_output_tokens_per_s"`
	SendLag                stats.Summary `json:"send_lag_ms"`
	Records                []Record      `json:"records,omitempty"`
}

// Record is one request. Pointer fields are null when undefined: TTFT
// without content, TPOT with fewer than two output tokens.
type Record struct {
	Seq          uint64   `json:"seq"`
	ScheduledMs  float64  `json:"scheduled_ms"`
	SendLagMs    float64  `json:"send_lag_ms"`
	TTFTMs       *float64 `json:"ttft_ms"`
	E2EMs        float64  `json:"e2e_ms"`
	TPOTMs       *float64 `json:"tpot_ms"`
	MaxITLMs     float64  `json:"max_itl_ms"`
	OutputTokens int      `json:"output_tokens"`
	InputTokens  int      `json:"input_tokens,omitempty"`
	TokenSource  string   `json:"token_source"`
	Chunks       int      `json:"chunks"`
	FinishReason string   `json:"finish_reason,omitempty"`
	Error        string   `json:"error,omitempty"`
	ErrorMessage string   `json:"error_message,omitempty"`
}

// Token count sources recorded per request.
const (
	SourceUsage  = "usage"
	SourceChunks = "chunks"
)

// NewStage aggregates a stage's measurements.
func NewStage(res *loadgen.StageResult) Stage {
	st := Stage{
		Load:            res.Stage.Load,
		DurationSeconds: round(res.Elapsed.Seconds()),
		Requests:        len(res.Records),
		ITL:             roundSummary(res.ITL.Summary()),
	}
	var ttft, tpot, e2e, outTok, inTok, reqTPS, lag []float64
	totalTokens := 0
	for i := range res.Records {
		rec := &res.Records[i]
		st.Records = append(st.Records, newRecord(rec))
		lag = append(lag, ms(rec.SendLag))
		if rec.Err != nil && rec.Err.Class == client.ClassCanceled {
			st.Canceled++
			continue
		}
		if rec.Err != nil {
			st.Failed++
			class := string(rec.Err.Class)
			if st.Errors == nil {
				st.Errors, st.ErrorSamples = map[string]int{}, map[string]string{}
			}
			if st.Errors[class] == 0 {
				st.ErrorSamples[class] = rec.Err.Message
			}
			st.Errors[class]++
			continue
		}
		st.Succeeded++
		if rec.UsageReported {
			st.TokenCounts.Exact++
			inTok = append(inTok, float64(rec.PromptTokens))
		} else {
			st.TokenCounts.Approximate++
		}
		totalTokens += rec.OutputTokens
		outTok = append(outTok, float64(rec.OutputTokens))
		e2e = append(e2e, ms(rec.E2E))
		if rec.Chunks > 0 {
			ttft = append(ttft, ms(rec.TTFT))
		}
		if rec.OutputTokens > 1 && rec.Chunks > 0 {
			tpot = append(tpot, ms(rec.TPOT))
		}
		if rec.E2E > 0 {
			reqTPS = append(reqTPS, float64(rec.OutputTokens)/rec.E2E.Seconds())
		}
	}
	if secs := res.Elapsed.Seconds(); secs > 0 {
		st.RequestsPerSecond = round(float64(st.Succeeded) / secs)
		st.OutputTokensPerSecond = round(float64(totalTokens) / secs)
	}
	st.TTFT = summarize(ttft)
	st.TPOT = summarize(tpot)
	st.E2E = summarize(e2e)
	st.OutputTokens = summarize(outTok)
	st.InputTokens = summarize(inTok)
	st.RequestTokensPerSecond = summarize(reqTPS)
	st.SendLag = summarize(lag)
	return st
}

func newRecord(rec *loadgen.Record) Record {
	out := Record{
		Seq:          rec.Seq,
		ScheduledMs:  round(ms(rec.Scheduled)),
		SendLagMs:    round(ms(rec.SendLag)),
		E2EMs:        round(ms(rec.E2E)),
		MaxITLMs:     round(ms(rec.MaxITL)),
		OutputTokens: rec.OutputTokens,
		InputTokens:  rec.PromptTokens,
		TokenSource:  SourceChunks,
		Chunks:       rec.Chunks,
		FinishReason: rec.FinishReason,
	}
	if rec.UsageReported {
		out.TokenSource = SourceUsage
	}
	if rec.Chunks > 0 {
		v := round(ms(rec.TTFT))
		out.TTFTMs = &v
		if rec.OutputTokens > 1 {
			t := round(ms(rec.TPOT))
			out.TPOTMs = &t
		}
	}
	if rec.Err != nil {
		out.Error = string(rec.Err.Class)
		out.ErrorMessage = rec.Err.Message
	}
	return out
}

// ErrorRate returns the failed fraction of the stage's finished requests,
// leaving out those canceled by an interrupt.
func (s *Stage) ErrorRate() float64 {
	finished := s.Succeeded + s.Failed
	if finished == 0 {
		return 0
	}
	return float64(s.Failed) / float64(finished)
}

// TimedOut returns the number of requests that failed with a timeout or an
// idle timeout. They are left out of the latency distributions, which
// therefore understate the tail.
func (s *Stage) TimedOut() int {
	return s.Errors[string(client.ClassTimeout)] + s.Errors[string(client.ClassIdleTimeout)]
}

// ErrorClasses returns the stage's error classes, most frequent first.
func (s *Stage) ErrorClasses() []string {
	classes := slices.Collect(maps.Keys(s.Errors))
	slices.SortFunc(classes, func(a, b string) int {
		return cmp.Or(cmp.Compare(s.Errors[b], s.Errors[a]), cmp.Compare(a, b))
	})
	return classes
}

// WriteJSON writes the result as indented JSON.
func WriteJSON(w io.Writer, r *Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}
	return nil
}

// ReadJSON reads a result written by WriteJSON.
func ReadJSON(rd io.Reader) (*Result, error) {
	var r Result
	if err := json.NewDecoder(rd).Decode(&r); err != nil {
		return nil, fmt.Errorf("read result: %w", err)
	}
	if r.SchemaVersion != SchemaVersion {
		if r.SchemaVersion == 0 {
			return nil, errors.New("read result: not an llm-loadtest result (no schema_version)")
		}
		return nil, fmt.Errorf("read result: schema version %d, this build reads %d", r.SchemaVersion, SchemaVersion)
	}
	return &r, nil
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// round keeps three decimals: microseconds for millisecond values.
func round(v float64) float64 { return math.Round(v*1000) / 1000 }

func summarize(values []float64) stats.Summary { return roundSummary(stats.Summarize(values)) }

func roundSummary(s stats.Summary) stats.Summary {
	s.Mean, s.Stddev, s.Min, s.Max = round(s.Mean), round(s.Stddev), round(s.Min), round(s.Max)
	s.P50, s.P90, s.P95, s.P99 = round(s.P50), round(s.P90), round(s.P95), round(s.P99)
	return s
}
