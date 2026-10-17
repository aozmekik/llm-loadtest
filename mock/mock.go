// Package mock implements an OpenAI-compatible chat-completions server
// whose latency and failures are drawn from configurable distributions.
//
// It serves POST /v1/chat/completions (streaming and non-streaming) and
// GET /v1/models, and needs no GPU or model: generated text is
// pseudo-random words. Use it to test streaming clients and load
// generators, to rehearse a benchmark before running it against a real
// engine, or with net/http/httptest in unit tests:
//
//	srv, _ := mock.New(mock.Config{
//		TTFT:         mock.Normal(80*time.Millisecond, 10*time.Millisecond),
//		ITL:          mock.Constant(15 * time.Millisecond),
//		OutputTokens: mock.Uniform(32, 128),
//	})
//	ts := httptest.NewServer(srv)
//	defer ts.Close()
//	// point an OpenAI client at ts.URL + "/v1"
//
// Each request draws its latencies from a random source seeded by
// Config.Seed and the request's sequence number, so a sequential run is
// reproducible.
package mock

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// defaultMaxTokens is the output length when neither Config.OutputTokens
// nor the request's max_tokens sets one.
const defaultMaxTokens = 16

// maxRequestBody limits the size of a request body.
const maxRequestBody = 8 << 20

// Config configures a Server. The zero value is a valid instant server.
type Config struct {
	// Model is the model name listed by /v1/models. Completions echo the
	// requested model. Default "mock".
	Model string
	// TTFT is the delay from accepting a request (after waiting for a slot)
	// to generating its first token.
	TTFT Dist[time.Duration]
	// ITL is the delay between consecutive generated tokens.
	ITL Dist[time.Duration]
	// OutputTokens is the number of tokens to generate, capped by the
	// request's max_tokens. When it yields zero (as the zero Dist does),
	// the server generates max_tokens tokens, or 16 if the request has none.
	OutputTokens Dist[int]
	// TokensPerChunk groups tokens into stream chunks, the way servers that
	// batch output do. A chunk is sent when its last token is generated.
	// Values below 1 mean 1.
	TokensPerChunk int
	// Slots limits how many requests generate concurrently; later requests
	// wait for a free slot, and the wait adds to their time to first token.
	// Zero means unlimited.
	Slots int
	// ErrorRate is the fraction of requests rejected with ErrorStatus
	// before any output.
	ErrorRate float64
	// ErrorStatus is the HTTP status used by ErrorRate. Default 500.
	ErrorStatus int
	// StreamErrorRate is the fraction of requests that send an error
	// payload part-way through the stream.
	StreamErrorRate float64
	// AbortRate is the fraction of requests whose connection is dropped
	// part-way through the stream.
	AbortRate float64
	// DisableUsage ignores stream_options.include_usage, like servers that
	// do not support it.
	DisableUsage bool
	// Seed seeds the per-request random sources.
	Seed uint64
}

// Validate reports whether the configuration is usable.
func (c *Config) Validate() error {
	for name, rate := range map[string]float64{
		"error rate": c.ErrorRate, "stream error rate": c.StreamErrorRate, "abort rate": c.AbortRate,
	} {
		if !(rate >= 0 && rate <= 1) {
			return fmt.Errorf("mock: %s %v is outside [0, 1]", name, rate)
		}
	}
	if sum := c.ErrorRate + c.StreamErrorRate + c.AbortRate; sum > 1 {
		return fmt.Errorf("mock: error, stream error and abort rates sum to %v, more than 1", sum)
	}
	if c.ErrorStatus != 0 && (c.ErrorStatus < 400 || c.ErrorStatus > 599) {
		return fmt.Errorf("mock: error status %d is not a 4xx or 5xx code", c.ErrorStatus)
	}
	if c.Slots < 0 {
		return errors.New("mock: slots must not be negative")
	}
	return nil
}

// Server is an http.Handler serving the mock API. It is safe for
// concurrent use.
type Server struct {
	cfg   Config
	mux   *http.ServeMux
	slots chan struct{}
	seq   atomic.Uint64
}

// New returns a Server for cfg.
func New(cfg Config) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.Model = cmp.Or(cfg.Model, "mock")
	cfg.ErrorStatus = cmp.Or(cfg.ErrorStatus, http.StatusInternalServerError)
	cfg.TokensPerChunk = max(cfg.TokensPerChunk, 1)
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	if cfg.Slots > 0 {
		s.slots = make(chan struct{}, cfg.Slots)
	}
	s.mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	s.mux.HandleFunc("GET /v1/models", s.handleModels)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no route for "+r.Method+" "+r.URL.Path)
	})
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	MaxTokens           *int `json:"max_tokens"`
	MaxCompletionTokens *int `json:"max_completion_tokens"`
	Stream              bool `json:"stream"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// failure is the fault injected into one request.
type failure uint8

const (
	failNone failure = iota
	failStatus
	failStream
	failAbort
)

// plan is everything random about one request, drawn up front.
type plan struct {
	rng          *rand.Rand
	id           string
	model        string
	tokens       int
	finish       string
	promptTokens int
	failure      failure
	failAt       int // tokens generated before a mid-stream failure
	usage        bool
}

func (s *Server) newPlan(req *chatRequest) *plan {
	seq := s.seq.Add(1)
	rng := rand.New(rand.NewPCG(s.cfg.Seed, seq))
	p := &plan{
		rng:   rng,
		id:    fmt.Sprintf("chatcmpl-mock-%d", seq),
		model: req.Model,
		usage: req.StreamOptions != nil && req.StreamOptions.IncludeUsage && !s.cfg.DisableUsage,
	}
	for _, m := range req.Messages {
		p.promptTokens += countWords(m.Content)
	}

	limit := 0
	if req.MaxCompletionTokens != nil {
		limit = *req.MaxCompletionTokens
	}
	if req.MaxTokens != nil {
		limit = *req.MaxTokens
	}
	p.tokens = s.cfg.OutputTokens.Sample(rng)
	switch {
	case p.tokens <= 0:
		p.tokens = cmp.Or(max(limit, 0), defaultMaxTokens)
	case limit > 0:
		p.tokens = min(p.tokens, limit)
	}
	p.finish = "stop"
	if limit > 0 && p.tokens == limit {
		p.finish = "length"
	}

	u := rng.Float64()
	switch {
	case u < s.cfg.ErrorRate:
		p.failure = failStatus
	case u < s.cfg.ErrorRate+s.cfg.StreamErrorRate:
		p.failure = failStream
	case u < s.cfg.ErrorRate+s.cfg.StreamErrorRate+s.cfg.AbortRate:
		p.failure = failAbort
	}
	p.failAt = rng.IntN(max(p.tokens, 1))
	return p
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if req.Model == "" || len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model and messages are required")
		return
	}
	p := s.newPlan(&req)
	if p.failure == failStatus {
		writeError(w, s.cfg.ErrorStatus, "server_error", "mock: injected error")
		return
	}
	if s.slots != nil {
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		case <-r.Context().Done():
			return
		}
	}
	if req.Stream {
		s.stream(r.Context(), w, p)
	} else {
		s.complete(r.Context(), w, p)
	}
}

// tokenTimes returns when each token is generated, relative to start.
func (s *Server) tokenTimes(p *plan, start time.Time) []time.Time {
	times := make([]time.Time, p.tokens)
	t := start.Add(s.cfg.TTFT.Sample(p.rng))
	for i := range times {
		if i > 0 {
			t = t.Add(s.cfg.ITL.Sample(p.rng))
		}
		times[i] = t
	}
	return times
}

func (s *Server) stream(ctx context.Context, w http.ResponseWriter, p *plan) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	created := time.Now().Unix()
	var buf bytes.Buffer
	send := func(v any) bool {
		buf.Reset()
		buf.WriteString("data: ")
		if err := json.NewEncoder(&buf).Encode(v); err != nil {
			return false
		}
		buf.WriteString("\n") // Encode wrote the first newline
		if _, err := w.Write(buf.Bytes()); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	newChunk := func() *streamChunk {
		return &streamChunk{ID: p.id, Object: "chat.completion.chunk", Created: created, Model: p.model}
	}

	// Like real servers, announce the assistant role before any token.
	first := newChunk()
	first.Choices = []streamChoice{{Delta: delta{Role: "assistant", Content: new(string)}}}
	if !send(first) {
		return
	}

	times := s.tokenTimes(p, time.Now())
	if p.failure != failNone && p.tokens == 0 {
		fail(w, p, send)
		return
	}
	for start := 0; start < p.tokens; start += s.cfg.TokensPerChunk {
		end := min(start+s.cfg.TokensPerChunk, p.tokens)
		if p.failure != failNone && p.failAt < end {
			if sleepUntil(ctx, times[p.failAt]) {
				fail(w, p, send)
			}
			return
		}
		if !sleepUntil(ctx, times[end-1]) {
			return
		}
		text := words(p.rng, end-start)
		c := newChunk()
		c.Choices = []streamChoice{{Delta: delta{Content: &text}}}
		if !send(c) {
			return
		}
	}

	last := newChunk()
	last.Choices = []streamChoice{{Delta: delta{}, FinishReason: &p.finish}}
	if !send(last) {
		return
	}
	if p.usage {
		u := newChunk()
		u.Choices = []streamChoice{}
		u.Usage = p.usageObject()
		if !send(u) {
			return
		}
	}
	if _, err := w.Write([]byte("data: [DONE]\n\n")); err == nil {
		_ = rc.Flush()
	}
}

// fail injects a mid-stream stream error or connection abort.
func fail(w http.ResponseWriter, p *plan, send func(any) bool) {
	if p.failure == failAbort {
		// The documented way to make net/http drop the connection.
		panic(http.ErrAbortHandler)
	}
	if send(errorBody("server_error", "mock: injected stream error")) {
		if _, err := w.Write([]byte("data: [DONE]\n\n")); err == nil {
			_ = http.NewResponseController(w).Flush()
		}
	}
}

func (s *Server) complete(ctx context.Context, w http.ResponseWriter, p *plan) {
	times := s.tokenTimes(p, time.Now())
	if p.failure == failAbort {
		panic(http.ErrAbortHandler)
	}
	if len(times) > 0 && !sleepUntil(ctx, times[len(times)-1]) {
		return
	}
	if p.failure == failStream {
		writeError(w, http.StatusInternalServerError, "server_error", "mock: injected error")
		return
	}
	text := words(p.rng, p.tokens)
	resp := completion{
		ID: p.id, Object: "chat.completion", Created: time.Now().Unix(), Model: p.model,
		Choices: []completionChoice{{Message: message{Role: "assistant", Content: text}, FinishReason: p.finish}},
		Usage:   p.usageObject(),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   []map[string]any{{"id": s.cfg.Model, "object": "model", "created": 0, "owned_by": "llm-loadtest"}},
	})
}

func (p *plan) usageObject() *usage {
	return &usage{PromptTokens: p.promptTokens, CompletionTokens: p.tokens, TotalTokens: p.promptTokens + p.tokens}
}

func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// countWords approximates the token count of a message content, which is
// either a string or an array of content parts.
func countWords(content json.RawMessage) int {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return len(strings.Fields(s))
	}
	return len(strings.Fields(string(content)))
}

var vocabulary = strings.Fields(`the of and to in is that for it as was with be by on not
he this are or his from at which but have an they you were her she there one all we
their can has more will would so if out about up what when time other them some into
two very after first new most then years could only these may such over people also
than any where much just well state through between back even under good know way
long day great water small house world place light point line hand high story`)

// words returns n pseudo-random words, each with a leading space, roughly
// one token apiece for common tokenizers.
func words(r *rand.Rand, n int) string {
	var b strings.Builder
	for range n {
		b.WriteByte(' ')
		b.WriteString(vocabulary[r.IntN(len(vocabulary))])
	}
	return b.String()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "mock: encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeError(w http.ResponseWriter, status int, kind, msg string) {
	body := errorBody(kind, msg)
	body.Error.Code = status
	writeJSON(w, status, body)
}

func errorBody(kind, msg string) *errorResponse {
	return &errorResponse{Error: apiError{Message: msg, Type: kind}}
}

type (
	streamChunk struct {
		ID      string         `json:"id"`
		Object  string         `json:"object"`
		Created int64          `json:"created"`
		Model   string         `json:"model"`
		Choices []streamChoice `json:"choices"`
		Usage   *usage         `json:"usage,omitempty"`
	}
	streamChoice struct {
		Index        int     `json:"index"`
		Delta        delta   `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}
	delta struct {
		Role    string  `json:"role,omitempty"`
		Content *string `json:"content,omitempty"`
	}
	usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	}
	completion struct {
		ID      string             `json:"id"`
		Object  string             `json:"object"`
		Created int64              `json:"created"`
		Model   string             `json:"model"`
		Choices []completionChoice `json:"choices"`
		Usage   *usage             `json:"usage"`
	}
	completionChoice struct {
		Index        int     `json:"index"`
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}
	message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	errorResponse struct {
		Error apiError `json:"error"`
	}
	apiError struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    int    `json:"code,omitempty"`
	}
)
