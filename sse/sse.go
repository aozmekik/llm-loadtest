// Package sse decodes Server-Sent Events (SSE) streams.
//
// The decoder implements the parsing rules of the WHATWG HTML Living
// Standard, section 9.2.6 "Interpreting an event stream": lines may end in
// CRLF, LF or a lone CR; lines starting with a colon are comments; "data"
// fields accumulate into a multi-line payload; an empty line dispatches the
// event; and an incomplete event at end of stream is discarded.
//
// The decoder is designed for latency measurement. It never waits for more
// input than it needs: an event is returned as soon as the line that ends it
// has been read, even when that line ends in a CR whose possible LF partner
// has not arrived yet.
//
// The decoder works on bytes and does not replace invalid UTF-8 sequences
// with U+FFFD as a browser would. Line terminators are ASCII, so splitting
// is safe for any input.
package sse

import (
	"bytes"
	"errors"
	"io"
	"math"
	"strconv"
	"time"
)

// DefaultMaxSize is the default limit, in bytes, on the length of a single
// line and on the accumulated data of a single event.
const DefaultMaxSize = 1 << 20

// ErrTooLarge is returned when a line or the data of an event exceeds the
// decoder's size limit.
var ErrTooLarge = errors.New("sse: line or event exceeds size limit")

var errInvalidRead = errors.New("sse: reader returned an invalid byte count")

// utf8BOM is the byte order mark stripped from the start of a stream.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// maxEmptyReads bounds consecutive (0, nil) reads before giving up, the same
// guard bufio uses against misbehaving readers.
const maxEmptyReads = 100

// Event is a dispatched server-sent event.
type Event struct {
	// Type is the value of the event's "event" field, or "message" when the
	// event had none.
	Type string
	// Data is the event payload: the values of its "data" fields joined by
	// "\n".
	Data string
	// ID is the stream's last event ID at the time of dispatch. As in the
	// specification it persists across events until the server changes it.
	ID string
}

// Decoder reads events from an SSE stream. A Decoder is not safe for
// concurrent use.
type Decoder struct {
	r       io.Reader
	maxSize int

	buf        []byte
	start, end int   // unconsumed input is buf[start:end]
	readErr    error // sticky error from r, reported once buf is drained
	bomDone    bool  // the leading BOM check has been made
	skipLF     bool  // the previous line ended in CR; drop one leading LF

	data      []byte // data buffer
	eventType string // event type buffer
	idBuf     string // last event ID buffer
	retry     time.Duration
}

// NewDecoder returns a Decoder that reads from r with a size limit of
// DefaultMaxSize.
func NewDecoder(r io.Reader) *Decoder {
	return NewDecoderSize(r, DefaultMaxSize)
}

// NewDecoderSize returns a Decoder that reads from r and rejects lines or
// events larger than maxSize bytes with ErrTooLarge. A maxSize below 64 is
// raised to 64.
func NewDecoderSize(r io.Reader, maxSize int) *Decoder {
	maxSize = max(maxSize, 64)
	return &Decoder{
		r:       r,
		maxSize: maxSize,
		buf:     make([]byte, min(4096, maxSize)),
	}
}

// Retry returns the reconnection time most recently set by a "retry" field,
// or zero if the stream has not set one.
func (d *Decoder) Retry() time.Duration { return d.retry }

// Next reads input until the next event is dispatched and returns it.
//
// At the end of the stream Next returns io.EOF; an event that was not
// terminated by an empty line is discarded, as the specification requires.
// Other errors from the underlying reader are returned unchanged. Once Next
// returns an error, every later call returns the same error.
func (d *Decoder) Next() (Event, error) {
	for {
		line, err := d.readLine()
		if err != nil {
			return Event{}, err
		}
		if len(line) == 0 {
			if ev, ok := d.dispatch(); ok {
				return ev, nil
			}
			continue
		}
		if err := d.processLine(line); err != nil {
			d.readErr = err
			d.start, d.end = 0, 0
			return Event{}, err
		}
	}
}

// dispatch implements the "dispatch the event" steps. It reports false when
// the data buffer is empty, in which case no event is produced.
func (d *Decoder) dispatch() (Event, bool) {
	if len(d.data) == 0 {
		d.eventType = ""
		return Event{}, false
	}
	data := d.data
	if data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	ev := Event{Type: "message", Data: string(data), ID: d.idBuf}
	if d.eventType != "" {
		ev.Type = d.eventType
	}
	d.data = d.data[:0]
	d.eventType = ""
	return ev, true
}

// processLine interprets one non-empty line.
func (d *Decoder) processLine(line []byte) error {
	if line[0] == ':' {
		return nil // comment
	}
	field, value, _ := bytes.Cut(line, []byte{':'})
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	switch string(field) {
	case "event":
		d.eventType = string(value)
	case "data":
		if len(d.data)+len(value)+1 > d.maxSize {
			return ErrTooLarge
		}
		d.data = append(d.data, value...)
		d.data = append(d.data, '\n')
	case "id":
		if bytes.IndexByte(value, 0) < 0 {
			d.idBuf = string(value)
		}
	case "retry":
		if isASCIIDigits(value) {
			if ms, err := strconv.ParseInt(string(value), 10, 64); err == nil && ms <= math.MaxInt64/int64(time.Millisecond) {
				d.retry = time.Duration(ms) * time.Millisecond
			}
		}
	}
	return nil
}

func isASCIIDigits(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// readLine returns the next complete line without its terminator. The
// returned slice aliases the internal buffer and is valid until the next
// call. A trailing partial line at end of input is dropped and the reader's
// error is returned instead.
func (d *Decoder) readLine() ([]byte, error) {
	for {
		pending := d.buf[d.start:d.end]

		if !d.bomDone {
			if len(pending) < len(utf8BOM) && bytes.HasPrefix(utf8BOM, pending) && d.readErr == nil {
				d.fill()
				continue
			}
			if bytes.HasPrefix(pending, utf8BOM) {
				d.start += len(utf8BOM)
			}
			d.bomDone = true
			continue
		}

		if d.skipLF && len(pending) > 0 {
			d.skipLF = false
			if pending[0] == '\n' {
				d.start++
			}
			continue
		}

		if i := bytes.IndexAny(pending, "\r\n"); i >= 0 {
			line := pending[:i]
			d.skipLF = pending[i] == '\r'
			d.start += i + 1
			return line, nil
		}

		if d.readErr != nil {
			return nil, d.readErr
		}
		d.fill()
	}
}

// fill reads more input into the buffer, compacting and growing it as
// needed. Errors, including ErrTooLarge, are recorded in d.readErr so that
// input already buffered is processed before the error is reported.
func (d *Decoder) fill() {
	if d.start > 0 {
		n := copy(d.buf, d.buf[d.start:d.end])
		d.start, d.end = 0, n
	}
	if d.end == len(d.buf) {
		if len(d.buf) >= d.maxSize {
			d.readErr = ErrTooLarge
			d.start, d.end = 0, 0
			return
		}
		grown := make([]byte, min(2*len(d.buf), d.maxSize))
		copy(grown, d.buf[:d.end])
		d.buf = grown
	}
	for range maxEmptyReads {
		n, err := d.r.Read(d.buf[d.end:])
		if n < 0 || n > len(d.buf)-d.end {
			d.readErr = errInvalidRead
			return
		}
		d.end += n
		d.readErr = err
		if err != nil || n > 0 {
			return
		}
	}
	d.readErr = io.ErrNoProgress
}
