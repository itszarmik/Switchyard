package config

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Bounds of the cache settings (DESIGN §11).
const (
	maxCacheTTL                = 30 * 24 * time.Hour
	maxSemanticMaxChars        = 100_000
	maxCacheLookupTimeout      = time.Second
	maxSemanticLookupTimeout   = 5 * time.Second
	minCacheMaxEntryBytes      = kib
	maxCacheMaxEntryBytes      = 16 * mib
	pathSemanticEmbeddingModel = "semantic_cache.embedding_model"
)

// validateCache checks every policy's cache block, every rule's set.cache
// block, cache and semantic_cache. Only governance rules may set
// set.cache: a quality rule's match depends on the request's content, and
// caching is a governance decision. Enabling the semantic cache anywhere
// requires semantic_cache.embedding_model, which must name an embedding
// model in the catalog, or a model@provider deployment of one, with
// dimensions. storage.redis.cache_url, which a cache in redis mode needs,
// is already required whenever storage.mode is redis (validateStorage);
// with the semantic cache enabled it must name database 0, the only one
// the Redis query engine indexes.
func (st *loadState) validateCache(c *Config, models map[string]*Model) {
	semanticAt := ""
	for i := range c.Policies {
		p := &c.Policies[i]
		base := indexPath("policies", i)
		st.checkCachePolicy(base+".cache", &p.Cache)
		if p.Cache.Semantic.Enabled && semanticAt == "" {
			semanticAt = base + ".cache"
		}
		for j := range p.Rules {
			r := &p.Rules[j]
			if r.Set.Cache == nil {
				continue
			}
			sp := indexPath(base+".rules", j) + ".set.cache"
			if IsQualityRule(&r.Match) {
				st.errorAt(sp, msgQualityRuleSet)
				continue
			}
			st.checkCachePolicy(sp, r.Set.Cache)
			if r.Set.Cache.Semantic.Enabled && semanticAt == "" {
				semanticAt = sp
			}
		}
	}

	st.checkDurationRange("cache.lookup_timeout", c.Cache.LookupTimeout, maxCacheLookupTimeout)
	st.checkIntRange("cache.max_entry_bytes", c.Cache.MaxEntryBytes, minCacheMaxEntryBytes, maxCacheMaxEntryBytes)
	if c.Cache.MemoryMaxEntries < 1 {
		st.errorAt("cache.memory_max_entries", "must be at least 1, got "+strconv.Itoa(c.Cache.MemoryMaxEntries))
	}
	st.checkDurationRange("semantic_cache.lookup_timeout", c.SemanticCache.LookupTimeout, maxSemanticLookupTimeout)

	ref := c.SemanticCache.EmbeddingModel
	switch {
	case ref != "":
		if msg := embeddingModelProblem(ref, st.describe(pathSemanticEmbeddingModel, ref), models); msg != "" {
			st.errorAt(pathSemanticEmbeddingModel, msg)
		}
	case semanticAt != "":
		st.errorAt(pathSemanticEmbeddingModel, "required when the semantic cache is enabled ("+semanticAt+".semantic.enabled)")
	}
	if semanticAt != "" && c.Storage.Mode == "redis" && !st.hasProblemAt(pathCacheURL) {
		if db, ok := redisDB(c.Storage.Redis.CacheURL.Value()); ok && db != 0 {
			st.errorAt(pathCacheURL, "must name database 0 (redis://host:port/0, with no db parameter naming another), not database "+
				strconv.Itoa(db)+", since the semantic cache is enabled ("+semanticAt+
				".semantic.enabled) and the Redis query engine indexes only database 0; put the state instance's database elsewhere")
		}
	}
}

// pathCacheURL is the cache instance's URL setting.
const pathCacheURL = "storage.redis.cache_url"

// redisDB returns the database a Redis URL selects, as the client reads it
// (redis.ParseURL, which honors a db query parameter over the path), and
// false when it does not parse (validateStorage reports that). It never
// returns the URL or the parser's error, which may quote a password.
func redisDB(raw string) (int, bool) {
	opts, err := redis.ParseURL(raw)
	if err != nil {
		return 0, false
	}
	return opts.DB, true
}

// checkCachePolicy validates a cache block, with its defaults applied, at
// path.
func (st *loadState) checkCachePolicy(path string, p *CachePolicy) {
	st.checkOneOf(path+".exact.scope", p.Exact.Scope, CacheScopeTenant, CacheScopeKey, CacheScopeUser)
	st.checkDurationRange(path+".exact.ttl", p.Exact.TTL, maxCacheTTL)
	if v := p.Semantic.MinSimilarity; !(v > 0 && v <= 1) { // NaN fails too
		st.errorAt(path+".semantic.min_similarity", "must be greater than 0 and at most 1, got "+strconv.FormatFloat(v, 'g', -1, 64))
	}
	st.checkDurationRange(path+".semantic.ttl", p.Semantic.TTL, maxCacheTTL)
	st.checkIntRange(path+".semantic.max_chars", p.Semantic.MaxChars, 1, maxSemanticMaxChars)
}

// embeddingModelProblem returns why ref, an embedding model id or
// model@provider, does not name an embedding deployment with dimensions in
// the catalog, or "" when it does. quoted is ref as messages may show it.
func embeddingModelProblem(ref, quoted string, models map[string]*Model) string {
	id, provider, pinned := strings.Cut(ref, "@")
	m := models[id]
	switch {
	case m == nil:
		msg := "unknown model " + quoted + "; use an embedding model id or model@provider"
		if s := suggest(id, overrideModelIDs(models)); s != "" && mayQuote(ref) {
			msg += fmt.Sprintf(" (did you mean %q?)", s)
		}
		return msg
	case m.Type != ModelTypeEmbedding:
		return quoted + " is not an embedding model (type: embedding)"
	case pinned && !slices.ContainsFunc(m.Deployments, func(d Deployment) bool { return d.Provider == provider }):
		return quoted + " names a provider that does not serve that model"
	case m.Dimensions <= 0:
		return "the embedding model " + quoted + " has no dimensions; the vector index needs them"
	}
	return ""
}
