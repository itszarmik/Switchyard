package router

import (
	"net/http"
	"strings"
	"testing"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/policy"
)

// Regression tests for the Phase 5 review findings.

// tierCatalog has three single-deployment tiers: s (Q 0.60), m (0.75) and
// l (0.95).
func tierCatalog(tb testing.TB) *catalog.Catalog {
	return buildCatalog(tb, []pspec{{id: "p1"}, {id: "p2"}, {id: "p3"}}, []mspec{
		{id: "s", ctx: 100000, quality: map[string]float64{"general": 0.60}, deps: []dspec{{provider: "p1", in: "0.10", out: "0.40"}}},
		{id: "m", ctx: 100000, quality: map[string]float64{"general": 0.75}, deps: []dspec{{provider: "p2", in: "0.50", out: "2.00"}}},
		{id: "l", ctx: 100000, quality: map[string]float64{"general": 0.95}, deps: []dspec{{provider: "p3", in: "3.00", out: "15.00"}}},
	})
}

// TestEquivalentHonorsQualityFloor: in equivalent mode the profile's bar
// does not apply, but a rule's or a header's min_quality (the quality
// floor) does: the bar is max(q̂(requested) − max_quality_drop, floor).
func TestEquivalentHonorsQualityFloor(t *testing.T) {
	c := tierCatalog(t)
	r := New(c, nil, config.Routing{})
	// At d = 0.5: q̂ s 0.80, m 0.875, l 0.975; m − 0.05 = 0.825.
	tests := []struct {
		name       string
		minQuality float64
		floor      float64
		bar        float64
		plan       []string
	}{
		{"no floor", 0.80, 0, 0.825, []string{"m@p2", "l@p3"}},
		{"the profile's bar does not apply", 0.99, 0, 0.825, []string{"m@p2", "l@p3"}},
		{"a floor below the substitution bar", 0.80, 0.81, 0.825, []string{"m@p2", "l@p3"}},
		{"a floor above it", 0.95, 0.95, 0.95, []string{"l@p3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := concreteInput(t, c, "m")
			in.Policy.Substitution = policy.SubstitutionEquivalent
			in.Policy.Constraints.MinQuality, in.Policy.Constraints.QualityFloor = tt.minQuality, tt.floor
			dec, err := r.Decide(in)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if got := planIDs(dec.Plan); dec.Explanation.QualityBar != tt.bar || strings.Join(got, ",") != strings.Join(tt.plan, ",") {
				t.Errorf("bar %v plan %v; want %v %v", dec.Explanation.QualityBar, got, tt.bar, tt.plan)
			}
		})
	}
}

// TestEquivalentHeaderMinQuality: X-Switchyard-Min-Quality, through the
// policy engine, holds an equivalent request's substitutes to it.
func TestEquivalentHeaderMinQuality(t *testing.T) {
	cfg := &config.Config{Policies: []config.Policy{{ID: "default", BaselineModel: "l",
		Substitution: config.Substitution{Mode: config.SubstitutionEquivalent, MaxQualityDrop: 0.05}}}}
	e, err := policy.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eff, err := e.Resolve(config.Tenant{ID: "t"}, "", policy.Request{Model: "m", Headers: http.Header{policy.HeaderMinQuality: {"0.9"}}})
	if err != nil {
		t.Fatal(err)
	}
	c := tierCatalog(t)
	in := concreteInput(t, c, "m")
	in.Policy = eff
	dec, err := New(c, nil, config.Routing{}).Decide(in)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := planIDs(dec.Plan); len(got) != 1 || got[0] != "l@p3" || dec.Explanation.QualityBar != 0.9 {
		t.Errorf("plan %v bar %v, want [l@p3] under the header's 0.9 (m's q̂ is 0.875)", got, dec.Explanation.QualityBar)
	}
}

// TestCostCapIsExact: a cost cap below a micro-USD is compared exactly,
// not rounded up to a whole micro-USD.
func TestCostCapIsExact(t *testing.T) {
	e, err := policy.New(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	c := buildCatalog(t, []pspec{{id: "p1"}}, []mspec{{id: "s", ctx: 100000, deps: []dspec{{provider: "p1", in: "0.10", out: "0.40"}}}})
	r := New(c, nil, config.Routing{})
	// 5 input tokens at 0.10 USD per million cost 0.0000005 USD.
	for _, tt := range []struct {
		header string
		ok     bool
	}{
		{"0.0000001", false},
		{"0.000000499", false},
		{"0.0000005", true},
		{"0.000000501", true},
	} {
		eff, err := e.Resolve(config.Tenant{ID: "t"}, "", policy.Request{Model: "s", Headers: http.Header{policy.HeaderMaxCostUSD: {tt.header}}})
		if err != nil {
			t.Fatal(err)
		}
		in := concreteInput(t, c, "s")
		in.Policy = eff
		in.InputBytes, in.EstOut = 20, 0
		dec, err := r.Decide(in)
		if (err == nil) != tt.ok {
			t.Errorf("cap %s USD: err %v, want ok=%v (%s)", tt.header, err, tt.ok, dec.Explanation.Candidates[0].ExcludedBy)
		}
		if !tt.ok && !strings.Contains(dec.Explanation.Candidates[0].ExcludedBy, "est cost 0.0000005 USD > max_cost_per_request "+strings.TrimRight(tt.header, "0")+" USD") {
			t.Errorf("cap %s USD: excluded_by %q", tt.header, dec.Explanation.Candidates[0].ExcludedBy)
		}
	}
}

// TestTokenRatioPerCandidate: every candidate's cost and context check use
// its own model's token_ratio, not the requested model's or 1.
func TestTokenRatioPerCandidate(t *testing.T) {
	c := buildCatalog(t, []pspec{{id: "p1"}, {id: "p2"}, {id: "p3"}}, []mspec{
		// s counts 10 tokens where the others count 1: 100000 input tokens.
		{id: "s", ctx: 1_000_000, ratio: 10, quality: map[string]float64{"general": 0.9}, deps: []dspec{{provider: "p1", in: "0.10", out: "0.40"}}},
		{id: "m", ctx: 1_000_000, quality: map[string]float64{"general": 0.9}, deps: []dspec{{provider: "p2", in: "0.50", out: "2.00"}}},
		// t would be cheapest at ratio 1, but its context cannot hold its
		// own estimate.
		{id: "t", ctx: 50_000, ratio: 6, quality: map[string]float64{"general": 0.9}, deps: []dspec{{provider: "p3", in: "0.01", out: "0.01"}}},
	})
	r := New(c, nil, config.Routing{})
	in := autoInput()
	in.InputBytes, in.ImageTokens, in.EstOut = 40_000, 1600, 500
	dec, err := r.Decide(in)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	cost := map[string]string{}
	for _, ci := range dec.Explanation.Candidates {
		cost[ci.Deployment] = ci.EstCostUSD
	}
	// s: 101600 × 0.10/M + 500 × 0.40/M; m: 11600 × 0.50/M + 500 × 2/M.
	if cost["s@p1"] != "0.01036" || cost["m@p2"] != "0.0068" {
		t.Errorf("costs %v", cost)
	}
	if got := planIDs(dec.Plan); len(got) != 2 || got[0] != "m@p2" || got[1] != "s@p1" {
		t.Errorf("plan %v, want m@p2 before s@p1", got)
	}
	if got := reasons(dec.Explanation)["t@p3"]; got != "context 50000 < 62100 needed" {
		t.Errorf("t@p3 excluded by %q", got)
	}
	if dec.Explanation.Estimates.InputTokens != 11600 {
		t.Errorf("explained input %d, want 11600 (ratio 1 for auto)", dec.Explanation.Estimates.InputTokens)
	}

	// A cap that s fits at ratio 1 but not at its own ratio.
	in.Policy.MaxCostPerRequestNano = 8_000_000 // 0.008 USD
	in.Policy.Allow = []string{"s@*"}
	if _, err := r.Decide(in); err == nil {
		t.Error("s@p1 fits a 0.008 USD cap at ratio 1, but costs 0.01036 USD at its own")
	}

	// A concrete request explains the input at its model's ratio.
	cin := concreteInput(t, c, "s")
	cin.InputBytes, cin.ImageTokens = 40_000, 1600
	if dec, err := r.Decide(cin); err != nil || dec.Explanation.Estimates.InputTokens != 101600 {
		t.Errorf("concrete s: input %d (%v), want 101600", dec.Explanation.Estimates.InputTokens, err)
	}
}

// TestCostCapReason checks the cap check the gateway applies to embedding
// deployments.
func TestCostCapReason(t *testing.T) {
	c := buildCatalog(t, []pspec{{id: "p1"}}, []mspec{{id: "e", typ: config.ModelTypeEmbedding, deps: []dspec{
		{provider: "p1", in: "0.02", out: "0"}}}, {id: "u", typ: config.ModelTypeEmbedding, deps: []dspec{{provider: "p1"}}}})
	e, u := c.Models()[0].Deployments[0], c.Models()[1].Deployments[0]
	pol := &policy.Effective{}
	if got := CostCapReason(pol, e, 1_000_000, 0); got != "" {
		t.Errorf("no cap: %q", got)
	}
	pol.MaxCostPerRequestNano = 20_000_000 // 0.02 USD
	if got := CostCapReason(pol, e, 1_000_000, 0); got != "" {
		t.Errorf("at the cap: %q", got)
	}
	if got := CostCapReason(pol, e, 1_000_001, 0); got != "est cost 0.02000002 USD > max_cost_per_request 0.02 USD" {
		t.Errorf("over the cap: %q", got)
	}
	if got := CostCapReason(pol, u, 1, 0); got != "cost unknown (pricing not set) under max_cost_per_request 0.02 USD" {
		t.Errorf("unknown pricing: %q", got)
	}
}
