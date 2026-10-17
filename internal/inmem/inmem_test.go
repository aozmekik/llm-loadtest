package inmem_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aozmekik/llm-loadtest/internal/inmem"
)

func do(t *testing.T, ctx context.Context, h http.HandlerFunc) (*http.Response, error) {
	t.Helper()
	client := &http.Client{Transport: &inmem.Transport{Handler: h}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://inmem/path", strings.NewReader("ping"))
	if err != nil {
		t.Fatal(err)
	}
	return client.Do(req)
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	resp, err := do(t, t.Context(), func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Echo", string(body)+" "+r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("pong"))
		w.Header().Set("X-Late", "ignored") // after WriteHeader, like net/http
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "pong" {
		t.Fatalf("body = %q, %v", body, err)
	}
	if resp.StatusCode != http.StatusTeapot || resp.Header.Get("X-Echo") != "ping /path" || resp.Header.Get("X-Late") != "" {
		t.Errorf("status %d, headers %v", resp.StatusCode, resp.Header)
	}
}

func TestImplicitOK(t *testing.T) {
	t.Parallel()
	resp, err := do(t, t.Context(), func(http.ResponseWriter, *http.Request) {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if body, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK || len(body) != 0 {
		t.Errorf("status %d, body %q; want 200 and empty", resp.StatusCode, body)
	}
}

func TestAbort(t *testing.T) {
	t.Parallel()
	resp, err := do(t, t.Context(), func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, inmem.ErrAborted) {
		t.Errorf("abort before header: error %v, want ErrAborted", err)
	}

	resp, err = do(t, t.Context(), func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
		panic(http.ErrAbortHandler)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if string(body) != "partial" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("abort mid-body: body %q, error %v; want partial body and io.ErrUnexpectedEOF", body, err)
	}
}

func TestCancellation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		release := make(chan struct{})
		defer close(release)
		resp, err := do(t, ctx, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			<-release // ignores the request context, unlike a well-behaved handler
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		start := time.Now()
		_, err = io.ReadAll(resp.Body)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != time.Second {
			t.Errorf("read error %v after %v; want DeadlineExceeded after 1s", err, time.Since(start))
		}
	})

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(time.Second)
			cancel()
		}()
		resp, err := do(t, ctx, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		if err == nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancel before header: error %v, want context.Canceled", err)
		}
	})
}
