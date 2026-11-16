package shadow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"switchyard/internal/budget"
	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/judge"
	"switchyard/internal/policy"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
	"switchyard/internal/usage"
)

// Content markers that must never reach a log line or a stored row.
const (
	secretPrompt     = "SECRET-PROMPT-7f3a"
	secretServed     = "SECRET-SERVED-ANSWER-91c2"
	secretComparison = "SECRET-COMPARISON-ANSWER-4d8e"
)

var testNow = time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)

func price(t testing.TB, s string) config.Price {
	t.Helper()
	p, err := config.ParsePrice(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// deployment returns a chat deployment model@provider priced at $1 input
// and $2 output per million tokens.
func deployment(t testing.TB, model, provider string, tags ...string) *catalog.Deployment {
	t.Helper()
	m := &catalog.Model{ID: model, Type: config.ModelTypeChat, MaxOutputTokens: 4096, TokenRatio: 1}
	d := &catalog.Deployment{
		ID: model + "@" + provider, Model: m, Provider: provider, ProviderType: "openai", UpstreamModel: "up-" + model,
		Tags:   tags,
		Prices: config.Pricing{Input: price(t, "1"), Output: price(t, "2")},
	}
	m.Deployments = []*catalog.Deployment{d}
	return d
}

// fakeAdapter answers every chat call with text and usage, after its gate
// (if set) opens or the call's context ends.
type fakeAdapter struct {
	text     string
	usage    *schema.Usage
	err      error
	gate     chan struct{}
	started  chan struct{} // receives once per call, if set
	mu       sync.Mutex
	requests []*providers.Request
	calls    atomic.Int64
}

func (a *fakeAdapter) Chat(ctx context.Context, req *providers.Request) (*providers.Response, error) {
	a.calls.Add(1)
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()
	if a.started != nil {
		a.started <- struct{}{}
	}
	if a.gate != nil {
		select {
		case <-a.gate:
		case <-ctx.Done():
			return nil, &providers.Error{Kind: providers.KindCanceled, Provider: "p", Message: "canceled"}
		}
	}
	if a.err != nil {
		return nil, a.err
	}
	body, _ := json.Marshal(map[string]any{
		"id": "c1", "object": "chat.completion",
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": a.text}}},
		"usage":   a.usage,
	})
	return &providers.Response{Status: 200, Body: body, Usage: a.usage}, nil
}

func (a *fakeAdapter) ChatStream(context.Context, *providers.Request) (providers.Stream, error) {
	return nil, errors.New("fake: shadow calls must not stream")
}

// fakeJudgeEst is the default estimate of a fake judge call: 2000 µUSD.
const fakeJudgeEst = 2_000_000

// fakeJudge returns verdict for every pair, or err. It makes calls metered
// calls (default 1), each reserved at est (default fakeJudgeEst) and
// charged an equal share of verdict.CostNano; a failed judgment's calls
// are charged their estimate.
type fakeJudge struct {
	verdict judge.Verdict
	err     error
	calls   int
	est     int64
	mu      sync.Mutex
	pairs   []judge.Pair
}

func (j *fakeJudge) Judge(ctx context.Context, p judge.Pair) (judge.Verdict, error) {
	return j.JudgeMetered(ctx, p, nil)
}

func (j *fakeJudge) JudgeMetered(ctx context.Context, p judge.Pair, m judge.Meter) (judge.Verdict, error) {
	calls, est := max(j.calls, 1), j.est
	if est == 0 {
		est = fakeJudgeEst
	}
	var spent int64
	for i := range calls {
		if m != nil {
			if err := m.Reserve(ctx, est); err != nil {
				return judge.Verdict{JudgeModel: "judge@p", CostNano: spent}, fmt.Errorf("fake judge: %w", err)
			}
		}
		if i == 0 {
			j.mu.Lock()
			j.pairs = append(j.pairs, p)
			j.mu.Unlock()
		}
		charge := est
		if j.err == nil {
			charge = j.verdict.CostNano / int64(calls)
			if j.verdict.CostNano < 0 {
				charge = -1
			}
		}
		if m != nil {
			m.Settle(charge)
		}
		if charge >= 0 && spent >= 0 {
			spent += charge
		} else {
			spent = -1
		}
	}
	if j.err != nil {
		return judge.Verdict{JudgeModel: "judge@p", CostNano: spent}, j.err
	}
	return j.verdict, nil
}

// harness is a runner over fakes.
type harness struct {
	r       *Runner
	adapter *fakeAdapter
	judge   *fakeJudge
	budgets budget.Store
	ledger  *usage.MemoryLedger
	logs    *syncBuffer
	comp    *catalog.Deployment
	judgeD  *catalog.Deployment
}

// syncBuffer is a bytes.Buffer safe for concurrent writes.
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

// accepted is the default verdict: equivalent, from judge@p, 300 µUSD.
var accepted = judge.Verdict{Outcome: judge.OutcomeEquivalent, Acceptable: true, JudgeModel: "judge@p", CostNano: 300_000}

func newHarness(t *testing.T, mod func(*RunnerOptions)) *harness {
	t.Helper()
	h := &harness{
		adapter: &fakeAdapter{text: secretComparison, usage: &schema.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}},
		judge:   &fakeJudge{verdict: accepted},
		budgets: budget.NewMemoryStore(),
		ledger:  usage.NewMemoryLedger(),
		logs:    &syncBuffer{},
		comp:    deployment(t, "big", "p"),
		judgeD:  deployment(t, "judge", "p"),
	}
	opts := RunnerOptions{
		Adapters: func(p string) providers.Adapter {
			if p == "p" {
				return h.adapter
			}
			return nil
		},
		Judge: h.judge, JudgeDeployment: h.judgeD,
		Budgets: h.budgets, DailyBudgetUSD: price(t, "1"), Store: h.ledger,
		Registerer: prometheus.NewRegistry(),
		Logger:     slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:        func() time.Time { return testNow },
	}
	if mod != nil {
		mod(&opts)
	}
	r, err := NewRunner(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.r = r
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return h
}

func chatRequest(t testing.TB, body string) *schema.ChatRequest {
	t.Helper()
	r, err := schema.DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// sample returns a valid enforce-mode sample with request ID id.
func (h *harness) sample(t testing.TB, id string) Sample {
	return Sample{
		RequestID: id, Tenant: "acme", KeyID: "k1", Time: testNow,
		Request:  chatRequest(t, `{"model":"auto","messages":[{"role":"user","content":"`+secretPrompt+`"}],"stream":true,"n":2,"max_tokens":5000,"stream_options":{"include_usage":true}}`),
		Category: "coding", Difficulty: 0.6,
		Served:     Answer{Deployment: "small@p", Text: secretServed},
		Comparison: h.comp, Mode: ModeEnforce, Profile: "balanced",
	}
}

// close closes the runner, failing the test on error.
func (h *harness) close(t *testing.T) {
	t.Helper()
	if err := h.r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// outcome returns the samples counted with outcome o.
func (h *harness) outcome(o string) float64 {
	return testutil.ToFloat64(h.r.m.samples.WithLabelValues(o))
}

// spent returns tenant's shadow spend today, in micro-USD.
func (h *harness) spent(t *testing.T, tenant string) int64 {
	t.Helper()
	n, err := h.budgets.Spent(context.Background(), h.r.Scope(tenant), testNow)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// recordingStore wraps a ShadowStore and keeps every row it is given.
type recordingStore struct {
	inner usage.ShadowStore
	mu    sync.Mutex
	rows  []usage.ShadowEval
}

func (s *recordingStore) RecordShadowEval(ctx context.Context, e *usage.ShadowEval) error {
	s.mu.Lock()
	s.rows = append(s.rows, *e)
	s.mu.Unlock()
	return s.inner.RecordShadowEval(ctx, e)
}

func (s *recordingStore) all() []usage.ShadowEval {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]usage.ShadowEval(nil), s.rows...)
}

// withRecorder makes the harness's store a recordingStore over its ledger.
func withRecorder(rec **recordingStore) func(*RunnerOptions) {
	return func(o *RunnerOptions) {
		*rec = &recordingStore{inner: o.Store}
		o.Store = *rec
	}
}

func TestRunnerPairing(t *testing.T) {
	tests := []struct {
		mode               string
		candDep, candText  string
		refDep, refText    string
		wantCand, wantRefD string
	}{
		{ModeEnforce, "small@p", secretServed, "big@p", secretComparison, "small@p", "big@p"},
		{ModeObserve, "big@p", secretComparison, "small@p", secretServed, "big@p", "small@p"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			var rec *recordingStore
			h := newHarness(t, withRecorder(&rec))
			s := h.sample(t, "req-"+tt.mode)
			s.Mode = tt.mode
			if !h.r.Offer(s) {
				t.Fatal("Offer refused a valid sample")
			}
			h.close(t)

			if len(h.judge.pairs) != 1 {
				t.Fatalf("judge called %d times, want 1", len(h.judge.pairs))
			}
			p := h.judge.pairs[0]
			if p.Candidate != tt.candText || p.Reference != tt.refText || p.Category != "coding" || p.Request != s.Request {
				t.Errorf("pair = candidate %q, reference %q, category %q", p.Candidate, p.Reference, p.Category)
			}
			rows := rec.all()
			if len(rows) != 1 {
				t.Fatalf("%d rows stored, want 1", len(rows))
			}
			got := rows[0]
			want := usage.ShadowEval{
				RequestID: s.RequestID, Time: testNow, Tenant: "acme", KeyID: "k1", Category: "coding", Difficulty: 0.6,
				Profile: "balanced", Mode: tt.mode, CandidateDeployment: tt.wantCand, ReferenceDeployment: tt.wantRefD,
				JudgeModel: "judge@p", Outcome: judge.OutcomeEquivalent, Acceptable: true,
				// 100 input and 50 output tokens at $1 and $2 per million,
				// plus the judge's 300 µUSD.
				Cost: usage.FromNanoUSD(200_000 + 300_000),
			}
			if !got.Cost.Equal(want.Cost) {
				t.Errorf("cost = %s, want %s", got.Cost, want.Cost)
			}
			got.Cost = want.Cost
			if got != want {
				t.Errorf("row = %+v\nwant  %+v", got, want)
			}
			if h.outcome(OutcomeJudged) != 1 {
				t.Errorf("judged = %v, want 1", h.outcome(OutcomeJudged))
			}
			if r := testutil.ToFloat64(h.r.m.ratio.WithLabelValues("coding")); r != 1 {
				t.Errorf("acceptable ratio = %v, want 1", r)
			}
		})
	}
}

// TestRunnerComparisonCall: the comparison call is never streamed, asks
// for one choice, caps the output at max_output_tokens, and goes to the
// comparison deployment's upstream model.
func TestRunnerComparisonCall(t *testing.T) {
	h := newHarness(t, func(o *RunnerOptions) { o.MaxOutputTokens = 300 })
	h.r.Offer(h.sample(t, "r1"))
	h.close(t)
	if len(h.adapter.requests) != 1 {
		t.Fatalf("%d comparison calls, want 1", len(h.adapter.requests))
	}
	req := h.adapter.requests[0]
	if req.UpstreamModel != "up-big" || req.Chat.Stream() {
		t.Errorf("upstream model %q, stream %v", req.UpstreamModel, req.Chat.Stream())
	}
	if n, ok := req.Chat.MaxTokens(); !ok || n != 300 {
		t.Errorf("max_tokens = %d, %v; want 300", n, ok)
	}
	for _, f := range []string{"n", "stream_options"} {
		if _, ok := req.Chat.Field(f); ok {
			t.Errorf("comparison request keeps %s", f)
		}
	}
	if len(req.Chat.Messages()) != 1 {
		t.Errorf("messages = %v", req.Chat.Messages())
	}
}

func TestComparisonRequestCaps(t *testing.T) {
	tests := []struct {
		body       string
		maxOut     int64
		wantField  string
		wantTokens int64
	}{
		{`{"model":"m","messages":[{"role":"user","content":"x"}]}`, 100, "max_tokens", 100},
		{`{"model":"m","messages":[{"role":"user","content":"x"}],"max_tokens":40}`, 100, "max_tokens", 40},
		{`{"model":"m","messages":[{"role":"user","content":"x"}],"max_completion_tokens":400}`, 100, "max_completion_tokens", 100},
	}
	for _, tt := range tests {
		r, limit, err := comparisonRequest(chatRequest(t, tt.body), tt.maxOut)
		if err != nil {
			t.Fatal(err)
		}
		raw, ok := r.Field(tt.wantField)
		if limit != tt.wantTokens || !ok || string(raw) != fmt.Sprint(tt.wantTokens) {
			t.Errorf("%s: limit %d, %s = %s; want %d", tt.body, limit, tt.wantField, raw, tt.wantTokens)
		}
	}
}

// budgetWatch wraps a budget store and records the highest spend it ever
// saw after an operation.
type budgetWatch struct {
	budget.Store
	scope   budget.Scope
	onAdmit func(est int64) // called on every admission, if set
	mu      sync.Mutex
	max     int64
}

func (b *budgetWatch) check(ctx context.Context) {
	n, err := b.Spent(ctx, b.scope, testNow)
	if err == nil {
		b.mu.Lock()
		b.max = max(b.max, n)
		b.mu.Unlock()
	}
}

func (b *budgetWatch) Admit(ctx context.Context, s []budget.Scope, est int64, now time.Time) (budget.Admission, error) {
	a, err := b.Store.Admit(ctx, s, est, now)
	if b.onAdmit != nil && err == nil {
		b.onAdmit(est)
	}
	b.check(ctx)
	return a, err
}

func (b *budgetWatch) Delta(ctx context.Context, a *budget.Admission, extra int64, now time.Time) error {
	err := b.Store.Delta(ctx, a, extra, now)
	b.check(ctx)
	return err
}

func (b *budgetWatch) Settle(ctx context.Context, a budget.Admission, actual int64) error {
	err := b.Store.Settle(ctx, a, actual)
	b.check(ctx)
	return err
}

// TestRunnerBudgetNeverExceeded is the P8 acceptance criterion: under
// concurrency, the shadow spend never exceeds the daily budget, every
// admitted call is settled at its actual cost, and the samples beyond the
// budget are dropped and counted. The judge makes four metered calls per
// judgment, each reserved on its own just before it is made (the
// incremental reservation), so the samples whose comparison fits but
// whose judge calls do not are dropped part way, with their spend so far
// charged and stored.
func TestRunnerBudgetNeverExceeded(t *testing.T) {
	const limitMicro = 50_000
	watch := &budgetWatch{Store: budget.NewMemoryStore()}
	var rec *recordingStore
	h := newHarness(t, func(o *RunnerOptions) {
		o.DailyBudgetUSD = price(t, "0.05")
		o.Budgets = watch
		o.Concurrency = 8
		o.QueueSize = 500
		withRecorder(&rec)(o)
	})
	h.judge.calls = 4
	h.budgets = watch
	watch.scope = h.r.Scope("acme")
	if watch.scope.LimitMicro != limitMicro || watch.scope.Name != "shadow:acme/day" || !watch.scope.Block {
		t.Fatalf("scope = %+v", watch.scope)
	}
	var wg sync.WaitGroup
	offered := atomic.Int64{}
	for g := range 8 {
		wg.Go(func() {
			for i := range 50 {
				if h.r.Offer(h.sample(t, fmt.Sprintf("r-%d-%d", g, i))) {
					offered.Add(1)
				}
			}
		})
	}
	wg.Wait()
	h.close(t)

	rows := rec.all()
	spent := h.spent(t, "acme")
	if watch.max > limitMicro || spent > limitMicro {
		t.Errorf("spend reached %d µUSD (now %d), over the %d µUSD budget", watch.max, spent, limitMicro)
	}
	judged, dropped := h.outcome(OutcomeJudged), h.outcome(OutcomeDroppedBudget)
	var verdicts, partial int
	var sum int64
	for _, r := range rows {
		switch r.ErrorKind {
		case "":
			verdicts++
			// The comparison's 200 µUSD and four judge calls of 75.
			if r.Cost.MicroUSDCeil() != 500 {
				t.Errorf("a judged sample cost %s, want 0.0005", r.Cost)
			}
		case ErrorJudgeBudget:
			partial++
			if r.Outcome != "" || r.Cost.MicroUSDCeil() < 200 || r.Cost.MicroUSDCeil() >= 500 {
				t.Errorf("a judge_budget row %+v: want no verdict and the spend so far", r)
			}
		default:
			t.Errorf("row error kind %q", r.ErrorKind)
		}
		sum += r.Cost.MicroUSDCeil()
	}
	if int(judged) != verdicts || judged == 0 || dropped == 0 || judged+dropped != float64(offered.Load()) || float64(partial) > dropped {
		t.Errorf("offered %d: judged %v (%d rows with a verdict), dropped for budget %v (%d rows stopped part way)",
			offered.Load(), judged, verdicts, dropped, partial)
	}
	// Settled at the actual cost.
	if spent != sum {
		t.Errorf("spent %d µUSD, rows cost %d", spent, sum)
	}
	// Reserving call by call fits about as many samples as their actual
	// cost allows (50,000 / 500), less what the calls in flight hold.
	if judged < 60 {
		t.Errorf("only %v samples judged within the budget; the reservation starves it", judged)
	}
	t.Logf("judged %v, dropped for budget %v (%d part way), spent %d of %d µUSD", judged, dropped, partial, spent, limitMicro)
	// Another tenant has a budget of its own.
	if n := h.spent(t, "globex"); n != 0 {
		t.Errorf("globex spent %d", n)
	}
}

// TestRunnerJudgeBudget: when the budget has room for the comparison but
// not for the next judge call, the judge call is not made, the
// comparison's cost is settled, and the sample is stored as judge_budget,
// without a verdict, and counted as dropped_budget.
func TestRunnerJudgeBudget(t *testing.T) {
	var rec *recordingStore
	h := newHarness(t, func(o *RunnerOptions) {
		o.MaxOutputTokens = 10
		o.DailyBudgetUSD = price(t, "0.0015")
		withRecorder(&rec)(o)
	})
	s := h.sample(t, "r1")
	creq := mustComparison(t, s.Request, 10)
	if est, _ := comparisonCost(s.Comparison, creq, 10); est.MicroUSDCeil() > 1500 || est.MicroUSDCeil()+fakeJudgeEst/1000 <= 1500 {
		t.Fatalf("the comparison estimate %s must fit the budget and the judge call not; adjust the test", est)
	}
	h.r.Offer(s)
	h.close(t)
	rows := rec.all()
	if len(rows) != 1 || rows[0].ErrorKind != ErrorJudgeBudget || rows[0].Outcome != "" || len(h.judge.pairs) != 0 {
		t.Fatalf("rows = %+v, judge calls %d; want one judge_budget row and no judge call", rows, len(h.judge.pairs))
	}
	if !rows[0].Cost.Equal(usage.FromNanoUSD(200_000)) || h.spent(t, "acme") != 200 {
		t.Errorf("cost %s, spent %d; want the comparison's 200 µUSD", rows[0].Cost, h.spent(t, "acme"))
	}
	if h.outcome(OutcomeDroppedBudget) != 1 || h.outcome(OutcomeError) != 0 {
		t.Errorf("dropped_budget %v, error %v; want 1 and 0", h.outcome(OutcomeDroppedBudget), h.outcome(OutcomeError))
	}
}

// TestRunnerMeteredLLM: with the real LLM judge, each of its calls is
// admitted at its own worst case just before it is made, and the sample
// costs what the calls reported.
func TestRunnerMeteredLLM(t *testing.T) {
	watch := &budgetWatch{Store: budget.NewMemoryStore()}
	judgeAdapter := &fakeAdapter{text: `{"winner":"tie","reason":"same"}`, usage: &schema.Usage{PromptTokens: 400, CompletionTokens: 20}}
	var rec *recordingStore
	var admits []int64
	var mu sync.Mutex
	watch.onAdmit = func(est int64) {
		mu.Lock()
		admits = append(admits, est)
		mu.Unlock()
	}
	h := newHarness(t, func(o *RunnerOptions) {
		o.Budgets = watch
		withRecorder(&rec)(o)
	})
	j, err := judge.NewLLM(judge.Options{Adapter: judgeAdapter, Deployment: h.judgeD})
	if err != nil {
		t.Fatal(err)
	}
	h.r.judge = j
	h.budgets = watch
	watch.scope = h.r.Scope("acme")
	h.r.Offer(h.sample(t, "r1"))
	h.close(t)
	rows := rec.all()
	if len(rows) != 1 || rows[0].ErrorKind != "" || rows[0].Outcome != judge.OutcomeEquivalent {
		t.Fatalf("rows = %+v", rows)
	}
	// One admission for the comparison, then one per judge call.
	if len(admits) != 3 || judgeAdapter.calls.Load() != 2 {
		t.Fatalf("admissions %v, judge calls %d; want 3 and 2", admits, judgeAdapter.calls.Load())
	}
	for _, est := range admits[1:] {
		// A judge call's worst case: its prompt at one token per byte
		// (at $1/M) and 256 tokens out at $2/M.
		if est < 512+1 || est > 50_000 {
			t.Errorf("a judge call admitted at %d µUSD", est)
		}
	}
	// 200 µUSD of comparison and 2 × (400 + 2·20) µUSD of judge calls.
	if want := int64(200 + 2*440); rows[0].Cost.MicroUSDCeil() != want || h.spent(t, "acme") != want {
		t.Errorf("cost %s, spent %d; want %d µUSD", rows[0].Cost, h.spent(t, "acme"), want)
	}
}

func mustComparison(t *testing.T, r *schema.ChatRequest, maxOut int64) *schema.ChatRequest {
	t.Helper()
	c, _, err := comparisonRequest(r, maxOut)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRunnerFailures(t *testing.T) {
	tests := []struct {
		name     string
		adapter  func(*fakeAdapter)
		judge    func(*fakeJudge)
		wantKind string
		// wantCost is "estimate" for the reserved estimate.
		wantCost string
	}{
		{"rate limited", func(a *fakeAdapter) {
			a.err = &providers.Error{Kind: providers.KindRateLimited, Provider: "p", Message: secretPrompt}
		}, nil,
			"comparison_rate_limited", "0"},
		{"timed out", func(a *fakeAdapter) {
			a.err = &providers.Error{Kind: providers.KindTimeout, Provider: "p", Message: "slow"}
		}, nil,
			"comparison_timeout", "estimate"},
		{"no usage", func(a *fakeAdapter) { a.usage = nil }, nil, "", "estimate+judge"},
		{"judge failed", nil, func(j *fakeJudge) { j.err = errors.New("judge: unparseable " + secretComparison) },
			ErrorJudge, "0.0002+judge-estimate"},
		{"judge cost unknown", nil, func(j *fakeJudge) { j.verdict.CostNano = -1 }, "", "0.0002+judge-estimate"},
		{"inconsistent", nil, func(j *fakeJudge) {
			j.verdict = judge.Verdict{Outcome: judge.OutcomeInconsistent, JudgeModel: "judge@p", CostNano: 300_000}
		}, "", "0.0005"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rec *recordingStore
			h := newHarness(t, withRecorder(&rec))
			if tt.adapter != nil {
				tt.adapter(h.adapter)
			}
			if tt.judge != nil {
				tt.judge(h.judge)
			}
			s := h.sample(t, "r1")
			creq := mustComparison(t, s.Request, config.DefaultShadowMaxOutputTokens)
			est, _ := comparisonCost(s.Comparison, creq, config.DefaultShadowMaxOutputTokens)
			jest := usage.FromNanoUSD(fakeJudgeEst)
			h.r.Offer(s)
			h.close(t)

			rows := rec.all()
			if len(rows) != 1 {
				t.Fatalf("%d rows, want 1", len(rows))
			}
			row := rows[0]
			if row.ErrorKind != tt.wantKind {
				t.Errorf("error kind %q, want %q", row.ErrorKind, tt.wantKind)
			}
			var want usage.Money
			switch tt.wantCost {
			case "0":
			case "estimate":
				want = est
			case "estimate+judge":
				want = est.Add(usage.FromNanoUSD(300_000))
			case "0.0002+judge-estimate":
				want = usage.FromNanoUSD(200_000).Add(jest)
			default:
				want, _ = usage.ParseMoney(tt.wantCost)
			}
			if !row.Cost.Equal(want) {
				t.Errorf("cost %s, want %s", row.Cost, want)
			}
			if got := h.spent(t, "acme"); got != want.MicroUSDCeil() {
				t.Errorf("spent %d µUSD, want %d", got, want.MicroUSDCeil())
			}
			wantOutcome := OutcomeJudged
			if tt.wantKind != "" {
				wantOutcome = OutcomeError
			}
			if h.outcome(wantOutcome) != 1 {
				t.Errorf("outcome %s = %v, want 1", wantOutcome, h.outcome(wantOutcome))
			}
			if tt.name == "inconsistent" && (row.Acceptable || row.Outcome != judge.OutcomeInconsistent) {
				t.Errorf("row = %+v, want an unacceptable inconsistent verdict", row)
			}
			checkNoContent(t, h, rows)
		})
	}
}

// checkNoContent fails if a log line or a stored row carries the prompt or
// an answer.
func checkNoContent(t *testing.T, h *harness, rows []usage.ShadowEval) {
	t.Helper()
	stored := fmt.Sprintf("%+v", rows)
	logs := h.logs.String()
	for _, secret := range []string{secretPrompt, secretServed, secretComparison} {
		if strings.Contains(stored, secret) {
			t.Errorf("a stored row contains %s", secret)
		}
		if strings.Contains(logs, secret) {
			t.Errorf("the logs contain %s:\n%s", secret, logs)
		}
	}
}

func TestRunnerNoContent(t *testing.T) {
	var rec *recordingStore
	h := newHarness(t, withRecorder(&rec))
	h.r.Offer(h.sample(t, "ok"))
	h.close(t)
	checkNoContent(t, h, rec.all())
	// The usage ledger's report holds counts only, too.
	r, err := h.ledger.Report(context.Background(), usage.Query{From: testNow.Add(-time.Hour), To: testNow.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if s := fmt.Sprintf("%+v", r); strings.Contains(s, "SECRET") {
		t.Errorf("report contains content: %s", s)
	}
}

// TestRunnerRowsJoinUsageEvents: a stored result joins to the request's
// usage event by request ID and shows in its tenant's quality report.
func TestRunnerRowsJoinUsageEvents(t *testing.T) {
	h := newHarness(t, nil)
	for _, id := range []string{"r1", "r2", "r3"} {
		h.ledger.Record(&usage.Event{ID: id, Time: testNow, Tenant: "acme", Category: "coding", Status: 200, CostKnown: true})
	}
	h.judge.verdict = judge.Verdict{Outcome: judge.OutcomeReferenceBetter, JudgeModel: "judge@p", CostNano: 1}
	h.r.Offer(h.sample(t, "r1"))
	h.close(t)
	r, err := h.ledger.Report(context.Background(), usage.Query{
		Tenant: "acme", From: testNow.Add(-time.Hour), To: testNow.Add(time.Hour), GroupBy: []string{usage.GroupByCategory},
	})
	if err != nil {
		t.Fatal(err)
	}
	q := r.Totals.Quality
	if len(q) != 1 || q[0].Category != "coding" || q[0].Judged != 1 || q[0].Acceptable != 0 {
		t.Errorf("quality = %+v, want one judged, unacceptable coding sample", q)
	}
	if len(r.Groups) != 1 || r.Groups[0].Totals.Quality[0].Judged != 1 || r.Totals.ShadowCostUSD.IsZero() {
		t.Errorf("groups = %+v, shadow cost %s", r.Groups, r.Totals.ShadowCostUSD)
	}
	other, err := h.ledger.Report(context.Background(), usage.Query{Tenant: "globex", From: testNow.Add(-time.Hour), To: testNow.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if other.Totals.Quality != nil {
		t.Errorf("globex sees acme's sample: %+v", other.Totals.Quality)
	}
}

func TestRunnerQueueDrop(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{}, 10)
	h := newHarness(t, func(o *RunnerOptions) { o.Concurrency = 1; o.QueueSize = 1 })
	h.adapter.gate, h.adapter.started = gate, started
	if !h.r.Offer(h.sample(t, "r1")) {
		t.Fatal("first offer refused")
	}
	<-started // r1 is running; the queue is empty
	if !h.r.Offer(h.sample(t, "r2")) {
		t.Fatal("second offer refused; the queue has room for one")
	}
	if h.r.Offer(h.sample(t, "r3")) {
		t.Fatal("third offer accepted with the queue full")
	}
	if h.outcome(OutcomeDroppedQueue) != 1 {
		t.Errorf("dropped_queue = %v, want 1", h.outcome(OutcomeDroppedQueue))
	}
	close(gate)
	h.close(t)
	if h.outcome(OutcomeJudged) != 2 {
		t.Errorf("judged = %v, want 2", h.outcome(OutcomeJudged))
	}
	if h.r.Offer(h.sample(t, "r4")) {
		t.Error("Offer accepted a sample after Close")
	}
}

// TestRunnerCloseDrains: Close waits for every queued sample.
func TestRunnerCloseDrains(t *testing.T) {
	var rec *recordingStore
	h := newHarness(t, func(o *RunnerOptions) { o.Concurrency = 2; withRecorder(&rec)(o) })
	for i := range 20 {
		if !h.r.Offer(h.sample(t, fmt.Sprint("r", i))) {
			t.Fatal("Offer refused")
		}
	}
	h.close(t)
	if n := len(rec.all()); n != 20 {
		t.Errorf("%d rows after Close, want 20", n)
	}
	if err := h.r.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestRunnerCloseCancels: when Close's context ends first, the running
// sample is canceled, settles at its estimate and is stored as canceled,
// and the queued ones are dropped.
func TestRunnerCloseCancels(t *testing.T) {
	var rec *recordingStore
	started := make(chan struct{}, 10)
	h := newHarness(t, func(o *RunnerOptions) { o.Concurrency = 1; withRecorder(&rec)(o) })
	h.adapter.gate, h.adapter.started = make(chan struct{}), started
	s := h.sample(t, "r1")
	h.r.Offer(s)
	<-started
	h.r.Offer(h.sample(t, "r2"))
	h.r.Offer(h.sample(t, "r3"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.r.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Close = %v, want context.Canceled", err)
	}
	rows := rec.all()
	if len(rows) != 1 || rows[0].ErrorKind != ErrorCanceled {
		t.Fatalf("rows = %+v, want one canceled row", rows)
	}
	est, _ := comparisonCost(s.Comparison, mustComparison(t, s.Request, config.DefaultShadowMaxOutputTokens), config.DefaultShadowMaxOutputTokens)
	if got := h.spent(t, "acme"); got != est.MicroUSDCeil() {
		t.Errorf("spent %d µUSD, want the comparison estimate %d", got, est.MicroUSDCeil())
	}
	if h.outcome(OutcomeDroppedQueue) != 2 || h.adapter.calls.Load() != 1 {
		t.Errorf("dropped_queue %v, comparison calls %d; want 2 and 1", h.outcome(OutcomeDroppedQueue), h.adapter.calls.Load())
	}
}

func TestRunnerOfferRejects(t *testing.T) {
	h := newHarness(t, nil)
	deny := &policy.Effective{Deny: []string{"big@*"}}
	denyJudge := &policy.Effective{Deny: []string{"judge@*"}}
	needTag := &policy.Effective{RequireTags: []string{"zdr"}}
	tests := []struct {
		name    string
		mod     func(*Sample)
		outcome string
	}{
		{"no request", func(s *Sample) { s.Request = nil }, OutcomeError},
		{"no request id", func(s *Sample) { s.RequestID = "" }, OutcomeError},
		{"no tenant", func(s *Sample) { s.Tenant = "" }, OutcomeError},
		{"no served deployment", func(s *Sample) { s.Served.Deployment = "" }, OutcomeError},
		{"no comparison", func(s *Sample) { s.Comparison = nil }, OutcomeError},
		{"embedding comparison", func(s *Sample) {
			d := *s.Comparison
			d.Model = &catalog.Model{ID: "e", Type: config.ModelTypeEmbedding}
			s.Comparison = &d
		}, OutcomeError},
		{"bad mode", func(s *Sample) { s.Mode = "shadow" }, OutcomeError},
		{"bad difficulty", func(s *Sample) { s.Difficulty = 1.5 }, OutcomeError},
		{"policy denies the comparison", func(s *Sample) { s.Policy = deny }, OutcomeDroppedPolicy},
		{"policy denies the judge", func(s *Sample) { s.Policy = denyJudge }, OutcomeDroppedPolicy},
		{"policy requires a tag", func(s *Sample) { s.Policy = needTag }, OutcomeDroppedPolicy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := h.outcome(tt.outcome)
			s := h.sample(t, "r")
			tt.mod(&s)
			if h.r.Offer(s) {
				t.Fatal("Offer accepted the sample")
			}
			if h.outcome(tt.outcome) != before+1 {
				t.Errorf("%s not counted", tt.outcome)
			}
		})
	}
	// A policy that permits both passes.
	s := h.sample(t, "ok")
	s.Policy = &policy.Effective{Allow: []string{"big@p", "judge@p"}}
	if !h.r.Offer(s) {
		t.Error("Offer refused a permitted sample")
	}
	var nilRunner *Runner
	if nilRunner.Offer(s) || nilRunner.Close(context.Background()) != nil {
		t.Error("a nil runner must refuse samples and close cleanly")
	}
}

func TestNewRunnerValidates(t *testing.T) {
	good := func(t *testing.T) RunnerOptions {
		return RunnerOptions{
			Adapters: func(string) providers.Adapter { return nil }, Judge: &fakeJudge{},
			JudgeDeployment: deployment(t, "j", "p"), Budgets: budget.NewMemoryStore(),
			DailyBudgetUSD: price(t, "1"), Store: usage.NewMemoryLedger(),
		}
	}
	tests := []struct {
		name string
		mod  func(*RunnerOptions)
	}{
		{"adapters", func(o *RunnerOptions) { o.Adapters = nil }},
		{"judge", func(o *RunnerOptions) { o.Judge = nil }},
		{"judge deployment", func(o *RunnerOptions) { o.JudgeDeployment = nil }},
		{"judge prices", func(o *RunnerOptions) { o.JudgeDeployment.Prices = config.Pricing{} }},
		{"budgets", func(o *RunnerOptions) { o.Budgets = nil }},
		{"store", func(o *RunnerOptions) { o.Store = nil }},
		{"no daily budget", func(o *RunnerOptions) { o.DailyBudgetUSD = config.Price{} }},
		{"zero daily budget", func(o *RunnerOptions) { o.DailyBudgetUSD = price(t, "0") }},
		{"negative limit", func(o *RunnerOptions) { o.Concurrency = -1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := good(t)
			tt.mod(&o)
			if r, err := NewRunner(o); err == nil {
				_ = r.Close(context.Background())
				t.Error("NewRunner accepted invalid options")
			}
		})
	}
	// Registering twice on one registry fails until the first closes.
	reg := prometheus.NewRegistry()
	o := good(t)
	o.Registerer = reg
	r1, err := NewRunner(o)
	if err != nil {
		t.Fatal(err)
	}
	if r2, err := NewRunner(o); err == nil {
		_ = r2.Close(context.Background())
		t.Error("a second runner registered the same metrics")
	}
	if err := r1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	r3, err := NewRunner(o)
	if err != nil {
		t.Fatalf("after Close: %v", err)
	}
	_ = r3.Close(context.Background())
}

// TestRunnerQueueBytes: the queue holds at most MaxQueueBytes of
// requests and served answers; a sample that would take it past the bound
// is dropped as dropped_queue, and the bytes of every sample a worker
// takes off the queue are released.
func TestRunnerQueueBytes(t *testing.T) {
	if MaxQueueBytes != 64<<20 {
		t.Errorf("MaxQueueBytes = %d, want the documented 64 MiB", MaxQueueBytes)
	}
	gate := make(chan struct{})
	started := make(chan struct{}, 10)
	h := newHarness(t, func(o *RunnerOptions) { o.Concurrency = 1; o.QueueSize = 50 })
	h.adapter.gate, h.adapter.started = gate, started
	s := h.sample(t, "r0")
	size := int64(s.Request.Size() + len(s.Served.Text))
	h.r.maxQueueBytes = 3 * size
	if !h.r.Offer(s) {
		t.Fatal("first offer refused")
	}
	<-started // r0 is running and off the queue: its bytes are released
	if got := h.r.queuedBytes.Load(); got != 0 {
		t.Fatalf("queued bytes %d with the queue empty, want 0", got)
	}
	for i := range 3 {
		if !h.r.Offer(h.sample(t, fmt.Sprint("r", i+1))) {
			t.Fatalf("offer %d refused within the bound", i+1)
		}
	}
	if got := h.r.queuedBytes.Load(); got != 3*size {
		t.Errorf("queued bytes %d, want %d", got, 3*size)
	}
	// The queue has room for 46 more samples, but not for their bytes.
	if h.r.Offer(h.sample(t, "r4")) || h.outcome(OutcomeDroppedQueue) != 1 {
		t.Errorf("a sample past the byte bound was queued (dropped_queue %v)", h.outcome(OutcomeDroppedQueue))
	}
	close(gate)
	h.close(t)
	if got := h.r.queuedBytes.Load(); got != 0 || h.outcome(OutcomeJudged) != 4 {
		t.Errorf("after the drain: queued bytes %d, judged %v; want 0 and 4", got, h.outcome(OutcomeJudged))
	}
}

// TestComparisonRequestStripsPersistence: the comparison call does not
// forward the client's provider-side persistence fields.
func TestComparisonRequestStripsPersistence(t *testing.T) {
	r, _, err := comparisonRequest(chatRequest(t, `{"model":"m","messages":[{"role":"user","content":"x"}],`+
		`"store":true,"metadata":{"k":"v"},"user":"u-1","service_tier":"priority","temperature":0.5}`), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"metadata", "user", "service_tier"} {
		if _, ok := r.Field(f); ok {
			t.Errorf("the comparison request keeps %s", f)
		}
	}
	if v, ok := r.Field("store"); !ok || string(v) != "false" {
		t.Errorf("store = %s, %v; want false", v, ok)
	}
	if _, ok := r.Field("temperature"); !ok {
		t.Error("other fields must be kept")
	}
}

// slowStore is a budget store and shadow store whose settlements and
// records wait for their context to end.
type slowStore struct {
	budget.Store
	settles atomic.Int64
}

func (s *slowStore) Settle(ctx context.Context, _ budget.Admission, _ int64) error {
	s.settles.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

func (s *slowStore) RecordShadowEval(ctx context.Context, _ *usage.ShadowEval) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestRunnerCloseBounded: when Close gives up, the running samples' store
// calls are bounded by Close's context too, so that a slow store cannot
// make Close outlast it (correctness L1).
func TestRunnerCloseBounded(t *testing.T) {
	store := &slowStore{Store: budget.NewMemoryStore()}
	started := make(chan struct{}, 10)
	h := newHarness(t, func(o *RunnerOptions) {
		o.Concurrency = 2
		o.Budgets = store
		o.Store = store
	})
	h.adapter.gate, h.adapter.started = make(chan struct{}), started
	h.r.Offer(h.sample(t, "r1"))
	h.r.Offer(h.sample(t, "r2"))
	<-started
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := h.r.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > finishTimeout/2 {
		t.Errorf("Close took %v with a slow store; its context allowed 100ms", d)
	}
	if store.settles.Load() != 2 {
		t.Errorf("%d settlements attempted, want 2", store.settles.Load())
	}
}
