// Command library shows the two public packages together: it serves the
// mock in-process and reads one streamed completion with the SSE decoder,
// printing when each token arrived.
//
//	go run ./examples/library
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aozmekik/llm-loadtest/mock"
	"github.com/aozmekik/llm-loadtest/sse"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	srv, err := mock.New(mock.Config{
		TTFT:         mock.Constant(80 * time.Millisecond),
		ITL:          mock.Normal(20*time.Millisecond, 5*time.Millisecond),
		OutputTokens: mock.Constant(8),
		Seed:         1,
	})
	if err != nil {
		return err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	defer func() { _ = hs.Close() }()

	body := `{"model": "mock", "stream": true, "stream_options": {"include_usage": true},
		"messages": [{"role": "user", "content": "Say something"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+ln.Addr().String()+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return err
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	dec := sse.NewDecoder(resp.Body)
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if ev.Data == "[DONE]" {
			fmt.Printf("%8.1f ms  [DONE]\n", ms(time.Since(start)))
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
			return err
		}
		switch {
		case chunk.Usage != nil:
			fmt.Printf("%8.1f ms  usage: %d completion tokens\n", ms(time.Since(start)), chunk.Usage.CompletionTokens)
		case len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "":
			fmt.Printf("%8.1f ms  token %q\n", ms(time.Since(start)), chunk.Choices[0].Delta.Content)
		}
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
