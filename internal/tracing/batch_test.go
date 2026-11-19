package tracing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// gatedExporter records the spans it exports. With a gate, each export
// reports that it started and then waits for the gate to open or its
// context to end, and returns that context's error.
type gatedExporter struct {
	started chan struct{}
	gate    chan struct{}
	err     error // returned by every export, when set

	mu       sync.Mutex
	exported int
	shutdown bool
}

func (e *gatedExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if e.gate != nil {
		e.started <- struct{}{}
		select {
		case <-e.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if e.err != nil {
		return e.err
	}
	e.mu.Lock()
	e.exported += len(spans)
	e.mu.Unlock()
	return nil
}

func (e *gatedExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	e.shutdown = true
	e.mu.Unlock()
	return nil
}

func (e *gatedExporter) count() (int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.exported, e.shutdown
}

// endSpans starts and ends n spans on a tracer provider whose only
// processor is b.
func endSpans(t *testing.T, b *batchProcessor, sampler sdktrace.Sampler, n int) {
	t.Helper()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(b), sdktrace.WithSampler(sampler))
	tr := tp.Tracer("test")
	for range n {
		_, s := tr.Start(context.Background(), "span")
		s.End()
	}
}

func TestBatchProcessor(t *testing.T) {
	errExport := errors.New("collector down")
	tests := []struct {
		name         string
		sampler      sdktrace.Sampler
		spans        int
		err          error
		wantExported int
		wantDropped  uint64
		wantReports  int
	}{
		{"flushed on shutdown", sdktrace.AlwaysSample(), 3, nil, 3, 0, 0},
		{"more than one batch", sdktrace.AlwaysSample(), 5, nil, 5, 0, 0},
		{"unsampled spans are not queued", sdktrace.NeverSample(), 3, nil, 0, 0, 0},
		{"failed exports are counted", sdktrace.AlwaysSample(), 5, errExport, 0, 5, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exp := &gatedExporter{err: tt.err}
			var reports int
			b := newBatchProcessor(exp, 16, 2, time.Hour, func(error) { reports++ })
			endSpans(t, b, tt.sampler, tt.spans)
			if err := b.Shutdown(context.Background()); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			exported, shut := exp.count()
			if exported != tt.wantExported || !shut || b.Dropped() != tt.wantDropped || reports != tt.wantReports {
				t.Errorf("exported %d, shut down %v, dropped %d, reports %d; want %d, true, %d, %d",
					exported, shut, b.Dropped(), reports, tt.wantExported, tt.wantDropped, tt.wantReports)
			}
			// After Shutdown, a span is dropped and counted.
			endSpans(t, b, sdktrace.AlwaysSample(), 1)
			if b.Dropped() != tt.wantDropped+1 {
				t.Errorf("dropped %d after a span past Shutdown, want %d", b.Dropped(), tt.wantDropped+1)
			}
		})
	}
}

// TestBatchProcessorQueueFull: while the exporter is stuck, OnEnd never
// blocks; spans beyond the queue are dropped and counted, and the queued
// ones are exported once it recovers.
func TestBatchProcessorQueueFull(t *testing.T) {
	exp := &gatedExporter{started: make(chan struct{}, 16), gate: make(chan struct{})}
	b := newBatchProcessor(exp, 4, 1, time.Hour, func(error) {})
	endSpans(t, b, sdktrace.AlwaysSample(), 1)
	<-exp.started // the first span's export is stuck
	endSpans(t, b, sdktrace.AlwaysSample(), 7)
	if got := b.Dropped(); got != 3 {
		t.Errorf("dropped %d with the queue full, want 3", got)
	}
	close(exp.gate)
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if exported, _ := exp.count(); exported != 5 || b.Dropped() != 3 {
		t.Errorf("exported %d, dropped %d; want 5 and 3", exported, b.Dropped())
	}
}

// TestBatchProcessorContexts: ForceFlush and Shutdown return when their
// context ends, even with the exporter stuck; the spans Shutdown abandons
// are counted as dropped.
func TestBatchProcessorContexts(t *testing.T) {
	exp := &gatedExporter{started: make(chan struct{}, 16), gate: make(chan struct{})}
	reported := make(chan error, 16)
	b := newBatchProcessor(exp, 16, 1, time.Hour, func(err error) { reported <- err })
	endSpans(t, b, sdktrace.AlwaysSample(), 3)
	<-exp.started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.ForceFlush(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ForceFlush = %v, want context.Canceled", err)
	}
	if err := b.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Shutdown = %v, want context.Canceled", err)
	}
	// The stuck export and the two after it fail on the canceled context.
	<-b.done
	if exported, shut := exp.count(); exported != 0 || !shut || b.Dropped() != 3 {
		t.Errorf("exported %d, shut down %v, dropped %d; want 0, true, 3", exported, shut, b.Dropped())
	}
	if err := b.ForceFlush(context.Background()); err != nil {
		t.Errorf("ForceFlush after the drain: %v", err)
	}
}

// TestBatchProcessorForceFlush: ForceFlush exports what is queued without
// waiting for the batch timeout.
func TestBatchProcessorForceFlush(t *testing.T) {
	exp := &gatedExporter{}
	b := newBatchProcessor(exp, 16, 512, time.Hour, func(error) {})
	endSpans(t, b, sdktrace.AlwaysSample(), 3)
	if err := b.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if exported, shut := exp.count(); exported != 3 || shut {
		t.Errorf("exported %d, shut down %v; want 3, false", exported, shut)
	}
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
