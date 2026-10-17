// Package loadgen drives a streaming client with closed-loop or open-loop
// load and records one measurement per request.
//
// In closed-loop mode a fixed number of workers each send their next
// request as soon as the previous one finishes. In open-loop mode requests
// are sent at scheduled arrival times, independent of how many are still
// in flight. To avoid coordinated omission, every latency is measured from
// the request's scheduled start, not from when it was actually sent; see
// docs/adr/0001-open-loop-latency-from-scheduled-time.md.
package loadgen

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aozmekik/llm-loadtest/internal/client"
	"github.com/aozmekik/llm-loadtest/internal/stats"
)

// ITLAccuracy is the relative accuracy of the inter-token latency sketch.
const ITLAccuracy = 0.01

// defaultProgressInterval is used when Config.ProgressInterval is zero.
const defaultProgressInterval = 500 * time.Millisecond

// Mode selects how requests are issued.
type Mode string

// Load modes.
const (
	// Closed keeps Load.Concurrency requests in flight.
	Closed Mode = "closed"
	// Open sends requests at Load.Rate per second.
	Open Mode = "open"
)

// Arrival is the inter-arrival distribution of an open-loop load.
type Arrival string

// Arrival processes.
const (
	// Poisson spaces requests by exponentially distributed gaps, the
	// standard model of independent users.
	Poisson Arrival = "poisson"
	// Constant spaces requests exactly 1/Rate apart.
	Constant Arrival = "constant"
)

// Load is the offered load of one stage.
type Load struct {
	// Mode selects closed-loop or open-loop load.
	Mode Mode `json:"mode"`
	// Concurrency is the number of closed-loop workers.
	Concurrency int `json:"concurrency,omitempty"`
	// Rate is the open-loop arrival rate in requests per second.
	Rate float64 `json:"rate,omitempty"`
	// Arrival is the open-loop arrival process.
	Arrival Arrival `json:"arrival,omitempty"`
}

// String formats the load for tables, e.g. "concurrency=8" or
// "rate=2/s poisson".
func (l Load) String() string {
	if l.Mode == Open {
		return fmt.Sprintf("rate=%g/s %s", l.Rate, l.Arrival)
	}
	return fmt.Sprintf("concurrency=%d", l.Concurrency)
}

// Validate reports whether the load is well formed.
func (l Load) Validate() error {
	switch l.Mode {
	case Closed:
		if l.Concurrency < 1 {
			return errors.New("closed-loop concurrency must be at least 1")
		}
	case Open:
		if !(l.Rate > 0) || math.IsInf(l.Rate, 0) {
			return errors.New("open-loop rate must be a positive number of requests per second")
		}
		if l.Arrival != Poisson && l.Arrival != Constant {
			return fmt.Errorf("unknown arrival process %q: want %q or %q", l.Arrival, Poisson, Constant)
		}
	default:
		return fmt.Errorf("unknown load mode %q", l.Mode)
	}
	return nil
}

// Stage is one measured period at a fixed load. It stops issuing requests
// when Duration has elapsed or Requests have been issued, whichever comes
// first, then waits for requests in flight to finish.
type Stage struct {
	// Load is the offered load.
	Load Load
	// Duration limits the issuing period; zero means no limit.
	Duration time.Duration
	// Requests limits the number of requests issued; zero means no limit.
	Requests int
}

func (s *Stage) validate() error {
	if err := s.Load.Validate(); err != nil {
		return err
	}
	if s.Duration < 0 || s.Requests < 0 {
		return errors.New("stage duration and request count must not be negative")
	}
	if s.Duration == 0 && s.Requests == 0 {
		return errors.New("stage needs a duration or a request count")
	}
	return nil
}

// Streamer sends one streaming request. *client.Client implements it.
// Errors should be *client.Error; others are recorded with class "other".
type Streamer interface {
	Stream(ctx context.Context, req client.Request) (client.Response, error)
}

// Prompts supplies the request for each sequence number. It must be safe
// for concurrent use.
type Prompts interface {
	Prompt(seq uint64) client.Request
}

// Progress is a snapshot of a running stage.
type Progress struct {
	// Load is the stage's load.
	Load Load
	// Elapsed is the time since the stage started.
	Elapsed time.Duration
	// Completed counts finished requests, successful or not.
	Completed int
	// Failed counts finished requests that failed.
	Failed int
	// InFlight counts requests sent but not finished.
	InFlight int
	// OutputTokens sums the output tokens of successful requests.
	OutputTokens int
}

// Config configures a Runner.
type Config struct {
	// Streamer sends the requests.
	Streamer Streamer
	// Prompts supplies them.
	Prompts Prompts
	// MaxInFlight caps concurrent open-loop requests; zero means no cap.
	// When the cap is reached, sending waits, and the wait is included in
	// latencies because they are measured from the scheduled start.
	MaxInFlight int
	// Seed seeds the Poisson arrival process.
	Seed uint64
	// OnProgress, if set, is called periodically during a stage and once
	// at its end, from a single goroutine.
	OnProgress func(Progress)
	// ProgressInterval is the period of OnProgress calls; default 500ms.
	ProgressInterval time.Duration
}

// Record is the measurement of one request. TTFT, E2E and SendLag are
// measured from the request's scheduled start.
type Record struct {
	// Seq is the sequence number passed to Prompts.
	Seq uint64
	// Scheduled is the scheduled start, relative to the stage start.
	Scheduled time.Duration
	// SendLag is how long after its scheduled start the request was sent.
	// It is near zero unless the generator fell behind or hit MaxInFlight.
	SendLag time.Duration
	// TTFT is the time to the first content token. It is meaningful only
	// when Chunks > 0.
	TTFT time.Duration
	// E2E is the time to the end of the stream (or to the failure).
	E2E time.Duration
	// TPOT is the time per output token after the first:
	// (end - first token) / (OutputTokens - 1). Zero below two tokens.
	TPOT time.Duration
	// MaxITL is the longest gap between consecutive content chunks.
	MaxITL time.Duration
	// OutputTokens is exact when UsageReported, else the chunk count.
	OutputTokens int
	// PromptTokens is the server-reported prompt length, or zero.
	PromptTokens int
	// UsageReported reports whether token counts came from the server.
	UsageReported bool
	// Chunks counts chunks that carried content.
	Chunks int
	// FinishReason is the server's finish_reason.
	FinishReason string
	// Err is nil for a successful request.
	Err *client.Error
}

// StageResult holds the measurements of one stage.
type StageResult struct {
	// Stage is the stage that was run.
	Stage Stage
	// Started is the stage's start time.
	Started time.Time
	// Elapsed runs from the start until the last request finished.
	Elapsed time.Duration
	// Records holds one entry per request, in completion order.
	Records []Record
	// ITL holds the gaps between consecutive content chunks of successful
	// requests, in milliseconds.
	ITL *stats.Sketch
	// Interrupted reports that the context was canceled mid-stage.
	Interrupted bool
}

// Runner runs stages. Sequence numbers continue across stages, so prompts
// are not repeated between stages unless the prompt source cycles.
type Runner struct {
	cfg    Config
	seq    atomic.Uint64
	stages atomic.Uint64
}

// New returns a Runner for cfg.
func New(cfg Config) (*Runner, error) {
	if cfg.Streamer == nil || cfg.Prompts == nil {
		return nil, errors.New("loadgen: Streamer and Prompts are required")
	}
	if cfg.MaxInFlight < 0 {
		return nil, errors.New("loadgen: MaxInFlight must not be negative")
	}
	if cfg.ProgressInterval <= 0 {
		cfg.ProgressInterval = defaultProgressInterval
	}
	return &Runner{cfg: cfg}, nil
}

// outcome is a finished request on its way to the collector.
type outcome struct {
	rec  Record
	gaps []time.Duration
}

// counters are shared between workers, the collector and the progress
// reporter.
type counters struct {
	inFlight, completed, failed, tokens atomic.Int64
}

// Run runs one stage and returns its measurements. It returns an error only
// for an invalid stage; failed requests are recorded, not returned. If ctx
// is canceled, Run stops issuing requests, cancels those in flight and
// returns the partial result with Interrupted set.
func (r *Runner) Run(ctx context.Context, st Stage) (*StageResult, error) {
	if err := st.validate(); err != nil {
		return nil, err
	}
	stageNo := r.stages.Add(1)
	res := &StageResult{Stage: st, ITL: stats.NewSketch(ITLAccuracy)}
	var c counters

	results := make(chan outcome, 256)
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for o := range results {
			res.Records = append(res.Records, o.rec)
			if o.rec.Err == nil {
				for _, g := range o.gaps {
					res.ITL.Add(float64(g) / float64(time.Millisecond))
				}
				c.tokens.Add(int64(o.rec.OutputTokens))
			} else {
				c.failed.Add(1)
			}
			c.completed.Add(1)
		}
	}()

	start := time.Now()
	res.Started = start
	stopProgress := r.reportProgress(st.Load, start, &c)

	issue := func(scheduled time.Time) {
		seq := r.seq.Add(1) - 1
		c.inFlight.Add(1)
		resp, err := r.cfg.Streamer.Stream(ctx, r.cfg.Prompts.Prompt(seq))
		c.inFlight.Add(-1)
		results <- newOutcome(seq, start, scheduled, &resp, err)
	}
	var deadline time.Time
	if st.Duration > 0 {
		deadline = start.Add(st.Duration)
	}
	if st.Load.Mode == Closed {
		runClosed(ctx, &st, deadline, issue)
	} else {
		r.runOpen(ctx, &st, start, deadline, newSchedule(st.Load, r.cfg.Seed, stageNo), issue)
	}
	close(results)
	<-collected
	res.Elapsed = time.Since(start)
	res.Interrupted = ctx.Err() != nil
	stopProgress()
	return res, nil
}

// runClosed runs Concurrency workers that each send requests back to back.
func runClosed(ctx context.Context, st *Stage, deadline time.Time, issue func(time.Time)) {
	var issued atomic.Int64
	var wg sync.WaitGroup
	for range st.Load.Concurrency {
		wg.Go(func() {
			for ctx.Err() == nil {
				now := time.Now()
				if !deadline.IsZero() && !now.Before(deadline) {
					return
				}
				if st.Requests > 0 && issued.Add(1) > int64(st.Requests) {
					return
				}
				issue(now)
			}
		})
	}
	wg.Wait()
}

// runOpen sends one request per scheduled arrival. Arrival times come from
// the schedule alone, never from when earlier requests finished, so a slow
// server cannot slow the offered load down.
func (r *Runner) runOpen(ctx context.Context, st *Stage, start, deadline time.Time, sched *schedule, issue func(time.Time)) {
	var sem chan struct{}
	if r.cfg.MaxInFlight > 0 {
		sem = make(chan struct{}, r.cfg.MaxInFlight)
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	for n := 0; st.Requests == 0 || n < st.Requests; n++ {
		at := start.Add(sched.next())
		if !deadline.IsZero() && !at.Before(deadline) {
			return
		}
		if !sleepUntil(ctx, at) {
			return
		}
		if sem != nil {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
		wg.Go(func() {
			if sem != nil {
				defer func() { <-sem }()
			}
			issue(at)
		})
	}
}

// reportProgress calls OnProgress periodically until the returned function
// is called, which also makes a final call.
func (r *Runner) reportProgress(load Load, start time.Time, c *counters) (stop func()) {
	if r.cfg.OnProgress == nil {
		return func() {}
	}
	snapshot := func() Progress {
		return Progress{
			Load:         load,
			Elapsed:      time.Since(start),
			Completed:    int(c.completed.Load()),
			Failed:       int(c.failed.Load()),
			InFlight:     int(c.inFlight.Load()),
			OutputTokens: int(c.tokens.Load()),
		}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(r.cfg.ProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.cfg.OnProgress(snapshot())
			case <-done:
				r.cfg.OnProgress(snapshot())
				return
			}
		}
	})
	return func() {
		close(done)
		wg.Wait()
	}
}

func newOutcome(seq uint64, stageStart, scheduled time.Time, resp *client.Response, err error) outcome {
	rec := Record{
		Seq:           seq,
		Scheduled:     scheduled.Sub(stageStart),
		OutputTokens:  resp.OutputTokens,
		PromptTokens:  resp.PromptTokens,
		UsageReported: resp.UsageReported,
		Chunks:        len(resp.Chunks),
		FinishReason:  resp.FinishReason,
	}
	if !resp.Sent.IsZero() {
		rec.SendLag = resp.Sent.Sub(scheduled)
	}
	if !resp.FirstToken.IsZero() {
		rec.TTFT = resp.FirstToken.Sub(scheduled)
		if rec.OutputTokens > 1 {
			rec.TPOT = resp.End.Sub(resp.FirstToken) / time.Duration(rec.OutputTokens-1)
		}
	}
	if !resp.End.IsZero() {
		rec.E2E = resp.End.Sub(scheduled)
	}
	var gaps []time.Duration
	if len(resp.Chunks) > 1 {
		gaps = make([]time.Duration, len(resp.Chunks)-1)
		for i := range gaps {
			gaps[i] = resp.Chunks[i+1] - resp.Chunks[i]
			rec.MaxITL = max(rec.MaxITL, gaps[i])
		}
	}
	if err != nil {
		if ce, ok := errors.AsType[*client.Error](err); ok {
			rec.Err = ce
		} else {
			rec.Err = &client.Error{Class: "other", Message: err.Error(), Err: err}
		}
	}
	return outcome{rec: rec, gaps: gaps}
}

// schedule yields arrival offsets from the stage start: the first arrival
// is at zero and later ones follow the arrival process.
type schedule struct {
	rate    float64
	poisson bool
	rng     *rand.Rand
	n       int
	t       float64 // offset of the last Poisson arrival, in seconds
}

func newSchedule(l Load, seed, stream uint64) *schedule {
	return &schedule{
		rate:    l.Rate,
		poisson: l.Arrival == Poisson,
		rng:     rand.New(rand.NewPCG(seed, stream)),
	}
}

func (s *schedule) next() time.Duration {
	defer func() { s.n++ }()
	if !s.poisson {
		// Computed from n rather than accumulated, so rounding never drifts.
		return time.Duration(float64(s.n) / s.rate * float64(time.Second))
	}
	if s.n > 0 {
		s.t += s.rng.ExpFloat64() / s.rate
	}
	return time.Duration(s.t * float64(time.Second))
}

// sleepUntil waits until t and reports whether ctx is still live.
func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
