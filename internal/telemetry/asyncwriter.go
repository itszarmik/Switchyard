package telemetry

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Defaults and bounds of an AsyncWriter.
const (
	// DefaultLogQueueLines is the queue length of an AsyncWriter when
	// AsyncOptions.QueueLines is zero. At about 1 KiB per access-log line it
	// holds roughly 8 MiB, several seconds of logs at thousands of requests
	// a second.
	DefaultLogQueueLines = 8192
	// maxBatchBytes is how much the writer goroutine gathers into one Write
	// to the output when lines are waiting, so a busy logger makes one
	// syscall per batch rather than one per line.
	maxBatchBytes = 64 << 10
	// maxPooledLineBytes is the largest line buffer kept for reuse; larger
	// buffers, from unusually long lines, are left to the GC.
	maxPooledLineBytes = 16 << 10
	// initialLineBytes is the capacity of a new line buffer.
	initialLineBytes = 1 << 10
	// DefaultDurableWait is how long a durable line waits for room in a
	// full queue when AsyncOptions.DurableWait is zero.
	DefaultDurableWait = time.Second
)

// AsyncOptions configures NewAsyncWriter.
type AsyncOptions struct {
	// QueueLines is the most lines that may wait to be written. Zero means
	// DefaultLogQueueLines.
	QueueLines int
	// Durable, when set, reports the lines that must not be dropped, such
	// as audit lines: when the queue is full, Write waits up to
	// DurableWait for room for them instead of dropping them at once.
	Durable func(line []byte) bool
	// DurableWait bounds that wait. Zero means DefaultDurableWait.
	DurableWait time.Duration
}

// AsyncWriter is an io.Writer for log output that takes the write off the
// caller's path. Write copies the line into a bounded queue and returns at
// once; one goroutine writes the queued lines to the output, in order,
// gathering the lines that are waiting into one Write of up to 64 KiB.
//
// When the queue is full, Write drops the line and counts it, so a slow or
// stalled output (a full pipe, a blocked terminal) never blocks a request.
// The count is exported as switchyard_log_lines_dropped_total by
// Collector. Every line is treated alike, whatever its level: an
// error-level line is queued, and dropped when the queue is full, like any
// other, because writing it synchronously would block on the same stalled
// output. The exception is a durable line (AsyncOptions.Durable), such as
// an audit line: its Write waits up to DurableWait for room, in order with
// the other lines, and drops it, counted, only if the output stays
// stalled that long. Other writers are not held up meanwhile.
//
// Close drains the queue. Writes after a completed Close go straight to the
// output, synchronously, so lines logged late in shutdown still appear in
// order. Writes after a Close that timed out, while the queue is still
// draining, are dropped and counted, since writing them would interleave
// with the drain.
//
// Each Write must carry whole lines, as slog handlers do: lines are never
// split, but separate Writes may be gathered into one. Errors from the
// output are ignored, since there is nowhere left to report them. An
// AsyncWriter is safe for concurrent use.
type AsyncWriter struct {
	out         io.Writer
	queue       chan asyncItem
	stop        chan struct{} // closed by Close
	done        chan struct{} // closed when the writer goroutine has drained and exited
	dropped     atomic.Uint64
	lines       sync.Pool // of *[]byte
	durable     func(line []byte) bool
	durableWait time.Duration

	// mu guards closed: every send to the queue holds it for reading, and
	// Close for writing, so nothing is queued after the drain starts.
	mu     sync.RWMutex
	closed bool
	// outMu serializes the writes after Close.
	outMu sync.Mutex
}

// asyncItem is a queued line, or a flush marker when flushed is set.
type asyncItem struct {
	line    *[]byte
	flushed chan struct{}
}

// NewAsyncWriter starts an AsyncWriter that writes to out. Call Close to
// flush it and stop its goroutine.
func NewAsyncWriter(out io.Writer, opts AsyncOptions) *AsyncWriter {
	n := opts.QueueLines
	if n <= 0 {
		n = DefaultLogQueueLines
	}
	a := &AsyncWriter{
		out:         out,
		queue:       make(chan asyncItem, n),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		durable:     opts.Durable,
		durableWait: opts.DurableWait,
	}
	if a.durableWait <= 0 {
		a.durableWait = DefaultDurableWait
	}
	a.lines.New = func() any {
		b := make([]byte, 0, initialLineBytes)
		return &b
	}
	go a.run()
	return a
}

// Write queues a copy of p and returns len(p) and nil, or drops it when the
// queue is full; a durable line waits for room first (see AsyncWriter).
// After Close it writes p to the output directly.
func (a *AsyncWriter) Write(p []byte) (int, error) {
	a.mu.RLock()
	if a.closed {
		a.mu.RUnlock()
		return a.writeAfterClose(p)
	}
	defer a.mu.RUnlock()
	line, _ := a.lines.Get().(*[]byte)
	*line = append((*line)[:0], p...)
	it := asyncItem{line: line}
	select {
	case a.queue <- it:
		return len(p), nil
	default:
	}
	if a.durable != nil && a.durable(p) {
		t := time.NewTimer(a.durableWait)
		defer t.Stop()
		select {
		case a.queue <- it:
			return len(p), nil
		case <-t.C:
		}
	}
	a.release(line)
	a.dropped.Add(1)
	return len(p), nil
}

// writeAfterClose writes p to the output once Close's drain is done, or
// drops it while the drain still runs (see AsyncWriter).
func (a *AsyncWriter) writeAfterClose(p []byte) (int, error) {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	select {
	case <-a.done:
	default:
		a.dropped.Add(1)
		return len(p), nil
	}
	if _, err := a.out.Write(p); err != nil {
		return 0, fmt.Errorf("telemetry: writing a log line: %w", err)
	}
	return len(p), nil
}

// Flush waits until every line queued before the call has been written to
// the output, or ctx is done, and returns an error if ctx ended first.
// After Close it waits for Close's drain. It blocks until the queue has
// room for its marker, so it is meant for tests and shutdown, not for the
// request path.
func (a *AsyncWriter) Flush(ctx context.Context) error {
	var marker chan struct{}
	a.mu.RLock()
	if !a.closed {
		marker = make(chan struct{})
		select {
		case a.queue <- asyncItem{flushed: marker}:
		case <-ctx.Done():
			a.mu.RUnlock()
			return fmt.Errorf("telemetry: flushing the log writer: %w", ctx.Err())
		}
	}
	a.mu.RUnlock()
	// After Close, or once the writer has exited, Close's drain writes
	// everything queued before this call; a nil marker never fires.
	select {
	case <-marker:
		return nil
	case <-a.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("telemetry: flushing the log writer: %w", ctx.Err())
	}
}

// Close stops accepting lines into the queue and waits until the queued
// lines have been written, or ctx is done. If ctx ends first it returns an
// error with the number of lines still queued; the goroutine keeps draining
// them in the background. Calling Close again waits for the same drain.
func (a *AsyncWriter) Close(ctx context.Context) error {
	a.mu.Lock()
	if !a.closed {
		a.closed = true
		close(a.stop)
	}
	a.mu.Unlock()
	select {
	case <-a.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("telemetry: closing the log writer with %d lines still queued: %w", len(a.queue), ctx.Err())
	}
}

// Dropped reports how many lines have been dropped so far.
func (a *AsyncWriter) Dropped() uint64 { return a.dropped.Load() }

// Collector returns the counter switchyard_log_lines_dropped_total, which
// reads Dropped, for registration on the metrics registry. Each call
// returns a new collector; register one.
func (a *AsyncWriter) Collector() prometheus.Collector {
	return prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "switchyard_log_lines_dropped_total",
		Help: "Log lines dropped because the asynchronous log writer's queue was full.",
	}, func() float64 { return float64(a.dropped.Load()) })
}

// run is the writer goroutine: it writes queued lines until Close, then
// drains the queue and exits.
func (a *AsyncWriter) run() {
	defer close(a.done)
	batch := make([]byte, 0, maxBatchBytes)
	for {
		select {
		case it := <-a.queue:
			batch = a.add(batch, it)
			// Gather what else is already waiting, without blocking.
		gather:
			for len(batch) < maxBatchBytes {
				select {
				case it := <-a.queue:
					batch = a.add(batch, it)
				default:
					break gather
				}
			}
			batch = a.writeBatch(batch)
		case <-a.stop:
			for {
				select {
				case it := <-a.queue:
					batch = a.add(batch, it)
					if len(batch) >= maxBatchBytes {
						batch = a.writeBatch(batch)
					}
				default:
					a.writeBatch(batch)
					return
				}
			}
		}
	}
}

// add appends a line to batch, or, for a flush marker, writes the batch
// and then signals the marker.
func (a *AsyncWriter) add(batch []byte, it asyncItem) []byte {
	if it.flushed != nil {
		batch = a.writeBatch(batch)
		close(it.flushed)
		return batch
	}
	batch = append(batch, *it.line...)
	a.release(it.line)
	return batch
}

// writeBatch writes batch to the output and returns it emptied. A batch
// that grew far past maxBatchBytes, from a huge line, is replaced so its
// memory is not kept.
func (a *AsyncWriter) writeBatch(batch []byte) []byte {
	if len(batch) > 0 {
		_, _ = a.out.Write(batch) // nowhere to report a failed log write
	}
	if cap(batch) > 4*maxBatchBytes {
		return make([]byte, 0, maxBatchBytes)
	}
	return batch[:0]
}

// release returns a line buffer to the pool unless it is too large to keep.
func (a *AsyncWriter) release(line *[]byte) {
	if cap(*line) <= maxPooledLineBytes {
		a.lines.Put(line)
	}
}
