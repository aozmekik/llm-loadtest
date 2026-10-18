package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/aozmekik/llm-loadtest/mock"
)

// shutdownTimeout bounds how long the mock waits for open streams on exit.
const shutdownTimeout = 5 * time.Second

func mockCommand(ctx context.Context, args []string, env *Env) int {
	fs := flag.NewFlagSet("mock", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var (
		cfg  mock.Config
		addr string
	)
	fs.StringVar(&addr, "addr", "127.0.0.1:8000", "listen `address`; port 0 picks a free port")
	fs.StringVar(&cfg.Model, "model", "mock", "model name listed by /v1/models")
	durationDistVar(fs, &cfg.TTFT, "ttft", mock.Normal(150*time.Millisecond, 30*time.Millisecond),
		"time to first token: `dist` 50ms, uniform:LO,HI, normal:MEAN,STDDEV or exp:MEAN")
	durationDistVar(fs, &cfg.ITL, "itl", mock.Normal(20*time.Millisecond, 4*time.Millisecond),
		"inter-token latency `dist`, same forms as --ttft")
	countDistVar(fs, &cfg.OutputTokens, "output-tokens", mock.Constant(0),
		"output length `dist` such as 128 or uniform:64,256, capped by max_tokens; 0 generates max_tokens")
	fs.IntVar(&cfg.TokensPerChunk, "tokens-per-chunk", 1, "tokens per streamed chunk")
	fs.IntVar(&cfg.Slots, "slots", 0, "concurrent generations before requests queue; 0 is unlimited")
	fs.Float64Var(&cfg.ErrorRate, "error-rate", 0, "fraction of requests rejected with --error-status")
	fs.IntVar(&cfg.ErrorStatus, "error-status", http.StatusInternalServerError, "HTTP status for --error-rate")
	fs.Float64Var(&cfg.StreamErrorRate, "stream-error-rate", 0, "fraction of streams that send an error payload part-way")
	fs.Float64Var(&cfg.AbortRate, "abort-rate", 0, "fraction of streams whose connection is dropped part-way")
	fs.BoolVar(&cfg.DisableUsage, "no-usage", false, "ignore stream_options.include_usage, like servers without it")
	fs.Uint64Var(&cfg.Seed, "seed", 1, "seed for the per-request random sources")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), "Usage: llm-loadtest mock [flags]\n\n"+
			"Serves an OpenAI-compatible chat-completions API with synthetic latency, for\n"+
			"rehearsing benchmarks and testing clients without a GPU.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		return usageError(env, "mock", fmt.Errorf("unexpected argument %q", fs.Arg(0)))
	}
	srv, err := mock.New(cfg)
	if err != nil {
		return usageError(env, "mock", err)
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return failure(env, "mock", err)
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	outputTokens, slots := cfg.OutputTokens.String(), strconv.Itoa(cfg.Slots)
	if outputTokens == "0" {
		outputTokens = "max_tokens"
	}
	if cfg.Slots == 0 {
		slots = "unlimited"
	}
	_, _ = fmt.Fprintf(env.Stderr, "mock: serving http://%s/v1 (ttft %s, itl %s, output tokens %s, slots %s)\n",
		ln.Addr(), cfg.TTFT, cfg.ITL, outputTokens, slots)

	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()
	select {
	case err := <-serveErr:
		return failure(env, "mock", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := hs.Shutdown(shutdownCtx); err != nil {
		_ = hs.Close()
	}
	_, _ = fmt.Fprintln(env.Stderr, "mock: stopped")
	return exitOK
}
