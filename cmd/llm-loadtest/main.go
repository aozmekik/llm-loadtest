// Command llm-loadtest load-tests OpenAI-compatible streaming
// chat-completion endpoints and reports time to first token, inter-token
// latency, end-to-end latency and throughput.
//
// Run "llm-loadtest -h" for usage.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/aozmekik/llm-loadtest/internal/cli"
)

// version is set at link time by GoReleaser; see .goreleaser.yaml.
var version = ""

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], cli.Env{
		Stdout:           os.Stdout,
		Stderr:           os.Stderr,
		StderrIsTerminal: isTerminal(os.Stderr),
		Getenv:           os.Getenv,
		Version:          version,
	})
	stop()
	os.Exit(code)
}

// isTerminal reports whether f is a character device such as a terminal.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
