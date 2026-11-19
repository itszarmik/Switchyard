package judge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/judge"
	"switchyard/internal/providers"
	"switchyard/internal/providers/anthropic"
	"switchyard/internal/providers/openai"
	"switchyard/internal/schema"
	"switchyard/test/eval/fakejudge"
	"switchyard/test/fakeanthropic"
)

// Answers of the test pairs.
const (
	candText = "CANDIDATE: 42"
	refText  = "REFERENCE: 42"
)

// testDeployment loads a one-provider catalog whose model "judge" is
// served at baseURL and returns its deployment and adapter.
func testDeployment(t *testing.T, typ, baseURL, caps string) (*catalog.Deployment, providers.Adapter) {
	t.Helper()
	text := fmt.Sprintf(`version: 1
providers:
  - {id: p, type: %s, base_url: %q}
models:
  - id: judge
    capabilities: [%s]
    deployments:
      - {provider: p, upstream_model: judge-up, pricing: {input: 1, output: 2}}
`, typ, baseURL, caps)
	path := filepath.Join(t.TempDir(), "switchyard.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := config.Load(path, config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := snap.Config()
	cat, err := catalog.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cat.Resolve("judge@p")
	if err != nil {
		t.Fatal(err)
	}
	var a providers.Adapter
	if typ == "anthropic" {
		a, err = anthropic.New(cfg.Providers[0], providers.Options{})
	} else {
		a, err = openai.New(cfg.Providers[0], providers.Options{})
	}
	if err != nil {
		t.Fatal(err)
	}
	return res.Candidates[0], a
}

// newJudge starts a fake answering with script and returns a judge on it.
func newJudge(t *testing.T, caps string, script fakejudge.Script) (*judge.LLM, *fakejudge.Server) {
	t.Helper()
	srv := fakejudge.New(script)
	t.Cleanup(srv.Close)
	d, a := testDeployment(t, "openai", srv.BaseURL(), caps)
	j, err := judge.NewLLM(judge.Options{Adapter: a, Deployment: d})
	if err != nil {
		t.Fatal(err)
	}
	return j, srv
}

// testPair is a pair whose request asks a question.
func testPair(t *testing.T) judge.Pair {
	t.Helper()
	req, err := schema.DecodeChatRequest([]byte(`{"model":"m","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"What is 6*7?"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return judge.Pair{Request: req, Category: "reasoning", Candidate: candText, Reference: refText}
}

// pref is what a scripted run prefers.
type pref string

const (
	cand pref = "candidate"
	ref  pref = "reference"
	tie  pref = "tie"
)

// letter returns the winner that names p for request r.
func letter(p pref, r fakejudge.Request) string {
	switch {
	case p == tie:
		return "tie"
	case (p == cand) == (r.A == candText):
		return "A"
	default:
		return "B"
	}
}

func TestSwapMapping(t *testing.T) {
	tests := []struct {
		first, second pref
		outcome       string
		acceptable    bool
	}{
		{tie, tie, judge.OutcomeEquivalent, true},
		{cand, tie, judge.OutcomeCandidateBetter, true},
		{tie, cand, judge.OutcomeCandidateBetter, true},
		{cand, cand, judge.OutcomeCandidateBetter, true},
		{ref, ref, judge.OutcomeReferenceBetter, false},
		{ref, tie, judge.OutcomeReferenceBetter, false},
		{tie, ref, judge.OutcomeReferenceBetter, false},
		{cand, ref, judge.OutcomeInconsistent, false},
		{ref, cand, judge.OutcomeInconsistent, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.first)+"/"+string(tt.second), func(t *testing.T) {
			var n atomic.Int32
			j, srv := newJudge(t, "", func(r fakejudge.Request) fakejudge.Reply {
				p := tt.first
				if n.Add(1) == 2 {
					p = tt.second
				}
				return fakejudge.Reply{Content: fakejudge.Verdict(letter(p, r))}
			})
			v, err := j.Judge(context.Background(), testPair(t))
			if err != nil {
				t.Fatalf("Judge: %v", err)
			}
			if v.Outcome != tt.outcome || v.Acceptable != tt.acceptable || v.JudgeModel != "judge@p" {
				t.Errorf("verdict = %+v, want outcome %s, acceptable %v, judge judge@p", v, tt.outcome, tt.acceptable)
			}
			reqs := srv.Requests()
			if len(reqs) != 2 {
				t.Fatalf("%d judge calls, want 2", len(reqs))
			}
			if reqs[0].A != candText || reqs[0].B != refText || reqs[1].A != refText || reqs[1].B != candText {
				t.Errorf("positions not swapped: run 1 A=%q B=%q, run 2 A=%q B=%q", reqs[0].A, reqs[0].B, reqs[1].A, reqs[1].B)
			}
		})
	}
}

func TestParseRobustnessAndRetry(t *testing.T) {
	tests := []struct {
		name    string
		replies []string // in call order; the last repeats
		calls   int
		outcome string
		err     error
	}{
		{name: "fenced", replies: []string{"```json\n{\"winner\": \"tie\", \"reason\": \"same\"}\n```"}, calls: 2, outcome: judge.OutcomeEquivalent},
		{name: "prose", replies: []string{"Both are right.\n\n{\"winner\": \"tie\", \"reason\": \"same\"}"}, calls: 2, outcome: judge.OutcomeEquivalent},
		{name: "retry once", replies: []string{"I think A.", `{"winner":"tie","reason":"r"}`}, calls: 3, outcome: judge.OutcomeEquivalent},
		{name: "retry on empty reply", replies: []string{"", `{"winner":"tie"}`}, calls: 3, outcome: judge.OutcomeEquivalent},
		{name: "two failures", replies: []string{"no idea", "still no idea"}, calls: 2, err: judge.ErrUnparseable},
		{name: "second run fails twice", replies: []string{`{"winner":"tie"}`, "?", "??"}, calls: 3, err: judge.ErrUnparseable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var n atomic.Int32
			j, srv := newJudge(t, "", func(fakejudge.Request) fakejudge.Reply {
				i := min(int(n.Add(1))-1, len(tt.replies)-1)
				return fakejudge.Reply{Content: tt.replies[i], PromptTokens: 1000, CompletionTokens: 50}
			})
			v, err := j.Judge(context.Background(), testPair(t))
			if !errors.Is(err, tt.err) {
				t.Fatalf("Judge error = %v, want %v", err, tt.err)
			}
			if got, _ := srv.Counts(); got != tt.calls {
				t.Errorf("%d judge calls, want %d", got, tt.calls)
			}
			if err == nil && v.Outcome != tt.outcome {
				t.Errorf("outcome %s, want %s", v.Outcome, tt.outcome)
			}
			// 1000 input tokens at $1/M plus 50 output at $2/M is $0.0011
			// per call, also when the verdict fails.
			if want := int64(tt.calls) * 1_100_000; v.CostNano != want {
				t.Errorf("CostNano = %d, want %d", v.CostNano, want)
			}
			if err != nil && (strings.Contains(err.Error(), candText) || strings.Contains(err.Error(), "idea")) {
				t.Errorf("error carries content: %v", err)
			}
		})
	}
}

// TestUpstreamFailure: a failed call is charged nothing only when it was
// refused (a 4xx); a call that may have been billed (a 5xx, a timeout) is
// charged its estimate, so that callers never record less than the
// provider could bill.
func TestUpstreamFailure(t *testing.T) {
	tests := []struct {
		name     string
		reply    fakejudge.Reply
		kind     providers.Kind
		estimate bool
	}{
		{"refused", fakejudge.Reply{Status: 400}, providers.KindClientError, false},
		{"rate limited", fakejudge.Reply{Status: 429}, providers.KindRateLimited, false},
		{"server error", fakejudge.Reply{Status: 500}, providers.KindServerError, true},
		{"timeout", fakejudge.Reply{Content: fakejudge.Verdict("tie"), Delay: 2 * time.Second}, providers.KindTimeout, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fakejudge.New(func(fakejudge.Request) fakejudge.Reply { return tt.reply })
			t.Cleanup(srv.Close)
			d, a := testDeployment(t, "openai", srv.BaseURL(), "")
			j, err := judge.NewLLM(judge.Options{Adapter: a, Deployment: d, Timeout: 50 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			p := testPair(t)
			v, err := j.Judge(context.Background(), p)
			var pe *providers.Error
			if !errors.As(err, &pe) || pe.Kind != tt.kind {
				t.Fatalf("Judge error = %v, want a %s *providers.Error", err, tt.kind)
			}
			want := int64(0)
			if tt.estimate {
				want = j.CallCostNano(p)
			}
			if v.Outcome != "" || v.CostNano != want || want < 0 {
				t.Errorf("verdict = %+v, want no outcome and CostNano %d", v, want)
			}
			if got, _ := srv.Counts(); got != 1 {
				t.Errorf("%d calls, want 1 (upstream failures are not retried by the judge)", got)
			}
		})
	}
}

func TestCost(t *testing.T) {
	t.Run("the estimate without usage", func(t *testing.T) {
		j, _ := newJudge(t, "", func(fakejudge.Request) fakejudge.Reply {
			return fakejudge.Reply{Content: fakejudge.Verdict("tie"), NoUsage: true}
		})
		p := testPair(t)
		v, err := j.Judge(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		// Both runs' requests have the same size: the answers swap places.
		if est := j.CallCostNano(p); est <= 0 || v.CostNano != 2*est {
			t.Errorf("CostNano = %d, want 2 × the estimate %d", v.CostNano, est)
		}
	})
	t.Run("the estimate without usage, unparseable replies too", func(t *testing.T) {
		j, srv := newJudge(t, "", func(fakejudge.Request) fakejudge.Reply {
			return fakejudge.Reply{Content: "not a verdict", NoUsage: true}
		})
		p := testPair(t)
		v, err := j.Judge(context.Background(), p)
		if !errors.Is(err, judge.ErrUnparseable) {
			t.Fatalf("Judge error = %v, want ErrUnparseable", err)
		}
		if calls, _ := srv.Counts(); calls != 2 || v.CostNano != 2*j.CallCostNano(p) {
			t.Errorf("%d calls, CostNano %d; want 2 calls at the estimate %d", calls, v.CostNano, j.CallCostNano(p))
		}
	})
	t.Run("estimate covers the default usage", func(t *testing.T) {
		j, _ := newJudge(t, "", func(r fakejudge.Request) fakejudge.Reply {
			return fakejudge.Reply{Content: fakejudge.Verdict("tie")}
		})
		p := testPair(t)
		v, err := j.Judge(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		est := j.CallCostNano(p)
		if est <= 0 || v.CostNano > 2*est {
			t.Errorf("CostNano %d for 2 calls, estimate %d per call", v.CostNano, est)
		}
		// The estimate assumes the full output budget, and one input
		// token per byte of the request.
		if min := int64(judge.DefaultMaxTokens)*2000/1000 + int64(len(candText)+len(refText))*1000/1000; est < min {
			t.Errorf("estimate %d is below the output budget's cost %d", est, min)
		}
	})
	if judge.DefaultMaxTokens != 256 {
		t.Errorf("DefaultMaxTokens = %d, want the documented 256", judge.DefaultMaxTokens)
	}
}

// meter records a judgment's reservations and settlements, and refuses
// the reservation numbered refuse (from 1), if set.
type meter struct {
	refuse   int
	reserved []int64
	settled  []int64
}

var errNoRoom = errors.New("no room")

func (m *meter) Reserve(_ context.Context, est int64) error {
	if len(m.reserved)+1 == m.refuse {
		return errNoRoom
	}
	m.reserved = append(m.reserved, est)
	return nil
}

func (m *meter) Settle(cost int64) { m.settled = append(m.settled, cost) }

// TestJudgeMetered: every upstream call, retries included, is reserved at
// its estimate before it is made and settled at its charge after; a
// refused reservation stops the judgment before the call.
func TestJudgeMetered(t *testing.T) {
	tests := []struct {
		name    string
		replies []string
		refuse  int
		calls   int
		err     error
	}{
		{name: "two runs", replies: []string{`{"winner":"tie"}`}, calls: 2},
		{name: "every retry", replies: []string{"?", `{"winner":"tie"}`, "?", `{"winner":"A"}`}, calls: 4},
		{name: "refused before the first call", replies: []string{`{"winner":"tie"}`}, refuse: 1, calls: 0, err: errNoRoom},
		{name: "refused before the second run", replies: []string{`{"winner":"tie"}`}, refuse: 2, calls: 1, err: errNoRoom},
		{name: "refused before a retry", replies: []string{"?"}, refuse: 2, calls: 1, err: errNoRoom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var n atomic.Int32
			j, srv := newJudge(t, "", func(fakejudge.Request) fakejudge.Reply {
				i := min(int(n.Add(1))-1, len(tt.replies)-1)
				return fakejudge.Reply{Content: tt.replies[i], PromptTokens: 1000, CompletionTokens: 50}
			})
			m := &meter{refuse: tt.refuse}
			p := testPair(t)
			v, err := j.JudgeMetered(context.Background(), p, m)
			if !errors.Is(err, tt.err) {
				t.Fatalf("JudgeMetered error = %v, want %v", err, tt.err)
			}
			if got, _ := srv.Counts(); got != tt.calls || len(m.reserved) != tt.calls || len(m.settled) != tt.calls {
				t.Fatalf("%d calls, %d reserved, %d settled; want %d of each", got, len(m.reserved), len(m.settled), tt.calls)
			}
			var sum int64
			for i := range tt.calls {
				if m.reserved[i] != j.CallCostNano(p) || m.settled[i] != 1_100_000 || m.settled[i] > m.reserved[i] {
					t.Errorf("call %d: reserved %d, settled %d; want the estimate %d and 1100000", i, m.reserved[i], m.settled[i], j.CallCostNano(p))
				}
				sum += m.settled[i]
			}
			if v.CostNano != sum {
				t.Errorf("CostNano = %d, want the settled %d", v.CostNano, sum)
			}
		})
	}
}

func TestRequestShape(t *testing.T) {
	tests := []struct {
		name, caps     string
		temperature    bool
		responseFormat string
	}{
		{name: "plain"},
		{name: "sampling", caps: "sampling", temperature: true},
		{name: "json mode", caps: "json_mode", responseFormat: "json_object"},
		{name: "json schema", caps: "json_mode, json_schema, sampling", temperature: true, responseFormat: "json_schema"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j, srv := newJudge(t, tt.caps, func(fakejudge.Request) fakejudge.Reply {
				return fakejudge.Reply{Content: fakejudge.Verdict("tie")}
			})
			if _, err := j.Judge(context.Background(), testPair(t)); err != nil {
				t.Fatal(err)
			}
			r := srv.Requests()[0]
			var body struct {
				Model          string          `json:"model"`
				Temperature    *float64        `json:"temperature"`
				MaxTokens      int64           `json:"max_completion_tokens"`
				Stream         bool            `json:"stream"`
				ResponseFormat json.RawMessage `json:"response_format"`
			}
			if err := json.Unmarshal(r.Body, &body); err != nil {
				t.Fatal(err)
			}
			if body.Model != "judge-up" || body.MaxTokens != judge.DefaultMaxTokens || body.Stream {
				t.Errorf("model %q, max_completion_tokens %d, stream %v", body.Model, body.MaxTokens, body.Stream)
			}
			if got := body.Temperature != nil && *body.Temperature == 0; got != tt.temperature || (!tt.temperature && body.Temperature != nil) {
				t.Errorf("temperature = %v, want 0: %v", body.Temperature, tt.temperature)
			}
			var rf struct{ Type string }
			_ = json.Unmarshal(body.ResponseFormat, &rf)
			if rf.Type != tt.responseFormat {
				t.Errorf("response_format type %q, want %q", rf.Type, tt.responseFormat)
			}
			for _, want := range []string{"What is 6*7?", "[system]", "Be brief.", "The final answer is correct"} {
				if !strings.Contains(r.System+r.User, want) {
					t.Errorf("judge prompt lacks %q", want)
				}
			}
			if strings.Contains(r.System, "reference") || strings.Contains(r.System, "candidate") {
				t.Error("the judge prompt must not say which answer is the reference")
			}
		})
	}
}

func TestAnthropicJudge(t *testing.T) {
	srv := fakeanthropic.New(fakeanthropic.Options{})
	defer srv.Close()
	msg := func(winner string) []byte {
		return []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"judge-up","content":[{"type":"text","text":` +
			fmt.Sprintf("%q", "```json\n"+fakejudge.Verdict(winner)+"\n```") +
			`}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2000,"output_tokens":100}}`)
	}
	// The candidate is A in the first run and B in the second.
	srv.Sequence(fakeanthropic.Behavior{Replay: msg("A")}, fakeanthropic.Behavior{Replay: msg("tie")})
	d, a := testDeployment(t, "anthropic", srv.BaseURL(), "")
	j, err := judge.NewLLM(judge.Options{Adapter: a, Deployment: d, MaxTokens: 300})
	if err != nil {
		t.Fatal(err)
	}
	v, err := j.Judge(context.Background(), testPair(t))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if v.Outcome != judge.OutcomeCandidateBetter || !v.Acceptable {
		t.Errorf("verdict %+v, want candidate_better", v)
	}
	// Two calls of 2000 input tokens at $1/M and 100 output at $2/M.
	if v.CostNano != 2*2_200_000 {
		t.Errorf("CostNano = %d, want %d", v.CostNano, 2*2_200_000)
	}
	reqs := srv.Requests()
	if len(reqs) != 2 || !strings.Contains(string(reqs[0].Body), `"max_tokens":300`) {
		t.Errorf("requests: %d; want 2 with max_tokens 300", len(reqs))
	}
}

func TestConcurrentJudging(t *testing.T) {
	j, _ := newJudge(t, "", func(r fakejudge.Request) fakejudge.Reply {
		return fakejudge.Reply{Content: fakejudge.Verdict(letter(cand, r))}
	})
	p := testPair(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			v, err := j.Judge(context.Background(), p)
			if err == nil && v.Outcome != judge.OutcomeCandidateBetter {
				err = fmt.Errorf("outcome %s", v.Outcome)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestNewLLMValidation(t *testing.T) {
	d, a := testDeployment(t, "openai", "http://127.0.0.1:1/v1", "")
	tests := []struct {
		name string
		opts judge.Options
	}{
		{name: "no adapter", opts: judge.Options{Deployment: d}},
		{name: "no deployment", opts: judge.Options{Adapter: a}},
		{name: "negative max tokens", opts: judge.Options{Adapter: a, Deployment: d, MaxTokens: -1}},
		{name: "negative timeout", opts: judge.Options{Adapter: a, Deployment: d, Timeout: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := judge.NewLLM(tt.opts); err == nil {
				t.Error("NewLLM: want an error")
			}
		})
	}
}
