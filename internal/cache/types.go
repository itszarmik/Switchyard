// Package cache implements the exact and semantic response caches (DESIGN
// §11): canonical key derivation, entry stores, per-tenant namespace
// versions, the semantic partition and vector index, stream replay and
// aggregation, and in-process coalescing of identical misses.
//
// The cache is the only place Switchyard stores prompt and response
// content. It is opt-in per policy, bounded by TTLs, scoped to the tenant
// and purgeable. Nothing in this package logs content.
//
// Every cache failure is a miss: callers treat an error from any method
// here as "not cached" and serve the request upstream.
package cache

import (
	"context"
	"time"
)

// Cache statuses, as sent in the X-Switchyard-Cache response header and
// recorded in usage events.
const (
	StatusMiss     = "miss"
	StatusExact    = "exact"
	StatusSemantic = "semantic"
)

// Cache layers, as metric labels.
const (
	LayerExact    = "exact"
	LayerSemantic = "semantic"
)

// Entry is a cached chat completion.
type Entry struct {
	// Body is the chat.completion JSON, in its non-streaming form: the
	// upstream's response body, or for a streamed miss the aggregated
	// response (see Aggregator).
	Body []byte
	// Deployment (model@provider) and Model (the catalog model ID)
	// produced the response.
	Deployment, Model string
	// DroppedParams lists the request parameters the serving attempt
	// removed because the deployment does not support them, as the miss
	// reported them in X-Switchyard-Dropped-Params; a hit reports them
	// again, since the cached body reflects their absence. Nil when none
	// were dropped.
	DroppedParams []string
	// Created is when the entry was stored.
	Created time.Time
}

// Store holds exact-cache entries by key. Implementations must be safe for
// concurrent use.
type Store interface {
	// Get returns the entry stored under key, or nil and no error on a
	// miss (absent or expired).
	Get(ctx context.Context, key string) (*Entry, error)
	// Set stores e under key for ttl, replacing any entry there.
	Set(ctx context.Context, key string, e *Entry, ttl time.Duration) error
}

// Namespaces holds each tenant's namespace version, which is part of every
// cache key and semantic partition of the tenant. Bumping it invalidates
// all of the tenant's entries at once. Versions live with the budget state
// (never evicted), not with the evictable cache: an evicted version would
// resurrect purged entries.
type Namespaces interface {
	// Version returns the tenant's current version; 0 for a tenant never
	// purged.
	Version(ctx context.Context, tenant string) (int64, error)
	// Bump increments the tenant's version and returns the new one.
	Bump(ctx context.Context, tenant string) (int64, error)
}

// Match is the nearest semantic entry to a query vector.
type Match struct {
	Entry *Entry
	// Similarity is the cosine similarity of the query and the entry's
	// vector, in [-1, 1].
	Similarity float64
}

// VectorIndex holds semantic-cache entries, each with the embedding of its
// last user message, in partitions that must match exactly. Implementations
// must be safe for concurrent use.
type VectorIndex interface {
	// Search returns the entry in partition nearest to vec, or nil and no
	// error when the partition has none. It never applies a threshold:
	// callers compare Similarity with min_similarity, and export it for
	// tuning either way.
	Search(ctx context.Context, partition string, vec []float32) (*Match, error)
	// Add stores e with vec in partition for ttl.
	Add(ctx context.Context, partition string, vec []float32, e *Entry, ttl time.Duration) error
}

// Embedding is the result of one embedding call.
type Embedding struct {
	// Vector is the embedding.
	Vector []float32
	// Tokens is the input token count the upstream reported, or an
	// estimate when it reported none.
	Tokens int64
	// CostNano is the call's cost in nano-USD from catalog prices, or -1
	// when unknown.
	CostNano int64
}

// Embedder embeds text for the semantic cache. Implementations must be
// safe for concurrent use.
type Embedder interface {
	// Embed returns the embedding of text. A failed call that may have
	// been billed still reports its estimated cost in the returned
	// Embedding, alongside the error.
	Embed(ctx context.Context, text string) (Embedding, error)
	// Model names the embedding deployment (model@provider), and
	// Dimensions its vector size; the vector index is named after both,
	// so changing the model starts a new index.
	Model() string
	Dimensions() int
}
