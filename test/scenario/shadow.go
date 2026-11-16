package scenario

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"switchyard/test/eval/fakejudge"
)

// The Phase 8 acceptance scenarios (docs/PLAN.md, P8) for shadow
// sampling: its budget is never exceeded and its results join to the
// ledger's rows, and observe mode's projected savings become verified in
// the categories where the judge accepts the cheaper answers.
//
// One scripted fake (test/eval/fakejudge) plays every model: the cheap
// model "small" (upstream small-up), the baseline "large" (large-up) and
// the judge (judge-up). Answers name their model; the judge's verdict
// comes from the scenario's script.

// shadowModels is the providers, models, policy and evaluation sections.
// policy is the default policy's extra fields, and evaluation the
// evaluation.shadow fields.
func shadowModels(baseURL, policy, shadowCfg string) string {
	return fmt.Sprintf(`providers:
  - {id: fj, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}"}
models:
  - id: small
    capabilities: [sampling]
    quality: {general: 0.9, coding: 0.9}
    deployments:
      - {provider: fj, upstream_model: small-up, pricing: {input: "0.10", output: "0.40"}}
  - id: large
    capabilities: [sampling]
    quality: {general: 0.97, coding: 0.97}
    deployments:
      - {provider: fj, upstream_model: large-up, pricing: {input: "1", output: "2"}}
  - id: judge
    capabilities: [sampling]
    quality: {general: 0.98}
    deployments:
      - {provider: fj, upstream_model: judge-up, pricing: {input: "1", output: "2"}}
policies:
  - id: default
    baseline_model: large
%s
evaluation:
  judge: {model: judge}
  shadow:
%s
`, baseURL, policy, shadowCfg)
}

// answer is the fakes' answer to a non-judge request: it names the model.
func answer(r fakejudge.Request) string { return "Answer by " + r.Model + "." }

// shadowOutcomes are the switchyard_shadow_samples_total outcomes.
var shadowOutcomes = []string{"judged", "dropped_budget", "dropped_queue", "dropped_policy", "error"}

// ShadowOutcomes returns the gateway's switchyard_shadow_samples_total by
// outcome.
func (g *Gateway) ShadowOutcomes(t testing.TB) map[string]float64 {
	t.Helper()
	_, lines := g.Metrics(t, "switchyard_shadow_samples_total")
	out := map[string]float64{}
	for _, line := range lines {
		for _, o := range shadowOutcomes {
			if strings.Contains(line, `outcome="`+o+`"`) {
				f := strings.Fields(line)
				v, err := strconv.ParseFloat(f[len(f)-1], 64)
				if err != nil {
					t.Fatalf("metric line %q: %v", line, err)
				}
				out[o] = v
			}
		}
	}
	return out
}

// waitShadow waits until n samples have an outcome, and returns the
// outcomes.
func (g *Gateway) waitShadow(t testing.TB, n float64) map[string]float64 {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		out := g.ShadowOutcomes(t)
		sum := 0.0
		for _, v := range out {
			sum += v
		}
		if sum >= n || time.Now().After(deadline) {
			if sum != n {
				t.Fatalf("shadow outcomes %v sum to %v, want %v", out, sum, n)
			}
			return out
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// QualityReport is the part of a usage report the shadow scenarios check.
type QualityReport struct {
	Verification struct {
		MinSamples  int     `json:"min_samples"`
		MinPassRate float64 `json:"min_pass_rate"`
	} `json:"verification"`
	Totals QualityTotals                `json:"totals"`
	Groups []map[string]json.RawMessage `json:"groups"`
}

// QualityTotals are a report's or a group's shadow fields.
type QualityTotals struct {
	Requests            int64  `json:"requests"`
	OverheadUSD         string `json:"overhead_usd"`
	ProjectedSavingsUSD string `json:"projected_savings_usd"`
	VerifiedSavingsUSD  string `json:"verified_savings_usd"`
	ShadowCostUSD       string `json:"shadow_cost_usd"`
	Feedback            struct {
		Negative int64 `json:"negative"`
		Neutral  int64 `json:"neutral"`
		Positive int64 `json:"positive"`
	} `json:"feedback"`
	Quality []struct {
		Category    string  `json:"category"`
		Judged      int64   `json:"judged"`
		Acceptable  int64   `json:"acceptable"`
		Errors      int64   `json:"errors"`
		PassRate    float64 `json:"pass_rate"`
		WilsonLower float64 `json:"wilson_lower"`
		Verified    bool    `json:"verified"`
	} `json:"quality"`
}

// qualityReport reads the admin usage report of the scenario's tenant.
func (g *Gateway) qualityReport(t testing.TB, query string) QualityReport {
	t.Helper()
	resp, err := g.get(g.AdminURL + "/admin/v1/usage?tenant=" + g.Tenant + "&" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var r QualityReport
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &r) != nil {
		t.Fatalf("admin usage report: %d %s", resp.StatusCode, b)
	}
	return r
}

// timed is one client request and how long it took.
type timed struct {
	status  int
	latency time.Duration
	id      string
}

// fire sends n concurrent chat requests and returns their outcomes.
func (g *Gateway) fire(t testing.TB, n int, body func(i int) string) []timed {
	t.Helper()
	out := make([]timed, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			start := time.Now()
			r, err := g.post("/v1/chat/completions", body(i))
			if err != nil {
				errs <- err
				return
			}
			out[i] = timed{status: r.Status, latency: time.Since(start), id: r.Header.Get("X-Request-Id")}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	return out
}

// percentile returns the p-quantile (0 < p ≤ 1) of the latencies.
func percentile(rs []timed, p float64) time.Duration {
	l := make([]time.Duration, len(rs))
	for i, r := range rs {
		l[i] = r.latency
	}
	slices.Sort(l)
	i := int(float64(len(l))*p+0.5) - 1
	return l[min(max(i, 0), len(l)-1)]
}

// ShadowResult is what ShadowBudget saw.
type ShadowResult struct {
	// Gateway is the gateway with shadow sampling on.
	Gateway *Gateway
	// RequestIDs are the X-Request-Id of its requests.
	RequestIDs []string
	// Outcomes are its switchyard_shadow_samples_total by outcome.
	Outcomes map[string]float64
}

// shadowBudgetUSD is ShadowBudget's daily shadow budget: room for about 40
// samples at their actual cost of about 0.0012 USD each, which the
// call-by-call reservation (a comparison call's worst case, then each
// judge call's) lets most of n = 60 samples use, while the rest are
// dropped for the budget.
const shadowBudgetUSD = "0.05"

// ShadowBudget runs n concurrent auto requests with shadow sampling at
// rate 1.0 and a daily shadow budget with room for a few samples only, and
// the same requests against a gateway with sampling off, and checks:
//
//   - every client response is 200 with and without sampling, and the
//     latency stays in the same band (p50 and p95 within a generous
//     margin of the gateway without sampling);
//   - every sample is accounted for, and some were dropped for the budget;
//   - the shadow spend, in the usage report (shadow_cost_usd) and the
//     switchyard_shadow_cost_usd_total metric, is at most the budget, and
//     counts as overhead;
//   - every stored shadow evaluation joins to a usage event: the report,
//     which joins them from the events, counts as many as were stored.
//
// It returns the sampling gateway and its request IDs, so the caller can
// check the database too.
func ShadowBudget(t *testing.T, o Options, n int) ShadowResult {
	t.Helper()
	fake := fakejudge.New(func(r fakejudge.Request) fakejudge.Reply {
		if r.Judge {
			return fakejudge.Reply{Content: fakejudge.Verdict("tie")}
		}
		d := time.Duration(0)
		if r.Model == "small-up" {
			d = 20 * time.Millisecond // the served answers
		}
		return fakejudge.Reply{Content: answer(r), Delay: d}
	})
	t.Cleanup(fake.Close)
	body := func(i int) string {
		return fmt.Sprintf(`{"model":"auto","messages":[{"role":"user","content":"Say hello to guest %d."}]}`, i)
	}
	shadowCfg := `    daily_budget_usd: "` + shadowBudgetUSD + `"
    concurrency: 4`

	off := start(t, o, shadowModels(fake.BaseURL(), "", shadowCfg), "")
	base := off.fire(t, n, body)
	on := start(t, o, shadowModels(fake.BaseURL(), "    shadow: {sample_rate: 1.0}", shadowCfg), "")
	got := on.fire(t, n, body)

	ids := make([]string, n)
	for i := range n {
		if base[i].status != http.StatusOK || got[i].status != http.StatusOK {
			t.Fatalf("request %d: status %d with shadow sampling, %d without", i, got[i].status, base[i].status)
		}
		ids[i] = got[i].id
	}
	const margin = 250 * time.Millisecond
	for _, p := range []float64{0.5, 0.95} {
		if b, s := percentile(base, p), percentile(got, p); s > 2*b+margin {
			t.Errorf("p%.0f latency %v with shadow sampling, %v without: outside the band", p*100, s, b)
		} else {
			t.Logf("p%.0f latency %v with shadow sampling, %v without", p*100, s, b)
		}
	}

	outcomes := on.waitShadow(t, float64(n))
	t.Logf("shadow outcomes of %d samples: %v", n, outcomes)
	if outcomes["judged"] == 0 || outcomes["dropped_budget"] == 0 || outcomes["error"] != 0 {
		t.Errorf("outcomes %v: want some judged, some dropped for the budget, no errors", outcomes)
	}

	limit := usd(t, shadowBudgetUSD)
	metricCost, _ := on.Metrics(t, "switchyard_shadow_cost_usd_total")
	if lf, _ := limit.Float64(); metricCost > lf+1e-12 {
		t.Errorf("switchyard_shadow_cost_usd_total = %v, over the %s budget", metricCost, shadowBudgetUSD)
	}
	rep := on.waitRequests(t, int64(n))
	spent := usd(t, rep.Totals.ShadowCostUSD)
	if spent.Cmp(limit) > 0 || spent.Sign() <= 0 {
		t.Errorf("shadow_cost_usd = %s, want in (0, %s]", rep.Totals.ShadowCostUSD, shadowBudgetUSD)
	}
	if usd(t, rep.Totals.OverheadUSD).Cmp(spent) < 0 {
		t.Errorf("overhead_usd %s does not include shadow_cost_usd %s", rep.Totals.OverheadUSD, rep.Totals.ShadowCostUSD)
	}
	t.Logf("shadow spend %s of the %s budget (metric %v); overhead %s", rep.Totals.ShadowCostUSD, shadowBudgetUSD, metricCost, rep.Totals.OverheadUSD)

	// Every stored evaluation joins to its request's event: the judged
	// ones and the errors, and the samples the budget stopped before a
	// judge call, which are stored (judge_budget) and counted among the
	// dropped_budget ones.
	var joined int64
	for _, q := range rep.Totals.Quality {
		joined += q.Judged + q.Errors
	}
	if stored := int64(outcomes["judged"] + outcomes["error"]); joined < stored || joined > stored+int64(outcomes["dropped_budget"]) {
		t.Errorf("the report joins %d shadow evaluations to usage events; %d were stored with a verdict or error, and up to %v stopped for the budget",
			joined, stored, outcomes["dropped_budget"])
	}
	return ShadowResult{Gateway: on, RequestIDs: ids, Outcomes: outcomes}
}

// waitRequests waits until the tenant's report counts n requests, and
// returns it.
func (g *Gateway) waitRequests(t testing.TB, n int64) QualityReport {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		r := g.qualityReport(t, "")
		if r.Totals.Requests >= n || time.Now().After(deadline) {
			if r.Totals.Requests != n {
				t.Fatalf("the report counts %d requests, want %d", r.Totals.Requests, n)
			}
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// codingRubric is a line of the judge's coding rubric (internal/judge),
// by which the fake judge recognizes a coding prompt.
const codingRubric = "The code is correct"

// VerifiedSavings runs n general and n coding requests in observe mode with
// shadow sampling at rate 1.0. The router would choose small, so the
// gateway serves the baseline, large, and the shadow runner asks small for
// the candidate answer. The judge accepts small's answers to general
// prompts and rejects them on coding. It checks, with verified_min_samples
// n and verified_min_pass_rate 0.7:
//
//   - general is verified, with n judged samples all acceptable, and its
//     projected savings count in verified_savings_usd, which is above 0;
//   - coding has n judged samples, none acceptable, stays unverified, and
//     its savings are not verified;
//   - the verified savings never replace the projected savings;
//   - feedback posted to /switchyard/v1/feedback is counted in the report,
//     and feedback on a request the tenant never made counts nowhere.
func VerifiedSavings(t *testing.T, o Options, n int) QualityReport {
	t.Helper()
	fake := fakejudge.New(func(r fakejudge.Request) fakejudge.Reply {
		if !r.Judge {
			return fakejudge.Reply{Content: answer(r)}
		}
		coding := strings.Contains(r.System, codingRubric)
		return fakejudge.Reply{Content: fakejudge.Prefer(func(a string) int {
			if coding && strings.Contains(a, "small-up") {
				return 0
			}
			return 1
		}, r)}
	})
	t.Cleanup(fake.Close)
	g := start(t, o, shadowModels(fake.BaseURL(), `    routing_mode: observe
    shadow: {sample_rate: 1.0}`, fmt.Sprintf(`    daily_budget_usd: "10"
    verified_min_samples: %d
    verified_min_pass_rate: 0.7`, n)), "")

	general := `{"model":"auto","messages":[{"role":"user","content":"Say hello to guest %d."}]}`
	coding := "{\"model\":\"auto\",\"messages\":[{\"role\":\"user\",\"content\":\"Fix the bug in this Go function %d:\\n```go\\nfunc add(a, b int) int {\\n\\treturn a - b\\n}\\n```\"}]}"
	res := g.fire(t, 2*n, func(i int) string {
		if i%2 == 0 {
			return fmt.Sprintf(general, i)
		}
		return fmt.Sprintf(coding, i)
	})
	for i, r := range res {
		if r.status != http.StatusOK {
			t.Fatalf("request %d: status %d", i, r.status)
		}
	}
	outcomes := g.waitShadow(t, float64(2*n))
	if outcomes["judged"] != float64(2*n) {
		t.Fatalf("shadow outcomes %v, want %d judged", outcomes, 2*n)
	}
	g.waitRequests(t, int64(2*n))
	// Feedback on two of the tenant's requests, through the public API.
	for i, score := range []int{1, -1} {
		r := g.Post(t, "/switchyard/v1/feedback", fmt.Sprintf(`{"request_id":%q,"score":%d,"comment":"scenario"}`, res[i].id, score))
		if r.Status != http.StatusAccepted {
			t.Fatalf("feedback: %d %s", r.Status, r.Body)
		}
	}
	if r := g.Post(t, "/switchyard/v1/feedback", `{"request_id":"01900000-0000-7000-8000-000000000000","score":1}`); r.Status != http.StatusAccepted && r.Status != http.StatusNotFound {
		t.Fatalf("feedback on an unknown request: %d %s", r.Status, r.Body)
	}
	rep := g.qualityReport(t, "group_by=category")
	if f := rep.Totals.Feedback; f.Positive != 1 || f.Negative != 1 || f.Neutral != 0 {
		t.Errorf("feedback counts %+v, want one positive and one negative", f)
	}
	t.Logf("verification %+v; totals: projected %s, verified %s, shadow cost %s, quality %+v",
		rep.Verification, rep.Totals.ProjectedSavingsUSD, rep.Totals.VerifiedSavingsUSD, rep.Totals.ShadowCostUSD, rep.Totals.Quality)

	byCat := map[string]int{}
	for i, q := range rep.Totals.Quality {
		byCat[q.Category] = i
	}
	gi, okG := byCat["general"]
	ci, okC := byCat["coding"]
	if !okG || !okC {
		t.Fatalf("quality %+v: want general and coding", rep.Totals.Quality)
	}
	if q := rep.Totals.Quality[gi]; !q.Verified || q.Judged != int64(n) || q.Acceptable != int64(n) {
		t.Errorf("general %+v: want verified, %d of %d acceptable", q, n, n)
	}
	if q := rep.Totals.Quality[ci]; q.Verified || q.Judged != int64(n) || q.Acceptable != 0 {
		t.Errorf("coding %+v: want unverified, 0 of %d acceptable", q, n)
	}
	verified, projected := usd(t, rep.Totals.VerifiedSavingsUSD), usd(t, rep.Totals.ProjectedSavingsUSD)
	if verified.Sign() <= 0 || verified.Cmp(projected) >= 0 {
		t.Errorf("verified savings %s, projected %s: want 0 < verified < projected", rep.Totals.VerifiedSavingsUSD, rep.Totals.ProjectedSavingsUSD)
	}
	// Per category: general's projected savings are all verified, coding's
	// none.
	for _, grp := range rep.Groups {
		var cat string
		_ = json.Unmarshal(grp["category"], &cat)
		var v, p string
		_ = json.Unmarshal(grp["verified_savings_usd"], &v)
		_ = json.Unmarshal(grp["projected_savings_usd"], &p)
		switch cat {
		case "general":
			if usd(t, v).Sign() <= 0 || usd(t, v).Cmp(usd(t, p)) != 0 {
				t.Errorf("general group: verified %s, projected %s; want all of it verified", v, p)
			}
		case "coding":
			if usd(t, v).Sign() != 0 || usd(t, p).Sign() <= 0 {
				t.Errorf("coding group: verified %s, projected %s; want none verified", v, p)
			}
		}
		t.Logf("group %s: projected %s, verified %s", cat, p, v)
	}
	return rep
}
