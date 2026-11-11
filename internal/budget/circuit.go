package budget

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Defaults of CircuitOptions.
const (
	// DefaultCircuitOpenFor is how long a Circuit skips its store after a
	// failure.
	DefaultCircuitOpenFor = 5 * time.Second
	// DefaultCircuitCallTimeout bounds each call a Circuit makes to its
	// store.
	DefaultCircuitCallTimeout = time.Second
)

// ErrCircuitOpen is wrapped, together with ErrUnavailable, by the errors a
// Circuit returns without calling its store, while the store is marked
// unavailable after a failure.
var ErrCircuitOpen = errors.New("budget store marked unavailable after a failure")

// errCircuitOpen is the error of a call a Circuit skips.
var errCircuitOpen = fmt.Errorf("budget: %w: %w", ErrUnavailable, ErrCircuitOpen)

// CircuitOptions configures NewCircuit. Zero values take the defaults.
type CircuitOptions struct {
	// OpenFor is how long the store is skipped after a failure
	// (DefaultCircuitOpenFor).
	OpenFor time.Duration
	// CallTimeout bounds each call to the store
	// (DefaultCircuitCallTimeout); a call that takes longer fails with
	// ErrUnavailable.
	CallTimeout time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Circuit is a Store that stops calling a failing store for a while, so
// that requests do not each wait for a store that is down (DESIGN §14.7).
//
// Each call to the store is bounded by CallTimeout. When a call fails with
// ErrUnavailable, the store is marked unavailable for OpenFor: calls fail
// at once with an error wrapping ErrUnavailable and ErrCircuitOpen. After
// that, one call probes the store, while the others still fail at once;
// if it succeeds, calls go through again, else the store is marked
// unavailable for another OpenFor. A refusal (ErrExhausted) is a success:
// the store answered. A call whose own context ends is neither.
//
// It is safe for concurrent use.
type Circuit struct {
	store   Store
	openFor time.Duration
	timeout time.Duration
	now     func() time.Time

	mu        sync.Mutex
	openUntil time.Time // zero while the store is available
	probing   bool      // a probe is in flight
}

var _ Store = (*Circuit)(nil)

// NewCircuit wraps store in a Circuit.
func NewCircuit(store Store, o CircuitOptions) *Circuit {
	c := &Circuit{store: store, openFor: o.OpenFor, timeout: o.CallTimeout, now: o.Now}
	if c.openFor <= 0 {
		c.openFor = DefaultCircuitOpenFor
	}
	if c.timeout <= 0 {
		c.timeout = DefaultCircuitCallTimeout
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// Available reports whether calls go to the store: false while it is
// marked unavailable after a failure.
func (c *Circuit) Available() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.openUntil.IsZero()
}

// Admit implements Store.
func (c *Circuit) Admit(ctx context.Context, scopes []Scope, est int64, now time.Time) (Admission, error) {
	var a Admission
	err := c.call(ctx, "admit", func(ctx context.Context) error {
		var err error
		a, err = c.store.Admit(ctx, scopes, est, now)
		return err
	})
	return a, err
}

// Settle implements Store.
func (c *Circuit) Settle(ctx context.Context, a Admission, actual int64) error {
	return c.call(ctx, "settle", func(ctx context.Context) error { return c.store.Settle(ctx, a, actual) })
}

// Delta implements Store.
func (c *Circuit) Delta(ctx context.Context, a *Admission, extra int64, now time.Time) error {
	return c.call(ctx, "delta", func(ctx context.Context) error { return c.store.Delta(ctx, a, extra, now) })
}

// Spent implements Store.
func (c *Circuit) Spent(ctx context.Context, s Scope, now time.Time) (int64, error) {
	var spent int64
	err := c.call(ctx, "spent", func(ctx context.Context) error {
		var err error
		spent, err = c.store.Spent(ctx, s, now)
		return err
	})
	return spent, err
}

// SoftCrossed implements Store.
func (c *Circuit) SoftCrossed(ctx context.Context, scopes []Scope, now time.Time) ([]State, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	var out []State
	err := c.call(ctx, "soft", func(ctx context.Context) error {
		var err error
		out, err = c.store.SoftCrossed(ctx, scopes, now)
		return err
	})
	return out, err
}

// call runs f against the store, unless the store is marked unavailable,
// and records its outcome.
func (c *Circuit) call(ctx context.Context, op string, f func(ctx context.Context) error) error {
	probe, ok := c.allow()
	if !ok {
		return errCircuitOpen
	}
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	err := f(cctx)
	timedOut := errors.Is(cctx.Err(), context.DeadlineExceeded)
	cancel()
	if err != nil && timedOut && ctx.Err() == nil && !errors.Is(err, ErrUnavailable) {
		// The store reports its own context's end as such; here it is the
		// store being too slow.
		err = fmt.Errorf("budget %s: %w: no answer within %v", op, ErrUnavailable, c.timeout)
	}
	c.record(probe, err)
	return err
}

// allow reports whether a call may go to the store, and whether it is the
// probe after the store was marked unavailable.
func (c *Circuit) allow() (probe, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.openUntil.IsZero():
		return false, true
	case c.probing || c.now().Before(c.openUntil):
		return false, false
	}
	c.probing = true
	return true, true
}

// record updates the state with the outcome err of a call.
func (c *Circuit) record(probe bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if probe {
		c.probing = false
	}
	switch {
	case errors.Is(err, ErrUnavailable):
		c.openUntil = c.now().Add(c.openFor)
	case err == nil || errors.Is(err, ErrExhausted):
		c.openUntil = time.Time{}
	}
}
