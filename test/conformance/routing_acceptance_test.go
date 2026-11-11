package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeanthropic"
	"switchyard/test/fakeupstream"
)

// The P5 acceptance cases (docs/PLAN.md) that need the gateway binary:
// header tightening, observe mode, the explain endpoint agreeing with
// `switchyard route explain`, and the soft-budget downgrade. The router's
// own invariants are unit- and property-tested in internal/router.

// routingEnv is a gateway in front of three tiers modeled on DESIGN
// §10.5: tier-s and tier-m on an OpenAI-wire fake, tier-l on a fake
// Messages API. The default policy's baseline is tier-l.
type routingEnv struct {
	g        *harness.Gateway
	bin, cfg string
	key      string
	oa       *fakeupstream.Server
	an       *fakeanthropic.Server
}

// routingYAML renders the config; policy and budgets are YAML fragments
// for the default policy's extra fields and t1's budgets.
func routingYAML(oaURL, anURL, hash, policy, budgets string) string {
	return fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 5s}
logging: {level: info, format: json}
providers:
  - {id: oa, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}"}
  - {id: an, type: anthropic, base_url: %q, api_key: "${PROVIDER_KEY}"}
models:
  - id: tier-s
    capabilities: [streaming, stream_usage, sampling]
    quality: {general: 0.60}
    deployments:
      - {provider: oa, upstream_model: tier-s-up, pricing: {input: "0.10", output: "0.40"}}
  - id: tier-m
    capabilities: [streaming, stream_usage, sampling]
    quality: {general: 0.75}
    deployments:
      - {provider: oa, upstream_model: tier-m-up, pricing: {input: "0.50", output: "2.00"}}
  - id: tier-l
    capabilities: [streaming, stream_usage]
    quality: {general: 0.95}
    deployments:
      - {provider: an, upstream_model: tier-l-up, pricing: {input: "3", output: "15"}}
policies:
  - id: default
    baseline_model: tier-l
%s
tenants:
  - id: t1
    keys: [{id: k1, hash: %q}]
%s
`, oaURL, anURL, policy, hash, budgets)
}

func startRouting(t *testing.T, policy, budgets string) *routingEnv {
	t.Helper()
	e := &routingEnv{
		oa: fakeupstream.New(fakeupstream.Options{APIKey: providerKey}),
		an: fakeanthropic.New(fakeanthropic.Options{APIKey: providerKey}),
	}
	t.Cleanup(e.oa.Close)
	t.Cleanup(e.an.Close)
	key, hash, err := harness.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	e.key = key
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if e.bin, err = harness.Build(ctx, dir, raceEnabled); err != nil {
		t.Fatal(err)
	}
	e.cfg = filepath.Join(dir, "switchyard.yaml")
	yaml := routingYAML(e.oa.BaseURL(), e.an.BaseURL(), hash, policy, budgets)
	if err := os.WriteFile(e.cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if e.g, err = harness.Start(ctx, e.bin, e.cfg, harness.StartOptions{Env: []string{"PROVIDER_KEY=" + providerKey}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.g.Stop(15 * time.Second); err != nil {
			t.Errorf("stopping the gateway: %v", err)
		}
		if strings.Contains(e.g.Stderr(), "WARNING: DATA RACE") {
			t.Errorf("the gateway reported a data race:\n%s", e.g.Stderr())
		}
		if t.Failed() {
			t.Logf("gateway logs:\n%s", strings.Join(e.g.Logs(), "\n"))
		}
	})
	return e
}

// routed is one gateway response.
type routed struct {
	status int
	header http.Header
	body   []byte
	code   string
}

// post sends body to path with the header pairs (name, value, …).
func (e *routingEnv) post(t *testing.T, path, body string, header ...string) routed {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.g.PublicURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	r := routed{status: resp.StatusCode, header: resp.Header, body: b}
	if r.status >= 400 {
		var env struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(b, &env)
		r.code = env.Error.Code
	}
	return r
}

// get fetches url from the public listener (with the key) or any other.
func (e *routingEnv) get(t *testing.T, url string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(url, e.g.PublicURL) {
		req.Header.Set("Authorization", "Bearer "+e.key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %v %s", url, resp.StatusCode, err, b)
	}
	return b
}

func chatBody(model string, extra string) string {
	return `{"model":"` + model + `",` + extra + `"messages":[{"role":"user","content":"Say hello."}]}`
}

// TestRoutingAcceptanceHeaderTightening: headers can tighten the policy but
// never loosen it. A denied model stays denied, a higher cost cap and a
// lower quality bar are ignored with a warning, a disallowed profile is
// ignored with a warning (or rejected in the model field), and a malformed
// value is rejected.
func TestRoutingAcceptanceHeaderTightening(t *testing.T) {
	e := startRouting(t, `    allowed_profiles: [balanced, cheap]
    deny: ["tier-m@*"]
    max_cost_per_request_usd: "0.005"`, "")
	const d = "X-Switchyard-Difficulty"
	tests := []struct {
		name    string
		model   string
		header  []string
		status  int
		code    string
		served  string
		profile string
		warning string
		reason  string
	}{
		// A hard request needs tier-l (est 0.0075 USD), which the cap rules
		// out; tier-m is denied, so quality is relaxed to tier-s.
		{name: "policy cap holds", model: "auto", header: []string{d, "high"},
			status: 200, served: "tier-s", profile: "balanced", reason: "quality_relaxed"},
		{name: "a higher cap is ignored", model: "auto", header: []string{d, "high", "X-Switchyard-Max-Cost-Usd", "10"},
			status: 200, served: "tier-s", profile: "balanced", reason: "quality_relaxed",
			warning: "X-Switchyard-Max-Cost-Usd ignored: a header may only lower the policy's cost cap"},
		{name: "a lower cap applies", model: "auto", header: []string{d, "low", "X-Switchyard-Max-Cost-Usd", "0.0000001"},
			status: 400, code: "no_eligible_model"},
		{name: "a denied model stays denied", model: "tier-m", status: 400, code: "no_eligible_model"},
		{name: "a denied model is never a fallback", model: "auto", header: []string{d, "0.6"},
			status: 200, served: "tier-s", profile: "balanced", reason: "quality_relaxed"},
		{name: "a lower quality bar is ignored", model: "auto", header: []string{d, "0.6", "X-Switchyard-Min-Quality", "0.1"},
			status: 200, served: "tier-s", profile: "balanced", reason: "quality_relaxed",
			warning: "X-Switchyard-Min-Quality ignored: a header may only raise the policy's quality bar"},
		{name: "a disallowed profile header is ignored", model: "auto", header: []string{d, "low", "X-Switchyard-Profile", "quality"},
			status: 200, served: "tier-s", profile: "balanced", reason: "cheapest_eligible",
			warning: "X-Switchyard-Profile ignored: the profile is not allowed by this policy (allowed: balanced, cheap)"},
		{name: "an allowed profile header selects", model: "auto", header: []string{d, "0.6", "X-Switchyard-Profile", "cheap"},
			status: 200, served: "tier-s", profile: "cheap", reason: "cheapest_eligible"},
		{name: "a disallowed profile in the model field", model: "auto:quality", status: 400, code: "model_not_supported"},
		{name: "a malformed cap", model: "auto", header: []string{"X-Switchyard-Max-Cost-Usd", "free"}, status: 400, code: "invalid_header"},
		{name: "a malformed difficulty", model: "auto", header: []string{d, "hard"}, status: 400, code: "invalid_header"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := e.post(t, "/v1/chat/completions", chatBody(tt.model, ""), tt.header...)
			if r.status != tt.status || r.code != tt.code {
				t.Fatalf("status %d code %q (%s), want %d %q", r.status, r.code, r.body, tt.status, tt.code)
			}
			if r.status != 200 {
				// A malformed value is never echoed. A valid cap may be:
				// the exclusions name the cap in force.
				for i := 1; tt.code == "invalid_header" && i < len(tt.header); i += 2 {
					if bytes.Contains(r.body, []byte(tt.header[i])) {
						t.Errorf("the error echoes %q: %s", tt.header[i], r.body)
					}
				}
				if tt.name == "a lower cap applies" && !bytes.Contains(r.body, []byte("max_cost_per_request 0.0000001 USD")) {
					t.Errorf("the exclusions do not name the exact cap: %s", r.body)
				}
				return
			}
			h := r.header
			if h.Get("X-Switchyard-Model") != tt.served || h.Get("X-Switchyard-Profile") != tt.profile ||
				h.Get("X-Switchyard-Policy-Warning") != tt.warning || h.Get("X-Switchyard-Route-Reason") != tt.reason {
				t.Errorf("model %q profile %q reason %q warning %q\nwant %q %q %q %q",
					h.Get("X-Switchyard-Model"), h.Get("X-Switchyard-Profile"), h.Get("X-Switchyard-Route-Reason"), h.Get("X-Switchyard-Policy-Warning"),
					tt.served, tt.profile, tt.reason, tt.warning)
			}
		})
	}
	for _, r := range e.oa.Requests() {
		if bytes.Contains(r.Body, []byte("tier-m-up")) {
			t.Errorf("the denied tier-m reached the upstream")
		}
	}
}

// usageTotals is the part of the usage report the routing tests read.
type usageTotals struct {
	Totals struct {
		Requests        int    `json:"requests"`
		CostUSD         string `json:"cost_usd"`
		BaselineCostUSD string `json:"baseline_cost_usd"`
		SavingsUSD      struct {
			Routing string `json:"routing"`
		} `json:"savings_usd"`
		Projected string `json:"projected_savings_usd"`
	} `json:"totals"`
	Groups []map[string]any `json:"groups"`
}

// TestRoutingAcceptanceObserveMode: in observe mode, 100% of responses come
// from the requested model, or the baseline for auto, every one names the
// model routing would have chosen, and the usage API reports projected
// savings above 0 while realized routing savings stay 0.
func TestRoutingAcceptanceObserveMode(t *testing.T) {
	e := startRouting(t, "    routing_mode: observe", "")
	requests := []struct {
		model, difficulty, want string
	}{
		{"auto", "low", "tier-l"}, {"auto", "0.6", "tier-l"}, {"auto", "high", "tier-l"},
		{"tier-m", "low", "tier-m"}, {"tier-s", "high", "tier-s"}, {"auto:cheap", "0.6", "tier-l"},
	}
	for range 3 {
		for _, rq := range requests {
			r := e.post(t, "/v1/chat/completions", chatBody(rq.model, ""), "X-Switchyard-Difficulty", rq.difficulty)
			if r.status != 200 {
				t.Fatalf("%s: status %d %s", rq.model, r.status, r.body)
			}
			if got := r.header.Get("X-Switchyard-Model"); got != rq.want {
				t.Errorf("%s at %s: served by %q, want %q", rq.model, rq.difficulty, got, rq.want)
			}
			if r.header.Get("X-Switchyard-Shadow-Model") == "" || r.header.Get("X-Switchyard-Route-Reason") != "observe" {
				t.Errorf("%s at %s: shadow %q reason %q", rq.model, rq.difficulty,
					r.header.Get("X-Switchyard-Shadow-Model"), r.header.Get("X-Switchyard-Route-Reason"))
			}
		}
	}
	// An easy auto request would have gone to tier-s.
	r := e.post(t, "/v1/chat/completions", chatBody("auto", ""), "X-Switchyard-Difficulty", "low")
	if got := r.header.Get("X-Switchyard-Shadow-Model"); got != "tier-s" {
		t.Errorf("shadow of an easy request %q, want tier-s", got)
	}

	var rep usageTotals
	if err := json.Unmarshal(e.get(t, e.g.PublicURL+"/switchyard/v1/usage?group_by=routing_mode"), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Totals.Requests != 3*len(requests)+1 || rep.Totals.SavingsUSD.Routing != "0" {
		t.Errorf("totals %+v, want realized routing savings 0", rep.Totals)
	}
	if !positive(rep.Totals.Projected) {
		t.Errorf("projected_savings_usd = %q, want > 0", rep.Totals.Projected)
	}
	if len(rep.Groups) != 1 || rep.Groups[0]["routing_mode"] != "observe" {
		t.Errorf("groups %v", rep.Groups)
	}
	if !bytes.Contains(e.get(t, e.g.AdminURL+"/metrics"), []byte("switchyard_routing_projected_savings_usd_total ")) {
		t.Error("no switchyard_routing_projected_savings_usd_total metric")
	}
}

// positive reports whether the decimal s is greater than 0.
func positive(s string) bool {
	return s != "" && !strings.HasPrefix(s, "-") && strings.Trim(s, "0.") != ""
}

// TestRoutingAcceptanceExplainAgrees: the explain endpoint of a fresh
// gateway (every deployment healthy) and `switchyard route explain`
// (offline) give byte-identical explanations for the same request, headers
// and config; and the endpoint calls no upstream.
func TestRoutingAcceptanceExplainAgrees(t *testing.T) {
	e := startRouting(t, `    prefer: ["tier-m@*"]
    rules:
      - name: code-needs-quality
        match: {task: coding}
        set: {min_quality: 0.85}`, "")
	cases := []struct {
		name   string
		body   string
		header []string
	}{
		{"easy", chatBody("auto", ""), nil},
		{"coding rule", `{"model":"auto","messages":[{"role":"user","content":"Fix the failing test:\n` + "```go\\nfunc TestSum(t *testing.T) {\\n\\tif Sum(1, 2) != 4 {\\n\\t\\tt.Fatal(\\\"want 3\\\")\\n\\t}\\n}\\n```" + `"}]}`, nil},
		{"headers", chatBody("auto:cheap", `"max_tokens":2000,`), []string{"X-Switchyard-Min-Quality", "0.3", "X-Switchyard-Difficulty", "high"}},
		{"concrete", chatBody("tier-l", ""), []string{"X-Switchyard-Task", "reasoning"}},
	}
	before := len(e.oa.Requests()) + len(e.an.Requests())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := e.post(t, "/switchyard/v1/route/explain", tc.body, tc.header...)
			if r.status != 200 {
				t.Fatalf("explain: %d %s", r.status, r.body)
			}
			file := filepath.Join(t.TempDir(), "req.json")
			if err := os.WriteFile(file, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"route", "explain", "-c", e.cfg, "--tenant", "t1", "--key", "k1"}
			for i := 0; i+1 < len(tc.header); i += 2 {
				args = append(args, "--header", tc.header[i]+": "+tc.header[i+1])
			}
			cmd := exec.Command(e.bin, append(args, file)...) // #nosec G204 -- the binary this test built
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "PROVIDER_KEY=" + providerKey}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("switchyard route explain: %v\n%s", err, stderr.String())
			}
			if !bytes.Equal(stdout.Bytes(), r.body) {
				t.Errorf("the CLI and the endpoint differ\nCLI:\n%s\nendpoint:\n%s", stdout.Bytes(), r.body)
			}
			var exp struct {
				Plan               []string `json:"plan"`
				MatchedRule        string   `json:"matched_rule"`
				MatchedQualityRule string   `json:"matched_quality_rule"`
			}
			if err := json.Unmarshal(r.body, &exp); err != nil || len(exp.Plan) == 0 {
				t.Errorf("explanation %s (%v)", r.body, err)
			}
			if tc.name == "coding rule" && (exp.MatchedRule != "" || exp.MatchedQualityRule != "code-needs-quality") {
				t.Errorf("matched_rule %q, matched_quality_rule %q", exp.MatchedRule, exp.MatchedQualityRule)
			}
		})
	}
	if after := len(e.oa.Requests()) + len(e.an.Requests()); after != before {
		t.Errorf("the explain endpoint made %d upstream calls", after-before)
	}
}

// TestRoutingAcceptanceSoftDowngrade: once a budget's soft threshold is
// crossed, auto requests switch to its soft profile, as the response
// headers, the explanation and the downgrade metric show.
func TestRoutingAcceptanceSoftDowngrade(t *testing.T) {
	e := startRouting(t, "", `    budgets:
      - {scope: tenant, window: day, limit_usd: "1", soft_at: 0.5, soft_profile: cheap}`)
	// d = 0.6: balanced (bar 0.80) needs tier-m; cheap (0.65) takes tier-s.
	r := e.post(t, "/v1/chat/completions", chatBody("auto", ""), "X-Switchyard-Difficulty", "0.6")
	if r.status != 200 || r.header.Get("X-Switchyard-Model") != "tier-m" || r.header.Get("X-Switchyard-Profile") != "balanced" {
		t.Fatalf("before the threshold: %d %v", r.status, r.header)
	}
	// Admission charges max_tokens of output at tier-l's price, 0.6 USD,
	// which crosses the 0.5 USD soft threshold; settlement then charges
	// the actual cost, but the threshold stays crossed for the window.
	if r := e.post(t, "/v1/chat/completions", chatBody("tier-l", `"max_tokens":40000,`)); r.status != 200 {
		t.Fatalf("the crossing request: %d %s", r.status, r.body)
	}
	r = e.post(t, "/v1/chat/completions", chatBody("auto", ""), "X-Switchyard-Difficulty", "0.6")
	if r.status != 200 || r.header.Get("X-Switchyard-Model") != "tier-s" || r.header.Get("X-Switchyard-Profile") != "cheap" ||
		r.header.Get("X-Switchyard-Route-Reason") != "budget_downgrade" {
		t.Fatalf("after the threshold: %d model %q profile %q reason %q", r.status,
			r.header.Get("X-Switchyard-Model"), r.header.Get("X-Switchyard-Profile"), r.header.Get("X-Switchyard-Route-Reason"))
	}
	x := e.post(t, "/switchyard/v1/route/explain", chatBody("auto", ""), "X-Switchyard-Difficulty", "0.6")
	var exp struct {
		Profile   string `json:"profile"`
		Downgrade string `json:"downgrade"`
	}
	if err := json.Unmarshal(x.body, &exp); err != nil || exp.Profile != "cheap" ||
		exp.Downgrade != "budget t1/day soft threshold crossed; profile cheap" {
		t.Errorf("explanation %s (%v)", x.body, err)
	}
	metrics := e.get(t, e.g.AdminURL+"/metrics")
	if !bytes.Contains(metrics, []byte(`switchyard_route_profile_downgrades_total{scope="t1/day"} 1`)) {
		t.Errorf("no downgrade in the metrics:\n%s", grepLines(metrics, "route_"))
	}
	if !bytes.Contains(metrics, []byte(`switchyard_route_decisions_total{profile="cheap",reason="budget_downgrade",served_model="tier-s"} 1`)) {
		t.Errorf("route_decisions_total:\n%s", grepLines(metrics, "route_decisions"))
	}
}

// grepLines returns the lines of b that contain s.
func grepLines(b []byte, s string) string {
	var out []string
	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.Contains(line, s) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
