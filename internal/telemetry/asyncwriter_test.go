package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// syncBuffer is a bytes.Buffer safe for the writer goroutine and the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// gatedWriter blocks every Write until the test releases it, and reports
// each Write that starts.
type gatedWriter struct {
	started chan struct{} // receives once per Write, before it blocks
	release chan struct{} // closed to let every Write through
	syncBuffer
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{started: make(chan struct{}, 100), release: make(chan struct{})}
}

func (g *gatedWriter) Write(p []byte) (int, error) {
	g.started <- struct{}{}
	<-g.release
	return g.syncBuffer.Write(p)
}

func closeWriter(t *testing.T, a *AsyncWriter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestAsyncWriterKeepsOrder(t *testing.T) {
	tests := []struct {
		name  string
		lines int
		size  int // bytes per line, newline included
	}{
		{name: "one line", lines: 1, size: 10},
		{name: "many short lines", lines: 5000, size: 40},
		{name: "lines past one batch", lines: 300, size: 1000},
		{name: "a line larger than a batch", lines: 3, size: 3 * maxBatchBytes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out syncBuffer
			a := NewAsyncWriter(&out, AsyncOptions{QueueLines: tt.lines})
			var want strings.Builder
			for i := range tt.lines {
				prefix := fmt.Sprintf("%06d ", i)
				line := prefix + strings.Repeat("x", tt.size-len(prefix)-1) + "\n"
				want.WriteString(line)
				if n, err := a.Write([]byte(line)); n != len(line) || err != nil {
					t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(line))
				}
			}
			closeWriter(t, a)
			if got := out.String(); got != want.String() {
				t.Fatalf("output differs from the lines written in order (got %d bytes, want %d)", len(got), want.Len())
			}
			if d := a.Dropped(); d != 0 {
				t.Fatalf("Dropped = %d, want 0", d)
			}
		})
	}
}

// TestAsyncWriterCopiesLine: the caller may reuse its buffer once Write
// returns, as slog does.
func TestAsyncWriterCopiesLine(t *testing.T) {
	var out syncBuffer
	a := NewAsyncWriter(&out, AsyncOptions{})
	buf := []byte("first\n")
	_, _ = a.Write(buf)
	copy(buf, "XXXXX\n")
	closeWriter(t, a)
	if got := out.String(); got != "first\n" {
		t.Fatalf("output = %q, want %q", got, "first\n")
	}
}

func TestAsyncWriterConcurrent(t *testing.T) {
	var out syncBuffer
	a := NewAsyncWriter(&out, AsyncOptions{QueueLines: 100000})
	const writers, perWriter = 8, 2000
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				_, _ = fmt.Fprintf(a, "w%d %d\n", w, i)
			}
		})
	}
	wg.Wait()
	closeWriter(t, a)

	next := make([]int, writers)
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != writers*perWriter {
		t.Fatalf("got %d lines, want %d", len(lines), writers*perWriter)
	}
	for _, l := range lines {
		var w, i int
		if _, err := fmt.Sscanf(l, "w%d %d", &w, &i); err != nil {
			t.Fatalf("mangled line %q: %v", l, err)
		}
		if i != next[w] {
			t.Fatalf("writer %d: line %d came after line %d", w, i, next[w]-1)
		}
		next[w]++
	}
}

func TestAsyncWriterDropsWhenFull(t *testing.T) {
	out := newGatedWriter()
	a := NewAsyncWriter(out, AsyncOptions{QueueLines: 2})
	reg := prometheus.NewRegistry()
	reg.MustRegister(a.Collector())

	_, _ = a.Write([]byte("1\n"))
	<-out.started // the goroutine holds line 1, blocked in the output
	for _, l := range []string{"2\n", "3\n", "4\n", "5\n"} {
		if n, err := a.Write([]byte(l)); n != len(l) || err != nil {
			t.Fatalf("Write(%q) = %d, %v; want %d, nil even when dropped", l, n, err, len(l))
		}
	}
	if d := a.Dropped(); d != 2 {
		t.Fatalf("Dropped = %d, want 2 (lines 4 and 5)", d)
	}
	if got := testutil.ToFloat64(a.Collector()); got != 2 {
		t.Fatalf("switchyard_log_lines_dropped_total = %v, want 2", got)
	}
	if n, err := testutil.GatherAndCount(reg, "switchyard_log_lines_dropped_total"); n != 1 || err != nil {
		t.Fatalf("registered collector: %d series, %v", n, err)
	}
	close(out.release)
	closeWriter(t, a)
	if got := out.String(); got != "1\n2\n3\n" {
		t.Fatalf("output = %q, want the lines that fit, in order", got)
	}
}

// TestAsyncWriterDurable: with the queue full, a durable line (an audit
// line) waits for room, in order after the queued lines, while other lines
// are still dropped at once; one whose wait runs out is dropped and
// counted.
func TestAsyncWriterDurable(t *testing.T) {
	tests := []struct {
		name        string
		wait        time.Duration
		release     bool // let the output through while the line waits
		wantOut     string
		wantDropped uint64
	}{
		{"room frees up", time.Minute, true, "1\n2\n3\naudit\n", 1},
		{"output stays stalled", time.Millisecond, false, "1\n2\n3\n", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := newGatedWriter()
			asked := make(chan struct{}, 1)
			a := NewAsyncWriter(out, AsyncOptions{QueueLines: 2, DurableWait: tt.wait, Durable: func(l []byte) bool {
				if string(l) != "audit\n" {
					return false
				}
				asked <- struct{}{}
				return true
			}})
			_, _ = a.Write([]byte("1\n"))
			<-out.started // the goroutine holds line 1, blocked in the output
			_, _ = a.Write([]byte("2\n"))
			_, _ = a.Write([]byte("3\n")) // the queue is full
			wrote := make(chan struct{})
			go func() {
				_, _ = a.Write([]byte("audit\n"))
				close(wrote)
			}()
			<-asked // the audit line found the queue full
			_, _ = a.Write([]byte("dropped\n"))
			if tt.release {
				close(out.release)
				<-wrote
			} else {
				<-wrote
				close(out.release)
			}
			closeWriter(t, a)
			if got := out.String(); got != tt.wantOut {
				t.Errorf("output = %q, want %q", got, tt.wantOut)
			}
			if d := a.Dropped(); d != tt.wantDropped {
				t.Errorf("Dropped = %d, want %d", d, tt.wantDropped)
			}
		})
	}
}

func TestAsyncWriterFlush(t *testing.T) {
	var out syncBuffer
	a := NewAsyncWriter(&out, AsyncOptions{})
	defer closeWriter(t, a)
	for i := range 100 {
		_, _ = fmt.Fprintf(a, "%d\n", i)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := strings.Count(out.String(), "\n"); got != 100 {
		t.Fatalf("after Flush the output has %d lines, want 100", got)
	}
}

func TestAsyncWriterFlushTimeout(t *testing.T) {
	out := newGatedWriter()
	a := NewAsyncWriter(out, AsyncOptions{QueueLines: 1})
	_, _ = a.Write([]byte("1\n"))
	<-out.started
	_, _ = a.Write([]byte("2\n")) // fills the queue
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Flush(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Flush with a full queue and a done context = %v, want context.Canceled", err)
	}
	close(out.release)
	closeWriter(t, a)
	// After Close, Flush returns at once.
	if err := a.Flush(context.Background()); err != nil {
		t.Fatalf("Flush after Close: %v", err)
	}
}

func TestAsyncWriterAfterClose(t *testing.T) {
	t.Run("writes after a completed Close are synchronous", func(t *testing.T) {
		var out syncBuffer
		a := NewAsyncWriter(&out, AsyncOptions{})
		_, _ = a.Write([]byte("before\n"))
		closeWriter(t, a)
		closeWriter(t, a) // a second Close is harmless
		if n, err := a.Write([]byte("after\n")); n != 6 || err != nil {
			t.Fatalf("Write after Close = %d, %v", n, err)
		}
		if got := out.String(); got != "before\nafter\n" {
			t.Fatalf("output = %q", got)
		}
	})
	t.Run("a failed synchronous write reports the error", func(t *testing.T) {
		a := NewAsyncWriter(failingWriter{}, AsyncOptions{})
		closeWriter(t, a)
		if _, err := a.Write([]byte("x\n")); !errors.Is(err, errFailingWriter) {
			t.Fatalf("Write after Close to a failing output = %v, want it wrapped", err)
		}
	})
	t.Run("a timed-out Close leaves the drain running and drops new lines", func(t *testing.T) {
		out := newGatedWriter()
		a := NewAsyncWriter(out, AsyncOptions{QueueLines: 4})
		_, _ = a.Write([]byte("1\n"))
		<-out.started
		_, _ = a.Write([]byte("2\n"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := a.Close(ctx)
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "1 lines still queued") {
			t.Fatalf("Close with a blocked output = %v, want context.Canceled and the queued count", err)
		}
		_, _ = a.Write([]byte("late\n"))
		if d := a.Dropped(); d != 1 {
			t.Fatalf("Dropped = %d, want 1 (the line written during the drain)", d)
		}
		close(out.release)
		closeWriter(t, a)
		if got := out.String(); got != "1\n2\n" {
			t.Fatalf("output = %q, want the queued lines", got)
		}
	})
}

var errFailingWriter = errors.New("output failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errFailingWriter }

// TestAsyncLoggerRedacts: the redacting handler runs before the line is
// encoded and queued, so the asynchronous path keeps its guarantees.
func TestAsyncLoggerRedacts(t *testing.T) {
	const secret = "async-secret-0123456789"
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			var out syncBuffer
			a := NewAsyncWriter(&out, AsyncOptions{})
			l := NewLogger(a, LogOptions{Format: format, Secrets: []string{secret}})
			l.Info("request", "authorization", "Bearer "+secret, "note", "x "+secret, "key", "sk-abcdefghijklmnopqrstuvwxyz")
			l.Error("failed", "error", errors.New("dial redis://u:"+secret+"@host:6379"))
			closeWriter(t, a)
			got := out.String()
			if strings.Contains(got, secret) || strings.Contains(got, "sk-abcdefghijklmnop") {
				t.Fatalf("secret leaked through the async writer: %s", got)
			}
			if strings.Count(got, "\n") != 2 || !strings.Contains(got, Redacted) {
				t.Fatalf("output = %q, want two redacted lines", got)
			}
			if format == "json" {
				for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
					if !json.Valid([]byte(line)) {
						t.Fatalf("line is not JSON: %s", line)
					}
				}
			}
		})
	}
}

// benchmarkLogger logs an access-log-sized line from parallel goroutines to
// a real file, through the writer that wrap returns.
func benchmarkLogger(b *testing.B, wrap func(io.Writer) (io.Writer, func())) {
	f, err := os.Create(filepath.Join(b.TempDir(), "log"))
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	w, done := wrap(f)
	defer done()
	l := NewLogger(w, LogOptions{Secrets: []string{"bench-secret-value"}}).With("request_id", "0190a5c3-7d2e-7f4a-9b1c-2d3e4f5a6b7c")
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l.LogAttrs(context.Background(), slog.LevelInfo, "request",
				slog.String("method", "POST"), slog.String("path", "/v1/chat/completions"),
				slog.Int("status", 200), slog.Int64("bytes", 512), slog.Float64("duration_ms", 1.234),
				slog.String("remote_ip", "127.0.0.1"), slog.String("user_agent", "bench/1.0"),
				slog.String("listener", "public"), slog.String("tenant", "t1"), slog.String("key_id", "k1"),
				slog.String("served_deployment", "model@provider"), slog.Int("input_tokens", 12),
				slog.Int("output_tokens", 34), slog.String("cost_usd", "0.000123"))
		}
	})
}

// BenchmarkLoggerSync and BenchmarkLoggerAsync compare the caller's cost of
// one access-log line written directly to a file and through an
// AsyncWriter.
func BenchmarkLoggerSync(b *testing.B) {
	benchmarkLogger(b, func(w io.Writer) (io.Writer, func()) { return w, func() {} })
}

func BenchmarkLoggerAsync(b *testing.B) {
	benchmarkLogger(b, func(w io.Writer) (io.Writer, func()) {
		a := NewAsyncWriter(w, AsyncOptions{})
		return a, func() {
			b.StopTimer()
			_ = a.Close(context.Background())
			b.ReportMetric(float64(a.Dropped())/float64(b.N), "dropped/op")
		}
	})
}
