package router

import (
	"sync"
	"testing"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/health"
	"switchyard/internal/policy"
	"switchyard/internal/providers"
	"switchyard/internal/quality"
	"switchyard/internal/schema"
)

// clock is a manual clock for the health tracker.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// trackerConfig opens a breaker after one failure, for 1 minute.
func trackerConfig() health.Config {
	return health.Config{
		ConsecutiveFailures: 1, FailureRatio: 1, MinRequests: 100, Window: time.Minute,
		OpenFor: time.Minute, HalfOpenProbes: 1,
		CooldownDefault: 30 * time.Second, CooldownMax: time.Minute,
		BudgetRatio: 0.2, BudgetMin: 2,
	}
}

func newTracker() *health.Tracker {
	return health.New(health.Options{Config: trackerConfig(), Now: newClock().Now})
}

// dspec describes a deployment of a test catalog.
type dspec struct {
	provider string
	in, out  string // prices; "" is unset
	tags     []string
	caps     []string // nil means the model's
	offset   float64
	ttftMs   int
}

// mspec describes a model of a test catalog.
type mspec struct {
	id      string
	typ     string // "" is chat
	ctx     int64
	ratio   float64 // token_ratio; 0 is 1
	caps    []string
	quality map[string]float64
	deps    []dspec
}

// pspec describes a provider.
type pspec struct {
	id      string
	tags    []string
	maxConc int
}

// buildCatalog builds a catalog by hand, without config validation.
func buildCatalog(tb testing.TB, provs []pspec, models []mspec) *catalog.Catalog {
	tb.Helper()
	cfg := &config.Config{}
	for _, p := range provs {
		cfg.Providers = append(cfg.Providers, config.Provider{ID: p.id, Type: "openai", Tags: p.tags, MaxConcurrency: p.maxConc})
	}
	for _, m := range models {
		cm := config.Model{ID: m.id, Type: m.typ, ContextWindow: m.ctx, TokenRatio: m.ratio, Capabilities: m.caps, Quality: m.quality}
		if cm.Type == "" {
			cm.Type = config.ModelTypeChat
		}
		for _, d := range m.deps {
			cm.Deployments = append(cm.Deployments, config.Deployment{
				Provider: d.provider, UpstreamModel: m.id,
				Pricing:      config.Pricing{Input: price(tb, d.in), Output: price(tb, d.out)},
				Capabilities: d.caps, Tags: d.tags, QualityOffset: d.offset,
				LatencyHint: config.LatencyHint{TTFTMs: d.ttftMs},
			})
		}
		cfg.Models = append(cfg.Models, cm)
	}
	c, err := catalog.New(cfg)
	if err != nil {
		tb.Fatalf("catalog.New: %v", err)
	}
	return c
}

func price(tb testing.TB, s string) config.Price {
	tb.Helper()
	if s == "" {
		return config.Price{}
	}
	p, err := config.ParsePrice(s)
	if err != nil {
		tb.Fatalf("ParsePrice(%q): %v", s, err)
	}
	return p
}

// basePolicy is the built-in policy with profile balanced.
func basePolicy() policy.Effective {
	return policy.Effective{
		Profile:      "balanced",
		Constraints:  policy.Constraints{MinQuality: 0.80, Objective: policy.ObjectiveCost},
		Substitution: policy.SubstitutionPinned, MaxQualityDrop: 0.05,
		RoutingMode: policy.ModeEnforce, OnUnmet: policy.OnUnmetBestEffort,
	}
}

// autoInput is an auto request with the base policy.
func autoInput() Input {
	return Input{
		Requested: "auto", Auto: true, Required: schema.CapabilitySet{},
		InputBytes: 6000, EstOut: 500,
		Assessment:           quality.Assessment{Category: quality.CategoryGeneral, Difficulty: 0.5},
		Policy:               basePolicy(),
		BudgetRemainingMicro: -1,
	}
}

// concreteInput is a request for a concrete model name.
func concreteInput(tb testing.TB, c *catalog.Catalog, name string) Input {
	tb.Helper()
	res, err := c.Resolve(name)
	if err != nil {
		tb.Fatalf("Resolve(%q): %v", name, err)
	}
	in := autoInput()
	in.Requested, in.Auto, in.Resolution = name, false, res
	return in
}

// planIDs returns the deployment IDs of a plan.
func planIDs(p []catalog.Candidate) []string {
	out := make([]string, len(p))
	for i, c := range p {
		out[i] = c.Deployment.ID
	}
	return out
}

// reasons maps each candidate's deployment to its excluded_by.
func reasons(e Explanation) map[string]string {
	out := map[string]string{}
	for _, c := range e.Candidates {
		if !c.Eligible {
			out[c.Deployment] = c.ExcludedBy
		}
	}
	return out
}

// openBreaker fails one attempt on deployment id, which opens its breaker
// under trackerConfig.
func openBreaker(tb testing.TB, tr *health.Tracker, id, provider string) {
	tb.Helper()
	d := tr.Deployment(id, provider, health.LatencyHint{})
	a, reason := d.Admit(false)
	if reason != "" {
		tb.Fatalf("Admit(%s): %s", id, reason)
	}
	d.Done(a, health.Result{Kind: providers.KindServerError})
}

// startCooldown rate-limits deployment id for 30 seconds.
func startCooldown(tb testing.TB, tr *health.Tracker, id, provider string) {
	tb.Helper()
	d := tr.Deployment(id, provider, health.LatencyHint{})
	a, reason := d.Admit(false)
	if reason != "" {
		tb.Fatalf("Admit(%s): %s", id, reason)
	}
	d.Done(a, health.Result{Kind: providers.KindRateLimited, RetryAfter: 30 * time.Second})
}
