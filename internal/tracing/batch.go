package tracing

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// batchProcessor is Switchyard's span processor, in place of the SDK's
// BatchSpanProcessor, whose queue-full drops are invisible to its caller.
// OnEnd queues each sampled span without blocking, and drops it, counted,
// when the queue is full. One goroutine exports the queue in batches of up
// to batchSize spans, at least every timeout, one export at a time. A
// batch whose export returns an error is dropped and counted too, so
// Dropped accounts for every span that never reached the exporter's
// success path. It is safe for concurrent use.
type batchProcessor struct {
	exp       sdktrace.SpanExporter
	report    func(error)
	batchSize int
	timeout   time.Duration

	queue   chan sdktrace.ReadOnlySpan
	flushes chan chan struct{} // ForceFlush requests
	stop    chan struct{}      // closed by Shutdown
	done    chan struct{}      // closed when run has drained and exited
	dropped atomic.Uint64
	// ctx bounds every export; Shutdown cancels it when its own context
	// ends first, so the remaining exports fail at once, counted.
	ctx    context.Context
	cancel context.CancelFunc

	// mu guards stopped: OnEnd holds it for reading while it queues, and
	// Shutdown for writing, so no span is queued once the drain begins.
	mu       sync.RWMutex
	stopped  bool
	stopOnce sync.Once
}

// newBatchProcessor starts a batchProcessor that exports to exp, queueing
// at most queueSize spans; report receives the export errors.
func newBatchProcessor(exp sdktrace.SpanExporter, queueSize, batchSize int, timeout time.Duration, report func(error)) *batchProcessor {
	ctx, cancel := context.WithCancel(context.Background())
	b := &batchProcessor{
		exp: exp, report: report, batchSize: batchSize, timeout: timeout,
		queue:   make(chan sdktrace.ReadOnlySpan, queueSize),
		flushes: make(chan chan struct{}),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		ctx:     ctx, cancel: cancel,
	}
	go b.run()
	return b
}

// OnStart implements sdktrace.SpanProcessor; it does nothing.
func (b *batchProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

// OnEnd queues s, if sampled, or drops and counts it when the queue is
// full or the processor is shut down. It never blocks.
func (b *batchProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	if !s.SpanContext().IsSampled() {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.stopped {
		b.dropped.Add(1)
		return
	}
	select {
	case b.queue <- s:
	default:
		b.dropped.Add(1)
	}
}

// ForceFlush exports every span queued before the call, waiting at most
// until ctx is done.
func (b *batchProcessor) ForceFlush(ctx context.Context) error {
	ch := make(chan struct{})
	select {
	case b.flushes <- ch:
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown stops queueing, exports what is queued, then shuts the exporter
// down, waiting at most until ctx is done. If ctx ends first, the exports
// still to come are abandoned (their spans are counted as dropped) and
// ctx's error is returned. Later calls wait for the same drain.
func (b *batchProcessor) Shutdown(ctx context.Context) error {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		b.stopped = true
		b.mu.Unlock()
		close(b.stop)
	})
	select {
	case <-b.done:
	case <-ctx.Done():
		b.cancel()
		_ = b.exp.Shutdown(ctx)
		return ctx.Err()
	}
	b.cancel()
	return b.exp.Shutdown(ctx)
}

// Dropped returns the spans dropped so far: queue full, after Shutdown, or
// in an export that returned an error.
func (b *batchProcessor) Dropped() uint64 { return b.dropped.Load() }

// run is the export goroutine.
func (b *batchProcessor) run() {
	defer close(b.done)
	batch := make([]sdktrace.ReadOnlySpan, 0, b.batchSize)
	timer := time.NewTimer(b.timeout)
	defer timer.Stop()
	export := func() {
		if len(batch) > 0 {
			b.export(batch)
			batch = make([]sdktrace.ReadOnlySpan, 0, b.batchSize)
		}
		timer.Reset(b.timeout)
	}
	add := func(s sdktrace.ReadOnlySpan) {
		batch = append(batch, s)
		if len(batch) >= b.batchSize {
			export()
		}
	}
	// drain adds every span queued now.
	drain := func() {
		for {
			select {
			case s := <-b.queue:
				add(s)
			default:
				return
			}
		}
	}
	for {
		select {
		case s := <-b.queue:
			add(s)
		case <-timer.C:
			export()
		case ch := <-b.flushes:
			drain()
			export()
			close(ch)
		case <-b.stop:
			drain()
			export()
			return
		}
	}
}

// export sends spans to the exporter in one call, bounded by exportTimeout.
func (b *batchProcessor) export(spans []sdktrace.ReadOnlySpan) {
	ctx, cancel := context.WithTimeout(b.ctx, exportTimeout)
	defer cancel()
	if err := b.exp.ExportSpans(ctx, spans); err != nil {
		b.dropped.Add(uint64(len(spans)))
		b.report(err)
	}
}
