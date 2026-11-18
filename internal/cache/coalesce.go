package cache

import (
	"context"
	"sync"
)

// Coalescer shares one upstream call among identical concurrent misses
// (DESIGN §11.2): an in-process singleflight keyed by the exact key, for
// non-streaming requests. The zero value is ready to use, and it is safe
// for concurrent use.
type Coalescer struct {
	mu    sync.Mutex
	calls map[string]*flight
}

// flight is one leader's call that followers wait on.
type flight struct {
	done  chan struct{}
	entry *Entry
	ok    bool
	// waiters counts the followers that joined, under Coalescer.mu.
	waiters int
}

// Do runs fn for key unless a call for key is already in flight.
//
// The caller that finds no call in flight is the leader: it runs fn with
// its own ctx and returns fn's result with shared false. fn reports
// whether its entry is cacheable, and so may be shared.
//
// Any other caller is a follower and waits for the leader or for its own
// ctx to end. When the leader's fn returns a cacheable entry, every
// follower gets that entry with shared and ok true. When fn returns
// ok false (an uncacheable response, a failure, the leader's cancellation)
// or panics, followers get ok false and serve the request themselves. A
// follower whose ctx ends returns at once with ok false; the leader and the
// other followers are unaffected.
//
// A panic in fn propagates to the leader's caller after the followers are
// released, and the key is free again for the next call.
func (c *Coalescer) Do(ctx context.Context, key string, fn func(ctx context.Context) (*Entry, bool)) (e *Entry, shared bool, ok bool) {
	c.mu.Lock()
	if f, inFlight := c.calls[key]; inFlight {
		f.waiters++
		c.mu.Unlock()
		select {
		case <-f.done:
			if f.ok {
				return f.entry, true, true
			}
			return nil, false, false
		case <-ctx.Done():
			return nil, false, false
		}
	}
	if c.calls == nil {
		c.calls = make(map[string]*flight)
	}
	f := &flight{done: make(chan struct{})}
	c.calls[key] = f
	c.mu.Unlock()

	defer func() {
		// Followers read f only after done is closed, so these writes,
		// made before, are visible to them. A panic leaves f.ok false.
		c.mu.Lock()
		delete(c.calls, key)
		c.mu.Unlock()
		close(f.done)
	}()
	entry, cacheable := fn(ctx)
	f.entry, f.ok = entry, cacheable && entry != nil
	return entry, false, cacheable
}

// Followers returns how many callers are waiting on calls in flight, over
// every key: a readiness signal for tests that need followers to have
// joined before the leader's call returns.
func (c *Coalescer) Followers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, f := range c.calls {
		n += f.waiters
	}
	return n
}
