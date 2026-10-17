package cli

import (
	"bytes"
	"flag"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aozmekik/llm-loadtest/internal/loadgen"
)

func TestListFlags(t *testing.T) {
	t.Parallel()
	var ints intList
	if err := ints.Set("1, 2,4"); err != nil || !slices.Equal(ints, intList{1, 2, 4}) || ints.String() != "1,2,4" {
		t.Errorf("intList = %v, %v", ints, err)
	}
	for _, bad := range []string{"", "0", "-1", "1,,2", "x"} {
		if err := ints.Set(bad); err == nil {
			t.Errorf("intList.Set(%q) succeeded", bad)
		}
	}
	var floats floatList
	if err := floats.Set("0.5,2"); err != nil || !slices.Equal(floats, floatList{0.5, 2}) || floats.String() != "0.5,2" {
		t.Errorf("floatList = %v, %v", floats, err)
	}
	for _, bad := range []string{"0", "-2", "NaN", "Inf", "fast"} {
		if err := floats.Set(bad); err == nil {
			t.Errorf("floatList.Set(%q) succeeded", bad)
		}
	}
}

func TestHeaderFlag(t *testing.T) {
	t.Parallel()
	var h headerList
	if h.String() != "" {
		t.Error("empty headerList String() is not empty")
	}
	for _, s := range []string{"x-tenant: acme", "Authorization:Bearer abc", "X-Tenant: other"} {
		if err := h.Set(s); err != nil {
			t.Fatalf("Set(%q): %v", s, err)
		}
	}
	if got := h.h.Values("X-Tenant"); !slices.Equal(got, []string{"acme", "other"}) {
		t.Errorf("X-Tenant = %v", got)
	}
	if h.h.Get("Authorization") != "Bearer abc" || h.String() != "Authorization, X-Tenant" {
		t.Errorf("headers = %v, String() = %q", h.h, h.String())
	}
	for _, bad := range []string{"novalue", ": empty name", "bad name: v"} {
		if err := h.Set(bad); err == nil {
			t.Errorf("Set(%q) succeeded", bad)
		}
	}
}

func TestOptionalFloat(t *testing.T) {
	t.Parallel()
	var o optionalFloat
	if o.String() != "" || o.v != nil {
		t.Error("unset optionalFloat is not empty")
	}
	if err := o.Set("0"); err != nil || o.v == nil || *o.v != 0 || o.String() != "0" {
		t.Errorf("Set(0): %v, %v", o.v, err)
	}
	if err := o.Set("NaN"); err == nil {
		t.Error("Set(NaN) succeeded")
	}
}

func TestParseInterleaved(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		pos  []string
		flag float64
	}{
		{[]string{"a", "b"}, []string{"a", "b"}, 0},
		{[]string{"-x", "1", "a", "b"}, []string{"a", "b"}, 1},
		{[]string{"a", "-x", "2", "b"}, []string{"a", "b"}, 2},
		{[]string{"a", "b", "--x=3"}, []string{"a", "b"}, 3},
		{[]string{"--", "-a", "-x"}, []string{"-a", "-x"}, 0},
		{[]string{"a", "--", "-x", "4"}, []string{"a", "-x", "4"}, 0},
	}
	for _, tt := range tests {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		x := fs.Float64("x", 0, "")
		pos, err := parseInterleaved(fs, tt.args)
		if err != nil || !slices.Equal(pos, tt.pos) || *x != tt.flag {
			t.Errorf("parseInterleaved(%q) = %q, x=%v, %v; want %q, x=%v", tt.args, pos, *x, err, tt.pos, tt.flag)
		}
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if _, err := parseInterleaved(fs, []string{"a", "-unknown"}); err == nil {
		t.Error("unknown flag after a positional argument was accepted")
	}
}

func TestProgressPrinter(t *testing.T) {
	t.Parallel()
	var tty bytes.Buffer
	p := &progressPrinter{w: &tty, tty: true}
	p.begin("[1/1] concurrency=2")
	p.update(loadgen.Progress{Elapsed: 2 * time.Second, Completed: 10, Failed: 2, InFlight: 2, OutputTokens: 400})
	p.end()
	want := "\r\x1b[2K[1/1] concurrency=2     2.0s  done 10  failed 2  in-flight 2  4.00 req/s  200 tok/s" +
		"\r\x1b[2K[1/1] concurrency=2     2.0s  done 10  failed 2  in-flight 2  4.00 req/s  200 tok/s\n"
	if tty.String() != want {
		t.Errorf("terminal output = %q\nwant %q", tty.String(), want)
	}

	var log bytes.Buffer
	p = &progressPrinter{w: &log}
	p.begin("stage")
	for s := 1; s <= 25; s++ {
		p.update(loadgen.Progress{Elapsed: time.Duration(s) * time.Second, Completed: s})
	}
	p.end()
	// Lines at 10s and 20s, then the final line at 25s.
	if lines := strings.Split(strings.TrimSpace(log.String()), "\n"); len(lines) != 3 || !strings.Contains(lines[2], "25.0s") {
		t.Errorf("log output:\n%s", log.String())
	}
}
