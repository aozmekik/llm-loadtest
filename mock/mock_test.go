package mock_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aozmekik/llm-loadtest/internal/inmem"
	"github.com/aozmekik/llm-loadtest/mock"
	"github.com/aozmekik/llm-loadtest/sse"
)

type wireChunk struct {
	Object  string `json:"object"`
	Choices []struct {
		Delta struct {
			Role    string  `json:"role"`
			Content *string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type streamResult struct {
	status  int
	chunks  []wireChunk
	done    bool
	readErr error
}

func newServer(t *testing.T, cfg mock.Config) *httptest.Server {
	t.Helper()
	srv, err := mock.New(cfg)
	if err != nil {
		t.Fatalf("mock.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// post sends a chat-completion request. The caller closes the body.
func post(t *testing.T, client *http.Client, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

// postStatus sends a request and returns only its status code.
func postStatus(t *testing.T, client *http.Client, url, body string) int {
	t.Helper()
	resp := post(t, client, url, body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// postStream sends a streaming request and decodes the whole response.
func postStream(t *testing.T, client *http.Client, url, body string) (streamResult, http.Header) {
	t.Helper()
	resp := post(t, client, url, body)
	defer func() { _ = resp.Body.Close() }()
	return readStream(t, resp), resp.Header
}

func readStream(t *testing.T, resp *http.Response) streamResult {
	t.Helper()
	res := streamResult{status: resp.StatusCode}
	dec := sse.NewDecoder(resp.Body)
	for {
		ev, err := dec.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				res.readErr = err
			}
			return res
		}
		if ev.Data == "[DONE]" {
			res.done = true
			continue
		}
		var c wireChunk
		if err := json.Unmarshal([]byte(ev.Data), &c); err != nil {
			t.Fatalf("invalid chunk %q: %v", ev.Data, err)
		}
		res.chunks = append(res.chunks, c)
	}
}

func contentOf(chunks []wireChunk) (text string, n int) {
	var b strings.Builder
	for _, c := range chunks {
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil && *ch.Delta.Content != "" {
				b.WriteString(*ch.Delta.Content)
				n++
			}
		}
	}
	return b.String(), n
}

const streamBody = `{"model":"m","messages":[{"role":"user","content":"one two three"}],` +
	`"max_tokens":5,"stream":true,"stream_options":{"include_usage":true}}`

func TestStreamFormat(t *testing.T) {
	t.Parallel()
	ts := newServer(t, mock.Config{OutputTokens: mock.Constant(8)})
	res, header := postStream(t, ts.Client(), ts.URL, streamBody)
	if ct := header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if res.status != http.StatusOK || !res.done || res.readErr != nil {
		t.Fatalf("status %d, done %v, error %v", res.status, res.done, res.readErr)
	}
	first := res.chunks[0]
	if first.Choices[0].Delta.Role != "assistant" || first.Object != "chat.completion.chunk" {
		t.Errorf("first chunk = %+v, want a role-only assistant chunk", first)
	}
	text, n := contentOf(res.chunks)
	if n != 5 {
		t.Errorf("content chunks = %d, want 5 (capped by max_tokens)", n)
	}
	if words := strings.Fields(text); len(words) != 5 {
		t.Errorf("generated %d words, want 5: %q", len(words), text)
	}
	finish := res.chunks[len(res.chunks)-2].Choices[0].FinishReason
	if finish == nil || *finish != "length" {
		t.Errorf("finish_reason = %v, want length", finish)
	}
	usage := res.chunks[len(res.chunks)-1]
	if usage.Usage == nil || usage.Usage.CompletionTokens != 5 || usage.Usage.PromptTokens != 3 || len(usage.Choices) != 0 {
		t.Errorf("usage chunk = %+v, want 3 prompt and 5 completion tokens with no choices", usage)
	}
}

func TestStreamOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		cfg        mock.Config
		body       string
		wantChunks int
		wantUsage  bool
		wantFinish string
	}{
		{
			name:       "usage disabled by server",
			cfg:        mock.Config{OutputTokens: mock.Constant(3), DisableUsage: true},
			body:       streamBody,
			wantChunks: 3, wantFinish: "stop",
		},
		{
			name:       "usage not requested",
			cfg:        mock.Config{OutputTokens: mock.Constant(3)},
			body:       `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true}`,
			wantChunks: 3, wantFinish: "stop",
		},
		{
			name:       "max_tokens used when output length unset",
			cfg:        mock.Config{},
			body:       `{"model":"m","messages":[{"role":"user","content":"x"}],"max_tokens":7,"stream":true}`,
			wantChunks: 7, wantFinish: "length",
		},
		{
			name:       "max_completion_tokens honored",
			cfg:        mock.Config{OutputTokens: mock.Constant(50)},
			body:       `{"model":"m","messages":[{"role":"user","content":"x"}],"max_completion_tokens":4,"stream":true}`,
			wantChunks: 4, wantFinish: "length",
		},
		{
			name:       "default length without max_tokens",
			cfg:        mock.Config{},
			body:       `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true}`,
			wantChunks: 16, wantFinish: "stop",
		},
		{
			name:       "tokens grouped into chunks",
			cfg:        mock.Config{OutputTokens: mock.Constant(10), TokensPerChunk: 4},
			body:       streamBody[:len(streamBody)-1] + `,"max_tokens":10}`,
			wantChunks: 3, wantUsage: true, wantFinish: "length",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := newServer(t, tt.cfg)
			res, _ := postStream(t, ts.Client(), ts.URL, tt.body)
			if !res.done {
				t.Fatal("stream did not end with [DONE]")
			}
			if _, n := contentOf(res.chunks); n != tt.wantChunks {
				t.Errorf("content chunks = %d, want %d", n, tt.wantChunks)
			}
			gotUsage := res.chunks[len(res.chunks)-1].Usage != nil
			if gotUsage != tt.wantUsage {
				t.Errorf("usage chunk present = %v, want %v", gotUsage, tt.wantUsage)
			}
			var finish string
			for _, c := range res.chunks {
				for _, ch := range c.Choices {
					if ch.FinishReason != nil {
						finish = *ch.FinishReason
					}
				}
			}
			if finish != tt.wantFinish {
				t.Errorf("finish_reason = %q, want %q", finish, tt.wantFinish)
			}
		})
	}
}

func TestNonStreaming(t *testing.T) {
	t.Parallel()
	ts := newServer(t, mock.Config{OutputTokens: mock.Constant(6)})
	resp := post(t, ts.Client(), ts.URL, `{"model":"m","messages":[{"role":"user","content":"hi there"}]}`)
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Object != "chat.completion" || body.Model != "m" || body.Usage.CompletionTokens != 6 || body.Usage.PromptTokens != 2 {
		t.Errorf("unexpected completion %+v", body)
	}
	if n := len(strings.Fields(body.Choices[0].Message.Content)); n != 6 {
		t.Errorf("content has %d words, want 6", n)
	}
}

func TestRoutesAndValidation(t *testing.T) {
	t.Parallel()
	ts := newServer(t, mock.Config{Model: "served-model"})
	client := ts.Client()

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/v1/models", http.NoBody)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	err = json.NewDecoder(resp.Body).Decode(&models)
	if err != nil || len(models.Data) != 1 || models.Data[0].ID != "served-model" {
		t.Errorf("/v1/models = %+v, %v", models, err)
	}

	for body, want := range map[string]int{
		`not json`:                      http.StatusBadRequest,
		`{"messages":[]}`:               http.StatusBadRequest,
		`{"model":"m","messages":[]}`:   http.StatusBadRequest,
		`{"model":"m","messages":[{}]}`: http.StatusOK,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a b"}]}]}`: http.StatusOK,
	} {
		if got := postStatus(t, client, ts.URL, body); got != want {
			t.Errorf("POST %s: status %d, want %d", body, got, want)
		}
	}

	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/nope", http.NoBody)
	resp404, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp404.Body.Close() }()
	if resp404.StatusCode != http.StatusNotFound || resp404.Header.Get("Content-Type") != "application/json" {
		t.Errorf("unknown route: status %d, content type %q", resp404.StatusCode, resp404.Header.Get("Content-Type"))
	}
}

func TestErrorInjection(t *testing.T) {
	t.Parallel()
	t.Run("status", func(t *testing.T) {
		t.Parallel()
		ts := newServer(t, mock.Config{ErrorRate: 1, ErrorStatus: http.StatusTooManyRequests})
		resp := post(t, ts.Client(), ts.URL, streamBody)
		defer func() { _ = resp.Body.Close() }()
		var body struct {
			Error struct {
				Message string `json:"message"`
				Code    int    `json:"code"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusTooManyRequests || body.Error.Code != 429 || body.Error.Message == "" {
			t.Errorf("status %d, body %+v; want 429 with an OpenAI-style error", resp.StatusCode, body)
		}
	})
	t.Run("stream error", func(t *testing.T) {
		t.Parallel()
		ts := newServer(t, mock.Config{StreamErrorRate: 1, OutputTokens: mock.Constant(20)})
		res, _ := postStream(t, ts.Client(), ts.URL, `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true}`)
		last := res.chunks[len(res.chunks)-1]
		if res.status != http.StatusOK || last.Error == nil || last.Error.Message == "" {
			t.Fatalf("status %d, last chunk %+v; want an error payload", res.status, last)
		}
		if _, n := contentOf(res.chunks); n >= 20 {
			t.Errorf("stream error came after all %d tokens, want it part-way", n)
		}
	})
	t.Run("abort", func(t *testing.T) {
		t.Parallel()
		ts := newServer(t, mock.Config{AbortRate: 1, OutputTokens: mock.Constant(20)})
		res, _ := postStream(t, ts.Client(), ts.URL, `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true}`)
		if res.readErr == nil || res.done {
			t.Errorf("read error = %v, done = %v; want the connection dropped before [DONE]", res.readErr, res.done)
		}
	})
	t.Run("rates are fractions of all requests", func(t *testing.T) {
		t.Parallel()
		srv, err := mock.New(mock.Config{ErrorRate: 0.25, OutputTokens: mock.Constant(1), Seed: 42})
		if err != nil {
			t.Fatal(err)
		}
		const n = 2000
		failed := 0
		for range n {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody))
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				failed++
			}
		}
		// Binomial(2000, 0.25) has a standard deviation of about 19.4.
		if failed < 400 || failed > 600 {
			t.Errorf("%d of %d requests failed, want about 500", failed, n)
		}
	})
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]mock.Config{
		"negative rate":  {ErrorRate: -0.1},
		"rate above one": {AbortRate: 1.5},
		"rates sum":      {ErrorRate: 0.6, StreamErrorRate: 0.6},
		"bad status":     {ErrorStatus: 200},
		"negative slots": {Slots: -1},
	} {
		if _, err := mock.New(cfg); err == nil {
			t.Errorf("%s: mock.New succeeded, want a validation error", name)
		}
	}
}

// synctestClient returns an HTTP client wired to srv through an in-memory
// pipe, for use inside a synctest bubble.
func synctestClient(t *testing.T, cfg mock.Config) *http.Client {
	t.Helper()
	srv, err := mock.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &inmem.Transport{Handler: srv}}
}

// TestStreamTiming checks token times exactly, using the fake clock.
func TestStreamTiming(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		client := synctestClient(t, mock.Config{
			TTFT:           mock.Constant(200 * time.Millisecond),
			ITL:            mock.Constant(25 * time.Millisecond),
			OutputTokens:   mock.Constant(9),
			TokensPerChunk: 3,
		})
		start := time.Now()
		resp := post(t, client, "http://mock", `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true}`)
		defer func() { _ = resp.Body.Close() }()
		dec := sse.NewDecoder(resp.Body)
		var arrivals []time.Duration
		for {
			ev, err := dec.Next()
			if err != nil {
				break
			}
			var c wireChunk
			_ = json.Unmarshal([]byte(ev.Data), &c)
			if _, n := contentOf([]wireChunk{c}); n > 0 {
				arrivals = append(arrivals, time.Since(start))
			}
		}
		// Tokens are generated at 200ms + k*25ms; each chunk of three is
		// sent when its last token is ready.
		want := []time.Duration{250 * time.Millisecond, 325 * time.Millisecond, 400 * time.Millisecond}
		if len(arrivals) != len(want) {
			t.Fatalf("arrivals = %v, want %v", arrivals, want)
		}
		for i := range want {
			if arrivals[i] != want[i] {
				t.Errorf("chunk %d arrived at %v, want %v", i, arrivals[i], want[i])
			}
		}
	})
}

// TestSlotsQueueRequests checks that requests beyond the slot count wait,
// and that the wait shows up as time to first token.
func TestSlotsQueueRequests(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		client := synctestClient(t, mock.Config{
			TTFT:         mock.Constant(100 * time.Millisecond),
			ITL:          mock.Constant(10 * time.Millisecond),
			OutputTokens: mock.Constant(11), // 100ms + 10*10ms = 200ms per request
			Slots:        2,
		})
		start := time.Now()
		var mu sync.Mutex
		var ends []time.Duration
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://mock/v1/chat/completions",
					bytes.NewReader([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true}`)))
				resp, err := client.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				mu.Lock()
				ends = append(ends, time.Since(start))
				mu.Unlock()
			})
		}
		wg.Wait()
		counts := map[time.Duration]int{}
		for _, e := range ends {
			counts[e]++
		}
		if counts[200*time.Millisecond] != 2 || counts[400*time.Millisecond] != 2 {
			t.Errorf("completion times = %v, want two at 200ms and two at 400ms", ends)
		}
	})
}

func TestCancelStopsGeneration(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		client := synctestClient(t, mock.Config{
			TTFT: mock.Constant(time.Second), OutputTokens: mock.Constant(5), Slots: 1,
		})
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://mock/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true}`))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		start := time.Now()
		_, _ = io.Copy(io.Discard, resp.Body)
		if elapsed := time.Since(start); elapsed != 100*time.Millisecond {
			t.Errorf("body read returned after %v, want it to stop at the 100ms deadline", elapsed)
		}
		// The bubble only exits if the handler goroutine returned too.
	})
}
