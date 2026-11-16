package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"switchyard/internal/quality"
	"switchyard/internal/schema"
)

// maxLineBytes bounds one line of a prompt set or a results file.
const maxLineBytes = 16 << 20

// Prompt is one line of a prompt set: {"id":…,"request":{…},"category"?:…}.
type Prompt struct {
	// ID identifies the prompt within its set.
	ID string `json:"id"`
	// Request is a chat completions request body.
	Request json.RawMessage `json:"request"`
	// Category, when set, is the prompt's quality category; otherwise the
	// assessor's category is used. The difficulty always comes from the
	// assessor.
	Category string `json:"category,omitempty"`
}

// ReadPrompts reads a JSONL prompt set. Blank lines are skipped. Every id
// must be unique and non-empty, every request a valid chat request and
// every category, when set, a known one. Errors name the line, never its
// content.
func ReadPrompts(r io.Reader) ([]Prompt, error) {
	var out []Prompt
	seen := make(map[string]int)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for line := 1; sc.Scan(); line++ {
		text := bytes.TrimSpace(sc.Bytes())
		if len(text) == 0 {
			continue
		}
		var p Prompt
		if err := json.Unmarshal(text, &p); err != nil {
			return nil, fmt.Errorf("line %d: not a JSON prompt object", line)
		}
		switch {
		case p.ID == "":
			return nil, fmt.Errorf("line %d: missing id", line)
		case seen[p.ID] > 0:
			return nil, fmt.Errorf("line %d: duplicate id %q (first on line %d)", line, p.ID, seen[p.ID])
		case len(p.Request) == 0:
			return nil, fmt.Errorf("line %d: missing request", line)
		case p.Category != "" && !quality.IsCategory(p.Category):
			return nil, fmt.Errorf("line %d: unknown category %q", line, p.Category)
		}
		if _, err := schema.DecodeChatRequest(p.Request); err != nil {
			var re *schema.RequestError
			if errors.As(err, &re) {
				return nil, fmt.Errorf("line %d: invalid request: %s", line, re.Message)
			}
			return nil, fmt.Errorf("line %d: invalid request", line)
		}
		seen[p.ID] = line
		out = append(out, p)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read prompts: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("the prompt set is empty")
	}
	return out, nil
}

// LoadPrompts reads the prompt set at path. Its name, recorded in results
// and override sources, is the file name without its extension.
func LoadPrompts(path string) (name string, prompts []Prompt, err error) {
	f, err := os.Open(path) // #nosec G304 -- the operator names the file
	if err != nil {
		return "", nil, fmt.Errorf("open the prompt set: %w", err)
	}
	defer func() { _ = f.Close() }()
	prompts, err = ReadPrompts(f)
	if err != nil {
		return "", nil, fmt.Errorf("prompt set %s: %w", path, err)
	}
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base)), prompts, nil
}
