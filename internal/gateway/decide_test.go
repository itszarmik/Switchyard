package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"switchyard/internal/auth"
	"switchyard/internal/budget"
	"switchyard/internal/config"
	"switchyard/internal/reqctx"
	"switchyard/internal/router"
)

// tierEnv is a gateway over three single-deployment chat models in the
// tiers of DESIGN §10.5: small@ps, medium@pm and large@pl, with the
// default policy's baseline large, for tenant t1 and its key k1.
type tierEnv struct {
	g          *Gateway
	reg        *prometheus.Registry
	logs       *logBuffer
	sink       *captureSink
	store      budget.Store
	s, m, l, e *scripted
	now        time.Time
}

// tierConfig is the tiers' config; edit changes it before the gateway is
// built.
func tierConfig(edit func(*config.Config)) *config.Config {
	q := func(v float64) map[string]float64 { return map[string]float64{"general": v, "coding": v} }
	caps := []string{"streaming", "stream_usage", "sampling", "completions"}
	cfg := &config.Config{
		Providers: []config.Provider{
			{ID: "ps", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"},
			{ID: "pm", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"},
			{ID: "pl", Type: "openai", BaseURL: "http://127.0.0.1:1/v1", Tags: []string{"zdr"}},
			{ID: "pe", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"},
		},
		Models: []config.Model{
			{ID: "small", Capabilities: caps, Quality: q(0.60), Deployments: []config.Deployment{
				{Provider: "ps", UpstreamModel: "s", Pricing: price("0.10", "0.40"), LatencyHint: config.LatencyHint{TTFTMs: 900}}}},
			{ID: "medium", Capabilities: caps, Quality: q(0.75), Deployments: []config.Deployment{
				{Provider: "pm", UpstreamModel: "m", Pricing: price("0.50", "2.00"), LatencyHint: config.LatencyHint{TTFTMs: 300}}}},
			{ID: "large", Capabilities: caps, Quality: q(0.95), Deployments: []config.Deployment{
				{Provider: "pl", UpstreamModel: "l", Pricing: price("3", "15"), LatencyHint: config.LatencyHint{TTFTMs: 600}}}},
			{ID: "emb", Type: "embedding", Deployments: []config.Deployment{
				{Provider: "pe", UpstreamModel: "e", Pricing: price("0.02", "0")},
				{Provider: "pl", UpstreamModel: "e", Pricing: price("0.01", "0")},
			}},
		},
		Aliases:  map[string]string{"careful": "auto:quality", "pin": "small@ps"},
		Policies: []config.Policy{{ID: "default", BaselineModel: "large"}},
		Tenants:  []config.Tenant{{ID: "t1", Keys: []config.APIKey{{ID: "k1", Tags: []string{"prod"}}}}},
		Routing:  fastRouting(),
	}
	cfg.Routing.Fallback.RetriesPerDeployment = 0
	if edit != nil {
		edit(cfg)
	}
	return cfg
}

func newTierEnv(t *testing.T, edit func(*config.Config), opts func(*Options)) *tierEnv {
	t.Helper()
	e := &tierEnv{
		reg: prometheus.NewRegistry(), logs: &logBuffer{}, sink: &captureSink{}, store: budget.NewMemoryStore(),
		s: &scripted{name: "ps"}, m: &scripted{name: "pm"}, l: &scripted{name: "pl"}, e: &scripted{name: "pe"},
		now: time.Date(2026, 11, 11, 15, 30, 0, 0, time.UTC),
	}
	o := Options{
		Config: tierConfig(edit), Logger: slog.New(slog.NewJSONHandler(e.logs, nil)), Registry: e.reg,
		Ledger: e.sink, Budgets: e.store, Clock: func() time.Time { return e.now },
	}
	if opts != nil {
		opts(&o)
	}
	g, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	e.g = g
	g.adapters["ps"], g.adapters["pm"], g.adapters["pl"], g.adapters["pe"] = e.s, e.m, e.l, e.e
	return e
}

// do serves body on endpoint as t1/k1 with the header pairs (name, value,
// name, value…).
func (e *tierEnv) do(endpoint, body string, header ...string) exchangeResult {
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Add(header[i], header[i+1])
	}
	ann := &reqctx.Annotations{}
	ctx := reqctx.WithAnnotations(reqctx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(e.logs, nil))), ann)
	ctx = auth.WithPrincipal(ctx, auth.Principal{TenantID: "t1", KeyID: "k1"})
	x := &hdrExchange{recExchange: recExchange{body: []byte(body)}, header: h}
	switch endpoint {
	case EndpointCompletions:
		e.g.Completions(ctx, x)
	case EndpointEmbeddings:
		e.g.Embeddings(ctx, x)
	case EndpointExplain:
		e.g.Explain(ctx, x)
	default:
		e.g.ChatCompletions(ctx, x)
	}
	x.res.ann = annotations(ann)
	return x.res
}

// served returns which tier adapters were called, in order s, m, l.
func (e *tierEnv) calls() [3]int { return [3]int{e.s.count(), e.m.count(), e.l.count()} }

const hiBody = `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`

func chatFor(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
}

// TestAutoRoutesByDifficulty checks that auto sends an easy request to the
// cheap tier and a hard one to the strong tier, and that the response
// headers, the access log and the usage event say so, with savings
// measured against the policy's baseline model.
func TestAutoRoutesByDifficulty(t *testing.T) {
	tests := []struct {
		difficulty string
		want       string
		calls      [3]int
	}{
		// q̂ = 1 − d·(1 − Q) against the balanced bar 0.80.
		{"low", "small", [3]int{1, 0, 0}},  // small 0.92
		{"0.6", "medium", [3]int{0, 1, 0}}, // small 0.76, medium 0.85
		{"high", "large", [3]int{0, 0, 1}}, // small 0.66, medium 0.7875, large 0.9575
	}
	for _, tt := range tests {
		t.Run(tt.difficulty, func(t *testing.T) {
			e := newTierEnv(t, nil, nil)
			r := e.do(EndpointChatCompletions, hiBody, HeaderDifficulty, tt.difficulty)
			if r.status != 200 {
				t.Fatalf("status %d: %+v", r.status, r.err)
			}
			if e.calls() != tt.calls {
				t.Errorf("calls %v, want %v", e.calls(), tt.calls)
			}
			if r.info.Model != tt.want || r.info.Profile != "balanced" || r.info.RouteReason != reasonCheapest || r.info.ShadowModel != "" {
				t.Errorf("info %+v", r.info)
			}
			ev := e.sink.only(t)
			if ev.RequestedModel != "auto" || ev.BaselineModel != "large" || ev.Profile != "balanced" ||
				ev.RoutingMode != "enforce" || ev.Deployment != tt.want+"@"+map[string]string{"small": "ps", "medium": "pm", "large": "pl"}[tt.want] {
				t.Errorf("event %+v", ev)
			}
			// defaultChat reports 3 prompt and 1 completion tokens: the
			// baseline, large, costs 0.000024.
			wantSavings := map[string]string{"small": "0.0000233", "medium": "0.0000205", "large": "0"}[tt.want]
			if got := ev.BaselineCost.Sub(ev.Cost).String(); got != wantSavings {
				t.Errorf("savings %s (baseline %s, cost %s), want %s", got, ev.BaselineCost, ev.Cost, wantSavings)
			}
			for _, k := range []string{"profile", "policy", "routing_mode", "category", "difficulty", "quality_bar", "served_q"} {
				if _, ok := r.ann[k]; !ok {
					t.Errorf("access log lacks %s: %v", k, r.ann)
				}
			}
			if r.ann["policy"] != "default" || r.ann["quality_bar"] != 0.8 {
				t.Errorf("access log %v", r.ann)
			}
			if got := testutil.ToFloat64(e.g.metrics.routeDecisions.WithLabelValues("balanced", tt.want, reasonCheapest)); got != 1 {
				t.Errorf("route_decisions_total = %v", got)
			}
		})
	}
}

// TestModelFieldSemantics checks each form of the model field (DESIGN §5).
func TestModelFieldSemantics(t *testing.T) {
	noBaseline := func(c *config.Config) { c.Policies[0].BaselineModel = "" }
	equivalent := func(c *config.Config) {
		c.Policies[0].Substitution = config.Substitution{Mode: config.SubstitutionEquivalent, MaxQualityDrop: 0.05}
	}
	allowed := func(c *config.Config) { c.Policies[0].AllowedProfiles = []string{"balanced", "quality"} }
	tests := []struct {
		name      string
		edit      func(*config.Config)
		body      string
		header    []string
		status    int
		code      string
		message   string
		model     string
		reason    string
		profile   string
		alias     string
		calls     [3]int
		requested string
	}{
		{name: "auto:cheap", body: chatFor("auto:cheap"), header: []string{HeaderDifficulty, "0.6"},
			status: 200, model: "small", reason: reasonCheapest, profile: "cheap", calls: [3]int{1, 0, 0}},
		{name: "auto:unknown", body: chatFor("auto:turbo"), status: 400, code: codeModelNotSupport,
			message: "unknown profile \"turbo\"; allowed profiles: balanced, cheap, fast, quality"},
		{name: "auto:<p> not allowed", edit: allowed, body: chatFor("auto:cheap"), status: 400, code: codeModelNotSupport,
			message: "profile \"cheap\" is not allowed by this policy; allowed profiles: balanced, quality"},
		{name: "alias to auto:quality", body: chatFor("careful"), header: []string{HeaderDifficulty, "0.6"},
			status: 200, model: "large", reason: reasonCheapest, profile: "quality", alias: "careful", calls: [3]int{0, 0, 1}},
		{name: "concrete is pinned", body: chatFor("medium"), header: []string{HeaderDifficulty, "low"},
			status: 200, model: "medium", reason: reasonCheapest, profile: "balanced", calls: [3]int{0, 1, 0}},
		{name: "concrete under equivalent", edit: equivalent, body: chatFor("medium"), header: []string{HeaderDifficulty, "low"},
			status: 200, model: "small", reason: reasonCheapest, profile: "balanced", calls: [3]int{1, 0, 0}},
		{name: "equivalent keeps a hard request", edit: equivalent, body: chatFor("large"), header: []string{HeaderDifficulty, "high"},
			status: 200, model: "large", reason: reasonCheapest, profile: "balanced", calls: [3]int{0, 0, 1}},
		{name: "model@provider is pinned", edit: equivalent, body: chatFor("medium@pm"), header: []string{HeaderDifficulty, "low"},
			status: 200, model: "medium", reason: reasonPinned, profile: "balanced", calls: [3]int{0, 1, 0}},
		{name: "alias to model@provider", body: chatFor("pin"), status: 200, model: "small", reason: reasonPinned,
			profile: "balanced", alias: "pin", calls: [3]int{1, 0, 0}},
		{name: "auto without baseline", edit: noBaseline, body: hiBody, status: 400, code: codeModelNotSupport,
			message: "this policy has no baseline_model; auto routing is disabled"},
		{name: "alias to auto without baseline", edit: noBaseline, body: chatFor("careful"), status: 400, code: codeModelNotSupport,
			message: "auto routing is disabled"},
		{name: "unknown model", body: chatFor("nope"), status: 404, code: codeModelNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTierEnv(t, tt.edit, nil)
			r := e.do(EndpointChatCompletions, tt.body, tt.header...)
			if r.status != tt.status {
				t.Fatalf("status %d (%+v), want %d", r.status, r.err, tt.status)
			}
			if tt.status != 200 {
				if r.err.Code != tt.code || !strings.Contains(r.err.Message, tt.message) {
					t.Errorf("error %+v, want %s containing %q", r.err, tt.code, tt.message)
				}
				if e.calls() != [3]int{} {
					t.Errorf("an upstream was called: %v", e.calls())
				}
				return
			}
			if r.info.Model != tt.model || r.info.RouteReason != tt.reason || r.info.Profile != tt.profile || r.info.Alias != tt.alias {
				t.Errorf("info %+v, want model %s reason %s profile %s alias %q", r.info, tt.model, tt.reason, tt.profile, tt.alias)
			}
			if e.calls() != tt.calls {
				t.Errorf("calls %v, want %v", e.calls(), tt.calls)
			}
		})
	}
}

// TestHeaderTightening checks that routing headers narrow the policy and
// never loosen it: a looser value is ignored with a warning, and a
// malformed one is 400 invalid_header without its value.
func TestHeaderTightening(t *testing.T) {
	capped := func(c *config.Config) { c.Policies[0].MaxCostPerRequestUSD = mustPriceS("0.005") }
	balancedOnly := func(c *config.Config) { c.Policies[0].AllowedProfiles = []string{"balanced"} }
	tests := []struct {
		name    string
		edit    func(*config.Config)
		header  []string
		status  int
		code    string
		model   string
		profile string
		warning string
		reason  string
	}{
		{name: "min quality raises the bar", header: []string{HeaderDifficulty, "0.6", "X-Switchyard-Min-Quality", "0.9"},
			status: 200, model: "large", profile: "balanced", reason: reasonCheapest},
		{name: "min quality above every selectable profile's is ignored", header: []string{HeaderDifficulty, "low", "X-Switchyard-Min-Quality", "0.96"},
			status: 200, model: "small", profile: "balanced", reason: reasonCheapest,
			warning: "X-Switchyard-Min-Quality ignored: it is above the quality bar of every profile this policy allows"},
		{name: "min quality cannot lower the bar", header: []string{HeaderDifficulty, "0.6", "X-Switchyard-Min-Quality", "0.1"},
			status: 200, model: "medium", profile: "balanced", reason: reasonCheapest,
			warning: "X-Switchyard-Min-Quality ignored: a header may only raise the policy's quality bar"},
		// The hard request needs large (est 0.00762 USD), which the cap
		// rules out: quality is relaxed to the best that fits.
		{name: "max cost lowers the cap", header: []string{HeaderDifficulty, "high", "X-Switchyard-Max-Cost-Usd", "0.002"},
			status: 200, model: "medium", profile: "balanced", reason: reasonQualityRelaxed},
		{name: "max cost cannot raise the cap", edit: capped, header: []string{HeaderDifficulty, "high", "X-Switchyard-Max-Cost-Usd", "1"},
			status: 200, model: "medium", profile: "balanced", reason: reasonQualityRelaxed,
			warning: "X-Switchyard-Max-Cost-Usd ignored: a header may only lower the policy's cost cap"},
		{name: "profile header selects", header: []string{HeaderDifficulty, "0.6", "X-Switchyard-Profile", "cheap"},
			status: 200, model: "small", profile: "cheap", reason: reasonCheapest},
		{name: "disallowed profile header is ignored", edit: balancedOnly, header: []string{HeaderDifficulty, "0.6", "X-Switchyard-Profile", "cheap"},
			status: 200, model: "medium", profile: "balanced", reason: reasonCheapest,
			warning: "X-Switchyard-Profile ignored: the profile is not allowed by this policy (allowed: balanced)"},
		{name: "latency target relaxed", header: []string{HeaderDifficulty, "low", "X-Switchyard-Max-TTFT-Ms", "100"},
			status: 200, model: "small", profile: "balanced", reason: reasonLatencyRelaxed},
		{name: "latency target picks a faster tier", header: []string{HeaderDifficulty, "low", "X-Switchyard-Max-TTFT-Ms", "500"},
			status: 200, model: "medium", profile: "balanced", reason: reasonCheapest},
		{name: "malformed max cost", header: []string{"X-Switchyard-Max-Cost-Usd", "lots"}, status: 400, code: codeInvalidHeader},
		{name: "malformed min quality", header: []string{"X-Switchyard-Min-Quality", "1.5"}, status: 400, code: codeInvalidHeader},
		{name: "malformed profile", header: []string{"X-Switchyard-Profile", "No Such"}, status: 400, code: codeInvalidHeader},
		{name: "malformed task", header: []string{HeaderTask, "poetry"}, status: 400, code: codeInvalidHeader},
		{name: "malformed difficulty", header: []string{HeaderDifficulty, "extreme"}, status: 400, code: codeInvalidHeader},
		{name: "difficulty out of range", header: []string{HeaderDifficulty, "1.5"}, status: 400, code: codeInvalidHeader},
		{name: "difficulty twice", header: []string{HeaderDifficulty, "low", HeaderDifficulty, "high"}, status: 400, code: codeInvalidHeader},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTierEnv(t, tt.edit, nil)
			r := e.do(EndpointChatCompletions, hiBody, tt.header...)
			if r.status != tt.status {
				t.Fatalf("status %d (%+v), want %d", r.status, r.err, tt.status)
			}
			if tt.status != 200 {
				if r.err.Code != tt.code || r.err.Param == "" {
					t.Errorf("error %+v, want %s with the header as param", r.err, tt.code)
				}
				for i := 1; i < len(tt.header); i += 2 {
					if strings.Contains(r.err.Message, tt.header[i]) {
						t.Errorf("the message %q echoes the value %q", r.err.Message, tt.header[i])
					}
				}
				return
			}
			if r.info.Model != tt.model || r.info.Profile != tt.profile || r.info.PolicyWarning != tt.warning || r.info.RouteReason != tt.reason {
				t.Errorf("info %+v\nwant model %s profile %s reason %s warning %q", r.info, tt.model, tt.profile, tt.reason, tt.warning)
			}
		})
	}
}

func mustPriceS(s string) config.Price {
	p, err := config.ParsePrice(s)
	if err != nil {
		panic(err)
	}
	return p
}

// TestCrossModelFallback checks that an auto plan falls back to another
// model that passed the same checks, and that the headers name it.
func TestCrossModelFallback(t *testing.T) {
	e := newTierEnv(t, nil, nil)
	e.s.script(step{err: err503})
	r := e.do(EndpointChatCompletions, hiBody, HeaderDifficulty, "low")
	if r.status != 200 || r.info.Model != "medium" || r.info.RouteReason != "fallback:server_error" || r.info.Attempts != 2 {
		t.Fatalf("status %d info %+v", r.status, r.info)
	}
	if e.calls() != [3]int{1, 1, 0} {
		t.Errorf("calls %v", e.calls())
	}
	if ev := e.sink.only(t); ev.Deployment != "medium@pm" || ev.BaselineModel != "large" {
		t.Errorf("event %+v", ev)
	}
}

// TestObserveMode checks that observe mode serves the requested model, or
// the baseline for auto, and records the shadow decision and its
// projected savings apart from the realized ones.
func TestObserveMode(t *testing.T) {
	observe := func(c *config.Config) { c.Policies[0].RoutingMode = config.RoutingModeObserve }
	tests := []struct {
		name   string
		body   string
		served string
		calls  [3]int
		shadow string
	}{
		{"auto serves the baseline", hiBody, "large", [3]int{0, 0, 1}, "small"},
		{"a concrete model is served as asked", chatFor("medium"), "medium", [3]int{0, 1, 0}, "medium"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTierEnv(t, func(c *config.Config) {
				observe(c)
				c.Policies[0].Substitution = config.Substitution{Mode: config.SubstitutionEquivalent, MaxQualityDrop: 0.05}
			}, nil)
			r := e.do(EndpointChatCompletions, tt.body, HeaderDifficulty, "low")
			if r.status != 200 || r.info.Model != tt.served || r.info.RouteReason != reasonObserve {
				t.Fatalf("status %d info %+v", r.status, r.info)
			}
			if e.calls() != tt.calls {
				t.Errorf("calls %v, want %v", e.calls(), tt.calls)
			}
			// Equivalent substitution would have chosen small for medium too.
			if r.info.ShadowModel != "small" {
				t.Errorf("shadow model %q, want small", r.info.ShadowModel)
			}
			ev := e.sink.only(t)
			if ev.RoutingMode != "observe" || ev.ShadowDeployment != "small@ps" || !ev.ShadowCostKnown {
				t.Errorf("event %+v", ev)
			}
			// 3 prompt and 1 completion tokens on small: 0.0000007.
			if ev.ShadowCost.String() != "0.0000007" || !ev.BaselineCost.Equal(ev.Cost) {
				t.Errorf("shadow cost %s, baseline %s, cost %s", ev.ShadowCost, ev.BaselineCost, ev.Cost)
			}
			if r.ann["shadow_deployment"] != "small@ps" || r.ann["routing_mode"] != "observe" {
				t.Errorf("access log %v", r.ann)
			}
			want := ev.BaselineCost.Sub(ev.ShadowCost).Float64()
			if got := testutil.ToFloat64(e.g.metrics.projectedSavings); got != want || got <= 0 {
				t.Errorf("projected savings metric %v, want %v", got, want)
			}
			if got := testutil.ToFloat64(e.g.metrics.savingsRouting); got != 0 {
				t.Errorf("realized routing savings %v, want 0", got)
			}
		})
	}
}

// TestSoftBudgetDowngrade checks that once a budget's soft threshold is
// crossed, auto requests use its soft profile, which the headers, the
// explanation and the metric show; concrete requests are unaffected.
func TestSoftBudgetDowngrade(t *testing.T) {
	limit := mustPriceS("1")
	e := newTierEnv(t, func(c *config.Config) {
		c.Tenants[0].Budgets = []config.Budget{{
			Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: limit,
			SoftAt: 0.5, SoftProfile: "cheap", OnExhausted: config.BudgetBlock,
		}}
	}, nil)
	// d = 0.6: balanced needs medium, cheap takes small.
	if r := e.do(EndpointChatCompletions, hiBody, HeaderDifficulty, "0.6"); r.info.Model != "medium" || r.info.RouteReason != reasonCheapest {
		t.Fatalf("before the threshold: %+v", r.info)
	}
	scope := e.g.scopes[principal{"t1", "k1"}][0]
	if _, err := e.store.Admit(context.Background(), []budget.Scope{scope}, 600_000, e.now); err != nil {
		t.Fatal(err)
	}
	r := e.do(EndpointChatCompletions, hiBody, HeaderDifficulty, "0.6")
	if r.status != 200 || r.info.Model != "small" || r.info.Profile != "cheap" || r.info.RouteReason != reasonBudgetDowngrade {
		t.Fatalf("after the threshold: status %d info %+v", r.status, r.info)
	}
	if got := testutil.ToFloat64(e.g.metrics.routeDowngrades.WithLabelValues("t1/day")); got != 1 {
		t.Errorf("route_profile_downgrades_total = %v, want 1", got)
	}
	x := e.do(EndpointExplain, hiBody, HeaderDifficulty, "0.6")
	var exp router.Explanation
	if err := json.Unmarshal([]byte(x.body), &exp); err != nil {
		t.Fatalf("explain: %v %s", err, x.body)
	}
	if exp.Downgrade != "budget t1/day soft threshold crossed; profile cheap" || exp.Profile != "cheap" {
		t.Errorf("explanation downgrade %q profile %q", exp.Downgrade, exp.Profile)
	}
	// The explanation is a dry run: no metric moves.
	if got := testutil.ToFloat64(e.g.metrics.routeDowngrades.WithLabelValues("t1/day")); got != 1 {
		t.Errorf("route_profile_downgrades_total = %v after explain, want 1", got)
	}
	if r := e.do(EndpointChatCompletions, chatFor("medium"), HeaderDifficulty, "0.6"); r.info.Model != "medium" || r.info.Profile != "balanced" {
		t.Errorf("a concrete request: %+v", r.info)
	}
}

// TestRouteBudgetRemaining checks that the remaining budget is a hard
// filter that steers auto to what fits, and that a request nothing fits
// still gets 429 budget_exceeded from the admission.
func TestRouteBudgetRemaining(t *testing.T) {
	e := newTierEnv(t, func(c *config.Config) {
		c.Tenants[0].Budgets = []config.Budget{{
			Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: mustPriceS("0.003"), OnExhausted: config.BudgetBlock,
		}}
	}, nil)
	// The hard request wants large (est 0.00762 USD); only medium fits.
	r := e.do(EndpointChatCompletions, hiBody, HeaderDifficulty, "high")
	if r.status != 200 || r.info.Model != "medium" || r.info.RouteReason != reasonQualityRelaxed {
		t.Fatalf("status %d info %+v", r.status, r.info)
	}
	x := e.do(EndpointExplain, hiBody, HeaderDifficulty, "high")
	if !strings.Contains(x.body, `"excluded_by": "est cost 0.007545 USD > remaining budget 0.00299`) {
		t.Errorf("explanation %s", x.body)
	}
	r = e.do(EndpointChatCompletions, chatFor("large"))
	if r.status != 429 || r.err.Code != codeBudgetExceeded {
		t.Errorf("a pinned request that does not fit: status %d %+v", r.status, r.err)
	}
}

// TestExplainEndpoint checks that the explain endpoint calls no upstream
// and records nothing, that it answers errors as chat does, and that an
// offline gateway (switchyard route explain) gives the same bytes as a
// fresh one.
func TestExplainEndpoint(t *testing.T) {
	e := newTierEnv(t, nil, nil)
	body := `{"model":"auto","messages":[{"role":"user","content":"Fix the failing test:\n` + "```go\\nfunc TestX(t *testing.T) { t.Fatal(1) }\\n```" + `"}]}`
	r := e.do(EndpointExplain, body, "X-Switchyard-Profile", "quality", "X-Switchyard-Min-Quality", "0.1")
	if r.status != 200 || e.calls() != [3]int{} || len(e.sink.events()) != 0 {
		t.Fatalf("status %d calls %v events %d", r.status, e.calls(), len(e.sink.events()))
	}
	var exp router.Explanation
	if err := json.Unmarshal([]byte(r.body), &exp); err != nil {
		t.Fatal(err)
	}
	if exp.Policy != "default" || exp.Profile != "quality" || exp.Mode != router.ModeAuto || len(exp.Candidates) != 5 ||
		len(exp.Plan) == 0 || exp.Assessment.Category != "coding" || len(exp.Warnings) != 1 {
		t.Errorf("explanation %s", r.body)
	}
	if r.info.Profile != "quality" || r.info.PolicyWarning == "" {
		t.Errorf("explain headers %+v", r.info)
	}
	if !strings.HasSuffix(r.body, "\n}\n") || !strings.Contains(r.body, "\n  \"policy\": \"default\",") ||
		!strings.Contains(r.body, " < 0.92") {
		t.Errorf("body is not indented JSON ending in a newline, with < unescaped:\n%s", r.body)
	}

	offline := newTierEnv(t, nil, func(o *Options) { o.Offline = true })
	h := http.Header{}
	h.Set("X-Switchyard-Profile", "quality")
	h.Set("X-Switchyard-Min-Quality", "0.1")
	got, gerr := offline.g.ExplainRequest(context.Background(), "t1", "k1", h, []byte(body))
	if gerr != nil || !bytes.Equal(got, []byte(r.body)) {
		t.Errorf("offline explanation differs (%v):\n%s\nendpoint:\n%s", gerr, got, r.body)
	}

	for _, tt := range []struct {
		name, body string
		header     []string
		code       string
	}{
		{"bad json", `{`, nil, "invalid_json"},
		{"unknown model", chatFor("nope"), nil, codeModelNotFound},
		{"bad header", hiBody, []string{"X-Switchyard-Min-Quality", "2"}, codeInvalidHeader},
		{"bad tags", hiBody, []string{HeaderTags, "x"}, codeInvalidTags},
		{"bad timeout", hiBody, []string{HeaderTimeoutMs, "-1"}, codeInvalidTimeout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := e.do(EndpointExplain, tt.body, tt.header...)
			c := e.do(EndpointChatCompletions, tt.body, tt.header...)
			if r.err == nil || r.err.Code != tt.code || c.err == nil || c.err.Code != r.err.Code || c.status != r.status {
				t.Errorf("explain %d %+v, chat %d %+v, want %s from both", r.status, r.err, c.status, c.err, tt.code)
			}
		})
	}
}

// TestEmbeddingsPolicy checks that the policy's deny list applies to
// embedding deployments, and that a virtual model is refused.
func TestEmbeddingsPolicy(t *testing.T) {
	e := newTierEnv(t, func(c *config.Config) { c.Policies[0].Deny = []string{"emb@pl"} }, nil)
	r := e.do(EndpointEmbeddings, `{"model":"emb","input":"x"}`)
	if r.status != 200 || r.info.Provider != "pe" || e.e.count() != 1 || e.l.count() != 0 {
		t.Fatalf("status %d info %+v", r.status, r.info)
	}
	if ev := e.sink.only(t); ev.RoutingMode != "enforce" || ev.Profile != "" {
		t.Errorf("event %+v", ev)
	}
	r = e.do(EndpointEmbeddings, `{"model":"auto","input":"x"}`)
	if r.status != 400 || r.err.Code != codeModelNotSupport {
		t.Errorf("auto embeddings: %d %+v", r.status, r.err)
	}
	e2 := newTierEnv(t, func(c *config.Config) { c.Policies[0].RequireTags = []string{"gpu"} }, nil)
	r = e2.do(EndpointEmbeddings, `{"model":"emb","input":"x"}`)
	if r.status != 400 || r.err.Code != codeNoEligible || !strings.Contains(r.err.Message, "lacks required tag gpu") {
		t.Errorf("require_tags: %d %+v", r.status, r.err)
	}
}

// TestCompletionsRouted checks that legacy completions are routed as chat
// requests are.
func TestCompletionsRouted(t *testing.T) {
	e := newTierEnv(t, nil, nil)
	r := e.do(EndpointCompletions, `{"model":"auto","prompt":"hi"}`, HeaderDifficulty, "high")
	if r.status != 200 || r.info.Model != "large" || r.info.Profile != "balanced" {
		t.Fatalf("status %d info %+v", r.status, r.info)
	}
	if ev := e.sink.only(t); ev.BaselineModel != "large" || ev.Endpoint != EndpointCompletions {
		t.Errorf("event %+v", ev)
	}
}

// TestListModelsVirtual checks that /v1/models lists the virtual models
// the caller's policy allows, and none without a baseline.
func TestListModelsVirtual(t *testing.T) {
	ids := func(ms []Model) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return out
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "t1", KeyID: "k1"})
	e := newTierEnv(t, func(c *config.Config) { c.Policies[0].AllowedProfiles = []string{"quality", "cheap"} }, nil)
	want := []string{"small", "medium", "large", "emb", "auto", "auto:cheap", "auto:quality", "careful", "pin"}
	if got := ids(e.g.ListModels(ctx)); !slices.Equal(got, want) {
		t.Errorf("models %v, want %v", got, want)
	}
	e = newTierEnv(t, func(c *config.Config) { c.Policies[0].BaselineModel = "" }, nil)
	want = []string{"small", "medium", "large", "emb", "careful", "pin"}
	if got := ids(e.g.ListModels(ctx)); !slices.Equal(got, want) {
		t.Errorf("without a baseline: %v, want %v", got, want)
	}
}

// TestRouteReasonPreferred checks that a preference tier that wins over a
// cheaper eligible deployment is reported as the route reason, and that
// one that changes nothing is not.
func TestRouteReasonPreferred(t *testing.T) {
	for _, tt := range []struct {
		prefer []string
		model  string
		reason string
	}{
		{[]string{"medium@*"}, "medium", reasonPreferred},
		{[]string{"small@*"}, "small", reasonCheapest},
	} {
		e := newTierEnv(t, func(c *config.Config) { c.Policies[0].Prefer = tt.prefer }, nil)
		r := e.do(EndpointChatCompletions, hiBody, HeaderDifficulty, "low")
		if r.status != 200 || r.info.Model != tt.model || r.info.RouteReason != tt.reason {
			t.Errorf("prefer %v: status %d info %+v, want %s %s", tt.prefer, r.status, r.info, tt.model, tt.reason)
		}
	}
}

// TestRouteLogRule checks that the access log names the governance and the
// quality rule that applied, and leaves each out when none did.
func TestRouteLogRule(t *testing.T) {
	e := newTierEnv(t, func(c *config.Config) {
		c.Policies[0].Rules = []config.Rule{
			{Name: "hard-coding", Match: config.RuleMatch{DifficultyGte: 0.8}, Set: config.PolicyOverride{MinQuality: 0.9}},
			{Name: "streams", Match: config.RuleMatch{Stream: "true"}, Set: config.PolicyOverride{OnUnmet: config.OnUnmetBestEffort}},
		}
	}, nil)
	r := e.do(EndpointChatCompletions, hiBody, HeaderDifficulty, "high")
	if r.ann["matched_quality_rule"] != "hard-coding" || r.ann["quality_bar"] != 0.9 {
		t.Errorf("access log %v", r.ann)
	}
	if _, ok := r.ann["matched_rule"]; ok {
		t.Errorf("matched_rule logged without a governance match: %v", r.ann)
	}
	r = e.do(EndpointChatCompletions, `{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`, HeaderDifficulty, "low")
	if r.ann["matched_rule"] != "streams" {
		t.Errorf("access log %v", r.ann)
	}
	if _, ok := r.ann["matched_quality_rule"]; ok {
		t.Errorf("matched_quality_rule logged without a quality match: %v", r.ann)
	}
}
