package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// testTimeout bounds every wait in these tests. It is far longer than any
// operation should take, so it only fires when something is broken.
const testTimeout = 10 * time.Second

// logBuffer is a concurrency-safe log sink that can decode its JSON lines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// records decodes every JSON log line.
func (b *logBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	var recs []map[string]any
	dec := json.NewDecoder(strings.NewReader(b.String()))
	for {
		var m map[string]any
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			return recs
		} else if err != nil {
			t.Fatalf("decode log output: %v", err)
		}
		recs = append(recs, m)
	}
}

// find returns the records whose msg is msg.
func (b *logBuffer) find(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range b.records(t) {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

func testOptions(logs io.Writer) Options {
	return Options{
		Listen:            "127.0.0.1:0",
		AdminListen:       "127.0.0.1:0",
		MaxRequestBytes:   1 << 20,
		MaxHeaderBytes:    1 << 16,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		ShutdownGrace:     testTimeout,
		ConfigVersion:     "cfg-0123abcd",
		Logger:            slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Registry:          prometheus.NewRegistry(),
	}
}

// newTestServer builds a server with test options, adjusted by mutate.
func newTestServer(t *testing.T, mutate func(*Options)) (*Server, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	opts := testOptions(logs)
	if mutate != nil {
		mutate(&opts)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, logs
}

// startServer starts s on real loopback listeners and shuts it down, forcibly,
// when the test ends.
func startServer(t *testing.T, s *Server) {
	t.Helper()
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = s.Shutdown(ctx)
	})
}

// newClient returns an HTTP client with its own connection pool.
func newClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: testTimeout}
}

type response struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, c *http.Client, method, url string, body io.Reader) response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, url, err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: string(b)}
}

// waitListenerClosed waits until addr refuses connections, which shows that
// shutdown has closed the listener and is draining.
func waitListenerClosed(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("listener %s still accepting connections", addr)
}

// recv waits for a value from ch, failing the test after testTimeout.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testTimeout):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// waitFor polls cond until it holds, failing the test after testTimeout.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
