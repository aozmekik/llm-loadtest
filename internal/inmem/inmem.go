// Package inmem provides an http.RoundTripper that serves each request by
// calling an http.Handler directly, with the response body streamed through
// an in-memory pipe.
//
// Unlike a loopback server it uses no sockets, so it works inside a
// testing/synctest bubble. With the bubble's fake clock, latencies injected
// by a handler are observed exactly by the client, which makes timing tests
// deterministic.
package inmem

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
)

// ErrAborted is returned by RoundTrip when the handler aborted with
// http.ErrAbortHandler before writing a response.
var ErrAborted = errors.New("inmem: handler aborted the connection")

// Transport is an http.RoundTripper backed by Handler.
type Transport struct {
	// Handler serves every request.
	Handler http.Handler
}

// RoundTrip implements http.RoundTripper. It returns once the handler has
// written the response header; the body streams while the handler runs.
// A handler that panics with http.ErrAbortHandler mid-response makes body
// reads fail with io.ErrUnexpectedEOF, as a dropped connection would.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	rw := &responseWriter{header: http.Header{}, body: pw, ready: make(chan struct{})}
	srvReq := req.Clone(req.Context())
	if srvReq.Body == nil {
		srvReq.Body = http.NoBody
	}
	go rw.serve(t.Handler, srvReq)

	select {
	case <-rw.ready:
	case <-req.Context().Done():
		_ = pr.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
	if rw.aborted {
		_ = pr.Close()
		return nil, ErrAborted
	}
	// Like http.Transport, fail body reads once the request is canceled.
	stop := context.AfterFunc(req.Context(), func() { _ = pw.CloseWithError(req.Context().Err()) })
	return &http.Response{
		Status:        http.StatusText(rw.status),
		StatusCode:    rw.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        rw.sent,
		Body:          &body{PipeReader: pr, stop: stop},
		ContentLength: -1,
		Request:       req,
	}, nil
}

// body is the client's side of the pipe.
type body struct {
	*io.PipeReader
	stop func() bool
}

// Close implements io.Closer.
func (b *body) Close() error {
	b.stop()
	return b.PipeReader.Close()
}

// responseWriter is the handler's side of the pipe.
type responseWriter struct {
	header http.Header
	body   *io.PipeWriter

	once    sync.Once
	ready   chan struct{} // closed once the header is written or the handler aborts
	status  int
	sent    http.Header // header snapshot taken at WriteHeader
	aborted bool
}

func (rw *responseWriter) serve(h http.Handler, req *http.Request) {
	defer func() { _ = req.Body.Close() }()
	defer func() {
		if v := recover(); v != nil {
			rw.once.Do(func() {
				rw.aborted = true
				close(rw.ready)
			})
			_ = rw.body.CloseWithError(io.ErrUnexpectedEOF)
			if v != http.ErrAbortHandler { //nolint:errorlint // ErrAbortHandler is compared by identity, as net/http does.
				panic(v)
			}
			return
		}
		rw.WriteHeader(http.StatusOK)
		_ = rw.body.Close()
	}()
	h.ServeHTTP(rw, req)
}

// Header implements http.ResponseWriter.
func (rw *responseWriter) Header() http.Header { return rw.header }

// WriteHeader implements http.ResponseWriter. Only the first call counts.
func (rw *responseWriter) WriteHeader(code int) {
	rw.once.Do(func() {
		rw.status = code
		rw.sent = rw.header.Clone()
		close(rw.ready)
	})
}

// Write implements http.ResponseWriter. It blocks until the client reads
// the bytes, like a connection with no buffering.
func (rw *responseWriter) Write(p []byte) (int, error) {
	rw.WriteHeader(http.StatusOK)
	return rw.body.Write(p)
}

// Flush implements http.Flusher. Writes are unbuffered, so it does nothing.
func (*responseWriter) Flush() {}
