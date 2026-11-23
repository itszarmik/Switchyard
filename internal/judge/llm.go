package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/router"
	"switchyard/internal/schema"
	"switchyard/internal/usage"
)

// Defaults of Options.
const (
	// DefaultMaxTokens bounds each judge completion: a verdict is a short
	// JSON object of about 50 to 100 tokens. It is the judge's own output
	// cap, independent of evaluation.shadow.max_output_tokens, and both
	// switchyard eval and shadow sampling use it, so that a judge call's
	// worst case, which the cost caps reserve, stays small.
	DefaultMaxTokens = 256
	// DefaultTimeout bounds each judge call.
	DefaultTimeout = 60 * time.Second
)

// MaxCalls is the most upstream calls one Judge makes: two runs, with the
// positions swapped, each retried once when its reply cannot be parsed.
// Callers that cap spending reserve MaxCalls × CallCostNano, or each call
// as it comes with JudgeMetered.
const MaxCalls = 4

// ErrUnparseable means the judge's reply was not a verdict, twice in a row.
// The caller counts it as a judge failure, not as a verdict.
var ErrUnparseable = errors.New("judge: the reply is not a verdict")

// Options configures an LLM judge.
type Options struct {
	// Adapter calls the judge deployment's provider.
	Adapter providers.Adapter
	// Deployment is the judge deployment. Its prices give Verdict.CostNano
	// and CallCostNano, unless Prices is set.
	Deployment *catalog.Deployment
	// Prices, when set, returns the judge deployment's prices in force; it
	// is read at each call and each estimate, so that new prices (a
	// reload, a dated price change) reach the judge's charges at once.
	Prices func() config.Pricing
	// MaxTokens bounds each judge completion; 0 means DefaultMaxTokens.
	MaxTokens int64
	// Timeout bounds each judge call; 0 means DefaultTimeout.
	Timeout time.Duration
}

// LLM is a pairwise LLM judge (DESIGN §10.7). Each pair is judged twice,
// with the candidate first as answer A and then as answer B, so that the
// judge's position bias cancels out; the two runs map to an Outcome as
// documented on Judge. It never logs prompt or answer content. An LLM is
// safe for concurrent use.
type LLM struct {
	adapter   providers.Adapter
	dep       *catalog.Deployment
	prices    func() config.Pricing
	maxTokens int64
	timeout   time.Duration
}

var _ MeteredJudge = (*LLM)(nil)

// NewLLM returns a judge that calls opts.Deployment through opts.Adapter.
func NewLLM(opts Options) (*LLM, error) {
	switch {
	case opts.Adapter == nil:
		return nil, errors.New("judge: missing Adapter")
	case opts.Deployment == nil || opts.Deployment.Model == nil:
		return nil, errors.New("judge: missing Deployment")
	case opts.Deployment.Model.Type != config.ModelTypeChat:
		return nil, fmt.Errorf("judge: deployment %s is not a chat model", opts.Deployment.ID)
	case opts.MaxTokens < 0:
		return nil, errors.New("judge: MaxTokens must not be negative")
	case opts.Timeout < 0:
		return nil, errors.New("judge: Timeout must not be negative")
	}
	j := &LLM{adapter: opts.Adapter, dep: opts.Deployment, prices: opts.Prices, maxTokens: opts.MaxTokens, timeout: opts.Timeout}
	if j.prices == nil {
		dep := opts.Deployment
		j.prices = func() config.Pricing { return dep.Prices }
	}
	if j.maxTokens == 0 {
		j.maxTokens = DefaultMaxTokens
	}
	if j.timeout == 0 {
		j.timeout = DefaultTimeout
	}
	return j, nil
}

// Model returns the judge deployment, as "model@provider".
func (j *LLM) Model() string { return j.dep.ID }

// preference is what one run preferred.
type preference int

const (
	prefTie preference = iota
	prefCandidate
	prefReference
)

// Judge compares p.Candidate with p.Reference. The first run shows the
// candidate as answer A, the second as answer B. The runs map to:
//
//   - both tie: equivalent;
//   - the candidate wins both, or wins one and ties the other:
//     candidate_better;
//   - the reference wins both, or wins one and ties the other:
//     reference_better;
//   - each run prefers a different answer: inconsistent, which is not
//     acceptable.
//
// A reply that cannot be parsed is retried once; a second failure returns
// ErrUnparseable. An upstream failure returns its *providers.Error. On
// error, the returned Verdict still carries JudgeModel and the CostNano
// of the calls made so far, the failed one included when it may have been
// billed, so that callers can account for them.
func (j *LLM) Judge(ctx context.Context, p Pair) (Verdict, error) {
	return j.JudgeMetered(ctx, p, nil)
}

// JudgeMetered is Judge, with m told of each upstream call: m.Reserve
// before it, with the call's estimate, and m.Settle after it, with its
// charge. When m.Reserve refuses a call, judging stops with its error,
// wrapped, and the Verdict's CostNano covers the calls already made.
func (j *LLM) JudgeMetered(ctx context.Context, p Pair, m Meter) (Verdict, error) {
	v := Verdict{JudgeModel: j.dep.ID}
	var acc costAcc
	first, err := j.run(ctx, p, false, &acc, m)
	if err != nil {
		v.CostNano = acc.nano()
		return v, fmt.Errorf("judge %s: first run: %w", j.dep.ID, err)
	}
	second, err := j.run(ctx, p, true, &acc, m)
	v.CostNano = acc.nano()
	if err != nil {
		return v, fmt.Errorf("judge %s: second run: %w", j.dep.ID, err)
	}
	v.Outcome = combine(first, second)
	v.Acceptable = v.Outcome == OutcomeCandidateBetter || v.Outcome == OutcomeEquivalent
	return v, nil
}

// combine maps the preferences of the two runs to an Outcome.
func combine(a, b preference) string {
	switch {
	case a == prefTie && b == prefTie:
		return OutcomeEquivalent
	case a != prefReference && b != prefReference:
		return OutcomeCandidateBetter
	case a != prefCandidate && b != prefCandidate:
		return OutcomeReferenceBetter
	default:
		return OutcomeInconsistent
	}
}

// run judges p once, with the candidate as answer B when swapped, and
// retries once when the reply cannot be parsed.
func (j *LLM) run(ctx context.Context, p Pair, swapped bool, acc *costAcc, m Meter) (preference, error) {
	req, err := j.request(p, swapped)
	if err != nil {
		return prefTie, err
	}
	for range 2 {
		text, err := j.call(ctx, req, acc, m)
		if err != nil {
			return prefTie, err
		}
		w, ok := parseWinner(text)
		if !ok {
			continue
		}
		switch {
		case w == winnerTie:
			return prefTie, nil
		case (w == winnerA) != swapped:
			return prefCandidate, nil
		default:
			return prefReference, nil
		}
	}
	return prefTie, ErrUnparseable
}

// request builds the judge request for p. When swapped, the reference is
// answer A and the candidate answer B.
func (j *LLM) request(p Pair, swapped bool) (*schema.ChatRequest, error) {
	a, b := p.Candidate, p.Reference
	if swapped {
		a, b = b, a
	}
	body, err := j.body(RenderRequest(p.Request), p.Category, a, b)
	if err != nil {
		return nil, err
	}
	req, err := schema.DecodeChatRequest(body)
	if err != nil {
		return nil, fmt.Errorf("judge: build the request: %w", err)
	}
	return req, nil
}

// verdictSchema is the structured-output schema of a reply.
var verdictSchema = json.RawMessage(`{"type":"json_schema","json_schema":{"name":"verdict","strict":true,"schema":{"type":"object","properties":{"winner":{"type":"string","enum":["A","B","tie"]},"reason":{"type":"string"}},"required":["winner","reason"],"additionalProperties":false}}}`)

// body encodes a judge request: the system prompt with the category's
// rubric, the user prompt, max_completion_tokens, temperature 0 when the
// deployment accepts sampling parameters, and structured output when it
// supports it.
func (j *LLM) body(request, category, a, b string) ([]byte, error) {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type body struct {
		Model          string          `json:"model"`
		Messages       []message       `json:"messages"`
		MaxTokens      int64           `json:"max_completion_tokens"`
		Temperature    *float64        `json:"temperature,omitempty"`
		ResponseFormat json.RawMessage `json:"response_format,omitempty"`
	}
	out := body{
		Model: j.dep.Model.ID,
		Messages: []message{
			{Role: "system", Content: strings.Replace(systemPrompt, "{{rubric}}", rubricFor(category), 1)},
			{Role: "user", Content: userPrompt(request, a, b)},
		},
		MaxTokens: j.maxTokens,
	}
	caps := j.dep.Capabilities
	if caps.Has(schema.CapSampling) {
		zero := 0.0
		out.Temperature = &zero
	}
	switch {
	case caps.Has(schema.CapJSONSchema):
		out.ResponseFormat = verdictSchema
	case caps.Has(schema.CapJSONMode):
		out.ResponseFormat = json.RawMessage(`{"type":"json_object"}`)
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("judge: encode the request: %w", err)
	}
	return data, nil
}

// call sends one judge request and returns the reply text, adding the
// call's charge to acc and reporting it to m, if set: its reported usage,
// or its estimate when it reached the upstream without reporting usage or
// failed in a way that may have been billed.
func (j *LLM) call(ctx context.Context, req *schema.ChatRequest, acc *costAcc, m Meter) (string, error) {
	est := j.estimate(req)
	if m != nil {
		if err := m.Reserve(ctx, est); err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, j.timeout)
	defer cancel()
	resp, err := j.adapter.Chat(ctx, &providers.Request{
		Chat:            req,
		UpstreamModel:   j.dep.UpstreamModel,
		MaxOutputTokens: j.dep.Model.MaxOutputTokens,
		Capabilities:    j.dep.Capabilities,
		NoStreamUsage:   true,
	})
	var charge int64
	switch {
	case err != nil && !providers.Kind(providers.Outcome(err)).MayHaveBilled():
		charge = 0
	case err != nil:
		charge = est
	default:
		charge = callCost(resp.Usage, j.prices(), est)
	}
	acc.add(charge)
	if m != nil {
		m.Settle(charge)
	}
	if err != nil {
		return "", err
	}
	text, err := AnswerText(resp.Body)
	if err != nil {
		// A body that is no chat completion is an unparseable reply.
		return "", nil
	}
	return text, nil
}

// callCost returns a successful call's charge in nano-USD: its usage at p,
// or est (which may be -1, unknown) when the usage or the prices are
// unknown.
func callCost(u *schema.Usage, p config.Pricing, est int64) int64 {
	if u == nil {
		return est
	}
	c, ok := usage.Cost(usage.FromUsage(u), p)
	if !ok {
		return est
	}
	return c.NanoUSDCeil()
}

// estimate is the cost bound of one call with req (see CallCostNano).
func (j *LLM) estimate(req *schema.ChatRequest) int64 {
	return estimateNano(j.prices(), router.InputTokenBound(req), j.maxTokens)
}

// CallCostNano estimates, in nano-USD rounded up, the worst case of one
// judge call on p: its input at router.InputTokenBound (one token per byte
// of the judge request) at the deployment's input or cache-write price,
// whichever is higher, plus MaxTokens of output. It returns -1 when the
// deployment's prices are unknown. One Judge makes up to MaxCalls calls.
func (j *LLM) CallCostNano(p Pair) int64 {
	req, err := j.request(p, false)
	if err != nil {
		return -1
	}
	return j.estimate(req)
}

// EstimateNano estimates, in nano-USD rounded up, the cost of a call to d
// with inputTokens of input, as router.InputTokenBound bounds them, and at
// most maxOutput output tokens: the input at the input or cache-write
// price, whichever is higher, plus maxOutput tokens at the output price.
// It returns -1 when d's prices are unknown.
func EstimateNano(d *catalog.Deployment, inputTokens, maxOutput int64) int64 {
	return estimateNano(d.Prices, inputTokens, maxOutput)
}

// estimateNano is EstimateNano at the prices p.
func estimateNano(p config.Pricing, inputTokens, maxOutput int64) int64 {
	in, out := max(inputTokens, 0), max(maxOutput, 0)
	plain, ok := usage.Cost(usage.Tokens{Input: in, Output: out}, p)
	if !ok {
		return -1
	}
	if written, ok := usage.Cost(usage.Tokens{CacheWrite: in, Output: out}, p); ok && written.Cmp(plain) > 0 {
		plain = written
	}
	return plain.NanoUSDCeil()
}

// costAcc sums the charges of a Judge's calls.
type costAcc struct {
	total   int64
	unknown bool
}

// add adds one call's charge in nano-USD; a negative charge, which is
// unknown, makes the total unknown.
func (a *costAcc) add(nano int64) {
	if nano < 0 {
		a.unknown = true
		return
	}
	if a.total > math.MaxInt64-nano {
		a.total = math.MaxInt64
		return
	}
	a.total += nano
}

// nano returns the total in nano-USD, or -1 when unknown.
func (a *costAcc) nano() int64 {
	if a.unknown {
		return -1
	}
	return a.total
}
