package loadkit

import (
	"context"
	"sync"
	"time"
)

// OpenLoop sends requests at a constant rate, whatever the responses do:
// request i is due at start + i/rate, and fire runs for it in a goroutine
// of its own at that time, or at once when the generator is behind. Unlike
// a closed loop of workers, a slow server does not slow the arrivals, so
// queueing shows up in the latencies instead of hiding in a lower
// throughput.
//
// It fires every request due before end, stops early when ctx is done,
// waits for every fire to return, and returns how many it fired.
func OpenLoop(ctx context.Context, rate float64, start, end time.Time, fire func(i int, due time.Time)) int {
	interval := time.Duration(float64(time.Second) / rate)
	timer := time.NewTimer(0)
	defer timer.Stop()
	<-timer.C
	var wg sync.WaitGroup
	n := 0
	for ; ; n++ {
		due := start.Add(time.Duration(n) * interval)
		if !due.Before(end) {
			break
		}
		if d := time.Until(due); d > 0 {
			timer.Reset(d)
			select {
			case <-timer.C:
			case <-ctx.Done():
				wg.Wait()
				return n
			}
		} else if ctx.Err() != nil {
			break
		}
		i := n
		wg.Go(func() { fire(i, due) })
	}
	wg.Wait()
	return n
}
