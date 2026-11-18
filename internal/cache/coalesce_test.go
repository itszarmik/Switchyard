package cache

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
)

// waitFollowers blocks until n followers have joined the call in flight
// for key, so that releasing the leader cannot race their arrival.
func waitFollowers(t *testing.T, c *Coalescer, key string, n int) {
	t.Helper()
	for {
		c.mu.Lock()
		f := c.calls[key]
		joined := f != nil && f.waiters >= n
		c.mu.Unlock()
		if joined {
			return
		}
		if f == nil {
			t.Fatalf("no call in flight for %q", key)
		}
		runtime.Gosched()
	}
}

func TestCoalescerShares(t *testing.T) {
	const followers = 99
	var c Coalescer
	var calls atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{})
	want := &Entry{Body: []byte("answer"), Deployment: "m@p", Model: "m"}
	fn := func(context.Context) (*Entry, bool) {
		calls.Add(1)
		close(entered)
		<-release
		return want, true
	}

	ctx := context.Background()
	type result struct {
		e          *Entry
		shared, ok bool
	}
	leader := make(chan result, 1)
	go func() {
		e, shared, ok := c.Do(ctx, "k", fn)
		leader <- result{e, shared, ok}
	}()
	<-entered // the leader is in fn, so every later caller follows

	results := make(chan result, followers)
	for range followers {
		go func() {
			e, shared, ok := c.Do(ctx, "k", fn)
			results <- result{e, shared, ok}
		}()
	}
	waitFollowers(t, &c, "k", followers)
	close(release)

	if r := <-leader; r.e != want || r.shared || !r.ok {
		t.Errorf("leader got %+v, want its own entry, not shared", r)
	}
	for range followers {
		if r := <-results; r.e != want || !r.shared || !r.ok {
			t.Errorf("follower got %+v, want the shared entry", r)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("fn ran %d times, want 1", n)
	}
	if len(c.calls) != 0 {
		t.Errorf("%d calls left in flight", len(c.calls))
	}
}

func TestCoalescerLeaderNotCacheable(t *testing.T) {
	tests := []struct {
		name  string
		entry *Entry
		ok    bool
	}{
		{"uncacheable", &Entry{Body: []byte("filtered")}, false},
		{"failure", nil, false},
		{"ok without an entry", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c Coalescer
			release := make(chan struct{})
			entered := make(chan struct{})
			leaderDone := make(chan bool, 1)
			go func() {
				_, shared, ok := c.Do(context.Background(), "k", func(context.Context) (*Entry, bool) {
					close(entered)
					<-release
					return tt.entry, tt.ok
				})
				leaderDone <- ok && !shared
			}()
			<-entered
			follower := make(chan bool, 1)
			go func() {
				e, shared, ok := c.Do(context.Background(), "k", func(context.Context) (*Entry, bool) {
					t.Error("the follower ran fn while the leader was in flight")
					return nil, false
				})
				follower <- e == nil && !shared && !ok
			}()
			waitFollowers(t, &c, "k", 1)
			close(release)
			if got := <-leaderDone; got != tt.ok {
				t.Errorf("leader ok = %v, want %v", got, tt.ok)
			}
			if !<-follower {
				t.Error("the follower did not get ok false")
			}
		})
	}
}

func TestCoalescerFollowerCancel(t *testing.T) {
	var c Coalescer
	release := make(chan struct{})
	entered := make(chan struct{})
	want := &Entry{Body: []byte("x")}
	leader := make(chan *Entry, 1)
	go func() {
		e, _, _ := c.Do(context.Background(), "k", func(context.Context) (*Entry, bool) {
			close(entered)
			<-release
			return want, true
		})
		leader <- e
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan bool, 1)
	go func() {
		_, shared, ok := c.Do(ctx, "k", nil)
		canceled <- !shared && !ok
	}()
	stays := make(chan bool, 1)
	go func() {
		e, shared, ok := c.Do(context.Background(), "k", nil)
		stays <- e == want && shared && ok
	}()
	waitFollowers(t, &c, "k", 2)
	cancel()
	if !<-canceled {
		t.Error("the canceled follower did not return ok false")
	}
	close(release)
	if <-leader != want {
		t.Error("the leader lost its entry")
	}
	if !<-stays {
		t.Error("the other follower did not get the shared entry")
	}
}

func TestCoalescerPanic(t *testing.T) {
	var c Coalescer
	release := make(chan struct{})
	entered := make(chan struct{})
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		c.Do(context.Background(), "k", func(context.Context) (*Entry, bool) {
			close(entered)
			<-release
			panic("boom")
		})
	}()
	<-entered
	follower := make(chan bool, 1)
	go func() {
		_, _, ok := c.Do(context.Background(), "k", nil)
		follower <- ok
	}()
	waitFollowers(t, &c, "k", 1)
	close(release)
	if p := <-panicked; p != "boom" {
		t.Errorf("leader recovered %v, want the panic to propagate", p)
	}
	if <-follower {
		t.Error("the follower got ok true after the leader panicked")
	}
	// The key is free: the next call leads and runs fn.
	e, shared, ok := c.Do(context.Background(), "k", func(context.Context) (*Entry, bool) {
		return &Entry{Body: []byte("again")}, true
	})
	if e == nil || string(e.Body) != "again" || shared || !ok {
		t.Errorf("the call after a panic got %v, %v, %v", e, shared, ok)
	}
}

func TestCoalescerKeysIndependent(t *testing.T) {
	var c Coalescer
	release := make(chan struct{})
	entered := make(chan struct{})
	go c.Do(context.Background(), "a", func(context.Context) (*Entry, bool) {
		close(entered)
		<-release
		return nil, false
	})
	<-entered
	defer close(release)
	e, shared, ok := c.Do(context.Background(), "b", func(context.Context) (*Entry, bool) {
		return &Entry{Body: []byte("b")}, true
	})
	if e == nil || shared || !ok {
		t.Errorf("a call on another key waited or shared: %v %v %v", e, shared, ok)
	}
}
