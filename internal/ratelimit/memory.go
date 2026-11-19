package ratelimit

import (
	"context"
	"sync"
	"time"

	"switchyard/internal/config"
)

// MemoryLimiter is a Limiter in the memory of one process, for
// storage.mode memory and tests. Every replica has its own, so limits are
// per replica.
//
// It keeps one GCRA arrival time per key that has made a request and one
// stream count per key with a stream open. Key IDs come from config, so
// the state is bounded by the number of keys ever configured. Its stream
// slots need no expiry: they live and die with the process that holds
// them.
type MemoryLimiter struct {
	now func() time.Time

	mu      sync.Mutex
	tats    map[string]time.Time
	streams map[string]int
}

var _ Limiter = (*MemoryLimiter)(nil)

// NewMemory returns an empty MemoryLimiter on the clock now; nil means
// time.Now.
func NewMemory(now func() time.Time) *MemoryLimiter {
	if now == nil {
		now = time.Now
	}
	return &MemoryLimiter{now: now, tats: make(map[string]time.Time), streams: make(map[string]int)}
}

// Allow implements Limiter. It never fails.
func (m *MemoryLimiter) Allow(_ context.Context, keyID string, lim config.RateLimit) (bool, time.Duration, error) {
	if lim.RequestsPerMinute <= 0 {
		return true, 0, nil
	}
	interval, tolerance := gcra(lim)
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	tat := m.tats[keyID]
	if tat.Before(now) {
		tat = now
	}
	// A TAT no request under lim can leave was left by a slower limit
	// before a reload (see gcraScript).
	if limit := now.Add(tolerance + interval); tat.After(limit) {
		tat = limit
		m.tats[keyID] = tat
	}
	if wait := tat.Sub(now) - tolerance; wait > 0 {
		return false, wait, nil
	}
	m.tats[keyID] = tat.Add(interval)
	return true, 0, nil
}

// AcquireStream implements Limiter. It never fails.
func (m *MemoryLimiter) AcquireStream(_ context.Context, keyID string, maxStreams int) (func(), bool, error) {
	if maxStreams <= 0 {
		return noRelease, true, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.streams[keyID] >= maxStreams {
		return nil, false, nil
	}
	m.streams[keyID]++
	var once sync.Once
	return func() { once.Do(func() { m.release(keyID) }) }, true, nil
}

// release gives back one of keyID's stream slots.
func (m *MemoryLimiter) release(keyID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := m.streams[keyID] - 1; n > 0 {
		m.streams[keyID] = n
	} else {
		delete(m.streams, keyID)
	}
}
