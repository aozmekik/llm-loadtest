package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/aozmekik/llm-loadtest/internal/report"
)

func compareCommand(args []string, env *Env) int {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	threshold := fs.Float64("threshold", 0,
		"fail (exit 1) when a throughput or latency metric is more than `PCT` percent worse, a base stage is\n"+
			"missing from head or has no successful request there, or either run was interrupted; 0 only reports")
	errThreshold := fs.Float64("error-threshold", 0,
		"with --threshold, fail when the error rate rises by more than `PP` percentage points (default: the --threshold value)")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), "Usage: llm-loadtest compare [--threshold PCT [--error-threshold PP]] BASE.json HEAD.json\n\n"+
			"Compares two results stage by stage, matching stages by load.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	files, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if len(files) != 2 {
		return usageError(env, "compare", fmt.Errorf("want 2 result files, got %d", len(files)))
	}
	if *threshold < 0 || *errThreshold < 0 {
		return usageError(env, "compare", errors.New("--threshold and --error-threshold must not be negative"))
	}
	thresholds := report.Thresholds{Relative: *threshold, ErrorRate: *threshold}
	if isSet(fs, "error-threshold") {
		if *threshold == 0 {
			return usageError(env, "compare", errors.New("--error-threshold needs --threshold"))
		}
		thresholds.ErrorRate = *errThreshold
	}
	base, err := readResult(files[0])
	if err != nil {
		return failure(env, "compare", err)
	}
	head, err := readResult(files[1])
	if err != nil {
		return failure(env, "compare", err)
	}
	c := report.Compare(base, head, thresholds)
	if err := c.WriteMarkdown(env.Stdout, files[0], files[1]); err != nil {
		return failure(env, "compare", err)
	}
	if failures := c.Failures(); len(failures) > 0 {
		_, _ = fmt.Fprintf(env.Stderr, "llm-loadtest compare: failed: %s\n", strings.Join(failures, "; "))
		return exitFailure
	}
	return exitOK
}

// isSet reports whether the named flag was given on the command line.
func isSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

func readResult(path string) (*report.Result, error) {
	f, err := os.Open(path) //nolint:gosec // The path is a result file the user named.
	if err != nil {
		return nil, fmt.Errorf("open result: %w", err)
	}
	defer func() { _ = f.Close() }()
	r, err := report.ReadJSON(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}
