package usage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeStore is a batchStore that records batches and can fail or block.
type fakeStore struct {
	mu      sync.Mutex
	fails   []error // returned by the next calls, in order
	always  error   // returned once fails is used up, if set
	gate    chan struct{}
	started chan struct{} // receives on every call, if set
	batches chan []*Event
	calls   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{batches: make(chan []*Event, 10_000)}
}

func (s *fakeStore) WriteBatch(ctx context.Context, events []*Event) error {
	s.mu.Lock()
	s.calls++
	var err error
	if len(s.fails) > 0 {
		err, s.fails = s.fails[0], s.fails[1:]
	} else {
		err = s.always
	}
	gate, started := s.gate, s.started
	s.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	s.batches <- slices.Clone(events)
	return nil
}

// next waits for the next written batch.
func (s *fakeStore) next(t *testing.T) []*Event {
	t.Helper()
	select {
	case b := <-s.batches:
		return b
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a batch")
		return nil
	}
}

// none checks that no batch is written within a short wait.
func (s *fakeStore) none(t *testing.T) {
	t.Helper()
	select {
	case b := <-s.batches:
		t.Fatalf("unexpected batch of %d events", len(b))
	case <-time.After(50 * time.Millisecond):
	}
}

func ids(events []*Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.ID
	}
	return out
}

func ev(id string) *Event { return &Event{ID: id, Tenant: "acme", Time: day0} }

// testWriter starts a writer whose flush ticker never fires unless the test
// sends on the returned channel.
func testWriter(t *testing.T, store batchStore, cfg writerConfig) (*writer, chan time.Time) {
	t.Helper()
	tick := make(chan time.Time)
	cfg.tick = tick
	if cfg.metrics == nil {
		cfg.metrics = unregisteredWriterMetrics()
	}
	w := newWriter(store, cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Close(ctx)
	})
	return w, tick
}

func TestWriterBatching(t *testing.T) {
	store := newFakeStore()
	w, _ := testWriter(t, store, writerConfig{batchSize: 3})
	for i := range 7 {
		w.Record(ev(fmt.Sprint(i)))
	}
	// Two full batches are written at once; the seventh event waits.
	if diff := cmp.Diff([]string{"0", "1", "2"}, ids(store.next(t))); diff != "" {
		t.Errorf("batch 1 (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"3", "4", "5"}, ids(store.next(t))); diff != "" {
		t.Errorf("batch 2 (-want +got):\n%s", diff)
	}
	store.none(t)
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"6"}, ids(store.next(t))); diff != "" {
		t.Errorf("final batch (-want +got):\n%s", diff)
	}
	if got := testutil.ToFloat64(w.m.written); got != 7 {
		t.Errorf("written = %v, want 7", got)
	}
	if got := testutil.ToFloat64(w.m.depth); got != 0 {
		t.Errorf("depth after Close = %v, want 0", got)
	}
}

func TestWriterFlushInterval(t *testing.T) {
	t.Run("manual tick", func(t *testing.T) {
		store := newFakeStore()
		w, tick := testWriter(t, store, writerConfig{batchSize: 100})
		w.Record(ev("a"))
		w.Record(ev("b"))
		store.none(t)
		tick <- time.Now()
		if diff := cmp.Diff([]string{"a", "b"}, ids(store.next(t))); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
		// A tick with nothing queued writes nothing.
		tick <- time.Now()
		store.none(t)
	})
	t.Run("partial flush splits at batch size", func(t *testing.T) {
		store := newFakeStore()
		w, tick := testWriter(t, store, writerConfig{batchSize: 2})
		store.mu.Lock()
		store.gate = make(chan struct{}) // hold the first full batch
		store.started = make(chan struct{}, 10)
		store.mu.Unlock()
		for _, id := range []string{"a", "b", "c", "d", "e"} {
			w.Record(ev(id))
		}
		<-store.started
		store.mu.Lock()
		gate := store.gate
		store.gate, store.started = nil, nil
		store.mu.Unlock()
		close(gate)
		var got [][]string
		got = append(got, ids(store.next(t)), ids(store.next(t)))
		tick <- time.Now()
		got = append(got, ids(store.next(t)))
		if diff := cmp.Diff([][]string{{"a", "b"}, {"c", "d"}, {"e"}}, got); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
	})
	t.Run("real ticker", func(t *testing.T) {
		store := newFakeStore()
		w := newWriter(store, writerConfig{batchSize: 100, flushInterval: 10 * time.Millisecond})
		defer func() { _ = w.Close(context.Background()) }()
		w.Record(ev("a"))
		if diff := cmp.Diff([]string{"a"}, ids(store.next(t))); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
	})
}

func TestWriterDropsOldestOnOverflow(t *testing.T) {
	store := newFakeStore()
	store.gate = make(chan struct{})
	store.started = make(chan struct{}, 10)
	var logs bytes.Buffer
	w, _ := testWriter(t, store, writerConfig{
		batchSize: 1, queueSize: 3,
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	w.Record(ev("e1"))
	<-store.started // e1 is in flight; the store holds it
	for _, id := range []string{"e2", "e3", "e4", "e5", "e6"} {
		w.Record(ev(id))
	}
	// The queue holds 3: e2 and e3 were dropped for e5 and e6.
	if got := testutil.ToFloat64(w.m.dropped); got != 2 {
		t.Errorf("dropped = %v, want 2", got)
	}
	if got := testutil.ToFloat64(w.m.depth); got != 4 {
		t.Errorf("depth = %v, want 4 (3 queued + 1 in flight)", got)
	}
	store.mu.Lock()
	store.started = nil
	store.mu.Unlock()
	close(store.gate)
	var got []string
	for range 4 {
		got = append(got, ids(store.next(t))...)
	}
	if diff := cmp.Diff([]string{"e1", "e4", "e5", "e6"}, got); diff != "" {
		t.Errorf("written (-want +got):\n%s", diff)
	}
	if n := strings.Count(logs.String(), "dropping the oldest events"); n != 1 {
		t.Errorf("logged the overflow %d times, want once:\n%s", n, logs.String())
	}
}

func TestWriterRetriesWithBackoff(t *testing.T) {
	store := newFakeStore()
	boom := errors.New("connection refused")
	store.fails = []error{boom, boom, boom, boom, boom}
	var mu sync.Mutex
	var delays []time.Duration
	after := func(d time.Duration) <-chan time.Time {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		c := make(chan time.Time, 1)
		c <- time.Now()
		return c
	}
	var logs bytes.Buffer
	w, tick := testWriter(t, store, writerConfig{
		batchSize: 10, retryMin: 10 * time.Millisecond, retryMax: 50 * time.Millisecond, after: after,
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	secret := &Event{ID: "id-1", Time: day0, Tenant: "tenant-secret", RequestedModel: "model-secret", Tags: map[string]string{"team": "tag-secret"}}
	w.Record(secret)
	w.Record(ev("id-2"))
	tick <- time.Now()
	if diff := cmp.Diff([]string{"id-1", "id-2"}, ids(store.next(t))); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	mu.Lock()
	got := slices.Clone(delays)
	mu.Unlock()
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("backoff delays (-want +got):\n%s", diff)
	}
	if got := testutil.ToFloat64(w.m.writeErrors); got != 5 {
		t.Errorf("write errors = %v, want 5", got)
	}
	if got := testutil.ToFloat64(w.m.written); got != 2 {
		t.Errorf("written = %v, want 2", got)
	}
	if got := testutil.ToFloat64(w.m.dropped); got != 0 {
		t.Errorf("dropped = %v, want 0", got)
	}
	for _, s := range []string{"tenant-secret", "model-secret", "tag-secret"} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("logs contain event contents %q:\n%s", s, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "retrying") {
		t.Errorf("no retry warning logged:\n%s", logs.String())
	}
}

func TestWriterDropsPermanentlyRejectedBatch(t *testing.T) {
	store := newFakeStore()
	store.fails = []error{&permanentError{err: errors.New("usage: write events: postgres error SQLSTATE 22003")}}
	store.started = make(chan struct{}, 10)
	w, tick := testWriter(t, store, writerConfig{batchSize: 10})
	w.Record(ev("bad"))
	tick <- time.Now()
	<-store.started // "bad" is in flight alone
	w.Record(ev("good"))
	tick <- time.Now()
	if diff := cmp.Diff([]string{"good"}, ids(store.next(t))); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	if got := testutil.ToFloat64(w.m.dropped); got != 1 {
		t.Errorf("dropped = %v, want 1", got)
	}
	store.mu.Lock()
	calls := store.calls
	store.mu.Unlock()
	if calls != 2 {
		t.Errorf("store called %d times, want 2 (no retry of a permanent error)", calls)
	}
}

func TestWriterCloseFlushes(t *testing.T) {
	store := newFakeStore()
	w, _ := testWriter(t, store, writerConfig{batchSize: 100})
	for i := range 5 {
		w.Record(ev(fmt.Sprint(i)))
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if diff := cmp.Diff([]string{"0", "1", "2", "3", "4"}, ids(store.next(t))); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	w.Record(ev("late"))
	if got := testutil.ToFloat64(w.m.dropped); got != 1 {
		t.Errorf("dropped = %v, want 1 for the event recorded after Close", got)
	}
	// Close is idempotent.
	if err := w.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestWriterCloseRetriesThenGivesUp(t *testing.T) {
	t.Run("store recovers during Close", func(t *testing.T) {
		store := newFakeStore()
		boom := errors.New("down")
		store.fails = []error{boom, boom}
		w, _ := testWriter(t, store, writerConfig{batchSize: 100, retryMin: time.Millisecond, retryMax: time.Millisecond})
		w.Record(ev("a"))
		if err := w.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if diff := cmp.Diff([]string{"a"}, ids(store.next(t))); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
	})
	t.Run("store stays down", func(t *testing.T) {
		store := newFakeStore()
		store.always = errors.New("down")
		w, _ := testWriter(t, store, writerConfig{batchSize: 2, queueSize: 100, retryMin: time.Millisecond, retryMax: time.Millisecond})
		for i := range 5 {
			w.Record(ev(fmt.Sprint(i)))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := w.Close(ctx)
		if err == nil || !strings.Contains(err.Error(), "5 events were not written") || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close = %v, want 5 events not written and a deadline error", err)
		}
		if got := testutil.ToFloat64(w.m.dropped); got != 5 {
			t.Errorf("dropped = %v, want 5", got)
		}
		if got := testutil.ToFloat64(w.m.depth); got != 0 {
			t.Errorf("depth = %v, want 0", got)
		}
	})
}

func TestWriterConcurrentRecord(t *testing.T) {
	store := newFakeStore()
	w, tick := testWriter(t, store, writerConfig{batchSize: 7, queueSize: 10_000})
	const goroutines, each = 50, 100
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range each {
				w.Record(ev(fmt.Sprintf("%d-%d", g, i)))
			}
		})
	}
	stop := make(chan struct{})
	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		for {
			select {
			case tick <- time.Now():
			case <-stop:
				return
			}
		}
	}()
	wg.Wait()
	close(stop)
	<-ticked
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for len(seen) < goroutines*each {
		for _, e := range store.next(t) {
			if seen[e.ID] {
				t.Fatalf("event %s written twice", e.ID)
			}
			seen[e.ID] = true
		}
	}
	if got := testutil.ToFloat64(w.m.written); got != goroutines*each {
		t.Errorf("written = %v, want %d", got, goroutines*each)
	}
}

func TestWriterMetricsRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := newWriterMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	m.dropped.Inc()
	names, err := testutil.GatherAndCount(reg)
	if err != nil {
		t.Fatal(err)
	}
	if names != 4 {
		t.Errorf("registered %d metrics, want 4", names)
	}
	// A second set conflicts and leaves nothing half-registered.
	if _, err := newWriterMetrics(reg); err == nil {
		t.Errorf("registering twice succeeded")
	}
	m.unregister()
	if n, _ := testutil.GatherAndCount(reg); n != 0 {
		t.Errorf("%d metrics left after unregister", n)
	}
	if _, err := newWriterMetrics(reg); err != nil {
		t.Errorf("register after unregister: %v", err)
	}
	if m, err := newWriterMetrics(nil); err != nil || m == nil {
		t.Errorf("nil registerer: %v", err)
	}
}
