// Package client sends streaming chat-completion requests to an
// OpenAI-compatible server and records when each streamed chunk arrives.
package client

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aozmekik/llm-loadtest/sse"
)

// drainTimeout bounds how long the client reads past [DONE] so that the
// connection can be reused. Timing is already recorded at that point.
const drainTimeout = 500 * time.Millisecond

// maxErrorBody limits how much of a non-200 response body is read.
const maxErrorBody = 64 << 10

var (
	errTimeout     = errors.New("request timeout")
	errIdleTimeout = errors.New("idle timeout")
	errDrained     = errors.New("drain finished")
)

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is one chat-completion request.
type Request struct {
	// Messages is the conversation to complete.
	Messages []Message
	// MaxTokens overrides Config.MaxTokens when positive.
	MaxTokens int
}

// Config configures a Client.
type Config struct {
	// BaseURL is the API root, for example "http://localhost:8000/v1".
	// Requests are sent to its path joined with "chat/completions"; a query
	// string, such as Azure's api-version, is kept.
	BaseURL string
	// Model is sent as the request's "model" field.
	Model string
	// APIKey, when non-empty, is sent as "Authorization: Bearer <APIKey>".
	// It never appears in errors or results.
	APIKey string
	// Headers are added to every request and override the defaults.
	Headers http.Header
	// UserAgent is sent as the User-Agent header when non-empty.
	UserAgent string
	// MaxTokens is sent as "max_tokens" when positive.
	MaxTokens int
	// Temperature is sent as "temperature" when non-nil.
	Temperature *float64
	// IncludeUsage requests a final usage chunk with exact token counts via
	// "stream_options": {"include_usage": true}.
	IncludeUsage bool
	// ExtraBody is merged into the JSON request body, overriding the fields
	// above; "stream" is always true.
	ExtraBody map[string]any
	// Timeout bounds each request from send to the end of the stream.
	// Zero means no limit.
	Timeout time.Duration
	// IdleTimeout fails a request when no bytes arrive for this long,
	// including while waiting for the response headers. Zero disables it.
	IdleTimeout time.Duration
	// HTTPClient sends the requests. When nil, a client tuned for many
	// concurrent streams (large idle pool, no compression) is used. That
	// client does not follow redirects: a 3xx response fails the request
	// with class "http_3xx", because following it would turn the POST into
	// a GET (301 to 303) or send custom key headers to another host.
	HTTPClient *http.Client
}

// Response holds the timing and token counts of one streamed completion.
// Times carry monotonic clock readings.
type Response struct {
	// Sent is when the request was handed to the HTTP client.
	Sent time.Time
	// FirstToken is when the first chunk carrying generated content
	// (text, reasoning or tool calls) arrived; zero if none did. A leading
	// chunk that only announces the assistant role does not count.
	FirstToken time.Time
	// End is when the stream finished or failed.
	End time.Time
	// Chunks holds the arrival time, relative to Sent, of every chunk that
	// carried generated content.
	Chunks []time.Duration
	// OutputTokens is the server-reported completion token count when
	// UsageReported is true, otherwise the number of content chunks.
	OutputTokens int
	// PromptTokens is the server-reported prompt token count, or zero.
	PromptTokens int
	// UsageReported reports whether the server sent a usage object.
	UsageReported bool
	// FinishReason is the last finish_reason the server sent.
	FinishReason string
}

// minSecretLen is the shortest header value, URL username or query value
// treated as a secret when scrubbing error messages; shorter values are too
// likely to be ordinary words. The API key and the URL password are
// scrubbed whatever their length.
const minSecretLen = 8

// Client sends chat-completion requests. It is safe for concurrent use.
type Client struct {
	cfg      Config
	endpoint string
	header   http.Header
	http     *http.Client
	secrets  []string // scrubbed from error messages, longest first
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		// *url.Error repeats the whole URL, credentials included.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid base URL %q: want http(s)://host[:port]/path", RedactURL(cfg.BaseURL))
	}
	if cfg.Model == "" {
		return nil, errors.New("model is required")
	}
	if cfg.Timeout < 0 || cfg.IdleTimeout < 0 {
		return nil, errors.New("timeouts must not be negative")
	}

	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "text/event-stream")
	if cfg.UserAgent != "" {
		header.Set("User-Agent", cfg.UserAgent)
	}
	if cfg.APIKey != "" {
		header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	for k, vs := range cfg.Headers {
		header[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}

	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{
			Transport: newTransport(),
			// Report a redirect instead of following it; see Config.HTTPClient.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	secrets := collectSecrets(&cfg, u)
	cfg.ExtraBody = maps.Clone(cfg.ExtraBody)
	cfg.Headers = nil
	return &Client{
		cfg:      cfg,
		endpoint: u.JoinPath("chat/completions").String(),
		header:   header,
		http:     hc,
		secrets:  secrets,
	}, nil
}

// collectSecrets lists configured values that must never appear in error
// messages, even when a server echoes them back: the API key, header
// values, and the credentials and query values of the base URL. Each is
// listed as configured and in its query- and path-escaped forms, and query
// values also as they appear in the raw query string, since servers tend to
// echo the URL they received.
func collectSecrets(cfg *Config, u *url.URL) []string {
	var secrets []string
	add := func(v string, always bool) {
		if v == "" || (!always && len(v) < minSecretLen) {
			return
		}
		secrets = append(secrets, v, url.QueryEscape(v), url.PathEscape(v))
	}
	add(cfg.APIKey, true)
	for _, values := range cfg.Headers {
		for _, v := range values {
			add(v, false)
			if _, token, ok := strings.Cut(v, " "); ok { // "Bearer <token>"
				add(strings.TrimSpace(token), false)
			}
		}
	}
	for _, values := range u.Query() {
		for _, v := range values {
			add(v, false)
		}
	}
	for pair := range strings.SplitSeq(u.RawQuery, "&") {
		if _, raw, ok := strings.Cut(pair, "="); ok {
			add(raw, false)
		}
	}
	if u.User != nil {
		add(u.User.Username(), false)
		if pw, ok := u.User.Password(); ok {
			add(pw, true)
		}
	}
	// Longest first, so that a value containing another is replaced whole.
	slices.SortFunc(secrets, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	return slices.Compact(secrets)
}

func (c *Client) redact(msg string) string {
	for _, s := range c.secrets {
		msg = strings.ReplaceAll(msg, s, "[REDACTED]")
	}
	return msg
}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 4096,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// Compressed event streams are buffered by the decompressor, which
		// would distort chunk arrival times.
		DisableCompression: true,
	}
}

// Stream sends req and reads the event stream to the end. The returned
// Response holds whatever was observed, even when the error is non-nil.
// Every non-nil error is an *Error.
func (c *Client) Stream(ctx context.Context, req Request) (Response, error) {
	resp, err := c.stream(ctx, req)
	if err != nil {
		// Redact before shortening: a secret cut in two by the truncation
		// would no longer match.
		err.Message = truncate(c.redact(err.Message))
		return resp, err
	}
	return resp, nil
}

func (c *Client) stream(ctx context.Context, req Request) (Response, *Error) {
	var resp Response
	payload, err := c.body(req)
	if err != nil {
		return resp, &Error{Class: ClassProtocol, Message: "encode request: " + err.Error(), Err: err}
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if c.cfg.Timeout > 0 {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeoutCause(ctx, c.cfg.Timeout, errTimeout)
		defer cancelTimeout()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return resp, &Error{Class: ClassProtocol, Message: "build request: " + err.Error(), Err: err}
	}
	httpReq.Header = c.header.Clone()

	var idle *time.Timer
	if c.cfg.IdleTimeout > 0 {
		idle = time.AfterFunc(c.cfg.IdleTimeout, func() { cancel(errIdleTimeout) })
		defer idle.Stop()
	}

	resp.Sent = time.Now()
	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		resp.End = time.Now()
		return resp, c.transportError(ctx, err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	if idle != nil {
		idle.Reset(c.cfg.IdleTimeout) // the response headers were data too
	}

	if httpResp.StatusCode != http.StatusOK {
		resp.End = time.Now()
		return resp, statusError(httpResp)
	}
	if mt, _, _ := mime.ParseMediaType(httpResp.Header.Get("Content-Type")); mt == "application/json" {
		resp.End = time.Now()
		return resp, &Error{Class: ClassProtocol, Message: "server answered with application/json instead of an event stream; does it support \"stream\": true?"}
	}

	var body io.Reader = httpResp.Body
	if idle != nil {
		body = &idleReader{r: body, timer: idle, timeout: c.cfg.IdleTimeout}
	}
	readErr := c.read(ctx, sse.NewDecoder(body), &resp)
	if !resp.UsageReported {
		resp.OutputTokens = len(resp.Chunks)
	}
	if readErr == nil {
		drain := time.AfterFunc(drainTimeout, func() { cancel(errDrained) })
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, maxErrorBody))
		drain.Stop()
	}
	return resp, readErr
}

func (c *Client) body(req Request) ([]byte, error) {
	body := make(map[string]any, len(c.cfg.ExtraBody)+6)
	body["model"] = c.cfg.Model
	body["messages"] = req.Messages
	if n := cmp.Or(req.MaxTokens, c.cfg.MaxTokens); n > 0 {
		body["max_tokens"] = n
	}
	if c.cfg.Temperature != nil {
		body["temperature"] = *c.cfg.Temperature
	}
	if c.cfg.IncludeUsage {
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	maps.Copy(body, c.cfg.ExtraBody)
	body["stream"] = true
	return json.Marshal(body)
}

// read consumes the event stream, filling resp. It returns nil once [DONE]
// arrives, or at end of stream after a finish_reason was seen.
func (c *Client) read(ctx context.Context, dec *sse.Decoder, resp *Response) *Error {
	for {
		ev, err := dec.Next()
		now := time.Now()
		if err != nil {
			resp.End = now
			switch {
			case ctx.Err() != nil:
				// The stream broke because we canceled it.
				return c.transportError(ctx, err)
			case errors.Is(err, io.EOF) && resp.FinishReason != "":
				return nil // tolerate servers that omit [DONE]
			case errors.Is(err, io.EOF):
				return &Error{Class: ClassIncomplete, Message: "stream ended before [DONE]"}
			case errors.Is(err, sse.ErrTooLarge):
				return &Error{Class: ClassProtocol, Message: err.Error(), Err: err}
			default:
				return c.transportError(ctx, err)
			}
		}
		switch ev.Type {
		case "error":
			resp.End = now
			return &Error{Class: ClassStreamError, Message: errorMessage([]byte(ev.Data))}
		case "message":
		default:
			continue // named events are not part of the OpenAI protocol
		}
		if ev.Data == "[DONE]" {
			resp.End = now
			return nil
		}

		var ch chunk
		if err := json.Unmarshal([]byte(ev.Data), &ch); err != nil {
			resp.End = now
			return &Error{Class: ClassProtocol, Message: "invalid JSON in stream: " + err.Error(), Err: err}
		}
		if ch.isError() {
			resp.End = now
			return &Error{Class: ClassStreamError, Message: errorMessage([]byte(ev.Data))}
		}
		if ch.hasContent() {
			if resp.FirstToken.IsZero() {
				resp.FirstToken = now
			}
			resp.Chunks = append(resp.Chunks, now.Sub(resp.Sent))
		}
		for _, choice := range ch.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				resp.FinishReason = *choice.FinishReason
			}
		}
		if ch.Usage != nil {
			resp.UsageReported = true
			resp.OutputTokens = ch.Usage.CompletionTokens
			resp.PromptTokens = ch.Usage.PromptTokens
		}
	}
}

// transportError classifies an error from sending the request or reading
// the body, preferring the reason the request context was canceled.
func (c *Client) transportError(ctx context.Context, err error) *Error {
	if ctx.Err() != nil {
		switch cause := context.Cause(ctx); {
		case errors.Is(cause, errTimeout):
			return &Error{Class: ClassTimeout, Message: "no complete response within " + c.cfg.Timeout.String()}
		case errors.Is(cause, errIdleTimeout):
			return &Error{Class: ClassIdleTimeout, Message: "no data received for " + c.cfg.IdleTimeout.String()}
		default:
			return &Error{Class: ClassCanceled, Message: "request canceled", Err: cause}
		}
	}
	// *url.Error repeats the request URL, which may embed credentials.
	if ue, ok := errors.AsType[*url.Error](err); ok {
		err = ue.Err
	}
	return &Error{Class: ClassConnection, Message: err.Error(), Err: err}
}

// statusError builds the error for a non-200 response. Its message is not
// yet redacted or truncated; Stream does both.
func statusError(resp *http.Response) *Error {
	var msg string
	if loc := resp.Header.Get("Location"); resp.StatusCode/100 == 3 && loc != "" {
		msg = "redirect to " + RedactURL(loc) + " not followed"
	} else {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		msg = cmp.Or(errorMessage(body), http.StatusText(resp.StatusCode))
	}
	return &Error{
		Class:      HTTPStatusClass(resp.StatusCode),
		StatusCode: resp.StatusCode,
		Message:    fmt.Sprintf("HTTP %d: %s", resp.StatusCode, msg),
	}
}

// idleReader resets the idle timer whenever bytes arrive.
type idleReader struct {
	r       io.Reader
	timer   *time.Timer
	timeout time.Duration
}

func (ir *idleReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	if n > 0 {
		ir.timer.Reset(ir.timeout)
	}
	return n, err
}

// RedactURL removes credentials and query values from a URL so that it can
// be printed or stored. Unparseable input is replaced entirely.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid URL>"
	}
	if u.User != nil {
		u.User = url.User("REDACTED")
	}
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			q.Set(k, "REDACTED")
		}
		u.RawQuery = q.Encode()
	}
	return u.String()
}
