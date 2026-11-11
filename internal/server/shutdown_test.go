package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// slowHandler blocks until release is closed (or the request is canceled),
// signaling entered first.
func slowHandler(entered chan<- struct{}, release <-chan struct{}) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			_, _ = io.WriteString(w, "done")
		case <-r.Context().Done():
		}
	})
}

type result struct {
	resp response
	err  error
}

// getAsync issues a GET in the background.
func getAsync(c *http.Client, url string) <-chan result {
	ch := make(chan result, 1)
	go func() {
		resp, err := c.Get(url)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		ch <- result{resp: response{status: resp.StatusCode, body: string(b)}, err: err}
	}()
	return ch
}

func shutdownAsync(forceCtx context.Context, s *Server) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- s.Shutdown(forceCtx) }()
	return ch
}

func TestShutdownFailsReadinessAndKeepsServingDuringDelay(t *testing.T) {
	s, logs := newTestServer(t, func(o *Options) { o.ShutdownDelay = time.Hour })
	delayStarted, endDelay := make(chan struct{}), make(chan struct{})
	s.sleep = func(ctx context.Context, d time.Duration) {
		if d != time.Hour {
			t.Errorf("delay = %s, want 1h", d)
		}
		close(delayStarted)
		select {
		case <-endDelay:
		case <-ctx.Done():
		}
	}
	s.Handle("GET /v1/work", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "worked")
	}))
	startServer(t, s)
	c := newClient(t)
	public, admin := "http://"+s.PublicAddr(), "http://"+s.AdminAddr()

	if r := do(t, c, http.MethodGet, public+"/readyz", nil); r.status != http.StatusOK {
		t.Fatalf("/readyz before shutdown = %d", r.status)
	}

	done := shutdownAsync(context.Background(), s)
	recv(t, delayStarted, "shutdown delay to start")

	r := do(t, c, http.MethodGet, public+"/readyz", nil)
	if want := `{"status":"not_ready","reason":"shutting_down"}` + "\n"; r.status != http.StatusServiceUnavailable || r.body != want {
		t.Errorf("/readyz during shutdown = %d %q, want 503 %q", r.status, r.body, want)
	}
	if r := do(t, c, http.MethodGet, public+"/healthz", nil); r.status != http.StatusOK {
		t.Errorf("/healthz during the delay = %d, want 200", r.status)
	}
	if r := do(t, c, http.MethodGet, public+"/v1/work", nil); r.status != http.StatusOK || r.body != "worked" {
		t.Errorf("API route during the delay = %d %q", r.status, r.body)
	}
	if r := do(t, c, http.MethodGet, admin+"/metrics", nil); r.status != http.StatusOK {
		t.Errorf("admin /metrics during the delay = %d", r.status)
	}

	close(endDelay)
	if err := recv(t, done, "Shutdown"); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	started := logs.find(t, "shutdown started")
	if len(started) != 1 || started[0]["in_flight"] != float64(0) {
		t.Errorf("shutdown started lines = %v", started)
	}
	if len(logs.find(t, "shutdown complete")) != 1 {
		t.Error("no shutdown complete line")
	}
}

func TestShutdownDelay(t *testing.T) {
	t.Run("waits for the delay", func(t *testing.T) {
		const delay = 50 * time.Millisecond
		s, _ := newTestServer(t, func(o *Options) { o.ShutdownDelay = delay })
		startServer(t, s)
		start := time.Now()
		if err := s.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown = %v", err)
		}
		if elapsed := time.Since(start); elapsed < delay {
			t.Fatalf("Shutdown returned after %s, before the %s delay", elapsed, delay)
		}
	})
	t.Run("forceCtx skips the rest of the delay", func(t *testing.T) {
		s, _ := newTestServer(t, func(o *Options) { o.ShutdownDelay = time.Hour })
		startServer(t, s)
		ctx, cancel := context.WithCancel(context.Background())
		done := shutdownAsync(ctx, s)
		cancel()
		// No connections are open, so the forced drain has nothing to abort.
		if err := recv(t, done, "Shutdown"); err != nil {
			t.Fatalf("Shutdown = %v, want nil", err)
		}
	})
}

func TestShutdownWaitsForInFlightRequest(t *testing.T) {
	s, logs := newTestServer(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	s.Handle("GET /slow", slowHandler(entered, release))
	startServer(t, s)

	res := getAsync(newClient(t), "http://"+s.PublicAddr()+"/slow")
	recv(t, entered, "slow handler")

	done := shutdownAsync(context.Background(), s)
	waitListenerClosed(t, s.PublicAddr())
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned %v while a request was in flight", err)
	default:
	}

	close(release)
	r := recv(t, res, "slow response")
	if r.err != nil || r.resp.status != http.StatusOK || r.resp.body != "done" {
		t.Fatalf("slow request = %+v, want 200 done", r)
	}
	if err := recv(t, done, "Shutdown"); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	if started := logs.find(t, "shutdown started"); len(started) != 1 || started[0]["in_flight"] != float64(1) {
		t.Errorf("shutdown started lines = %v, want in_flight 1", started)
	}
}

func TestShutdownAbortsRequestsAfterGrace(t *testing.T) {
	const grace = 100 * time.Millisecond
	s, logs := newTestServer(t, func(o *Options) { o.ShutdownGrace = grace })
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	s.Handle("GET /slow", slowHandler(entered, release))
	startServer(t, s)

	res := getAsync(newClient(t), "http://"+s.PublicAddr()+"/slow")
	recv(t, entered, "slow handler")

	start := time.Now()
	err := recv(t, shutdownAsync(context.Background(), s), "Shutdown")
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Shutdown = %v, want ErrShutdownTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Errorf("Shutdown gave up after %s, before the %s grace period", elapsed, grace)
	}
	if r := recv(t, res, "aborted response"); r.err == nil {
		t.Errorf("aborted request succeeded: %+v", r.resp)
	}

	warn := logs.find(t, "shutdown aborted in-flight requests")
	want := map[string]any{"aborted": float64(1), "reason": "grace_period_exceeded", "level": "WARN"}
	if len(warn) != 1 {
		t.Fatalf("got %d abort warnings, want 1", len(warn))
	}
	got := map[string]any{"aborted": warn[0]["aborted"], "reason": warn[0]["reason"], "level": warn[0]["level"]}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("abort warning mismatch (-want +got):\n%s", diff)
	}

	// Later calls return the same result without running shutdown again.
	if err := s.Shutdown(context.Background()); !errors.Is(err, ErrShutdownTimeout) {
		t.Errorf("second Shutdown = %v, want ErrShutdownTimeout", err)
	}
	if n := len(logs.find(t, "shutdown started")); n != 1 {
		t.Errorf("shutdown started logged %d times, want 1", n)
	}
}

// TestShutdownWithIdleNewConnection covers a client that connected but sent
// nothing, such as a load balancer's pre-connect or a TCP health check.
// net/http counts such a connection as busy for 5s, so the grace period
// runs out, but no request was aborted and shutdown must still succeed.
func TestShutdownWithIdleNewConnection(t *testing.T) {
	const grace = time.Second
	s, logs := newTestServer(t, func(o *Options) { o.ShutdownGrace = grace })
	accepted := make(chan struct{})
	var once sync.Once
	s.public.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			once.Do(func() { close(accepted) })
		}
	}
	startServer(t, s)

	conn, err := net.Dial("tcp", s.PublicAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	recv(t, accepted, "the server to accept the connection")

	if err := recv(t, shutdownAsync(context.Background(), s), "Shutdown"); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	if warn := logs.find(t, "shutdown aborted in-flight requests"); len(warn) != 0 {
		t.Errorf("abort warnings = %v, want none", warn)
	}
	if n := len(logs.find(t, "shutdown complete")); n != 1 {
		t.Errorf("shutdown complete logged %d times, want 1", n)
	}
	// The connection was closed by the server.
	_ = conn.SetReadDeadline(time.Now().Add(testTimeout))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Error("connection still open after Shutdown")
	}
}

func TestShutdownForceCtxEndsGrace(t *testing.T) {
	s, logs := newTestServer(t, func(o *Options) { o.ShutdownGrace = time.Hour })
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	s.Handle("GET /slow", slowHandler(entered, release))
	startServer(t, s)

	res := getAsync(newClient(t), "http://"+s.PublicAddr()+"/slow")
	recv(t, entered, "slow handler")

	forceCtx, force := context.WithCancel(context.Background())
	done := shutdownAsync(forceCtx, s)
	waitListenerClosed(t, s.PublicAddr())
	force()

	if err := recv(t, done, "forced Shutdown"); !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Shutdown = %v, want ErrShutdownTimeout", err)
	}
	if r := recv(t, res, "aborted response"); r.err == nil {
		t.Errorf("aborted request succeeded: %+v", r.resp)
	}
	warn := logs.find(t, "shutdown aborted in-flight requests")
	if len(warn) != 1 || warn[0]["reason"] != "forced" || warn[0]["aborted"] != float64(1) {
		t.Errorf("abort warnings = %v, want one forced abort of 1 request", warn)
	}
}

func TestShutdownIsIdempotentAndConcurrent(t *testing.T) {
	s, logs := newTestServer(t, nil)
	startServer(t, s)

	const callers = 8
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() { results <- s.Shutdown(context.Background()) })
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("concurrent Shutdown = %v, want nil", err)
		}
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("repeated Shutdown = %v, want nil", err)
	}
	if n := len(logs.find(t, "shutdown started")); n != 1 {
		t.Errorf("shutdown started logged %d times, want 1", n)
	}
	if n := len(logs.find(t, "shutdown complete")); n != 1 {
		t.Errorf("shutdown complete logged %d times, want 1", n)
	}
	if err := s.Start(); err == nil {
		t.Error("Start after Shutdown succeeded")
	}
}

func TestShutdownBeforeStart(t *testing.T) {
	s, _ := newTestServer(t, nil)
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Start = %v, want nil", err)
	}
	if err := s.Start(); err == nil {
		t.Fatal("Start after Shutdown succeeded")
	}
}
