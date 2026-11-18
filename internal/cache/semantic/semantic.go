// Package semantic implements the semantic response cache (DESIGN §11.3):
// the embedding client, the vector indexes (the Redis query engine, and an
// in-process index for storage.mode memory and tests) and the lookup that
// ties them together under one time budget.
//
// An entry matches only within its partition, the hash of everything but
// the last user message (cache.Partition), and only when the cosine
// similarity of the last user messages reaches min_similarity. Nothing
// here logs prompt or response content, and as everywhere in the cache,
// every failure is a miss.
package semantic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"switchyard/internal/cache"
)

// ErrTimeout is wrapped by a Result's Err when the lookup timeout ran out,
// as opposed to the caller's own context ending or a failure.
var ErrTimeout = errors.New("semantic cache lookup timed out")

// Options configure a Cache.
type Options struct {
	// LookupTimeout bounds each Lookup, the embedding and the search
	// together (semantic_cache.lookup_timeout). Zero leaves only the
	// caller's context.
	LookupTimeout time.Duration
	// Clock stamps an entry's Created when Store is given none; nil means
	// time.Now.
	Clock func() time.Time
}

// Cache is the semantic cache: an embedder and a vector index. It is safe
// for concurrent use.
type Cache struct {
	emb     cache.Embedder
	idx     cache.VectorIndex
	timeout time.Duration
	now     func() time.Time
}

// New returns the semantic cache of emb and idx. idx must be the index of
// emb's model and dimensions (see NewRedisIndex).
func New(emb cache.Embedder, idx cache.VectorIndex, opts Options) *Cache {
	now := opts.Clock
	if now == nil {
		now = time.Now
	}
	return &Cache{emb: emb, idx: idx, timeout: opts.LookupTimeout, now: now}
}

// Result is the outcome of a Lookup.
type Result struct {
	// Entry is the cached response, nil unless Similarity >=
	// minSimilarity.
	Entry *cache.Entry
	// Similarity is the best match's similarity, hit or not, for the
	// similarity histograms; NaN when there was no candidate or the
	// lookup failed.
	Similarity float64
	// Vector is text's embedding, for Store; nil if embedding failed.
	Vector []float32
	// EmbedCostNano is the embedding's cost in nano-USD, charged as
	// overhead whether the lookup hit, missed or failed; -1 when unknown.
	EmbedCostNano int64
	// Err is why the lookup failed (it wraps ErrTimeout when the lookup
	// timeout ran out); a failure is a miss.
	Err error
}

// Lookup embeds text and searches partition for its nearest entry, all
// within the lookup timeout. It is a hit when the similarity is at least
// minSimilarity.
func (c *Cache) Lookup(ctx context.Context, partition, text string, minSimilarity float64) Result {
	res := Result{Similarity: math.NaN()}
	lctx := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		lctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	fail := func(what string, err error) Result {
		if ctx.Err() == nil && errors.Is(lctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%w: %s: %w", ErrTimeout, what, err)
		} else {
			err = fmt.Errorf("semantic cache: %s: %w", what, err)
		}
		res.Err = err
		return res
	}

	emb, err := c.emb.Embed(lctx, text)
	res.EmbedCostNano = emb.CostNano
	if err != nil {
		return fail("embedding", err)
	}
	res.Vector = emb.Vector
	m, err := c.idx.Search(lctx, partition, emb.Vector)
	if err != nil {
		return fail("search", err)
	}
	if m == nil || m.Entry == nil {
		return res
	}
	res.Similarity = m.Similarity
	if m.Similarity >= minSimilarity {
		res.Entry = m.Entry
	}
	return res
}

// Store adds e to partition for ttl under vec, a Lookup's Vector, so the
// text is not embedded twice. e's Created is stamped with the clock when
// zero. ctx alone bounds the write: the gateway stores asynchronously,
// after the response, with a deadline of its own.
func (c *Cache) Store(ctx context.Context, partition string, vec []float32, e *cache.Entry, ttl time.Duration) error {
	if vec == nil {
		return errors.New("semantic cache: store: no vector")
	}
	if e == nil {
		return errors.New("semantic cache: store: nil entry")
	}
	if e.Created.IsZero() {
		stamped := *e
		stamped.Created = c.now()
		e = &stamped
	}
	if err := c.idx.Add(ctx, partition, vec, e, ttl); err != nil {
		return fmt.Errorf("semantic cache: store: %w", err)
	}
	return nil
}
