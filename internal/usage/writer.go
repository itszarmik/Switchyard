package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Writer defaults (DESIGN §12.2).
const (
	DefaultBatchSize     = 500
	DefaultFlushInterval = time.Second
	DefaultQueueSize     = 100_000
	DefaultRetryMin      = 100 * time.Millisecond
	DefaultRetryMax      = 10 * time.Second
	// defaultWriteTimeout bounds one attempt to write a batch, so that a
	// database that stops answering is retried rather than waited on.
	defaultWriteTimeout = 30 * time.Second
)

// batchStore writes a batch of events atomically: all or none.
type batchStore interface {
	WriteBatch(ctx context.Context, events []*Event) error
}

// permanentError marks a batch error that retrying cannot fix, such as
// data the database rejects. The writer drops such a batch and counts it.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// writerConfig configures a writer. Zero values take the defaults.
type writerConfig struct {
	batchSize     int
	flushInterval time.Duration
	queueSize     int
	retryMin      time.Duration
	retryMax      time.Duration
	writeTimeout  time.Duration
	logger        *slog.Logger
	metrics       *writerMetrics

	// Test hooks. after replaces time.After for retry backoff; tick, when
	// set, replaces the flush ticker.
	after func(time.Duration) <-chan time.Time
	tick  <-chan time.Time
}

// writer is the asynchronous batching writer behind the Postgres ledger: a
// bounded in-memory queue, drained in batches of batchSize or every
// flushInterval, with retries and exponential backoff while the store
// fails. When the queue is full the oldest queued event is dropped. The
// batch being written is held outside the queue, so at most queueSize +
// batchSize events are buffered.
type writer struct {
	store batchStore
	cfg   writerConfig
	log   *slog.Logger
	m     *writerMetrics

	mu       sync.Mutex
	ring     []*Event // circular, oldest at head
	head, n  int
	inflight int
	closed   bool
	dropping bool  // events have been dropped since the last logged recovery
	lost     int64 // events abandoned at shutdown

	wake      chan struct{} // a full batch is queued
	closing   chan struct{} // Close was called
	done      chan struct{} // run has returned
	closeOnce sync.Once
	runCtx    context.Context
	cancelRun context.CancelFunc
}

// newWriter starts a writer over store. Close stops it.
func newWriter(store batchStore, cfg writerConfig) *writer {
	if cfg.batchSize <= 0 {
		cfg.batchSize = DefaultBatchSize
	}
	if cfg.flushInterval <= 0 {
		cfg.flushInterval = DefaultFlushInterval
	}
	if cfg.queueSize <= 0 {
		cfg.queueSize = DefaultQueueSize
	}
	if cfg.retryMin <= 0 {
		cfg.retryMin = DefaultRetryMin
	}
	if cfg.retryMax < cfg.retryMin {
		cfg.retryMax = max(DefaultRetryMax, cfg.retryMin)
	}
	if cfg.writeTimeout <= 0 {
		cfg.writeTimeout = defaultWriteTimeout
	}
	if cfg.after == nil {
		cfg.after = time.After
	}
	log := cfg.logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := cfg.metrics
	if m == nil {
		m = unregisteredWriterMetrics()
	}
	runCtx, cancel := context.WithCancel(context.Background())
	w := &writer{
		store:     store,
		cfg:       cfg,
		log:       log,
		m:         m,
		ring:      make([]*Event, cfg.queueSize),
		wake:      make(chan struct{}, 1),
		closing:   make(chan struct{}),
		done:      make(chan struct{}),
		runCtx:    runCtx,
		cancelRun: cancel,
	}
	go w.run()
	return w
}

// Record queues ev without blocking. With the queue full, the oldest queued
// event is dropped. After Close, ev is dropped. Both are counted.
func (w *writer) Record(ev *Event) {
	if ev == nil {
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		w.m.dropped.Inc()
		return
	}
	dropped, firstDrop := false, false
	if w.n == len(w.ring) {
		w.ring[w.head] = ev
		w.head = (w.head + 1) % len(w.ring)
		dropped, firstDrop = true, !w.dropping
		w.dropping = true
	} else {
		w.ring[(w.head+w.n)%len(w.ring)] = ev
		w.n++
	}
	full := w.n >= w.cfg.batchSize
	w.m.depth.Set(float64(w.n + w.inflight))
	w.mu.Unlock()

	if dropped {
		w.m.dropped.Inc()
		if firstDrop {
			w.log.Warn("usage: event queue is full; dropping the oldest events", "queue_size", w.cfg.queueSize)
		}
	}
	if full {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

// Close stops accepting events and writes the queued ones, retrying while
// the store fails, until the queue is empty or ctx is done. It returns an
// error if events were left unwritten; they are counted as dropped.
func (w *writer) Close(ctx context.Context) error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		close(w.closing)
	})
	select {
	case <-w.done:
	case <-ctx.Done():
		w.cancelRun()
		<-w.done
	}
	w.cancelRun()
	w.mu.Lock()
	lost := w.lost
	w.mu.Unlock()
	if lost > 0 {
		cause := ctx.Err()
		if cause == nil {
			cause = context.Canceled
		}
		return fmt.Errorf("usage: close: %d events were not written: %w", lost, cause)
	}
	return nil
}

// run is the writer's goroutine.
func (w *writer) run() {
	defer close(w.done)
	tick := w.cfg.tick
	if tick == nil {
		t := time.NewTicker(w.cfg.flushInterval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-w.closing:
			w.flush(true)
			w.mu.Lock()
			if w.n > 0 {
				w.lost += int64(w.n)
				w.m.dropped.Add(float64(w.n))
				w.n = 0
				clear(w.ring)
			}
			w.m.depth.Set(0)
			w.mu.Unlock()
			return
		case <-w.wake:
			w.flush(false)
		case <-tick:
			w.flush(true)
		}
	}
}

// flush writes queued events in batches of at most batchSize: only full
// batches unless partial is set, in which case everything queued. It
// returns early when the writer is shutting down and gives up.
func (w *writer) flush(partial bool) {
	for {
		batch := w.take(partial)
		if batch == nil {
			return
		}
		if !w.write(batch) {
			return
		}
	}
}

// take removes the next batch from the queue, or returns nil if there is no
// full batch (or, with partial, no event at all).
func (w *writer) take(partial bool) []*Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.n == 0 || (!partial && w.n < w.cfg.batchSize) {
		return nil
	}
	size := min(w.n, w.cfg.batchSize)
	batch := make([]*Event, size)
	for i := range size {
		j := (w.head + i) % len(w.ring)
		batch[i] = w.ring[j]
		w.ring[j] = nil
	}
	w.head = (w.head + size) % len(w.ring)
	w.n -= size
	w.inflight = size
	return batch
}

// write writes batch, retrying with exponential backoff until it is
// written, or the store rejects it permanently, or the writer gives up at
// shutdown. It returns false only in the last case.
func (w *writer) write(batch []*Event) bool {
	delay := w.cfg.retryMin
	for {
		ctx, cancel := context.WithTimeout(w.runCtx, w.cfg.writeTimeout)
		err := w.store.WriteBatch(ctx, batch)
		cancel()
		if err == nil {
			w.m.written.Add(float64(len(batch)))
			w.finish(true)
			return true
		}
		if w.runCtx.Err() != nil {
			w.abandon(len(batch))
			return false
		}
		w.m.writeErrors.Inc()
		if perm, ok := errors.AsType[*permanentError](err); ok {
			w.m.dropped.Add(float64(len(batch)))
			w.log.Error("usage: the database rejected a batch of events; dropping it",
				"events", len(batch), "first_id", batch[0].ID, "error", perm)
			w.finish(false)
			return true
		}
		w.log.Warn("usage: writing events failed; retrying",
			"events", len(batch), "retry_in", delay, "error", err)
		select {
		case <-w.cfg.after(delay):
		case <-w.runCtx.Done():
			w.abandon(len(batch))
			return false
		}
		delay = min(delay*2, w.cfg.retryMax)
	}
}

// finish clears the in-flight batch. After a successful write it logs once
// that writes have recovered, if events were dropped meanwhile.
func (w *writer) finish(ok bool) {
	w.mu.Lock()
	w.inflight = 0
	recovered := ok && w.dropping
	if recovered {
		w.dropping = false
	}
	w.m.depth.Set(float64(w.n))
	w.mu.Unlock()
	if recovered {
		w.log.Info("usage: event writes have caught up after dropping events; see switchyard_usage_events_dropped_total")
	}
}

// abandon gives up on the in-flight batch at shutdown.
func (w *writer) abandon(n int) {
	w.mu.Lock()
	w.inflight = 0
	w.lost += int64(n)
	w.m.depth.Set(float64(w.n))
	w.mu.Unlock()
	w.m.dropped.Add(float64(n))
}

// writerMetrics are the ledger writer's Prometheus metrics.
type writerMetrics struct {
	depth       prometheus.Gauge
	dropped     prometheus.Counter
	written     prometheus.Counter
	writeErrors prometheus.Counter
	reg         prometheus.Registerer
}

// unregisteredWriterMetrics returns metrics that are counted but exported
// nowhere.
func unregisteredWriterMetrics() *writerMetrics {
	return &writerMetrics{
		depth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "switchyard_usage_queue_depth",
			Help: "Usage events waiting to be written to the ledger, including the batch being written.",
		}),
		dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "switchyard_usage_events_dropped_total",
			Help: "Usage events that were never written: dropped from a full queue, rejected by the database, or left at shutdown.",
		}),
		written: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "switchyard_usage_events_written_total",
			Help: "Usage events written to the ledger.",
		}),
		writeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "switchyard_usage_write_errors_total",
			Help: "Failed attempts to write a batch of usage events to the ledger.",
		}),
	}
}

// newWriterMetrics creates the metrics and registers them on reg, unless
// reg is nil. If one fails to register, the others are removed again.
func newWriterMetrics(reg prometheus.Registerer) (*writerMetrics, error) {
	m := unregisteredWriterMetrics()
	if reg == nil {
		return m, nil
	}
	collectors := m.collectors()
	for i, c := range collectors {
		if err := reg.Register(c); err != nil {
			for _, prev := range collectors[:i] {
				reg.Unregister(prev)
			}
			return nil, fmt.Errorf("usage: register metrics: %w", err)
		}
	}
	m.reg = reg
	return m, nil
}

func (m *writerMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.depth, m.dropped, m.written, m.writeErrors}
}

// unregister removes the metrics from the registerer they were registered
// on, if any.
func (m *writerMetrics) unregister() {
	if m.reg == nil {
		return
	}
	for _, c := range m.collectors() {
		m.reg.Unregister(c)
	}
}
