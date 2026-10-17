// Package cli implements the llm-loadtest command line: the run, mock,
// compare and version subcommands.
package cli

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

// Exit codes.
const (
	exitOK          = 0
	exitFailure     = 1   // the run failed, or compare found regressions
	exitUsage       = 2   // invalid flags or arguments
	exitInterrupted = 130 // stopped by SIGINT or SIGTERM; partial results written
)

// Env is the environment a command runs in. Tests substitute every field.
type Env struct {
	// Stdout receives results; Stderr receives progress and diagnostics.
	Stdout, Stderr io.Writer
	// StderrIsTerminal enables the in-place progress line.
	StderrIsTerminal bool
	// Getenv reads environment variables, such as the API key.
	Getenv func(string) string
	// Version is the build version; empty means "use build info".
	Version string
}

const usage = `llm-loadtest load-tests OpenAI-compatible streaming chat-completion endpoints.

Usage:
  llm-loadtest run --base-url URL --model NAME [flags]   run a load test
  llm-loadtest mock [flags]                              start a mock server
  llm-loadtest compare [--threshold PCT] BASE HEAD       diff two JSON results
  llm-loadtest version                                   print the version

Run "llm-loadtest <command> -h" for the flags of a command.
`

// Main runs the command line args (without the program name) and returns
// the process exit code.
func Main(ctx context.Context, args []string, env Env) int {
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	env.Version = resolveVersion(env.Version)
	if len(args) == 0 {
		_, _ = fmt.Fprint(env.Stderr, usage)
		return exitUsage
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "run":
		return runCommand(ctx, rest, &env)
	case "mock":
		return mockCommand(ctx, rest, &env)
	case "compare":
		return compareCommand(rest, &env)
	case "version", "--version", "-version":
		_, _ = fmt.Fprintf(env.Stdout, "llm-loadtest %s (%s %s/%s)\n", env.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return exitOK
	case "help", "-h", "-help", "--help":
		_, _ = fmt.Fprint(env.Stdout, usage)
		return exitOK
	default:
		_, _ = fmt.Fprintf(env.Stderr, "llm-loadtest: unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
}

// resolveVersion prefers a version set at link time (GoReleaser), then the
// module version recorded by "go install module@version".
func resolveVersion(linked string) string {
	if linked != "" {
		return linked
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return "dev"
}

// usageError reports a bad invocation of a subcommand.
func usageError(env *Env, cmd string, err error) int {
	_, _ = fmt.Fprintf(env.Stderr, "llm-loadtest %s: %v\nRun \"llm-loadtest %s -h\" for usage.\n", cmd, err, cmd)
	return exitUsage
}

// failure reports an error that stopped a subcommand.
func failure(env *Env, cmd string, err error) int {
	_, _ = fmt.Fprintf(env.Stderr, "llm-loadtest %s: %v\n", cmd, err)
	return exitFailure
}
