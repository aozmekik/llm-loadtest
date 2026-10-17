package prompt_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/aozmekik/llm-loadtest/internal/client"
	"github.com/aozmekik/llm-loadtest/internal/prompt"
)

func TestReadJSONL(t *testing.T) {
	t.Parallel()
	input := `{"prompt": "What is TCP slow start?", "id": "q1"}

{"messages": [{"role": "system", "content": "Be brief."}, {"role": "user", "content": "Define ITL."}], "max_tokens": 64}
   {"prompt": "no trailing newline"}`
	d, err := prompt.ReadJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if d.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", d.Len())
	}
	first := d.Prompt(0)
	if len(first.Messages) != 1 || first.Messages[0] != (client.Message{Role: "user", Content: "What is TCP slow start?"}) {
		t.Errorf("prompt row = %+v", first)
	}
	second := d.Prompt(1)
	if len(second.Messages) != 2 || second.Messages[0].Role != "system" || second.MaxTokens != 64 {
		t.Errorf("messages row = %+v", second)
	}
	if d.Prompt(3).Messages[0].Content != first.Messages[0].Content {
		t.Error("Prompt does not cycle through the dataset")
	}
}

func TestReadJSONLErrors(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"empty":                "",
		"blank lines only":     "\n  \n",
		"invalid JSON":         `{"prompt": "ok"}` + "\n{oops}",
		"neither field":        `{"text": "hi"}`,
		"both fields":          `{"prompt": "a", "messages": [{"role": "user", "content": "b"}]}`,
		"message without role": `{"messages": [{"content": "b"}]}`,
		"negative max_tokens":  `{"prompt": "a", "max_tokens": -1}`,
	}
	for name, input := range tests {
		if _, err := prompt.ReadJSONL(strings.NewReader(input)); err == nil {
			t.Errorf("%s: ReadJSONL succeeded, want an error", name)
		}
	}
	_, err := prompt.ReadJSONL(strings.NewReader(`{"prompt": "ok"}` + "\n\n" + `{"prompt": 3}`))
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error = %v, want it to name line 3", err)
	}
	boom := errors.New("disk on fire")
	if _, err := prompt.ReadJSONL(iotest.ErrReader(boom)); !errors.Is(err, boom) {
		t.Errorf("read error = %v, want %v", err, boom)
	}
}

func TestLoadJSONL(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "prompts.jsonl")
	if err := os.WriteFile(path, []byte(`{"prompt": "a"}`+"\n"+`{"prompt": "b"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := prompt.LoadJSONL(path)
	if err != nil || d.Len() != 2 {
		t.Fatalf("LoadJSONL = %v, %v", d, err)
	}
	if _, err := prompt.LoadJSONL(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Error("LoadJSONL of a missing file succeeded")
	}
}

func TestShuffleIsDeterministic(t *testing.T) {
	t.Parallel()
	lines := make([]string, 0, 20)
	for i := range 20 {
		lines = append(lines, `{"prompt": "p`+string(rune('a'+i))+`"}`)
	}
	input := strings.Join(lines, "\n")
	order := func(seed uint64) string {
		d, err := prompt.ReadJSONL(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		d.Shuffle(seed)
		var b strings.Builder
		for i := range d.Len() {
			b.WriteString(d.Prompt(uint64(i)).Messages[0].Content)
		}
		return b.String()
	}
	if first, second := order(1), order(1); first != second {
		t.Error("same seed produced different orders")
	}
	if order(1) == order(2) {
		t.Error("different seeds produced the same order")
	}
}

func TestSynthetic(t *testing.T) {
	t.Parallel()
	s, err := prompt.NewSynthetic(256, 7)
	if err != nil {
		t.Fatal(err)
	}
	a, b := s.Prompt(0), s.Prompt(1)
	if a.Messages[0].Content == b.Messages[0].Content {
		t.Error("consecutive synthetic prompts are identical")
	}
	if again := s.Prompt(0); again.Messages[0].Content != a.Messages[0].Content {
		t.Error("synthetic prompt is not reproducible for the same index")
	}
	if a.Messages[0].Role != "user" || a.MaxTokens != 0 {
		t.Errorf("unexpected request shape %+v", a)
	}
	// 256 tokens minus the instruction's 16, at one word each.
	body, _, _ := strings.Cut(a.Messages[0].Content, "\n\n")
	if n := len(strings.Fields(body)); n != 240 {
		t.Errorf("prompt has %d words before the instruction, want 240", n)
	}
	other, _ := prompt.NewSynthetic(256, 8)
	if other.Prompt(0).Messages[0].Content == a.Messages[0].Content {
		t.Error("different seeds produced the same prompt")
	}
	tiny, _ := prompt.NewSynthetic(1, 1)
	if body, _, _ := strings.Cut(tiny.Prompt(0).Messages[0].Content, "\n\n"); len(strings.Fields(body)) != 1 {
		t.Error("a 1-token target should still produce one word")
	}
	if _, err := prompt.NewSynthetic(0, 1); err == nil {
		t.Error("NewSynthetic(0) succeeded")
	}
	if !strings.Contains(s.String(), "256") {
		t.Errorf("String() = %q", s.String())
	}
}
