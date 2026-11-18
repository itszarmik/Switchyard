package eval

import (
	"math/rand/v2"
	"sync"
	"testing"
)

// TestGateUnderConcurrency admits and settles from many goroutines and
// checks, after every admission, that spending plus reservations stays
// within the limit, and that the gate stops only when nothing fits.
func TestGateUnderConcurrency(t *testing.T) {
	limit, _ := nanoMoney(1_000_000) // $0.001
	g := newGate(limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var violations int
	for w := range 16 {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 1))
			for {
				est, _ := nanoMoney(int64(1 + rng.IntN(50_000)))
				if !g.admit(est) {
					return
				}
				g.mu.Lock()
				if g.spent.Add(g.reserved).Cmp(g.limit) > 0 {
					mu.Lock()
					violations++
					mu.Unlock()
				}
				g.mu.Unlock()
				actual, _ := nanoMoney(int64(rng.IntN(int(est.NanoUSDCeil()) + 1)))
				g.settle(est, actual)
			}
		})
	}
	wg.Wait()
	spent, stopped := g.totals()
	if violations > 0 || !stopped || spent.Cmp(limit) > 0 {
		t.Errorf("%d violations, stopped %v, spent %s of %s", violations, stopped, spent, limit)
	}
	if g.inflight != 0 || !g.reserved.IsZero() {
		t.Errorf("%d in flight, %s reserved after the run", g.inflight, g.reserved)
	}
}
