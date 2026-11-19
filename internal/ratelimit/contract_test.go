package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"

	"switchyard/internal/config"
)

// t0 is the start time of every fake clock.
var t0 = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

// fakeClock is a clock that moves only when told to.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: t0} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// harness is a Limiter under test and a way to move its clock.
type harness struct {
	l       Limiter
	advance func(d time.Duration)
}

// step is one call in a GCRA scenario: advance the clock by after, then
// call Allow, expecting ok and, when refused, wait.
type step struct {
	after time.Duration
	ok    bool
	wait  time.Duration
}

// allowed and refused build steps.
func allowed(after time.Duration) step       { return step{after: after, ok: true} }
func refused(after, wait time.Duration) step { return step{after: after, wait: wait} }
func times(n int, after time.Duration, s step) []step { // n copies of s, the first after after
	out := make([]step, n)
	for i := range out {
		out[i] = s
	}
	out[0].after = after
	return out
}

func concat(parts ...[]step) []step {
	var out []step
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// testContract runs the Limiter contract against the limiters newHarness
// returns, a fresh one per subtest.
func testContract(t *testing.T, newHarness func(t *testing.T) harness) {
	ctx := context.Background()
	perSecond := config.RateLimit{RequestsPerMinute: 60, Burst: 2} // T = 1 s, τ = 2 s

	gcraTests := []struct {
		name  string
		lim   config.RateLimit
		steps []step
	}{
		{
			name: "burst then retry after one interval",
			lim:  perSecond,
			steps: concat(
				times(3, 0, allowed(0)), // b+1 at once
				[]step{
					refused(0, time.Second),
					refused(400*time.Millisecond, 600*time.Millisecond),
					allowed(600 * time.Millisecond),
					refused(0, time.Second),
				},
			),
		},
		{
			name: "steady rate at the interval is always allowed",
			lim:  perSecond,
			steps: concat(
				times(3, 0, allowed(0)),
				times(20, time.Second, allowed(time.Second)),
				[]step{refused(0, time.Second)},
			),
		},
		{
			name: "idle time restores the burst, never more",
			lim:  perSecond,
			steps: concat(
				times(3, 0, allowed(0)),
				[]step{refused(0, time.Second)},
				times(3, time.Hour, allowed(0)),
				[]step{refused(0, time.Second)},
			),
		},
		{
			name: "no burst spaces requests strictly",
			lim:  config.RateLimit{RequestsPerMinute: 120},
			steps: []step{
				allowed(0),
				refused(0, 500*time.Millisecond),
				refused(499*time.Millisecond, time.Millisecond),
				allowed(time.Millisecond),
			},
		},
		{
			name: "a rate that does not divide a minute is rounded to a slower one",
			lim:  config.RateLimit{RequestsPerMinute: 7}, // 8.571428571 s, rounded up to the µs
			steps: []step{
				allowed(0),
				refused(0, 8571429*time.Microsecond),
				allowed(8571429 * time.Microsecond),
			},
		},
		{
			name:  "no rate allows everything",
			lim:   config.RateLimit{MaxConcurrentStreams: 1},
			steps: times(100, 0, allowed(0)),
		},
	}
	for _, tt := range gcraTests {
		t.Run("gcra/"+tt.name, func(t *testing.T) {
			h := newHarness(t)
			for i, s := range tt.steps {
				h.advance(s.after)
				ok, wait, err := h.l.Allow(ctx, "k1", tt.lim)
				if err != nil {
					t.Fatalf("step %d: Allow: %v", i, err)
				}
				if ok != s.ok || wait != s.wait {
					t.Fatalf("step %d: Allow = %v, %v; want %v, %v", i, ok, wait, s.ok, s.wait)
				}
			}
		})
	}

	t.Run("gcra/keys are independent", func(t *testing.T) {
		h := newHarness(t)
		lim := config.RateLimit{RequestsPerMinute: 60}
		for _, key := range []string{"k1", "k2", "k:3", "k%3A3"} {
			if ok, _, err := h.l.Allow(ctx, key, lim); !ok || err != nil {
				t.Errorf("first request of %s: %v, %v", key, ok, err)
			}
		}
		if ok, _, _ := h.l.Allow(ctx, "k1", lim); ok {
			t.Error("second request of k1 allowed")
		}
	})

	t.Run("gcra/a new limit applies at once", func(t *testing.T) {
		h := newHarness(t)
		slow := config.RateLimit{RequestsPerMinute: 1}
		if ok, _, _ := h.l.Allow(ctx, "k1", slow); !ok {
			t.Fatal("first request refused")
		}
		if ok, wait, _ := h.l.Allow(ctx, "k1", slow); ok || wait != time.Minute {
			t.Fatalf("under 1/min: %v, %v", ok, wait)
		}
		// Raising the limit (a reload) lets the next request through as
		// soon as the arrival time passes under the new tolerance.
		fast := config.RateLimit{RequestsPerMinute: 60, Burst: 60}
		if ok, wait, _ := h.l.Allow(ctx, "k1", fast); !ok {
			t.Fatalf("under the raised limit: refused, wait %v", wait)
		}
	})

	t.Run("gcra/a raised limit applies within one interval after a long debt", func(t *testing.T) {
		h := newHarness(t)
		slow := config.RateLimit{RequestsPerMinute: 1, Burst: 1}
		for range 2 {
			if ok, _, _ := h.l.Allow(ctx, "k1", slow); !ok {
				t.Fatal("request within the burst refused")
			}
		}
		// The arrival time is 2 min ahead: far beyond the new limit's
		// tolerance + interval (10 s + 10 ms), which caps it.
		fast := config.RateLimit{RequestsPerMinute: 6000, Burst: 1000}
		if ok, wait, _ := h.l.Allow(ctx, "k1", fast); ok || wait != 10*time.Millisecond {
			t.Fatalf("at once under the raised limit: %v, %v; want refused for one interval", ok, wait)
		}
		h.advance(10 * time.Millisecond)
		if ok, wait, _ := h.l.Allow(ctx, "k1", fast); !ok {
			t.Fatalf("one interval later: refused, wait %v", wait)
		}
	})

	t.Run("gcra/concurrent requests never exceed the burst", func(t *testing.T) {
		h := newHarness(t)
		lim := config.RateLimit{RequestsPerMinute: 60, Burst: 4}
		var wg sync.WaitGroup
		var mu sync.Mutex
		ok := 0
		for range 40 {
			wg.Go(func() {
				allowed, _, err := h.l.Allow(ctx, "k1", lim)
				if err != nil {
					t.Errorf("Allow: %v", err)
				}
				if allowed {
					mu.Lock()
					ok++
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		if ok != 5 {
			t.Errorf("%d of 40 concurrent requests allowed, want 5", ok)
		}
	})

	t.Run("streams/concurrent acquires never exceed the cap", func(t *testing.T) {
		h := newHarness(t)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var releases []func()
		for range 40 {
			wg.Go(func() {
				release, ok, err := h.l.AcquireStream(ctx, "k1", 3)
				if err != nil {
					t.Errorf("AcquireStream: %v", err)
				}
				if ok {
					mu.Lock()
					releases = append(releases, release)
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		if len(releases) != 3 {
			t.Errorf("%d of 40 concurrent streams acquired, want 3", len(releases))
		}
		for _, release := range releases {
			release()
		}
	})

	t.Run("streams/cap and release", func(t *testing.T) {
		h := newHarness(t)
		acquire := func(key string, maxStreams int) (func(), bool) {
			t.Helper()
			release, ok, err := h.l.AcquireStream(ctx, key, maxStreams)
			if err != nil {
				t.Fatalf("AcquireStream(%s, %d): %v", key, maxStreams, err)
			}
			if ok != (release != nil) {
				t.Fatalf("AcquireStream(%s, %d): ok %v but release nil is %v", key, maxStreams, ok, release == nil)
			}
			return release, ok
		}
		a, ok1 := acquire("k1", 2)
		b, ok2 := acquire("k1", 2)
		if !ok1 || !ok2 {
			t.Fatal("first two streams refused")
		}
		if _, ok := acquire("k1", 2); ok {
			t.Fatal("third stream allowed under a cap of 2")
		}
		if other, ok := acquire("k2", 2); !ok {
			t.Fatal("another key's stream refused")
		} else {
			other()
		}
		a()
		c, ok := acquire("k1", 2)
		if !ok {
			t.Fatal("stream refused after a release")
		}
		// Releasing a twice frees no second slot.
		a()
		if _, ok := acquire("k1", 2); ok {
			t.Fatal("a double release freed a second slot")
		}
		// A lower cap (a reload) applies at once; a higher one too.
		if _, ok := acquire("k1", 1); ok {
			t.Fatal("stream allowed over a lowered cap")
		}
		d, ok := acquire("k1", 3)
		if !ok {
			t.Fatal("stream refused under a raised cap")
		}
		b()
		c()
		d()
		for range 3 {
			release, ok := acquire("k1", 3)
			if !ok {
				t.Fatal("stream refused after everything was released")
			}
			defer release()
		}
	})

	t.Run("streams/no cap acquires nothing", func(t *testing.T) {
		h := newHarness(t)
		for range 5 {
			release, ok, err := h.l.AcquireStream(ctx, "k1", 0)
			if !ok || err != nil || release == nil {
				t.Fatalf("AcquireStream with no cap: %v, %v, nil release %v", ok, err, release == nil)
			}
		}
		if _, ok, _ := h.l.AcquireStream(ctx, "k1", 1); !ok {
			t.Error("the uncapped streams took a slot")
		}
	})
}

func TestMemoryContract(t *testing.T) {
	testContract(t, func(t *testing.T) harness {
		c := newFakeClock()
		return harness{l: NewMemory(c.Now), advance: c.Advance}
	})
}

func TestCircuitContract(t *testing.T) {
	testContract(t, func(t *testing.T) harness {
		c := newFakeClock()
		return harness{l: NewCircuit(NewMemory(c.Now), CircuitOptions{Now: c.Now}), advance: c.Advance}
	})
}
