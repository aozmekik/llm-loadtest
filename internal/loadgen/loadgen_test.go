package loadgen_test

import (
	"context"
	"errors"
	"math"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aozmekik/llm-loadtest/internal/client"
	"github.com/aozmekik/llm-loadtest/internal/inmem"
	"github.com/aozmekik/llm-loadtest/internal/loadgen"
	"github.com/aozmekik/llm-loadtest/internal/report"
	"github.com/aozmekik/llm-loadtest/mock"
)

const ms = time.Millisecond

type promptFunc func(uint64) client.Request

func (f promptFunc) Prompt(seq uint64) client.Request { return f(seq) }

var hello = promptFunc(func(uint64) client.Request {
	return client.Request{Messages: []client.Message{{Role: "user", Content: "hello"}}}
})

// mockClient returns a client for an in-process mock server. It must be
// used inside a synctest bubble, where the mock's latencies are exact.
func mockClient(t *testing.T, cfg mock.Config) *client.Client {
	t.Helper()
	srv, err := mock.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(client.Config{
		BaseURL: "http://mock/v1", Model: "m", IncludeUsage: true,
		HTTPClient: &http.Client{Transport: &inmem.Transport{Handler: srv}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newRunner(t *testing.T, cfg loadgen.Config) *loadgen.Runner {
	t.Helper()
	if cfg.Prompts == nil {
		cfg.Prompts = hello
	}
	r, err := loadgen.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func run(t *testing.T, r *loadgen.Runner, st loadgen.Stage) *loadgen.StageResult {
	t.Helper()
	res, err := r.Run(t.Context(), st)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func field(recs []loadgen.Record, f func(loadgen.Record) time.Duration) []time.Duration {
	out := make([]time.Duration, len(recs))
	for i, r := range recs {
		out[i] = f(r)
	}
	slices.Sort(out)
	return out
}

func durations(msValues ...int) []time.Duration {
	out := make([]time.Duration, len(msValues))
	for i, v := range msValues {
		out[i] = time.Duration(v) * ms
	}
	return out
}

// tenTokens takes 100ms to the first token and 10ms per token after it,
// so a request ends 190ms after it starts generating.
var tenTokens = mock.Config{
	TTFT: mock.Constant(100 * ms), ITL: mock.Constant(10 * ms), OutputTokens: mock.Constant(10),
}

func TestClosedLoopMeasuresExactly(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newRunner(t, loadgen.Config{Streamer: mockClient(t, tenTokens)})
		res := run(t, r, loadgen.Stage{Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: 2}, Requests: 6})
		if len(res.Records) != 6 {
			t.Fatalf("got %d records, want 6", len(res.Records))
		}
		for _, rec := range res.Records {
			if rec.Err != nil {
				t.Fatalf("request failed: %v", rec.Err)
			}
			if rec.TTFT != 100*ms || rec.E2E != 190*ms || rec.TPOT != 10*ms || rec.MaxITL != 10*ms || rec.SendLag != 0 {
				t.Errorf("record %+v: want TTFT 100ms, E2E 190ms, TPOT 10ms, MaxITL 10ms, no send lag", rec)
			}
			if rec.OutputTokens != 10 || !rec.UsageReported || rec.Chunks != 10 {
				t.Errorf("record tokens=%d usage=%v chunks=%d", rec.OutputTokens, rec.UsageReported, rec.Chunks)
			}
		}
		if got, want := field(res.Records, func(r loadgen.Record) time.Duration { return r.Scheduled }), durations(0, 0, 190, 190, 380, 380); !slices.Equal(got, want) {
			t.Errorf("scheduled starts = %v, want %v (two workers back to back)", got, want)
		}
		if res.Elapsed != 570*ms {
			t.Errorf("Elapsed = %v, want 570ms", res.Elapsed)
		}
		if n := res.ITL.Count(); n != 6*9 {
			t.Errorf("ITL samples = %d, want 54", n)
		}
		if p50 := res.ITL.Quantile(0.5); math.Abs(p50-10) > 10*loadgen.ITLAccuracy {
			t.Errorf("ITL p50 = %vms, want 10ms within the sketch accuracy", p50)
		}
		if res.Interrupted {
			t.Error("Interrupted set on a completed stage")
		}
	})
}

func TestClosedLoopDuration(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newRunner(t, loadgen.Config{Streamer: mockClient(t, tenTokens)})
		res := run(t, r, loadgen.Stage{Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: 3}, Duration: time.Second})
		// Each worker starts at 0, 190, ..., 950ms: six requests, the last
		// finishing at 1140ms.
		if len(res.Records) != 18 || res.Elapsed != 1140*ms {
			t.Errorf("got %d records in %v, want 18 in 1.14s", len(res.Records), res.Elapsed)
		}
	})
}

// recordingStreamer completes every request after a fixed latency and
// records when each was sent.
type recordingStreamer struct {
	latency time.Duration
	mu      sync.Mutex
	sent    []time.Time
}

func (s *recordingStreamer) Stream(ctx context.Context, _ client.Request) (client.Response, error) {
	sent := time.Now()
	s.mu.Lock()
	s.sent = append(s.sent, sent)
	s.mu.Unlock()
	select {
	case <-time.After(s.latency):
	case <-ctx.Done():
		return client.Response{Sent: sent, End: time.Now()}, &client.Error{Class: client.ClassCanceled, Message: "canceled"}
	}
	end := time.Now()
	return client.Response{
		Sent: sent, FirstToken: sent.Add(s.latency / 2), End: end,
		Chunks: []time.Duration{s.latency / 2, s.latency}, OutputTokens: 2,
	}, nil
}

func TestOpenLoopFollowsSchedule(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := &recordingStreamer{latency: 250 * ms}
		r := newRunner(t, loadgen.Config{Streamer: s})
		start := time.Now()
		res := run(t, r, loadgen.Stage{
			Load:     loadgen.Load{Mode: loadgen.Open, Rate: 10, Arrival: loadgen.Constant},
			Requests: 5,
		})
		sentAt := make([]time.Duration, 0, len(s.sent))
		for _, ts := range s.sent {
			sentAt = append(sentAt, ts.Sub(start))
		}
		// Requests overlap: arrivals do not wait for earlier responses.
		if want := durations(0, 100, 200, 300, 400); !slices.Equal(sentAt, want) {
			t.Errorf("sent at %v, want %v", sentAt, want)
		}
		if res.Elapsed != 650*ms {
			t.Errorf("Elapsed = %v, want 650ms", res.Elapsed)
		}
		for _, rec := range res.Records {
			if rec.SendLag != 0 || rec.E2E != 250*ms {
				t.Errorf("record %+v: want no send lag and 250ms latency", rec)
			}
		}
	})
}

func TestOpenLoopDuration(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newRunner(t, loadgen.Config{Streamer: &recordingStreamer{latency: 50 * ms}})
		res := run(t, r, loadgen.Stage{
			Load:     loadgen.Load{Mode: loadgen.Open, Rate: 10, Arrival: loadgen.Constant},
			Duration: time.Second,
		})
		// Arrivals at 0, 100, ..., 900ms; the one at 1000ms is excluded.
		if got := field(res.Records, func(r loadgen.Record) time.Duration { return r.Scheduled }); !slices.Equal(got, durations(0, 100, 200, 300, 400, 500, 600, 700, 800, 900)) {
			t.Errorf("scheduled starts = %v", got)
		}
	})
}

// TestCoordinatedOmission is the reason the open-loop mode exists. The
// server serves one request at a time and needs 90ms per request. A closed
// loop with one worker never sends while the server is busy, so it reports
// 90ms for every request and hides the overload. An open loop at 20 req/s
// (one every 50ms) sends on schedule regardless, so the server-side queue
// grows by 40ms per request and shows up in the measured latency.
// TestMaxInFlightKeepsLatencyHonest covers the client-side counterpart.
func TestCoordinatedOmission(t *testing.T) {
	t.Parallel()
	server := mock.Config{ITL: mock.Constant(10 * ms), OutputTokens: mock.Constant(10), Slots: 1}

	synctest.Test(t, func(t *testing.T) {
		r := newRunner(t, loadgen.Config{Streamer: mockClient(t, server)})
		closed := run(t, r, loadgen.Stage{Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: 1}, Requests: 10})
		for _, rec := range closed.Records {
			if rec.E2E != 90*ms {
				t.Errorf("closed loop: E2E = %v, want 90ms", rec.E2E)
			}
		}
	})

	synctest.Test(t, func(t *testing.T) {
		r := newRunner(t, loadgen.Config{Streamer: mockClient(t, server)})
		open := run(t, r, loadgen.Stage{
			Load:     loadgen.Load{Mode: loadgen.Open, Rate: 20, Arrival: loadgen.Constant},
			Requests: 10,
		})
		got := field(open.Records, func(r loadgen.Record) time.Duration { return r.E2E })
		// Request k is scheduled at 50k ms and finishes at 90(k+1) ms.
		want := durations(90, 130, 170, 210, 250, 290, 330, 370, 410, 450)
		if !slices.Equal(got, want) {
			t.Errorf("open loop: E2E = %v, want %v", got, want)
		}
		ttft := field(open.Records, func(r loadgen.Record) time.Duration { return r.TTFT })
		if want := durations(0, 40, 80, 120, 160, 200, 240, 280, 320, 360); !slices.Equal(ttft, want) {
			t.Errorf("open loop: TTFT = %v, want queueing delay %v", ttft, want)
		}
	})
}

// TestMaxInFlightKeepsLatencyHonest checks that when the in-flight cap
// delays sending, the delay is reported as send lag and counted in the
// latency rather than silently dropped.
func TestMaxInFlightKeepsLatencyHonest(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newRunner(t, loadgen.Config{Streamer: &recordingStreamer{latency: 250 * ms}, MaxInFlight: 1})
		res := run(t, r, loadgen.Stage{
			Load:     loadgen.Load{Mode: loadgen.Open, Rate: 10, Arrival: loadgen.Constant},
			Requests: 5,
		})
		lags := field(res.Records, func(r loadgen.Record) time.Duration { return r.SendLag })
		if want := durations(0, 150, 300, 450, 600); !slices.Equal(lags, want) {
			t.Errorf("send lag = %v, want %v", lags, want)
		}
		e2e := field(res.Records, func(r loadgen.Record) time.Duration { return r.E2E })
		if want := durations(250, 400, 550, 700, 850); !slices.Equal(e2e, want) {
			t.Errorf("E2E = %v, want %v", e2e, want)
		}
	})
}

// TestOpenLoopSendsBacklogAfterDuration pins down a documented trade-off:
// arrivals scheduled within the duration but held back by MaxInFlight are
// still sent, and measured from their scheduled start, so the stage
// outlasts its duration instead of dropping them.
func TestOpenLoopSendsBacklogAfterDuration(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newRunner(t, loadgen.Config{Streamer: &recordingStreamer{latency: time.Second}, MaxInFlight: 1})
		res := run(t, r, loadgen.Stage{
			Load:     loadgen.Load{Mode: loadgen.Open, Rate: 10, Arrival: loadgen.Constant},
			Duration: time.Second,
		})
		// Arrivals at 0, 100, ..., 900ms, sent one per second: arrival k
		// waits 900k ms.
		lags := field(res.Records, func(r loadgen.Record) time.Duration { return r.SendLag })
		if want := durations(0, 900, 1800, 2700, 3600, 4500, 5400, 6300, 7200, 8100); !slices.Equal(lags, want) {
			t.Errorf("send lag = %v, want %v", lags, want)
		}
		if res.Elapsed != 10*time.Second {
			t.Errorf("Elapsed = %v, want 10s for a 1s stage", res.Elapsed)
		}
	})
}

func TestPoissonArrivals(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := &recordingStreamer{latency: ms}
		r := newRunner(t, loadgen.Config{Streamer: s, Seed: 3})
		const n, rate = 5000, 100.0
		res := run(t, r, loadgen.Stage{Load: loadgen.Load{Mode: loadgen.Open, Rate: rate, Arrival: loadgen.Poisson}, Requests: n})
		starts := field(res.Records, func(r loadgen.Record) time.Duration { return r.Scheduled })
		var sum, sq float64
		for i := 1; i < len(starts); i++ {
			g := (starts[i] - starts[i-1]).Seconds()
			sum += g
			sq += g * g
		}
		mean := sum / (n - 1)
		cv := math.Sqrt(sq/(n-1)-mean*mean) / mean
		// Exponential gaps: mean 1/rate and coefficient of variation 1.
		// The bounds are five standard errors, so they hold for any seed.
		if math.Abs(mean-1/rate) > 5*(1/rate)/math.Sqrt(n) {
			t.Errorf("mean gap = %.5fs, want %.5fs", mean, 1/rate)
		}
		if math.Abs(cv-1) > 0.07 {
			t.Errorf("coefficient of variation = %.3f, want about 1", cv)
		}
	})
}

// TestMeasuredDistributionsMatchMock runs the whole measurement path (mock,
// client, load generator, ITL sketch and report) with normally distributed
// latencies and checks the reported percentiles against the configured
// distributions. Every request draws from a source seeded by its sequence
// number, so the sample, and hence the test, is the same on every run. The
// bounds are five standard errors of each sample quantile, plus the
// sketch's 1% for ITL.
func TestMeasuredDistributionsMatchMock(t *testing.T) {
	t.Parallel()
	const (
		requests          = 2000
		tokens            = 16
		ttftMean, ttftSD  = 150.0, 30.0 // ms
		itlMean, itlSD    = 20.0, 4.0   // ms
		z50, z90, z99     = 0.0, 1.2815515655446004, 2.3263478740408408
		standardErrorsMax = 5
	)
	synctest.Test(t, func(t *testing.T) {
		c := mockClient(t, mock.Config{
			TTFT:         mock.Normal(time.Duration(ttftMean*float64(ms)), time.Duration(ttftSD*float64(ms))),
			ITL:          mock.Normal(time.Duration(itlMean*float64(ms)), time.Duration(itlSD*float64(ms))),
			OutputTokens: mock.Constant(tokens),
			Seed:         5,
		})
		res := run(t, newRunner(t, loadgen.Config{Streamer: c}),
			loadgen.Stage{Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: 16}, Requests: requests})
		st := report.NewStage(res)
		if st.Succeeded != requests || st.ITL.Count != requests*(tokens-1) {
			t.Fatalf("%d successes and %d ITL samples, want %d and %d", st.Succeeded, st.ITL.Count, requests, requests*(tokens-1))
		}
		check := func(name string, got, mean, sd, z float64, n int, slack float64) {
			t.Helper()
			p := 0.5 * math.Erfc(-z/math.Sqrt2)                       // the quantile's probability
			density := math.Exp(-z*z/2) / (sd * math.Sqrt(2*math.Pi)) // at the true quantile
			se := math.Sqrt(p*(1-p)/float64(n)) / density
			want := mean + z*sd
			if bound := standardErrorsMax*se + slack*want; math.Abs(got-want) > bound {
				t.Errorf("%s = %.2f ms, want %.2f ms within %.2f", name, got, want, bound)
			}
		}
		for _, q := range []struct {
			name      string
			ttft, itl float64
			z         float64
		}{
			{"p50", st.TTFT.P50, st.ITL.P50, z50},
			{"p90", st.TTFT.P90, st.ITL.P90, z90},
			{"p99", st.TTFT.P99, st.ITL.P99, z99},
		} {
			check("TTFT "+q.name, q.ttft, ttftMean, ttftSD, q.z, requests, 0)
			check("ITL "+q.name, q.itl, itlMean, itlSD, q.z, requests*(tokens-1), loadgen.ITLAccuracy)
		}
		// TPOT averages 15 gaps per request: mean 20ms, sd 4/sqrt(15) ms.
		check("TPOT p50", st.TPOT.P50, itlMean, itlSD/math.Sqrt(tokens-1), z50, requests, 0)
	})
}

func TestFailuresAreRecorded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var last loadgen.Progress
		r := newRunner(t, loadgen.Config{
			Streamer:   mockClient(t, mock.Config{ErrorRate: 1, ErrorStatus: 503}),
			OnProgress: func(p loadgen.Progress) { last = p },
		})
		res := run(t, r, loadgen.Stage{Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: 2}, Requests: 4})
		for _, rec := range res.Records {
			if rec.Err == nil || rec.Err.Class != "http_503" {
				t.Errorf("record error = %v, want http_503", rec.Err)
			}
		}
		if res.ITL.Count() != 0 {
			t.Error("failed requests contributed ITL samples")
		}
		if last.Completed != 4 || last.Failed != 4 || last.InFlight != 0 {
			t.Errorf("final progress = %+v, want 4 completed and 4 failed", last)
		}
	})
}

type errStreamer struct{}

func (errStreamer) Stream(context.Context, client.Request) (client.Response, error) {
	return client.Response{}, errors.New("not a client error")
}

func TestForeignErrorsAreWrapped(t *testing.T) {
	t.Parallel()
	r := newRunner(t, loadgen.Config{Streamer: errStreamer{}})
	res := run(t, r, loadgen.Stage{Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: 1}, Requests: 1})
	if rec := res.Records[0]; rec.Err == nil || rec.Err.Class != "other" {
		t.Errorf("record error = %v, want class other", rec.Err)
	}
}

func TestInterrupt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		slow := mock.Config{TTFT: mock.Constant(10 * time.Second), OutputTokens: mock.Constant(1)}
		r := newRunner(t, loadgen.Config{Streamer: mockClient(t, slow)})
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(time.Second, cancel)
		res, err := r.Run(ctx, loadgen.Stage{Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: 3}, Duration: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Interrupted || len(res.Records) != 3 || res.Elapsed != time.Second {
			t.Fatalf("interrupted=%v records=%d elapsed=%v; want true, 3, 1s", res.Interrupted, len(res.Records), res.Elapsed)
		}
		for _, rec := range res.Records {
			if rec.Err == nil || rec.Err.Class != client.ClassCanceled {
				t.Errorf("in-flight request error = %v, want canceled", rec.Err)
			}
		}
	})
}

func TestProgressReporting(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var snaps []loadgen.Progress
		r := newRunner(t, loadgen.Config{
			Streamer:         mockClient(t, tenTokens),
			OnProgress:       func(p loadgen.Progress) { snaps = append(snaps, p) },
			ProgressInterval: 100 * ms,
		})
		run(t, r, loadgen.Stage{Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: 1}, Requests: 2})
		// The stage takes 380ms: ticks at 100, 200, 300ms plus a final call.
		if len(snaps) != 4 {
			t.Fatalf("got %d progress calls, want 4: %+v", len(snaps), snaps)
		}
		if mid := snaps[1]; mid.Completed != 1 || mid.InFlight != 1 || mid.OutputTokens != 10 || mid.Elapsed != 200*ms {
			t.Errorf("progress at 200ms = %+v", mid)
		}
		if final := snaps[3]; final.Completed != 2 || final.InFlight != 0 || final.OutputTokens != 20 {
			t.Errorf("final progress = %+v", final)
		}
	})
}

func TestSequenceContinuesAcrossStages(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seqs []uint64
	prompts := promptFunc(func(seq uint64) client.Request {
		mu.Lock()
		seqs = append(seqs, seq)
		mu.Unlock()
		return hello(seq)
	})
	r := newRunner(t, loadgen.Config{Streamer: &recordingStreamer{}, Prompts: prompts})
	st := loadgen.Stage{Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: 2}, Requests: 3}
	run(t, r, st)
	run(t, r, st)
	slices.Sort(seqs)
	if want := []uint64{0, 1, 2, 3, 4, 5}; !slices.Equal(seqs, want) {
		t.Errorf("sequence numbers = %v, want %v", seqs, want)
	}
}

func TestValidation(t *testing.T) {
	t.Parallel()
	if _, err := loadgen.New(loadgen.Config{}); err == nil {
		t.Error("New without Streamer succeeded")
	}
	if _, err := loadgen.New(loadgen.Config{Streamer: errStreamer{}, Prompts: hello, MaxInFlight: -1}); err == nil {
		t.Error("New with negative MaxInFlight succeeded")
	}
	r := newRunner(t, loadgen.Config{Streamer: errStreamer{}})
	for name, st := range map[string]loadgen.Stage{
		"no limit":         {Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: 1}},
		"zero concurrency": {Load: loadgen.Load{Mode: loadgen.Closed}, Requests: 1},
		"zero rate":        {Load: loadgen.Load{Mode: loadgen.Open, Arrival: loadgen.Poisson}, Requests: 1},
		"infinite rate":    {Load: loadgen.Load{Mode: loadgen.Open, Rate: math.Inf(1), Arrival: loadgen.Poisson}, Requests: 1},
		"bad arrival":      {Load: loadgen.Load{Mode: loadgen.Open, Rate: 1, Arrival: "bursty"}, Requests: 1},
		"bad mode":         {Load: loadgen.Load{Mode: "chaos"}, Requests: 1},
		"negative count":   {Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: 1}, Requests: -1},
	} {
		if _, err := r.Run(t.Context(), st); err == nil {
			t.Errorf("%s: Run succeeded, want a validation error", name)
		}
	}
}

func TestLoadString(t *testing.T) {
	t.Parallel()
	for l, want := range map[loadgen.Load]string{
		{Mode: loadgen.Closed, Concurrency: 8}:                    "concurrency=8",
		{Mode: loadgen.Open, Rate: 2.5, Arrival: loadgen.Poisson}: "rate=2.5/s poisson",
		{Mode: loadgen.Open, Rate: 10, Arrival: loadgen.Constant}: "rate=10/s constant",
	} {
		if got := l.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}
