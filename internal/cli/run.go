package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aozmekik/llm-loadtest/internal/client"
	"github.com/aozmekik/llm-loadtest/internal/loadgen"
	"github.com/aozmekik/llm-loadtest/internal/prompt"
	"github.com/aozmekik/llm-loadtest/internal/report"
)

// defaultStageDuration applies when neither --duration nor --requests is set.
const defaultStageDuration = 30 * time.Second

type runOptions struct {
	baseURL, model, apiKeyEnv string
	headers                   headerList
	dataset                   string
	shuffle                   bool
	inputTokens               int
	maxTokens                 int
	temperature               optionalFloat
	extraBody                 string
	includeUsage              bool
	concurrency               intList
	rates                     floatList
	arrival                   string
	maxInFlight               int
	duration                  time.Duration
	requests                  int
	warmup                    int
	timeout, idleTimeout      time.Duration
	seed                      uint64
	outs                      stringList
	jsonRecords               bool
	quiet                     bool
}

func (o *runOptions) register(fs *flag.FlagSet) {
	fs.StringVar(&o.baseURL, "base-url", "", "API root of the server, e.g. http://localhost:8000/v1 (required)")
	fs.StringVar(&o.model, "model", "", "model name sent with each request (required)")
	fs.StringVar(&o.apiKeyEnv, "api-key-env", "OPENAI_API_KEY", "`name` of the environment variable holding the API key; never printed or saved")
	fs.Var(&o.headers, "header", "extra request `header` \"Name: value\" (repeatable); only names are saved")
	fs.StringVar(&o.dataset, "dataset", "", "JSONL `file` of prompts, one {\"prompt\": ...} or {\"messages\": [...]} per line")
	fs.BoolVar(&o.shuffle, "shuffle", false, "shuffle the dataset, seeded by --seed")
	fs.IntVar(&o.inputTokens, "input-tokens", 256, "approximate length of synthetic prompts when no --dataset is given")
	fs.IntVar(&o.maxTokens, "max-tokens", 256, "max_tokens of each request; 0 omits the field")
	fs.Var(&o.temperature, "temperature", "sampling temperature (default: the server's)")
	fs.StringVar(&o.extraBody, "extra-body", "", "JSON object merged into every request body, e.g. '{\"ignore_eos\": true}'")
	fs.BoolVar(&o.includeUsage, "include-usage", true, "ask for exact token counts with stream_options.include_usage")
	fs.Var(&o.concurrency, "concurrency", "closed-loop `levels`; a list such as 1,2,4,8 runs a sweep (default 1)")
	fs.Var(&o.rates, "rate", "open-loop arrival `rates` in requests/s; a list such as 0.5,1,2 runs a sweep")
	fs.StringVar(&o.arrival, "arrival", string(loadgen.Poisson), "open-loop arrival process: poisson or constant")
	fs.IntVar(&o.maxInFlight, "max-in-flight", 1024, "open-loop cap on concurrent requests; 0 means no cap")
	fs.DurationVar(&o.duration, "duration", 0, "length of each stage (default 30s unless --requests is set)")
	fs.IntVar(&o.requests, "requests", 0, "number of requests per stage")
	fs.IntVar(&o.warmup, "warmup", 0, "requests sent before the first stage and left out of the results")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Minute, "per-request timeout; 0 disables it")
	fs.DurationVar(&o.idleTimeout, "idle-timeout", 0, "fail a request after this long without receiving data; 0 disables it")
	fs.Uint64Var(&o.seed, "seed", 1, "seed for synthetic prompts, shuffling and Poisson arrivals")
	fs.Var(&o.outs, "out", "write results to a `file`: .json, .csv (one row per request) or .md (repeatable)")
	fs.BoolVar(&o.jsonRecords, "json-records", true, "include one record per request in JSON output (CSV always has them)")
	fs.BoolVar(&o.quiet, "quiet", false, "do not print progress")
}

// output is one --out destination.
type output struct {
	path  string
	write func(io.Writer, *report.Result) error
}

// runPlan is a validated run configuration.
type runPlan struct {
	client  client.Config
	prompts loadgen.Prompts
	stages  []loadgen.Stage
	outputs []output
	report  report.Config
}

// plan validates the options and resolves them into a runPlan.
func (o *runOptions) plan(env *Env) (*runPlan, error) {
	if o.baseURL == "" || o.model == "" {
		return nil, errors.New("--base-url and --model are required")
	}
	if o.maxTokens < 0 || o.maxInFlight < 0 || o.warmup < 0 {
		return nil, errors.New("--max-tokens, --max-in-flight and --warmup must not be negative")
	}
	p := &runPlan{}

	var extra map[string]any
	if o.extraBody != "" {
		if err := json.Unmarshal([]byte(o.extraBody), &extra); err != nil {
			return nil, fmt.Errorf("--extra-body: %w", err)
		}
		if extra == nil {
			return nil, errors.New("--extra-body must be a JSON object")
		}
	}
	apiKey := ""
	if o.apiKeyEnv != "" {
		apiKey = env.Getenv(o.apiKeyEnv)
	}
	p.client = client.Config{
		BaseURL:      o.baseURL,
		Model:        o.model,
		APIKey:       apiKey,
		Headers:      o.headers.h,
		UserAgent:    "llm-loadtest/" + env.Version,
		MaxTokens:    o.maxTokens,
		Temperature:  o.temperature.v,
		IncludeUsage: o.includeUsage,
		ExtraBody:    extra,
		Timeout:      o.timeout,
		IdleTimeout:  o.idleTimeout,
	}

	if err := o.buildStages(p); err != nil {
		return nil, err
	}
	promptDesc, err := o.loadPrompts(p)
	if err != nil {
		return nil, err
	}
	if p.outputs, err = parseOutputs(o.outs, o.jsonRecords); err != nil {
		return nil, err
	}

	p.report = report.Config{
		BaseURL:      client.RedactURL(o.baseURL),
		Model:        o.model,
		Prompts:      promptDesc,
		MaxTokens:    o.maxTokens,
		Temperature:  o.temperature.v,
		IncludeUsage: o.includeUsage,
		HeaderNames:  headerNames(o.headers.h),
		ExtraBody:    slices.Sorted(maps.Keys(extra)),
		Requests:     p.stages[0].Requests,
		Warmup:       o.warmup,
		Seed:         o.seed,
	}
	if d := p.stages[0].Duration; d > 0 {
		p.report.Duration = d.String()
	}
	if o.timeout > 0 {
		p.report.Timeout = o.timeout.String()
	}
	if o.idleTimeout > 0 {
		p.report.IdleTimeout = o.idleTimeout.String()
	}
	if len(o.rates) > 0 {
		p.report.MaxInFlight = o.maxInFlight
	}
	return p, nil
}

func (o *runOptions) loadPrompts(p *runPlan) (string, error) {
	if o.dataset == "" {
		s, err := prompt.NewSynthetic(o.inputTokens, o.seed)
		if err != nil {
			return "", err
		}
		p.prompts = s
		return s.String(), nil
	}
	d, err := prompt.LoadJSONL(o.dataset)
	if err != nil {
		return "", err
	}
	desc := fmt.Sprintf("dataset %s (%d prompts)", filepath.Base(o.dataset), d.Len())
	if o.shuffle {
		d.Shuffle(o.seed)
		desc = fmt.Sprintf("dataset %s (%d prompts, shuffled with seed %d)", filepath.Base(o.dataset), d.Len(), o.seed)
	}
	p.prompts = d
	return desc, nil
}

func (o *runOptions) buildStages(p *runPlan) error {
	if len(o.concurrency) > 0 && len(o.rates) > 0 {
		return errors.New("--concurrency (closed loop) and --rate (open loop) are mutually exclusive")
	}
	if o.duration < 0 || o.requests < 0 {
		return errors.New("--duration and --requests must not be negative")
	}
	duration := o.duration
	if duration == 0 && o.requests == 0 {
		duration = defaultStageDuration
	}
	arrival := loadgen.Arrival(o.arrival)
	if arrival != loadgen.Poisson && arrival != loadgen.Constant {
		return fmt.Errorf("--arrival must be poisson or constant, not %q", o.arrival)
	}
	for _, r := range o.rates {
		p.stages = append(p.stages, loadgen.Stage{
			Load: loadgen.Load{Mode: loadgen.Open, Rate: r, Arrival: arrival}, Duration: duration, Requests: o.requests,
		})
	}
	levels := o.concurrency
	if len(o.rates) == 0 && len(levels) == 0 {
		levels = intList{1}
	}
	for _, c := range levels {
		p.stages = append(p.stages, loadgen.Stage{
			Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: c}, Duration: duration, Requests: o.requests,
		})
	}
	return nil
}

func parseOutputs(paths []string, jsonRecords bool) ([]output, error) {
	outs := make([]output, 0, len(paths))
	for _, path := range paths {
		var write func(io.Writer, *report.Result) error
		switch strings.ToLower(filepath.Ext(path)) {
		case ".json":
			write = report.WriteJSON
			if !jsonRecords {
				write = writeJSONSummary
			}
		case ".csv":
			write = report.WriteCSV
		case ".md":
			write = report.WriteMarkdown
		default:
			return nil, fmt.Errorf("--out %q: unknown extension; use .json, .csv or .md", path)
		}
		// Fail now rather than after a long run.
		if info, err := os.Stat(filepath.Dir(path)); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("--out %q: directory %q does not exist", path, filepath.Dir(path))
		}
		outs = append(outs, output{path: path, write: write})
	}
	return outs, nil
}

// writeJSONSummary writes the result without per-request records, which
// dominate the file size of long or high-throughput runs.
func writeJSONSummary(w io.Writer, r *report.Result) error {
	summary := *r
	summary.Stages = slices.Clone(r.Stages)
	for i := range summary.Stages {
		summary.Stages[i].Records = nil
	}
	return report.WriteJSON(w, &summary)
}

func headerNames(h http.Header) []string {
	return slices.Sorted(maps.Keys(h))
}

func runCommand(ctx context.Context, args []string, env *Env) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var o runOptions
	o.register(fs)
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), "Usage: llm-loadtest run --base-url URL --model NAME [flags]\n\n"+
			"Load-tests a streaming chat-completions endpoint and prints a Markdown report.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		return usageError(env, "run", fmt.Errorf("unexpected argument %q", fs.Arg(0)))
	}
	p, err := o.plan(env)
	if err != nil {
		return usageError(env, "run", err)
	}
	c, err := client.New(p.client)
	if err != nil {
		return usageError(env, "run", err)
	}

	printer := &progressPrinter{w: env.Stderr, tty: env.StderrIsTerminal}
	cfg := loadgen.Config{Streamer: c, Prompts: p.prompts, MaxInFlight: o.maxInFlight, Seed: o.seed}
	if !o.quiet {
		cfg.OnProgress = printer.update
		cfg.ProgressInterval = time.Second
		if env.StderrIsTerminal {
			cfg.ProgressInterval = 250 * time.Millisecond
		}
	}
	runner, err := loadgen.New(cfg)
	if err != nil {
		return failure(env, "run", err)
	}

	result := &report.Result{
		SchemaVersion: report.SchemaVersion,
		Tool:          "llm-loadtest " + env.Version,
		StartedAt:     time.Now().UTC().Truncate(time.Second),
		Config:        p.report,
	}

	if o.warmup > 0 {
		if err := warmup(ctx, runner, p, o.warmup, printer, !o.quiet, env); err != nil {
			return failure(env, "run", err)
		}
	}
	for i, st := range p.stages {
		if ctx.Err() != nil {
			break
		}
		printer.begin(fmt.Sprintf("[%d/%d] %s", i+1, len(p.stages), st.Load))
		res, err := runner.Run(ctx, st)
		if err != nil {
			return failure(env, "run", err)
		}
		if !o.quiet {
			printer.end()
		}
		result.Stages = append(result.Stages, report.NewStage(res))
	}
	result.Interrupted = ctx.Err() != nil
	if len(result.Stages) == 0 {
		_, _ = fmt.Fprintln(env.Stderr, "llm-loadtest run: interrupted before any stage started")
		return exitInterrupted
	}

	if err := report.WriteMarkdown(env.Stdout, result); err != nil {
		return failure(env, "run", err)
	}
	for _, out := range p.outputs {
		if err := writeFile(out.path, func(w io.Writer) error { return out.write(w, result) }); err != nil {
			return failure(env, "run", err)
		}
		_, _ = fmt.Fprintf(env.Stderr, "wrote %s\n", out.path)
	}
	switch {
	case result.Interrupted:
		return exitInterrupted
	case succeeded(result) == 0:
		_, _ = fmt.Fprintln(env.Stderr, "llm-loadtest run: no request succeeded; see the errors in the report")
		return exitFailure
	}
	return exitOK
}

// warmup sends n requests that are not part of the results. It fails only
// when every warmup request fails, which usually means a wrong URL, model
// or key, so a long run is not wasted.
func warmup(ctx context.Context, r *loadgen.Runner, p *runPlan, n int, printer *progressPrinter, verbose bool, env *Env) error {
	concurrency := 1
	if first := p.stages[0].Load; first.Mode == loadgen.Closed {
		concurrency = first.Concurrency
	}
	printer.begin("warmup")
	res, err := r.Run(ctx, loadgen.Stage{
		Load:     loadgen.Load{Mode: loadgen.Closed, Concurrency: min(concurrency, n)},
		Requests: n,
	})
	if err != nil {
		return err
	}
	if verbose {
		printer.end()
	}
	var failed []*client.Error
	for i := range res.Records {
		if e := res.Records[i].Err; e != nil {
			failed = append(failed, e)
		}
	}
	if !res.Interrupted && len(failed) == len(res.Records) {
		return fmt.Errorf("all %d warmup requests failed; first error: %w", n, failed[0])
	}
	_, _ = fmt.Fprintf(env.Stderr, "warmup: %d requests, %d failed (not included in results)\n", len(res.Records), len(failed))
	return nil
}

func succeeded(r *report.Result) int {
	n := 0
	for i := range r.Stages {
		n += r.Stages[i].Succeeded
	}
	return n
}

// writeFile creates path and writes it with write, reporting close errors.
func writeFile(path string, write func(io.Writer) error) (err error) {
	f, err := os.Create(path) //nolint:gosec // The path is an output file the user named.
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("close %s: %w", path, cerr)
		}
	}()
	return write(f)
}
