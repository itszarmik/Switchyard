package budget

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// flakyStore is a Store whose calls fail with err while it is set, and
// block until release is closed while hold is set. It counts its calls.
type flakyStore struct {
	Store

	mu      sync.Mutex
	err     error
	hold    bool
	calls   int
	started chan struct{} // receives once per held call
	release chan struct{}
}

func newFlakyStore() *flakyStore {
	return &flakyStore{Store: NewMemoryStore(), started: make(chan struct{}, 10), release: make(chan struct{})}
}

func (s *flakyStore) set(err error, hold bool) {
	s.mu.Lock()
	s.err, s.hold = err, hold
	s.mu.Unlock()
}

func (s *flakyStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *flakyStore) Spent(ctx context.Context, sc Scope, now time.Time) (int64, error) {
	s.mu.Lock()
	s.calls++
	err, hold := s.err, s.hold
	s.mu.Unlock()
	if hold {
		s.started <- struct{}{}
		<-s.release
	}
	if err != nil {
		return 0, err
	}
	return s.Store.Spent(ctx, sc, now)
}

var errDown = fmt.Errorf("budget spent: %w: connection refused", ErrUnavailable)

// TestCircuit checks that a failure marks the store unavailable for
// OpenFor, during which calls fail at once without reaching it, and that
// one probe then decides whether calls go through again.
func TestCircuit(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	clock := now
	var clockMu sync.Mutex
	advance := func(d time.Duration) {
		clockMu.Lock()
		clock = clock.Add(d)
		clockMu.Unlock()
	}
	s := newFlakyStore()
	c := NewCircuit(s, CircuitOptions{OpenFor: 5 * time.Second, Now: func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}})
	sc := Scope{Tenant: "t", Window: WindowDay, LimitMicro: 10, Block: true, Name: "t/day"}
	spent := func() error {
		_, err := c.Spent(context.Background(), sc, now)
		return err
	}

	if err := spent(); err != nil || !c.Available() {
		t.Fatalf("healthy store: %v, available %v", err, c.Available())
	}
	s.set(errDown, false)
	if err := spent(); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("failing store: %v", err)
	}
	if c.Available() {
		t.Error("still available after a failure")
	}
	// Within OpenFor, the store is not called, even once it is back.
	s.set(nil, false)
	calls := s.count()
	for range 3 {
		if err := spent(); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrCircuitOpen) {
			t.Errorf("while marked unavailable: %v", err)
		}
	}
	advance(5*time.Second - time.Nanosecond)
	if err := spent(); !errors.Is(err, ErrCircuitOpen) || s.count() != calls {
		t.Errorf("just before OpenFor: %v, %d calls", err, s.count()-calls)
	}

	// After OpenFor, one probe goes to the store; the others still fail
	// at once while it is in flight. It fails: another OpenFor.
	advance(time.Nanosecond)
	s.set(errDown, true)
	probe := make(chan error, 1)
	go func() { probe <- spent() }()
	<-s.started
	if err := spent(); !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("during the probe: %v", err)
	}
	s.set(errDown, false)
	s.release <- struct{}{}
	if err := <-probe; !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrCircuitOpen) {
		t.Errorf("failed probe: %v", err)
	}
	calls = s.count()
	if err := spent(); !errors.Is(err, ErrCircuitOpen) || s.count() != calls {
		t.Errorf("after a failed probe: %v", err)
	}

	// The next probe succeeds: calls go through again.
	advance(5 * time.Second)
	s.set(nil, false)
	if err := spent(); err != nil || !c.Available() {
		t.Errorf("successful probe: %v, available %v", err, c.Available())
	}
	if err := spent(); err != nil {
		t.Errorf("after a successful probe: %v", err)
	}
}

// TestCircuitOutcomes checks which outcomes mark the store unavailable: a
// store failure and a call that exceeds CallTimeout do; a refusal and the
// caller's own cancellation do not.
func TestCircuitOutcomes(t *testing.T) {
	sc := Scope{Tenant: "t", Window: WindowDay, LimitMicro: 10, Block: true, Name: "t/day"}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

	t.Run("refusal", func(t *testing.T) {
		c := NewCircuit(NewMemoryStore(), CircuitOptions{})
		if _, err := c.Admit(context.Background(), []Scope{sc}, 11, now); !errors.Is(err, ErrExhausted) {
			t.Fatalf("Admit = %v, want ErrExhausted", err)
		}
		if !c.Available() {
			t.Error("a refusal marked the store unavailable")
		}
		a, err := c.Admit(context.Background(), []Scope{sc}, 4, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Delta(context.Background(), &a, 2, now); err != nil || a.Charged() != 6 {
			t.Errorf("Delta = %v, charged %d", err, a.Charged())
		}
		if err := c.Settle(context.Background(), a, 1); err != nil {
			t.Fatal(err)
		}
		if n, err := c.Spent(context.Background(), sc, now); err != nil || n != 1 {
			t.Errorf("Spent = %d, %v; want 1", n, err)
		}
	})

	t.Run("caller canceled", func(t *testing.T) {
		s := newFlakyStore()
		s.set(nil, true)
		c := NewCircuit(s, CircuitOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			_, _ = c.Spent(ctx, sc, now)
			close(done)
		}()
		<-s.started
		cancel()
		s.release <- struct{}{}
		<-done
		if !c.Available() {
			t.Error("the caller's cancellation marked the store unavailable")
		}
	})

	t.Run("call timeout", func(t *testing.T) {
		c := NewCircuit(slowStore{NewMemoryStore()}, CircuitOptions{CallTimeout: 20 * time.Millisecond})
		start := time.Now()
		_, err := c.Spent(context.Background(), sc, now)
		if !errors.Is(err, ErrUnavailable) || time.Since(start) > 5*time.Second {
			t.Errorf("slow store: %v after %v, want ErrUnavailable after about the call timeout", err, time.Since(start))
		}
		if c.Available() {
			t.Error("a call that timed out left the store available")
		}
	})
}

// slowStore never answers Spent before its context ends, and then
// reports the context's error, as RedisStore does.
type slowStore struct{ Store }

func (slowStore) Spent(ctx context.Context, _ Scope, _ time.Time) (int64, error) {
	<-ctx.Done()
	return 0, fmt.Errorf("budget spent: %w", ctx.Err())
}

// softDownStore fails SoftCrossed as a store that is down.
type softDownStore struct{ Store }

func (softDownStore) SoftCrossed(context.Context, []Scope, time.Time) ([]State, error) {
	return nil, errDown
}

// TestCircuitSoftCrossed checks that SoftCrossed goes through the circuit:
// it answers from the store, and a failure marks the store unavailable.
func TestCircuitSoftCrossed(t *testing.T) {
	ctx := context.Background()
	s := scope("acme", WindowDay, 1000, true)
	s.SoftAtMicro = 100
	inner := NewMemoryStore()
	if _, err := inner.Admit(ctx, []Scope{s}, 150, noon); err != nil {
		t.Fatal(err)
	}
	c := NewCircuit(inner, CircuitOptions{})
	got, err := c.SoftCrossed(ctx, []Scope{s}, noon)
	if err != nil || len(got) != 1 || got[0] != (State{Spent: 150, SoftCrossed: true}) {
		t.Fatalf("SoftCrossed = %+v, %v", got, err)
	}
	if got, err := c.SoftCrossed(ctx, nil, noon); err != nil || got != nil {
		t.Errorf("SoftCrossed(no scopes) = %v, %v", got, err)
	}

	down := NewCircuit(softDownStore{Store: inner}, CircuitOptions{})
	if _, err := down.SoftCrossed(ctx, []Scope{s}, noon); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if down.Available() {
		t.Error("a failed SoftCrossed left the store available")
	}
	if _, err := down.SoftCrossed(ctx, []Scope{s}, noon); !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("err = %v, want ErrCircuitOpen while the store is marked unavailable", err)
	}
}
