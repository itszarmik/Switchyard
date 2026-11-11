package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"switchyard/internal/config"
	"switchyard/internal/router"
)

// Regression tests for the Phase 5 review findings, on the tier gateway of
// decide_test.go.

// explainOf decodes an explain endpoint answer.
func explainOf(t *testing.T, r exchangeResult) router.Explanation {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("explain: status %d %+v", r.status, r.err)
	}
	var exp router.Explanation
	if err := json.Unmarshal([]byte(r.body), &exp); err != nil {
		t.Fatalf("explain: %v\n%s", err, r.body)
	}
	return exp
}

// TestQualityRuleDoesNotShadowGovernance: a rule matched through the
// request's content or the client's hints (a quality rule) no longer
// stops a later governance rule, here a deny, from applying.
func TestQualityRuleDoesNotShadowGovernance(t *testing.T) {
	e := newTierEnv(t, func(c *config.Config) {
		c.Policies[0].Rules = []config.Rule{
			{Name: "coding-quality", Match: config.RuleMatch{Task: "coding"}, Set: config.PolicyOverride{MinQuality: 0.85}},
			{Name: "prod-no-large", Match: config.RuleMatch{KeyTags: []string{"prod"}}, Set: config.PolicyOverride{Deny: []string{"large@*"}}},
		}
	}, nil)
	for _, hdr := range [][]string{nil, {HeaderTask, "coding"}} {
		r := e.do(EndpointChatCompletions, chatFor("large"), hdr...)
		if r.status != http.StatusBadRequest || r.err.Code != codeNoEligible || !strings.Contains(r.err.Message, "denied by policy") {
			t.Errorf("headers %v: large served to a key whose rule denies it: status %d %+v", hdr, r.status, r.err)
		}
	}
	if e.calls() != [3]int{} {
		t.Errorf("calls %v", e.calls())
	}
	exp := explainOf(t, e.do(EndpointExplain, hiBody, HeaderTask, "coding", HeaderDifficulty, "0.6"))
	if exp.MatchedRule != "prod-no-large" || exp.MatchedQualityRule != "coding-quality" || exp.QualityBar != 0.85 {
		t.Errorf("explanation rules %q/%q bar %v", exp.MatchedRule, exp.MatchedQualityRule, exp.QualityBar)
	}
	if got := excludedBy(exp)["large@pl"]; got != `denied by policy (deny "large@*")` {
		t.Errorf("large@pl excluded by %q", got)
	}
}

// excludedBy maps each ineligible candidate of exp to its reason.
func excludedBy(exp router.Explanation) map[string]string {
	out := map[string]string{}
	for _, x := range exclusions(exp) {
		out[x.Deployment] = x.Reason
	}
	return out
}

// TestTokenRatioInAuto: auto routing prices, caps and admits each
// candidate at its own model's token_ratio.
func TestTokenRatioInAuto(t *testing.T) {
	body := func(m string) string {
		return `{"model":"` + m + `","messages":[{"role":"user","content":"` + strings.Repeat("a", 40000) + `"}]}`
	}
	// small at ratio 1: 10015 input tokens × 0.10/M + 500 × 0.40/M ≈
	// 0.0012 USD; at its ratio 10, ≈ 0.0102 USD.
	capped := newTierEnv(t, func(c *config.Config) {
		c.Models[0].TokenRatio = 10
		c.Policies[0].MaxCostPerRequestUSD = mustPriceS("0.002")
		c.Policies[0].Allow = []string{"small@*"}
	}, nil)
	for _, m := range []string{"auto", "small"} {
		r := capped.do(EndpointChatCompletions, body(m), HeaderDifficulty, "low")
		if r.status != http.StatusBadRequest || r.err.Code != codeNoEligible || !strings.Contains(r.err.Message, "> max_cost_per_request 0.002 USD") {
			t.Errorf("%s: status %d model %q err %+v, want small excluded by the cap", m, r.status, r.info.Model, r.err)
		}
	}
	if capped.calls() != [3]int{} {
		t.Errorf("calls %v", capped.calls())
	}

	// The budget estimate of an auto request is at the served model's
	// ratio: an upstream 5xx settles the admitted estimate.
	e := newTierEnv(t, func(c *config.Config) {
		c.Models[0].TokenRatio = 10
		c.Policies[0].Allow = []string{"small@*"}
		c.Tenants[0].Budgets = []config.Budget{{Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: mustPriceS("100"), OnExhausted: config.BudgetBlock}}
	}, nil)
	e.s.script(step{err: err503})
	b := body("auto")
	if r := e.do(EndpointChatCompletions, b, HeaderDifficulty, "low"); r.status < 500 {
		t.Fatalf("status %d", r.status)
	}
	// femto-USD: input tokens × 1e8 (0.10 USD/M) + 500 × 4e8 (0.40 USD/M).
	in := int64(float64(len(b)) / 4 * 10)
	femto := new(big.Int).Add(big.NewInt(in*100_000_000), big.NewInt(500*400_000_000))
	want := new(big.Int).Div(new(big.Int).Add(femto, big.NewInt(999_999_999)), big.NewInt(1_000_000_000)).Int64()
	if spent, err := e.store.Spent(context.Background(), e.g.scopes[principal{"t1", "k1"}][0], e.now); err != nil || spent != want {
		t.Errorf("settled %d micro-USD (%v), want the estimate at small's ratio, %d", spent, err, want)
	}
}

// TestEmbeddingsCostCap: the per-request cost cap, from the policy or
// tightened by the header, excludes embedding deployments over it.
func TestEmbeddingsCostCap(t *testing.T) {
	// About 1006 tokens: 0.0000201 USD on emb@pe, 0.00001 on emb@pl.
	body := `{"model":"emb","input":"` + strings.Repeat("x", 4000) + `"}`
	e := newTierEnv(t, func(c *config.Config) { c.Policies[0].MaxCostPerRequestUSD = mustPriceS("0.000001") }, nil)
	for _, hdr := range [][]string{nil, {"X-Switchyard-Max-Cost-Usd", "0.000001"}} {
		r := e.do(EndpointEmbeddings, body, hdr...)
		if r.status != http.StatusBadRequest || r.err.Code != codeNoEligible || !strings.Contains(r.err.Message, "> max_cost_per_request 0.000001 USD") {
			t.Errorf("headers %v: status %d err %+v", hdr, r.status, r.err)
		}
	}
	if e.e.count()+e.l.count() != 0 {
		t.Errorf("an upstream was called: pe %d pl %d", e.e.count(), e.l.count())
	}

	e = newTierEnv(t, func(c *config.Config) { c.Policies[0].Deny = []string{"emb@pl"} }, nil)
	r := e.do(EndpointEmbeddings, body, "X-Switchyard-Max-Cost-Usd", "0.000015")
	if r.status != http.StatusBadRequest || !strings.Contains(r.err.Message, "emb@pe (est cost 0.0000201") ||
		!strings.Contains(r.err.Message, " USD > max_cost_per_request 0.000015 USD)") {
		t.Errorf("header cap: status %d err %+v", r.status, r.err)
	}
	if r := e.do(EndpointEmbeddings, body, "X-Switchyard-Max-Cost-Usd", "0.00003"); r.status != http.StatusOK || r.info.Provider != "pe" {
		t.Errorf("under the cap: status %d info %+v", r.status, r.info)
	}
}

// TestMinQualityHeaderClamp: X-Switchyard-Min-Quality may ask for no more
// than the best profile the caller may select, or during a budget
// downgrade the soft profile; above that it is ignored with a warning.
func TestMinQualityHeaderClamp(t *testing.T) {
	// At d = 0.6: q̂ small 0.76, medium 0.85, large 0.97.
	t.Run("allowed profiles", func(t *testing.T) {
		e := newTierEnv(t, func(c *config.Config) {
			c.Policies[0].Profile = "cheap"
			c.Policies[0].AllowedProfiles = []string{"cheap", "balanced"}
		}, nil)
		tests := []struct {
			minQuality, model, warning string
		}{
			{"0.8", "medium", ""},
			{"1", "small", "X-Switchyard-Min-Quality ignored: it is above the quality bar of every profile this policy allows"},
			{"0.9", "small", "X-Switchyard-Min-Quality ignored: it is above the quality bar of every profile this policy allows"},
		}
		for _, tt := range tests {
			r := e.do(EndpointChatCompletions, hiBody, HeaderDifficulty, "0.6", "X-Switchyard-Min-Quality", tt.minQuality)
			if r.status != http.StatusOK || r.info.Model != tt.model || r.info.Profile != "cheap" || r.info.PolicyWarning != tt.warning {
				t.Errorf("min quality %s: status %d info %+v, want %s with warning %q", tt.minQuality, r.status, r.info, tt.model, tt.warning)
			}
		}
		// The profile header is refused too.
		if r := e.do(EndpointChatCompletions, hiBody, HeaderDifficulty, "0.6", "X-Switchyard-Profile", "quality"); r.info.Model != "small" || r.info.PolicyWarning == "" {
			t.Errorf("profile header: info %+v", r.info)
		}
	})
	t.Run("budget downgrade", func(t *testing.T) {
		e := newTierEnv(t, func(c *config.Config) {
			c.Tenants[0].Budgets = []config.Budget{{
				Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: mustPriceS("1"),
				SoftAt: 0.5, SoftProfile: "cheap", OnExhausted: config.BudgetBlock,
			}}
		}, nil)
		if _, err := e.store.Admit(t.Context(), e.g.scopes[principal{"t1", "k1"}][:1], 600_000, e.now); err != nil {
			t.Fatal(err)
		}
		r := e.do(EndpointChatCompletions, hiBody, HeaderDifficulty, "0.6", "X-Switchyard-Min-Quality", "0.9")
		if r.info.Model != "small" || r.info.Profile != "cheap" || r.info.RouteReason != reasonBudgetDowngrade ||
			r.info.PolicyWarning != "X-Switchyard-Min-Quality ignored: it is above the quality bar of the budget's soft profile" {
			t.Errorf("downgraded with min quality 0.9: info %+v", r.info)
		}
	})
}

// TestRuleProfileBeatsClientProfile: a matched governance rule's profile
// replaces the one the client selects, with a warning; a profile the
// policy does not allow in the model field is still refused first.
func TestRuleProfileBeatsClientProfile(t *testing.T) {
	staging := func(c *config.Config) {
		c.Policies[0].Rules = []config.Rule{{Name: "staging-is-cheap",
			Match: config.RuleMatch{Header: map[string]string{"x-env": "staging"}}, Set: config.PolicyOverride{Profile: "cheap"}}}
	}
	e := newTierEnv(t, staging, nil)
	const warning = "the requested profile is replaced by the profile of policy rule staging-is-cheap"
	for _, tt := range []struct {
		model  string
		header []string
	}{
		{"auto:quality", []string{"X-Env", "staging"}},
		{"careful", []string{"X-Env", "staging"}}, // the alias of auto:quality
		{"auto", []string{"X-Env", "staging", "X-Switchyard-Profile", "quality"}},
	} {
		r := e.do(EndpointChatCompletions, chatFor(tt.model), append(tt.header, HeaderDifficulty, "0.6")...)
		if r.status != http.StatusOK || r.info.Profile != "cheap" || r.info.Model != "small" || r.info.PolicyWarning != warning {
			t.Errorf("%s %v: status %d info %+v", tt.model, tt.header, r.status, r.info)
		}
	}
	e = newTierEnv(t, func(c *config.Config) { staging(c); c.Policies[0].AllowedProfiles = []string{"cheap", "balanced"} }, nil)
	if r := e.do(EndpointChatCompletions, chatFor("auto:quality"), "X-Env", "staging"); r.status != http.StatusBadRequest || r.err.Code != codeModelNotSupport {
		t.Errorf("disallowed auto:quality: status %d %+v", r.status, r.err)
	}
}

// TestSaturationCountsOnlyAcquire: routing decisions and explanations read
// a saturated provider without counting it in
// switchyard_provider_saturated_total; only a refused Acquire counts.
func TestSaturationCountsOnlyAcquire(t *testing.T) {
	e := newTierEnv(t, func(c *config.Config) { c.Providers[0].MaxConcurrency = 1 }, nil)
	p := e.g.provs["ps"]
	if !p.Acquire() {
		t.Fatal("acquire")
	}
	saturated := func() float64 { return testutil.ToFloat64(e.g.metrics.saturated.WithLabelValues("ps")) }
	for range 20 {
		exp := explainOf(t, e.do(EndpointExplain, hiBody, HeaderDifficulty, "low"))
		if got := excludedBy(exp)["small@ps"]; got != "provider at max_concurrency 1" {
			t.Fatalf("small@ps excluded by %q", got)
		}
	}
	if r := e.do(EndpointChatCompletions, hiBody, HeaderDifficulty, "low"); r.status != http.StatusOK || r.info.Model == "small" {
		t.Errorf("chat: status %d info %+v", r.status, r.info)
	}
	if got := saturated(); got != 0 {
		t.Errorf("provider_saturated_total{ps} = %v after routing around it, want 0", got)
	}
	if p.Acquire() {
		t.Fatal("a second slot")
	}
	if got := saturated(); got != 1 {
		t.Errorf("provider_saturated_total{ps} = %v after a refused Acquire, want 1", got)
	}
}

// TestEmptyAutoProfile: "auto:" names no profile and is refused, rather
// than routed as plain auto.
func TestEmptyAutoProfile(t *testing.T) {
	e := newTierEnv(t, func(c *config.Config) { c.Policies[0].AllowedProfiles = []string{"cheap"} }, nil)
	for _, ep := range []string{EndpointChatCompletions, EndpointExplain} {
		r := e.do(ep, chatFor("auto:"))
		if r.status != http.StatusBadRequest || r.err.Code != codeModelNotSupport ||
			!strings.Contains(r.err.Message, "the auto: prefix needs a profile name; allowed profiles: cheap") {
			t.Errorf("%s: status %d err %+v", ep, r.status, r.err)
		}
	}
	if r := e.do(EndpointCompletions, `{"model":"auto:","prompt":"hi"}`); r.status != http.StatusBadRequest || r.err.Code != codeModelNotSupport {
		t.Errorf("completions: status %d err %+v", r.status, r.err)
	}
	if e.calls() != [3]int{} {
		t.Errorf("calls %v", e.calls())
	}
}

// TestEquivalentQualityFloor: in equivalent mode a header's or a rule's
// min_quality still holds substitutes to it.
func TestEquivalentQualityFloor(t *testing.T) {
	equivalent := func(c *config.Config) {
		c.Policies[0].Substitution = config.Substitution{Mode: config.SubstitutionEquivalent, MaxQualityDrop: 0.05}
	}
	// At d = 0.6: q̂ small 0.76, medium 0.85, large 0.97; medium − 0.05 = 0.80.
	e := newTierEnv(t, equivalent, nil)
	if r := e.do(EndpointChatCompletions, chatFor("medium"), HeaderDifficulty, "0.6"); r.info.Model != "medium" {
		t.Errorf("no floor: served %s", r.info.Model)
	}
	r := e.do(EndpointChatCompletions, chatFor("medium"), HeaderDifficulty, "0.6", "X-Switchyard-Min-Quality", "0.9")
	if r.status != http.StatusOK || r.info.Model != "large" || r.info.PolicyWarning != "" {
		t.Errorf("header min quality 0.9: status %d info %+v, want large", r.status, r.info)
	}
	// Above every selectable profile's bar, the header is ignored.
	r = e.do(EndpointChatCompletions, chatFor("medium"), HeaderDifficulty, "0.6", "X-Switchyard-Min-Quality", "0.98")
	if r.info.Model != "medium" || r.info.PolicyWarning == "" {
		t.Errorf("header min quality 0.98: info %+v, want medium with a warning", r.info)
	}
	// A rule is trusted: its 0.98 applies.
	e = newTierEnv(t, func(c *config.Config) {
		equivalent(c)
		c.Policies[0].Rules = []config.Rule{{Name: "strict", Set: config.PolicyOverride{MinQuality: 0.95}}}
	}, nil)
	r = e.do(EndpointChatCompletions, chatFor("medium"), HeaderDifficulty, "0.6")
	if r.status != http.StatusOK || r.info.Model != "large" {
		t.Errorf("rule min quality 0.95: status %d info %+v, want large", r.status, r.info)
	}
	if exp := explainOf(t, e.do(EndpointExplain, chatFor("medium"), HeaderDifficulty, "0.6")); exp.Mode != router.ModeEquivalent || exp.QualityBar != 0.95 {
		t.Errorf("explanation mode %s bar %v", exp.Mode, exp.QualityBar)
	}
}

// TestImageBytesDoNotSteerRouting: an inline image's bytes count neither
// as prompt text for the assessor nor, beyond the image estimate, for the
// input, so they do not change where a request goes.
func TestImageBytesDoNotSteerRouting(t *testing.T) {
	e := newTierEnv(t, func(c *config.Config) {
		for i := range c.Models {
			c.Models[i].Capabilities = append(c.Models[i].Capabilities, "vision")
		}
	}, nil)
	const q = "What color is the car?"
	img := func(url string) string {
		return `{"model":"auto","messages":[{"role":"user","content":[{"type":"text","text":"` + q + `"},{"type":"image_url","image_url":{"url":"` + url + `"}}]}]}`
	}
	byURL := explainOf(t, e.do(EndpointExplain, img("https://example.com/car.png")))
	inline := explainOf(t, e.do(EndpointExplain, img("data:image/png;base64,"+strings.Repeat("A", 200_000))))
	text := explainOf(t, e.do(EndpointExplain, `{"model":"auto","messages":[{"role":"user","content":"`+q+`"}]}`))
	if inline.Assessment.Difficulty != byURL.Assessment.Difficulty || text.Assessment.Difficulty != byURL.Assessment.Difficulty ||
		strings.Join(inline.Assessment.Signals, ",") != strings.Join(byURL.Assessment.Signals, ",") ||
		strings.Join(inline.Plan, ",") != strings.Join(byURL.Plan, ",") {
		t.Errorf("by URL d=%v %v plan %v; inline d=%v %v plan %v; text d=%v",
			byURL.Assessment.Difficulty, byURL.Assessment.Signals, byURL.Plan,
			inline.Assessment.Difficulty, inline.Assessment.Signals, inline.Plan, text.Assessment.Difficulty)
	}
}

// TestExplainParityBudget: with blocking budgets, the offline explanation
// (switchyard route explain) treats each budget as untouched and matches
// a fresh gateway's.
func TestExplainParityBudget(t *testing.T) {
	budgets := func(c *config.Config) {
		c.Tenants[0].Budgets = []config.Budget{
			{Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: mustPriceS("0.002"), OnExhausted: config.BudgetBlock},
			{Scope: config.BudgetScopeKey, Window: config.BudgetWindowMonth, LimitUSD: mustPriceS("0.001"), OnExhausted: config.BudgetBlock,
				SoftAt: 0.5, SoftProfile: "cheap"},
		}
	}
	on := newTierEnv(t, budgets, nil)
	off := newTierEnv(t, budgets, func(o *Options) { o.Offline = true })
	body := `{"model":"auto","messages":[{"role":"user","content":"Prove that sqrt 2 is irrational step by step"}]}`
	r := on.do(EndpointExplain, body, HeaderDifficulty, "high")
	got, gerr := off.g.ExplainRequest(context.Background(), "t1", "k1", http.Header{HeaderDifficulty: {"high"}}, []byte(body))
	if r.status != http.StatusOK || gerr != nil || !bytes.Equal(got, []byte(r.body)) {
		t.Errorf("offline (%v) and endpoint (%d) differ:\n%s\nendpoint:\n%s", gerr, r.status, got, r.body)
	}
	if !strings.Contains(r.body, "remaining budget 0.001 USD") {
		t.Errorf("the remaining budget does not cap the decision:\n%s", r.body)
	}
}

// TestExplainParityRandom compares the explain endpoint of a fresh gateway
// with an offline one over 300 random requests, for each of three
// configurations.
func TestExplainParityRandom(t *testing.T) {
	edits := []struct {
		name string
		edit func(*config.Config)
	}{
		{"plain", nil},
		{"rules", func(c *config.Config) {
			c.Policies[0].Prefer = []string{"medium@*"}
			c.Policies[0].Substitution = config.Substitution{Mode: config.SubstitutionEquivalent, MaxQualityDrop: 0.05}
			c.Policies[0].Rules = []config.Rule{{Name: "code", Match: config.RuleMatch{Task: "coding"}, Set: config.PolicyOverride{MinQuality: 0.9}}}
			c.Providers[1].MaxConcurrency = 1
		}},
		{"budget", func(c *config.Config) {
			c.Tenants[0].Budgets = []config.Budget{{Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: mustPriceS("0.002"), OnExhausted: config.BudgetBlock}}
		}},
	}
	models := []string{"auto", "auto:cheap", "auto:fast", "small", "medium", "large", "medium@pm", "careful", "pin"}
	contents := []string{"hi", "Fix this: ```go\\nfunc x() {}\\n``` panic: boom", "Prove that sqrt 2 is irrational step by step"}
	rng := rand.New(rand.NewPCG(7, 7))
	for _, ed := range edits {
		t.Run(ed.name, func(t *testing.T) {
			on := newTierEnv(t, ed.edit, nil)
			off := newTierEnv(t, ed.edit, func(o *Options) { o.Offline = true })
			for i := range 300 {
				mt := ""
				if rng.IntN(2) == 0 {
					mt = `"max_tokens":` + strconv.Itoa(1+rng.IntN(5000)) + `,`
				}
				body := `{"model":"` + models[rng.IntN(len(models))] + `",` + mt + `"messages":[{"role":"user","content":"` + contents[rng.IntN(len(contents))] + `"}]}`
				var hdr []string
				if rng.IntN(2) == 0 {
					hdr = append(hdr, HeaderDifficulty, strconv.FormatFloat(rng.Float64(), 'f', 3, 64))
				}
				if rng.IntN(3) == 0 {
					hdr = append(hdr, "X-Switchyard-Max-TTFT-Ms", strconv.Itoa(100+rng.IntN(1000)))
				}
				if rng.IntN(3) == 0 {
					hdr = append(hdr, "X-Switchyard-Max-Cost-Usd", "0.00"+strconv.Itoa(1+rng.IntN(9)))
				}
				r := on.do(EndpointExplain, body, hdr...)
				h := http.Header{}
				for k := 0; k+1 < len(hdr); k += 2 {
					h.Add(hdr[k], hdr[k+1])
				}
				got, gerr := off.g.ExplainRequest(context.Background(), "t1", "k1", h, []byte(body))
				if r.status != http.StatusOK {
					if gerr == nil || gerr.Code != r.err.Code || gerr.Message != r.err.Message {
						t.Fatalf("%d: %s %v: endpoint %d %+v, offline %+v", i, body, hdr, r.status, r.err, gerr)
					}
					continue
				}
				if gerr != nil || !bytes.Equal(got, []byte(r.body)) {
					t.Fatalf("%d: %s %v differ (%v)\noffline:\n%s\nendpoint:\n%s", i, body, hdr, gerr, got, r.body)
				}
			}
		})
	}
}
