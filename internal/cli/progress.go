package cli

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/aozmekik/llm-loadtest/internal/loadgen"
)

// nonTTYInterval is how often progress is logged when stderr is not a
// terminal, e.g. in CI logs.
const nonTTYInterval = 10 * time.Second

// progressPrinter renders the live progress line on stderr: rewritten in
// place on a terminal, appended at a slower pace otherwise.
type progressPrinter struct {
	w   io.Writer
	tty bool

	mu      sync.Mutex
	label   string
	last    loadgen.Progress
	logged  time.Duration // elapsed time of the last non-TTY line
	printed bool          // the last snapshot already has its own non-TTY line
}

// begin starts a new stage and labels its progress lines.
func (p *progressPrinter) begin(label string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.label, p.last, p.logged, p.printed = label, loadgen.Progress{}, 0, false
}

// update is the loadgen.Config.OnProgress callback.
func (p *progressPrinter) update(pr loadgen.Progress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = pr
	if p.tty {
		_, _ = fmt.Fprintf(p.w, "\r\x1b[2K%s", formatProgress(p.label, pr))
		return
	}
	p.printed = pr.Elapsed-p.logged >= nonTTYInterval
	if p.printed {
		_, _ = fmt.Fprintln(p.w, formatProgress(p.label, pr))
		p.logged = pr.Elapsed
	}
}

// end prints the stage's final progress line, unless the final snapshot
// was just logged on a line of its own.
func (p *progressPrinter) end() {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.tty:
		_, _ = fmt.Fprint(p.w, "\r\x1b[2K")
	case p.printed:
		return
	}
	_, _ = fmt.Fprintln(p.w, formatProgress(p.label, p.last))
}

func formatProgress(label string, pr loadgen.Progress) string {
	secs := pr.Elapsed.Seconds()
	var rps, tps float64
	if secs > 0 {
		rps = float64(pr.Completed-pr.Failed) / secs
		tps = float64(pr.OutputTokens) / secs
	}
	return fmt.Sprintf("%s  %6.1fs  done %d  failed %d  in-flight %d  %.2f req/s  %.0f tok/s",
		label, secs, pr.Completed, pr.Failed, pr.InFlight, rps, tps)
}
