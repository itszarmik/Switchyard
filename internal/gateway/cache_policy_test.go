package gateway

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"switchyard/internal/auth"
	"switchyard/internal/budget"
	"switchyard/internal/cache"
	"switchyard/internal/config"
	"switchyard/internal/policy"
	"switchyard/internal/schema"
)

// countingEmbedder embeds as fakeEmbedder does, at cost nano-USD a call,
// and counts its calls.
type countingEmbedder struct {
	fakeEmbedder
	cost  int64
	calls atomic.Int32
}

func (c *countingEmbedder) Embed(ctx context.Context, text string) (cache.Embedding, error) {
	c.calls.Add(1)
	emb, err := c.fakeEmbedder.Embed(ctx, text)
	if c.cost > 0 {
		emb.CostNano = c.cost
	}
	return emb, err
}

// Questions whose embeddings are the paraphrase pair of TestCacheSemantic.
const (
	franceQ     = `{"model":"m","messages":[{"role":"user","content":"What is the capital of France?"}]}`
	franceAgain = `{"model":"m","messages":[{"role":"user","content":"capital of France?"}]}`
)

var franceVecs = map[string][]float32{
	"What is the capital of France?": {1, 0},
	"capital of France?":             {0.97, float32(math.Sqrt(1 - 0.97*0.97))},
}

// semOn is a cache block with both layers on.
var semOn = config.CachePolicy{Exact: config.ExactCachePolicy{Enabled: true}, Semantic: config.SemanticCachePolicy{Enabled: true}}

// zdrForK2 tags provider b zdr, embeds on e@b, and gives key k2 a rule
// whose set is s.
func zdrForK2(s config.PolicyOverride) func(*config.Config) {
	return func(c *config.Config) {
		c.Providers[1].Tags = []string{"zdr"}
		c.Policies[0].Rules = []config.Rule{{Name: "k2", Match: config.RuleMatch{Key: "k2"}, Set: s}}
	}
}

// newPolicyCacheEnv is a cacheEnv for the policy checks of cache hits:
// both layers on, the semantic cache embedding with emb on e@b.
func newPolicyCacheEnv(t *testing.T, emb cache.Embedder, edit func(*config.Config)) *cacheEnv {
	t.Helper()
	e := newCacheEnv(t, semOn, nil, CacheOptions{}, edit)
	e.setSemanticOn(t, emb, "e@b")
	return e
}

// TestCacheHitPolicy: an entry is served only to a request whose own
// effective policy permits the deployment that produced it, in the exact
// and the semantic layer. Otherwise the lookup is a miss counted policy,
// the request is routed as usual, and its answer is written in turn (both
// reviews' H1).
func TestCacheHitPolicy(t *testing.T) {
	k2 := t1
	k2.KeyID = "k2"
	tests := []struct {
		name   string
		set    config.PolicyOverride
		second string
		layer  string
		// wantStatus and wantProvider are k2's answer after k1 warmed
		// the entry, from m@a; hit is whether it was a hit.
		wantStatus   int
		wantProvider string
		hit          bool
	}{
		{"exact, deny", config.PolicyOverride{Deny: []string{"m@*"}}, franceQ, cache.LayerExact, http.StatusBadRequest, "", false},
		{"exact, require_tags", config.PolicyOverride{RequireTags: []string{"zdr"}}, franceQ, cache.LayerExact, http.StatusOK, "b", false},
		{"exact, allow", config.PolicyOverride{Allow: []string{"m@b", "e@*"}}, franceQ, cache.LayerExact, http.StatusOK, "b", false},
		{"exact, permitted", config.PolicyOverride{Deny: []string{"m@c"}}, franceQ, cache.LayerExact, http.StatusOK, "a", true},
		{"semantic, deny", config.PolicyOverride{Deny: []string{"m@*"}}, franceAgain, cache.LayerSemantic, http.StatusBadRequest, "", false},
		{"semantic, require_tags", config.PolicyOverride{RequireTags: []string{"zdr"}}, franceAgain, cache.LayerSemantic, http.StatusOK, "b", false},
		{"semantic, permitted", config.PolicyOverride{Deny: []string{"m@c"}}, franceAgain, cache.LayerSemantic, http.StatusOK, "a", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newPolicyCacheEnv(t, fakeEmbedder{vecs: franceVecs}, zdrForK2(tt.set))
			if r := e.do(EndpointChatCompletions, franceQ, nil); r.status != 200 || r.info.Provider != "a" {
				t.Fatalf("k1 warm: %d %+v", r.status, r.info)
			}
			e.wait()
			r := e.doAs(context.Background(), k2, EndpointChatCompletions, tt.second, nil)
			e.wait()
			wantCache := cache.StatusMiss
			if tt.hit {
				wantCache = tt.layer
			}
			if r.status != tt.wantStatus || r.info.Cache != wantCache || (tt.wantProvider != "" && r.info.Provider != tt.wantProvider) {
				t.Fatalf("k2: status %d cache %q provider %q; want %d %q %q", r.status, r.info.Cache, r.info.Provider, tt.wantStatus, wantCache, tt.wantProvider)
			}
			wantPolicy := 1.0
			if tt.hit {
				wantPolicy = 0
			}
			if got := testutil.ToFloat64(e.g.metrics.cacheLookups.WithLabelValues(tt.layer, resultPolicy)); got != wantPolicy {
				t.Errorf("lookups{%s,policy} = %v, want %v", tt.layer, got, wantPolicy)
			}
			if tt.hit || tt.wantStatus != http.StatusOK {
				return
			}
			// k2's own answer was written, and k1, whose policy permits
			// m@b, is now served it.
			if got := testutil.ToFloat64(e.g.metrics.cacheWrites.WithLabelValues(tt.layer, resultOK)); got != 2 {
				t.Errorf("writes{%s,ok} = %v, want 2", tt.layer, got)
			}
			if r := e.do(EndpointChatCompletions, tt.second, nil); r.info.Cache != cache.StatusExact || r.info.Provider != "b" {
				t.Errorf("k1 after k2: cache %q provider %q, want an exact hit from b", r.info.Cache, r.info.Provider)
			}
		})
	}
}

// TestCacheHitSubstitution: under substitution pinned, a concrete model is
// answered only by an entry of its own deployments; under equivalent, by
// any the filters permit (correctness review H1).
func TestCacheHitSubstitution(t *testing.T) {
	tests := []struct {
		name         string
		substitution string
		request      string
		entry        string // the deployment of the stored entry
		hit          bool
	}{
		{"pinned, the model's own deployment", policy.SubstitutionPinned, chatReq, "m@b", true},
		{"pinned, another model", policy.SubstitutionPinned, chatReq, "n@a", false},
		{"equivalent, another model", policy.SubstitutionEquivalent, chatReq, "n@a", true},
		{"equivalent, model@provider is pinned", policy.SubstitutionEquivalent, `{"model":"m@a","messages":[{"role":"user","content":"hi"}]}`, "m@b", false},
		{"equivalent, an embedding deployment", policy.SubstitutionEquivalent, chatReq, "e@a", false},
		{"a deployment no longer in the catalog", policy.SubstitutionEquivalent, chatReq, "gone@a", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newCacheEnv(t, exactOn, nil, CacheOptions{}, func(c *config.Config) {
				c.Models = append(c.Models, config.Model{ID: "n", Capabilities: []string{"streaming"},
					Deployments: []config.Deployment{{Provider: "a", UpstreamModel: "n-a", Pricing: price("1", "1")}}})
				c.Policies[0].Substitution = config.Substitution{Mode: tt.substitution}
			})
			// Store the entry under the request's key, as a miss of
			// another request of the same target would have.
			req, err := schema.DecodeChatRequest([]byte(tt.request))
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := cache.Canonical(req, "m")
			if err != nil {
				t.Fatal(err)
			}
			model, _, _ := strings.Cut(tt.entry, "@")
			entry := &cache.Entry{Body: []byte(defaultChat), Deployment: tt.entry, Model: model}
			key := cache.ExactKey(cache.ScopeID(exactOn.Exact.Scope, "t1", "k1", ""), 0, canonical)
			if err := e.g.cache.store.Set(context.Background(), key, entry, time.Hour); err != nil {
				t.Fatal(err)
			}
			r := e.do(EndpointChatCompletions, tt.request, nil)
			if got := r.info.Cache == cache.StatusExact; got != tt.hit || r.status != 200 {
				t.Errorf("status %d cache %q model %q, want hit %v", r.status, r.info.Cache, r.info.Model, tt.hit)
			}
		})
	}
}

// TestCacheCoalescedFollowerPolicy: a follower whose policy forbids the
// deployment that answered the leader is not served the leader's answer:
// it executes on its own.
func TestCacheCoalescedFollowerPolicy(t *testing.T) {
	k2 := t1
	k2.KeyID = "k2"
	e := newCacheEnv(t, exactOn, nil, CacheOptions{}, zdrForK2(config.PolicyOverride{RequireTags: []string{"zdr"}}))
	release := make(chan struct{})
	e.a.script(step{hold: release})
	var wg sync.WaitGroup
	var leader, follower exchangeResult
	wg.Go(func() { leader = e.do(EndpointChatCompletions, chatReq, nil) })
	for e.a.count() < 1 {
		time.Sleep(time.Millisecond)
	}
	wg.Go(func() { follower = e.doAs(context.Background(), k2, EndpointChatCompletions, chatReq, nil) })
	for e.g.cache.coalescer.Followers() < 1 {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	if leader.status != 200 || leader.info.Provider != "a" || follower.status != 200 || follower.info.Provider != "b" || follower.info.Cache != "miss" {
		t.Errorf("leader %d %+v; follower %d %+v; want the follower served by b, a miss", leader.status, leader.info, follower.status, follower.info)
	}
	if a, b := e.a.count(), e.b.count(); a != 1 || b != 1 {
		t.Errorf("upstream calls a=%d b=%d, want 1 each", a, b)
	}
	if got := testutil.ToFloat64(e.g.metrics.cacheCoalesced); got != 0 {
		t.Errorf("coalesced = %v, want 0", got)
	}
}

// TestCacheDroppedParamsOnHit: a hit reports the parameters the miss
// dropped, as the miss did, since its body reflects their absence
// (correctness review H2).
func TestCacheDroppedParamsOnHit(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-streaming hit", true: "streaming hit"}[stream], func(t *testing.T) {
			cfg := routingConfig(true)
			cfg.Tenants = []config.Tenant{{ID: "t1", Keys: []config.APIKey{{ID: "k1"}}}}
			cfg.Policies = []config.Policy{{ID: "default", Cache: exactOn}}
			logs := &logBuffer{}
			g, err := New(Options{Config: cfg, Logger: slog.New(slog.NewJSONHandler(logs, nil)), Registry: prometheus.NewRegistry()})
			if err != nil {
				t.Fatal(err)
			}
			g.current().adapters["cheap"], g.current().adapters["rich"] = &fullAdapter{chatAdapter{name: "cheap"}}, &fullAdapter{chatAdapter{name: "rich"}}
			g.current().adapters["claude"] = &chatAdapter{name: "claude"}
			do := func(body string) exchangeResult {
				ctx, ann := annotated(logs)
				x := &recExchange{body: []byte(body)}
				g.ChatCompletions(auth.WithPrincipal(ctx, t1), x)
				x.res.ann = annotations(ann)
				return x.res
			}
			body := `{"model":"sonnet","n":2,"logprobs":true,"messages":[{"role":"user","content":"hi"}]}`
			miss := do(body)
			g.cache.writes.wg.Wait()
			hitBody := body
			if stream {
				hitBody = strings.Replace(body, `"n":2,`, `"n":2,"stream":true,`, 1)
			}
			hit := do(hitBody)
			if miss.info.Cache != "miss" || strings.Join(miss.info.DroppedParams, ",") != "logprobs,n" {
				t.Fatalf("miss: cache %q dropped %v", miss.info.Cache, miss.info.DroppedParams)
			}
			if hit.info.Cache != "exact" || strings.Join(hit.info.DroppedParams, ",") != "logprobs,n" {
				t.Errorf("hit: cache %q dropped %v, want exact with logprobs,n", hit.info.Cache, hit.info.DroppedParams)
			}
			if got := hit.ann["dropped_params"]; got != "logprobs,n" {
				t.Errorf("hit access log dropped_params = %v", got)
			}
		})
	}
}

// TestCacheSemanticEmbeddingPolicy: a request whose effective policy does
// not permit the embedding deployment skips the semantic layer, counted
// policy, without embedding (security review M1).
func TestCacheSemanticEmbeddingPolicy(t *testing.T) {
	k2 := t1
	k2.KeyID = "k2"
	tests := []struct {
		name    string
		set     config.PolicyOverride
		embeds  int32
		skipped bool
	}{
		{"deny", config.PolicyOverride{Deny: []string{"e@*"}}, 0, true},
		{"require_tags", config.PolicyOverride{RequireTags: []string{"eu"}}, 0, true},
		{"permitted", config.PolicyOverride{Deny: []string{"e@a"}}, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			emb := &countingEmbedder{fakeEmbedder: fakeEmbedder{vecs: franceVecs}}
			e := newPolicyCacheEnv(t, emb, zdrForK2(tt.set))
			r := e.doAs(context.Background(), k2, EndpointChatCompletions, franceQ, nil)
			if r.info.Cache != "miss" || emb.calls.Load() != tt.embeds {
				t.Errorf("status %d cache %q, %d embeddings; want a miss and %d", r.status, r.info.Cache, emb.calls.Load(), tt.embeds)
			}
			want := 0.0
			if tt.skipped {
				want = 1
			}
			if got := testutil.ToFloat64(e.g.metrics.cacheLookups.WithLabelValues(cache.LayerSemantic, resultPolicy)); got != want {
				t.Errorf("lookups{semantic,policy} = %v, want %v", got, want)
			}
			if ev := e.sink.only(t); tt.skipped != ev.OverheadCost.IsZero() {
				t.Errorf("overhead %s, want some only when embedded", ev.OverheadCost)
			}
		})
	}
}

// TestCacheSemanticBudget: with a blocking budget already exhausted, the
// semantic layer is skipped, counted budget, without embedding, while an
// exact hit is still served; otherwise the embedding's cost is charged to
// the request's budgets, with the chat cost on a miss and alone on a hit
// (security review M2).
func TestCacheSemanticBudget(t *testing.T) {
	const embedNano = 5000 // 5 micro-USD
	limit := func(usd string) []config.Budget {
		return []config.Budget{{Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: mustPrice(t, usd), OnExhausted: config.BudgetBlock}}
	}
	spent := func(t *testing.T, e *cacheEnv) int64 {
		t.Helper()
		n, err := e.g.budgets.Spent(context.Background(), e.g.current().scopes[principal{"t1", "k1"}][0], e.now)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	newEnv := func(t *testing.T, usd string) (*cacheEnv, *countingEmbedder) {
		emb := &countingEmbedder{fakeEmbedder: fakeEmbedder{vecs: franceVecs}, cost: embedNano}
		e := newCacheEnv(t, semOn, limit(usd), CacheOptions{}, nil)
		e.setSemantic(t, emb)
		return e, emb
	}

	t.Run("charged on a miss and on a hit", func(t *testing.T) {
		e, emb := newEnv(t, "1")
		if r := e.do(EndpointChatCompletions, franceQ, nil); r.status != 200 || r.info.Cache != "miss" {
			t.Fatalf("miss: %d %q", r.status, r.info.Cache)
		}
		e.wait()
		// defaultChat's 4 tokens at m@a, 1 micro-USD each, and the
		// embedding.
		if got := spent(t, e); got != 4+5 {
			t.Errorf("spent after the miss %d micro-USD, want 9", got)
		}
		if r := e.do(EndpointChatCompletions, franceAgain, nil); r.info.Cache != "semantic" {
			t.Fatalf("hit: %q", r.info.Cache)
		}
		if got := spent(t, e); got != 9+5 || emb.calls.Load() != 2 {
			t.Errorf("spent after the hit %d micro-USD with %d embeddings, want 14 and 2", got, emb.calls.Load())
		}
	})

	t.Run("an exhausted budget skips the semantic layer", func(t *testing.T) {
		e, emb := newEnv(t, "1")
		e.do(EndpointChatCompletions, franceQ, nil)
		e.wait()
		// Spend the rest of the budget.
		scope := e.g.current().scopes[principal{"t1", "k1"}][0]
		if _, err := e.g.budgets.Admit(context.Background(), []budget.Scope{scope}, scope.LimitMicro-spent(t, e), e.now); err != nil {
			t.Fatal(err)
		}
		r := e.do(EndpointChatCompletions, franceAgain, nil)
		if r.status != http.StatusTooManyRequests || r.info.Cache != "miss" || emb.calls.Load() != 1 {
			t.Errorf("paraphrase: %d %q with %d embeddings; want 429, a miss, no second embedding", r.status, r.info.Cache, emb.calls.Load())
		}
		if got := testutil.ToFloat64(e.g.metrics.cacheLookups.WithLabelValues(cache.LayerSemantic, resultBudget)); got != 1 {
			t.Errorf("lookups{semantic,budget} = %v, want 1", got)
		}
		if r := e.do(EndpointChatCompletions, franceQ, nil); r.status != 200 || r.info.Cache != "exact" {
			t.Errorf("exact hit with the budget spent: %d %q", r.status, r.info.Cache)
		}
	})

	t.Run("a failed charge is counted, never fails the hit", func(t *testing.T) {
		emb := &countingEmbedder{fakeEmbedder: fakeEmbedder{vecs: franceVecs}, cost: embedNano}
		store := &flakyBudgets{Store: budget.NewMemoryStore()}
		e := newCacheEnv(t, semOn, limit("1"), CacheOptions{}, nil)
		e.g.budgets = store
		e.setSemantic(t, emb)
		e.do(EndpointChatCompletions, franceQ, nil)
		e.wait()
		store.fail.Store(true)
		if r := e.do(EndpointChatCompletions, franceAgain, nil); r.status != 200 || r.info.Cache != "semantic" {
			t.Fatalf("hit with the budget store down: %d %q", r.status, r.info.Cache)
		}
		if got := testutil.ToFloat64(e.g.metrics.overheadUncharged); got != 1 {
			t.Errorf("overhead_uncharged = %v, want 1", got)
		}
	})
}

// flakyBudgets is a budget store whose admissions fail once fail is set;
// its soft reads keep working, so a request still reaches the semantic
// layer.
type flakyBudgets struct {
	budget.Store
	fail atomic.Bool
}

func (f *flakyBudgets) Admit(ctx context.Context, scopes []budget.Scope, est int64, now time.Time) (budget.Admission, error) {
	if f.fail.Load() {
		return budget.Admission{}, budget.ErrUnavailable
	}
	return f.Store.Admit(ctx, scopes, est, now)
}

// TestCacheStoreDownSkipsSemantic: when the exact read fails, the cache
// instance holding the vector index too is down, so the semantic layer is
// skipped, counted as an error, without paying for an embedding
// (correctness review L3).
func TestCacheStoreDownSkipsSemantic(t *testing.T) {
	emb := &countingEmbedder{fakeEmbedder: fakeEmbedder{vecs: franceVecs}}
	e := newCacheEnv(t, semOn, nil, CacheOptions{Store: failingCache{}}, nil)
	e.setSemantic(t, emb)
	if r := e.do(EndpointChatCompletions, franceQ, nil); r.status != 200 || r.info.Cache != "miss" {
		t.Fatalf("status %d cache %q", r.status, r.info.Cache)
	}
	if n := emb.calls.Load(); n != 0 {
		t.Errorf("%d embeddings, want none", n)
	}
	m := e.g.metrics
	if ex, sem := testutil.ToFloat64(m.cacheLookups.WithLabelValues("exact", "error")), testutil.ToFloat64(m.cacheLookups.WithLabelValues("semantic", "error")); ex != 1 || sem != 1 {
		t.Errorf("lookup errors exact %v, semantic %v; want 1 each", ex, sem)
	}
}

// TestCacheAggregateBound: the stream aggregation is bounded by the raw
// size of the chunks it takes, whatever they carry: logprobs with one
// character of text each stop it once past max_entry_bytes (both reviews'
// M1/M3).
func TestCacheAggregateBound(t *testing.T) {
	const maxBytes = 1 << 20
	st := &cacheState{eligible: true, key: "k"}
	st.startAggregating(true)
	top := make([]string, 20)
	for i := range top {
		top[i] = `{"token":"tok","logprob":-1.2345678,"bytes":[116,111,107]}`
	}
	data := `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},` +
		`"logprobs":{"content":[{"token":"x","logprob":-0.1,"bytes":[120],"top_logprobs":[` + strings.Join(top, ",") + `]}]},"finish_reason":null}]}`
	c, err := schema.ParseChatChunk([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for ; st.agg != nil && n < 10_000; n++ {
		st.aggregate(c, maxBytes)
	}
	if !st.aggOver || st.agg != nil || st.aggBytes > maxBytes+int64(len(data)) {
		t.Fatalf("after %d chunks of %d bytes: over %v, aggregating %v, counted %d", n, len(data), st.aggOver, st.agg != nil, st.aggBytes)
	}
	if want := maxBytes/len(data) + 1; n != want {
		t.Errorf("stopped after %d chunks, want %d", n, want)
	}
}

// TestCacheDowngradedNotWritten: an auto answer routed with a budget's
// soft profile is not written under the key of the profile it was asked
// for (security review M4).
func TestCacheDowngradedNotWritten(t *testing.T) {
	e := newTierEnv(t, func(c *config.Config) {
		c.Policies[0].Cache = exactOn
		c.Tenants[0].Budgets = []config.Budget{{
			Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: mustPriceS("1"),
			SoftAt: 0.5, SoftProfile: "cheap", OnExhausted: config.BudgetBlock,
		}}
	}, nil)
	scope := e.g.current().scopes[principal{"t1", "k1"}][0]
	if _, err := e.store.Admit(context.Background(), []budget.Scope{scope}, 600_000, e.now); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		r := e.do(EndpointChatCompletions, hiBody, HeaderDifficulty, "0.6")
		e.g.cache.writes.wg.Wait()
		if r.status != 200 || r.info.Model != "small" || r.info.RouteReason != reasonBudgetDowngrade || r.info.Cache != "miss" {
			t.Fatalf("request %d: status %d %+v; want a downgraded miss", i, r.status, r.info)
		}
	}
	m := e.g.metrics
	if w, h := testutil.ToFloat64(m.cacheWrites.WithLabelValues("exact", resultNotCacheable)), testutil.ToFloat64(m.cacheLookups.WithLabelValues("exact", "hit")); w != 2 || h != 0 {
		t.Errorf("writes{not_cacheable} %v, hits %v; want 2 and 0", w, h)
	}
}

// TestCacheAutoTargetConstraints: the auto target names every constraint
// a header or rule can tighten, so a request that demands more never
// shares an entry with one that did not (security review L1).
func TestCacheAutoTargetConstraints(t *testing.T) {
	auto := func(q float64, ttft time.Duration, capNano int64) *routed {
		rt := &routed{auto: true, requested: "auto:cheap"}
		rt.eff.Profile, rt.eff.PolicyID = "cheap", "default"
		rt.eff.Constraints.QualityFloor, rt.eff.Constraints.MaxTTFT, rt.eff.MaxCostPerRequestNano = q, ttft, capNano
		return rt
	}
	tests := []struct {
		name string
		rt   *routed
		want string
	}{
		{"none", auto(0, 0, 0), "auto:cheap@default#q=0,t=0,c=0"},
		{"all", auto(0.85, 1500*time.Millisecond, 50_000_000), "auto:cheap@default#q=0.85,t=1500,c=50000000"},
		{"a concrete model has none", &routed{requested: "m"}, "m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cacheTarget(tt.rt); got != tt.want {
				t.Errorf("cacheTarget = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("through the gateway", func(t *testing.T) {
		e := newTierEnv(t, func(c *config.Config) { c.Policies[0].Cache = exactOn }, nil)
		steps := []struct {
			header []string
			want   string
		}{
			{nil, "miss"},
			{nil, "exact"},
			{[]string{"X-Switchyard-Min-Quality", "0.8"}, "miss"},
			{[]string{"X-Switchyard-Min-Quality", "0.8"}, "exact"},
			{[]string{"X-Switchyard-Max-TTFT-Ms", "400"}, "miss"},
			{[]string{"X-Switchyard-Max-Cost-Usd", "0.5"}, "miss"},
		}
		for i, s := range steps {
			r := e.do(EndpointChatCompletions, hiBody, append([]string{HeaderDifficulty, "low"}, s.header...)...)
			e.g.cache.writes.wg.Wait()
			if r.status != 200 || r.info.Cache != s.want {
				t.Errorf("request %d (%v): status %d cache %q, want %q", i, s.header, r.status, r.info.Cache, s.want)
			}
		}
	})
}
