package sse_test

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aozmekik/llm-loadtest/sse"
)

func ExampleDecoder() {
	stream := ": keep-alive comment\r\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\r\n\r\n" +
		"event: notice\n" +
		"data: line one\n" +
		"data: line two\n\n" +
		"data: [DONE]\n\n"

	d := sse.NewDecoder(strings.NewReader(stream))
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		fmt.Printf("%s: %q\n", ev.Type, ev.Data)
	}
	// Output:
	// message: "{\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}"
	// notice: "line one\nline two"
	// message: "[DONE]"
}
