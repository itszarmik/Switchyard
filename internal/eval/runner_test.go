package eval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/eval"
	"switchyard/internal/judge"
	"switchyard/internal/quality"
	"switchyard/internal/usage"
	"switchyard/test/eval/fakejudge"
)

// Answers the fake gives. The judge prefers good answers; secrets mark
// text that must never reach the results.
const (
	goodAnswer = "GOOD answer with secret-answer-text"
	badAnswer  = "BAD answer with secret-answer-text"
)

// testEnv is a catalog served by one fake that answers for every model
// and judges.
type testEnv struct {
	srv *fakejudge.Server
	cat *catalog.Catalog
	cfg *config.Config
	opt eval.Options
}

// runnerConfig serves models small, mid, large and judge from baseURL.
// Model "notools" lacks the tools capability.
func runnerConfig(baseURL string) string {
	return fmt.Sprintf(`version: 1
providers:
  - {id: fake, type: openai, base_url: %q}
models:
  - id: small
    capabilities: [tools, vision]
    deployments: [{provider: fake, upstream_model: small-up, pricing: {input: 1, output: 2}}]
  - id: mid
    capabilities: [tools]
    deployments: [{provider: fake, upstream_model: mid-up, pricing: {input: 2, output: 4}}]
  - id: notools
    deployments: [{provider: fake, upstream_model: notools-up, pricing: {input: 1, output: 1}}]
  - id: large
    capabilities: [tools, vision]
    deployments: [{provider: fake, upstream_model: large-up, pricing: {input: 10, output: 20}}]
  - id: judge
    deployments: [{provider: fake, upstream_model: judge-up, pricing: {input: 1, output: 2}}]
`, baseURL)
}

// newEnv starts a fake with script and returns options for a run of
// prompts against candidates, with large as the reference.
func newEnv(t *testing.T, script fakejudge.Script, prompts []eval.Prompt, candidates ...string) *testEnv {
	t.Helper()
	srv := fakejudge.New(script)
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "switchyard.yaml")
	if err := os.WriteFile(path, []byte(runnerConfig(srv.BaseURL())), 0o600); err != nil {
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
	adapters, err := eval.Adapters(cfg)
	if err != nil {
		t.Fatal(err)
	}
	target := func(name string) eval.Target {
		tg, err := eval.NewTarget(cat, adapters, name)
		if err != nil {
			t.Fatal(err)
		}
		return tg
	}
	jt := target("judge")
	j, err := judge.NewLLM(judge.Options{Adapter: jt.Adapter, Deployment: jt.Deployment, MaxTokens: 64})
	if err != nil {
		t.Fatal(err)
	}
	h, err := quality.NewHeuristic(nil)
	if err != nil {
		t.Fatal(err)
	}
	env := &testEnv{srv: srv, cat: cat, cfg: cfg}
	env.opt = eval.Options{
		PromptSet: "test", Prompts: prompts, Catalog: cat,
		Reference: target("large"), Judge: j, Assessor: h,
		MaxCost: money(t, "100"), MaxOutputTokens: 128, Concurrency: 2,
	}
	for _, c := range candidates {
		env.opt.Candidates = append(env.opt.Candidates, target(c))
	}
	return env
}

func money(t *testing.T, s string) usage.Money {
	t.Helper()
	m, err := usage.ParseMoney(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// prompts returns n prompts asking secret questions; every third is hard.
func prompts(n int) []eval.Prompt {
	out := make([]eval.Prompt, n)
	for i := range out {
		q := fmt.Sprintf("secret-question-%d: what is the capital of France?", i)
		if i%3 == 0 {
			q = fmt.Sprintf("secret-question-%d: prove step by step that the algorithm below is correct and analyze the trade-offs of its design in depth", i)
		}
		body, _ := json.Marshal(map[string]any{"model": "x", "messages": []any{map[string]any{"role": "user", "content": q}}})
		out[i] = eval.Prompt{ID: fmt.Sprintf("p%02d", i), Request: body}
	}
	return out
}

// standardScript answers well for large and mid, badly for small, and
// judges by preferring good answers.
func standardScript(r fakejudge.Request) fakejudge.Reply {
	if r.Judge {
		return fakejudge.Reply{Content: fakejudge.Prefer(func(a string) int {
			if strings.HasPrefix(a, "GOOD") {
				return 1
			}
			return 0
		}, r)}
	}
	if r.Model == "small-up" {
		return fakejudge.Reply{Content: badAnswer}
	}
	return fakejudge.Reply{Content: goodAnswer}
}

// run runs env and returns the summary and the parsed lines.
func run(t *testing.T, env *testEnv) (eval.Summary, []eval.Result, []byte) {
	t.Helper()
	var buf syncBuffer
	env.opt.Out = &buf
	sum, err := eval.Run(context.Background(), env.opt)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	data := buf.Bytes()
	res, _, err := eval.ReadResults(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadResults: %v", err)
	}
	return sum, res, data
}

// syncBuffer is a bytes.Buffer safe for the runner's writer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

func TestRun(t *testing.T) {
	env := newEnv(t, standardScript, prompts(6), "small", "mid")
	sum, res, data := run(t, env)
	if sum.Pairs != 12 || sum.Judged != 12 || sum.Acceptable != 6 || sum.NotRun != 0 || sum.Stopped || len(res) != 12 {
		t.Errorf("summary %+v with %d lines; want 12 pairs judged, 6 acceptable", sum, len(res))
	}
	judges, answers := env.srv.Counts()
	if judges != 24 || answers != 6+12 {
		t.Errorf("%d judge and %d answer calls; want 24 and 18 (one reference answer per prompt)", judges, answers)
	}
	total := usage.Money{}
	for _, r := range res {
		want := judge.OutcomeEquivalent
		if r.Model == "small" {
			want = judge.OutcomeReferenceBetter
		}
		if r.Outcome != want || r.Acceptable != (want == judge.OutcomeEquivalent) || r.Error != "" {
			t.Errorf("%s/%s: outcome %s acceptable %v error %q; want %s", r.ID, r.Model, r.Outcome, r.Acceptable, r.Error, want)
		}
		if r.PromptSet != "test" || r.Reference != "large@fake" || r.Judge != "judge@fake" || r.Deployment != r.Model+"@fake" {
			t.Errorf("%s/%s: provenance %+v", r.ID, r.Model, r)
		}
		if r.Category == "" || r.Difficulty < 0 || r.Difficulty > 1 || r.Time.IsZero() {
			t.Errorf("%s/%s: category %q difficulty %v time %v", r.ID, r.Model, r.Category, r.Difficulty, r.Time)
		}
		if r.CandidateCostUSD.IsZero() || r.JudgeCostUSD.IsZero() {
			t.Errorf("%s/%s: costs %v / %v", r.ID, r.Model, r.CandidateCostUSD, r.JudgeCostUSD)
		}
		total = total.Add(r.CandidateCostUSD).Add(r.ReferenceCostUSD).Add(r.JudgeCostUSD)
	}
	if !total.Equal(sum.Spent) {
		t.Errorf("lines add up to %s, summary spent %s", total, sum.Spent)
	}
	if want := fakeCost(t, env); !want.Equal(sum.Spent) {
		t.Errorf("spent %s, the fake's usage at catalog prices is %s", sum.Spent, want)
	}
	for _, secret := range []string{"secret-question", "secret-answer", "capital of France", "GOOD", "BAD", "scripted"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("results contain content %q", secret)
		}
	}
}

// fakeCost prices every request the fake answered at its model's catalog
// price, from the usage the fake reported (one token per 4 bytes).
func fakeCost(t *testing.T, env *testEnv) usage.Money {
	t.Helper()
	prices := map[string]config.Pricing{}
	for _, m := range env.cat.Models() {
		prices[m.Deployments[0].UpstreamModel] = m.Deployments[0].Prices
	}
	total := usage.Money{}
	for _, r := range env.srv.Requests() {
		content := ""
		if r.Judge {
			content = standardScriptContent(r)
		} else if r.Model == "small-up" {
			content = badAnswer
		} else {
			content = goodAnswer
		}
		c, ok := usage.Cost(usage.Tokens{Input: int64(max(1, len(r.Body)/4)), Output: int64(max(1, len(content)/4))}, prices[r.Model])
		if !ok {
			t.Fatalf("no price for %s", r.Model)
		}
		total = total.Add(c)
	}
	return total
}

func standardScriptContent(r fakejudge.Request) string { return standardScript(r).Content }

func TestRunCostCap(t *testing.T) {
	// Every reply takes a while, so that the workers' reservations overlap.
	slow := func(r fakejudge.Request) fakejudge.Reply {
		rep := standardScript(r)
		rep.Delay = 5 * time.Millisecond
		rep.PromptTokens, rep.CompletionTokens = 50, 20
		return rep
	}
	for _, cap := range []string{"0.02", "0.03", "0.04"} {
		t.Run(cap, func(t *testing.T) {
			env := newEnv(t, slow, prompts(30), "small", "mid")
			env.opt.Concurrency = 8
			env.opt.MaxCost = money(t, cap)
			sum, res, _ := run(t, env)
			if !sum.Stopped || sum.NotRun == 0 {
				t.Fatalf("summary %+v; want the cap to stop the run", sum)
			}
			if sum.Spent.Cmp(env.opt.MaxCost) > 0 {
				t.Errorf("spent %s, over the cap %s", sum.Spent, cap)
			}
			// What the fake actually served, priced independently.
			prices := map[string]config.Pricing{}
			for _, m := range env.cat.Models() {
				prices[m.Deployments[0].UpstreamModel] = m.Deployments[0].Prices
			}
			served := usage.Money{}
			for _, r := range env.srv.Requests() {
				c, _ := usage.Cost(usage.Tokens{Input: 50, Output: 20}, prices[r.Model])
				served = served.Add(c)
			}
			if !served.Equal(sum.Spent) || served.Cmp(env.opt.MaxCost) > 0 {
				t.Errorf("the fake served %s, the run counted %s, the cap is %s", served, sum.Spent, cap)
			}
			if sum.Judged+sum.AnswerFailures+sum.JudgeFailures+sum.NotRun != sum.Pairs || len(res) != sum.Judged {
				t.Errorf("summary %+v does not add up with %d lines", sum, len(res))
			}
			t.Logf("cap $%s: %d of %d pairs judged, spent $%s, peak %d calls in flight", cap, sum.Judged, sum.Pairs, sum.Spent, env.srv.PeakInFlight())
		})
	}
	t.Run("nothing fits", func(t *testing.T) {
		env := newEnv(t, standardScript, prompts(3), "small")
		env.opt.MaxCost = money(t, "0.000000001")
		sum, res, _ := run(t, env)
		if judges, answers := env.srv.Counts(); judges+answers != 0 || len(res) != 0 || !sum.Stopped || sum.NotRun != 3 {
			t.Errorf("summary %+v, %d calls, %d lines; want no call at all", sum, judges+answers, len(res))
		}
	})
}

func TestRunResume(t *testing.T) {
	env := newEnv(t, standardScript, prompts(9), "small", "mid")
	env.opt.MaxCost = money(t, "0.02")
	env.opt.Concurrency = 1
	first, res1, data := run(t, env)
	if !first.Stopped || first.Judged == 0 || first.Judged == first.Pairs {
		t.Fatalf("first run %+v; want it to stop part way", first)
	}

	env.opt.MaxCost = money(t, "100")
	env.opt.Completed = eval.Completed(res1)
	env.srv.Close()
	second, res2, data2 := run(t, newEnvLike(t, env))
	if second.Skipped != first.Judged || second.Judged != first.Pairs-first.Judged || second.Stopped {
		t.Errorf("second run %+v after a first with %d judged", second, first.Judged)
	}
	all, _, err := eval.ReadResults(bytes.NewReader(append(data, data2...)))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[eval.Key]int{}
	for _, r := range all {
		if r.Judged() {
			seen[r.Key()]++
		}
	}
	if len(seen) != first.Pairs {
		t.Errorf("%d pairs judged in all, want %d", len(seen), first.Pairs)
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("pair %v judged %d times", k, n)
		}
	}
	_ = res2
}

// newEnvLike returns a fresh fake with env's script and options, for a
// resumed run.
func newEnvLike(t *testing.T, env *testEnv) *testEnv {
	t.Helper()
	fresh := newEnv(t, standardScript, env.opt.Prompts, "small", "mid")
	fresh.opt.MaxCost, fresh.opt.Completed, fresh.opt.Concurrency = env.opt.MaxCost, env.opt.Completed, env.opt.Concurrency
	return fresh
}

func TestDryRun(t *testing.T) {
	env := newEnv(t, standardScript, prompts(6), "small", "mid")
	env.opt.Completed = map[eval.Key]bool{{PromptSet: "test", ID: "p00", Model: "small"}: true, {PromptSet: "test", ID: "p00", Model: "mid"}: true}
	plan, err := eval.PlanRun(env.opt)
	if err != nil {
		t.Fatal(err)
	}
	if judges, answers := env.srv.Counts(); judges+answers != 0 {
		t.Fatalf("the dry run made %d calls", judges+answers)
	}
	want := eval.Plan{Prompts: 5, Pairs: 12, Skipped: 2, ReferenceCalls: 5, CandidateCalls: 10, JudgeRuns: 10}
	got := plan
	got.Expected, got.Worst = usage.Money{}, usage.Money{}
	if got != want {
		t.Errorf("plan %+v, want %+v", got, want)
	}
	if plan.Expected.IsZero() || plan.Expected.Cmp(plan.Worst) >= 0 {
		t.Errorf("expected %s, worst %s", plan.Expected, plan.Worst)
	}
	sum, _, _ := run(t, env)
	if sum.Spent.Cmp(plan.Expected) > 0 {
		t.Errorf("the run spent %s, more than the plan's estimate %s", sum.Spent, plan.Expected)
	}
}

func TestRunFailures(t *testing.T) {
	script := func(r fakejudge.Request) fakejudge.Reply {
		switch {
		case r.Judge && strings.HasPrefix(r.A+r.B, "BAD"), r.Judge && strings.Contains(r.A+r.B, "BAD"):
			return fakejudge.Reply{Content: "I cannot decide."} // small's pairs: unparseable
		case !r.Judge && r.Model == "mid-up" && strings.Contains(r.User, "secret-question-1:"):
			return fakejudge.Reply{Status: 500}
		case !r.Judge && r.Model == "large-up" && strings.Contains(r.User, "secret-question-2:"):
			return fakejudge.Reply{Status: 503}
		}
		return standardScript(r)
	}
	env := newEnv(t, script, prompts(3), "small", "mid")
	sum, res, data := run(t, env)
	errs := map[string]string{}
	for _, r := range res {
		errs[r.ID+"/"+r.Model] = r.Error
	}
	want := map[string]string{
		"p00/small": "judge: judge judge@fake: first run: judge: the reply is not a verdict",
		"p00/mid":   "",
		"p01/small": "judge: judge judge@fake: first run: judge: the reply is not a verdict",
		"p01/mid":   "candidate: provider fake: server_error:",
		"p02/small": "reference: provider fake: server_error:",
		"p02/mid":   "reference: provider fake: server_error:",
	}
	for k, w := range want {
		got, ok := errs[k]
		if !ok || (w == "" && got != "") || !strings.HasPrefix(got, w) {
			t.Errorf("%s: error %q, want prefix %q", k, got, w)
		}
	}
	if sum.JudgeFailures != 2 || sum.AnswerFailures != 3 || sum.Judged != 1 || sum.NotRun != 0 {
		t.Errorf("summary %+v; want 2 judge failures, 3 answer failures, 1 verdict", sum)
	}
	if bytes.Contains(data, []byte("cannot decide")) {
		t.Error("results contain the judge's reply")
	}
	// The failed judge calls are paid for.
	for _, r := range res {
		if strings.HasPrefix(r.Error, "judge:") && r.JudgeCostUSD.IsZero() {
			t.Errorf("%s/%s: judge failure without its cost", r.ID, r.Model)
		}
	}
}

func TestRunIneligible(t *testing.T) {
	body := []byte(`{"model":"x","stream":true,"messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`)
	env := newEnv(t, standardScript, []eval.Prompt{{ID: "t", Request: body, Category: "tool_use"}}, "notools", "small")
	sum, res, _ := run(t, env)
	byModel := map[string]eval.Result{}
	for _, r := range res {
		byModel[r.Model] = r
	}
	if e := byModel["notools"].Error; !strings.HasPrefix(e, "candidate: request: ineligible for notools@fake: lacks capability tools") {
		t.Errorf("notools: error %q", e)
	}
	if r := byModel["small"]; r.Error != "" || r.Category != "tool_use" {
		t.Errorf("small: %+v", r)
	}
	for _, r := range env.srv.Requests() {
		if r.Model == "notools-up" {
			t.Error("an ineligible deployment was called")
		}
		if bytes.Contains(r.Body, []byte(`"stream"`)) || !bytes.Contains(r.Body, []byte(`"max_completion_tokens":128`)) {
			if !r.Judge {
				t.Errorf("answer request not prepared: %s", r.Body)
			}
		}
	}
	if sum.AnswerFailures != 1 || sum.Judged != 1 {
		t.Errorf("summary %+v", sum)
	}
}

func TestRunCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	script := func(r fakejudge.Request) fakejudge.Reply {
		once.Do(cancel)
		rep := standardScript(r)
		rep.Delay = time.Second
		return rep
	}
	env := newEnv(t, script, prompts(4), "small")
	var buf syncBuffer
	env.opt.Out = &buf
	sum, err := eval.Run(ctx, env.opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(buf.Bytes()) != 0 || sum.NotRun != 4 {
		t.Errorf("summary %+v, %d bytes written; a canceled call must not be written as a failure", sum, len(buf.Bytes()))
	}
}

func TestRunValidation(t *testing.T) {
	env := newEnv(t, standardScript, prompts(1), "small")
	tests := []struct {
		name   string
		mutate func(*eval.Options)
	}{
		{name: "no prompts", mutate: func(o *eval.Options) { o.Prompts = nil }},
		{name: "no candidates", mutate: func(o *eval.Options) { o.Candidates = nil }},
		{name: "zero cap", mutate: func(o *eval.Options) { o.MaxCost = usage.Money{} }},
		{name: "duplicate candidate", mutate: func(o *eval.Options) { o.Candidates = append(o.Candidates, o.Candidates[0]) }},
		{name: "no judge", mutate: func(o *eval.Options) { o.Judge = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := env.opt
			o.Out = &bytes.Buffer{}
			tt.mutate(&o)
			if _, err := eval.Run(context.Background(), o); err == nil {
				t.Error("Run: want an error")
			}
		})
	}
}
