package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aozmekik/llm-loadtest/internal/client"
	"github.com/aozmekik/llm-loadtest/internal/inmem"
	"github.com/aozmekik/llm-loadtest/mock"
)

const secret = "sk-test-SECRET-do-not-leak"

var userMsg = client.Request{Messages: []client.Message{{Role: "user", Content: "hello"}}}

// streamParts returns a handler that writes each part and flushes, so the
// client sees the given fragmentation.
func streamParts(parts ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		for _, p := range parts {
			if _, err := io.WriteString(w, p); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}
}

func newClient(t *testing.T, baseURL string, mutate func(*client.Config)) *client.Client {
	t.Helper()
	cfg := client.Config{BaseURL: baseURL, Model: "test-model", APIKey: secret, IncludeUsage: true}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := client.New(cfg)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

func serve(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func TestRequestShape(t *testing.T) {
	t.Parallel()
	var (
		gotBody   map[string]any
		gotHeader http.Header
		gotPath   string
	)
	ts := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHeader = r.URL.Path, r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		streamParts("data: [DONE]\n\n")(w, r)
	}))
	temp := 0.5
	c := newClient(t, ts.URL+"/v1/", func(cfg *client.Config) {
		cfg.MaxTokens = 64
		cfg.Temperature = &temp
		cfg.UserAgent = "llm-loadtest/test"
		cfg.Headers = http.Header{"x-tenant": {"acme"}}
		cfg.ExtraBody = map[string]any{"ignore_eos": true, "max_tokens": 99, "stream": false}
	})
	if _, err := c.Stream(t.Context(), client.Request{Messages: userMsg.Messages, MaxTokens: 7}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	want := map[string]any{
		"model":          "test-model",
		"messages":       []any{map[string]any{"role": "user", "content": "hello"}},
		"max_tokens":     99.0, // ExtraBody overrides the typed fields...
		"temperature":    0.5,
		"stream":         true, // ...except stream, which is always on.
		"stream_options": map[string]any{"include_usage": true},
		"ignore_eos":     true,
	}
	if g, w := mustJSON(t, gotBody), mustJSON(t, want); g != w {
		t.Errorf("body = %s\nwant   %s", g, w)
	}
	for k, v := range map[string]string{
		"Authorization": "Bearer " + secret, "Accept": "text/event-stream",
		"Content-Type": "application/json", "User-Agent": "llm-loadtest/test", "X-Tenant": "acme",
	} {
		if got := gotHeader.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	if enc := gotHeader.Get("Accept-Encoding"); enc != "" {
		t.Errorf("Accept-Encoding = %q, want none: compression distorts chunk timing", enc)
	}
}

func TestRequestDefaults(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	var gotAuth string
	ts := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		streamParts("data: [DONE]\n\n")(w, r)
	}))
	c := newClient(t, ts.URL, func(cfg *client.Config) {
		cfg.APIKey, cfg.IncludeUsage, cfg.MaxTokens = "", false, 32
	})
	if _, err := c.Stream(t.Context(), userMsg); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want none without an API key", gotAuth)
	}
	for _, absent := range []string{"temperature", "stream_options"} {
		if _, ok := gotBody[absent]; ok {
			t.Errorf("body has %q, want it omitted", absent)
		}
	}
	if gotBody["max_tokens"] != 32.0 {
		t.Errorf("max_tokens = %v, want the configured default 32", gotBody["max_tokens"])
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStreamParsing(t *testing.T) {
	t.Parallel()
	role := `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\r\n\r\n"
	tests := []struct {
		name       string
		parts      []string
		wantChunks int
		wantTokens int
		wantUsage  bool
		wantPrompt int
		wantFinish string
	}{
		{
			name: "fragmented CRLF stream with usage",
			parts: []string{
				": comment from a proxy\r\n\r\n",
				role[:20], role[20:],
				`data: {"choices":[{"delta":{"content":"Hel"}}]}` + "\r", "\n\r\n",
				`data: {"choices":[{"delta":{"content":"lo"}}]}` + "\r\n\r\n",
				`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\r\n\r\n",
				`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3}}` + "\r\n\r\n",
				"data: [DONE]\r\n\r\n",
			},
			wantChunks: 2, wantTokens: 3, wantUsage: true, wantPrompt: 9, wantFinish: "stop",
		},
		{
			name: "approximate count without usage",
			parts: []string{
				role,
				"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"c\"},\"finish_reason\":\"length\"}]}\n\n",
				"data: [DONE]\n\n",
			},
			wantChunks: 3, wantTokens: 3, wantFinish: "length",
		},
		{
			name: "reasoning and tool calls count as output",
			parts: []string{
				"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"reasoning\":\"more\"}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0}]}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":null,\"content\":null}}]}\n\n",
				"data: [DONE]\n\n",
			},
			wantChunks: 3, wantTokens: 3,
		},
		{
			name: "missing [DONE] tolerated after finish_reason",
			parts: []string{
				"data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":\"stop\"}]}\n\n",
			},
			wantChunks: 1, wantTokens: 1, wantFinish: "stop",
		},
		{
			name: "empty tool_calls in the role chunk is not output",
			parts: []string{
				"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"tool_calls\":[ ]}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":\"stop\"}]}\n\n",
				"data: [DONE]\n\n",
			},
			wantChunks: 1, wantTokens: 1, wantFinish: "stop",
		},
		{
			name: "named events ignored",
			parts: []string{
				"event: ping\ndata: {}\n\n",
				"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n",
				"data: [DONE]\n\n",
			},
			wantChunks: 1, wantTokens: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := serve(t, streamParts(tt.parts...))
			resp, err := newClient(t, ts.URL, nil).Stream(t.Context(), userMsg)
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if len(resp.Chunks) != tt.wantChunks || resp.OutputTokens != tt.wantTokens ||
				resp.UsageReported != tt.wantUsage || resp.PromptTokens != tt.wantPrompt || resp.FinishReason != tt.wantFinish {
				t.Errorf("got chunks=%d tokens=%d usage=%v prompt=%d finish=%q; want %d %d %v %d %q",
					len(resp.Chunks), resp.OutputTokens, resp.UsageReported, resp.PromptTokens, resp.FinishReason,
					tt.wantChunks, tt.wantTokens, tt.wantUsage, tt.wantPrompt, tt.wantFinish)
			}
			if resp.FirstToken.IsZero() || resp.FirstToken.Before(resp.Sent) || resp.End.Before(resp.FirstToken) {
				t.Errorf("timestamps out of order: sent %v first %v end %v", resp.Sent, resp.FirstToken, resp.End)
			}
			if !slices.IsSorted(resp.Chunks) {
				t.Errorf("chunk times not monotonic: %v", resp.Chunks)
			}
		})
	}
}

func TestErrorClasses(t *testing.T) {
	t.Parallel()
	jsonStatus := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}
	}
	tests := []struct {
		name      string
		handler   http.Handler
		wantClass client.Class
		wantMsg   string
	}{
		{"429 with OpenAI error", jsonStatus(429, `{"error":{"message":"Rate limit reached","type":"requests"}}`), "http_429", "Rate limit reached"},
		{"404 with string error", jsonStatus(404, `{"error":"model not found"}`), "http_404", "model not found"},
		{"400 with FastAPI detail", jsonStatus(400, `{"detail":"bad request body"}`), "http_400", "bad request body"},
		{"503 plain text", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "upstream   overloaded\n", http.StatusServiceUnavailable)
		}), "http_503", "upstream overloaded"},
		{"500 empty body", jsonStatus(500, ""), "http_500", "Internal Server Error"},
		{"JSON instead of stream", jsonStatus(200, `{"choices":[]}`), client.ClassProtocol, "application/json"},
		{"error payload mid-stream", streamParts(
			"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n",
			"data: {\"error\":{\"message\":\"CUDA out of memory\",\"code\":500}}\n\n",
			"data: [DONE]\n\n",
		), client.ClassStreamError, "CUDA out of memory"},
		{"vLLM error object", streamParts("data: {\"object\":\"error\",\"message\":\"engine dead\"}\n\n"), client.ClassStreamError, "engine dead"},
		{"error event", streamParts("event: error\ndata: overloaded\n\n"), client.ClassStreamError, "overloaded"},
		{"invalid JSON", streamParts("data: {not json}\n\n"), client.ClassProtocol, "invalid JSON"},
		{"oversized event", streamParts("data: " + strings.Repeat("x", 2<<20) + "\n\n"), client.ClassProtocol, "size limit"},
		{"stream cut short", streamParts("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"), client.ClassIncomplete, "before [DONE]"},
		{"connection dropped", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			streamParts("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")(w, r)
			panic(http.ErrAbortHandler)
		}), client.ClassConnection, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := serve(t, tt.handler)
			resp, err := newClient(t, ts.URL, nil).Stream(t.Context(), userMsg)
			var cerr *client.Error
			if !errors.As(err, &cerr) {
				t.Fatalf("error = %v (%T), want *client.Error", err, err)
			}
			if cerr.Class != tt.wantClass || !strings.Contains(cerr.Message, tt.wantMsg) {
				t.Errorf("error = %q (class %q), want class %q containing %q", cerr.Message, cerr.Class, tt.wantClass, tt.wantMsg)
			}
			if strings.HasPrefix(string(tt.wantClass), "http_") && cerr.StatusCode == 0 {
				t.Error("StatusCode not set for an HTTP error")
			}
			if resp.Sent.IsZero() || resp.End.IsZero() {
				t.Error("Response timestamps not recorded on failure")
			}
			if strings.Contains(err.Error(), secret) {
				t.Error("error message contains the API key")
			}
		})
	}
}

func TestConnectionRefused(t *testing.T) {
	t.Parallel()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	c := newClient(t, "http://"+addr+"/v1?api_key="+secret, nil)
	_, err = c.Stream(t.Context(), userMsg)
	var cerr *client.Error
	if !errors.As(err, &cerr) || cerr.Class != client.ClassConnection {
		t.Fatalf("error = %v, want class connection", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "api_key") {
		t.Errorf("error message leaks the URL or key: %q", err)
	}
}

// inmemClient returns a client served by h through the in-memory
// transport, for use inside a synctest bubble.
func inmemClient(t *testing.T, h http.Handler, mutate func(*client.Config)) *client.Client {
	t.Helper()
	return newClient(t, "http://inmem/v1", func(cfg *client.Config) {
		cfg.HTTPClient = &http.Client{Transport: &inmem.Transport{Handler: h}}
		if mutate != nil {
			mutate(cfg)
		}
	})
}

func newMock(t *testing.T, cfg mock.Config) *mock.Server {
	t.Helper()
	srv, err := mock.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestTimeouts(t *testing.T) {
	t.Parallel()
	slow := mock.Config{
		TTFT: mock.Constant(2 * time.Second), ITL: mock.Constant(100 * time.Millisecond), OutputTokens: mock.Constant(50),
	}
	tests := []struct {
		name      string
		cfg       func(*client.Config)
		wantClass client.Class
		wantEnd   time.Duration
	}{
		{"total timeout", func(c *client.Config) { c.Timeout = 3 * time.Second }, client.ClassTimeout, 3 * time.Second},
		{"idle timeout before first token", func(c *client.Config) { c.IdleTimeout = time.Second }, client.ClassIdleTimeout, time.Second},
		{"idle timeout not hit by steady stream", func(c *client.Config) {
			c.IdleTimeout = 2500 * time.Millisecond
			c.Timeout = 10 * time.Second
		}, "", 2*time.Second + 49*100*time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				c := inmemClient(t, newMock(t, slow), tt.cfg)
				start := time.Now()
				resp, err := c.Stream(t.Context(), userMsg)
				var cerr *client.Error
				switch {
				case tt.wantClass == "" && err != nil:
					t.Fatalf("unexpected error %v", err)
				case tt.wantClass != "" && (!errors.As(err, &cerr) || cerr.Class != tt.wantClass):
					t.Fatalf("error = %v, want class %q", err, tt.wantClass)
				}
				if got := resp.End.Sub(start); got != tt.wantEnd {
					t.Errorf("request ended after %v, want %v", got, tt.wantEnd)
				}
			})
		})
	}
}

// TestIdleTimeoutCountsHeaders checks that the idle timer restarts when the
// response headers arrive, not only when body bytes do.
func TestIdleTimeoutCountsHeaders(t *testing.T) {
	t.Parallel()
	pause := func(r *http.Request, d time.Duration) bool {
		select {
		case <-time.After(d):
			return true
		case <-r.Context().Done():
			return false
		}
	}
	handler := func(headerDelay time.Duration) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !pause(r, headerDelay) {
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if !pause(r, 600*time.Millisecond) {
				return
			}
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
	}
	idle := func(cfg *client.Config) { cfg.IdleTimeout = time.Second }
	synctest.Test(t, func(t *testing.T) {
		// Headers after 900ms, the body 600ms later: no gap reaches 1s.
		if _, err := inmemClient(t, handler(900*time.Millisecond), idle).Stream(t.Context(), userMsg); err != nil {
			t.Errorf("gaps of 900ms and 600ms with a 1s idle timeout: %v", err)
		}
		// Headers after 1.5s: the wait for them counts as idle time.
		resp, err := inmemClient(t, handler(1500*time.Millisecond), idle).Stream(t.Context(), userMsg)
		var cerr *client.Error
		if !errors.As(err, &cerr) || cerr.Class != client.ClassIdleTimeout || resp.End.Sub(resp.Sent) != time.Second {
			t.Errorf("headers after 1.5s: error %v after %v, want idle_timeout after 1s", err, resp.End.Sub(resp.Sent))
		}
	})
}

func TestCallerCancel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c := inmemClient(t, newMock(t, mock.Config{TTFT: mock.Constant(time.Hour)}), nil)
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(time.Second, cancel)
		_, err := c.Stream(ctx, userMsg)
		var cerr *client.Error
		if !errors.As(err, &cerr) || cerr.Class != client.ClassCanceled {
			t.Fatalf("error = %v, want class canceled", err)
		}
	})
}

// TestTimingExact checks recorded times against the mock's schedule using
// the fake clock: the client must add no latency of its own.
func TestTimingExact(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		srv := newMock(t, mock.Config{
			TTFT: mock.Constant(200 * time.Millisecond), ITL: mock.Constant(10 * time.Millisecond), OutputTokens: mock.Constant(5),
		})
		resp, err := inmemClient(t, srv, nil).Stream(t.Context(), userMsg)
		if err != nil {
			t.Fatal(err)
		}
		const ms = time.Millisecond
		want := []time.Duration{200 * ms, 210 * ms, 220 * ms, 230 * ms, 240 * ms}
		if !slices.Equal(resp.Chunks, want) {
			t.Errorf("chunk arrivals = %v, want %v", resp.Chunks, want)
		}
		if ttft := resp.FirstToken.Sub(resp.Sent); ttft != 200*time.Millisecond {
			t.Errorf("TTFT = %v, want 200ms (the role-only chunk at 0ms must not count)", ttft)
		}
		if !resp.UsageReported || resp.OutputTokens != 5 || resp.FinishReason != "stop" {
			t.Errorf("usage=%v tokens=%d finish=%q", resp.UsageReported, resp.OutputTokens, resp.FinishReason)
		}
	})
}

// TestTimingOverLoopback measures the mock over a real loopback connection.
// Wall-clock timing varies with machine load, so it asserts on robust
// statistics with tolerances: TTFT can only be late, never early, and the
// mean inter-chunk gap is pinned by the mock's absolute token schedule.
func TestTimingOverLoopback(t *testing.T) {
	t.Parallel()
	const (
		ttft   = 30 * time.Millisecond
		itl    = 5 * time.Millisecond
		tokens = 21
		runs   = 10
	)
	ts := serve(t, newMock(t, mock.Config{TTFT: mock.Constant(ttft), ITL: mock.Constant(itl), OutputTokens: mock.Constant(tokens)}))
	c := newClient(t, ts.URL+"/v1", nil)
	ttfts := make([]time.Duration, 0, runs)
	meanITLs := make([]time.Duration, 0, runs)
	for range runs {
		resp, err := c.Stream(t.Context(), userMsg)
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Chunks) != tokens {
			t.Fatalf("got %d chunks, want %d", len(resp.Chunks), tokens)
		}
		ttfts = append(ttfts, resp.FirstToken.Sub(resp.Sent))
		meanITLs = append(meanITLs, (resp.Chunks[tokens-1]-resp.Chunks[0])/(tokens-1))
	}
	slices.Sort(ttfts)
	slices.Sort(meanITLs)
	if med := ttfts[runs/2]; med < ttft || med > ttft+40*time.Millisecond {
		t.Errorf("median TTFT = %v, want within [%v, %v]", med, ttft, ttft+40*time.Millisecond)
	}
	if med := meanITLs[runs/2]; med < itl-time.Millisecond || med > itl+2*time.Millisecond {
		t.Errorf("median of per-request mean ITL = %v, want %v within -1ms/+2ms", med, itl)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	t.Parallel()
	var followed atomic.Bool
	target := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Store(true)
		streamParts("data: [DONE]\n\n")(w, r)
	}))
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		ts := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/v1/chat/completions?sig=redirect-secret", status)
		}))
		c := newClient(t, ts.URL, func(cfg *client.Config) {
			cfg.Headers = http.Header{"Api-Key": {"azure-style-secret-key"}}
		})
		_, err := c.Stream(t.Context(), userMsg)
		var cerr *client.Error
		if !errors.As(err, &cerr) || cerr.Class != client.HTTPStatusClass(status) || !strings.Contains(cerr.Message, "not followed") {
			t.Errorf("HTTP %d: error = %v, want class %s mentioning the redirect", status, err, client.HTTPStatusClass(status))
		}
		if err != nil && strings.Contains(err.Error(), "redirect-secret") {
			t.Errorf("HTTP %d: error repeats the Location query: %v", status, err)
		}
	}
	if followed.Load() {
		t.Error("the client followed a redirect and sent the request to another server")
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]client.Config{
		"no scheme":        {BaseURL: "localhost:8000/v1", Model: "m"},
		"ftp scheme":       {BaseURL: "ftp://host/v1", Model: "m"},
		"no host":          {BaseURL: "http:///v1", Model: "m"},
		"unparseable":      {BaseURL: "http://[::1", Model: "m"},
		"no model":         {BaseURL: "http://host/v1"},
		"negative timeout": {BaseURL: "http://host/v1", Model: "m", Timeout: -time.Second},
	} {
		if _, err := client.New(cfg); err == nil {
			t.Errorf("%s: New succeeded, want an error", name)
		}
	}
}

// TestInvalidURLIsNotRepeated checks that a URL that fails to parse is not
// repeated in the error, since it may hold a password.
func TestInvalidURLIsNotRepeated(t *testing.T) {
	t.Parallel()
	_, err := client.New(client.Config{BaseURL: "https://user:hunter2-secret@api.example.com/v1/%zz", Model: "m"})
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "invalid URL escape") {
		t.Errorf("error = %v, want the parse problem without the URL", err)
	}
}

func TestRedactURL(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"http://localhost:8000/v1":                     "http://localhost:8000/v1",
		"https://user:pass@api.example.com/v1":         "https://REDACTED@api.example.com/v1",
		"https://api.example.com/v1?key=abc&region=eu": "https://api.example.com/v1?key=REDACTED&region=REDACTED",
		"http://[::1": "<invalid URL>",
	}
	for in, want := range tests {
		if got := client.RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHTTPStatusClass(t *testing.T) {
	t.Parallel()
	if got := client.HTTPStatusClass(429); got != "http_429" {
		t.Errorf("HTTPStatusClass(429) = %q", got)
	}
	e := &client.Error{Class: client.ClassTimeout, Message: "slow", Err: context.DeadlineExceeded}
	if e.Error() != "timeout: slow" || !errors.Is(e, context.DeadlineExceeded) {
		t.Errorf("Error() = %q, Unwrap chain broken", e.Error())
	}
}

func TestEchoedSecretsAreRedacted(t *testing.T) {
	t.Parallel()
	ts := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad credentials: "+r.Header.Get("Authorization")+" "+r.Header.Get("X-Org-Token")+
			" "+r.Header.Get("X-Short")+" "+r.URL.RawQuery, http.StatusUnauthorized)
	}))
	c := newClient(t, ts.URL+"/v1?sig=query-secret-123&b64=abc%2Bdef%2Fghi%3Djkl", func(cfg *client.Config) {
		cfg.Headers = http.Header{"X-Org-Token": {"Token org-secret-456"}, "X-Short": {"abc"}}
	})
	_, err := c.Stream(t.Context(), userMsg)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, leaked := range []string{secret, "org-secret-456", "query-secret-123", "abc+def/ghi=jkl", "abc%2Bdef"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("error message leaks %q: %s", leaked, msg)
		}
	}
	// Short header values are not treated as secrets: they are usually
	// ordinary words, and scrubbing them would garble messages.
	if !strings.Contains(msg, "[REDACTED]") || !strings.Contains(msg, "bad credentials") || !strings.Contains(msg, " abc ") {
		t.Errorf("error message = %q, want the server text with secrets redacted", msg)
	}
}

// TestSecretAcrossTruncationIsRedacted puts the echoed key where the
// 300-byte message limit cuts it: redaction must happen before truncation.
func TestSecretAcrossTruncationIsRedacted(t *testing.T) {
	t.Parallel()
	for _, pad := range []int{260, 270, 285, 290, 299} {
		ts := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			body, _ := json.Marshal(map[string]any{"error": map[string]string{
				"message": strings.Repeat("x", pad) + " " + r.Header.Get("Authorization"),
			}})
			_, _ = w.Write(body)
		}))
		_, err := newClient(t, ts.URL, nil).Stream(t.Context(), userMsg)
		if err == nil {
			t.Fatal("expected an error")
		}
		// Any 4-byte piece of the key would be a leak.
		for i := 0; i+4 <= len(secret); i++ {
			if strings.Contains(err.Error(), secret[i:i+4]) {
				t.Errorf("padding %d: message leaks %q of the key: ...%s", pad, secret[i:i+4], err.Error()[max(0, len(err.Error())-40):])
				break
			}
		}
	}
}

func TestShortAPIKeyIsRedacted(t *testing.T) {
	t.Parallel()
	ts := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown key "+r.Header.Get("Authorization"), http.StatusUnauthorized)
	}))
	_, err := newClient(t, ts.URL, func(cfg *client.Config) { cfg.APIKey = "k3y" }).Stream(t.Context(), userMsg)
	if err == nil || strings.Contains(err.Error(), "k3y") || !strings.Contains(err.Error(), "Bearer [REDACTED]") {
		t.Errorf("error = %v, want the 3-byte API key redacted", err)
	}
}
