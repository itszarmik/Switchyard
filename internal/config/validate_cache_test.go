package config

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// cacheYAML returns a config with chat model m and embedding model e (8
// dimensions, on provider a), and the given extra sections.
func cacheYAML(extra ...string) string {
	return catalogYAML(chatModel("", "")+embeddingModel("dimensions: 8"), strings.Join(extra, ""))
}

func TestCacheDefaults(t *testing.T) {
	cfg := mustLoad(t, cacheYAML(policiesYAML(
		"{id: none}",
		"{id: some, cache: {exact: {enabled: true, scope: user}, semantic: {enabled: true, max_chars: 500}}}",
		"{id: ruled, rules: [{name: r, match: {key: k1}, set: {cache: {exact: {enabled: true}}}}, {name: q, match: {key: k2}, set: {min_quality: 0.9}}]}",
	), "semantic_cache: {embedding_model: e}\n"), nil, nil).Config()

	def := CachePolicy{
		Exact:    ExactCachePolicy{TTL: Duration(24 * time.Hour), Scope: CacheScopeTenant},
		Semantic: SemanticCachePolicy{MinSimilarity: 0.95, TTL: Duration(72 * time.Hour), MaxChars: 2000},
	}
	if diff := cmp.Diff(def, CachePolicyWithDefaults(CachePolicy{})); diff != "" {
		t.Errorf("CachePolicyWithDefaults (-want +got):\n%s", diff)
	}
	some := def
	some.Exact.Enabled, some.Exact.Scope = true, CacheScopeUser
	some.Semantic.Enabled, some.Semantic.MaxChars = true, 500
	ruled := def
	ruled.Exact.Enabled = true

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"unset policy block", cfg.Policies[0].Cache, def},
		{"partly set policy block", cfg.Policies[1].Cache, some},
		{"rule set.cache", *cfg.Policies[2].Rules[0].Set.Cache, ruled},
		{"rule without set.cache", cfg.Policies[2].Rules[1].Set.Cache == nil, true},
		{"cache", cfg.Cache, CacheConfig{LookupTimeout: Duration(25 * time.Millisecond), MaxEntryBytes: 1 << 20, MemoryMaxEntries: 10000}},
		{"semantic_cache", cfg.SemanticCache, SemanticCacheConfig{EmbeddingModel: "e", LookupTimeout: Duration(150 * time.Millisecond)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, tt.got); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}

	t.Run("set values are kept", func(t *testing.T) {
		cfg := mustLoad(t, cacheYAML(policiesYAML(
			"{id: p, cache: {exact: {enabled: true, ttl: 1h, scope: key, deterministic_only: true}, semantic: {enabled: true, min_similarity: 0.9, ttl: 2h, max_chars: 10}}}",
		), "cache: {lookup_timeout: 5ms, max_entry_bytes: 2048, memory_max_entries: 3}\n",
			"semantic_cache: {embedding_model: e@a, lookup_timeout: 1s}\n"), nil, nil).Config()
		want := CachePolicy{
			Exact:    ExactCachePolicy{Enabled: true, TTL: Duration(time.Hour), Scope: CacheScopeKey, DeterministicOnly: true},
			Semantic: SemanticCachePolicy{Enabled: true, MinSimilarity: 0.9, TTL: Duration(2 * time.Hour), MaxChars: 10},
		}
		if diff := cmp.Diff(want, cfg.Policies[0].Cache); diff != "" {
			t.Errorf("policy cache (-want +got):\n%s", diff)
		}
		if got := cfg.Cache; got != (CacheConfig{LookupTimeout: Duration(5 * time.Millisecond), MaxEntryBytes: 2048, MemoryMaxEntries: 3}) {
			t.Errorf("cache = %+v", got)
		}
		if got := cfg.SemanticCache; got != (SemanticCacheConfig{EmbeddingModel: "e@a", LookupTimeout: Duration(time.Second)}) {
			t.Errorf("semantic_cache = %+v", got)
		}
	})
}

func TestCacheValidation(t *testing.T) {
	pol := func(cache string) string { return policiesYAML("{id: p, cache: " + cache + "}") }
	const semOn = "{semantic: {enabled: true}}"
	const emb = "semantic_cache: {embedding_model: e}\n"
	tests := []struct {
		name  string
		yaml  string
		wants []want
	}{
		{"off by default", cacheYAML(policiesYAML("{id: p}")), nil},
		{"exact on", cacheYAML(pol("{exact: {enabled: true, ttl: 720h, scope: key}}")), nil},
		{"semantic on", cacheYAML(pol(semOn), emb), nil},
		{"semantic on, pinned embedding", cacheYAML(pol(semOn), "semantic_cache: {embedding_model: e@a}\n"), nil},
		{"semantic at the bounds", cacheYAML(pol("{semantic: {enabled: true, min_similarity: 1, max_chars: 100000, ttl: 720h}}"), emb), nil},
		{"limits at the bounds", cacheYAML("cache: {lookup_timeout: 1s, max_entry_bytes: 1024, memory_max_entries: 1}\n",
			"semantic_cache: {lookup_timeout: 5s}\n"), nil},
		{"largest entry", cacheYAML("cache: {max_entry_bytes: 16777216}\n"), nil},

		{"unknown scope", cacheYAML(pol("{exact: {scope: global}}")),
			[]want{errAt("policies[0].cache.exact.scope", `must be one of tenant, key, user, got "global"`)}},
		{"ttl above 30 days", cacheYAML(pol("{exact: {enabled: true, ttl: 721h}}")),
			[]want{errAt("policies[0].cache.exact.ttl", "must be at most 720h0m0s")}},
		{"semantic ttl above 30 days", cacheYAML(pol("{semantic: {ttl: 1000h}}")),
			[]want{errAt("policies[0].cache.semantic.ttl", "must be at most 720h0m0s")}},
		{"min_similarity above 1", cacheYAML(pol("{semantic: {min_similarity: 1.01}}")),
			[]want{errAt("policies[0].cache.semantic.min_similarity", "must be greater than 0 and at most 1, got 1.01")}},
		{"min_similarity negative", cacheYAML(pol("{semantic: {min_similarity: -0.5}}")),
			[]want{errAt("policies[0].cache.semantic.min_similarity", "got -0.5")}},
		{"max_chars too large", cacheYAML(pol("{semantic: {max_chars: 100001}}")),
			[]want{errAt("policies[0].cache.semantic.max_chars", "must be between 1 and 100000, got 100001")}},
		{"max_chars negative", cacheYAML(pol("{semantic: {max_chars: -1}}")),
			[]want{errAt("policies[0].cache.semantic.max_chars", "got -1")}},
		{"lookup_timeout above 1s", cacheYAML("cache: {lookup_timeout: 1001ms}\n"),
			[]want{errAt("cache.lookup_timeout", "must be at most 1s")}},
		{"semantic lookup_timeout above 5s", cacheYAML("semantic_cache: {lookup_timeout: 6s}\n"),
			[]want{errAt("semantic_cache.lookup_timeout", "must be at most 5s")}},
		{"max_entry_bytes too small", cacheYAML("cache: {max_entry_bytes: 1023}\n"),
			[]want{errAt("cache.max_entry_bytes", "must be between 1024 and 16777216, got 1023")}},
		{"max_entry_bytes too large", cacheYAML("cache: {max_entry_bytes: 16777217}\n"),
			[]want{errAt("cache.max_entry_bytes", "got 16777217")}},
		{"memory_max_entries negative", cacheYAML("cache: {memory_max_entries: -1}\n"),
			[]want{errAt("cache.memory_max_entries", "must be at least 1, got -1")}},

		{"semantic needs an embedding model", cacheYAML(policiesYAML("{id: q}", "{id: p, cache: "+semOn+"}")),
			[]want{errAt("semantic_cache.embedding_model", "required when the semantic cache is enabled (policies[1].cache.semantic.enabled)")}},
		{"a rule enabling semantic needs one too", cacheYAML(policiesYAML("{id: p, rules: [{name: r, match: {key: k1}, set: {cache: " + semOn + "}}]}")),
			[]want{errAt("semantic_cache.embedding_model", "(policies[0].rules[0].set.cache.semantic.enabled)")}},
		{"semantic off needs none", cacheYAML(pol("{exact: {enabled: true}, semantic: {enabled: false}}")), nil},
		{"unknown embedding model", cacheYAML(pol(semOn), "semantic_cache: {embedding_model: ee}\n"),
			[]want{errAt("semantic_cache.embedding_model", `unknown model "ee"; use an embedding model id or model@provider (did you mean "e"?)`)}},
		{"a chat model", cacheYAML(pol(semOn), "semantic_cache: {embedding_model: m}\n"),
			[]want{errAt("semantic_cache.embedding_model", `"m" is not an embedding model`)}},
		{"a provider that does not serve it", cacheYAML(pol(semOn), "semantic_cache: {embedding_model: e@b}\n"),
			[]want{errAt("semantic_cache.embedding_model", `"e@b" names a provider that does not serve that model`)}},
		{"checked even when semantic is off", cacheYAML("semantic_cache: {embedding_model: m}\n"),
			[]want{errAt("semantic_cache.embedding_model", "is not an embedding model")}},
		{"embedding model without dimensions", catalogYAML(chatModel("", "")+embeddingModel(""), pol(semOn)+emb),
			[]want{
				errAt("models[1].dimensions", "required for an embedding model"),
				errAt("semantic_cache.embedding_model", `the embedding model "e" has no dimensions`),
			}},

		{"governance rule may set cache", cacheYAML(policiesYAML("{id: p, rules: [{name: r, match: {key: k1, tags: {team: x}}, set: {cache: {exact: {enabled: true}}}}]}")), nil},
		{"governance rule cache is validated", cacheYAML(policiesYAML("{id: p, rules: [{name: r, match: {key: k1}, set: {cache: {exact: {scope: everyone}}}}]}")),
			[]want{errAt("policies[0].rules[0].set.cache.exact.scope", `got "everyone"`)}},
		{"quality rule may not set cache", cacheYAML(policiesYAML("{id: p, rules: [{name: r, match: {task: coding}, set: {cache: {exact: {enabled: true}}}}]}")),
			[]want{errAt("policies[0].rules[0].set.cache", msgQualityRuleSet)}},
		{"difficulty rule may not set cache", cacheYAML(policiesYAML("{id: p, rules: [{name: r, match: {difficulty_gte: 0.5}, set: {min_quality: 0.9, cache: {}}}]}")),
			[]want{errAt("policies[0].rules[0].set.cache", msgQualityRuleSet)}},

		{"semantic in redis mode on database 0", cacheYAML(pol(semOn), emb,
			"storage: {mode: redis, redis: {cache_url: \"redis://localhost:6379/0\", state_url: \"redis://localhost:6379/1\"}}\n"), nil},
		{"semantic in redis mode, no database named", cacheYAML(pol(semOn), emb,
			"storage: {mode: redis, redis: {cache_url: \"redis://localhost:6379\", state_url: \"redis://localhost:6380/0\"}}\n"), nil},
		{"semantic in redis mode on another database", cacheYAML(pol(semOn), emb,
			"storage: {mode: redis, redis: {cache_url: \"redis://localhost:6379/1\", state_url: \"redis://localhost:6380/0\"}}\n"),
			[]want{errAt("storage.redis.cache_url", "must name database 0 (redis://host:port/0, with no db parameter naming another), not database 1, since the semantic cache is enabled (policies[0].cache.semantic.enabled)")}},
		{"semantic in redis mode, database 0 written 00", cacheYAML(pol(semOn), emb,
			"storage: {mode: redis, redis: {cache_url: \"redis://localhost:6379/00\", state_url: \"redis://localhost:6380/0\"}}\n"), nil},
		{"semantic in redis mode, db=0", cacheYAML(pol(semOn), emb,
			"storage: {mode: redis, redis: {cache_url: \"redis://localhost:6379?db=0\", state_url: \"redis://localhost:6380/0\"}}\n"), nil},
		{"semantic in redis mode, db=1", cacheYAML(pol(semOn), emb,
			"storage: {mode: redis, redis: {cache_url: \"redis://localhost:6379?db=1\", state_url: \"redis://localhost:6380/0\"}}\n"),
			[]want{errAt("storage.redis.cache_url", "must name database 0 (redis://host:port/0, with no db parameter naming another), not database 1")}},
		{"semantic in redis mode, /0?db=1: the parameter wins", cacheYAML(pol(semOn), emb,
			"storage: {mode: redis, redis: {cache_url: \"redis://localhost:6379/0?db=1\", state_url: \"redis://localhost:6380/0\"}}\n"),
			[]want{errAt("storage.redis.cache_url", "not database 1")}},
		{"semantic in redis mode, a unix socket on db 2", cacheYAML(pol(semOn), emb,
			"storage: {mode: redis, redis: {cache_url: \"unix:///tmp/redis.sock?db=2\", state_url: \"redis://localhost:6380/0\"}}\n"),
			[]want{errAt("storage.redis.cache_url", "must be a redis:// or rediss:// URL with a host")}},
		{"exact only in redis mode on another database", cacheYAML(pol("{exact: {enabled: true}}"),
			"storage: {mode: redis, redis: {cache_url: \"redis://localhost:6379/1\", state_url: \"redis://localhost:6380/0\"}}\n"), nil},
		{"redis mode needs the cache instance", cacheYAML("storage: {mode: redis, redis: {state_url: \"redis://localhost:6380/0\"}}\n", pol("{exact: {enabled: true}}")),
			[]want{errAt("storage.redis.cache_url", "required when storage.mode is redis")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := problemsOf(t, tt.yaml, nil, nil)
			checkProblems(t, problems, tt.wants...)
			for _, p := range problems {
				if strings.Contains(p.String(), "localhost:6379") {
					t.Errorf("a problem quotes the URL: %s", p)
				}
			}
		})
	}
}

// TestRuleCacheDecoding checks the decoding of the optional set.cache
// block: absent and null leave it nil, an empty mapping sets it with every
// default, and a type error is reported at its path.
func TestRuleCacheDecoding(t *testing.T) {
	rule := func(set string) string {
		return cacheYAML(policiesYAML("{id: p, rules: [{name: r, match: {key: k1}, set: " + set + "}]}"))
	}
	def := CachePolicyWithDefaults(CachePolicy{})
	tests := []struct {
		name string
		set  string
		want *CachePolicy
	}{
		{"absent", "{min_quality: 0.5}", nil},
		{"null", "{cache: null}", nil},
		{"empty", "{cache: {}}", &def},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustLoad(t, rule(tt.set), nil, nil).Config().Policies[0].Rules[0].Set.Cache
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("set.cache (-want +got):\n%s", diff)
			}
		})
	}
	t.Run("not a mapping", func(t *testing.T) {
		checkProblems(t, problemsOf(t, rule("{cache: on}"), nil, nil),
			errAt("policies[0].rules[0].set.cache", "expected a mapping"))
	})
}
