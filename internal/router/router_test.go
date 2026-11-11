package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/health"
	"switchyard/internal/policy"
	"switchyard/internal/quality"
	"switchyard/internal/schema"
)

// workedExample is the catalog of DESIGN §10.5.
func workedExample(tb testing.TB) *catalog.Catalog {
	return buildCatalog(tb,
		[]pspec{{id: "together"}, {id: "openai"}, {id: "anthropic"}},
		[]mspec{
			{id: "small", quality: map[string]float64{"coding": 0.60}, deps: []dspec{{provider: "together", in: "0.10", out: "0.40"}}},
			{id: "medium", quality: map[string]float64{"coding": 0.75}, deps: []dspec{{provider: "openai", in: "0.50", out: "2.00"}}},
			{id: "large", quality: map[string]float64{"coding": 0.95}, deps: []dspec{{provider: "anthropic", in: "3.00", out: "15.00"}}},
		})
}

// TestWorkedExample reproduces DESIGN §10.5: three coding requests of
// 1,500 input and 500 output tokens under balanced (bar 0.80) go to S, M
// and L.
func TestWorkedExample(t *testing.T) {
	r := New(workedExample(t), newTracker(), config.Routing{})
	tests := []struct {
		name  string
		d     float64
		want  string
		wantQ []float64 // S, M, L
		plan  []string
	}{
		{"rename", 0.20, "small@together", []float64{0.92, 0.95, 0.99}, []string{"small@together", "medium@openai", "large@anthropic"}},
		{"fix failing test", 0.55, "medium@openai", []float64{0.78, 0.8625, 0.9725}, []string{"medium@openai", "large@anthropic"}},
		{"lock-free queue", 0.90, "large@anthropic", []float64{0.64, 0.775, 0.955}, []string{"large@anthropic"}},
	}
	var total femto
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := autoInput()
			in.Assessment = quality.Assessment{Category: quality.CategoryCoding, Difficulty: tt.d}
			dec, err := r.Decide(in)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if diff := cmp.Diff(tt.plan, planIDs(dec.Plan)); diff != "" {
				t.Errorf("plan (-want +got):\n%s", diff)
			}
			for i, c := range dec.Explanation.Candidates {
				if c.Q != tt.wantQ[i] {
					t.Errorf("%s q = %v, want %v", c.Deployment, c.Q, tt.wantQ[i])
				}
			}
			if got := dec.Explanation.Candidates; got[0].EstCostUSD != "0.00035" || got[1].EstCostUSD != "0.00175" || got[2].EstCostUSD != "0.012" {
				t.Errorf("costs = %s, %s, %s", got[0].EstCostUSD, got[1].EstCostUSD, got[2].EstCostUSD)
			}
			if len(dec.Relaxed) != 0 {
				t.Errorf("relaxed = %v", dec.Relaxed)
			}
			for _, c := range dec.Explanation.Candidates {
				if c.Deployment == tt.want {
					total = total.add(mustCost(t, c.EstCostUSD))
				}
			}
		})
	}
	// $0.0141 in all, instead of $0.036 at L.
	if got := total.String(); got != "0.0141" {
		t.Errorf("total = %s, want 0.0141", got)
	}
}

// mustCost parses a decimal USD amount with at most 15 decimals.
func mustCost(t *testing.T, s string) femto {
	t.Helper()
	whole, frac, _ := strings.Cut(s, ".")
	p, err := config.ParsePrice(whole + "." + (frac + "000000000")[:9])
	if err != nil || len(frac) > 9 {
		t.Fatalf("cost %q: %v", s, err)
	}
	n, _ := nanoPerMillion(p)
	return mulFemto(n, 1_000_000)
}

// TestExplanationShape checks the DESIGN §9.7 example: the second worked
// request with a rule raising the bar to 0.85.
func TestExplanationShape(t *testing.T) {
	r := New(workedExample(t), newTracker(), config.Routing{})
	in := autoInput()
	in.Policy.PolicyID, in.Policy.MatchedQualityRule = "default", "code-needs-quality"
	in.Policy.Constraints.MinQuality, in.Policy.Constraints.QualityFloor = 0.85, 0.85
	in.Assessment = quality.Assessment{Category: quality.CategoryCoding, Difficulty: 0.55, Signals: []string{"code_block", "stack_trace"}}
	dec, err := r.Decide(in)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	got, err := json.MarshalIndent(dec.Explanation, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "policy": "default",
  "matched_rule": "",
  "matched_quality_rule": "code-needs-quality",
  "profile": "balanced",
  "mode": "auto",
  "assessment": {
    "category": "coding",
    "difficulty": 0.55,
    "signals": [
      "code_block",
      "stack_trace"
    ],
    "hinted": false
  },
  "estimates": {
    "input_tokens": 1500,
    "output_tokens": 500
  },
  "quality_bar": 0.85,
  "candidates": [
    {
      "deployment": "small@together",
      "model": "small",
      "q": 0.78,
      "est_cost_usd": "0.00035",
      "est_ttft_ms": 1000,
      "tier": 0,
      "eligible": false,
      "excluded_by": "quality 0.78 < 0.85",
      "notes": [
        "no latency estimate; assumed 1000ms"
      ]
    },
    {
      "deployment": "medium@openai",
      "model": "medium",
      "q": 0.8625,
      "est_cost_usd": "0.00175",
      "est_ttft_ms": 1000,
      "tier": 0,
      "eligible": true,
      "rank": 1,
      "notes": [
        "no latency estimate; assumed 1000ms"
      ]
    },
    {
      "deployment": "large@anthropic",
      "model": "large",
      "q": 0.9725,
      "est_cost_usd": "0.012",
      "est_ttft_ms": 1000,
      "tier": 0,
      "eligible": true,
      "rank": 2,
      "notes": [
        "no latency estimate; assumed 1000ms"
      ]
    }
  ],
  "plan": [
    "medium@openai",
    "large@anthropic"
  ],
  "relaxed": [],
  "dropped_params": [],
  "warnings": []
}`
	// encoding/json escapes "<" as \u003c.
	if diff := cmp.Diff(want, strings.ReplaceAll(string(got), `\u003c`, "<")); diff != "" {
		t.Errorf("explanation (-want +got):\n%s", diff)
	}
}

// exclusionCatalog has a cheap deployment "a" that each test case excludes
// for one reason, and a pricier fallback "b" that stays eligible.
func exclusionCatalog(tb testing.TB) *catalog.Catalog {
	return buildCatalog(tb,
		[]pspec{{id: "p"}, {id: "q", tags: []string{"zdr"}}, {id: "busy", maxConc: 1}},
		[]mspec{
			{id: "a", ctx: 4000, caps: []string{"streaming"}, quality: map[string]float64{"general": 0.9},
				deps: []dspec{{provider: "p", in: "0.10", out: "0.10", ttftMs: 300}}},
			{id: "b", caps: []string{"streaming", "tools"}, quality: map[string]float64{"general": 0.9},
				deps: []dspec{{provider: "q", in: "1", out: "1", ttftMs: 300}}},
			{id: "e", typ: config.ModelTypeEmbedding, deps: []dspec{{provider: "p", in: "0.01"}}},
			{id: "c", caps: []string{"streaming", "tools"}, quality: map[string]float64{"general": 0.9},
				deps: []dspec{{provider: "busy", in: "0.05", out: "0.05", ttftMs: 300}}},
		})
}

// TestExclusions checks that every hard filter excludes with the right
// reason, never relaxed, and that the chosen deployment is the cheapest one
// that passes them all.
func TestExclusions(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Input)
		setup  func(tb testing.TB, tr *health.Tracker)
		want   string            // the primary
		reason map[string]string // excluded_by of the given deployments
	}{
		{
			name: "chat only", want: "c@busy",
			reason: map[string]string{"e@p": "not a chat model"},
		},
		{
			name: "deny wins over allow", want: "b@q",
			modify: func(in *Input) { in.Policy.Allow = []string{"*"}; in.Policy.Deny = []string{"a@*", "c@*"} },
			reason: map[string]string{"a@p": `denied by policy (deny "a@*")`, "c@busy": `denied by policy (deny "c@*")`},
		},
		{
			name: "allow", want: "b@q",
			modify: func(in *Input) { in.Policy.Allow = []string{"b@*"} },
			reason: map[string]string{"a@p": "not allowed by policy", "c@busy": "not allowed by policy"},
		},
		{
			name: "require tags", want: "b@q",
			modify: func(in *Input) { in.Policy.RequireTags = []string{"zdr"} },
			reason: map[string]string{"a@p": "lacks required tag zdr", "c@busy": "lacks required tag zdr"},
		},
		{
			name: "require several tags", want: "",
			modify: func(in *Input) { in.Policy.RequireTags = []string{"zdr", "eu"} },
			reason: map[string]string{"a@p": "lacks required tags zdr, eu", "b@q": "lacks required tag eu"},
		},
		{
			name: "capability", want: "c@busy",
			modify: func(in *Input) { in.Required = schema.CapabilitySet{schema.CapTools: {}} },
			reason: map[string]string{"a@p": "lacks capability tools"},
		},
		{
			name: "context", want: "c@busy",
			modify: func(in *Input) { in.InputBytes = 16000 },
			reason: map[string]string{"a@p": "context 4000 < 4500 needed"},
		},
		{
			name: "breaker open", want: "a@p",
			setup:  func(tb testing.TB, tr *health.Tracker) { openBreaker(tb, tr, "c@busy", "busy") },
			reason: map[string]string{"c@busy": "circuit open"},
		},
		{
			name: "cooldown", want: "a@p",
			setup:  func(tb testing.TB, tr *health.Tracker) { startCooldown(tb, tr, "c@busy", "busy") },
			reason: map[string]string{"c@busy": "rate-limit cooldown 30s"},
		},
		{
			name: "saturated", want: "a@p",
			setup: func(tb testing.TB, tr *health.Tracker) {
				if !tr.Provider("busy", 1).Acquire() {
					tb.Fatal("Acquire failed")
				}
			},
			reason: map[string]string{"c@busy": "provider at max_concurrency 1"},
		},
		{
			// a costs 0.0002, b 0.002, c 0.0001.
			name: "cost cap", want: "c@busy",
			modify: func(in *Input) { in.Policy.MaxCostPerRequestNano = 150_000 },
			reason: map[string]string{
				"a@p": "est cost 0.0002 USD > max_cost_per_request 0.00015 USD",
				"b@q": "est cost 0.002 USD > max_cost_per_request 0.00015 USD",
			},
		},
		{
			name: "budget", want: "a@p",
			modify: func(in *Input) {
				in.Policy.MaxCostPerRequestNano = 5_000_000
				in.BudgetRemainingMicro = 1000
				in.Policy.Deny = []string{"c@*"}
			},
			reason: map[string]string{"b@q": "est cost 0.002 USD > remaining budget 0.001 USD"},
		},
		{
			name: "budget exhausted", want: "",
			modify: func(in *Input) { in.BudgetRemainingMicro = 0 },
			reason: map[string]string{
				"a@p": "est cost 0.0002 USD > remaining budget 0 USD",
				"e@p": "not a chat model",
			},
		},
		{
			name: "gateway filter", want: "a@p",
			modify: func(in *Input) {
				in.Keep = func(d *catalog.Deployment) string {
					if d.Provider == "busy" {
						return "provider type x is not supported"
					}
					return ""
				}
			},
			reason: map[string]string{"c@busy": "provider type x is not supported"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTracker()
			if tt.setup != nil {
				tt.setup(t, tr)
			}
			r := New(exclusionCatalog(t), tr, config.Routing{})
			in := autoInput()
			in.Assessment.Difficulty = 0
			if tt.modify != nil {
				tt.modify(&in)
			}
			dec, err := r.Decide(in)
			got := reasons(dec.Explanation)
			for id, want := range tt.reason {
				if got[id] != want {
					t.Errorf("%s excluded_by = %q, want %q", id, got[id], want)
				}
			}
			if tt.want == "" {
				var ne *catalog.NoEligibleError
				if !errors.As(err, &ne) || len(ne.Exclusions) != len(dec.Explanation.Candidates) {
					t.Fatalf("err = %v, want a NoEligibleError for every candidate", err)
				}
				for _, x := range ne.Exclusions {
					if x.Reason == "" || x.Reason != got[x.Deployment] {
						t.Errorf("error exclusion %s = %q, explanation %q", x.Deployment, x.Reason, got[x.Deployment])
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if dec.Plan[0].Deployment.ID != tt.want {
				t.Errorf("primary = %s, want %s (plan %v)", dec.Plan[0].Deployment.ID, tt.want, planIDs(dec.Plan))
			}
			for _, p := range dec.Plan {
				if reason, ok := got[p.Deployment.ID]; ok {
					t.Errorf("planned %s is excluded: %s", p.Deployment.ID, reason)
				}
			}
			// Every candidate has a reason or a rank, never both.
			for _, c := range dec.Explanation.Candidates {
				if c.Eligible == (c.ExcludedBy != "") || c.Eligible != (c.Rank > 0) {
					t.Errorf("candidate %+v", c)
				}
			}
		})
	}
}

// TestUnhealthy: every candidate passing the other hard filters is
// skipped for health, so the gateway answers 503, not 400.
func TestUnhealthy(t *testing.T) {
	tr := newTracker()
	openBreaker(t, tr, "a@p", "p")
	startCooldown(t, tr, "b@q", "q")
	openBreaker(t, tr, "c@busy", "busy")
	r := New(exclusionCatalog(t), tr, config.Routing{})
	dec, err := r.Decide(autoInput())
	if err != nil || !dec.Unhealthy || len(dec.Plan) != 0 {
		t.Fatalf("Decide = unhealthy %v, plan %v, err %v; want unhealthy", dec.Unhealthy, planIDs(dec.Plan), err)
	}
	want := map[string]string{"a@p": "circuit open", "b@q": "rate-limit cooldown 30s", "c@busy": "circuit open", "e@p": "not a chat model"}
	if diff := cmp.Diff(want, reasons(dec.Explanation)); diff != "" {
		t.Errorf("reasons (-want +got):\n%s", diff)
	}

	// With a hard reason besides health for the healthy ones, it is 400.
	in := autoInput()
	in.Policy.Deny = []string{"b@*"}
	dec, err = r.Decide(in)
	if !dec.Unhealthy || err != nil {
		t.Errorf("deny b: unhealthy %v, err %v; want unhealthy (a and c are only unhealthy)", dec.Unhealthy, err)
	}
	in.Policy.Deny = []string{"*"}
	dec, err = r.Decide(in)
	var ne *catalog.NoEligibleError
	if dec.Unhealthy || !errors.As(err, &ne) {
		t.Errorf("deny all: unhealthy %v, err %v; want NoEligibleError", dec.Unhealthy, err)
	}
}

// modesCatalog has a requested model "big" and cheaper models of lower
// quality.
func modesCatalog(tb testing.TB) *catalog.Catalog {
	return buildCatalog(tb,
		[]pspec{{id: "p"}, {id: "q"}, {id: "r"}},
		[]mspec{
			{id: "big", quality: map[string]float64{"general": 0.95}, deps: []dspec{
				{provider: "p", in: "3", out: "15"},
				{provider: "q", in: "2", out: "10"},
			}},
			{id: "near", quality: map[string]float64{"general": 0.92}, deps: []dspec{{provider: "r", in: "1", out: "4"}}},
			{id: "far", quality: map[string]float64{"general": 0.70}, deps: []dspec{{provider: "r", in: "0.1", out: "0.4"}}},
		})
}

func TestModes(t *testing.T) {
	c := modesCatalog(t)
	r := New(c, newTracker(), config.Routing{})
	tests := []struct {
		name     string
		input    func() Input
		wantMode string
		wantPlan []string
		wantBar  float64
		wantErr  bool
	}{
		{
			name:     "pinned model",
			input:    func() Input { return concreteInput(t, c, "big") },
			wantMode: ModePinned, wantPlan: []string{"big@q", "big@p"},
		},
		{
			name: "pinned skips quality",
			input: func() Input {
				in := concreteInput(t, c, "far")
				in.Assessment.Difficulty = 1
				return in
			},
			wantMode: ModePinned, wantPlan: []string{"far@r"},
		},
		{
			name:     "model@provider",
			input:    func() Input { return concreteInput(t, c, "big@p") },
			wantMode: ModePinned, wantPlan: []string{"big@p"},
		},
		{
			name: "model@provider ignores equivalent",
			input: func() Input {
				in := concreteInput(t, c, "big@p")
				in.Policy.Substitution = policy.SubstitutionEquivalent
				return in
			},
			wantMode: ModePinned, wantPlan: []string{"big@p"},
		},
		{
			name: "model@provider still obeys deny",
			input: func() Input {
				in := concreteInput(t, c, "big@p")
				in.Policy.Deny = []string{"big@p"}
				return in
			},
			wantMode: ModePinned, wantErr: true,
		},
		{
			// d 0.5: q̂ big 0.975, near 0.96, far 0.85; bar 0.975 − 0.05.
			name: "equivalent",
			input: func() Input {
				in := concreteInput(t, c, "big")
				in.Policy.Substitution = policy.SubstitutionEquivalent
				return in
			},
			wantMode: ModeEquivalent, wantBar: 0.925, wantPlan: []string{"near@r", "big@q", "big@p"},
		},
		{
			name: "equivalent with a small drop",
			input: func() Input {
				in := concreteInput(t, c, "big")
				in.Policy.Substitution, in.Policy.MaxQualityDrop = policy.SubstitutionEquivalent, 0.01
				return in
			},
			wantMode: ModeEquivalent, wantBar: 0.965, wantPlan: []string{"big@q", "big@p"},
		},
		{
			name:     "auto",
			input:    autoInput,
			wantMode: ModeAuto, wantBar: 0.80, wantPlan: []string{"far@r", "big@q", "big@p"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec, err := r.Decide(tt.input())
			if dec.Explanation.Mode != tt.wantMode {
				t.Errorf("mode = %q, want %q", dec.Explanation.Mode, tt.wantMode)
			}
			if tt.wantErr {
				var ne *catalog.NoEligibleError
				if !errors.As(err, &ne) {
					t.Fatalf("err = %v, want NoEligibleError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if diff := cmp.Diff(tt.wantPlan, planIDs(dec.Plan)); diff != "" {
				t.Errorf("plan (-want +got):\n%s", diff)
			}
			if dec.Explanation.QualityBar != tt.wantBar {
				t.Errorf("bar = %v, want %v", dec.Explanation.QualityBar, tt.wantBar)
			}
		})
	}
}

// latencyCatalog has a cheap slow model, a pricier fast one and an
// expensive fastest one of lower quality.
func latencyCatalog(tb testing.TB) *catalog.Catalog {
	return buildCatalog(tb,
		[]pspec{{id: "p"}, {id: "q"}, {id: "r"}},
		[]mspec{
			{id: "slow", quality: map[string]float64{"general": 0.95}, deps: []dspec{{provider: "p", in: "0.1", out: "0.1", ttftMs: 2000}}},
			{id: "fast", quality: map[string]float64{"general": 0.90}, deps: []dspec{{provider: "q", in: "1", out: "1", ttftMs: 400}}},
			{id: "fastest", quality: map[string]float64{"general": 0.60}, deps: []dspec{{provider: "r", in: "2", out: "2", ttftMs: 100}}},
		})
}

func TestSoftChecks(t *testing.T) {
	r := New(latencyCatalog(t), newTracker(), config.Routing{})
	tests := []struct {
		name        string
		d           float64
		minQ        float64
		maxTTFT     time.Duration
		objective   string
		onUnmet     string
		wantPlan    []string
		wantRelaxed []string
		wantReasons map[string]string
		wantErr     string
	}{
		{
			// d 0.5: q̂ slow 0.975, fast 0.95, fastest 0.8.
			name: "both met", d: 0.5, minQ: 0.9, maxTTFT: 500 * time.Millisecond,
			wantPlan:    []string{"fast@q"},
			wantReasons: map[string]string{"slow@p": "est ttft 2000ms > 500ms", "fastest@r": "quality 0.8 < 0.9"},
		},
		{
			name: "latency relaxed", d: 0.5, minQ: 0.96, maxTTFT: 500 * time.Millisecond,
			wantPlan: []string{"slow@p"}, wantRelaxed: []string{RelaxedLatency},
			wantReasons: map[string]string{"fast@q": "quality 0.95 < 0.96", "fastest@r": "quality 0.8 < 0.96"},
		},
		{
			// Nothing reaches 0.99, so both are relaxed and the best q̂ wins.
			name: "latency then quality relaxed", d: 0.5, minQ: 0.99, maxTTFT: 500 * time.Millisecond,
			wantPlan: []string{"slow@p", "fast@q", "fastest@r"}, wantRelaxed: []string{RelaxedLatency, RelaxedQuality},
			wantReasons: map[string]string{},
		},
		{
			name: "quality relaxed without a latency target", d: 1, minQ: 0.99,
			wantPlan: []string{"slow@p", "fast@q", "fastest@r"}, wantRelaxed: []string{RelaxedQuality},
			wantReasons: map[string]string{},
		},
		{
			// Every candidate meets latency, so only quality is relaxed.
			name: "quality relaxed, latency met", d: 1, minQ: 0.99, maxTTFT: 5 * time.Second,
			wantPlan: []string{"slow@p", "fast@q", "fastest@r"}, wantRelaxed: []string{RelaxedQuality},
			wantReasons: map[string]string{},
		},
		{
			name: "reject on quality", d: 0.5, minQ: 0.99, onUnmet: policy.OnUnmetReject,
			wantReasons: map[string]string{"slow@p": "quality 0.975 < 0.99", "fast@q": "quality 0.95 < 0.99", "fastest@r": "quality 0.8 < 0.99"},
			wantErr:     "quality 0.975 < 0.99",
		},
		{
			name: "reject on latency", d: 0.5, minQ: 0.96, maxTTFT: 50 * time.Millisecond, onUnmet: policy.OnUnmetReject,
			wantReasons: map[string]string{"slow@p": "est ttft 2000ms > 50ms", "fast@q": "quality 0.95 < 0.96", "fastest@r": "quality 0.8 < 0.96"},
			wantErr:     "slow@p: est ttft 2000ms > 50ms",
		},
		{
			name: "latency objective", d: 0, minQ: 0.8, objective: policy.ObjectiveLatency,
			wantPlan: []string{"fastest@r", "fast@q", "slow@p"}, wantReasons: map[string]string{},
		},
		{
			name: "latency objective with a quality bar", d: 1, minQ: 0.8, objective: policy.ObjectiveLatency,
			wantPlan:    []string{"fast@q", "slow@p"},
			wantReasons: map[string]string{"fastest@r": "quality 0.6 < 0.8"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := autoInput()
			in.Assessment.Difficulty = tt.d
			in.Policy.Constraints = policy.Constraints{MinQuality: tt.minQ, MaxTTFT: tt.maxTTFT, Objective: tt.objective}
			if tt.onUnmet != "" {
				in.Policy.OnUnmet = tt.onUnmet
			}
			dec, err := r.Decide(in)
			if diff := cmp.Diff(tt.wantReasons, reasons(dec.Explanation)); diff != "" {
				t.Errorf("reasons (-want +got):\n%s", diff)
			}
			if tt.wantErr != "" {
				var ne *catalog.NoEligibleError
				if !errors.As(err, &ne) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want a NoEligibleError containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if diff := cmp.Diff(tt.wantPlan, planIDs(dec.Plan)); diff != "" {
				t.Errorf("plan (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantRelaxed, dec.Relaxed); diff != "" {
				t.Errorf("relaxed (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(append([]string{}, tt.wantRelaxed...), dec.Explanation.Relaxed); diff != "" {
				t.Errorf("explanation relaxed (-want +got):\n%s", diff)
			}
		})
	}
}

// TestLatencyFromHealth: measurements replace the latency hint after 20
// samples.
func TestLatencyFromHealth(t *testing.T) {
	c := latencyCatalog(t)
	tr := newTracker()
	r := New(c, tr, config.Routing{})
	d := tr.Deployment("slow@p", "p", health.LatencyHint{TTFT: 2 * time.Second})
	for range 20 {
		a, reason := d.Admit(false)
		if reason != "" {
			t.Fatal(reason)
		}
		d.Done(a, health.Result{TTFT: 50 * time.Millisecond})
	}
	in := autoInput()
	in.Assessment.Difficulty = 0
	in.Policy.Constraints.Objective = policy.ObjectiveLatency
	dec, err := r.Decide(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := dec.Plan[0].Deployment.ID; got != "slow@p" {
		t.Errorf("primary = %s, want slow@p (measured 50ms)", got)
	}
	if got := dec.Explanation.Candidates[0].EstTTFTMs; got != 50 {
		t.Errorf("est_ttft_ms = %d, want 50", got)
	}
}

func TestPreferenceTiers(t *testing.T) {
	c := buildCatalog(t,
		[]pspec{{id: "gpu"}, {id: "p"}, {id: "q"}},
		[]mspec{
			{id: "llama", quality: map[string]float64{"general": 0.9}, deps: []dspec{
				{provider: "gpu", in: "0.5", out: "0.5"},
				{provider: "p", in: "0.2", out: "0.2"},
			}},
			{id: "claude-x", quality: map[string]float64{"general": 0.9}, deps: []dspec{{provider: "q", in: "3", out: "3"}}},
			{id: "claude-y", quality: map[string]float64{"general": 0.9}, deps: []dspec{{provider: "p", in: "2", out: "2"}}},
			{id: "cheapest", quality: map[string]float64{"general": 0.9}, deps: []dspec{{provider: "q", in: "0.01", out: "0.01"}}},
		})
	r := New(c, newTracker(), config.Routing{Fallback: config.Fallback{MaxDeployments: 10}})
	tests := []struct {
		name   string
		prefer []string
		want   []string
		tiers  map[string]int
	}{
		{"none", nil, []string{"cheapest@q", "llama@p", "llama@gpu", "claude-y@p", "claude-x@q"}, nil},
		{"claude first", []string{"claude-*"}, []string{"claude-y@p", "claude-x@q", "cheapest@q", "llama@p", "llama@gpu"},
			map[string]int{"claude-x@q": 0, "llama@gpu": 1}},
		{"spillover", []string{"*@gpu", "claude-*"}, []string{"llama@gpu", "claude-y@p", "claude-x@q", "cheapest@q", "llama@p"},
			map[string]int{"llama@gpu": 0, "claude-x@q": 1, "cheapest@q": 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := autoInput()
			in.Policy.Prefer = tt.prefer
			dec, err := r.Decide(in)
			if err != nil {
				t.Fatal(err)
			}
			// Diversity reorders the plan, so compare the ranking.
			ranked := make([]string, len(tt.want))
			for _, c := range dec.Explanation.Candidates {
				ranked[c.Rank-1] = c.Deployment
				if want, ok := tt.tiers[c.Deployment]; ok && c.Tier != want {
					t.Errorf("%s tier = %d, want %d", c.Deployment, c.Tier, want)
				}
			}
			if diff := cmp.Diff(tt.want, ranked); diff != "" {
				t.Errorf("ranking (-want +got):\n%s", diff)
			}
			if dec.Plan[0].Deployment.ID != tt.want[0] {
				t.Errorf("primary = %s, want %s", dec.Plan[0].Deployment.ID, tt.want[0])
			}
		})
	}
}

// TestPlanDiversity: fallbacks prefer providers not yet in the plan, and
// the plan has at most max_deployments entries.
func TestPlanDiversity(t *testing.T) {
	c := buildCatalog(t,
		[]pspec{{id: "p"}, {id: "q"}},
		[]mspec{
			{id: "m1", quality: map[string]float64{"general": 0.9}, deps: []dspec{{provider: "p", in: "1", out: "1"}}},
			{id: "m2", quality: map[string]float64{"general": 0.9}, deps: []dspec{{provider: "p", in: "2", out: "2"}}},
			{id: "m3", quality: map[string]float64{"general": 0.9}, deps: []dspec{{provider: "q", in: "3", out: "3"}}},
			{id: "m4", quality: map[string]float64{"general": 0.9}, deps: []dspec{{provider: "p", in: "4", out: "4"}}},
		})
	tests := []struct {
		max  int
		want []string
	}{
		{0, []string{"m1@p", "m3@q", "m2@p"}},
		{1, []string{"m1@p"}},
		{2, []string{"m1@p", "m3@q"}},
		{10, []string{"m1@p", "m3@q", "m2@p", "m4@p"}},
		{50, []string{"m1@p", "m3@q", "m2@p", "m4@p"}},
	}
	for _, tt := range tests {
		r := New(c, nil, config.Routing{Fallback: config.Fallback{MaxDeployments: tt.max}})
		dec, err := r.Decide(autoInput())
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(tt.want, planIDs(dec.Plan)); diff != "" {
			t.Errorf("max %d: plan (-want +got):\n%s", tt.max, diff)
		}
		if diff := cmp.Diff(tt.want, dec.Explanation.Plan); diff != "" {
			t.Errorf("max %d: explanation plan (-want +got):\n%s", tt.max, diff)
		}
	}
}

// TestFallbacksAndNotes covers the quality, latency and pricing fallbacks.
func TestFallbacksAndNotes(t *testing.T) {
	c := buildCatalog(t,
		[]pspec{{id: "p"}, {id: "q"}},
		[]mspec{
			{id: "noprior", deps: []dspec{{provider: "p", in: "1", out: "1", ttftMs: 200}}},
			{id: "generalonly", quality: map[string]float64{"general": 0.8}, deps: []dspec{{provider: "p", in: "2", out: "2", offset: -0.1}}},
			{id: "unpriced", quality: map[string]float64{"coding": 0.9}, deps: []dspec{{provider: "q"}}},
		})
	r := New(c, nil, config.Routing{})
	in := autoInput()
	in.Assessment = quality.Assessment{Category: quality.CategoryCoding, Difficulty: 1}
	in.Policy.Constraints.MinQuality = 0
	dec, err := r.Decide(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []CandidateInfo{
		{Deployment: "noprior@p", Model: "noprior", Q: 0.5, EstCostUSD: "0.002", EstTTFTMs: 200, Eligible: true, Rank: 1, Notes: []string{NoteNoQualityPrior}},
		{Deployment: "generalonly@p", Model: "generalonly", Q: 0.7, EstCostUSD: "0.004", EstTTFTMs: 1000, Eligible: true, Rank: 2, Notes: []string{NoteNoLatency}},
		{Deployment: "unpriced@q", Model: "unpriced", Q: 0.9, EstTTFTMs: 1000, Eligible: true, Rank: 3, Notes: []string{NoteNoLatency, NotePricingUnknown}},
	}
	if diff := cmp.Diff(want, dec.Explanation.Candidates); diff != "" {
		t.Errorf("candidates (-want +got):\n%s", diff)
	}

	// A cost cap excludes the unpriced deployment: its cost is unknown.
	in.Policy.MaxCostPerRequestNano = 1_000_000_000
	dec, err = r.Decide(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := reasons(dec.Explanation)["unpriced@q"]; got != "cost unknown (pricing not set) under max_cost_per_request 1 USD" {
		t.Errorf("unpriced reason = %q", got)
	}
}

func TestDroppedParams(t *testing.T) {
	c := buildCatalog(t,
		[]pspec{{id: "p"}},
		[]mspec{{id: "m", caps: []string{"streaming"}, quality: map[string]float64{"general": 0.9}, deps: []dspec{{provider: "p", in: "1", out: "1"}}}})
	r := New(c, nil, config.Routing{})
	in := autoInput()
	in.Required = schema.CapabilitySet{schema.CapSeed: {}, schema.CapSampling: {}}
	if _, err := r.Decide(in); err == nil {
		t.Fatal("seed without drop: want NoEligibleError")
	}
	in.Drop = true
	dec, err := r.Decide(in)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"sampling", "seed"}, dec.Explanation.DroppedParams); diff != "" {
		t.Errorf("dropped (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]schema.Capability{schema.CapSampling, schema.CapSeed}, dec.Plan[0].Dropped); diff != "" {
		t.Errorf("plan dropped (-want +got):\n%s", diff)
	}
}

func TestExplanationFields(t *testing.T) {
	r := New(workedExample(t), nil, config.Routing{})
	in := autoInput()
	in.Policy.PolicyID, in.Policy.Profile = "acme", "cheap"
	in.Policy.Warnings = []string{"X-Switchyard-Min-Quality would lower the quality bar; ignored"}
	in.Downgrade = "budget acme/month soft threshold crossed; profile cheap"
	in.Assessment = quality.Assessment{Category: quality.CategoryCoding, Difficulty: 0.123456, Hinted: true}
	dec, err := r.Decide(in)
	if err != nil {
		t.Fatal(err)
	}
	e := dec.Explanation
	if e.Policy != "acme" || e.Profile != "cheap" || e.Downgrade != in.Downgrade || !e.Assessment.Hinted ||
		e.Assessment.Difficulty != 0.1235 || len(e.Warnings) != 1 || e.Assessment.Signals == nil {
		t.Errorf("explanation = %+v", e)
	}
	// The explanation does not alias the input.
	in.Policy.Warnings[0] = "changed"
	if e.Warnings[0] == "changed" {
		t.Error("warnings alias the policy's")
	}
}

// TestDeterminism: the same input gives byte-identical explanations, from
// one decision marshaled twice and from two decisions.
func TestDeterminism(t *testing.T) {
	c := exclusionCatalog(t)
	tr := newTracker()
	openBreaker(t, tr, "c@busy", "busy")
	r := New(c, tr, config.Routing{})
	in := autoInput()
	in.Policy.Prefer = []string{"b@*"}
	in.Policy.Warnings = []string{"w"}
	in.Assessment.Signals = []string{"code_block"}
	first, err := r.Decide(in)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first.Explanation)
	b, _ := json.Marshal(first.Explanation)
	if !bytes.Equal(a, b) {
		t.Fatal("marshaling twice differs")
	}
	for range 20 {
		again, err := r.Decide(in)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := json.Marshal(again.Explanation)
		if !bytes.Equal(a, c) {
			t.Fatalf("decisions differ:\n%s\n%s", a, c)
		}
	}
}

func TestDecideErrors(t *testing.T) {
	r := New(workedExample(t), nil, config.Routing{})
	in := autoInput()
	in.Auto = false
	if _, err := r.Decide(in); err == nil {
		t.Error("concrete request without a resolution: want an error")
	}
	other := buildCatalog(t, []pspec{{id: "z"}}, []mspec{{id: "zz", deps: []dspec{{provider: "z", in: "1", out: "1"}}}})
	res, err := other.Resolve("zz")
	if err != nil {
		t.Fatal(err)
	}
	in.Resolution = res
	if _, err := r.Decide(in); err == nil || !strings.Contains(err.Error(), "zz@z") {
		t.Errorf("foreign resolution: err = %v", err)
	}
}

func TestFemto(t *testing.T) {
	tests := []struct {
		f    femto
		want string
	}{
		{femto{}, "0"},
		{mulFemto(1, 1), "0.000000000000001"},
		{mulFemto(350, 1_000_000_000), "0.00035"},
		{mulFemto(12, 1_000_000_000_000_000), "12"},
		{mulFemto(1<<63, 4), "36893.488147419103232"},
		{maxFemto, "340282366920938463463374.607431768211455"},
	}
	for _, tt := range tests {
		if got := tt.f.String(); got != tt.want {
			t.Errorf("String(%v) = %s, want %s", tt.f, got, tt.want)
		}
	}
	if got := maxFemto.add(mulFemto(1, 1)); got != maxFemto {
		t.Errorf("saturating add = %v", got)
	}
	if mulFemto(1, 2).cmp(femto{hi: 1}) >= 0 || (femto{hi: 1}).cmp(mulFemto(1, 2)) <= 0 || mulFemto(3, 3).cmp(mulFemto(9, 1)) != 0 {
		t.Error("cmp")
	}
	for s, want := range map[string]uint64{"0": 0, "3": 3_000_000_000, ".5": 500_000_000, "0.000000001": 1, "99999999999999999999": 1<<64 - 1} {
		if got, ok := nanoPerMillion(price(t, s)); !ok || got != want {
			t.Errorf("nanoPerMillion(%s) = %d, %v; want %d", s, got, ok, want)
		}
	}
	if _, ok := nanoPerMillion(config.Price{}); ok {
		t.Error("unset price is known")
	}
}

// TestDecideContextOut checks that the context filter reserves room for
// ContextOut (max_tokens) while costs use EstOut, and that PolicyReason
// applies deny, allow and require_tags to any deployment.
func TestDecideContextOut(t *testing.T) {
	cat := buildCatalog(t, []pspec{{id: "p"}}, []mspec{
		{id: "small", ctx: 4000, quality: map[string]float64{"general": 0.9}, deps: []dspec{{provider: "p", in: "1", out: "1"}}},
		{id: "big", ctx: 100000, quality: map[string]float64{"general": 0.9}, deps: []dspec{{provider: "p", in: "2", out: "2"}}},
	})
	r := New(cat, nil, config.Routing{})
	in := autoInput()
	in.InputBytes, in.EstOut, in.ContextOut = 4000, 500, 8000
	dec, err := r.Decide(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := planIDs(dec.Plan); len(got) != 1 || got[0] != "big@p" {
		t.Fatalf("plan %v, want [big@p]", got)
	}
	if got := reasons(dec.Explanation)["small@p"]; got != "context 4000 < 9000 needed" {
		t.Errorf("small@p excluded by %q", got)
	}
	// 1000 input + 500 output tokens at 2 USD per million.
	if got := dec.Explanation.Candidates[1].EstCostUSD; got != "0.003" {
		t.Errorf("est cost of big@p = %s, want 0.003 (EstOut, not ContextOut)", got)
	}
	if got := dec.Explanation.Estimates; got.OutputTokens != 500 {
		t.Errorf("estimates %+v, want output 500", got)
	}

	d := cat.Models()[0].Deployments[0]
	pol := basePolicy()
	pol.Deny = []string{"small@*"}
	if got := PolicyReason(&pol, d); got != `denied by policy (deny "small@*")` {
		t.Errorf("PolicyReason(deny) = %q", got)
	}
	pol = basePolicy()
	pol.RequireTags = []string{"zdr"}
	if got := PolicyReason(&pol, d); got != "lacks required tag zdr" {
		t.Errorf("PolicyReason(require_tags) = %q", got)
	}
}
