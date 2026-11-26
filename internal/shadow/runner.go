package shadow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/budget"
	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/judge"
	"switchyard/internal/providers"
	"switchyard/internal/router"
	"switchyard/internal/schema"
	"switchyard/internal/usage"
)

// Runner defaults, for zero RunnerOptions fields.
const (
	// DefaultQueueSize bounds the samples waiting for a worker.
	DefaultQueueSize = 100
	// DefaultTimeout bounds one sample: the comparison and the judge
	// calls.
	DefaultTimeout = 2 * time.Minute
)

// MaxQueueBytes bounds the memory of the samples waiting for a worker: the
// requests (ChatRequest.Size) and served answers they hold, in all. A
// sample that would take the queue past it is dropped as dropped_queue, so
// that large requests at a high sample rate cannot pile up before the
// budget is even asked.
const MaxQueueBytes = 64 << 20

// finishTimeout bounds settling the budget and storing the result after a
// sample's own context has ended, for example on Close.
const finishTimeout = 5 * time.Second

// RunnerOptions configures a Runner. Zero values take the defaults.
type RunnerOptions struct {
	// Adapters returns the adapter of a provider id, or nil. Required.
	Adapters func(provider string) providers.Adapter
	// Judge compares the answers, reporting each of its upstream calls so
	// that the runner reserves the shadow budget for it first. Required.
	Judge judge.MeteredJudge
	// JudgeDeployment is the deployment Judge calls, for the policy
	// check. Required, with known prices.
	JudgeDeployment *catalog.Deployment

	// Budgets holds the shadow budgets. Required.
	Budgets budget.Store
	// DailyBudgetUSD caps each tenant's shadow spend per UTC day
	// (evaluation.shadow.daily_budget_usd). Required, greater than 0.
	DailyBudgetUSD config.Price
	// Store receives the results. Required.
	Store usage.ShadowStore

	// Concurrency is the number of samples evaluated at once
	// (config.DefaultShadowConcurrency).
	Concurrency int
	// QueueSize bounds the samples waiting for a worker (DefaultQueueSize).
	QueueSize int
	// MaxOutputTokens caps the comparison answer
	// (config.DefaultShadowMaxOutputTokens).
	MaxOutputTokens int64
	// Timeout bounds each sample (DefaultTimeout).
	Timeout time.Duration

	// Registerer receives the switchyard_shadow_* metrics. Nil leaves them
	// unregistered. Close unregisters them.
	Registerer prometheus.Registerer
	// Logger receives warnings, which carry request IDs and error kinds,
	// never content. Nil discards them.
	Logger *slog.Logger
	// Now is the clock that picks the budget's day. Nil means time.Now.
	Now func() time.Time
}

// Runner evaluates offered samples in the background. Its methods are safe
// for concurrent use, and a nil *Runner is a valid runner that samples
// nothing.
type Runner struct {
	adapters   func(string) providers.Adapter
	judge      judge.MeteredJudge
	judgeDep   *catalog.Deployment
	budgets    budget.Store
	limitMicro int64
	store      usage.ShadowStore
	maxOut     int64
	timeout    time.Duration
	log        *slog.Logger
	now        func() time.Time
	m          *metrics

	// mu guards closed and sends on queue, so that Offer never sends on a
	// closed channel.
	mu     sync.RWMutex
	closed bool
	queue  chan queued
	// queuedBytes is the size of the samples in queue, bounded by
	// maxQueueBytes (MaxQueueBytes).
	queuedBytes   atomic.Int64
	maxQueueBytes int64
	// ctx is canceled when Close gives up waiting; it cancels every
	// sample in flight.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewRunner validates opts and starts the runner's workers. Close stops
// them.
func NewRunner(opts RunnerOptions) (*Runner, error) {
	switch {
	case opts.Adapters == nil:
		return nil, errors.New("shadow: RunnerOptions.Adapters is required")
	case opts.Judge == nil:
		return nil, errors.New("shadow: RunnerOptions.Judge is required")
	case opts.JudgeDeployment == nil || opts.JudgeDeployment.Model == nil:
		return nil, errors.New("shadow: RunnerOptions.JudgeDeployment is required")
	case !config.PricingKnown(config.ModelTypeChat, opts.JudgeDeployment.Prices):
		return nil, errors.New("shadow: the judge deployment's prices must be known, for the shadow budget")
	case opts.Budgets == nil:
		return nil, errors.New("shadow: RunnerOptions.Budgets is required")
	case opts.Store == nil:
		return nil, errors.New("shadow: RunnerOptions.Store is required")
	case opts.Concurrency < 0 || opts.QueueSize < 0 || opts.MaxOutputTokens < 0 || opts.Timeout < 0:
		return nil, errors.New("shadow: RunnerOptions limits must not be negative")
	}
	limit, err := usage.ParseMoney(opts.DailyBudgetUSD.String())
	if err != nil || limit.Cmp(usage.Money{}) <= 0 {
		return nil, errors.New("shadow: RunnerOptions.DailyBudgetUSD must be greater than 0")
	}
	m, err := newMetrics(opts.Registerer)
	if err != nil {
		return nil, err
	}
	r := &Runner{
		adapters: opts.Adapters, judge: opts.Judge, judgeDep: opts.JudgeDeployment,
		budgets: opts.Budgets, maxQueueBytes: MaxQueueBytes, limitMicro: limit.MicroUSDCeil(), store: opts.Store,
		maxOut: opts.MaxOutputTokens, timeout: opts.Timeout, log: opts.Logger, now: opts.Now, m: m,
	}
	if r.maxOut == 0 {
		r.maxOut = config.DefaultShadowMaxOutputTokens
	}
	if r.timeout == 0 {
		r.timeout = DefaultTimeout
	}
	if r.log == nil {
		r.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if r.now == nil {
		r.now = time.Now
	}
	workers := opts.Concurrency
	if workers == 0 {
		workers = config.DefaultShadowConcurrency
	}
	size := opts.QueueSize
	if size == 0 {
		size = DefaultQueueSize
	}
	r.queue = make(chan queued, size)
	r.ctx, r.cancel = context.WithCancel(context.Background())
	for range workers {
		r.wg.Go(r.work)
	}
	return r, nil
}

// Scope returns the budget scope of tenant's shadow spend:
// shadow:<tenant>/day, blocking at the daily budget.
func (r *Runner) Scope(tenant string) budget.Scope {
	return budget.Scope{
		Tenant: tenant, Window: budget.WindowDay, LimitMicro: r.limitMicro, Block: true,
		Name: "shadow:" + tenant + "/" + budget.WindowDay,
	}
}

// Offer queues s for evaluation and returns at once. It reports false when
// the sample is dropped: the runner is nil or closed, the queue is full or
// would hold more than MaxQueueBytes with s, s is incomplete, or its
// policy does not permit the comparison or the judge deployment. Whether
// the tenant's shadow budget has room is decided when the sample starts,
// and again before each judge call; a sample it refuses is dropped then
// and counted as dropped_budget.
//
// The caller decides which requests to offer, with Sampled and the
// policy's sample rate, after the client's response is complete.
func (r *Runner) Offer(s Sample) bool {
	if r == nil {
		return false
	}
	switch {
	case s.Request == nil || s.RequestID == "" || s.Tenant == "" || s.Served.Deployment == "" ||
		s.Comparison == nil || s.Comparison.Model == nil || s.Comparison.Model.Type != config.ModelTypeChat ||
		(s.Mode != ModeEnforce && s.Mode != ModeObserve) || !(s.Difficulty >= 0 && s.Difficulty <= 1):
		r.m.count(OutcomeError)
		return false
	case s.Policy != nil && (router.PolicyReason(s.Policy, s.Comparison) != "" || router.PolicyReason(s.Policy, r.judgeDep) != ""):
		r.m.count(OutcomeDroppedPolicy)
		return false
	}
	size := int64(s.Request.Size() + len(s.Served.Text))
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return false
	}
	if r.queuedBytes.Add(size) > r.maxQueueBytes {
		r.queuedBytes.Add(-size)
		r.m.count(OutcomeDroppedQueue)
		return false
	}
	select {
	case r.queue <- queued{Sample: s, bytes: size}:
		return true
	default:
		r.queuedBytes.Add(-size)
		r.m.count(OutcomeDroppedQueue)
		return false
	}
}

// queued is a sample in the queue, with its size for MaxQueueBytes.
type queued struct {
	Sample
	bytes int64
}

// Close stops accepting samples and waits for the queued and running ones
// to finish. When ctx ends first, it cancels them, drops the queued ones,
// waits for the running ones to settle their budget and store what they
// have, with store calls bounded by ctx too (a settlement that cannot be
// made keeps the estimate counted), and returns ctx's error. It
// unregisters the metrics. Closing a nil or closed runner does nothing.
func (r *Runner) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	close(r.queue)
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		r.cancel()
		<-done
		err = fmt.Errorf("shadow: close: %w", ctx.Err())
	}
	r.cancel()
	r.m.unregister()
	return err
}

// work evaluates queued samples until the queue is closed. Samples still
// queued once the runner has been canceled are dropped.
func (r *Runner) work() {
	for q := range r.queue {
		r.queuedBytes.Add(-q.bytes)
		if r.ctx.Err() != nil {
			r.m.count(OutcomeDroppedQueue)
			continue
		}
		r.run(&q.Sample)
	}
}

// run evaluates one sample, reserving the tenant's shadow budget one call
// at a time: it admits the comparison call's worst case, makes the call
// and settles it at its actual cost; then, before each judge call, it
// admits that call's worst case (input at router.InputTokenBound, output
// at the judge's max tokens) and settles it after. A call is made only
// once its worst case is admitted, so the spend can exceed the budget only
// by a single call's estimator error. A sample whose comparison does not
// fit is dropped without a row; one whose judge call does not fit stops
// there, with its spend so far charged, and is stored with error kind
// judge_budget and no verdict, so that its cost shows in reports; both
// count as dropped_budget.
func (r *Runner) run(s *Sample) {
	ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
	defer cancel()
	log := r.log.With("request_id", s.RequestID)

	maxOut := r.maxOut
	if m := s.Comparison.Model.MaxOutputTokens; m > 0 {
		maxOut = min(maxOut, m)
	}
	creq, compOut, err := comparisonRequest(s.Request, maxOut)
	if err != nil {
		r.m.count(OutcomeError)
		log.Warn("shadow sample skipped: the request could not be re-encoded")
		return
	}
	est, ok := comparisonCost(s.Comparison, creq, compOut)
	if !ok {
		r.m.count(OutcomeError)
		log.Warn("shadow sample skipped: the comparison deployment's prices are unknown", "deployment", s.Comparison.ID)
		return
	}
	adm, err := r.budgets.Admit(ctx, []budget.Scope{r.Scope(s.Tenant)}, est.MicroUSDCeil(), r.now())
	switch {
	case errors.Is(err, budget.ErrExhausted):
		r.m.count(OutcomeDroppedBudget)
		return
	case err != nil:
		r.m.count(OutcomeError)
		log.Warn("shadow sample skipped: the budget store failed", "error", err)
		return
	}

	row := &usage.ShadowEval{
		RequestID: s.RequestID, Time: s.Time, Tenant: s.Tenant, KeyID: s.KeyID,
		Category: s.Category, Difficulty: s.Difficulty, Profile: s.Profile, Mode: s.Mode,
		JudgeModel: r.judgeDep.ID,
	}
	compCost, text, ok := r.compare(ctx, s, creq, est, row)
	r.settle(ctx, adm, compCost, log)
	cost := compCost
	if ok {
		cost = cost.Add(r.judgeSample(ctx, s, text, row, log))
	}
	row.Cost = cost
	r.m.cost.Add(cost.Float64())

	fctx, fcancel := r.finishContext(ctx)
	defer fcancel()
	if err := r.store.RecordShadowEval(fctx, row); err != nil {
		log.Warn("shadow result not stored", "error", err)
	}
	switch row.ErrorKind {
	case "":
		r.m.count(OutcomeJudged)
		r.m.observe(s.Category, row.Acceptable)
	case ErrorJudgeBudget:
		r.m.count(OutcomeDroppedBudget)
	default:
		r.m.count(OutcomeError)
		log.Warn("shadow evaluation failed", "error_kind", row.ErrorKind)
	}
}

// finishContext returns the context of a store call that settles or
// records a sample, which may come after the sample's own context ended:
// at most finishTimeout, and canceled when Close gives up waiting
// (r.ctx), so that Close never outlasts its own context.
func (r *Runner) finishContext(ctx context.Context) (context.Context, context.CancelFunc) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	stop := context.AfterFunc(r.ctx, cancel)
	return fctx, func() {
		stop()
		cancel()
	}
}

// settle settles adm at cost; a failure keeps the admitted estimate
// counted, which over-counts and is logged.
func (r *Runner) settle(ctx context.Context, adm budget.Admission, cost usage.Money, log *slog.Logger) {
	fctx, cancel := r.finishContext(ctx)
	defer cancel()
	if err := r.budgets.Settle(fctx, adm, cost.MicroUSDCeil()); err != nil {
		log.Warn("shadow budget settlement failed; the budget keeps the estimate", "error", err)
	}
}

// compare asks the comparison deployment for its answer to creq, whose
// worst case est is admitted. It fills row's deployments, or its error
// kind, and returns what the call cost (the actual cost where the usage is
// known, a failed call's billed usage included, est where the call may
// have been billed without it) and, when ok, the answer text.
func (r *Runner) compare(ctx context.Context, s *Sample, creq *schema.ChatRequest, est usage.Money, row *usage.ShadowEval) (cost usage.Money, text string, ok bool) {
	row.CandidateDeployment, _, row.ReferenceDeployment, _ = s.roles("")
	d := s.Comparison
	adapter := r.adapters(d.Provider)
	if adapter == nil {
		row.ErrorKind = ErrorComparison + "no_adapter"
		return usage.Money{}, "", false
	}
	resp, err := adapter.Chat(ctx, &providers.Request{
		Chat: creq, UpstreamModel: d.UpstreamModel, MaxOutputTokens: d.Model.MaxOutputTokens,
		Capabilities: d.Capabilities,
	})
	if err != nil {
		kind := providers.Kind(providers.Outcome(err))
		row.ErrorKind = ErrorComparison + string(kind)
		if r.ctx.Err() != nil {
			row.ErrorKind = ErrorCanceled
		}
		switch u := providers.BilledUsage(err); {
		case u != nil:
			// An answer the upstream billed although the adapter failed it.
			if c, ok := usage.Cost(usage.FromUsage(u), d.Prices); ok {
				return c, "", false
			}
			return est, "", false
		case providers.MayHaveBilled(err):
			return est, "", false
		}
		return usage.Money{}, "", false
	}
	cost = est
	if resp.Usage != nil {
		if c, ok := usage.Cost(usage.FromUsage(resp.Usage), d.Prices); ok {
			cost = c
		}
	}
	text, ok = ResponseText(resp.Body)
	switch {
	case !ok:
		row.ErrorKind = ErrorNoAnswer
		return cost, "", false
	case len(text) > DefaultMaxAnswerBytes:
		row.ErrorKind = ErrorAnswerTooLarge
		return cost, "", false
	}
	return cost, text, true
}

// judgeSample has the judge compare the served answer with the comparison
// answer text, each judge call reserved against the tenant's shadow
// budget first (budgetMeter). It fills row's deployments and verdict, or
// its error kind, and returns what the judge calls were charged.
func (r *Runner) judgeSample(ctx context.Context, s *Sample, text string, row *usage.ShadowEval, log *slog.Logger) usage.Money {
	candDep, candText, refDep, refText := s.roles(text)
	row.CandidateDeployment, row.ReferenceDeployment = candDep, refDep
	m := &budgetMeter{r: r, scope: r.Scope(s.Tenant), log: log}
	v, err := r.judge.JudgeMetered(ctx, judge.Pair{Request: s.Request, Category: s.Category, Candidate: candText, Reference: refText}, m)
	m.close(ctx)
	switch {
	case err == nil:
		if v.JudgeModel != "" {
			row.JudgeModel = v.JudgeModel
		}
		row.Outcome, row.Acceptable = v.Outcome, v.Acceptable
	case errors.Is(err, errJudgeBudget):
		row.ErrorKind = ErrorJudgeBudget
	case r.ctx.Err() != nil:
		row.ErrorKind = ErrorCanceled
	default:
		row.ErrorKind = ErrorJudge
	}
	return m.charged
}

// errJudgeBudget means the shadow budget refused a judge call.
var errJudgeBudget = errors.New("shadow: the daily shadow budget has no room for the next judge call")

// budgetMeter reserves each judge call of one sample against its tenant's
// shadow budget (judge.Meter): an admission per call, settled at the call's
// charge right after it, so that what one call does not use is free for
// the next call or sample at once.
type budgetMeter struct {
	r     *Runner
	scope budget.Scope
	log   *slog.Logger

	// ctx, adm and est are the open admission, if any, and its estimate in
	// nano-USD.
	ctx  context.Context
	adm  budget.Admission
	est  int64
	open bool
	// charged is what the settled calls were charged in all.
	charged usage.Money
}

// Reserve implements judge.Meter.
func (m *budgetMeter) Reserve(ctx context.Context, estNano int64) error {
	if estNano < 0 {
		return errors.New("shadow: the judge call's cost cannot be estimated")
	}
	adm, err := m.r.budgets.Admit(ctx, []budget.Scope{m.scope}, microCeil(estNano), m.r.now())
	switch {
	case errors.Is(err, budget.ErrExhausted):
		return errJudgeBudget
	case err != nil:
		m.log.Warn("shadow judge call skipped: the budget store failed", "error", err)
		return fmt.Errorf("shadow: reserve a judge call: %w", err)
	}
	m.ctx, m.adm, m.est, m.open = ctx, adm, estNano, true
	return nil
}

// Settle implements judge.Meter. An unknown charge is settled at the
// estimate.
func (m *budgetMeter) Settle(costNano int64) {
	if !m.open {
		return
	}
	if costNano < 0 {
		costNano = m.est
	}
	cost := usage.FromNanoUSD(costNano)
	m.charged = m.charged.Add(cost)
	m.r.settle(m.ctx, m.adm, cost, m.log)
	m.open = false
}

// close settles an admission the judge left open, at its estimate.
func (m *budgetMeter) close(context.Context) {
	if m.open {
		m.Settle(m.est)
	}
}

// microCeil converts nano-USD to micro-USD, rounding up.
func microCeil(nano int64) int64 {
	return nano/1000 + min(nano%1000, 1)
}

// comparisonCost returns the worst case of the comparison call to d with
// creq: its input at router.InputTokenBound and its output cap compOut,
// at d's prices. It reports false when they are unknown.
func comparisonCost(d *catalog.Deployment, creq *schema.ChatRequest, compOut int64) (usage.Money, bool) {
	return usage.Cost(usage.Tokens{Input: router.InputTokenBound(creq), Output: compOut}, d.Prices)
}

// comparisonRequest returns req as the comparison call sends it: not
// streamed, a single choice, without the client's provider-side
// persistence fields (judge.StripPersistence), and its output capped at
// maxOut, or at the request's own cap if lower. It also returns that cap.
func comparisonRequest(req *schema.ChatRequest, maxOut int64) (*schema.ChatRequest, int64, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(req.EncodeFor(req.Model(), false), &obj); err != nil {
		return nil, 0, fmt.Errorf("shadow: re-encode the request: %w", err)
	}
	for _, f := range []string{"stream", "stream_options", "n"} {
		delete(obj, f)
	}
	judge.StripPersistence(obj)
	limit := maxOut
	if v, ok := req.MaxTokens(); ok && v > 0 {
		limit = min(limit, v)
	}
	capped := json.RawMessage(fmt.Sprint(limit))
	set := false
	for _, f := range []string{"max_completion_tokens", "max_tokens"} {
		if _, ok := obj[f]; ok {
			obj[f], set = capped, true
		}
	}
	if !set {
		obj["max_tokens"] = capped
	}
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, 0, fmt.Errorf("shadow: re-encode the request: %w", err)
	}
	out, err := schema.DecodeChatRequest(body)
	if err != nil {
		return nil, 0, fmt.Errorf("shadow: re-encode the request: %w", err)
	}
	return out, limit, nil
}
