package policy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/config"
)

// defaultCache is the cache block of a policy that configures none.
var defaultCache = config.CachePolicy{
	Exact:    config.ExactCachePolicy{TTL: config.Duration(24 * time.Hour), Scope: config.CacheScopeTenant},
	Semantic: config.SemanticCachePolicy{MinSimilarity: 0.95, TTL: config.Duration(72 * time.Hour), MaxChars: 2000},
}

// cacheConfig adds an embedding model for the semantic cache to the
// catalog, and has a default policy with an exact cache, a governance rule
// that replaces the block for the batch key, one that sets something else,
// a quality rule, and a policy without a cache block.
const cacheConfig = catalog + `  - id: emb
    type: embedding
    dimensions: 8
    deployments: [{provider: a, upstream_model: emb, pricing: {input: 0.02}}]
semantic_cache: {embedding_model: emb}
policies:
  - id: default
    baseline_model: m
    cache:
      exact: {enabled: true, ttl: 1h, scope: key, deterministic_only: true}
    rules:
      - name: batch-cache
        match: {key: acme-batch}
        set:
          cache:
            semantic: {enabled: true, min_similarity: 0.97}
      - name: other
        match: {key: acme-other}
        set: {min_quality: 0.9}
      - name: hard
        match: {difficulty_gte: 0.5}
        set: {min_quality: 0.95}
  - id: plain
    baseline_model: m
tenants:
  - id: acme
    keys:
      - {id: acme-prod, hash: "` + "sha256:1111111111111111111111111111111111111111111111111111111111111111" + `"}
      - {id: acme-batch, hash: "` + "sha256:2222222222222222222222222222222222222222222222222222222222222222" + `"}
      - {id: acme-other, hash: "` + "sha256:3333333333333333333333333333333333333333333333333333333333333333" + `"}
      - {id: acme-plain, policy: plain, hash: "` + "sha256:4444444444444444444444444444444444444444444444444444444444444444" + `"}
`

func TestResolveCache(t *testing.T) {
	e, cfg := newEngine(t, cacheConfig)
	acme := tenant(t, cfg, "acme")

	base := defaultCache
	base.Exact = config.ExactCachePolicy{Enabled: true, TTL: config.Duration(time.Hour), Scope: config.CacheScopeKey, DeterministicOnly: true}
	// The rule's block replaces the policy's whole: its exact cache is the
	// default (off), not the policy's.
	replaced := defaultCache
	replaced.Semantic.Enabled, replaced.Semantic.MinSimilarity = true, 0.97

	tests := []struct {
		name string
		key  string
		req  Request
		want config.CachePolicy
	}{
		{"base policy", "acme-prod", Request{Model: "m"}, base},
		{"governance rule replaces the block", "acme-batch", Request{Model: "m"}, replaced},
		{"rule without set.cache keeps it", "acme-other", Request{Model: "m"}, base},
		{"quality rule keeps it", "acme-prod", Request{Model: "m", Difficulty: 0.9}, base},
		{"quality rule on top of the governance rule", "acme-batch", Request{Model: "m", Difficulty: 0.9}, replaced},
		{"policy without a block gets the defaults", "acme-plain", Request{Model: "m"}, defaultCache},
		{"headers do not change it", "acme-prod", Request{Model: "m", Headers: http.Header{
			"X-Switchyard-Cache": {"off"}, HeaderMinQuality: {"0.9"},
		}}, base},
		{"auto requests too", "acme-batch", Request{Model: "auto"}, replaced},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := e.Resolve(acme, tt.key, tt.req)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if diff := cmp.Diff(tt.want, got.Cache); diff != "" {
				t.Errorf("Cache (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("soft-budget downgrade keeps it", func(t *testing.T) {
		got, err := e.ResolveDowngraded(acme, "acme-batch", Request{Model: "auto"}, "cheap")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(replaced, got.Cache); diff != "" {
			t.Errorf("Cache (-want +got):\n%s", diff)
		}
	})
}

func TestResolveCacheBuiltIn(t *testing.T) {
	e, cfg := newEngine(t, catalog+"tenants:\n  - {id: acme, keys: [{id: k, hash: \""+hash("a")+"\"}]}\n")
	got, err := e.Resolve(cfg.Tenants[0], "k", Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(defaultCache, got.Cache); diff != "" {
		t.Errorf("built-in policy Cache (-want +got):\n%s", diff)
	}
}

// TestCacheHandBuilt checks a configuration that did not go through
// config.Load: the engine applies the cache defaults itself, and rejects
// set.cache on a quality rule.
func TestCacheHandBuilt(t *testing.T) {
	cfg := &config.Config{
		Policies: []config.Policy{{
			ID:    "default",
			Cache: config.CachePolicy{Exact: config.ExactCachePolicy{Enabled: true}},
			Rules: []config.Rule{{Name: "r", Match: config.RuleMatch{Key: "k2"}, Set: config.PolicyOverride{
				Cache: &config.CachePolicy{Exact: config.ExactCachePolicy{Enabled: true, Scope: config.CacheScopeUser}},
			}}},
		}},
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tenant := config.Tenant{ID: "acme"}
	tests := []struct {
		key  string
		want config.CachePolicy
	}{
		{"k1", func() config.CachePolicy { c := defaultCache; c.Exact.Enabled = true; return c }()},
		{"k2", func() config.CachePolicy {
			c := defaultCache
			c.Exact.Enabled, c.Exact.Scope = true, config.CacheScopeUser
			return c
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got, err := e.Resolve(tenant, tt.key, Request{Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, got.Cache); diff != "" {
				t.Errorf("Cache (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("quality rule may not set cache", func(t *testing.T) {
		bad := &config.Config{Policies: []config.Policy{{
			ID: "p",
			Rules: []config.Rule{{Name: "q", Match: config.RuleMatch{Task: "coding"}, Set: config.PolicyOverride{
				Cache: &config.CachePolicy{},
			}}},
		}}}
		if _, err := New(bad); err == nil || !strings.Contains(err.Error(), "may only set min_quality and prefer") {
			t.Errorf("New = %v, want the quality rule rejected", err)
		}
	})
}

// TestCacheQualityRuleRejectedByValidation checks that config.Load, which
// every served configuration goes through, rejects set.cache on a quality
// rule before the engine sees it.
func TestCacheQualityRuleRejectedByValidation(t *testing.T) {
	text := catalog + `
policies:
  - id: default
    rules:
      - {name: q, match: {task: coding}, set: {cache: {exact: {enabled: true}}}}
`
	_, err := config.Load("cfg.yaml", config.LoadOptions{
		LookupEnv: func(string) (string, bool) { return "", false },
		ReadFile:  func(string) ([]byte, error) { return []byte(text), nil },
		Glob:      func(string) ([]string, error) { return nil, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "policies[0].rules[0].set.cache: a quality rule") {
		t.Errorf("Load = %v, want set.cache on a quality rule rejected", err)
	}
}
