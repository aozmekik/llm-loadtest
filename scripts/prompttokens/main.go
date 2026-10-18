// Command prompttokens prints synthetic prompts as JSON lines so that
// check.py can count their tokens with tiktoken. Together they check the
// claim that synthetic prompts are exactly --input-tokens long in the
// cl100k_base and o200k_base encodings:
//
//	python3 -m venv .venv
//	.venv/bin/pip install -r scripts/prompttokens/requirements.txt
//	go run ./scripts/prompttokens | .venv/bin/python scripts/prompttokens/check.py
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aozmekik/llm-loadtest/internal/prompt"
)

// line is one prompt and the length it was generated for.
type line struct {
	Target int    `json:"target"`
	Seed   uint64 `json:"seed"`
	Seq    uint64 `json:"seq"`
	Text   string `json:"text"`
}

func main() {
	lengths := flag.String("lengths", "18,19,20,32,64,128,256,512,1000,2048,4096", "comma-separated target lengths")
	seeds := flag.Uint64("seeds", 3, "number of seeds, starting at 1")
	count := flag.Uint64("count", 20, "prompts per length and seed")
	flag.Parse()
	if err := run(*lengths, *seeds, *count); err != nil {
		fmt.Fprintln(os.Stderr, "prompttokens:", err)
		os.Exit(1)
	}
}

func run(lengths string, seeds, count uint64) error {
	enc := json.NewEncoder(os.Stdout)
	for field := range strings.SplitSeq(lengths, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil {
			return fmt.Errorf("length %q: %w", field, err)
		}
		for seed := range seeds {
			s, err := prompt.NewSynthetic(n, seed+1)
			if err != nil {
				return err
			}
			for seq := range count {
				text := s.Prompt(seq).Messages[0].Content
				if err := enc.Encode(line{Target: n, Seed: seed + 1, Seq: seq, Text: text}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
