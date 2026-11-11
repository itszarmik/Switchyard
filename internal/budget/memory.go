package budget

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// sweepEvery is how often, in the time passed to Admit, the memory store
// deletes counters of windows that ended more than counterGrace ago.
const sweepEvery = time.Hour

// MemoryStore is a Store held in process memory, guarded by a mutex. It
// suits a single replica and tests; its counters are lost on restart.
type MemoryStore struct {
	mu        sync.Mutex
	counters  map[counterKey]*counter
	lastSweep time.Time
}

// counterKey identifies the counter of one scope and window.
type counterKey struct {
	name  string
	start time.Time
}

// counter is the spend of one scope in one window.
type counter struct {
	spent       int64
	softCrossed bool
	expires     time.Time
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() Store {
	return &MemoryStore{counters: make(map[counterKey]*counter)}
}

// Admit implements Store.
func (m *MemoryStore) Admit(_ context.Context, scopes []Scope, est int64, now time.Time) (Admission, error) {
	if len(scopes) == 0 {
		return Admission{}, nil
	}
	windows, err := checkArgs(scopes, est, now)
	if err != nil {
		return Admission{}, err
	}
	a := Admission{scopes: slices.Clone(scopes), windows: windows}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweep(now)
	if err := m.charge(&a, est); err != nil {
		return Admission{}, err
	}
	return a, nil
}

// Delta implements Store.
func (m *MemoryStore) Delta(_ context.Context, a *Admission, extra int64, _ time.Time) error {
	if extra <= 0 || len(a.scopes) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.charge(a, extra)
}

// charge applies Admit's rule for amount to a's scopes and windows, and on
// success adds amount to a.charged and records what it found in a. The
// caller holds m.mu.
func (m *MemoryStore) charge(a *Admission, amount int64) error {
	var full []int
	for i, s := range a.scopes {
		if s.Block && m.get(s, a.windows[i]).spent+amount > s.LimitMicro {
			full = append(full, i)
		}
	}
	if len(full) > 0 {
		return exhausted(a.scopes, a.windows, full)
	}
	var exceeded, crossed []Scope
	for i, s := range a.scopes {
		c := m.get(s, a.windows[i])
		c.spent += amount
		if !s.Block && c.spent > s.LimitMicro {
			exceeded = append(exceeded, s)
		}
		if s.SoftAtMicro > 0 && c.spent >= s.SoftAtMicro && !c.softCrossed {
			c.softCrossed = true
			crossed = append(crossed, s)
		}
	}
	a.charged += amount
	a.Exceeded, a.SoftCrossed = exceeded, crossed
	return nil
}

// Settle implements Store.
func (m *MemoryStore) Settle(_ context.Context, a Admission, actual int64) error {
	if actual < 0 {
		return fmt.Errorf("budget: negative actual cost %d", actual)
	}
	diff := actual - a.charged
	if diff == 0 || len(a.scopes) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, s := range a.scopes {
		m.get(s, a.windows[i]).spent += diff
	}
	return nil
}

// Spent implements Store.
func (m *MemoryStore) Spent(_ context.Context, s Scope, now time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.counters[counterKey{s.Name, windowStart(s.Window, now)}]; ok {
		return c.spent, nil
	}
	return 0, nil
}

// get returns the counter of s in the window starting at start, creating
// it if needed. The caller holds m.mu.
func (m *MemoryStore) get(s Scope, start time.Time) *counter {
	k := counterKey{s.Name, start}
	c, ok := m.counters[k]
	if !ok {
		c = &counter{expires: expiry(s.Window, start)}
		m.counters[k] = c
	}
	return c
}

// sweep deletes expired counters, at most once per sweepEvery of now. The
// caller holds m.mu.
func (m *MemoryStore) sweep(now time.Time) {
	if now.Sub(m.lastSweep) < sweepEvery {
		return
	}
	m.lastSweep = now
	for k, c := range m.counters {
		if now.After(c.expires) {
			delete(m.counters, k)
		}
	}
}
