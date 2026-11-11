// Package scenario holds the Phase 4 acceptance scenarios (docs/PLAN.md,
// P4), run through the gateway binary against fake upstreams: exact costs
// per provider, the ledger agreeing with the cost metric, and budgets
// under 1,000 concurrent requests. test/conformance runs them with the
// memory ledger and memory budgets; test/integration runs them again with
// the Postgres ledger and Redis budgets.
//
// Like the harness, it talks to the gateway only through its CLI and HTTP
// API.
package scenario

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeanthropic"
	"switchyard/test/fakeupstream"
)

// providerKey is the fakes' API key.
const providerKey = "sk-scenario-provider"

// callTimeout bounds each request to the gateway.
const callTimeout = 30 * time.Second

// Options configure a scenario's gateway.
type Options struct {
	// Storage is the config's storage section, as YAML, or "" for the
	// memory ledger and memory budgets.
	Storage string
	// Env holds extra environment variables for the gateway.
	Env []string
	// Race builds the gateway with the race detector.
	Race bool
}

// randomID returns a tenant id no other run uses, so that runs sharing a
// Redis or a Postgres never see each other's budgets or events.
func randomID(prefix string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// Gateway is a gateway started by a scenario, with its tenant's key.
type Gateway struct {
	*harness.Gateway
	Key, Tenant string
	client      *http.Client
}

func start(t *testing.T, o Options, providersModels, budgets string) *Gateway {
	t.Helper()
	key, hash, err := harness.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	tenant := randomID("p4-")
	cfg := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 10s}
logging: {level: info, format: json}
%s
%s
tenants:
  - id: %s
    keys: [{id: k1, hash: %q}]
%s
`, o.Storage, providersModels, tenant, hash, budgets)
	g := harness.RunYAML(t, []byte(cfg), harness.RunOptions{Race: o.Race, Env: append([]string{"PROVIDER_KEY=" + providerKey}, o.Env...)})
	tr := &http.Transport{MaxIdleConns: 2000, MaxIdleConnsPerHost: 2000, MaxConnsPerHost: 0}
	t.Cleanup(tr.CloseIdleConnections)
	return &Gateway{Gateway: g, Key: key, Tenant: tenant, client: &http.Client{Transport: tr, Timeout: callTimeout}}
}

// Reply is one gateway response.
type Reply struct {
	Status int
	Header http.Header
	Body   string
	// Events are the data payloads of an event stream.
	Events []string
}

// Post sends body to path as the tenant's key.
func (g *Gateway) Post(t testing.TB, path, body string) Reply {
	t.Helper()
	r, err := g.post(path, body)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (g *Gateway) post(path, body string) (Reply, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, g.PublicURL+path, strings.NewReader(body))
	if err != nil {
		return Reply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+g.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return Reply{}, err
	}
	defer resp.Body.Close()
	r := Reply{Status: resp.StatusCode, Header: resp.Header}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				r.Events = append(r.Events, data)
			}
		}
		return r, sc.Err()
	}
	b, err := io.ReadAll(resp.Body)
	r.Body = string(b)
	return r, err
}

// get sends a GET to url.
func (g *Gateway) get(url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return g.client.Do(req)
}

// Report is the part of a usage report the scenarios check.
type Report struct {
	Totals struct {
		Requests int64  `json:"requests"`
		Errors   int64  `json:"errors"`
		CostUSD  string `json:"cost_usd"`
	} `json:"totals"`
	Groups []map[string]any `json:"groups"`
}

// AdminReport reads the admin usage report of the scenario's tenant.
func (g *Gateway) AdminReport(t testing.TB, query string) Report {
	t.Helper()
	resp, err := g.get(g.AdminURL + "/admin/v1/usage?tenant=" + g.Tenant + "&" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var r Report
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &r) != nil {
		t.Fatalf("admin usage report: %d %s", resp.StatusCode, b)
	}
	return r
}

// WaitReport waits until the tenant's report counts n requests, which may
// take a flush interval or more with an asynchronous ledger.
func (g *Gateway) WaitReport(t testing.TB, query string, n int64) Report {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		r := g.AdminReport(t, query)
		if r.Totals.Requests >= n || time.Now().After(deadline) {
			if r.Totals.Requests != n {
				t.Fatalf("the report counts %d requests, want %d", r.Totals.Requests, n)
			}
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Metrics scrapes the admin /metrics and returns the sum of every sample
// of the metric name, and each sample's line.
func (g *Gateway) Metrics(t testing.TB, name string) (float64, []string) {
	t.Helper()
	resp, err := g.get(g.AdminURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var sum float64
	var lines []string
	for line := range strings.SplitSeq(string(b), "\n") {
		if !strings.HasPrefix(line, name+"{") && !strings.HasPrefix(line, name+" ") {
			continue
		}
		f := strings.Fields(line)
		v, err := strconv.ParseFloat(f[len(f)-1], 64)
		if err != nil {
			t.Fatalf("metric line %q: %v", line, err)
		}
		sum += v
		lines = append(lines, line)
	}
	return sum, lines
}

// usd parses a decimal amount of USD exactly.
func usd(t testing.TB, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("not a decimal amount: %q", s)
	}
	return r
}

// CostCase is one request of ExactCost and the cost it must have.
type CostCase struct {
	Name, Model, Path, Body string
	// Want is the hand-computed cost in USD.
	Want string
	// RequestID is the gateway's X-Request-Id for the request.
	RequestID string
}

// The hand-computed costs of ExactCost. Prices are USD per 1M tokens.
const (
	// OpenAI wire: prompt 1,000 of which 300 cached and 200 written to the
	// cache, so 500 uncached; 50 output. At 0.15 / 0.075 / 0.1875 / 0.6:
	// 500 × 0.15 + 300 × 0.075 + 200 × 0.1875 + 50 × 0.6 = 75 + 22.5 +
	// 37.5 + 30 = 165, so 0.000165 USD.
	wantOpenAI = "0.000165"
	// Anthropic: input 2,095, cache read 2,051, cache creation 2,051,
	// output 503. At 3 / 0.3 / 3.75 / 15: 6,285 + 615.3 + 7,691.25 +
	// 7,545 = 22,136.55, so 0.02213655 USD.
	wantAnthropic = "0.02213655"
	// Embeddings: 4 input tokens at 0.02: 0.08, so 0.00000008 USD.
	wantEmbedding = "0.00000008"
)

// ExactCost sends a non-streaming and a streaming chat request to an
// OpenAI-wire fake and to an Anthropic fake, and an embedding request,
// each reporting known usage with cached input and cache writes, and
// checks:
//
//   - X-Switchyard-Cost-Usd on every non-streaming response equals the
//     hand-computed decimal;
//   - the usage report's cost per model equals the sum of the hand-computed
//     costs, exactly;
//   - the ledger's total cost equals the sum of switchyard_cost_usd_total
//     to within 1e-6.
//
// It returns the cases with their request IDs, so the caller can check the
// ledger rows themselves.
func ExactCost(t *testing.T, o Options) []CostCase {
	t.Helper()
	oa := fakeupstream.New(fakeupstream.Options{APIKey: providerKey})
	t.Cleanup(oa.Close)
	oa.Script(fakeupstream.Behavior{Usage: &fakeupstream.Usage{PromptTokens: 1000, CompletionTokens: 50, CachedTokens: 300, CacheWriteTokens: 200}})
	claude := fakeanthropic.New(fakeanthropic.Options{APIKey: providerKey})
	t.Cleanup(claude.Close)
	claude.Script(fakeanthropic.Behavior{Usage: &fakeanthropic.Usage{InputTokens: 2095, OutputTokens: 503, CacheCreationInputTokens: 2051, CacheReadInputTokens: 2051}})
	embed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"em-up","usage":{"prompt_tokens":4,"total_tokens":4}}`)
	}))
	t.Cleanup(embed.Close)

	g := start(t, o, fmt.Sprintf(`providers:
  - {id: oa, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}"}
  - {id: claude, type: anthropic, base_url: %q, api_key: "${PROVIDER_KEY}"}
  - {id: embed, type: openai, base_url: %q}
models:
  - id: oa-model
    capabilities: [streaming, stream_usage, prompt_caching]
    deployments:
      - provider: oa
        upstream_model: oa-up
        pricing: {input: "0.15", cached_input: "0.075", cache_write: "0.1875", output: "0.6"}
  - id: claude-model
    capabilities: [streaming, stream_usage, prompt_caching]
    deployments:
      - provider: claude
        upstream_model: claude-up
        pricing: {input: "3", cached_input: "0.3", cache_write: "3.75", output: "15"}
  - id: embed-model
    type: embedding
    dimensions: 2
    deployments:
      - {provider: embed, upstream_model: em-up, pricing: {input: "0.02"}}`, oa.BaseURL(), claude.BaseURL(), embed.URL+"/v1"), "")

	chat := func(model string, stream bool) string {
		s := ""
		if stream {
			s = `,"stream":true,"stream_options":{"include_usage":true}`
		}
		return `{"model":"` + model + `","max_tokens":600,"messages":[{"role":"user","content":"Say hello."}]` + s + `}`
	}
	cases := []CostCase{
		{Name: "openai", Model: "oa-model", Path: "/v1/chat/completions", Body: chat("oa-model", false), Want: wantOpenAI},
		{Name: "openai stream", Model: "oa-model", Path: "/v1/chat/completions", Body: chat("oa-model", true), Want: wantOpenAI},
		{Name: "anthropic", Model: "claude-model", Path: "/v1/chat/completions", Body: chat("claude-model", false), Want: wantAnthropic},
		{Name: "anthropic stream", Model: "claude-model", Path: "/v1/chat/completions", Body: chat("claude-model", true), Want: wantAnthropic},
		{Name: "embeddings", Model: "embed-model", Path: "/v1/embeddings", Body: `{"model":"embed-model","input":"hello"}`, Want: wantEmbedding},
	}
	byModel := map[string]*big.Rat{}
	total := new(big.Rat)
	for i := range cases {
		c := &cases[i]
		r := g.Post(t, c.Path, c.Body)
		if r.Status != http.StatusOK {
			t.Fatalf("%s: status %d %s", c.Name, r.Status, r.Body)
		}
		c.RequestID = r.Header.Get("X-Request-Id")
		got := r.Header.Get("X-Switchyard-Cost-Usd")
		switch {
		case len(r.Events) > 0 && got != "":
			t.Errorf("%s: a stream has X-Switchyard-Cost-Usd %q", c.Name, got)
		case len(r.Events) == 0 && got != c.Want:
			t.Errorf("%s: X-Switchyard-Cost-Usd = %q, want %s", c.Name, got, c.Want)
		}
		if byModel[c.Model] == nil {
			byModel[c.Model] = new(big.Rat)
		}
		byModel[c.Model].Add(byModel[c.Model], usd(t, c.Want))
		total.Add(total, usd(t, c.Want))
	}

	rep := g.WaitReport(t, "group_by=model", int64(len(cases)))
	if usd(t, rep.Totals.CostUSD).Cmp(total) != 0 {
		t.Errorf("report total cost_usd = %s, want %s", rep.Totals.CostUSD, total.FloatString(15))
	}
	for _, grp := range rep.Groups {
		model, _ := grp["model"].(string)
		cost, _ := grp["cost_usd"].(string)
		if want := byModel[model]; want == nil || usd(t, cost).Cmp(want) != 0 {
			t.Errorf("report group %s: cost_usd %s, want %v", model, cost, want)
		}
	}
	if len(rep.Groups) != len(byModel) {
		t.Errorf("report has %d groups, want %d", len(rep.Groups), len(byModel))
	}

	metric, lines := g.Metrics(t, "switchyard_cost_usd_total")
	ledger, _ := usd(t, rep.Totals.CostUSD).Float64()
	if d := metric - ledger; d > 1e-6 || d < -1e-6 {
		t.Errorf("Σ switchyard_cost_usd_total = %v, ledger %s (%v)", metric, rep.Totals.CostUSD, lines)
	}
	t.Logf("ledger total %s USD, Σ cost_usd_total %v, difference %g", rep.Totals.CostUSD, metric, metric-ledger)
	return cases
}

// BudgetResult is what BudgetConcurrency observed.
type BudgetResult struct {
	// Admitted and Rejected count the 200 and 429 answers.
	Admitted, Rejected int
	// UpstreamCalls counts the requests the fake received.
	UpstreamCalls int
	// EstimateMicro is each request's admission estimate and LimitMicro the
	// budget, in micro-USD; SpentMicro is the final spend from the ledger.
	EstimateMicro, LimitMicro, SpentMicro int64
}

// BudgetConcurrency sends n concurrent requests against a tenant budget
// with room for 50 of them, to a fake that reports each request's usage
// as exactly the gateway's admission estimate plus extraOutput output
// tokens (an under-estimate when positive). Prices are 1 USD per 1M tokens
// in and out, 1 micro-USD per token, so every amount is a whole number of
// micro-USD. It checks:
//
//   - every answer is 200 or 429 budget_exceeded, of type
//     insufficient_quota, with a Retry-After of whole seconds;
//   - only admitted requests reach the upstream;
//   - the final spend in the ledger is at most the limit plus the sum of
//     the admitted requests' under-estimates (with no under-estimate, at
//     most the limit).
func BudgetConcurrency(t *testing.T, o Options, n, extraOutput int) BudgetResult {
	t.Helper()
	const maxTokens = 100
	body := `{"model":"b","max_tokens":100,"messages":[{"role":"user","content":"budget test"}]}`
	// The gateway's input estimate is bytes/4 of the body.
	estIn := len(body) / 4
	est := int64(estIn + maxTokens)
	limit := 50 * est

	up := fakeupstream.New(fakeupstream.Options{APIKey: providerKey})
	t.Cleanup(up.Close)
	up.Script(fakeupstream.Behavior{
		TTFT:  20 * time.Millisecond,
		Usage: &fakeupstream.Usage{PromptTokens: estIn, CompletionTokens: maxTokens + extraOutput},
	})
	g := start(t, o, fmt.Sprintf(`providers:
  - {id: up, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}"}
models:
  - id: b
    deployments:
      - {provider: up, upstream_model: b-up, pricing: {input: "1", output: "1"}}
routing: {fallback: {retries_per_deployment: 0}}`, up.BaseURL()),
		fmt.Sprintf(`    budgets: [{scope: tenant, window: day, limit_usd: "%d.%06d"}]`, limit/1_000_000, limit%1_000_000))

	var (
		mu       sync.Mutex
		res      = BudgetResult{EstimateMicro: est, LimitMicro: limit}
		failures []string
		wg       sync.WaitGroup
	)
	gate := make(chan struct{})
	for range n {
		wg.Go(func() {
			<-gate
			r, err := g.post("/v1/chat/completions", body)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				failures = append(failures, err.Error())
			case r.Status == http.StatusOK:
				res.Admitted++
			case r.Status == http.StatusTooManyRequests:
				res.Rejected++
				var env struct {
					Error struct{ Type, Code string }
				}
				_ = json.Unmarshal([]byte(r.Body), &env)
				ra, err := strconv.Atoi(r.Header.Get("Retry-After"))
				if env.Error.Code != "budget_exceeded" || env.Error.Type != "insufficient_quota" || err != nil || ra < 1 || ra > 86400 {
					failures = append(failures, fmt.Sprintf("429 with Retry-After %q: %s", r.Header.Get("Retry-After"), r.Body))
				}
			default:
				failures = append(failures, fmt.Sprintf("status %d: %s", r.Status, r.Body))
			}
		})
	}
	close(gate)
	wg.Wait()
	for i, f := range failures {
		if i == 5 {
			t.Errorf("... and %d more", len(failures)-5)
			break
		}
		t.Error(f)
	}

	res.UpstreamCalls = len(up.Requests())
	if res.UpstreamCalls != res.Admitted {
		t.Errorf("the upstream got %d requests, want the %d admitted ones only", res.UpstreamCalls, res.Admitted)
	}
	rep := g.WaitReport(t, "", int64(n))
	spent := new(big.Rat).Mul(usd(t, rep.Totals.CostUSD), big.NewRat(1_000_000, 1))
	if !spent.IsInt() {
		t.Fatalf("spend %s USD is not whole micro-USD", rep.Totals.CostUSD)
	}
	res.SpentMicro = spent.Num().Int64()
	underEstimates := int64(res.Admitted * extraOutput)
	if res.SpentMicro > limit+underEstimates {
		t.Errorf("spent %d micro-USD, more than the limit %d plus the under-estimates %d", res.SpentMicro, limit, underEstimates)
	}
	if res.Admitted == 0 || res.Rejected == 0 || res.Admitted+res.Rejected != n {
		t.Errorf("admitted %d, rejected %d of %d", res.Admitted, res.Rejected, n)
	}
	// Exact estimates leave no slack: the budget admits exactly the 50
	// requests it has room for. Under-estimates admit fewer, as settled
	// spend grows past the estimates.
	if (extraOutput == 0 && res.Admitted != 50) || res.Admitted > 50 {
		t.Errorf("admitted %d requests, want 50 at most (exactly 50 with exact estimates)", res.Admitted)
	}
	if res.SpentMicro != int64(res.Admitted)*(est+int64(extraOutput)) {
		t.Errorf("spent %d micro-USD, want %d admitted × %d", res.SpentMicro, res.Admitted, est+int64(extraOutput))
	}
	if ratio, lines := g.Metrics(t, "switchyard_budget_utilization_ratio"); len(lines) != 1 ||
		ratio != float64(res.SpentMicro)/float64(limit) {
		t.Errorf("budget utilization %v (%v), want %d/%d", ratio, lines, res.SpentMicro, limit)
	}
	if rejected, _ := g.Metrics(t, "switchyard_budget_rejections_total"); int(rejected) != res.Rejected {
		t.Errorf("switchyard_budget_rejections_total = %v, want %d", rejected, res.Rejected)
	}
	t.Logf("%d requests: %d admitted, %d rejected, %d upstream calls; spent %d of %d micro-USD (estimate %d, under-estimate %d each)",
		n, res.Admitted, res.Rejected, res.UpstreamCalls, res.SpentMicro, limit, est, extraOutput)
	return res
}
