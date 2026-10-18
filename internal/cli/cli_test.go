package cli_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aozmekik/llm-loadtest/internal/cli"
	"github.com/aozmekik/llm-loadtest/internal/loadgen"
	"github.com/aozmekik/llm-loadtest/internal/report"
	"github.com/aozmekik/llm-loadtest/mock"
)

// syncBuffer is a bytes.Buffer safe for concurrent writes and reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type invocation struct {
	code           int
	stdout, stderr string
}

func invoke(ctx context.Context, t *testing.T, env map[string]string, args ...string) invocation {
	t.Helper()
	var stdout, stderr syncBuffer
	code := cli.Main(ctx, args, cli.Env{
		Stdout:  &stdout,
		Stderr:  &stderr,
		Getenv:  func(k string) string { return env[k] },
		Version: "test",
	})
	return invocation{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func mockServer(t *testing.T, cfg mock.Config) *httptest.Server {
	t.Helper()
	srv, err := mock.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

var fast = mock.Config{
	TTFT: mock.Constant(2 * time.Millisecond), ITL: mock.Constant(time.Millisecond), OutputTokens: mock.Constant(8),
}

func readResult(t *testing.T, path string) *report.Result {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	r, err := report.ReadJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunSweep(t *testing.T) {
	t.Parallel()
	ts := mockServer(t, fast)
	dir := t.TempDir()
	out := invoke(t.Context(), t, nil, "run",
		"--base-url", ts.URL+"/v1", "--model", "m", "--concurrency", "1,2", "--requests", "6", "--warmup", "2",
		"--out", filepath.Join(dir, "r.json"), "--out", filepath.Join(dir, "r.csv"), "--out", filepath.Join(dir, "r.md"))
	if out.code != 0 {
		t.Fatalf("exit %d\nstderr: %s", out.code, out.stderr)
	}
	for _, want := range []string{"| concurrency=1 | 6 | 0 |", "| concurrency=2 | 6 | 0 |", "exact (server-reported usage) for all 12"} {
		if !strings.Contains(out.stdout, want) {
			t.Errorf("report lacks %q:\n%s", want, out.stdout)
		}
	}
	for _, want := range []string{"warmup: 2 requests, 0 failed", "[2/2] concurrency=2", "wrote " + filepath.Join(dir, "r.csv")} {
		if !strings.Contains(out.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out.stderr)
		}
	}
	r := readResult(t, filepath.Join(dir, "r.json"))
	if len(r.Stages) != 2 || r.Stages[1].Succeeded != 6 || r.Stages[1].OutputTokens.Mean != 8 || r.Tool != "llm-loadtest test" {
		t.Errorf("unexpected result %+v", r)
	}
	if r.Config.Warmup != 2 || r.Config.Requests != 6 || r.Config.Duration != "" {
		t.Errorf("config = %+v", r.Config)
	}
	// Warmup requests take sequence numbers 0 and 1 and are not reported.
	for _, s := range r.Stages {
		for _, rec := range s.Records {
			if rec.Seq < 2 {
				t.Errorf("warmup request %d appears in the results", rec.Seq)
			}
		}
	}
	md, err := os.ReadFile(filepath.Join(dir, "r.md"))
	if err != nil || string(md) != out.stdout {
		t.Errorf("--out .md differs from stdout (%v)", err)
	}
	csvData, err := os.ReadFile(filepath.Join(dir, "r.csv"))
	if err != nil || strings.Count(string(csvData), "\n") != 13 {
		t.Errorf("CSV should have a header and 12 rows: %v\n%s", err, csvData)
	}
}

func TestRunOpenLoopWithDataset(t *testing.T) {
	t.Parallel()
	ts := mockServer(t, fast)
	dataset := filepath.Join(t.TempDir(), "prompts.jsonl")
	if err := os.WriteFile(dataset, []byte("{\"prompt\": \"a b c\"}\n{\"prompt\": \"d e\", \"max_tokens\": 4}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(t.TempDir(), "open.json")
	out := invoke(t.Context(), t, nil, "run", "--quiet",
		"--base-url", ts.URL+"/v1", "--model", "m", "--rate", "200", "--arrival", "constant",
		"--requests", "10", "--dataset", dataset, "--shuffle", "--out", jsonPath)
	if out.code != 0 {
		t.Fatalf("exit %d\nstderr: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stdout, "| rate=200/s constant |") || !strings.Contains(out.stdout, "Send lag (ms)") {
		t.Errorf("open-loop report:\n%s", out.stdout)
	}
	if strings.Contains(out.stderr, "done ") {
		t.Errorf("--quiet still printed progress:\n%s", out.stderr)
	}
	if !strings.Contains(out.stderr, "10 requests cycled through the dataset's 2 prompts") {
		t.Errorf("no note about repeated prompts:\n%s", out.stderr)
	}
	r := readResult(t, jsonPath)
	if r.Config.Prompts != "dataset prompts.jsonl (2 prompts, shuffled)" || r.Config.MaxInFlight != 1024 {
		t.Errorf("config = %+v", r.Config)
	}
	if len(r.Stages[0].Records) != 10 {
		t.Errorf("JSON has %d records, want 10", len(r.Stages[0].Records))
	}
	if got := r.Stages[0].OutputTokens.Min; got != 4 {
		t.Errorf("min output tokens = %v, want 4 from the per-row max_tokens", got)
	}
}

func TestJSONWithoutRecords(t *testing.T) {
	t.Parallel()
	ts := mockServer(t, fast)
	dir := t.TempDir()
	out := invoke(t.Context(), t, nil, "run", "--quiet", "--base-url", ts.URL+"/v1", "--model", "m",
		"--requests", "4", "--json-records=false", "--out", filepath.Join(dir, "r.json"), "--out", filepath.Join(dir, "r.csv"))
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	r := readResult(t, filepath.Join(dir, "r.json"))
	if len(r.Stages) != 1 || r.Stages[0].Records != nil || r.Stages[0].Succeeded != 4 {
		t.Errorf("summary JSON = %+v, want aggregates without records", r.Stages)
	}
	csvData, err := os.ReadFile(filepath.Join(dir, "r.csv"))
	if err != nil || strings.Count(string(csvData), "\n") != 5 {
		t.Errorf("CSV must keep the 4 records: %v\n%s", err, csvData)
	}
}

func TestMaxTokensField(t *testing.T) {
	t.Parallel()
	ts := mockServer(t, fast)
	jsonPath := filepath.Join(t.TempDir(), "r.json")
	out := invoke(t.Context(), t, nil, "run", "--quiet", "--base-url", ts.URL+"/v1", "--model", "m", "--requests", "2",
		"--max-tokens", "3", "--max-tokens-field", "max_completion_tokens", "--out", jsonPath)
	if out.code != 0 || !strings.Contains(out.stdout, "; max_completion_tokens 3") {
		t.Fatalf("exit %d\n%s%s", out.code, out.stdout, out.stderr)
	}
	r := readResult(t, jsonPath)
	// The mock honors max_completion_tokens, so the limit took effect.
	if r.Config.MaxTokensField != "max_completion_tokens" || r.Stages[0].OutputTokens.Max != 3 {
		t.Errorf("config %+v, output tokens %+v", r.Config, r.Stages[0].OutputTokens)
	}
}

func TestSecretsAreNeverWritten(t *testing.T) {
	t.Parallel()
	const (
		key       = "sk-live-0123456789abcdef"
		headerVal = "header-secret-value"
		queryVal  = "query-secret-value"
	)
	inner, err := mock.New(fast)
	if err != nil {
		t.Fatal(err)
	}
	var sawKey bool
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawKey = sawKey || r.Header.Get("Authorization") == "Bearer "+key
		mu.Unlock()
		if r.URL.Query().Get("sig") == "fail" {
			http.Error(w, "denied for "+r.URL.RawQuery, http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	env := map[string]string{"MY_KEY": key}
	for _, query := range []string{"?token=" + queryVal, "?token=" + queryVal + "&sig=fail"} {
		out := invoke(t.Context(), t, env, "run", "--api-key-env", "MY_KEY", "--header", "X-Secret: "+headerVal,
			"--base-url", ts.URL+"/v1"+query, "--model", "m", "--requests", "3",
			"--out", filepath.Join(dir, "r.json"), "--out", filepath.Join(dir, "r.csv"), "--out", filepath.Join(dir, "r.md"))
		var written strings.Builder
		for _, name := range []string{"r.json", "r.csv", "r.md"} {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			written.Write(b)
		}
		files := written.String()
		for _, secret := range []string{key, headerVal, queryVal} {
			for where, text := range map[string]string{"stdout": out.stdout, "stderr": out.stderr, "output files": files} {
				if strings.Contains(text, secret) {
					t.Errorf("%s contains secret %q", where, secret)
				}
			}
		}
		if !strings.Contains(files, `"X-Secret"`) {
			t.Error("header name should still be recorded")
		}
	}
	if !sawKey {
		t.Error("the API key from --api-key-env was not sent")
	}
}

// TestDefaultKeyIsNotSentToOtherHosts runs with OPENAI_API_KEY set and no
// --api-key-env against a local server, which must not receive the key.
func TestDefaultKeyIsNotSentToOtherHosts(t *testing.T) {
	t.Parallel()
	inner, err := mock.New(fast)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var auth []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	out := invoke(t.Context(), t, map[string]string{"OPENAI_API_KEY": "sk-real-openai-key"},
		"run", "--quiet", "--base-url", ts.URL+"/v1", "--model", "m", "--requests", "2")
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, a := range auth {
		if a != "" {
			t.Errorf("a local server received Authorization %q from the default variable", a)
		}
	}
}

func TestRunFailures(t *testing.T) {
	t.Parallel()
	failing := mockServer(t, mock.Config{ErrorRate: 1, ErrorStatus: 404})

	out := invoke(t.Context(), t, nil, "run", "--base-url", failing.URL+"/v1", "--model", "m", "--requests", "3", "--warmup", "2")
	if out.code != 1 || !strings.Contains(out.stderr, "all 2 warmup requests failed") || !strings.Contains(out.stderr, "http_404") {
		t.Errorf("warmup failure: exit %d\n%s", out.code, out.stderr)
	}

	out = invoke(t.Context(), t, nil, "run", "--base-url", failing.URL+"/v1", "--model", "m", "--requests", "3")
	if out.code != 1 || !strings.Contains(out.stderr, "no request succeeded") || !strings.Contains(out.stdout, "3 x http_404") {
		t.Errorf("all requests failing: exit %d\nstdout: %s\nstderr: %s", out.code, out.stdout, out.stderr)
	}
}

func TestRunInterrupted(t *testing.T) {
	t.Parallel()
	slow := mockServer(t, mock.Config{TTFT: mock.Constant(time.Minute)})
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)
	out := invoke(ctx, t, nil, "run", "--quiet", "--base-url", slow.URL+"/v1", "--model", "m",
		"--concurrency", "2,4", "--duration", "1h")
	if out.code != 130 {
		t.Fatalf("exit %d, want 130\n%s", out.code, out.stderr)
	}
	// The two requests in flight were canceled: neither successes nor errors.
	for _, want := range []string{"**Interrupted**", "the 2 requests in flight were canceled", "| concurrency=2 | 0 | 0 |"} {
		if !strings.Contains(out.stdout, want) {
			t.Errorf("partial report lacks %q:\n%s", want, out.stdout)
		}
	}
	if strings.Contains(out.stdout, "concurrency=4") {
		t.Errorf("the second stage should not have started:\n%s", out.stdout)
	}
}

func TestRunUsageErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := []string{"run", "--base-url", "http://127.0.0.1:1/v1", "--model", "m"}
	tests := map[string][]string{
		"missing base URL":   {"run", "--model", "m"},
		"missing model":      {"run", "--base-url", "http://x/v1"},
		"bad base URL":       {"run", "--base-url", "localhost:8000", "--model", "m"},
		"closed and open":    append(base, "--concurrency", "2", "--rate", "1"),
		"zero concurrency":   append(base, "--concurrency", "1,0"),
		"bad rate":           append(base, "--rate", "fast"),
		"bad arrival":        append(base, "--rate", "1", "--arrival", "bursty"),
		"bad extension":      append(base, "--out", filepath.Join(dir, "r.txt")),
		"missing directory":  append(base, "--out", filepath.Join(dir, "nope", "r.json")),
		"bad extra body":     append(base, "--extra-body", "[1]"),
		"null extra body":    append(base, "--extra-body", "null"),
		"bad header":         append(base, "--header", "no colon"),
		"negative tokens":    append(base, "--max-tokens", "-1"),
		"bad token field":    append(base, "--max-tokens-field", "n_predict"),
		"negative duration":  append(base, "--duration", "-1s"),
		"bad temperature":    append(base, "--temperature", "hot"),
		"positional":         append(base, "extra"),
		"unknown flag":       append(base, "--nope"),
		"missing dataset":    append(base, "--dataset", filepath.Join(dir, "missing.jsonl")),
		"zero input tokens":  append(base, "--input-tokens", "0"),
		"too few tokens":     append(base, "--input-tokens", "17"),
		"negative timeout":   append(base, "--timeout", "-1s"),
		"negative in-flight": append(base, "--max-in-flight", "-1"),
	}
	for name, args := range tests {
		if out := invoke(t.Context(), t, nil, args...); out.code != 2 {
			t.Errorf("%s: exit %d, want 2\n%s", name, out.code, out.stderr)
		}
	}
}

// stage4 is a closed-loop stage at concurrency c with the given throughput
// and number of failed requests out of 10.
func stage4(c int, rps float64, failed int) report.Stage {
	return report.Stage{
		Load: loadgen.Load{Mode: loadgen.Closed, Concurrency: c}, Requests: 10, Succeeded: 10 - failed, Failed: failed,
		RequestsPerSecond: rps,
	}
}

func writeResult(t *testing.T, path string, interrupted bool, stages ...report.Stage) {
	t.Helper()
	r := &report.Result{SchemaVersion: report.SchemaVersion, Interrupted: interrupted, Stages: stages}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := report.WriteJSON(f, r); err != nil {
		t.Fatal(err)
	}
}

func TestCompare(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, head := filepath.Join(dir, "base.json"), filepath.Join(dir, "head.json")
	writeResult(t, base, false, stage4(4, 10, 0))
	writeResult(t, head, false, stage4(4, 8, 0))

	out := invoke(t.Context(), t, nil, "compare", base, head)
	if out.code != 0 || !strings.Contains(out.stdout, "| Req/s | 10.0 | 8.00 | -20.0% |") {
		t.Errorf("report-only compare: exit %d\n%s%s", out.code, out.stdout, out.stderr)
	}
	// Flags may follow the file names.
	out = invoke(t.Context(), t, nil, "compare", base, head, "--threshold", "10")
	if out.code != 1 || !strings.Contains(out.stdout, "regression") || !strings.Contains(out.stderr, "1 metric regressed") {
		t.Errorf("threshold compare: exit %d\n%s%s", out.code, out.stdout, out.stderr)
	}
	if out = invoke(t.Context(), t, nil, "compare", "--threshold", "25", base, head); out.code != 0 {
		t.Errorf("change within threshold: exit %d", out.code)
	}
	if out = invoke(t.Context(), t, nil, "compare", base); out.code != 2 {
		t.Errorf("one file: exit %d, want 2", out.code)
	}
	if out = invoke(t.Context(), t, nil, "compare", "--threshold", "-1", base, head); out.code != 2 {
		t.Errorf("negative threshold: exit %d, want 2", out.code)
	}
	if out = invoke(t.Context(), t, nil, "compare", "--error-threshold", "1", base, head); out.code != 2 {
		t.Errorf("--error-threshold without --threshold: exit %d, want 2", out.code)
	}
	if out = invoke(t.Context(), t, nil, "compare", base, filepath.Join(dir, "missing.json")); out.code != 1 {
		t.Errorf("missing file: exit %d, want 1", out.code)
	}
	notResult := filepath.Join(dir, "other.json")
	if err := os.WriteFile(notResult, []byte(`{"hello": "world"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out = invoke(t.Context(), t, nil, "compare", base, notResult); out.code != 1 || !strings.Contains(out.stderr, "schema_version") {
		t.Errorf("foreign JSON: exit %d\n%s", out.code, out.stderr)
	}
	if out = invoke(t.Context(), t, nil, "compare", "--", base, head); out.code != 0 {
		t.Errorf("-- separator: exit %d\n%s", out.code, out.stderr)
	}
}

// TestCompareGate checks the cases where compare --threshold must fail even
// though no matched metric regressed.
func TestCompareGate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := func(name string) string { return filepath.Join(dir, name) }
	writeResult(t, path("base.json"), false, stage4(1, 10, 0), stage4(8, 50, 0))
	writeResult(t, path("partial.json"), true, stage4(1, 10, 0))
	writeResult(t, path("other-load.json"), false, stage4(99, 10, 0))
	writeResult(t, path("errors.json"), false, stage4(1, 10, 2), stage4(8, 50, 0))
	tests := []struct {
		name   string
		args   []string
		code   int
		stderr string
	}{
		{"interrupted head missing a stage", []string{"partial.json"}, 1, "base stage concurrency=8 is missing from head; the head run was interrupted"},
		{"no stage matches", []string{"other-load.json"}, 1, "no stage loads match"},
		// Error rate 0% -> 20%: within 25 pp, beyond 10 pp.
		{"error rate within threshold", []string{"errors.json", "--threshold", "25"}, 0, ""},
		{"error rate beyond its own threshold", []string{"errors.json", "--threshold", "25", "--error-threshold", "10"}, 1, "1 metric regressed"},
	}
	for _, tt := range tests {
		args := []string{"compare", path("base.json"), path(tt.args[0]), "--threshold", "5"}
		if len(tt.args) > 1 {
			args = append([]string{"compare", path("base.json"), path(tt.args[0])}, tt.args[1:]...)
		}
		out := invoke(t.Context(), t, nil, args...)
		if out.code != tt.code || !strings.Contains(out.stderr, tt.stderr) {
			t.Errorf("%s: exit %d (want %d)\nstdout: %s\nstderr: %s", tt.name, out.code, tt.code, out.stdout, out.stderr)
		}
	}
}

func TestMockCommand(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Main(ctx, []string{"mock", "--addr", "127.0.0.1:0", "--ttft", "1ms", "--itl", "1ms", "--output-tokens", "3"},
			cli.Env{Stdout: &syncBuffer{}, Stderr: &stderr})
	}()
	addr := regexp.MustCompile(`serving (http://\S+/v1)`)
	var baseURL string
	for deadline := time.Now().Add(10 * time.Second); baseURL == "" && time.Now().Before(deadline); {
		if m := addr.FindStringSubmatch(stderr.String()); m != nil {
			baseURL = m[1]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if baseURL == "" {
		t.Fatalf("mock did not start:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "output tokens 3, slots unlimited)") {
		t.Errorf("startup line: %s", stderr.String())
	}
	out := invoke(t.Context(), t, nil, "run", "--quiet", "--base-url", baseURL, "--model", "m", "--requests", "2")
	if out.code != 0 || !strings.Contains(out.stdout, "| concurrency=1 | 2 | 0 |") {
		t.Errorf("run against mock command: exit %d\n%s%s", out.code, out.stdout, out.stderr)
	}
	cancel()
	if code := <-done; code != 0 || !strings.Contains(stderr.String(), "mock: stopped") {
		t.Errorf("mock exit %d\n%s", code, stderr.String())
	}
}

func TestMockCommandErrors(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"bad dist":       {"mock", "--ttft", "soon"},
		"bad rate":       {"mock", "--error-rate", "2"},
		"positional":     {"mock", "extra"},
		"bad count dist": {"mock", "--output-tokens", "uniform:9,1"},
	} {
		if out := invoke(t.Context(), t, nil, args...); out.code != 2 {
			t.Errorf("%s: exit %d, want 2\n%s", name, out.code, out.stderr)
		}
	}
	// A port that is already taken.
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	if out := invoke(t.Context(), t, nil, "mock", "--addr", strings.TrimPrefix(ts.URL, "http://")); out.code != 1 {
		t.Errorf("busy port: exit %d, want 1\n%s", out.code, out.stderr)
	}
}

func TestTopLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args     []string
		code     int
		inStdout string
		inStderr string
	}{
		{nil, 2, "", "Usage:"},
		{[]string{"help"}, 0, "Usage:", ""},
		{[]string{"--help"}, 0, "Usage:", ""},
		{[]string{"version"}, 0, "llm-loadtest test (go", ""},
		{[]string{"bogus"}, 2, "", `unknown command "bogus"`},
		{[]string{"run", "-h"}, 0, "", "--base-url URL --model NAME"},
		{[]string{"mock", "-h"}, 0, "", "Usage: llm-loadtest mock"},
		{[]string{"compare", "-h"}, 0, "", "Usage: llm-loadtest compare"},
	}
	for _, tt := range tests {
		out := invoke(t.Context(), t, nil, tt.args...)
		if out.code != tt.code || !strings.Contains(out.stdout, tt.inStdout) || !strings.Contains(out.stderr, tt.inStderr) {
			t.Errorf("%v: exit %d (want %d)\nstdout: %s\nstderr: %s", tt.args, out.code, tt.code, out.stdout, out.stderr)
		}
	}
	var stdout syncBuffer
	cli.Main(t.Context(), []string{"version"}, cli.Env{Stdout: &stdout, Stderr: &syncBuffer{}})
	if !strings.HasPrefix(stdout.String(), "llm-loadtest ") || strings.Contains(stdout.String(), "llm-loadtest  ") {
		t.Errorf("version without a linked version = %q", stdout.String())
	}
}
