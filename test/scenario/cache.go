package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeupstream"
)

// The Phase 6 acceptance scenarios (docs/PLAN.md, P6) that test/conformance
// runs with memory storage and test/integration again with Redis (and the
// Postgres ledger): an exact hit's ledger row and report, and purging one
// tenant's entries, exact and semantic, without touching another's.

// Embedding vectors of the scenarios' fake embedding model: Question and
// Paraphrase are 0.97 similar, above the default threshold of 0.95, and
// any other text is orthogonal to both.
const (
	// Question is the cached question.
	Question = "What is the capital of France?"
	// Paraphrase is Question reworded.
	Paraphrase = "What's the capital of France?"
	// embedDims is the fake embedding model's dimensions.
	embedDims = 4
)

// embed is the fake embedding model.
func embed(text string) []float32 {
	switch text {
	case Question:
		return []float32{1, 0, 0, 0}
	case Paraphrase:
		return []float32{0.97, float32(math.Sqrt(1 - 0.97*0.97)), 0, 0}
	}
	return []float32{0, 0, 0, 1}
}

// CacheGateway is a gateway with two tenants, A and B, each with one key,
// in front of one fake upstream that also embeds.
type CacheGateway struct {
	// Gateway's Key and Tenant are A's.
	*Gateway
	Up               *fakeupstream.Server
	TenantA, TenantB string
	KeyA, KeyB       string
}

// StartCache starts a CacheGateway whose default policy has the extra
// fields policy (such as its cache block). Prices are USD per 1M tokens:
// small 1 in, 2 out; large, the baseline of auto, 10 in, 20 out; the
// embedding model emb 0.5 in.
func StartCache(t *testing.T, o Options, policy string) *CacheGateway {
	t.Helper()
	up := fakeupstream.New(fakeupstream.Options{APIKey: providerKey, Embed: embed})
	t.Cleanup(up.Close)
	g := &CacheGateway{Up: up, TenantA: randomID("p6a-"), TenantB: randomID("p6b-")}
	var hashA, hashB string
	var err error
	if g.KeyA, hashA, err = harness.GenerateKey(); err != nil {
		t.Fatal(err)
	}
	if g.KeyB, hashB, err = harness.GenerateKey(); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 10s}
logging: {level: info, format: json}
%s
providers:
  - {id: up, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}"}
models:
  - id: small
    capabilities: [streaming, stream_usage, sampling]
    quality: {general: 0.9}
    deployments:
      - {provider: up, upstream_model: small-up, pricing: {input: "1", output: "2"}}
  - id: large
    capabilities: [streaming, stream_usage, sampling]
    quality: {general: 0.97}
    deployments:
      - {provider: up, upstream_model: large-up, pricing: {input: "10", output: "20"}}
  - id: emb
    type: embedding
    dimensions: %d
    deployments:
      - {provider: up, upstream_model: emb-up, pricing: {input: "0.5"}}
routing: {fallback: {retries_per_deployment: 0}}
semantic_cache: {embedding_model: emb}
policies:
  - id: default
    baseline_model: large
%s
tenants:
  - id: %s
    keys: [{id: ka, hash: %q}]
  - id: %s
    keys: [{id: kb, hash: %q}]
`, o.Storage, up.BaseURL(), embedDims, policy, g.TenantA, hashA, g.TenantB, hashB)
	hg := harness.RunYAML(t, []byte(cfg), harness.RunOptions{Race: o.Race, Env: append([]string{"PROVIDER_KEY=" + providerKey}, o.Env...)})
	tr := &http.Transport{MaxIdleConnsPerHost: 200}
	t.Cleanup(tr.CloseIdleConnections)
	g.Gateway = &Gateway{Gateway: hg, Key: g.KeyA, Tenant: g.TenantA, client: &http.Client{Transport: tr, Timeout: callTimeout}}
	return g
}

// Chat sends a chat completion as key, with the header pairs (name,
// value, …).
func (g *CacheGateway) Chat(t testing.TB, key, body string, header ...string) Reply {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, g.PublicURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	resp, err := g.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return Reply{Status: resp.StatusCode, Header: resp.Header, Body: string(b)}
}

// ChatCalls counts the chat completion requests the upstream received.
func (g *CacheGateway) ChatCalls() int {
	n := 0
	for _, r := range g.Up.Requests() {
		if strings.HasSuffix(r.Path, "/chat/completions") {
			n++
		}
	}
	return n
}

// Metric returns the sum of the samples of name whose labels include
// every one of labels (such as `layer="exact"`).
func (g *CacheGateway) Metric(t testing.TB, name string, labels ...string) float64 {
	t.Helper()
	_, lines := g.Metrics(t, name)
	var sum float64
	for _, line := range lines {
		ok := true
		for _, l := range labels {
			ok = ok && strings.Contains(line, l)
		}
		if ok {
			f := strings.Fields(line)
			var v float64
			if _, err := fmt.Sscan(f[len(f)-1], &v); err != nil {
				t.Fatalf("metric line %q: %v", line, err)
			}
			sum += v
		}
	}
	return sum
}

// WaitWrites waits until n writes to layer have succeeded: writes are
// asynchronous, after the response.
func (g *CacheGateway) WaitWrites(t testing.TB, layer string, n float64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := g.Metric(t, "switchyard_cache_writes_total", `layer="`+layer+`"`, `result="ok"`)
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%v %s cache writes succeeded, want %v", got, layer, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Purge purges tenant through the admin API and returns the status and
// the namespace version it reports.
func (g *CacheGateway) Purge(t testing.TB, body string) (int, int64) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, g.AdminURL+"/admin/v1/cache/purge", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		NamespaceVersion int64 `json:"namespace_version"`
	}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out.NamespaceVersion
}

// ask is a chat request for auto:cheap with one user message.
func ask(text string) string {
	return `{"model":"auto:cheap","messages":[{"role":"user","content":"` + text + `"}]}`
}

// cacheStatus returns a reply's X-Switchyard-Cache, failing on a non-200.
func cacheStatus(t testing.TB, r Reply) string {
	t.Helper()
	if r.Status != http.StatusOK {
		t.Fatalf("status %d: %s", r.Status, r.Body)
	}
	return r.Header.Get("X-Switchyard-Cache")
}

// CachePolicy is the default policy's cache block of the cache scenarios:
// exact and semantic, with the defaults.
const CachePolicy = `    cache: {exact: {enabled: true}, semantic: {enabled: true}}`

// CachePurge checks purging (DESIGN §11.5): tenants A and B both have
// exact and semantic entries for the same question; purging A through
// POST /admin/v1/cache/purge makes A miss on both layers, while B still
// hits on both. Unknown and missing tenants are 400.
func CachePurge(t *testing.T, o Options) {
	t.Helper()
	g := StartCache(t, o, CachePolicy)
	for _, key := range []string{g.KeyA, g.KeyB} {
		if got := cacheStatus(t, g.Chat(t, key, ask(Question))); got != "miss" {
			t.Fatalf("first request: %q, want miss (tenants never share entries)", got)
		}
	}
	g.WaitWrites(t, "exact", 2)
	g.WaitWrites(t, "semantic", 2)
	for _, key := range []string{g.KeyA, g.KeyB} {
		if got := cacheStatus(t, g.Chat(t, key, ask(Question))); got != "exact" {
			t.Fatalf("before the purge, the question: %q, want exact", got)
		}
		if got := cacheStatus(t, g.Chat(t, key, ask(Paraphrase))); got != "semantic" {
			t.Fatalf("before the purge, the paraphrase: %q, want semantic", got)
		}
	}

	for _, body := range []string{`{"tenant":"nope"}`, `{}`} {
		if status, _ := g.Purge(t, body); status != http.StatusBadRequest {
			t.Errorf("purge %s: %d, want 400", body, status)
		}
	}
	status, version := g.Purge(t, `{"tenant":"`+g.TenantA+`"}`)
	if status != http.StatusOK || version < 1 {
		t.Fatalf("purge A: %d, namespace_version %d", status, version)
	}

	// no-store keeps the purged tenant's misses from writing new entries.
	for _, text := range []string{Question, Paraphrase} {
		if got := cacheStatus(t, g.Chat(t, g.KeyA, ask(text), "X-Switchyard-Cache", "no-store")); got != "miss" {
			t.Errorf("A after the purge, %q: %q, want miss", text, got)
		}
	}
	if got := cacheStatus(t, g.Chat(t, g.KeyB, ask(Question))); got != "exact" {
		t.Errorf("B after A's purge, the question: %q, want exact", got)
	}
	if got := cacheStatus(t, g.Chat(t, g.KeyB, ask(Paraphrase))); got != "semantic" {
		t.Errorf("B after A's purge, the paraphrase: %q, want semantic", got)
	}
	if calls := g.ChatCalls(); calls != 4 {
		t.Errorf("%d upstream chat calls, want 4 (two first requests, two after the purge)", calls)
	}
}

// cacheReport is the part of a usage report CacheLedger checks.
type cacheReport struct {
	Totals struct {
		Requests        int64  `json:"requests"`
		CostUSD         string `json:"cost_usd"`
		BaselineCostUSD string `json:"baseline_cost_usd"`
		OverheadUSD     string `json:"overhead_usd"`
		SavingsUSD      struct {
			Routing string `json:"routing"`
			Cache   string `json:"cache"`
			Net     string `json:"net"`
		} `json:"savings_usd"`
		CacheHitRate float64 `json:"cache_hit_rate"`
	} `json:"totals"`
	// Groups are flat: the cache_status, then the group's totals.
	Groups []struct {
		CacheStatus string `json:"cache_status"`
		Requests    int64  `json:"requests"`
		CostUSD     string `json:"cost_usd"`
	} `json:"groups"`
}

// CacheLedger checks what an exact hit records (DESIGN §12.4): a miss
// routed from auto:cheap to small (100 input and 50 output tokens), then
// the same request served by the exact cache. Through both usage APIs,
// the tenant's and the admin's:
//
//   - the hit costs 0, with cache_status exact;
//   - cache savings equal the hit's baseline cost, those tokens at large:
//     100 × 10 + 50 × 20 = 2,000 micro-USD;
//   - routing savings are the miss's alone: 2,000 − (100 × 1 + 50 × 2) =
//     1,800 micro-USD;
//   - the cache hit rate is 0.5.
func CacheLedger(t *testing.T, o Options) {
	t.Helper()
	g := StartCache(t, o, `    cache: {exact: {enabled: true}}`)
	g.Up.Script(fakeupstream.Behavior{Usage: &fakeupstream.Usage{PromptTokens: 100, CompletionTokens: 50}})
	miss := g.Chat(t, g.KeyA, ask(Question))
	if got := cacheStatus(t, miss); got != "miss" {
		t.Fatalf("first request: %q", got)
	}
	g.WaitWrites(t, "exact", 1)
	hit := g.Chat(t, g.KeyA, ask(Question))
	if got := cacheStatus(t, hit); got != "exact" || hit.Body != miss.Body {
		t.Fatalf("second request: %q, body equal %v", got, hit.Body == miss.Body)
	}
	if c, a, rr := hit.Header.Get("X-Switchyard-Cost-Usd"), hit.Header.Get("X-Switchyard-Attempts"), hit.Header.Get("X-Switchyard-Route-Reason"); c != "0" || a != "0" || rr != "cache_exact" {
		t.Errorf("hit headers: cost %q, attempts %q, route reason %q", c, a, rr)
	}
	want := map[string]string{"cost": "0.0002", "baseline": "0.004", "routing": "0.0018", "cache": "0.002", "net": "0.0038"}
	check := func(name, url string, withKey bool) {
		var rep cacheReport
		deadline := time.Now().Add(30 * time.Second)
		for {
			rep = g.cacheReport(t, url, withKey)
			if rep.Totals.Requests >= 2 || time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		tot := rep.Totals
		got := map[string]string{"cost": tot.CostUSD, "baseline": tot.BaselineCostUSD, "routing": tot.SavingsUSD.Routing,
			"cache": tot.SavingsUSD.Cache, "net": tot.SavingsUSD.Net}
		for k, w := range want {
			if usd(t, got[k]).Cmp(usd(t, w)) != 0 {
				t.Errorf("%s report: %s = %s, want %s", name, k, got[k], w)
			}
		}
		if tot.Requests != 2 || tot.CacheHitRate != 0.5 {
			t.Errorf("%s report: %d requests, cache_hit_rate %v; want 2 and 0.5", name, tot.Requests, tot.CacheHitRate)
		}
		byStatus := map[string]string{}
		for _, grp := range rep.Groups {
			byStatus[grp.CacheStatus] = grp.CostUSD
		}
		if len(byStatus) != 2 || usd(t, byStatus["exact"]).Sign() != 0 || usd(t, byStatus["miss"]).Cmp(usd(t, want["cost"])) != 0 {
			t.Errorf("%s report by cache_status: cost %v, want exact 0 and miss %s", name, byStatus, want["cost"])
		}
		t.Logf("%s report: cost %s, baseline %s, savings routing %s cache %s net %s, cache_hit_rate %v",
			name, tot.CostUSD, tot.BaselineCostUSD, tot.SavingsUSD.Routing, tot.SavingsUSD.Cache, tot.SavingsUSD.Net, tot.CacheHitRate)
	}
	check("tenant", g.PublicURL+"/switchyard/v1/usage?group_by=cache_status", true)
	check("admin", g.AdminURL+"/admin/v1/usage?tenant="+g.TenantA+"&group_by=cache_status", false)
	if saved := g.Metric(t, "switchyard_savings_usd_total", `source="cache"`); math.Abs(saved-0.002) > 1e-9 {
		t.Errorf(`switchyard_savings_usd_total{source="cache"} = %v, want 0.002`, saved)
	}
}

// cacheReport reads a usage report, as tenant A's key when withKey.
func (g *CacheGateway) cacheReport(t testing.TB, url string, withKey bool) cacheReport {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if withKey {
		req.Header.Set("Authorization", "Bearer "+g.KeyA)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var rep cacheReport
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &rep) != nil {
		t.Fatalf("usage report %s: %d %s", url, resp.StatusCode, b)
	}
	return rep
}
