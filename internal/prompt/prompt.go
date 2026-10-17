// Package prompt supplies the requests a load test sends: rows of a JSONL
// dataset, or synthetic prompts of a target length.
//
// Both sources are indexed by the request's sequence number rather than
// drawn from shared state, so they are safe for concurrent use and a run's
// prompts do not depend on goroutine scheduling.
package prompt

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strings"

	"github.com/aozmekik/llm-loadtest/internal/client"
)

// Dataset is a fixed list of requests, served in order and repeated as
// needed.
type Dataset struct {
	requests []client.Request
}

// row is one JSONL line. Unknown fields are ignored so that datasets can
// carry their own metadata.
type row struct {
	Prompt    string           `json:"prompt"`
	Messages  []client.Message `json:"messages"`
	MaxTokens int              `json:"max_tokens"`
}

// LoadJSONL reads a dataset from a JSONL file. See ReadJSONL for the format.
func LoadJSONL(path string) (*Dataset, error) {
	f, err := os.Open(path) //nolint:gosec // The path is the dataset the user asked for.
	if err != nil {
		return nil, fmt.Errorf("open dataset: %w", err)
	}
	defer func() { _ = f.Close() }()
	d, err := ReadJSONL(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return d, nil
}

// ReadJSONL reads a dataset with one JSON object per line. Each object has
// either a "prompt" string, sent as a single user message, or a "messages"
// array of {"role", "content"} objects, and optionally "max_tokens".
// Blank lines are skipped.
func ReadJSONL(r io.Reader) (*Dataset, error) {
	br := bufio.NewReader(r)
	var d Dataset
	for lineNo := 1; ; lineNo++ {
		line, err := br.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read dataset: %w", err)
		}
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			req, perr := parseRow(trimmed)
			if perr != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, perr)
			}
			d.requests = append(d.requests, req)
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if len(d.requests) == 0 {
		return nil, errors.New("dataset has no prompts")
	}
	return &d, nil
}

func parseRow(line []byte) (client.Request, error) {
	var r row
	if err := json.Unmarshal(line, &r); err != nil {
		return client.Request{}, fmt.Errorf("invalid JSON: %w", err)
	}
	switch {
	case r.Prompt != "" && len(r.Messages) > 0:
		return client.Request{}, errors.New(`set "prompt" or "messages", not both`)
	case r.Prompt != "":
		r.Messages = []client.Message{{Role: "user", Content: r.Prompt}}
	case len(r.Messages) == 0:
		return client.Request{}, errors.New(`need a non-empty "prompt" or "messages"`)
	}
	for i, m := range r.Messages {
		if m.Role == "" {
			return client.Request{}, fmt.Errorf("message %d has no role", i)
		}
	}
	if r.MaxTokens < 0 {
		return client.Request{}, errors.New(`"max_tokens" must not be negative`)
	}
	return client.Request{Messages: r.Messages, MaxTokens: r.MaxTokens}, nil
}

// Len returns the number of requests in the dataset.
func (d *Dataset) Len() int { return len(d.requests) }

// Shuffle permutes the dataset deterministically for the given seed.
func (d *Dataset) Shuffle(seed uint64) {
	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	rng.Shuffle(len(d.requests), func(i, j int) {
		d.requests[i], d.requests[j] = d.requests[j], d.requests[i]
	})
}

// Prompt returns request number i, cycling through the dataset. The
// returned messages are shared and must not be modified.
func (d *Dataset) Prompt(i uint64) client.Request {
	return d.requests[i%uint64(len(d.requests))]
}

// instruction ends every synthetic prompt. Putting the random words first
// keeps prompts from sharing a prefix, which would let a server's prefix
// cache skip work and flatter the measured time to first token.
const instruction = "\n\nContinue the list of words above. Write as many more words as you can."

// instructionTokens approximates the length of instruction for common
// byte-pair encodings.
const instructionTokens = 16

// Synthetic generates distinct prompts of an approximate token length from
// a vocabulary of common English words, most of which are a single token
// in widely used tokenizers. Exact lengths depend on the model's tokenizer
// and chat template; the server-reported prompt token count is the ground
// truth.
type Synthetic struct {
	inputTokens int
	seed        uint64
}

// NewSynthetic returns a source of prompts of about inputTokens tokens.
func NewSynthetic(inputTokens int, seed uint64) (*Synthetic, error) {
	if inputTokens < 1 {
		return nil, errors.New("synthetic prompt length must be at least 1 token")
	}
	return &Synthetic{inputTokens: inputTokens, seed: seed}, nil
}

// Prompt returns synthetic request number i. The same seed and i always
// produce the same prompt.
func (s *Synthetic) Prompt(i uint64) client.Request {
	rng := rand.New(rand.NewPCG(s.seed, i))
	n := max(s.inputTokens-instructionTokens, 1)
	var b strings.Builder
	b.Grow(n * 7)
	for j := range n {
		if j > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(vocabulary[rng.IntN(len(vocabulary))])
	}
	b.WriteString(instruction)
	return client.Request{Messages: []client.Message{{Role: "user", Content: b.String()}}}
}

// String describes the source for reports.
func (s *Synthetic) String() string {
	return fmt.Sprintf("synthetic (~%d input tokens, seed %d)", s.inputTokens, s.seed)
}

var vocabulary = strings.Fields(`
able about above after again against air all also always among and animal
answer any area around away back ball because become before begin behind best
better between big bird black blue boat body book both bring build but call came
car carry cause center change city class clear close cold come common could
country course cut dark day deep did different does dog done door down draw
dry during each early earth east eat end enough even ever every eye face fact
fall family far farm fast father feel field fill find fire first fish five
follow food foot force form found four free friend from front full game gave
general girl give glass gold good great green ground group grow half hand happy
hard have head hear heart heat help here high hold home horse hot hour house
idea inch island just keep kind king land large last late learn leave left
less letter life light line list little live long look love low machine main
make man many map mark measure might mile mind minute miss money moon more
morning most mother mountain move much music must name near need never new
next night north note nothing notice number object ocean office often old once
only open order other over page paper part pass people perhaps person picture
piece place plan plant play point power present pull question quick rain read
ready real record red remember rest right river road rock room round rule run
same school sea second see serve set shape ship short show side simple since
sing size sleep slow small snow some song soon sound south space speak special
spring stand star start state step still stone stop story street strong study
such summer sun sure table take talk teach ten test than that their them then
there these thing think three through time together too took town tree true
turn under unit until upon very voice walk wall want warm watch water wave
weather week well west what wheel where which while white whole wide wind
window winter with wood word work world write year young`)
