package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"switchyard/internal/config"
)

// Defaults of CircuitOptions.
const (
	// DefaultCircuitOpenFor is how long a Circuit skips its limiter after
	// a failure.
	DefaultCircuitOpenFor = 5 * time.Second
	// DefaultCircuitCallTimeout bounds each call a Circuit makes to its
	// limiter. Every request waits for the rate limit, so it is short.
	DefaultCircuitCallTimeout = 250 * time.Millisecond
)

// ErrCircuitOpen is wrapped, together with ErrUnavailable, by the errors a
// Circuit returns without calling its limiter, while the limiter is marked
// unavailable after a failure.
var ErrCircuitOpen = errors.New("rate-limit store marked unavailable after a failure")

// errCircuitOpen is the error of a call a Circuit skips.
var errCircuitOpen = fmt.Errorf("ratelimit: %w: %w", ErrUnavailable, ErrCircuitOpen)

// CircuitOptions configures NewCircuit. Zero values take the defaults.
type CircuitOptions struct {
	// OpenFor is how long the limiter is skipped after a failure
	// (DefaultCircuitOpenFor).
	OpenFor time.Duration
	// CallTimeout bounds each call to the limiter
	// (DefaultCircuitCallTimeout); a call that takes longer fails with
	// ErrUnavailable.
	CallTimeout time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Circuit is a Limiter that stops calling a failing limiter for a while,
// so that requests do not each wait for a store that is down (DESIGN
// §14.7). It works like budget.Circuit.
//
// Each call is bounded by CallTimeout. When a call fails with
// ErrUnavailable, the limiter is marked unavailable for OpenFor: calls
// fail at once with an error wrapping ErrUnavailable and ErrCircuitOpen.
// After that, one call probes the limiter while the others still fail at
// once; if it succeeds, calls go through again, else the limiter is marked
// unavailable for another OpenFor. A refusal is a success: the store
// answered. A call whose own context ends is neither.
//
// The Circuit only reports failures; the caller applies
// rate_limits.fail_mode (see Enforcer), so that a reload can change it.
// Releases and lease heartbeats go straight to the wrapped limiter. A
// stream lease that Redis took for a call the Circuit then timed out has
// no holder, and frees its slot when it expires.
//
// It is safe for concurrent use.
type Circuit struct {
	limiter Limiter
	openFor time.Duration
	timeout time.Duration
	now     func() time.Time

	mu        sync.Mutex
	openUntil time.Time // zero while the limiter is available
	probing   bool      // a probe is in flight
}

var _ Limiter = (*Circuit)(nil)

// NewCircuit wraps l in a Circuit.
func NewCircuit(l Limiter, o CircuitOptions) *Circuit {
	c := &Circuit{limiter: l, openFor: o.OpenFor, timeout: o.CallTimeout, now: o.Now}
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

// Available reports whether calls go to the limiter: false while it is
// marked unavailable after a failure.
func (c *Circuit) Available() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.openUntil.IsZero()
}

// Allow implements Limiter.
func (c *Circuit) Allow(ctx context.Context, keyID string, lim config.RateLimit) (bool, time.Duration, error) {
	if lim.RequestsPerMinute <= 0 {
		return true, 0, nil
	}
	var (
		ok   bool
		wait time.Duration
	)
	err := c.call(ctx, "allow", func(ctx context.Context) error {
		var err error
		ok, wait, err = c.limiter.Allow(ctx, keyID, lim)
		return err
	})
	return ok, wait, err
}

// AcquireStream implements Limiter.
func (c *Circuit) AcquireStream(ctx context.Context, keyID string, maxStreams int) (func(), bool, error) {
	if maxStreams <= 0 {
		return noRelease, true, nil
	}
	var (
		release func()
		ok      bool
	)
	err := c.call(ctx, "acquire", func(ctx context.Context) error {
		var err error
		release, ok, err = c.limiter.AcquireStream(ctx, keyID, maxStreams)
		return err
	})
	return release, ok, err
}

// call runs f against the limiter, unless it is marked unavailable, and
// records its outcome.
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
		// The limiter reports its own context's end as such; here it is
		// the store being too slow.
		err = fmt.Errorf("ratelimit %s: %w: no answer within %v", op, ErrUnavailable, c.timeout)
	}
	c.record(probe, err)
	return err
}

// allow reports whether a call may go to the limiter, and whether it is
// the probe after the limiter was marked unavailable.
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
	case err == nil:
		c.openUntil = time.Time{}
	}
}
