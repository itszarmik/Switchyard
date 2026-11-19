// Package health keeps the per-replica state the gateway routes by
// (DESIGN §14.4–14.6): for every deployment a circuit breaker, a 429
// cooldown, a retry budget and latency statistics, and for every provider
// the count of requests in flight against its max_concurrency.
//
// One Tracker serves a replica. Its state is keyed by deployment and
// provider ID, so a caller that builds a new routing snapshot (a config
// reload) and asks the same Tracker for its handles keeps the breaker,
// cooldown and latency state of every deployment that still exists. The
// handles are safe for concurrent use, and their per-request methods do
// not allocate, except to format the reason a deployment is skipped.
package health

import (
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"switchyard/internal/providers"
)

// State is a circuit breaker state. The values are those of the
// switchyard_circuit_state gauge.
type State int

// The breaker states.
const (
	// StateClosed lets every request through.
	StateClosed State = 0
	// StateHalfOpen lets a few probe requests through.
	StateHalfOpen State = 1
	// StateOpen lets nothing through until OpenFor has passed.
	StateOpen State = 2
)

// String returns closed, half_open or open.
func (s State) String() string {
	switch s {
	case StateHalfOpen:
		return "half_open"
	case StateOpen:
		return "open"
	default:
		return "closed"
	}
}

// BudgetWindow is the sliding window of the retry budget.
const BudgetWindow = 10 * time.Second

// minLatencySamples is how many measurements a deployment needs before
// its own statistics replace the catalog's latency hint.
const minLatencySamples = 20

// ewmaAlpha is the weight of each new latency sample.
const ewmaAlpha = 0.2

// windowBuckets is the resolution of the sliding windows: a window is
// windowBuckets buckets long, and keeps one bucket more (see window).
const windowBuckets = 10

// Config holds the breaker, cooldown and retry budget settings.
type Config struct {
	// ConsecutiveFailures opens the breaker after that many failures in a
	// row.
	ConsecutiveFailures int
	// FailureRatio and MinRequests open the breaker when at least
	// MinRequests requests within Window failed at FailureRatio or more.
	FailureRatio float64
	MinRequests  int
	Window       time.Duration
	// OpenFor is how long an open breaker rejects requests.
	OpenFor time.Duration
	// HalfOpenProbes is how many concurrent probes a half-open breaker
	// admits.
	HalfOpenProbes int
	// CooldownDefault is the cooldown after a 429 without Retry-After;
	// CooldownMax caps every cooldown.
	CooldownDefault, CooldownMax time.Duration
	// BudgetRatio and BudgetMin bound the retries to a deployment within
	// BudgetWindow: at most max(BudgetMin, BudgetRatio × requests).
	BudgetRatio float64
	BudgetMin   int
}

// Observer is told about the events that the gateway's metrics count. Its
// methods are called with internal locks held and must not call back into
// the Tracker.
type Observer interface {
	// Transition is called when a deployment's breaker changes state.
	Transition(deployment string, to State)
	// Cooldown is called when a deployment starts a rate-limit cooldown.
	Cooldown(deployment string)
	// Saturated is called when a request is turned away from a provider
	// at max_concurrency.
	Saturated(provider string)
	// BudgetExhausted is called when a retry is refused because the
	// deployment's retry budget is spent.
	BudgetExhausted(deployment string)
}

// Options configure a Tracker.
type Options struct {
	Config Config
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Observer, if set, receives the countable events.
	Observer Observer
}

// Tracker holds the health state of every deployment and provider.
type Tracker struct {
	now func() time.Time
	obs Observer

	cfg Config // immutable

	mu          sync.Mutex
	deployments map[string]*Deployment
	providers   map[string]*Provider
}

// New returns an empty Tracker.
func New(opts Options) *Tracker {
	t := &Tracker{
		now:         opts.Now,
		obs:         opts.Observer,
		deployments: make(map[string]*Deployment),
		providers:   make(map[string]*Provider),
	}
	t.cfg = opts.Config
	if t.now == nil {
		t.now = time.Now
	}
	if t.obs == nil {
		t.obs = nopObserver{}
	}
	return t
}

type nopObserver struct{}

func (nopObserver) Transition(string, State) {}
func (nopObserver) Cooldown(string)          {}
func (nopObserver) Saturated(string)         {}
func (nopObserver) BudgetExhausted(string)   {}

// LatencyHint seeds a deployment's latency estimates until it has enough
// measurements. Zero values mean unknown.
type LatencyHint struct {
	TTFT         time.Duration
	TokensPerSec float64
}

// Deployment returns the handle of deployment id, served by provider,
// creating its state on first use. A later call with the same id returns
// the same handle, with the hint updated.
func (t *Tracker) Deployment(id, provider string, hint LatencyHint) *Deployment {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.deployments[id]
	if d == nil {
		d = t.newDeployment(id, provider)
		t.deployments[id] = d
	}
	d.set(provider, hint)
	return d
}

// newDeployment returns a new, unregistered deployment handle.
func (t *Tracker) newDeployment(id, provider string) *Deployment {
	d := &Deployment{
		id: id, provider: provider, t: t,
		window: newWindow(t.config().Window),
		budget: newWindow(BudgetWindow),
	}
	d.healthy.Store(true)
	return d
}

// set updates the deployment's provider and latency hint.
func (d *Deployment) set(provider string, hint LatencyHint) {
	d.mu.Lock()
	d.provider = provider
	d.hint = hint
	d.mu.Unlock()
}

// Provider returns the handle of provider id, creating it on first use,
// and sets its concurrency limit; 0 means unlimited.
func (t *Tracker) Provider(id string, maxConcurrency int) *Provider {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.providers[id]
	if p == nil {
		p = &Provider{id: id, t: t}
		t.providers[id] = p
	}
	p.max.Store(int64(maxConcurrency))
	return p
}

// Registry hands out deployment and provider handles. *Tracker registers
// them, and applies their settings, at once; *Batch defers both to Apply.
type Registry interface {
	Deployment(id, provider string, hint LatencyHint) *Deployment
	Provider(id string, maxConcurrency int) *Provider
}

// Batch collects handle registrations and setting changes, for a
// configuration that may never be put in force (a reload is staged, then
// checked, then committed), and applies them with Apply. Until then the
// Tracker and every handle it holds are unchanged: the handle of an ID the
// Tracker does not know is created unregistered, and the settings given
// for a known one are recorded, not set. A Batch is used by one goroutine.
// Batches whose lifetimes overlap must not register the same new IDs.
type Batch struct {
	t     *Tracker
	deps  map[string]depSettings
	provs map[string]provSettings
}

type depSettings struct {
	d        *Deployment
	provider string
	hint     LatencyHint
}

type provSettings struct {
	p   *Provider
	max int
}

// Batch returns an empty Batch over t.
func (t *Tracker) Batch() *Batch {
	return &Batch{t: t, deps: make(map[string]depSettings), provs: make(map[string]provSettings)}
}

// Deployment returns the handle of deployment id: the Tracker's, or a new
// one that Apply registers. Apply sets its provider and hint.
func (b *Batch) Deployment(id, provider string, hint LatencyHint) *Deployment {
	s, ok := b.deps[id]
	if !ok {
		b.t.mu.Lock()
		s.d = b.t.deployments[id]
		b.t.mu.Unlock()
		if s.d == nil {
			s.d = b.t.newDeployment(id, provider)
		}
	}
	s.provider, s.hint = provider, hint
	b.deps[id] = s
	return s.d
}

// Provider returns the handle of provider id: the Tracker's, or a new one
// that Apply registers. Apply sets its concurrency limit.
func (b *Batch) Provider(id string, maxConcurrency int) *Provider {
	s, ok := b.provs[id]
	if !ok {
		b.t.mu.Lock()
		s.p = b.t.providers[id]
		b.t.mu.Unlock()
		if s.p == nil {
			s.p = &Provider{id: id, t: b.t}
		}
	}
	s.max = maxConcurrency
	b.provs[id] = s
	return s.p
}

// Apply registers the batch's new handles with the Tracker and applies
// every setting the batch recorded, the last one given for each ID.
func (b *Batch) Apply() {
	t := b.t
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, s := range b.provs {
		if t.providers[id] == nil {
			t.providers[id] = s.p
		}
		s.p.max.Store(int64(s.max))
	}
	for id, s := range b.deps {
		if t.deployments[id] == nil {
			t.deployments[id] = s.d
		}
		s.d.set(s.provider, s.hint)
	}
}

// Provider is the concurrency state of one provider on this replica.
type Provider struct {
	id       string
	t        *Tracker
	max      atomic.Int64
	inFlight atomic.Int64
}

// Max returns the concurrency limit, 0 for unlimited.
func (p *Provider) Max() int { return int(p.max.Load()) }

// InFlight returns the number of requests in flight.
func (p *Provider) InFlight() int64 { return p.inFlight.Load() }

// IsSaturated reports whether the provider is at its concurrency limit. It
// has no side effects, so that routing decisions and explanations can
// read it freely; only Acquire counts a request turned away.
func (p *Provider) IsSaturated() bool {
	m := p.max.Load()
	return m > 0 && p.inFlight.Load() >= m
}

// Acquire takes a concurrency slot. It never waits: it returns false at
// once, counting the request turned away, when the provider is saturated.
// Every successful Acquire must be matched by a Release.
func (p *Provider) Acquire() bool {
	for {
		n := p.inFlight.Load()
		if m := p.max.Load(); m > 0 && n >= m {
			p.t.obs.Saturated(p.id)
			return false
		}
		if p.inFlight.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// Release returns a slot taken by Acquire.
func (p *Provider) Release() { p.inFlight.Add(-1) }

// Deployment is the health state of one deployment on this replica.
type Deployment struct {
	id string
	t  *Tracker

	// healthy is set while the breaker is closed; cooldownNanos mirrors
	// cooldown. They let Skip answer the common case without the lock.
	healthy       atomic.Bool
	cooldownNanos atomic.Int64

	mu          sync.Mutex
	provider    string
	hint        LatencyHint
	state       State
	consecutive int
	openUntil   time.Time
	probes      int
	// episode counts the half-open episodes, so that a probe admitted in
	// an earlier one cannot move the breaker or free a probe slot of the
	// current one.
	episode   uint64
	window    window // breaker: requests and failures
	cooldown  time.Time
	budget    window // retry budget: requests and retries
	ttft, tps ewma
}

// advance moves an open breaker whose time is up to half-open. The caller
// holds d.mu.
func (d *Deployment) advance(now time.Time) {
	if d.state == StateOpen && !now.Before(d.openUntil) {
		d.setState(StateHalfOpen)
	}
}

// setState changes the breaker state. The caller holds d.mu.
func (d *Deployment) setState(s State) {
	if d.state == s {
		return
	}
	d.state = s
	d.probes = 0
	if s == StateHalfOpen {
		d.episode++
	}
	d.healthy.Store(s == StateClosed)
	d.t.obs.Transition(d.id, s)
}

// Skip reports why a new request should not be planned on the deployment
// ("circuit open", "rate-limit cooldown 4s", ...), or "" if it may be.
// It reserves nothing; Admit does.
func (d *Deployment) Skip() string {
	now := d.t.now()
	if d.healthy.Load() && now.UnixNano() >= d.cooldownNanos.Load() {
		return ""
	}
	cfg := d.t.config()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.advance(now)
	return d.skipLocked(now, cfg, false)
}

func (d *Deployment) skipLocked(now time.Time, cfg Config, retry bool) string {
	switch {
	case d.state == StateOpen:
		return "circuit open"
	case d.state == StateHalfOpen && d.probes >= cfg.HalfOpenProbes:
		return "circuit half-open, probes in flight"
	case !retry && now.Before(d.cooldown):
		return "rate-limit cooldown " + roundUp(d.cooldown.Sub(now))
	}
	return ""
}

// roundUp formats d rounded up to a whole second, or to a millisecond
// below one second.
func roundUp(d time.Duration) string {
	if d < time.Second {
		return (d + time.Millisecond - 1).Truncate(time.Millisecond).String()
	}
	return (d + time.Second - 1).Truncate(time.Second).String()
}

// Admission is an attempt that Admit let through, to be ended with Done.
// The zero value is an ordinary attempt, not a probe.
type Admission struct {
	probe   bool
	episode uint64 // the half-open episode of a probe
}

// Probe reports whether the attempt is a half-open probe.
func (a Admission) Probe() bool { return a.probe }

// Admit reserves an attempt on the deployment: it returns the admission,
// or why the attempt may not be made. A retry of the same request ignores
// the rate-limit cooldown, whose Retry-After it has waited out. A
// request's first attempt (not a retry) counts the request for the retry
// budget. Every admitted attempt must be ended with Done.
func (d *Deployment) Admit(retry bool) (Admission, string) {
	now := d.t.now()
	cfg := d.t.config()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.advance(now)
	if r := d.skipLocked(now, cfg, retry); r != "" {
		return Admission{}, r
	}
	if !retry {
		d.budget.add(now, 1, 0)
	}
	if d.state == StateHalfOpen {
		d.probes++
		return Admission{probe: true, episode: d.episode}, ""
	}
	return Admission{}, ""
}

// Result is the outcome of an attempt.
type Result struct {
	// Kind is "" for success, else the failure's kind.
	Kind providers.Kind
	// RetryAfter is the upstream's Retry-After of a 429, if any.
	RetryAfter time.Duration
	// TTFT and TokensPerSec measure a successful attempt; zero values are
	// unknown and not recorded.
	TTFT         time.Duration
	TokensPerSec float64
}

// Done records the result of an admitted attempt. It is called once per
// admission.
//
// Only the kinds that count toward the breaker are failures for it, and
// only probes of the current half-open episode move a half-open breaker:
// a success closes it, a failure opens it again. A probe admitted before
// the breaker last re-opened is an ordinary attempt. A rate-limited
// failure starts a cooldown of min(Retry-After, or CooldownDefault without
// one, CooldownMax). A success adds its latency measurements.
func (d *Deployment) Done(a Admission, r Result) {
	now := d.t.now()
	cfg := d.t.config()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.advance(now)
	halfOpenProbe := a.probe && d.state == StateHalfOpen && a.episode == d.episode
	kind := r.Kind
	switch {
	case kind == "":
		d.recordLatency(r.TTFT, r.TokensPerSec)
		d.window.add(now, 1, 0)
		d.consecutive = 0
		if halfOpenProbe {
			d.window.reset()
			d.setState(StateClosed)
			return
		}
	case kind.CountsTowardBreaker():
		d.window.add(now, 1, 1)
		d.consecutive++
		if halfOpenProbe || (d.state == StateClosed && d.tripped(now, cfg)) {
			d.open(now, cfg)
			return
		}
	case kind == providers.KindRateLimited:
		cool := r.RetryAfter
		if cool <= 0 {
			cool = cfg.CooldownDefault
		}
		cool = min(cool, cfg.CooldownMax)
		if until := now.Add(cool); until.After(d.cooldown) {
			d.cooldown = until
			d.cooldownNanos.Store(until.UnixNano())
		}
		d.t.obs.Cooldown(d.id)
	}
	if halfOpenProbe && d.probes > 0 {
		d.probes--
	}
}

// tripped reports whether the closed breaker's counts call for opening it.
// The caller holds d.mu.
func (d *Deployment) tripped(now time.Time, cfg Config) bool {
	if d.consecutive >= cfg.ConsecutiveFailures {
		return true
	}
	reqs, fails := d.window.sum(now)
	return reqs >= float64(cfg.MinRequests) && fails >= cfg.FailureRatio*reqs
}

// open opens the breaker for OpenFor. The caller holds d.mu.
func (d *Deployment) open(now time.Time, cfg Config) {
	d.openUntil = now.Add(cfg.OpenFor)
	d.consecutive = 0
	d.window.reset()
	d.setState(StateOpen)
}

// State returns the breaker state.
func (d *Deployment) State() State {
	now := d.t.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.advance(now)
	return d.state
}

// startRequest counts a request for the retry budget, as Admit does for
// a first attempt.
func (d *Deployment) startRequest() {
	now := d.t.now()
	d.mu.Lock()
	d.budget.add(now, 1, 0)
	d.mu.Unlock()
}

// CanRetry reports whether the deployment's retry budget has room for one
// more retry, without taking it; TryRetry takes it. It returns false,
// counting the refusal, when the budget is spent. A caller checks it
// before waiting for a retry, so that it does not wait for one it may not
// make, and takes the retry only once the wait is over.
func (d *Deployment) CanRetry() bool {
	return d.retry(false)
}

// TryRetry takes one retry from the deployment's budget. It returns false,
// counting the refusal, when the budget is spent: the retries in the last
// BudgetWindow already reach max(BudgetMin, BudgetRatio × requests).
func (d *Deployment) TryRetry() bool {
	return d.retry(true)
}

func (d *Deployment) retry(take bool) bool {
	now := d.t.now()
	cfg := d.t.config()
	d.mu.Lock()
	defer d.mu.Unlock()
	reqs, retries := d.budget.sum(now)
	if retries+1 > budgetAllowed(cfg, reqs) {
		d.t.obs.BudgetExhausted(d.id)
		return false
	}
	if take {
		d.budget.add(now, 0, 1)
	}
	return true
}

// budgetAllowed is the retries a window with reqs requests allows.
func budgetAllowed(cfg Config, reqs float64) float64 {
	return math.Max(float64(cfg.BudgetMin), cfg.BudgetRatio*reqs)
}

// Commit ends admission a at the attempt's commit point, once the upstream
// has answered (a stream's first event, or a whole non-streaming
// response), instead of Done. A half-open probe of the current episode
// closes the breaker and frees its slot at once, as a successful Done
// would; any other attempt only adds its time to first token. The
// attempt's outcome is recorded later with Finish, once it is known.
func (d *Deployment) Commit(a Admission, ttft time.Duration) {
	now := d.t.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.advance(now)
	d.recordLatency(ttft, 0)
	if a.probe && d.state == StateHalfOpen && a.episode == d.episode {
		d.consecutive = 0
		d.window.reset()
		d.setState(StateClosed)
	}
}

// Finish records the outcome of an attempt that Commit ended. A success
// counts a request in the breaker window, resets the run of consecutive
// failures and adds its speed (r.TokensPerSec; its TTFT was recorded by
// Commit). A failure of a kind that counts toward the breaker (an upstream
// that stalled, dropped the connection or sent an error mid-stream) is an
// ordinary failure for it: a closed breaker that crosses its thresholds
// opens. Any other kind (the client going away, the request's own total
// timeout, which the caller reports as neutral) records nothing.
func (d *Deployment) Finish(r Result) {
	now := d.t.now()
	cfg := d.t.config()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.advance(now)
	switch {
	case r.Kind == "":
		d.recordLatency(0, r.TokensPerSec)
		d.window.add(now, 1, 0)
		d.consecutive = 0
	case r.Kind.CountsTowardBreaker():
		d.window.add(now, 1, 1)
		d.consecutive++
		if d.state == StateClosed && d.tripped(now, cfg) {
			d.open(now, cfg)
		}
	}
}

// recordLatency adds the measurements of a successful attempt. The caller
// holds d.mu.
func (d *Deployment) recordLatency(ttft time.Duration, tokensPerSec float64) {
	if ttft > 0 {
		d.ttft.add(float64(ttft))
	}
	if tokensPerSec > 0 && !math.IsInf(tokensPerSec, 0) {
		d.tps.add(tokensPerSec)
	}
}

// EstimatedTTFT returns the time-to-first-token estimate: the catalog hint
// until the deployment has 20 measurements, then their EWMA. It is 0 when
// unknown.
func (d *Deployment) EstimatedTTFT() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return time.Duration(d.ttft.estimate(float64(d.hint.TTFT)))
}

// EstimatedTokensPerSec returns the output-speed estimate, as
// EstimatedTTFT does.
func (d *Deployment) EstimatedTokensPerSec() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tps.estimate(d.hint.TokensPerSec)
}

// config returns the current settings.
func (t *Tracker) config() Config { return t.cfg }

// ewma is an exponentially weighted moving average with a sample count.
type ewma struct {
	value float64
	n     int64
}

func (e *ewma) add(v float64) {
	if e.n == 0 {
		e.value = v
	} else {
		e.value += ewmaAlpha * (v - e.value)
	}
	e.n++
}

// estimate returns the average once there are enough samples, else hint
// when it is known, else the average of what there is.
func (e *ewma) estimate(hint float64) float64 {
	if e.n >= minLatencySamples || hint <= 0 {
		return e.value
	}
	return hint
}

// windowSlots is how many buckets a window keeps: windowBuckets, and the
// one that is sliding out.
const windowSlots = windowBuckets + 1

// window counts two quantities over a sliding window of windowBuckets
// buckets. The bucket that is sliding out of the window is kept too, and
// counted pro rata for the part of it still within the window, so the
// window covers its whole length at every moment. Without it, a window
// of ten 1s buckets would cover only 9 to 10 seconds.
type window struct {
	bucket time.Duration
	counts [windowSlots][2]int64
	epochs [windowSlots]int64 // the bucket number each slot holds
}

func newWindow(d time.Duration) window {
	b := d / windowBuckets
	if b <= 0 {
		b = time.Millisecond
	}
	return window{bucket: b}
}

func (w *window) epoch(now time.Time) int64 { return now.UnixNano() / int64(w.bucket) }

func (w *window) add(now time.Time, a, b int64) {
	e := w.epoch(now)
	i := e % windowSlots
	if i < 0 {
		i += windowSlots
	}
	if w.epochs[i] != e {
		w.epochs[i] = e
		w.counts[i] = [2]int64{}
	}
	w.counts[i][0] += a
	w.counts[i][1] += b
}

// sum returns the totals within the window that ends now: the
// windowBuckets newest buckets (the current one included) in full, and
// the bucket sliding out weighted by the share of it still within the
// window, which is what the current bucket has yet to cover.
func (w *window) sum(now time.Time) (a, b float64) {
	n := now.UnixNano()
	e := n / int64(w.bucket)
	// The share of the oldest bucket still within the window.
	rest := 1 - float64(n-e*int64(w.bucket))/float64(w.bucket)
	for i := range w.counts {
		switch age := e - w.epochs[i]; {
		case age >= 0 && age < windowBuckets:
			a += float64(w.counts[i][0])
			b += float64(w.counts[i][1])
		case age == windowBuckets:
			a += rest * float64(w.counts[i][0])
			b += rest * float64(w.counts[i][1])
		}
	}
	return a, b
}

func (w *window) reset() {
	w.counts = [windowSlots][2]int64{}
	w.epochs = [windowSlots]int64{}
}

// Snapshot is the state of every provider and deployment, for
// /admin/v1/providers. It holds no secrets.
type Snapshot struct {
	Providers []ProviderSnapshot `json:"providers"`
}

// ProviderSnapshot is one provider and its deployments.
type ProviderSnapshot struct {
	ID             string               `json:"id"`
	InFlight       int64                `json:"in_flight"`
	MaxConcurrency int                  `json:"max_concurrency"`
	Deployments    []DeploymentSnapshot `json:"deployments"`
}

// DeploymentSnapshot is one deployment's state.
type DeploymentSnapshot struct {
	ID      string          `json:"id"`
	Breaker BreakerSnapshot `json:"breaker"`
	// CooldownUntil is when the rate-limit cooldown ends, or nil.
	CooldownUntil *time.Time `json:"cooldown_until"`
	// Recent counts the requests and breaker failures within the breaker
	// window.
	Recent      RecentSnapshot  `json:"recent"`
	RetryBudget BudgetSnapshot  `json:"retry_budget"`
	Latency     LatencySnapshot `json:"latency"`
}

// BreakerSnapshot is a breaker's state.
type BreakerSnapshot struct {
	State string `json:"state"`
	// NextTransition is when an open breaker goes half-open, or nil.
	NextTransition      *time.Time `json:"next_transition"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	ProbesInFlight      int        `json:"probes_in_flight"`
}

// RecentSnapshot counts what the breaker window holds.
type RecentSnapshot struct {
	WindowSeconds float64 `json:"window_seconds"`
	Requests      int64   `json:"requests"`
	Failures      int64   `json:"failures"`
}

// BudgetSnapshot is the retry budget over BudgetWindow.
type BudgetSnapshot struct {
	WindowSeconds float64 `json:"window_seconds"`
	Requests      int64   `json:"requests"`
	Used          int64   `json:"used"`
	Allowed       int64   `json:"allowed"`
}

// LatencySnapshot holds the latency statistics. TTFTMs and TokensPerSec
// are the current estimates (the hint until there are 20 samples); the
// EWMA fields are the measurements alone.
type LatencySnapshot struct {
	TTFTMs           float64 `json:"ttft_ms"`
	TTFTEWMAMs       float64 `json:"ttft_ewma_ms"`
	TTFTSamples      int64   `json:"ttft_samples"`
	TokensPerSec     float64 `json:"tokens_per_sec"`
	TokensPerSecEWMA float64 `json:"tokens_per_sec_ewma"`
	TPSSamples       int64   `json:"tokens_per_sec_samples"`
}

// Snapshot returns the state of every provider and deployment, sorted by
// ID. Deployments are listed under their provider.
func (t *Tracker) Snapshot() Snapshot {
	now := t.now()
	cfg := t.config()
	t.mu.Lock()
	provs := make([]*Provider, 0, len(t.providers))
	for _, p := range t.providers {
		provs = append(provs, p)
	}
	deps := make([]*Deployment, 0, len(t.deployments))
	for _, d := range t.deployments {
		deps = append(deps, d)
	}
	t.mu.Unlock()

	byProvider := make(map[string][]DeploymentSnapshot)
	for _, d := range deps {
		provider, ds := d.snapshot(now, cfg)
		byProvider[provider] = append(byProvider[provider], ds)
	}
	out := Snapshot{Providers: make([]ProviderSnapshot, 0, len(provs))}
	add := func(ps ProviderSnapshot) {
		ds := byProvider[ps.ID]
		delete(byProvider, ps.ID)
		slices.SortFunc(ds, func(a, b DeploymentSnapshot) int { return strings.Compare(a.ID, b.ID) })
		if ds == nil {
			ds = []DeploymentSnapshot{}
		}
		ps.Deployments = ds
		out.Providers = append(out.Providers, ps)
	}
	for _, p := range provs {
		add(ProviderSnapshot{ID: p.id, InFlight: p.InFlight(), MaxConcurrency: p.Max()})
	}
	// Deployments of a provider without a handle of its own.
	for id := range byProvider {
		add(ProviderSnapshot{ID: id})
	}
	slices.SortFunc(out.Providers, func(a, b ProviderSnapshot) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// round rounds a pro-rated window count to the nearest whole one.
func round(x float64) int64 { return int64(math.Round(x)) }

func (d *Deployment) snapshot(now time.Time, cfg Config) (string, DeploymentSnapshot) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.advance(now)
	reqs, fails := d.window.sum(now)
	breqs, retries := d.budget.sum(now)
	s := DeploymentSnapshot{
		ID: d.id,
		Breaker: BreakerSnapshot{
			State: d.state.String(), ConsecutiveFailures: d.consecutive, ProbesInFlight: d.probes,
		},
		Recent: RecentSnapshot{WindowSeconds: cfg.Window.Seconds(), Requests: round(reqs), Failures: round(fails)},
		RetryBudget: BudgetSnapshot{
			WindowSeconds: BudgetWindow.Seconds(), Requests: round(breqs), Used: round(retries),
			Allowed: int64(budgetAllowed(cfg, breqs)),
		},
		Latency: LatencySnapshot{
			TTFTMs:           d.ttft.estimate(float64(d.hint.TTFT)) / float64(time.Millisecond),
			TTFTEWMAMs:       d.ttft.value / float64(time.Millisecond),
			TTFTSamples:      d.ttft.n,
			TokensPerSec:     d.tps.estimate(d.hint.TokensPerSec),
			TokensPerSecEWMA: d.tps.value,
			TPSSamples:       d.tps.n,
		},
	}
	if d.state == StateOpen {
		t := d.openUntil
		s.Breaker.NextTransition = &t
	}
	if now.Before(d.cooldown) {
		t := d.cooldown
		s.CooldownUntil = &t
	}
	return d.provider, s
}
