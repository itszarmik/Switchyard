package cache

import (
	"bytes"
	"container/list"
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryMaxBytes bounds the bytes an in-process cache holds, beyond its
// entry cap (cache.memory_max_entries): each of the memory store and the
// memory vector index evicts its least recently used entries to stay
// within it, so that entries of up to cache.max_entry_bytes cannot add up
// to more memory than a replica has. An entry counts its body, its names
// and its key or vector, plus a fixed overhead.
const MemoryMaxBytes = 256 << 20

// entryOverheadBytes is the fixed size an in-process cache counts for each
// entry, beyond its variable-length fields, against MemoryMaxBytes.
const entryOverheadBytes = 128

// Size returns the bytes e counts against MemoryMaxBytes: its body, its
// deployment, model and dropped parameters, and a fixed overhead.
func (e *Entry) Size() int64 {
	n := entryOverheadBytes + len(e.Body) + len(e.Deployment) + len(e.Model)
	for _, p := range e.DroppedParams {
		n += len(p)
	}
	return int64(n)
}

// MemoryStore is an in-process Store for storage.mode memory: an LRU of at
// most maxEntries entries and MemoryMaxBytes bytes, each entry with its own
// expiry. It is safe for concurrent use. Entries are lost on restart and
// not shared by replicas.
type MemoryStore struct {
	clock      func() time.Time
	maxEntries int
	// maxBytes is MemoryMaxBytes; tests lower it.
	maxBytes  int64
	evictions atomic.Int64

	mu    sync.Mutex
	order *list.List // of *memoryItem, most recently used first
	items map[string]*list.Element
	// bytes is the size of every item held.
	bytes int64
}

// memoryItem is one entry of a MemoryStore.
type memoryItem struct {
	key     string
	entry   Entry
	expires time.Time
	size    int64
}

// NewMemoryStore returns an empty MemoryStore that holds at most
// maxEntries entries (at least 1), and at most MemoryMaxBytes bytes, and
// reads the time from clock.
func NewMemoryStore(maxEntries int, clock func() time.Time) *MemoryStore {
	return &MemoryStore{
		clock:      clock,
		maxEntries: max(maxEntries, 1),
		maxBytes:   MemoryMaxBytes,
		order:      list.New(),
		items:      make(map[string]*list.Element),
	}
}

// Get returns a copy of the entry under key, or nil if there is none or it
// has expired; an expired entry is removed. A hit makes the entry the most
// recently used. The returned Body is shared and must not be modified.
func (s *MemoryStore) Get(_ context.Context, key string) (*Entry, error) {
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[key]
	if !ok {
		return nil, nil
	}
	it := el.Value.(*memoryItem) //nolint:errcheck // the list holds only *memoryItem
	if !now.Before(it.expires) {
		s.remove(el)
		return nil, nil
	}
	s.order.MoveToFront(el)
	e := it.entry
	return &e, nil
}

// Set stores a copy of e under key for ttl, replacing any entry there,
// and evicts the least recently used entries beyond the size caps, of
// entries and of bytes (counted by Evictions). A ttl that is not positive
// is an error.
func (s *MemoryStore) Set(_ context.Context, key string, e *Entry, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("cache: memory set: ttl %s is not positive", ttl)
	}
	it := &memoryItem{key: key, entry: *e, expires: s.clock().Add(ttl), size: e.Size() + int64(len(key))}
	it.entry.Body = bytes.Clone(e.Body)
	it.entry.DroppedParams = slices.Clone(e.DroppedParams)
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		s.bytes += it.size - el.Value.(*memoryItem).size //nolint:errcheck // the list holds only *memoryItem
		el.Value = it
		s.order.MoveToFront(el)
	} else {
		s.items[key] = s.order.PushFront(it)
		s.bytes += it.size
	}
	// The entry just stored stays, even alone over the byte cap: it is
	// at most cache.max_entry_bytes.
	for s.order.Len() > 1 && (s.order.Len() > s.maxEntries || s.bytes > s.maxBytes) {
		s.remove(s.order.Back())
		s.evictions.Add(1)
	}
	return nil
}

// remove deletes the element el. s.mu must be held.
func (s *MemoryStore) remove(el *list.Element) {
	it := s.order.Remove(el).(*memoryItem) //nolint:errcheck // the list holds only *memoryItem
	delete(s.items, it.key)
	s.bytes -= it.size
}

// Bytes returns the size of the entries held, expired ones not yet removed
// included, as counted against MemoryMaxBytes.
func (s *MemoryStore) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// Len returns the number of entries held, expired ones not yet removed
// included.
func (s *MemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len()
}

// Evictions returns how many entries the size cap has evicted, for the
// cache eviction metric. Expired entries are not counted.
func (s *MemoryStore) Evictions() int64 { return s.evictions.Load() }

// MemoryNamespaces holds namespace versions in process, for storage.mode
// memory. It is safe for concurrent use.
type MemoryNamespaces struct {
	mu       sync.Mutex
	versions map[string]int64
}

// NewMemoryNamespaces returns MemoryNamespaces with every version at 0.
func NewMemoryNamespaces() *MemoryNamespaces {
	return &MemoryNamespaces{versions: make(map[string]int64)}
}

// Version returns tenant's namespace version.
func (n *MemoryNamespaces) Version(_ context.Context, tenant string) (int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.versions[tenant], nil
}

// Bump increments tenant's namespace version and returns the new one.
func (n *MemoryNamespaces) Bump(_ context.Context, tenant string) (int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.versions[tenant]++
	return n.versions[tenant], nil
}

// Compile-time checks.
var (
	_ Store      = (*MemoryStore)(nil)
	_ Namespaces = (*MemoryNamespaces)(nil)
)
