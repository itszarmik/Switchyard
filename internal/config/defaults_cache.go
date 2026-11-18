package config

import "time"

// Response-cache defaults (DESIGN §11), documented on the fields in
// types.go.
const (
	// DefaultExactCacheTTL is cache.exact.ttl.
	DefaultExactCacheTTL = 24 * time.Hour
	// DefaultCacheScope is cache.exact.scope.
	DefaultCacheScope = CacheScopeTenant
	// DefaultSemanticMinSimilarity is cache.semantic.min_similarity.
	DefaultSemanticMinSimilarity = 0.95
	// DefaultSemanticCacheTTL is cache.semantic.ttl.
	DefaultSemanticCacheTTL = 72 * time.Hour
	// DefaultSemanticMaxChars is cache.semantic.max_chars.
	DefaultSemanticMaxChars = 2000
	// DefaultCacheLookupTimeout is the top-level cache.lookup_timeout.
	DefaultCacheLookupTimeout = 25 * time.Millisecond
	// DefaultCacheMaxEntryBytes is cache.max_entry_bytes.
	DefaultCacheMaxEntryBytes = 1 * mib
	// DefaultCacheMemoryMaxEntries is cache.memory_max_entries.
	DefaultCacheMemoryMaxEntries = 10_000
	// DefaultSemanticLookupTimeout is semantic_cache.lookup_timeout.
	DefaultSemanticLookupTimeout = 150 * time.Millisecond
)

// CachePolicyWithDefaults returns p with its zero-valued fields set to
// their defaults. Enabled and DeterministicOnly default to false, and no
// other field may be 0, so a zero value always takes the default. The
// policy engine applies it too, for configurations built by hand.
func CachePolicyWithDefaults(p CachePolicy) CachePolicy {
	setDefault(&p.Exact.TTL, Duration(DefaultExactCacheTTL))
	setDefault(&p.Exact.Scope, DefaultCacheScope)
	setDefault(&p.Semantic.MinSimilarity, DefaultSemanticMinSimilarity)
	setDefault(&p.Semantic.TTL, Duration(DefaultSemanticCacheTTL))
	setDefault(&p.Semantic.MaxChars, DefaultSemanticMaxChars)
	return p
}

// applyCacheDefaults fills every policy's cache block, every rule's
// set.cache block, cache and semantic_cache.
func applyCacheDefaults(c *Config) {
	for i := range c.Policies {
		p := &c.Policies[i]
		p.Cache = CachePolicyWithDefaults(p.Cache)
		for j := range p.Rules {
			if s := p.Rules[j].Set.Cache; s != nil {
				*s = CachePolicyWithDefaults(*s)
			}
		}
	}
	setDefault(&c.Cache.LookupTimeout, Duration(DefaultCacheLookupTimeout))
	setDefault(&c.Cache.MaxEntryBytes, DefaultCacheMaxEntryBytes)
	setDefault(&c.Cache.MemoryMaxEntries, DefaultCacheMemoryMaxEntries)
	setDefault(&c.SemanticCache.LookupTimeout, Duration(DefaultSemanticLookupTimeout))
}
