package mock_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/aozmekik/llm-loadtest/mock"
	"github.com/aozmekik/llm-loadtest/sse"
)

func ExampleServer() {
	srv, err := mock.New(mock.Config{
		TTFT:         mock.Constant(20 * time.Millisecond),
		ITL:          mock.Constant(5 * time.Millisecond),
		OutputTokens: mock.Constant(4),
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	body := `{"model":"demo","stream":true,"stream_options":{"include_usage":true},` +
		`"messages":[{"role":"user","content":"Say hello"}]}`
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+"/v1/chat/completions", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	dec := sse.NewDecoder(resp.Body)
	for {
		ev, err := dec.Next()
		if err != nil {
			break
		}
		if ev.Data == "[DONE]" {
			fmt.Println("[DONE]")
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal([]byte(ev.Data), &chunk)
		switch {
		case chunk.Usage != nil:
			fmt.Println("usage: completion_tokens =", chunk.Usage.CompletionTokens)
		case chunk.Choices[0].FinishReason != nil:
			fmt.Println("finish_reason:", *chunk.Choices[0].FinishReason)
		case chunk.Choices[0].Delta.Content != "":
			fmt.Println("token")
		}
	}
	// Output:
	// token
	// token
	// token
	// token
	// finish_reason: stop
	// usage: completion_tokens = 4
	// [DONE]
}
