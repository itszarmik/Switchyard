package gateway

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"switchyard/internal/cache"
	"switchyard/internal/cache/semantic"
	"switchyard/internal/config"
	"switchyard/internal/usage"
)

// cacheEnv is an acctEnv whose default policy caches.
type cacheEnv struct {
	*acctEnv
}

// newCacheEnv builds a gateway whose default policy has the cache block
// pol, with t1's budgets, caches opts and the config edited by edit.
func newCacheEnv(t *testing.T, pol config.CachePolicy, budgets []config.Budget, opts CacheOptions, edit func(*config.Config)) *cacheEnv {
	t.Helper()
	sink := &captureSink{}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	r := fastRouting()
	r.Fallback.RetriesPerDeployment = 0
	e := newExecEnvOpts(t, r, func(c *config.Config) {
		c.Tenants = []config.Tenant{
			{ID: "t1", Keys: []config.APIKey{{ID: "k1"}, {ID: "k2"}}, Budgets: budgets},
			{ID: "t2", Keys: []config.APIKey{{ID: "k1"}}},
		}
		c.Policies = []config.Policy{{ID: "default", Cache: pol}}
		if edit != nil {
			edit(c)
		}
	}, func(o *Options) {
		o.Ledger, o.ConfigVersion, o.Cache = sink, "cfg-v1", opts
		o.Clock = func() time.Time { return now }
	})
	return &cacheEnv{&acctEnv{execEnv: e, sink: sink, now: now}}
}

// wait waits for the cache writes in flight.
func (e *cacheEnv) wait() { e.g.cache.writes.wg.Wait() }

// setSemantic gives the gateway a semantic cache embedding with emb, in
// memory, on the deployment e@a.
func (e *cacheEnv) setSemantic(t *testing.T, emb cache.Embedder) { e.setSemanticOn(t, emb, "e@a") }

// setSemanticOn gives the gateway a semantic cache embedding with emb, in
// memory, on the deployment dep.
func (e *cacheEnv) setSemanticOn(t *testing.T, emb cache.Embedder, dep string) {
	t.Helper()
	res, err := e.g.catalog.Resolve(dep)
	if err != nil {
		t.Fatal(err)
	}
	e.g.SetSemanticCache(semantic.New(emb, semantic.NewMemoryIndex(10, time.Now), semantic.Options{}), res.Candidates[0])
}

// exactOn is a policy cache block with only the exact cache.
var exactOn = config.CachePolicy{Exact: config.ExactCachePolicy{Enabled: true}}

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

const (
	chatReq   = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	streamReq = `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`
)

func TestParseCacheHeader(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
		bad    bool
	}{
		{"absent", nil, "", false},
		{"off", []string{"off"}, cacheHeaderOff, false},
		{"no-store", []string{"no-store"}, cacheHeaderNoStore, false},
		{"exact-only, any case and spaces", []string{" Exact-Only "}, cacheHeaderExactOnly, false},
		{"unknown", []string{"always"}, "", true},
		{"empty", []string{""}, "", true},
		{"twice", []string{"off", "off"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for _, v := range tt.values {
				h.Add(HeaderCache, v)
			}
			got, e := parseCacheHeader(h)
			if (e != nil) != tt.bad || got != tt.want {
				t.Fatalf("parseCacheHeader(%q) = %q, %v", tt.values, got, e)
			}
			if e != nil && (e.Status != http.StatusBadRequest || e.Code != codeInvalidHeader || strings.Contains(e.Message, "always")) {
				t.Errorf("error %+v: want 400 invalid_header that does not echo the value", e)
			}
		})
	}
}

// TestCacheExact serves a first request and a second one, and checks what
// the second got.
func TestCacheExact(t *testing.T) {
	tests := []struct {
		name          string
		pol           config.CachePolicy
		first, second string
		h1, h2        http.Header
		// wantCache is the X-Switchyard-Cache of the first and second
		// responses; calls the upstream calls in all.
		wantCache [2]string
		calls     int
	}{
		{"non-stream hit", exactOn, chatReq, chatReq, nil, nil, [2]string{"miss", "exact"}, 1},
		{"stream hit", exactOn, streamReq, streamReq, nil, nil, [2]string{"miss", "exact"}, 1},
		{"non-stream miss, stream hit", exactOn, chatReq, streamReq, nil, nil, [2]string{"miss", "exact"}, 1},
		{"stream miss, non-stream hit", exactOn, streamReq, chatReq, nil, nil, [2]string{"miss", "exact"}, 1},
		{"user and stream_options do not change the key", exactOn, chatReq,
			`{"model":"m","user":"u","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`,
			nil, nil, [2]string{"miss", "exact"}, 1},
		{"temperature changes the key", exactOn, chatReq,
			`{"model":"m","temperature":0.5,"messages":[{"role":"user","content":"hi"}]}`, nil, nil, [2]string{"miss", "miss"}, 2},
		{"no-store does not write", exactOn, chatReq, chatReq, hdr(HeaderCache, "no-store"), nil, [2]string{"miss", "miss"}, 2},
		{"no-store still reads", exactOn, chatReq, chatReq, nil, hdr(HeaderCache, "no-store"), [2]string{"miss", "exact"}, 1},
		{"off neither reads", exactOn, chatReq, chatReq, nil, hdr(HeaderCache, "off"), [2]string{"miss", ""}, 2},
		{"off nor writes", exactOn, chatReq, chatReq, hdr(HeaderCache, "off"), nil, [2]string{"", "miss"}, 2},
		{"caching off in the policy", config.CachePolicy{}, chatReq, chatReq, nil, nil, [2]string{"", ""}, 2},
		{"deterministic_only skips a sampled request", config.CachePolicy{Exact: config.ExactCachePolicy{Enabled: true, DeterministicOnly: true}},
			chatReq, chatReq, nil, nil, [2]string{"", ""}, 2},
		{"deterministic_only caches temperature 0", config.CachePolicy{Exact: config.ExactCachePolicy{Enabled: true, DeterministicOnly: true}},
			`{"model":"m","temperature":0,"messages":[{"role":"user","content":"hi"}]}`,
			`{"model":"m","temperature":0,"messages":[{"role":"user","content":"hi"}]}`, nil, nil, [2]string{"miss", "exact"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCacheEnv(t, tt.pol, nil, CacheOptions{}, nil)
			r1 := e.do(EndpointChatCompletions, tt.first, tt.h1)
			e.wait()
			r2 := e.do(EndpointChatCompletions, tt.second, tt.h2)
			e.wait()
			if r1.status != 200 || r2.status != 200 {
				t.Fatalf("statuses %d, %d (%v %v)", r1.status, r2.status, r1.err, r2.err)
			}
			if got := [2]string{r1.info.Cache, r2.info.Cache}; got != tt.wantCache {
				t.Errorf("X-Switchyard-Cache = %q, want %q", got, tt.wantCache)
			}
			if got := e.a.count(); got != tt.calls {
				t.Errorf("upstream calls = %d, want %d", got, tt.calls)
			}
			if r2.info.Cache != cache.StatusExact {
				return
			}
			info := r2.info
			if info.Model != "m" || info.Provider != "a" || info.Attempts != 0 || info.RouteReason != reasonCacheExact {
				t.Errorf("hit info = %+v", info)
			}
			if strings.Contains(tt.second, `"stream":true`) {
				if info.CostUSD != "" || len(r2.events) < 4 || r2.events[len(r2.events)-1] != "[DONE]" {
					t.Errorf("stream hit: cost %q, events %q", info.CostUSD, r2.events)
				}
				if strings.Contains(tt.second, "include_usage") != strings.Contains(strings.Join(r2.events, ""), `"usage":{`) {
					t.Errorf("stream hit events %q: usage only when asked", r2.events)
				}
			} else if info.CostUSD != "0" || !strings.Contains(r2.body, `"content":"hel`) && !strings.Contains(r2.body, `"content":"hello"`) {
				t.Errorf("hit: cost %q, body %s", info.CostUSD, r2.body)
			}
			evs := e.sink.events()
			hit := evs[len(evs)-1]
			if hit.CacheStatus != usage.CacheStatusExact || !hit.Cost.IsZero() || !hit.CostKnown || hit.Attempts != 0 ||
				hit.Deployment != "m@a" || hit.Provider != "a" || hit.RouteReason != reasonCacheExact || !hit.UsageKnown {
				t.Errorf("hit event %+v", hit)
			}
			// The hit's tokens at the baseline m's cheapest deployment, a:
			// 1 USD per 1M tokens in and out.
			want := usage.FromNanoUSD((hit.Tokens.Input + hit.Tokens.Output) * 1000)
			if hit.BaselineCost.IsZero() || !hit.BaselineCost.Equal(want) {
				t.Errorf("hit baseline %s, want %s (tokens %+v)", hit.BaselineCost, want, hit.Tokens)
			}
			if got := testutil.ToFloat64(e.g.metrics.cacheLookups.WithLabelValues("exact", "hit")); got != 1 {
				t.Errorf("exact hits = %v", got)
			}
			if got := r2.ann["cache"]; got != "exact" {
				t.Errorf("access log cache = %v", got)
			}
		})
	}
}

// TestCacheHitIgnoresBudgetsAndHealth: a hit is served when the budget is
// exhausted and every deployment fails, and charges nothing.
func TestCacheHitIgnoresBudgetsAndHealth(t *testing.T) {
	store, ns := cache.NewMemoryStore(10, time.Now), cache.NewMemoryNamespaces()
	opts := CacheOptions{Store: store, Namespaces: ns}
	warm := newCacheEnv(t, exactOn, nil, opts, nil)
	if r := warm.do(EndpointChatCompletions, chatReq, nil); r.status != 200 {
		t.Fatalf("warm: %d", r.status)
	}
	warm.wait()

	broke := []config.Budget{{Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: mustPrice(t, "0.000001")}}
	e := newCacheEnv(t, exactOn, broke, opts, nil)
	e.a.script(step{err: err503})
	e.b.script(step{err: err503})
	e.c.script(step{err: err503})
	// A miss is refused: 429 for the spent budget.
	if r := e.do(EndpointChatCompletions, `{"model":"m","messages":[{"role":"user","content":"other"}]}`, nil); r.status != http.StatusTooManyRequests || r.info.Cache != "miss" {
		t.Fatalf("miss: %d %q, want 429 with X-Switchyard-Cache miss", r.status, r.info.Cache)
	}
	r := e.do(EndpointChatCompletions, chatReq, nil)
	if r.status != 200 || r.info.Cache != "exact" {
		t.Fatalf("hit: %d %q %v", r.status, r.info.Cache, r.err)
	}
	if n := e.a.count() + e.b.count() + e.c.count(); n != 0 {
		t.Errorf("%d upstream calls, want none", n)
	}
	if got := testutil.ToFloat64(e.g.metrics.budgetRejections); got != 1 {
		t.Errorf("budget rejections = %v, want 1 (the miss)", got)
	}
}

// failingCache fails every call.
type failingCache struct{}

var errDown = errors.New("redis: connection refused")

func (failingCache) Get(context.Context, string) (*cache.Entry, error) { return nil, errDown }
func (failingCache) Set(context.Context, string, *cache.Entry, time.Duration) error {
	return errDown
}

// hungCache blocks every call until its context ends.
type hungCache struct{}

func (hungCache) Get(ctx context.Context, _ string) (*cache.Entry, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (hungCache) Set(ctx context.Context, _ string, _ *cache.Entry, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestCacheFailOpen: a store that fails or hangs is a counted miss, and
// the request is served.
func TestCacheFailOpen(t *testing.T) {
	tests := []struct {
		name  string
		store cache.Store
	}{
		{"failing", failingCache{}},
		{"hung", hungCache{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCacheEnv(t, exactOn, nil, CacheOptions{Store: tt.store}, func(c *config.Config) {
				c.Cache.LookupTimeout = config.Duration(5 * time.Millisecond)
			})
			for range 3 {
				if r := e.do(EndpointChatCompletions, chatReq, nil); r.status != 200 || r.info.Cache != "miss" {
					t.Fatalf("status %d cache %q", r.status, r.info.Cache)
				}
			}
			e.wait()
			m := e.g.metrics
			if got := testutil.ToFloat64(m.cacheLookups.WithLabelValues("exact", "error")); got != 3 {
				t.Errorf("lookup errors = %v, want 3", got)
			}
			if got := testutil.ToFloat64(m.cacheWrites.WithLabelValues("exact", "error")); got != 3 {
				t.Errorf("write errors = %v, want 3", got)
			}
			if n := len(e.logs.find(t, "exact cache lookup failed; serving as a miss")); n != 1 {
				t.Errorf("%d lookup warnings, want 1 (rate-limited)", n)
			}
		})
	}
}

// TestCacheWriteGates: what is not written, and how it is counted.
func TestCacheWriteGates(t *testing.T) {
	big := `{"id":"x","object":"chat.completion","created":1,"model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"` +
		strings.Repeat("a", 2000) + `"},"finish_reason":"stop"}]}`
	filtered := `{"id":"x","object":"chat.completion","created":1,"model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"no"},"finish_reason":"content_filter"}]}`
	bigChunks := []string{
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"role":"assistant","content":"` + strings.Repeat("b", 1500) + `"}}]}`,
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	tests := []struct {
		name   string
		body   string
		step   step
		result string
	}{
		{"ok", chatReq, step{}, resultOK},
		{"too large", chatReq, step{body: big}, resultTooLarge},
		{"content filter", chatReq, step{body: filtered}, resultNotCacheable},
		{"stream ok", streamReq, step{}, resultOK},
		{"stream too large", streamReq, step{chunks: bigChunks}, resultTooLarge},
		{"stream without a finish", streamReq, step{chunks: defaultChunks[:2]}, resultNotCacheable},
		{"stream error", streamReq, step{end: errTransport}, resultNotCacheable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCacheEnv(t, exactOn, nil, CacheOptions{}, func(c *config.Config) { c.Cache.MaxEntryBytes = 1024 })
			e.a.script(tt.step)
			e.do(EndpointChatCompletions, tt.body, nil)
			e.wait()
			if got := testutil.ToFloat64(e.g.metrics.cacheWrites.WithLabelValues("exact", tt.result)); got != 1 {
				t.Errorf("writes{%s} = %v, want 1", tt.result, got)
			}
			hits := 0
			if tt.result == resultOK {
				hits = 1
			}
			e.a.script(step{})
			e.do(EndpointChatCompletions, tt.body, nil)
			if got := testutil.ToFloat64(e.g.metrics.cacheLookups.WithLabelValues("exact", "hit")); got != float64(hits) {
				t.Errorf("hits = %v, want %d", got, hits)
			}
		})
	}
}

// TestCacheScopes: tenants never share entries; the key and user scopes
// narrow sharing within a tenant.
func TestCacheScopes(t *testing.T) {
	t1k2 := t1
	t1k2.KeyID = "k2"
	t2 := t1
	t2.TenantID = "t2"
	withUser := func(u string) string {
		return `{"model":"m","user":"` + u + `","messages":[{"role":"user","content":"hi"}]}`
	}
	tests := []struct {
		name    string
		scope   string
		second  func(e *cacheEnv) exchangeResult
		wantHit bool
	}{
		{"tenant: another tenant misses", config.CacheScopeTenant, func(e *cacheEnv) exchangeResult {
			return e.doAs(context.Background(), t2, EndpointChatCompletions, withUser("u1"), nil)
		}, false},
		{"tenant: another key hits", config.CacheScopeTenant, func(e *cacheEnv) exchangeResult {
			return e.doAs(context.Background(), t1k2, EndpointChatCompletions, withUser("u2"), nil)
		}, true},
		{"key: another key misses", config.CacheScopeKey, func(e *cacheEnv) exchangeResult {
			return e.doAs(context.Background(), t1k2, EndpointChatCompletions, withUser("u1"), nil)
		}, false},
		{"key: another tenant misses", config.CacheScopeKey, func(e *cacheEnv) exchangeResult {
			return e.doAs(context.Background(), t2, EndpointChatCompletions, withUser("u1"), nil)
		}, false},
		{"user: the same user hits", config.CacheScopeUser, func(e *cacheEnv) exchangeResult {
			return e.doAs(context.Background(), t1k2, EndpointChatCompletions, withUser("u1"), nil)
		}, true},
		{"user: another user misses", config.CacheScopeUser, func(e *cacheEnv) exchangeResult {
			return e.doAs(context.Background(), t1, EndpointChatCompletions, withUser("u2"), nil)
		}, false},
		{"user: the same user of another tenant misses", config.CacheScopeUser, func(e *cacheEnv) exchangeResult {
			return e.doAs(context.Background(), t2, EndpointChatCompletions, withUser("u1"), nil)
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pol := exactOn
			pol.Exact.Scope = tt.scope
			e := newCacheEnv(t, pol, nil, CacheOptions{}, nil)
			e.do(EndpointChatCompletions, withUser("u1"), nil)
			e.wait()
			r := tt.second(e)
			if got := r.info.Cache == "exact"; got != tt.wantHit {
				t.Errorf("X-Switchyard-Cache = %q, want hit %v", r.info.Cache, tt.wantHit)
			}
		})
	}
}

// TestCacheCoalescing: concurrent identical non-streaming misses make one
// upstream call, and the others are served as exact hits.
func TestCacheCoalescing(t *testing.T) {
	const n = 20
	e := newCacheEnv(t, exactOn, nil, CacheOptions{}, nil)
	release := make(chan struct{})
	e.a.script(step{hold: release})
	var wg sync.WaitGroup
	results := make([]exchangeResult, n)
	for i := range n {
		wg.Go(func() { results[i] = e.do(EndpointChatCompletions, chatReq, nil) })
	}
	// Release the leader once every follower waits on it.
	for e.g.cache.coalescer.Followers() < n-1 {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	hits := 0
	for _, r := range results {
		if r.status != 200 || r.body == "" {
			t.Fatalf("status %d body %q", r.status, r.body)
		}
		if r.info.Cache == "exact" {
			hits++
		}
	}
	if calls := e.a.count(); calls != 1 || hits != n-1 {
		t.Errorf("%d upstream calls and %d exact hits, want 1 and %d", calls, hits, n-1)
	}
	if got := testutil.ToFloat64(e.g.metrics.cacheCoalesced); got != n-1 {
		t.Errorf("coalesced = %v", got)
	}
	if evs := e.sink.events(); len(evs) != n {
		t.Errorf("%d usage events, want %d", len(evs), n)
	}
}

// fakeEmbedder embeds each text as a scripted vector.
type fakeEmbedder struct {
	vecs map[string][]float32
	err  error
}

func (f fakeEmbedder) Embed(_ context.Context, text string) (cache.Embedding, error) {
	if f.err != nil {
		return cache.Embedding{CostNano: 7}, f.err
	}
	return cache.Embedding{Vector: f.vecs[text], Tokens: 2, CostNano: 50}, nil
}
func (fakeEmbedder) Model() string   { return "e@a" }
func (fakeEmbedder) Dimensions() int { return 2 }

// TestCacheSemantic: a similar last message hits within its partition; a
// dissimilar one misses; the embedding is overhead either way; an
// embedding failure is a counted miss.
func TestCacheSemantic(t *testing.T) {
	ask := func(text string) string {
		return `{"model":"m","messages":[{"role":"user","content":"` + text + `"}]}`
	}
	sem := config.CachePolicy{Exact: config.ExactCachePolicy{Enabled: true}, Semantic: config.SemanticCachePolicy{Enabled: true}}
	tests := []struct {
		name   string
		second string
		h      http.Header
		emb    fakeEmbedder
		want   string
	}{
		{"paraphrase hits", "capital of France?", nil, fakeEmbedder{}, "semantic"},
		{"near miss misses", "capital of Germany?", nil, fakeEmbedder{}, "miss"},
		{"exact-only skips semantic", "capital of France?", hdr(HeaderCache, "exact-only"), fakeEmbedder{}, "miss"},
		{"identical hits exact", "What is the capital of France?", nil, fakeEmbedder{}, "exact"},
	}
	vecs := map[string][]float32{
		"What is the capital of France?": {1, 0},
		"capital of France?":             {0.97, float32(math.Sqrt(1 - 0.97*0.97))},
		"capital of Germany?":            {0.85, float32(math.Sqrt(1 - 0.85*0.85))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCacheEnv(t, sem, nil, CacheOptions{}, nil)
			e.setSemantic(t, fakeEmbedder{vecs: vecs})
			e.do(EndpointChatCompletions, ask("What is the capital of France?"), nil)
			e.wait()
			r := e.do(EndpointChatCompletions, ask(tt.second), tt.h)
			if r.status != 200 || r.info.Cache != tt.want {
				t.Fatalf("status %d, X-Switchyard-Cache %q, want %q", r.status, r.info.Cache, tt.want)
			}
			evs := e.sink.events()
			ev := evs[len(evs)-1]
			wantOverhead := usage.FromNanoUSD(50)
			if tt.want == "exact" || tt.h != nil {
				wantOverhead = usage.Money{}
			}
			if !ev.OverheadCost.Equal(wantOverhead) || ev.CacheStatus != tt.want {
				t.Errorf("event: cache %q overhead %s, want %s", ev.CacheStatus, ev.OverheadCost, wantOverhead)
			}
			if tt.want == "semantic" && (r.info.RouteReason != reasonCacheSemantic || !ev.Cost.IsZero()) {
				t.Errorf("semantic hit: reason %q cost %s", r.info.RouteReason, ev.Cost)
			}
		})
	}
	t.Run("embedding failure is a miss", func(t *testing.T) {
		e := newCacheEnv(t, sem, nil, CacheOptions{}, nil)
		e.setSemantic(t, fakeEmbedder{err: errors.New("embed down")})
		r := e.do(EndpointChatCompletions, ask("x"), nil)
		e.wait()
		if r.status != 200 || r.info.Cache != "miss" {
			t.Fatalf("status %d cache %q", r.status, r.info.Cache)
		}
		if got := testutil.ToFloat64(e.g.metrics.cacheLookups.WithLabelValues("semantic", "error")); got != 1 {
			t.Errorf("semantic errors = %v", got)
		}
		if ev := e.sink.only(t); !ev.OverheadCost.Equal(usage.FromNanoUSD(7)) {
			t.Errorf("overhead %s, want the failed call's 7 nano-USD", ev.OverheadCost)
		}
	})
}

// TestCachePurge: purging a tenant bumps its namespace version, so its
// entries miss and other tenants' still hit.
func TestCachePurge(t *testing.T) {
	e := newCacheEnv(t, exactOn, nil, CacheOptions{}, nil)
	t2 := t1
	t2.TenantID = "t2"
	e.do(EndpointChatCompletions, chatReq, nil)
	e.doAs(context.Background(), t2, EndpointChatCompletions, chatReq, nil)
	e.wait()

	tests := []struct {
		body   string
		status int
	}{
		{`{"tenant":"nope"}`, 400},
		{`{}`, 400},
		{`not json`, 400},
		{`{"tenant":"t1","x":1}`, 400},
		{`{"tenant":"t1"}`, 200},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		e.g.CachePurgeHandler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/v1/cache/purge", strings.NewReader(tt.body)))
		if w.Code != tt.status || strings.Contains(w.Body.String(), "nope") {
			t.Errorf("purge %s: %d %s", tt.body, w.Code, w.Body)
		}
		if tt.status == 200 && w.Body.String() != `{"tenant":"t1","namespace_version":1}` {
			t.Errorf("purge answer %s", w.Body)
		}
	}
	if r := e.do(EndpointChatCompletions, chatReq, nil); r.info.Cache != "miss" {
		t.Errorf("t1 after purge: %q, want miss", r.info.Cache)
	}
	if r := e.doAs(context.Background(), t2, EndpointChatCompletions, chatReq, nil); r.info.Cache != "exact" {
		t.Errorf("t2 after t1's purge: %q, want exact", r.info.Cache)
	}

	t.Run("store down", func(t *testing.T) {
		e := newCacheEnv(t, exactOn, nil, CacheOptions{Namespaces: failingNamespaces{}}, nil)
		w := httptest.NewRecorder()
		e.g.CachePurgeHandler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/v1/cache/purge", strings.NewReader(`{"tenant":"t1"}`)))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("purge with the store down: %d", w.Code)
		}
	})
}

// failingNamespaces fails every call.
type failingNamespaces struct{}

func (failingNamespaces) Version(context.Context, string) (int64, error) { return 0, errDown }
func (failingNamespaces) Bump(context.Context, string) (int64, error)    { return 0, errDown }

// TestWritePool: a full pool drops the write; closing waits for the
// writes in flight, gives up when its context ends, and refuses new ones.
func TestWritePool(t *testing.T) {
	p := writePool{slots: make(chan struct{}, 1)}
	release, started := make(chan struct{}), make(chan struct{})
	if !p.submit(func() { close(started); <-release }) {
		t.Fatal("the first write was refused")
	}
	<-started
	if p.submit(func() {}) {
		t.Error("a write was taken with the pool full")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.close(ctx); err == nil {
		t.Error("close returned before the write in flight ended")
	}
	close(release)
	if err := p.close(context.Background()); err != nil {
		t.Errorf("close: %v", err)
	}
	if p.submit(func() {}) {
		t.Error("a write was taken after close")
	}
}

// TestCacheTarget: the key's target is the model, or the auto profile and
// policy.
func TestCacheTarget(t *testing.T) {
	auto := &routed{auto: true, requested: "auto:cheap"}
	auto.eff.Profile, auto.eff.PolicyID = "cheap", "default"
	if got := cacheTarget(auto); got != "auto:cheap@default#q=0,t=0,c=0" {
		t.Errorf("auto target %q", got)
	}
	if got := cacheTarget(&routed{requested: "m"}); got != "m" {
		t.Errorf("model target %q", got)
	}
	if got := entryProvider("m@a"); got != "a" {
		t.Errorf("entryProvider = %q", got)
	}
}
