package client

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Class categorizes why a request failed. Results count errors by class.
type Class string

// Error classes. HTTP status errors use HTTPStatusClass instead.
const (
	// ClassTimeout means the request exceeded Config.Timeout.
	ClassTimeout Class = "timeout"
	// ClassIdleTimeout means no bytes arrived for Config.IdleTimeout.
	ClassIdleTimeout Class = "idle_timeout"
	// ClassCanceled means the caller's context was canceled.
	ClassCanceled Class = "canceled"
	// ClassConnection means the connection failed or broke mid-stream.
	ClassConnection Class = "connection"
	// ClassStreamError means the server sent an error payload in the stream.
	ClassStreamError Class = "stream_error"
	// ClassProtocol means the response violated the streaming protocol,
	// for example with invalid JSON or an oversized event.
	ClassProtocol Class = "protocol"
	// ClassIncomplete means the stream ended without [DONE] or a
	// finish_reason.
	ClassIncomplete Class = "incomplete"
)

// HTTPStatusClass returns the class for a non-200 HTTP status, such as
// "http_429".
func HTTPStatusClass(code int) Class { return Class("http_" + strconv.Itoa(code)) }

// Error describes a failed request.
type Error struct {
	// Class categorizes the failure.
	Class Class
	// StatusCode is the HTTP status for HTTP errors, otherwise zero.
	StatusCode int
	// Message is a human-readable description of at most about 300 bytes.
	// It never contains the request URL. Configured secrets are replaced
	// with "[REDACTED]", even when the server echoes them: the API key and
	// the base URL's password whatever their length, and header values,
	// the URL's username and query values of 8 bytes or more. A server that
	// echoes only part of a secret is not caught.
	Message string
	// Err is the underlying error, if any.
	Err error
}

// Error implements the error interface.
func (e *Error) Error() string { return string(e.Class) + ": " + e.Message }

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error { return e.Err }

// chunk is the subset of a chat.completion.chunk object the client reads.
type chunk struct {
	Object  string `json:"object"`
	Choices []struct {
		Delta struct {
			Content          string          `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			Reasoning        string          `json:"reasoning"`
			ToolCalls        json.RawMessage `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// hasContent reports whether the chunk carries generated output.
func (c *chunk) hasContent() bool {
	for _, choice := range c.Choices {
		d := choice.Delta
		if d.Content != "" || d.ReasoningContent != "" || d.Reasoning != "" || !isNullJSON(d.ToolCalls) {
			return true
		}
	}
	return false
}

// isError reports whether the chunk is an error payload, either
// {"error": ...} or vLLM-style {"object": "error", ...}.
func (c *chunk) isError() bool {
	return !isNullJSON(c.Error) || c.Object == "error"
}

func isNullJSON(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

// maxMessageLen bounds error messages copied from server responses.
const maxMessageLen = 300

// errorMessage extracts a readable message from an error body in any of the
// shapes OpenAI-compatible servers use, falling back to the raw text. The
// result is neither redacted nor truncated; Client.Stream does both, in
// that order.
func errorMessage(body []byte) string {
	var shape struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &shape) == nil {
		var nested struct {
			Message string `json:"message"`
		}
		var text string
		switch {
		case json.Unmarshal(shape.Error, &nested) == nil && nested.Message != "":
			return nested.Message
		case json.Unmarshal(shape.Error, &text) == nil && text != "":
			return text
		case shape.Message != "":
			return shape.Message
		case json.Unmarshal(shape.Detail, &text) == nil && text != "":
			return text
		}
	}
	return strings.TrimSpace(string(body))
}

// truncate collapses whitespace runs to single spaces and cuts s to about
// maxMessageLen bytes on a UTF-8 boundary.
func truncate(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxMessageLen {
		return s
	}
	cut := maxMessageLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
