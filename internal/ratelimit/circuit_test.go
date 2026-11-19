package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"switchyard/internal/config"
)

// flakyLimiter is a Limiter whose calls fail with err while it is set, and
// block until release receives while hold is set. It counts its calls.
type flakyLimiter struct {
	Limiter

	mu      sync.Mutex
	err     error
	hold    bool
	calls   int
	started chan struct{} // receives once per held call
	release chan struct{}
}

func newFlakyLimiter() *flakyLimiter {
	return &flakyLimiter{
		Limiter: NewMemory(newFakeClock().Now),
		started: make(chan struct{}, 10), release: make(chan struct{}),
	}
}

func (f *flakyLimiter) set(err error, hold bool) {
	f.mu.Lock()
	f.err, f.hold = err, hold
	f.mu.Unlock()
}

func (f *flakyLimiter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// enter records a call and returns its error, after blocking if held.
func (f *flakyLimiter) enter(ctx context.Context) error {
	f.mu.Lock()
	f.calls++
	err, hold := f.err, f.hold
	f.mu.Unlock()
	if hold {
		f.started <- struct{}{}
		select {
		case <-f.release:
		case <-ctx.Done():
			return fmt.Errorf("ratelimit allow: %w", ctx.Err())
		}
	}
	return err
}

func (f *flakyLimiter) Allow(ctx context.Context, keyID string, lim config.RateLimit) (bool, time.Duration, error) {
	if err := f.enter(ctx); err != nil {
		return false, 0, err
	}
	return f.Limiter.Allow(ctx, keyID, lim)
}

func (f *flakyLimiter) AcquireStream(ctx context.Context, keyID string, maxStreams int) (func(), bool, error) {
	if err := f.enter(ctx); err != nil {
		return nil, false, err
	}
	return f.Limiter.AcquireStream(ctx, keyID, maxStreams)
}

var errDown = fmt.Errorf("ratelimit allow: %w: connection refused", ErrUnavailable)

// TestCircuit checks that a failure marks the limiter unavailable for
// OpenFor, during which calls fail at once without reaching it, and that
// one probe then decides whether calls go through again.
func TestCircuit(t *testing.T) {
	clock := newFakeClock()
	f := newFlakyLimiter()
	c := NewCircuit(f, CircuitOptions{OpenFor: 5 * time.Second, Now: clock.Now})
	lim := config.RateLimit{RequestsPerMinute: 1_000_000, Burst: 1_000_000}
	allow := func() error {
		_, _, err := c.Allow(context.Background(), "k1", lim)
		return err
	}

	if err := allow(); err != nil || !c.Available() {
		t.Fatalf("healthy limiter: %v, available %v", err, c.Available())
	}
	f.set(errDown, false)
	if err := allow(); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("failing limiter: %v", err)
	}
	if c.Available() {
		t.Error("still available after a failure")
	}
	// Within OpenFor, the limiter is not called, even once it is back,
	// by either method.
	f.set(nil, false)
	calls := f.count()
	for range 3 {
		if err := allow(); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrCircuitOpen) {
			t.Errorf("while marked unavailable: %v", err)
		}
	}
	if release, _, err := c.AcquireStream(context.Background(), "k1", 1); !errors.Is(err, ErrCircuitOpen) || release != nil {
		t.Errorf("AcquireStream while marked unavailable: %v", err)
	}
	clock.Advance(5*time.Second - time.Nanosecond)
	if err := allow(); !errors.Is(err, ErrCircuitOpen) || f.count() != calls {
		t.Errorf("just before OpenFor: %v, %d calls", err, f.count()-calls)
	}

	// After OpenFor, one probe goes to the limiter; the others still fail
	// at once while it is in flight. It fails: another OpenFor.
	clock.Advance(time.Nanosecond)
	f.set(errDown, true)
	probe := make(chan error, 1)
	go func() { probe <- allow() }()
	<-f.started
	if err := allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("during the probe: %v", err)
	}
	f.set(errDown, false)
	f.release <- struct{}{}
	if err := <-probe; !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrCircuitOpen) {
		t.Errorf("failed probe: %v", err)
	}
	calls = f.count()
	if err := allow(); !errors.Is(err, ErrCircuitOpen) || f.count() != calls {
		t.Errorf("after a failed probe: %v", err)
	}

	// The next probe succeeds: calls go through again.
	clock.Advance(5 * time.Second)
	f.set(nil, false)
	if err := allow(); err != nil {
		t.Fatalf("successful probe: %v", err)
	}
	if !c.Available() {
		t.Error("unavailable after a successful probe")
	}
	release, ok, err := c.AcquireStream(context.Background(), "k1", 1)
	if !ok || err != nil {
		t.Fatalf("AcquireStream after recovery: %v, %v", ok, err)
	}
	release()
}

// TestCircuitOutcomes checks which outcomes mark the limiter unavailable.
func TestCircuitOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		used      bool // the key's one request was made before
		err       error
		hold      bool
		timeout   time.Duration
		cancel    bool // the caller's context ends during the call
		wantErr   error
		available bool
	}{
		{name: "refusal is a success", used: true, available: true},
		{name: "store failure", err: errDown, wantErr: ErrUnavailable},
		{name: "slow store", hold: true, timeout: time.Millisecond, wantErr: ErrUnavailable},
		{name: "caller gone", hold: true, cancel: true, wantErr: context.Canceled, available: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFlakyLimiter()
			timeout := tt.timeout
			if timeout == 0 {
				timeout = time.Hour // never, so only the cancellation ends a held call
			}
			c := NewCircuit(f, CircuitOptions{CallTimeout: timeout})
			lim := config.RateLimit{RequestsPerMinute: 1}
			if tt.used {
				_, _, _ = c.Allow(context.Background(), "k1", lim)
			}
			f.set(tt.err, tt.hold)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				go func() {
					<-f.started
					cancel()
				}()
			}
			ok, _, err := c.Allow(ctx, "k1", lim)
			switch {
			case tt.wantErr == nil && (err != nil || ok):
				t.Errorf("Allow = %v, %v; want a refusal", ok, err)
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Errorf("Allow error = %v, want %v", err, tt.wantErr)
			}
			if c.Available() != tt.available {
				t.Errorf("Available = %v, want %v", c.Available(), tt.available)
			}
		})
	}
}

// TestCircuitSkipsUnlimited checks that a call without a limit never
// reaches the limiter, so it neither probes nor fails.
func TestCircuitSkipsUnlimited(t *testing.T) {
	f := newFlakyLimiter()
	f.set(errDown, false)
	c := NewCircuit(f, CircuitOptions{})
	if ok, _, err := c.Allow(context.Background(), "k1", config.RateLimit{MaxConcurrentStreams: 2}); !ok || err != nil {
		t.Errorf("Allow without a rate: %v, %v", ok, err)
	}
	if release, ok, err := c.AcquireStream(context.Background(), "k1", 0); !ok || err != nil || release == nil {
		t.Errorf("AcquireStream without a cap: %v, %v", ok, err)
	}
	if f.count() != 0 || !c.Available() {
		t.Errorf("%d calls, available %v", f.count(), c.Available())
	}
}
