package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/judge"
	"switchyard/internal/providers"
	"switchyard/internal/quality"
	"switchyard/internal/router"
	"switchyard/internal/schema"
	"switchyard/internal/usage"
)

// Defaults of Options.
const (
	DefaultMaxOutputTokens = 1024
	DefaultConcurrency     = 4
	DefaultTimeout         = 2 * time.Minute
)

// Judge is what a run needs of its judge: verdicts, the judge's name and a
// per-call cost estimate. *judge.LLM implements it.
type Judge interface {
	judge.Judge
	// Model names the judge deployment.
	Model() string
	// CallCostNano estimates one judge call on p in nano-USD, or -1 when
	// unknown. One Judge makes up to judge.MaxCalls calls.
	CallCostNano(p judge.Pair) int64
}

// Options configures a run.
type Options struct {
	// PromptSet names the prompt set in results.
	PromptSet string
	// Prompts are the prompts to run, in order.
	Prompts []Prompt
	// Catalog is the catalog the deployments belong to, for their
	// capability and context checks.
	Catalog *catalog.Catalog
	// Candidates are the deployments under evaluation; Reference answers
	// every prompt once, for comparison.
	Candidates []Target
	Reference  Target
	// Judge compares each candidate answer with the reference answer.
	Judge Judge
	// Assessor rates each prompt's difficulty, and its category when the
	// prompt sets none.
	Assessor quality.Assessor
	// MaxCost caps the run's total cost. It must be positive.
	MaxCost usage.Money
	// MaxOutputTokens caps every answer: a prompt's max_tokens or
	// max_completion_tokens is lowered to it, and set to it when absent.
	// 0 means DefaultMaxOutputTokens.
	MaxOutputTokens int64
	// Concurrency is the number of prompts in flight; 0 means
	// DefaultConcurrency.
	Concurrency int
	// Timeout bounds each answer call; 0 means DefaultTimeout.
	Timeout time.Duration
	// Completed lists the pairs that already have a verdict; they are
	// skipped. See Completed.
	Completed map[Key]bool
	// Out receives one JSON line per (prompt, candidate) pair attempted.
	Out io.Writer
	// Now returns the time recorded on each line; nil means time.Now.
	Now func() time.Time
}

// Summary is the outcome of a run.
type Summary struct {
	// Pairs is prompts × candidates; Skipped had a verdict already.
	Pairs, Skipped int
	// Judged pairs got a verdict, Acceptable of them acceptable.
	Judged, Acceptable int
	// AnswerFailures are pairs whose candidate or reference answer failed;
	// JudgeFailures pairs whose judge failed. Both are written with their
	// error and retried by a resumed run.
	AnswerFailures, JudgeFailures int
	// NotRun is the pairs neither skipped nor written: those left when the
	// cost cap stopped the run or ctx was canceled.
	NotRun int
	// Spent is the run's total cost.
	Spent usage.Money
	// Stopped reports that the run stopped because the next call did not
	// fit within MaxCost.
	Stopped bool
}

// errStopped means a call did not fit within the cost cap.
var errStopped = errors.New("eval: the cost cap is reached")

// validate checks o and fills its defaults.
func (o *Options) validate() error {
	switch {
	case len(o.Prompts) == 0:
		return errors.New("eval: no prompts")
	case len(o.Candidates) == 0:
		return errors.New("eval: no candidates")
	case o.Catalog == nil:
		return errors.New("eval: missing catalog")
	case o.Reference.Deployment == nil || o.Reference.Adapter == nil:
		return errors.New("eval: missing reference")
	case o.Judge == nil:
		return errors.New("eval: missing judge")
	case o.Assessor == nil:
		return errors.New("eval: missing assessor")
	case o.MaxCost.Cmp(usage.Money{}) <= 0:
		return errors.New("eval: the cost cap must be positive")
	case o.MaxOutputTokens < 0 || o.Concurrency < 0 || o.Timeout < 0:
		return errors.New("eval: negative limits")
	}
	for _, t := range append([]Target{o.Reference}, o.Candidates...) {
		if t.Deployment == nil || t.Adapter == nil {
			return errors.New("eval: a candidate lacks its deployment or adapter")
		}
		if judge.EstimateNano(t.Deployment, 1, 1) < 0 {
			return fmt.Errorf("eval: deployment %s: %w", t.Deployment.ID, errNoPrices)
		}
	}
	if o.Judge.CallCostNano(judge.Pair{Category: quality.CategoryGeneral}) < 0 {
		return fmt.Errorf("eval: judge %s: %w", o.Judge.Model(), errNoPrices)
	}
	seen := make(map[string]bool)
	for _, c := range o.Candidates {
		if seen[c.Deployment.Model.ID] {
			return fmt.Errorf("eval: model %s is a candidate twice", c.Deployment.Model.ID)
		}
		seen[c.Deployment.Model.ID] = true
	}
	if o.MaxOutputTokens == 0 {
		o.MaxOutputTokens = DefaultMaxOutputTokens
	}
	if o.Concurrency == 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.Timeout == 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return nil
}

// pending returns, for each prompt, the candidates still to run.
func (o *Options) pending() (todo [][]Target, skipped int) {
	todo = make([][]Target, len(o.Prompts))
	for i, p := range o.Prompts {
		for _, c := range o.Candidates {
			if o.Completed[Key{PromptSet: o.PromptSet, ID: p.ID, Model: c.Deployment.Model.ID}] {
				skipped++
				continue
			}
			todo[i] = append(todo[i], c)
		}
	}
	return todo, skipped
}

// Run runs the evaluation: for each prompt, the reference answers once and
// each pending candidate answers, then the judge compares the candidate's
// answer with the reference's, and one line goes to o.Out. Prompts run
// o.Concurrency at a time.
//
// Every upstream call is first admitted against o.MaxCost at its
// estimated cost: an answer at its input bound (router.InputTokenBound:
// one token per byte of the request, 1,600 per image) plus
// MaxOutputTokens of output, and a judge call at its input bound plus the
// judge's max tokens. The reference answer is admitted once per prompt;
// each candidate answer is admitted together with the judge.MaxCalls judge
// calls that judge it, as one unit, before the answer call, so that a
// pair is all or nothing: a cap stop never leaves an answer paid for and
// unjudged. What is spent plus the estimates of the calls in flight never
// exceeds o.MaxCost. A unit that does not fit waits for the calls in
// flight to settle; if it still does not fit, the run stops cleanly: calls
// in flight finish and are written, and no new call starts. Calls are then
// charged at their actual cost, from the reported usage; a failed call
// that was refused (providers.Kind.MayHaveBilled is false) is charged
// nothing, and one without usage, or one that may have been billed, is
// charged its estimate.
//
// The cap therefore holds whenever each call costs no more than its
// estimate, which it can exceed only by provider-side tokens the estimate
// cannot see, such as a hidden system prompt. MaxCost covers this run
// only: what earlier runs recorded in the results file is not counted.
//
// Run returns an error only for invalid options or when writing a line
// fails; upstream and judge failures are written as lines and counted.
func Run(ctx context.Context, o Options) (Summary, error) {
	if err := o.validate(); err != nil {
		return Summary{}, err
	}
	todo, skipped := o.pending()
	r := &runner{o: &o, gate: newGate(o.MaxCost), out: &sink{w: o.Out}}
	r.sum.Pairs, r.sum.Skipped = len(o.Prompts)*len(o.Candidates), skipped

	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(o.Concurrency, len(o.Prompts)) {
		wg.Go(func() {
			for i := range jobs {
				r.prompt(ctx, o.Prompts[i], todo[i])
			}
		})
	}
feed:
	for i := range o.Prompts {
		if len(todo[i]) == 0 {
			continue
		}
		if r.halted(ctx) {
			break
		}
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()
	sum := r.sum
	sum.Spent, sum.Stopped = r.gate.totals()
	sum.NotRun = sum.Pairs - sum.Skipped - r.written
	if err := r.out.error(); err != nil {
		return sum, err
	}
	return sum, nil
}

// runner is the state of one run.
type runner struct {
	o    *Options
	gate *gate
	out  *sink

	mu      sync.Mutex
	sum     Summary
	written int
}

// halted reports whether no new work may start: the cap stopped the run,
// ctx is done, or writing failed.
func (r *runner) halted(ctx context.Context) bool {
	_, stopped := r.gate.totals()
	return stopped || ctx.Err() != nil || r.out.error() != nil
}

// prompt runs one prompt for its pending candidates.
func (r *runner) prompt(ctx context.Context, p Prompt, pending []Target) {
	if r.halted(ctx) {
		return
	}
	req, err := schema.DecodeChatRequest(p.Request)
	if err != nil {
		return // ReadPrompts validated it
	}
	a := r.o.Assessor.Assess(req, quality.Hints{})
	category := p.Category
	if category == "" {
		category = a.Category
	}
	base := Result{
		ID: p.ID, PromptSet: r.o.PromptSet,
		Reference: r.o.Reference.Deployment.ID, Judge: r.o.Judge.Model(),
		Category: category, Difficulty: a.Difficulty,
	}

	ref := r.o.Reference
	refReq, err := prepare(r.o.Catalog, p.Request, ref.Deployment, r.o.MaxOutputTokens)
	if err != nil {
		for _, c := range pending {
			line := r.line(base, c)
			line.Error = "reference: " + err.Error()
			r.record(line, func(s *Summary) { s.AnswerFailures++ })
		}
		return
	}
	refEst, ok := nanoMoney(answerEstimate(ref.Deployment, refReq, r.o.MaxOutputTokens))
	if !ok {
		r.gate.stop()
		return
	}

	// The reference's cost goes on the first line written for the prompt.
	var (
		refText     string
		refCost     usage.Money
		refAnswered bool
	)
	emit := func(line Result, count func(*Summary)) {
		line.ReferenceCostUSD, refCost = refCost, usage.Money{}
		r.record(line, count)
	}
	for i, c := range pending {
		line := r.line(base, c)
		creq, err := prepare(r.o.Catalog, p.Request, c.Deployment, r.o.MaxOutputTokens)
		if err != nil {
			line.Error = "candidate: " + err.Error()
			emit(line, func(s *Summary) { s.AnswerFailures++ })
			continue
		}
		// The unit: the answer and the judge calls on answers of the whole
		// output budget at 4 bytes per token, and, for the prompt's first
		// pair, the reference's answer, so that its cost is always
		// written with a pair.
		refFor := refText
		if !refAnswered {
			refFor = placeholder(r.o.MaxOutputTokens)
		}
		ansEst, ok1 := nanoMoney(answerEstimate(c.Deployment, creq, r.o.MaxOutputTokens))
		judgeEst, ok2 := nanoMoney(judge.MaxCalls * r.o.Judge.CallCostNano(judge.Pair{
			Request: req, Category: category, Candidate: placeholder(r.o.MaxOutputTokens), Reference: refFor,
		}))
		unit := ansEst.Add(judgeEst)
		if !refAnswered {
			unit = unit.Add(refEst)
		}
		if !ok1 || !ok2 || !r.gate.admit(unit) {
			r.gate.stop()
			return
		}
		if !refAnswered {
			refText, refCost, err = r.call(ctx, ref, refReq, refEst)
			switch {
			case err != nil && ctx.Err() != nil:
				r.gate.settle(ansEst.Add(judgeEst), usage.Money{})
				return
			case err != nil:
				r.gate.settle(ansEst.Add(judgeEst), usage.Money{})
				for _, c := range pending[i:] {
					line := r.line(base, c)
					line.Error = "reference: " + err.Error()
					emit(line, func(s *Summary) { s.AnswerFailures++ })
				}
				return
			}
			refAnswered = true
		}
		text, cost, err := r.call(ctx, c, creq, ansEst)
		line.CandidateCostUSD = cost
		switch {
		case err != nil && ctx.Err() != nil:
			r.gate.settle(judgeEst, usage.Money{})
			return
		case err != nil:
			r.gate.settle(judgeEst, usage.Money{})
			line.Error = "candidate: " + err.Error()
			emit(line, func(s *Summary) { s.AnswerFailures++ })
			continue
		}

		pair := judge.Pair{Request: req, Category: category, Candidate: text, Reference: refText}
		// Answers longer than 4 bytes per token need more judge budget;
		// it is admitted now, without waiting, since the unit is held.
		if need, ok := nanoMoney(judge.MaxCalls * r.o.Judge.CallCostNano(pair)); !ok || need.Cmp(judgeEst) > 0 {
			if !ok || !r.gate.grow(need.Sub(judgeEst)) {
				r.gate.settle(judgeEst, usage.Money{})
				r.gate.stop()
				line.Error = "judge: not run: " + errStopped.Error()
				emit(line, func(s *Summary) { s.JudgeFailures++ })
				return
			}
			judgeEst = need
		}
		v, err := r.o.Judge.Judge(ctx, pair)
		actual := judgeEst
		if m, ok := nanoMoney(v.CostNano); ok && v.CostNano >= 0 {
			actual = m
		}
		r.gate.settle(judgeEst, actual)
		line.JudgeCostUSD = actual
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			line.Error = "judge: " + err.Error()
			emit(line, func(s *Summary) { s.JudgeFailures++ })
			continue
		}
		line.Outcome, line.Acceptable = v.Outcome, v.Acceptable
		emit(line, func(s *Summary) {
			s.Judged++
			if v.Acceptable {
				s.Acceptable++
			}
		})
	}
}

// placeholder is an answer of maxOut tokens at 4 bytes per token, for the
// judge estimate before the answer exists.
func placeholder(maxOut int64) string {
	return strings.Repeat("x", int(min(maxOut, 1<<20))*4)
}

// answerEstimate is the cost bound of an answer by d to req, in nano-USD:
// the input at router.InputTokenBound and maxOut tokens of output; -1 when
// d's prices are unknown.
func answerEstimate(d *catalog.Deployment, req *schema.ChatRequest, maxOut int64) int64 {
	return judge.EstimateNano(d, router.InputTokenBound(req), maxOut)
}

// line returns base for candidate c.
func (r *runner) line(base Result, c Target) Result {
	base.Model, base.Deployment = c.Deployment.Model.ID, c.Deployment.ID
	return base
}

// record writes line and updates the summary with count.
func (r *runner) record(line Result, count func(*Summary)) {
	line.Time = r.o.Now().UTC()
	if !r.out.write(line) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.written++
	count(&r.sum)
}

// call asks t for its answer to req, whose estimate est is admitted, and
// returns the answer text and the call's cost, which it charges to the
// gate in place of est. It does not end the admission.
func (r *runner) call(ctx context.Context, t Target, req *schema.ChatRequest, est usage.Money) (string, usage.Money, error) {
	d := t.Deployment
	cctx, cancel := context.WithTimeout(ctx, r.o.Timeout)
	defer cancel()
	resp, err := t.Adapter.Chat(cctx, &providers.Request{
		Chat: req, UpstreamModel: d.UpstreamModel,
		MaxOutputTokens: d.Model.MaxOutputTokens, Capabilities: d.Capabilities,
		NoStreamUsage: true,
	})
	if err != nil {
		charged := est
		switch u := providers.BilledUsage(err); {
		case u != nil:
			// An answer the upstream billed although the adapter failed it.
			if c, ok := usage.Cost(usage.FromUsage(u), d.Prices); ok {
				charged = c
			}
		case !providers.MayHaveBilled(err):
			charged = usage.Money{}
		}
		r.gate.charge(est, charged)
		return "", charged, err
	}
	cost := est
	if resp.Usage != nil {
		if c, ok := usage.Cost(usage.FromUsage(resp.Usage), d.Prices); ok {
			cost = c
		}
	}
	r.gate.charge(est, cost)
	text, err := judge.AnswerText(resp.Body)
	if err != nil {
		return "", cost, err
	}
	return text, cost, nil
}

// droppedFields are the request parameters removed for a dropped
// capability, as the gateway removes them.
var droppedFields = map[schema.Capability][]string{
	schema.CapSampling:      {"temperature", "top_p"},
	schema.CapTemperature2:  {"temperature"},
	schema.CapN:             {"n"},
	schema.CapLogprobs:      {"logprobs", "top_logprobs"},
	schema.CapSeed:          {"seed"},
	schema.CapPenalties:     {"frequency_penalty", "presence_penalty"},
	schema.CapParallelTools: {"parallel_tool_calls"},
}

// prepare makes the prompt request raw sendable to d: non-streaming, one
// choice, without the provider-side persistence fields (see
// StripPersistence), output capped at maxOut tokens, and without the parameters of
// droppable capabilities d lacks. A request that needs a capability d
// lacks, or does not fit its context window, is an error that names the
// reason.
func prepare(cat *catalog.Catalog, raw json.RawMessage, d *catalog.Deployment, maxOut int64) (*schema.ChatRequest, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errors.New("request: not a JSON object")
	}
	delete(obj, "stream")
	delete(obj, "stream_options")
	delete(obj, "n")
	judge.StripPersistence(obj)
	capped := false
	for _, f := range []string{"max_completion_tokens", "max_tokens"} {
		v, ok := obj[f]
		if !ok {
			continue
		}
		var n int64
		if json.Unmarshal(v, &n) == nil && n > 0 && n <= maxOut {
			capped = true
			continue
		}
		obj[f] = json.RawMessage(fmt.Sprint(maxOut))
		capped = true
	}
	if !capped {
		obj["max_completion_tokens"] = json.RawMessage(fmt.Sprint(maxOut))
	}
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, errors.New("request: cannot encode")
	}
	req, err := schema.DecodeChatRequest(body)
	if err != nil {
		return nil, errors.New("request: not a valid chat request")
	}
	res := catalog.Resolution{Model: d.Model, Candidates: []*catalog.Deployment{d}, Pinned: true}
	rank, err := cat.Rank(res, req.RequiredCapabilities(), router.InputTokens(int64(len(body)), d.Model.TokenRatio), maxOut, true)
	if err != nil {
		var ne *catalog.NoEligibleError
		if errors.As(err, &ne) && len(ne.Exclusions) > 0 {
			return nil, fmt.Errorf("request: ineligible for %s: %s", d.ID, ne.Exclusions[0].Reason)
		}
		return nil, fmt.Errorf("request: ineligible for %s", d.ID)
	}
	var drop []string
	for _, c := range rank.Candidates[0].Dropped {
		for _, f := range droppedFields[c] {
			if !slices.Contains(drop, f) {
				drop = append(drop, f)
			}
		}
	}
	if len(drop) > 0 {
		req, _ = req.Without(drop...)
	}
	return req, nil
}

// nanoMoney converts nano-USD to Money; ok is false for a negative amount.
func nanoMoney(nano int64) (usage.Money, bool) {
	if nano < 0 {
		return usage.Money{}, false
	}
	m, err := usage.ParseMoney(fmt.Sprintf("%d.%09d", nano/1_000_000_000, nano%1_000_000_000))
	return m, err == nil
}

// gate admits calls against the cost cap: a call is admitted only if what
// is spent, plus the estimates of the calls in flight, plus its own
// estimate, stays within the limit. A call that does not fit waits for the
// calls in flight to settle, which usually cost less than their estimate;
// if it still does not fit with none in flight, the gate stops and admits
// nothing more. A caller never waits while it holds a reservation, so
// waiting cannot deadlock.
type gate struct {
	mu                     sync.Mutex
	settled                *sync.Cond // signaled on every settle and stop
	limit, spent, reserved usage.Money
	inflight               int
	stopped                bool
}

// newGate returns a gate with the given limit.
func newGate(limit usage.Money) *gate {
	g := &gate{limit: limit}
	g.settled = sync.NewCond(&g.mu)
	return g
}

// admit reserves est, or stops the gate and reports false.
func (g *gate) admit(est usage.Money) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for {
		switch {
		case g.stopped:
			return false
		case g.spent.Add(g.reserved).Add(est).Cmp(g.limit) <= 0:
			g.reserved = g.reserved.Add(est)
			g.inflight++
			return true
		case g.inflight == 0:
			g.stopped = true
			g.settled.Broadcast()
			return false
		}
		g.settled.Wait()
	}
}

// settle releases the reservation est, charges actual, and ends the
// admission.
func (g *gate) settle(est, actual usage.Money) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reserved = g.reserved.Sub(est)
	g.spent = g.spent.Add(actual)
	g.inflight--
	g.settled.Broadcast()
}

// charge releases est of an admission's reservation and charges actual,
// for a call of a unit whose other calls are still to come; settle ends
// the admission.
func (g *gate) charge(est, actual usage.Money) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reserved = g.reserved.Sub(est)
	g.spent = g.spent.Add(actual)
	g.settled.Broadcast()
}

// grow adds extra to an admission's reservation if it fits now. It never
// waits, since its caller holds a reservation; it reports false, without
// stopping the gate, when extra does not fit.
func (g *gate) grow(extra usage.Money) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped || g.spent.Add(g.reserved).Add(extra).Cmp(g.limit) > 0 {
		return false
	}
	g.reserved = g.reserved.Add(extra)
	return true
}

// stop stops the gate.
func (g *gate) stop() {
	g.mu.Lock()
	g.stopped = true
	g.settled.Broadcast()
	g.mu.Unlock()
}

// totals returns what was spent and whether the gate stopped.
func (g *gate) totals() (usage.Money, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.spent, g.stopped
}

// sink writes result lines, one at a time.
type sink struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

// write appends one line; it reports false once a write has failed.
func (s *sink) write(r Result) bool {
	data, err := json.Marshal(r)
	if err != nil {
		return false
	}
	data = append(data, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return false
	}
	if _, err := s.w.Write(data); err != nil {
		s.err = fmt.Errorf("eval: write the results: %w", err)
		return false
	}
	return true
}

// error returns the first write error.
func (s *sink) error() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Plan is the dry run of a run: the calls it would make and their
// estimated cost.
type Plan struct {
	// Prompts have at least one pending pair; Pairs and Skipped are as in
	// Summary; Ineligible pairs need a capability their deployment lacks
	// and would fail without a call.
	Prompts, Pairs, Skipped, Ineligible int
	// ReferenceCalls and CandidateCalls are answer calls; JudgeRuns are
	// judgments, of 2 judge calls each (judge.MaxCalls with retries).
	ReferenceCalls, CandidateCalls, JudgeRuns int
	// Expected is the estimate with 2 judge calls per judgment; Worst with
	// judge.MaxCalls, which is what Run admits for each judgment. Both use
	// the estimates Run admits: every answer at its input bound and whole
	// output budget, and judge calls on answers of that length (4 bytes
	// per token) at the judge's max tokens. A cap of at least Worst never
	// stops the run, as long as no answer is longer than that.
	Expected, Worst usage.Money
}

// PlanRun estimates the run o would make, without calling anything.
func PlanRun(o Options) (Plan, error) {
	if err := o.validate(); err != nil {
		return Plan{}, err
	}
	todo, skipped := o.pending()
	plan := Plan{Pairs: len(o.Prompts) * len(o.Candidates), Skipped: skipped}
	answer := placeholder(o.MaxOutputTokens)
	add := func(dst *usage.Money, nano int64) {
		if m, ok := nanoMoney(nano); ok {
			*dst = dst.Add(m)
		}
	}
	estimate := func(t Target, raw json.RawMessage) (int64, bool) {
		req, err := prepare(o.Catalog, raw, t.Deployment, o.MaxOutputTokens)
		if err != nil {
			return 0, false
		}
		return answerEstimate(t.Deployment, req, o.MaxOutputTokens), true
	}
	for i, p := range o.Prompts {
		if len(todo[i]) == 0 {
			continue
		}
		plan.Prompts++
		req, err := schema.DecodeChatRequest(p.Request)
		if err != nil {
			continue
		}
		ref, ok := estimate(o.Reference, p.Request)
		if !ok {
			plan.Ineligible += len(todo[i])
			continue
		}
		plan.ReferenceCalls++
		add(&plan.Expected, ref)
		add(&plan.Worst, ref)
		category := p.Category
		if category == "" {
			category = o.Assessor.Assess(req, quality.Hints{}).Category
		}
		for _, c := range todo[i] {
			n, ok := estimate(c, p.Request)
			if !ok {
				plan.Ineligible++
				continue
			}
			plan.CandidateCalls++
			plan.JudgeRuns++
			call := o.Judge.CallCostNano(judge.Pair{Request: req, Category: category, Candidate: answer, Reference: answer})
			add(&plan.Expected, n+2*call)
			add(&plan.Worst, n+judge.MaxCalls*call)
		}
	}
	return plan, nil
}
