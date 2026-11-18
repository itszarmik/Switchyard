package semantic

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"switchyard/internal/cache"
)

// searchCheckEvery is how many vectors Search compares between two checks
// of its context.
const searchCheckEvery = 256

// MemoryIndex is an in-process cache.VectorIndex, for storage.mode memory
// and tests. Search is a brute-force cosine comparison within the
// partition. Entries expire after their TTL, and beyond maxEntries in all,
// or beyond cache.MemoryMaxBytes of bodies and vectors, the least recently
// used entries (added, or returned by Search) are evicted.
//
// Searches share the index: each scans under a read lock, checking its
// context every searchCheckEvery vectors, and marks the entry it returns
// as used without a write lock. Eviction approximates LRU with that mark
// (the CLOCK algorithm): an entry at the tail that was used since it was
// last considered moves back to the head instead of going.
type MemoryIndex struct {
	max int
	// maxBytes is cache.MemoryMaxBytes; tests lower it.
	maxBytes  int64
	now       func() time.Time
	evictions atomic.Int64

	mu sync.RWMutex
	// lru holds every *memEntry, the most recently added or reconsidered
	// first.
	lru   *list.List
	parts map[string]map[*list.Element]struct{}
	// bytes is the size of every entry held.
	bytes int64
}

// memEntry is one stored entry.
type memEntry struct {
	partition string
	vec       []float32
	norm      float64
	entry     cache.Entry
	expires   time.Time
	size      int64
	// used is set when Search returns the entry, and cleared when
	// eviction passes over it.
	used atomic.Bool
}

var _ cache.VectorIndex = (*MemoryIndex)(nil)

// NewMemoryIndex returns an empty index holding at most maxEntries entries
// (at least 1) and cache.MemoryMaxBytes bytes. clock tells the time for
// TTLs; nil means time.Now.
func NewMemoryIndex(maxEntries int, clock func() time.Time) *MemoryIndex {
	if clock == nil {
		clock = time.Now
	}
	return &MemoryIndex{
		max:      max(maxEntries, 1),
		maxBytes: cache.MemoryMaxBytes,
		now:      clock,
		lru:      list.New(),
		parts:    make(map[string]map[*list.Element]struct{}),
	}
}

// Search returns the live entry of partition most similar to vec, or nil
// when the partition has none. It returns ctx's error once ctx is done,
// checking it every searchCheckEvery vectors. Expired entries it meets are
// removed after the scan. An entry whose vector length differs from vec's
// is an error: the index never compares vectors of different spaces. The
// returned entry's Body is shared with the index and must not be modified.
func (ix *MemoryIndex) Search(ctx context.Context, partition string, vec []float32) (*cache.Match, error) {
	if err := checkPartition(partition); err != nil {
		return nil, err
	}
	norm, err := checkVector(vec, 0)
	if err != nil {
		return nil, err
	}
	best, bestSim, expired, err := ix.scan(ctx, partition, vec, norm)
	if expired {
		ix.sweep(partition)
	}
	if err != nil || best == nil {
		return nil, err
	}
	best.used.Store(true)
	e := best.entry
	return &cache.Match{Entry: &e, Similarity: bestSim}, nil
}

// scan compares vec with every live entry of partition under the read
// lock, and reports whether it met expired ones.
func (ix *MemoryIndex) scan(ctx context.Context, partition string, vec []float32, norm float64) (best *memEntry, bestSim float64, expired bool, err error) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	now := ix.now()
	n := 0
	for el := range ix.parts[partition] {
		if n++; n%searchCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, expired, fmt.Errorf("semantic: memory search: %w", err)
			}
		}
		me := el.Value.(*memEntry) //nolint:errcheck // the list holds only *memEntry
		if !now.Before(me.expires) {
			expired = true
			continue
		}
		if len(me.vec) != len(vec) {
			return nil, 0, expired, fmt.Errorf("semantic: vector has %d dimensions, the partition holds %d", len(vec), len(me.vec))
		}
		var dot float64
		for i, f := range me.vec {
			dot += float64(f) * float64(vec[i])
		}
		if sim := dot / (me.norm * norm); best == nil || sim > bestSim {
			best, bestSim = me, sim
		}
	}
	return best, bestSim, expired, nil
}

// sweep removes the expired entries of partition.
func (ix *MemoryIndex) sweep(partition string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	now := ix.now()
	for el := range ix.parts[partition] {
		if me := el.Value.(*memEntry); !now.Before(me.expires) { //nolint:errcheck // the list holds only *memEntry
			ix.remove(el)
		}
	}
}

// Add stores a copy of e and vec in partition for ttl, evicting the least
// recently used entries (see evict) to stay within maxEntries and
// cache.MemoryMaxBytes.
func (ix *MemoryIndex) Add(_ context.Context, partition string, vec []float32, e *cache.Entry, ttl time.Duration) error {
	if err := checkPartition(partition); err != nil {
		return err
	}
	norm, err := checkVector(vec, 0)
	if err != nil {
		return err
	}
	if e == nil {
		return errors.New("semantic: nil entry")
	}
	if ttl <= 0 {
		return errors.New("semantic: ttl must be positive")
	}
	me := &memEntry{
		partition: partition,
		vec:       slices.Clone(vec),
		norm:      norm,
		entry:     *e,
		size:      e.Size() + int64(4*len(vec)+len(partition)),
	}
	me.entry.Body = bytes.Clone(e.Body)
	me.entry.DroppedParams = slices.Clone(e.DroppedParams)

	ix.mu.Lock()
	defer ix.mu.Unlock()
	now := ix.now()
	me.expires = now.Add(ttl)
	ix.evict(now, me.size)
	el := ix.lru.PushFront(me)
	ix.bytes += me.size
	set := ix.parts[partition]
	if set == nil {
		set = make(map[*list.Element]struct{})
		ix.parts[partition] = set
	}
	set[el] = struct{}{}
	return nil
}

// evict makes room for one more entry of size bytes. Each pass looks at
// the tail: an entry used since it was last considered, and not expired,
// moves to the head with its mark cleared; any other goes. Every entry is
// moved at most once, so it ends. The caller holds ix.mu.
func (ix *MemoryIndex) evict(now time.Time, size int64) {
	for ix.lru.Len() > 0 && (ix.lru.Len() >= ix.max || ix.bytes+size > ix.maxBytes) {
		el := ix.lru.Back()
		me := el.Value.(*memEntry) //nolint:errcheck // the list holds only *memEntry
		if me.used.Swap(false) && now.Before(me.expires) {
			ix.lru.MoveToFront(el)
			continue
		}
		ix.remove(el)
		ix.evictions.Add(1)
	}
}

// Len returns the number of entries held, expired ones not yet removed
// included.
func (ix *MemoryIndex) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.lru.Len()
}

// Bytes returns the size of the entries held, expired ones not yet removed
// included, as counted against cache.MemoryMaxBytes: each entry's
// cache.Entry.Size, its vector and its partition.
func (ix *MemoryIndex) Bytes() int64 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.bytes
}

// Evictions returns how many entries were evicted to stay within
// maxEntries or the byte cap (expiry is not counted, unless an expired
// entry was evicted to make room before it was swept).
func (ix *MemoryIndex) Evictions() int64 { return ix.evictions.Load() }

// remove deletes el. The caller holds ix.mu.
func (ix *MemoryIndex) remove(el *list.Element) {
	me := ix.lru.Remove(el).(*memEntry) //nolint:errcheck // the list holds only *memEntry
	ix.bytes -= me.size
	set := ix.parts[me.partition]
	delete(set, el)
	if len(set) == 0 {
		delete(ix.parts, me.partition)
	}
}
