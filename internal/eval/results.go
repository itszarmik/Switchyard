package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"switchyard/internal/usage"
)

// Result is one line of a results file: the judgment of one candidate on
// one prompt. It holds no prompt or answer content.
type Result struct {
	// ID is the prompt's id and PromptSet the name of its set.
	ID        string `json:"id"`
	PromptSet string `json:"prompt_set"`
	// Model is the candidate's catalog model and Deployment the deployment
	// that answered.
	Model      string `json:"model"`
	Deployment string `json:"deployment"`
	// Reference and Judge are the deployments of the reference model and
	// of the judge.
	Reference string `json:"reference"`
	Judge     string `json:"judge"`
	// Category and Difficulty are the prompt's, from the prompt set or the
	// assessor.
	Category   string  `json:"category"`
	Difficulty float64 `json:"difficulty"`
	// Outcome is a judge.Outcome constant, and Acceptable whether it counts
	// as acceptable; both are empty when Error is set.
	Outcome    string `json:"outcome,omitempty"`
	Acceptable bool   `json:"acceptable"`
	// CandidateCostUSD, ReferenceCostUSD and JudgeCostUSD are what the
	// calls behind this line cost. The reference answer of a prompt is
	// charged to the first line written for it in a run.
	CandidateCostUSD usage.Money `json:"candidate_cost_usd"`
	ReferenceCostUSD usage.Money `json:"reference_cost_usd"`
	JudgeCostUSD     usage.Money `json:"judge_cost_usd"`
	// Error, when set, says which step failed and how (for example
	// "judge: the reply is not a verdict"); the line is then no verdict,
	// and a resumed run tries the pair again.
	Error string `json:"error,omitempty"`
	// Time is when the line was written.
	Time time.Time `json:"ts"`
}

// Key identifies a (prompt, candidate) pair.
type Key struct {
	PromptSet, ID, Model string
}

// Key returns the result's pair.
func (r *Result) Key() Key { return Key{PromptSet: r.PromptSet, ID: r.ID, Model: r.Model} }

// Judged reports whether the line is a verdict rather than a failure.
func (r *Result) Judged() bool { return r.Error == "" && r.Outcome != "" }

// ReadResults reads a JSONL results file. Blank lines are skipped. A final
// line without a newline that does not parse is a torn write, which an
// interrupted run can leave behind: it is ignored, and torn is its length
// in bytes (0 when there is none), so that a caller can warn and a resumed
// run can cut it off before appending. A line that does not parse anywhere
// else is an error.
func ReadResults(r io.Reader) (results []Result, torn int64, err error) {
	var out []Result
	br := bufio.NewReaderSize(r, 64<<10)
	for line := 1; ; line++ {
		text, err := br.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, fmt.Errorf("read results: %w", err)
		}
		complete := err == nil
		if len(text) > maxLineBytes {
			return nil, 0, fmt.Errorf("results line %d: longer than %d bytes", line, maxLineBytes)
		}
		if t := bytes.TrimSpace(text); len(t) > 0 {
			var res Result
			switch perr := json.Unmarshal(t, &res); {
			case perr == nil && res.ID != "" && res.Model != "":
				out = append(out, res)
			case !complete:
				// A torn last line; the pair is simply not done.
				return out, int64(len(text)), nil
			default:
				return nil, 0, fmt.Errorf("results line %d: not a result object", line)
			}
		}
		if !complete {
			return out, 0, nil
		}
	}
}

// LoadResults reads the results file at path (see ReadResults).
func LoadResults(path string) (results []Result, torn int64, err error) {
	f, err := os.Open(path) // #nosec G304 -- the operator names the file
	if err != nil {
		return nil, 0, fmt.Errorf("open the results: %w", err)
	}
	defer func() { _ = f.Close() }()
	res, torn, err := ReadResults(f)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", path, err)
	}
	return res, torn, nil
}

// Spent returns what the calls recorded in results cost in all: every
// line's candidate, reference and judge cost.
func Spent(results []Result) usage.Money {
	var total usage.Money
	for i := range results {
		r := &results[i]
		total = total.Add(r.CandidateCostUSD).Add(r.ReferenceCostUSD).Add(r.JudgeCostUSD)
	}
	return total
}

// Latest keeps the last line of every pair, in the order of their first
// appearance: a resumed run appends a new line for a pair whose earlier
// attempt failed.
func Latest(results []Result) []Result {
	index := make(map[Key]int, len(results))
	var out []Result
	for _, r := range results {
		if i, ok := index[r.Key()]; ok {
			out[i] = r
			continue
		}
		index[r.Key()] = len(out)
		out = append(out, r)
	}
	return out
}

// Completed returns the pairs that have a verdict in results.
func Completed(results []Result) map[Key]bool {
	done := make(map[Key]bool)
	for i := range results {
		if results[i].Judged() {
			done[results[i].Key()] = true
		}
	}
	return done
}
