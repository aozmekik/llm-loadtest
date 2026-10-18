package sse_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/aozmekik/llm-loadtest/sse"
)

// decodeAll drains d and returns its events and the terminating error.
func decodeAll(d *sse.Decoder) ([]sse.Event, error) {
	var events []sse.Event
	for {
		ev, err := d.Next()
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
}

func msg(data string) sse.Event { return sse.Event{Type: "message", Data: data} }

func TestDecoder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  []sse.Event
	}{
		{"empty stream", "", nil},
		{"single event LF", "data: hello\n\n", []sse.Event{msg("hello")}},
		{"single event CRLF", "data: hello\r\n\r\n", []sse.Event{msg("hello")}},
		{"single event CR", "data: hello\r\r", []sse.Event{msg("hello")}},
		{"mixed terminators", "data: a\r\n\ndata: b\r\rdata: c\n\r\n", []sse.Event{msg("a"), msg("b"), msg("c")}},
		{"multi-line data", "data: first\ndata: second\ndata:\ndata: third\n\n", []sse.Event{msg("first\nsecond\n\nthird")}},
		{"no space after colon", "data:x\n\n", []sse.Event{msg("x")}},
		{"only one leading space stripped", "data:  two\n\n", []sse.Event{msg(" two")}},
		{"field without colon", "data\n\n", []sse.Event{msg("")}},
		{"colon in value kept", "data: a:b: c\n\n", []sse.Event{msg("a:b: c")}},
		{"comments ignored", ": keep-alive\n:\ndata: x\n: trailing\n\n", []sse.Event{msg("x")}},
		{"comment-only stream", ": ping\n\n: ping\n\n", nil},
		{"event type", "event: error\ndata: boom\n\n", []sse.Event{{Type: "error", Data: "boom"}}},
		{"event type resets after dispatch", "event: a\ndata: 1\n\ndata: 2\n\n", []sse.Event{{Type: "a", Data: "1"}, msg("2")}},
		{"event type without data is dropped", "event: a\n\ndata: 2\n\n", []sse.Event{msg("2")}},
		{"empty event type means message", "event:\ndata: x\n\n", []sse.Event{msg("x")}},
		{"id persists", "id: 7\ndata: a\n\ndata: b\n\nid\ndata: c\n\n", []sse.Event{
			{Type: "message", Data: "a", ID: "7"},
			{Type: "message", Data: "b", ID: "7"},
			{Type: "message", Data: "c", ID: ""},
		}},
		{"id with NUL ignored", "id: 1\ndata: a\n\nid: x\x00y\ndata: b\n\n", []sse.Event{
			{Type: "message", Data: "a", ID: "1"},
			{Type: "message", Data: "b", ID: "1"},
		}},
		{"unknown fields ignored", "foo: bar\nDATA: no\ndata: yes\n\n", []sse.Event{msg("yes")}},
		{"BOM stripped", "\xEF\xBB\xBFdata: x\n\n", []sse.Event{msg("x")}},
		{"second BOM kept", "\xEF\xBB\xBF\xEF\xBB\xBFdata: x\n\ndata: y\n\n", []sse.Event{msg("y")}},
		{"incomplete event discarded", "data: a\n\ndata: b\n", []sse.Event{msg("a")}},
		{"unterminated line discarded", "data: a\n\ndata: b", []sse.Event{msg("a")}},
		{"blank lines between events", "\n\n\ndata: a\n\n\n\ndata: b\n\n", []sse.Event{msg("a"), msg("b")}},
		{"openai stream", "data: {\"x\":1}\n\ndata: [DONE]\n\n", []sse.Event{msg(`{"x":1}`), msg("[DONE]")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// The result must not depend on how the input is fragmented.
			readers := map[string]io.Reader{
				"whole":    strings.NewReader(tt.input),
				"one-byte": iotest.OneByteReader(strings.NewReader(tt.input)),
				"half":     iotest.HalfReader(strings.NewReader(tt.input)),
				"data-err": iotest.DataErrReader(strings.NewReader(tt.input)),
			}
			for name, r := range readers {
				got, err := decodeAll(sse.NewDecoder(r))
				if !errors.Is(err, io.EOF) {
					t.Errorf("%s: terminal error = %v, want io.EOF", name, err)
				}
				if !slices.Equal(got, tt.want) {
					t.Errorf("%s: events = %q, want %q", name, got, tt.want)
				}
			}
		})
	}
}

func TestDecoderRetry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"retry: 1500\n\n", 1500 * time.Millisecond},
		{"retry: 10\nretry: 1x\n\n", 10 * time.Millisecond},
		{"retry: -5\n\n", 0},
		{"retry:\n\n", 0},
		{"retry: 99999999999999999999999\n\n", 0},
	}
	for _, tt := range tests {
		d := sse.NewDecoder(strings.NewReader(tt.input))
		if _, err := decodeAll(d); !errors.Is(err, io.EOF) {
			t.Fatalf("%q: unexpected error %v", tt.input, err)
		}
		if got := d.Retry(); got != tt.want {
			t.Errorf("%q: Retry() = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// scriptedReader returns the given chunks one Read at a time and fails the
// test if the decoder asks for more input than it was given.
type scriptedReader struct {
	t      *testing.T
	chunks []string
}

var errReadPastScript = errors.New("read past the end of the script")

func (r *scriptedReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		r.t.Error("decoder read more input than needed to dispatch the event")
		return 0, errReadPastScript
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

// TestDecoderDispatchesWithoutLookahead checks the property latency
// measurement depends on: an event is returned as soon as its terminating
// blank line has been read, without waiting for a possible LF after a CR.
func TestDecoderDispatchesWithoutLookahead(t *testing.T) {
	t.Parallel()
	for _, chunks := range [][]string{
		{"data: a\n\n"},
		{"data: a\r\n\r"},
		{"data: a\r", "\n", "\r"},
		{"da", "ta", ": ", "a", "\n", "\n"},
	} {
		d := sse.NewDecoder(&scriptedReader{t: t, chunks: slices.Clone(chunks)})
		ev, err := d.Next()
		if err != nil || ev.Data != "a" {
			t.Errorf("chunks %q: Next() = %q, %v; want data \"a\"", chunks, ev, err)
		}
	}
}

// TestDecoderCRLFSplitAcrossReads checks that a CRLF pair split across two
// reads counts as one line terminator, not as a line followed by a blank
// line (which would dispatch early).
func TestDecoderCRLFSplitAcrossReads(t *testing.T) {
	t.Parallel()
	r := &chunkReader{chunks: []string{"data: a\r", "\ndata: b\r", "\n\r", "\n"}}
	got, err := decodeAll(sse.NewDecoder(r))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("error = %v, want io.EOF", err)
	}
	if want := []sse.Event{msg("a\nb")}; !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

type chunkReader struct{ chunks []string }

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

func TestDecoderReaderError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	r := io.MultiReader(strings.NewReader("data: a\n\ndata: partial\n"), iotest.ErrReader(boom))
	d := sse.NewDecoder(r)
	got, err := decodeAll(d)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want %v", err, boom)
	}
	if want := []sse.Event{msg("a")}; !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if _, err := d.Next(); !errors.Is(err, boom) {
		t.Fatalf("error is not sticky: second Next() = %v", err)
	}
}

func TestDecoderSizeLimit(t *testing.T) {
	t.Parallel()
	t.Run("long line", func(t *testing.T) {
		t.Parallel()
		input := "data: " + strings.Repeat("x", 200) + "\n\n"
		d := sse.NewDecoderSize(strings.NewReader(input), 100)
		if _, err := d.Next(); !errors.Is(err, sse.ErrTooLarge) {
			t.Fatalf("Next() error = %v, want ErrTooLarge", err)
		}
		if _, err := d.Next(); !errors.Is(err, sse.ErrTooLarge) {
			t.Fatalf("error is not sticky: second Next() = %v", err)
		}
	})
	t.Run("many data lines", func(t *testing.T) {
		t.Parallel()
		input := strings.Repeat("data: 0123456789\n", 20) + "\n"
		d := sse.NewDecoderSize(strings.NewReader(input), 100)
		if _, err := d.Next(); !errors.Is(err, sse.ErrTooLarge) {
			t.Fatalf("Next() error = %v, want ErrTooLarge", err)
		}
	})
	t.Run("limit is inclusive", func(t *testing.T) {
		t.Parallel()
		for _, maxSize := range []int{100, 5000} {
			lines := func(sizes ...int) string {
				var b strings.Builder
				for _, n := range sizes {
					b.WriteString("data:" + strings.Repeat("z", n-len("data:")) + "\r\n")
				}
				return b.String() + "\r\n"
			}
			// Line lengths include the "data:" prefix; data lengths add one
			// LF between lines.
			for _, tt := range []struct {
				input    string
				wantData int // -1 for ErrTooLarge
			}{
				{lines(maxSize), maxSize - 5},
				{lines(maxSize + 1), -1},
				{lines(maxSize/2+5, maxSize/2+4), maxSize},
				{lines(maxSize/2+5, maxSize/2+5), -1},
			} {
				for name, r := range map[string]io.Reader{
					"whole":    strings.NewReader(tt.input),
					"one-byte": iotest.OneByteReader(strings.NewReader(tt.input)),
				} {
					ev, err := sse.NewDecoderSize(r, maxSize).Next()
					switch {
					case tt.wantData < 0 && !errors.Is(err, sse.ErrTooLarge):
						t.Errorf("max %d, %s, %d bytes of input: error = %v, want ErrTooLarge", maxSize, name, len(tt.input), err)
					case tt.wantData >= 0 && (err != nil || len(ev.Data) != tt.wantData):
						t.Errorf("max %d, %s: got %d bytes of data, %v; want %d", maxSize, name, len(ev.Data), err, tt.wantData)
					}
				}
			}
		}
	})
	t.Run("within limit grows buffer", func(t *testing.T) {
		t.Parallel()
		payload := strings.Repeat("y", 10_000)
		d := sse.NewDecoderSize(strings.NewReader("data: "+payload+"\n\n"), 20_000)
		ev, err := d.Next()
		if err != nil || ev.Data != payload {
			t.Fatalf("Next() = %d bytes, %v; want %d bytes", len(ev.Data), err, len(payload))
		}
	})
}

type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, nil }

func TestDecoderNoProgress(t *testing.T) {
	t.Parallel()
	if _, err := sse.NewDecoder(zeroReader{}).Next(); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("Next() error = %v, want io.ErrNoProgress", err)
	}
}

// referenceDecode is a deliberately naive, non-streaming implementation of
// the same specification, used as an oracle by the fuzz test.
func referenceDecode(input string) []sse.Event {
	input = strings.TrimPrefix(input, "\uFEFF")
	var lines []string
	for {
		i := strings.IndexAny(input, "\r\n")
		if i < 0 {
			break // an unterminated final line is never processed
		}
		lines = append(lines, input[:i])
		if strings.HasPrefix(input[i:], "\r\n") {
			input = input[i+2:]
		} else {
			input = input[i+1:]
		}
	}
	var (
		events    []sse.Event
		data      strings.Builder
		eventType string
		lastID    string
	)
	for _, line := range lines {
		if line == "" {
			if data.Len() == 0 {
				eventType = ""
				continue
			}
			ev := sse.Event{Type: "message", Data: strings.TrimSuffix(data.String(), "\n"), ID: lastID}
			if eventType != "" {
				ev.Type = eventType
			}
			events = append(events, ev)
			data.Reset()
			eventType = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			eventType = value
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
		case "id":
			if !strings.Contains(value, "\x00") {
				lastID = value
			}
		}
	}
	return events
}

// splitReader serves input in chunks whose sizes are taken cyclically from
// sizes, so the fuzzer controls every chunk boundary.
type splitReader struct {
	input []byte
	sizes []byte
	i     int
}

func (r *splitReader) Read(p []byte) (int, error) {
	if len(r.input) == 0 {
		return 0, io.EOF
	}
	n := 1
	if len(r.sizes) > 0 {
		n = int(r.sizes[r.i%len(r.sizes)])%16 + 1
		r.i++
	}
	n = min(n, len(p), len(r.input))
	copy(p, r.input[:n])
	r.input = r.input[n:]
	return n, nil
}

func FuzzDecoder(f *testing.F) {
	seeds := []string{
		"data: hello\n\n",
		"data: a\r\n\r\ndata: b\r\r",
		"\xEF\xBB\xBFevent: e\nid: 1\ndata: x\ndata: y\n\n",
		": comment\ndata\n\nretry: 10\n\n",
		"data: {\"choices\":[]}\n\ndata: [DONE]\n\n",
		"data: a\r",
		"id: \x00\ndata:  z\n\n",
	}
	for i, s := range seeds {
		f.Add([]byte(s), []byte{byte(i), 3, 1})
	}
	f.Fuzz(func(t *testing.T, input, sizes []byte) {
		want := referenceDecode(string(input))
		got, err := decodeAll(sse.NewDecoderSize(&splitReader{input: input, sizes: sizes}, 1<<20))
		if !errors.Is(err, io.EOF) {
			t.Fatalf("terminal error = %v, want io.EOF", err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("input %q split by %v:\n got  %q\n want %q", input, sizes, got, want)
		}
	})
}

func BenchmarkDecoder(b *testing.B) {
	var stream bytes.Buffer
	for i := range 1000 {
		fmt.Fprintf(&stream, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m",`+
			`"choices":[{"index":0,"delta":{"content":" token%s"},"finish_reason":null}]}`+"\n\n", strconv.Itoa(i))
	}
	stream.WriteString("data: [DONE]\n\n")
	input := stream.Bytes()
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for b.Loop() {
		d := sse.NewDecoder(bytes.NewReader(input))
		for {
			if _, err := d.Next(); err != nil {
				break
			}
		}
	}
}
