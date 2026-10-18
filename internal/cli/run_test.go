package cli

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// planFor parses run flags the way runCommand does and plans the run.
func planFor(t *testing.T, env map[string]string, args ...string) *runPlan {
	t.Helper()
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o runOptions
	o.register(fs)
	if err := fs.Parse(append([]string{"--model", "m"}, args...)); err != nil {
		t.Fatal(err)
	}
	o.apiKeyEnvSet = isSet(fs, "api-key-env")
	p, err := o.plan(&Env{Getenv: func(k string) string { return env[k] }, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAPIKeySelection(t *testing.T) {
	t.Parallel()
	env := map[string]string{"OPENAI_API_KEY": "sk-openai-key", "VLLM_KEY": "vllm-key"}
	tests := []struct {
		name    string
		args    []string
		wantKey string
		note    string // substring of the only note, or "" for no note
	}{
		{"default key stays home", []string{"--base-url", "http://localhost:8000/v1"}, "", ""},
		{"default key not sent to third parties", []string{"--base-url", "https://openrouter.ai/api/v1"}, "", "no API key is sent"},
		{"default key for OpenAI", []string{"--base-url", "https://API.openai.com/v1"}, "sk-openai-key", "from $OPENAI_API_KEY to API.openai.com"},
		{"named key to loopback", []string{"--base-url", "http://127.0.0.1:8000/v1", "--api-key-env", "VLLM_KEY"}, "vllm-key", "sending the API key"},
		{"named key over plain http", []string{"--base-url", "http://10.0.0.5:8000/v1", "--api-key-env", "VLLM_KEY"}, "vllm-key", "unencrypted (http) to 10.0.0.5:8000"},
		{"named key over https", []string{"--base-url", "https://gpu.example.com/v1", "--api-key-env", "VLLM_KEY"}, "vllm-key", "sending the API key"},
		{"named variable unset", []string{"--base-url", "https://gpu.example.com/v1", "--api-key-env", "NOPE"}, "", "$NOPE is empty or unset"},
		{"empty name disables the default", []string{"--base-url", "https://api.openai.com/v1", "--api-key-env", ""}, "", "no API key is sent"},
		{"no key to loopback needs no note", []string{"--base-url", "http://[::1]:8000/v1", "--api-key-env", ""}, "", ""},
	}
	for _, tt := range tests {
		p := planFor(t, env, tt.args...)
		if p.client.APIKey != tt.wantKey {
			t.Errorf("%s: API key %q, want %q", tt.name, p.client.APIKey, tt.wantKey)
		}
		switch {
		case tt.note == "" && len(p.notes) != 0:
			t.Errorf("%s: notes %q, want none", tt.name, p.notes)
		case tt.note != "" && (len(p.notes) != 1 || !strings.Contains(p.notes[0], tt.note)):
			t.Errorf("%s: notes %q, want one containing %q", tt.name, p.notes, tt.note)
		}
		for _, n := range p.notes {
			for _, key := range env {
				if strings.Contains(n, key) {
					t.Errorf("%s: note %q repeats a key", tt.name, n)
				}
			}
		}
	}
}
