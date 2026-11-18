package conformance

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeupstream"
)

// startPolicyCacheEnv starts a gateway for the policy checks of cache
// hits: tenant ta has key ka1 on policy open and ka2 on policy strict,
// which denies cm@* (so only cm2 serves it) and caches with strictCache;
// tenant tb has a blocking budget of 1 micro-USD, too small for any chat
// request, and the semantic cache. The fake embeds every text alike, so any two
// messages of a partition are paraphrases.
func startPolicyCacheEnv(t *testing.T, strictCache string) *cacheEnv {
	t.Helper()
	embed := func(string) []float32 { return []float32{1, 2, 0.5, 0.25} }
	e := &cacheEnv{up: fakeupstream.New(fakeupstream.Options{APIKey: providerKey, Embed: embed}), keys: map[string]string{}}
	t.Cleanup(e.up.Close)
	hashes := map[string]string{}
	for _, id := range []string{"ka1", "ka2", "kb1"} {
		key, hash, err := harness.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		e.keys[id], hashes[id] = key, hash
	}
	cfg := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 5s}
logging: {level: info, format: json}
providers:
  - {id: up, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}"}
models:
  - id: cm
    deployments:
      - {provider: up, upstream_model: cm-up, pricing: {input: "1", output: "2"}}
  - id: cm2
    deployments:
      - {provider: up, upstream_model: cm2-up, pricing: {input: "1", output: "2"}}
  - id: emb
    type: embedding
    dimensions: 4
    deployments:
      - {provider: up, upstream_model: emb-up, pricing: {input: "0.5"}}
routing: {fallback: {retries_per_deployment: 0}}
semantic_cache: {embedding_model: emb}
policies:
  - id: open
    cache: {exact: {enabled: true}, semantic: {enabled: true}}
  - id: strict
    deny: ["cm@*"]
    substitution: {mode: equivalent}
    cache: %s
tenants:
  - id: ta
    policy: open
    keys: [{id: ka1, hash: %q}, {id: ka2, hash: %q, policy: strict}]
  - id: tb
    policy: open
    budgets: [{scope: tenant, window: day, limit_usd: "0.000001"}]
    keys: [{id: kb1, hash: %q}]
`, e.up.BaseURL(), strictCache, hashes["ka1"], hashes["ka2"], hashes["kb1"])
	e.g = harness.RunYAML(t, []byte(cfg), harness.RunOptions{Race: raceEnabled, Env: []string{"PROVIDER_KEY=" + providerKey}})
	e.http = &http.Client{Timeout: callTimeout}
	return e
}

// embedCalls counts the embedding requests the upstream received.
func (e *cacheEnv) embedCalls() int {
	n := 0
	for _, r := range e.up.Requests() {
		if strings.HasSuffix(r.Path, "/embeddings") {
			n++
		}
	}
	return n
}

// TestCacheAcceptancePolicy: a key whose policy denies a model is never
// served that model's cached answer, exact or semantic, though another key
// of the tenant cached it (both reviews' H1); its own request goes
// upstream to what its policy permits.
func TestCacheAcceptancePolicy(t *testing.T) {
	e := startPolicyCacheEnv(t, `{exact: {enabled: true}, semantic: {enabled: true}}`)
	q := `{"model":"cm","messages":[{"role":"user","content":"What is 2+2?"}]}`
	if r := e.post(t, "ka1", q); r.status != http.StatusOK || r.cache() != "miss" || r.header.Get("X-Switchyard-Model") != "cm" {
		t.Fatalf("ka1: %d %q %s", r.status, r.cache(), r.body)
	}
	e.waitWrites(t, "exact", 1)
	e.waitWrites(t, "semantic", 1)
	for _, tc := range []struct{ name, body, layer string }{
		{"exact", q, "exact"},
		{"semantic", `{"model":"cm","messages":[{"role":"user","content":"What's 2+2?"}]}`, "semantic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := e.metric(t, "switchyard_cache_lookups_total", `layer="`+tc.layer+`"`, `result="policy"`)
			// no-store keeps ka2's own answers out of the caches, so that
			// each case meets only ka1's entry.
			r := e.post(t, "ka2", tc.body, "X-Switchyard-Cache", "no-store")
			if r.status != http.StatusOK || r.cache() != "miss" || r.header.Get("X-Switchyard-Model") != "cm2" {
				t.Errorf("ka2: %d cache %q model %q; want a miss served by cm2", r.status, r.cache(), r.header.Get("X-Switchyard-Model"))
			}
			if got := e.metric(t, "switchyard_cache_lookups_total", `layer="`+tc.layer+`"`, `result="policy"`); got <= before {
				t.Errorf(`lookups{layer=%q,result="policy"} = %v, want more than %v`, tc.layer, got, before)
			}
		})
	}
}

// TestCacheAcceptanceSemanticBudget: every request of tenant tb is
// refused 429 by its budget, but the first one's embedding is charged to
// it, which spends it; from then on the semantic layer is skipped, counted
// budget, and makes no more paid embedding calls (security review M2).
func TestCacheAcceptanceSemanticBudget(t *testing.T) {
	const n = 5
	e := startPolicyCacheEnv(t, `{exact: {enabled: true}}`)
	for i := range n {
		r := e.post(t, "kb1", fmt.Sprintf(`{"model":"cm","messages":[{"role":"user","content":"question number %d"}]}`, i))
		if r.status != http.StatusTooManyRequests {
			t.Fatalf("request %d: %d %s, want 429", i, r.status, r.body)
		}
	}
	if calls, embeds := e.chatCalls(), e.embedCalls(); calls != 0 || embeds != 1 {
		t.Errorf("%d chat and %d embedding calls, want 0 and 1", calls, embeds)
	}
	if got := e.metric(t, "switchyard_cache_lookups_total", `layer="semantic"`, `result="budget"`); got != n-1 {
		t.Errorf(`lookups{layer="semantic",result="budget"} = %v, want %d`, got, n-1)
	}
}
