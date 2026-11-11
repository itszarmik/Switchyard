package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"switchyard/internal/reqctx"
)

func TestNewValidatesOptions(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Options)
		wantErr []string
	}{
		{name: "valid", mutate: func(*Options) {}},
		{name: "zero delay is allowed", mutate: func(o *Options) { o.ShutdownDelay = 0 }},
		{name: "missing listen", mutate: func(o *Options) { o.Listen = "" }, wantErr: []string{"Listen"}},
		{name: "missing admin listen", mutate: func(o *Options) { o.AdminListen = "" }, wantErr: []string{"AdminListen"}},
		{name: "zero body limit", mutate: func(o *Options) { o.MaxRequestBytes = 0 }, wantErr: []string{"MaxRequestBytes"}},
		{name: "negative header limit", mutate: func(o *Options) { o.MaxHeaderBytes = -1 }, wantErr: []string{"MaxHeaderBytes"}},
		{name: "zero header timeout", mutate: func(o *Options) { o.ReadHeaderTimeout = 0 }, wantErr: []string{"ReadHeaderTimeout"}},
		{name: "zero idle timeout", mutate: func(o *Options) { o.IdleTimeout = 0 }, wantErr: []string{"IdleTimeout"}},
		{name: "negative delay", mutate: func(o *Options) { o.ShutdownDelay = -time.Second }, wantErr: []string{"ShutdownDelay"}},
		{name: "zero grace", mutate: func(o *Options) { o.ShutdownGrace = 0 }, wantErr: []string{"ShutdownGrace"}},
		{name: "nil logger", mutate: func(o *Options) { o.Logger = nil }, wantErr: []string{"Logger"}},
		{name: "nil registry", mutate: func(o *Options) { o.Registry = nil }, wantErr: []string{"Registry"}},
		{
			name:   "every problem is reported",
			mutate: func(o *Options) { *o = Options{} },
			wantErr: []string{"Listen", "AdminListen", "MaxRequestBytes", "MaxHeaderBytes", "ReadHeaderTimeout",
				"IdleTimeout", "ShutdownGrace", "Logger", "Registry"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := testOptions(io.Discard)
			tt.mutate(&opts)
			_, err := New(opts)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("New succeeded, want an error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %s", err, want)
				}
			}
		})
	}
}

func TestNewRejectsRegistryReuse(t *testing.T) {
	opts := testOptions(io.Discard)
	if _, err := New(opts); err != nil {
		t.Fatal(err)
	}
	if _, err := New(opts); err == nil {
		t.Fatal("second New on the same registry succeeded, want a registration error")
	}
}

func TestRequestID(t *testing.T) {
	tests := []struct {
		name          string
		clientID      string
		wantClientLog bool
	}{
		{name: "no client id"},
		{name: "valid client id", clientID: "client-abc_123:XYZ", wantClientLog: true},
		{name: "128 characters", clientID: strings.Repeat("a", 128), wantClientLog: true},
		{name: "129 characters", clientID: strings.Repeat("a", 129)},
		{name: "contains a space", clientID: "has space"},
		{name: "control character", clientID: "a\x01b"},
		{name: "non-ASCII", clientID: "caf\u00e9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, logs := newTestServer(t, nil)
			var ctxID string
			s.Handle("GET /x", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctxID = reqctx.RequestID(r.Context())
				reqctx.Logger(r.Context()).Info("in handler")
				reqctx.Logger(r.Context()).With("k", "v").Warn("again in handler")
			}))

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tt.clientID != "" {
				req.Header.Set("X-Request-Id", tt.clientID)
			}
			rec := httptest.NewRecorder()
			s.publicHandler.ServeHTTP(rec, req)

			id := rec.Header().Get("X-Request-Id")
			u, err := uuid.Parse(id)
			if err != nil {
				t.Fatalf("X-Request-Id %q is not a UUID: %v", id, err)
			}
			if u.Version() != 7 {
				t.Errorf("X-Request-Id version = %d, want 7", u.Version())
			}
			if id == tt.clientID {
				t.Error("the client's request ID was adopted")
			}
			if ctxID != id {
				t.Errorf("reqctx.RequestID = %q, want %q", ctxID, id)
			}

			recs := logs.records(t)
			if len(recs) != 3 {
				t.Fatalf("got %d log records, want 3: %v", len(recs), recs)
			}
			for _, r := range recs {
				if r["request_id"] != id {
					t.Errorf("record %q has request_id %v, want %s", r["msg"], r["request_id"], id)
				}
				cid, ok := r["client_request_id"]
				if tt.wantClientLog && cid != tt.clientID {
					t.Errorf("record %q has client_request_id %v, want %q", r["msg"], cid, tt.clientID)
				}
				if !tt.wantClientLog && ok {
					t.Errorf("record %q has client_request_id %v, want none", r["msg"], cid)
				}
			}
		})
	}
}

func TestRequestIDsAreUnique(t *testing.T) {
	s, _ := newTestServer(t, nil)
	seen := map[string]bool{}
	for range 100 {
		rec := httptest.NewRecorder()
		s.publicHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		id := rec.Header().Get("X-Request-Id")
		if seen[id] {
			t.Fatalf("request ID %s repeated", id)
		}
		seen[id] = true
	}
}

func serve(h http.Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, body))
	return rec
}

func TestBuiltinRoutes(t *testing.T) {
	s, _ := newTestServer(t, nil)
	tests := []struct {
		name       string
		admin      bool
		method     string
		path       string
		wantStatus int
		wantBody   string // exact, when set
		wantInBody string // substring, when set
		wantType   string
	}{
		{
			name: "healthz", method: http.MethodGet, path: "/healthz",
			wantStatus: http.StatusOK, wantBody: `{"status":"ok"}` + "\n", wantType: "application/json; charset=utf-8",
		},
		{
			name: "readyz", method: http.MethodGet, path: "/readyz",
			wantStatus: http.StatusOK, wantBody: `{"status":"ready","config_version":"cfg-0123abcd"}` + "\n",
			wantType: "application/json; charset=utf-8",
		},
		{
			name: "unknown path", method: http.MethodGet, path: "/v1/nope?x=1",
			wantStatus: http.StatusNotFound,
			wantBody:   `{"error":{"message":"Invalid URL (GET /v1/nope)","type":"invalid_request_error","param":null,"code":null}}` + "\n",
			wantType:   "application/json; charset=utf-8",
		},
		{
			name: "wrong method on a built-in route", method: http.MethodPost, path: "/healthz",
			wantStatus: http.StatusNotFound,
			wantBody:   `{"error":{"message":"Invalid URL (POST /healthz)","type":"invalid_request_error","param":null,"code":null}}` + "\n",
			wantType:   "application/json; charset=utf-8",
		},
		{
			name: "metrics is not on the public listener", method: http.MethodGet, path: "/metrics",
			wantStatus: http.StatusNotFound,
			wantBody:   `{"error":{"message":"Invalid URL (GET /metrics)","type":"invalid_request_error","param":null,"code":null}}` + "\n",
			wantType:   "application/json; charset=utf-8",
		},
		{
			name: "admin metrics", admin: true, method: http.MethodGet, path: "/metrics",
			wantStatus: http.StatusOK, wantInBody: "# TYPE switchyard_http_requests_total counter",
		},
		{
			name: "admin unknown path", admin: true, method: http.MethodGet, path: "/admin/v1/nope",
			wantStatus: http.StatusNotFound,
			wantBody:   `{"error":{"message":"Invalid URL (GET /admin/v1/nope)","type":"invalid_request_error","param":null,"code":null}}` + "\n",
			wantType:   "application/json; charset=utf-8",
		},
		{
			name: "health is not on the admin listener", admin: true, method: http.MethodGet, path: "/healthz",
			wantStatus: http.StatusNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := s.publicHandler
			if tt.admin {
				h = s.adminHandler
			}
			rec := serve(h, tt.method, tt.path, nil)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("body mismatch (-want +got):\n%s", cmp.Diff(tt.wantBody, rec.Body.String()))
			}
			if tt.wantInBody != "" && !strings.Contains(rec.Body.String(), tt.wantInBody) {
				t.Errorf("body lacks %q:\n%s", tt.wantInBody, rec.Body.String())
			}
			if tt.wantType != "" && rec.Header().Get("Content-Type") != tt.wantType {
				t.Errorf("Content-Type = %q, want %q", rec.Header().Get("Content-Type"), tt.wantType)
			}
		})
	}
}

func TestReadinessChecks(t *testing.T) {
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })

	ok := func(context.Context) error { return nil }
	fail := func(context.Context) error { return errors.New("connection refused") }
	waitCtx := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	ignoreCtx := func(context.Context) error { <-never; return nil }
	panics := func(context.Context) error { panic("boom") }

	type check struct {
		name string
		fn   func(context.Context) error
	}
	tests := []struct {
		name       string
		checks     []check
		wantStatus int
		wantBody   string
	}{
		{
			name:       "all pass",
			checks:     []check{{"cache", ok}, {"db", ok}},
			wantStatus: http.StatusOK,
			wantBody:   `{"status":"ready","config_version":"cfg-0123abcd"}` + "\n",
		},
		{
			name:       "one fails",
			checks:     []check{{"cache", ok}, {"db", fail}},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"status":"not_ready","failed_checks":["db"]}` + "\n",
		},
		{
			name:       "failures in registration order",
			checks:     []check{{"state", fail}, {"cache", ok}, {"db", fail}},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"status":"not_ready","failed_checks":["state","db"]}` + "\n",
		},
		{
			name:       "timeout honored by the check",
			checks:     []check{{"slow", waitCtx}, {"db", ok}},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"status":"not_ready","failed_checks":["slow"]}` + "\n",
		},
		{
			name:       "check ignoring its context",
			checks:     []check{{"stuck", ignoreCtx}},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"status":"not_ready","failed_checks":["stuck"]}` + "\n",
		},
		{
			name:       "panicking check",
			checks:     []check{{"broken", panics}},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"status":"not_ready","failed_checks":["broken"]}` + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, logs := newTestServer(t, nil)
			s.checkTimeout = 50 * time.Millisecond
			for _, c := range tt.checks {
				s.AddReadinessCheck(c.name, c.fn)
			}
			rec := serve(s.publicHandler, http.MethodGet, "/readyz", nil)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if rec.Body.String() != tt.wantBody {
				t.Errorf("body mismatch (-want +got):\n%s", cmp.Diff(tt.wantBody, rec.Body.String()))
			}
			if tt.wantStatus != http.StatusOK && len(logs.find(t, "readiness check failed")) == 0 {
				t.Error("no log line for the failed check")
			}
		})
	}
}

func TestReadinessChecksRunConcurrently(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// Each check waits for the other to start, so they only pass when run
	// at the same time.
	a, b := make(chan struct{}), make(chan struct{})
	s.AddReadinessCheck("a", func(ctx context.Context) error {
		close(a)
		select {
		case <-b:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	s.AddReadinessCheck("b", func(ctx context.Context) error {
		close(b)
		select {
		case <-a:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if rec := serve(s.publicHandler, http.MethodGet, "/readyz", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", rec.Code, rec.Body)
	}
}

func TestPanicRecovery(t *testing.T) {
	s, logs := newTestServer(t, nil)
	s.Handle("GET /panic", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	}))

	rec := serve(s.publicHandler, http.MethodGet, "/panic", nil)
	id := rec.Header().Get("X-Request-Id")
	want := `{"error":{"message":"The server had an error while processing your request. Request ID: ` + id +
		`","type":"server_error","param":null,"code":"internal_error"}}` + "\n"
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if rec.Body.String() != want {
		t.Errorf("body mismatch (-want +got):\n%s", cmp.Diff(want, rec.Body.String()))
	}

	panics := logs.find(t, "panic serving request")
	if len(panics) != 1 {
		t.Fatalf("got %d panic log lines, want 1", len(panics))
	}
	p := panics[0]
	if p["level"] != "ERROR" || p["request_id"] != id || p["panic"] != "kaboom" {
		t.Errorf("panic log line = %v", p)
	}
	if stack, _ := p["stack"].(string); !strings.Contains(stack, "goroutine") {
		t.Errorf("panic log line has no stack: %v", p["stack"])
	}
	access := logs.find(t, "request")
	if len(access) != 1 || access[0]["status"] != float64(500) {
		t.Errorf("access log = %v, want one line with status 500", access)
	}
	checkNothingInFlight(t, s)
}

// checkNothingInFlight fails the test unless the in-flight gauge and counter
// are back to zero.
func checkNothingInFlight(t *testing.T, s *Server) {
	t.Helper()
	if got := testutil.ToFloat64(s.metrics.inFlight.WithLabelValues("public")); got != 0 {
		t.Errorf("in-flight gauge = %v, want 0", got)
	}
	if got := s.inFlight.Load(); got != 0 {
		t.Errorf("in-flight counter = %d, want 0", got)
	}
}

func TestPanicAborts(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantLogged bool
	}{
		{
			name:    "ErrAbortHandler is re-panicked",
			handler: func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) },
		},
		{
			name: "panic after headers were sent aborts the connection",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("partial"))
				panic("late")
			},
			wantLogged: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, logs := newTestServer(t, nil)
			s.Handle("GET /p", tt.handler)
			rec := httptest.NewRecorder()
			func() {
				defer func() {
					if err, _ := recover().(error); !errors.Is(err, http.ErrAbortHandler) {
						t.Errorf("recovered %v, want http.ErrAbortHandler", err)
					}
				}()
				s.publicHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/p", nil))
			}()
			if got := len(logs.find(t, "panic serving request")) == 1; got != tt.wantLogged {
				t.Errorf("panic logged = %v, want %v", got, tt.wantLogged)
			}
			access := logs.find(t, "request")
			if len(access) != 1 || access[0]["aborted"] != true {
				t.Errorf("access log = %v, want one aborted request", access)
			}
			checkNothingInFlight(t, s)
		})
	}
}

func TestBodyLimit(t *testing.T) {
	const limit = 16
	tests := []struct {
		name          string
		body          string
		chunked       bool
		wantStatus    int
		wantHandlerIn bool
	}{
		{name: "under the limit", body: strings.Repeat("a", limit), wantStatus: http.StatusOK, wantHandlerIn: true},
		{name: "content-length over the limit", body: strings.Repeat("a", limit+1), wantStatus: http.StatusRequestEntityTooLarge},
		{name: "chunked under the limit", body: "small", chunked: true, wantStatus: http.StatusOK, wantHandlerIn: true},
		{name: "chunked over the limit", body: strings.Repeat("a", 100), chunked: true, wantStatus: http.StatusRequestEntityTooLarge, wantHandlerIn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestServer(t, func(o *Options) { o.MaxRequestBytes = limit })
			handlerIn := false
			s.Handle("POST /echo", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handlerIn = true
				b, err := io.ReadAll(r.Body)
				if IsRequestTooLarge(err) {
					WriteRequestTooLarge(w, limit)
					return
				}
				if err != nil {
					t.Errorf("read body: %v", err)
				}
				_, _ = w.Write(b)
			}))

			req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(tt.body))
			if tt.chunked {
				req.ContentLength = -1
			}
			rec := httptest.NewRecorder()
			s.publicHandler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if handlerIn != tt.wantHandlerIn {
				t.Errorf("handler called = %v, want %v", handlerIn, tt.wantHandlerIn)
			}
			if tt.wantStatus == http.StatusRequestEntityTooLarge {
				want := `{"error":{"message":"Request body too large: the limit is 16 bytes.","type":"invalid_request_error","param":null,"code":"request_too_large"}}` + "\n"
				if rec.Body.String() != want {
					t.Errorf("body mismatch (-want +got):\n%s", cmp.Diff(want, rec.Body.String()))
				}
			} else if rec.Body.String() != tt.body {
				t.Errorf("echoed %q, want %q", rec.Body.String(), tt.body)
			}
		})
	}
}

func TestIsRequestTooLarge(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"other", io.ErrUnexpectedEOF, false},
		{"max bytes", &http.MaxBytesError{Limit: 1}, true},
		{"wrapped", errors.Join(errors.New("decode"), &http.MaxBytesError{Limit: 1}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRequestTooLarge(tt.err); got != tt.want {
				t.Fatalf("IsRequestTooLarge(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestLimitsOverTheWire checks the body and header limits through real
// listeners, where net/http itself enforces part of them.
func TestLimitsOverTheWire(t *testing.T) {
	s, _ := newTestServer(t, func(o *Options) {
		o.MaxRequestBytes = 16
		o.MaxHeaderBytes = 1024
	})
	s.Handle("POST /echo", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			if IsRequestTooLarge(err) {
				WriteRequestTooLarge(w, 16)
				return
			}
			t.Errorf("read body: %v", err)
		}
	}))
	startServer(t, s)
	c := newClient(t)
	base := "http://" + s.PublicAddr()

	t.Run("content-length", func(t *testing.T) {
		resp := do(t, c, http.MethodPost, base+"/echo", strings.NewReader(strings.Repeat("a", 1000)))
		if resp.status != http.StatusRequestEntityTooLarge || !strings.Contains(resp.body, "request_too_large") {
			t.Fatalf("got %d %s, want 413 request_too_large", resp.status, resp.body)
		}
	})
	t.Run("chunked", func(t *testing.T) {
		// io.MultiReader hides the length, so the client sends it chunked.
		resp := do(t, c, http.MethodPost, base+"/echo", io.MultiReader(strings.NewReader(strings.Repeat("a", 1000))))
		if resp.status != http.StatusRequestEntityTooLarge || !strings.Contains(resp.body, "request_too_large") {
			t.Fatalf("got %d %s, want 413 request_too_large", resp.status, resp.body)
		}
	})
	t.Run("headers", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, base+"/healthz", nil)
		// net/http allows MaxHeaderBytes plus 4096 bytes of slack.
		req.Header.Set("X-Big", strings.Repeat("b", 16<<10))
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("status = %d, want 431", resp.StatusCode)
		}
	})
}

func TestAccessLog(t *testing.T) {
	tests := []struct {
		name      string
		admin     bool
		method    string
		target    string
		wantLevel string
		wantPath  string
		status    int
	}{
		{name: "api request", method: http.MethodGet, target: "/v1/items/42?api_key=supersecret&x=1", wantLevel: "INFO", wantPath: "/v1/items/42", status: 200},
		{name: "not found", method: http.MethodPost, target: "/nope", wantLevel: "INFO", wantPath: "/nope", status: 404},
		{name: "healthz probe", method: http.MethodGet, target: "/healthz", wantLevel: "DEBUG", wantPath: "/healthz", status: 200},
		{name: "readyz probe", method: http.MethodGet, target: "/readyz", wantLevel: "DEBUG", wantPath: "/readyz", status: 200},
		{name: "healthz with wrong method", method: http.MethodPost, target: "/healthz", wantLevel: "INFO", wantPath: "/healthz", status: 404},
		{name: "metrics scrape", admin: true, method: http.MethodGet, target: "/metrics", wantLevel: "DEBUG", wantPath: "/metrics", status: 200},
		{name: "admin not found", admin: true, method: http.MethodGet, target: "/healthz", wantLevel: "INFO", wantPath: "/healthz", status: 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, logs := newTestServer(t, nil)
			s.Handle("GET /v1/items/{id}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("hello"))
			}))
			h, listener := s.publicHandler, "public"
			if tt.admin {
				h, listener = s.adminHandler, "admin"
			}
			req := httptest.NewRequest(tt.method, tt.target, nil)
			req.Header.Set("User-Agent", strings.Repeat("u", 300))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			lines := logs.find(t, "request")
			if len(lines) != 1 {
				t.Fatalf("got %d access log lines, want 1", len(lines))
			}
			l := lines[0]
			want := map[string]any{
				"level":      tt.wantLevel,
				"msg":        "request",
				"request_id": rec.Header().Get("X-Request-Id"),
				"method":     tt.method,
				"path":       tt.wantPath,
				"status":     float64(tt.status),
				"bytes":      float64(rec.Body.Len()),
				"remote_ip":  "192.0.2.1",
				"user_agent": strings.Repeat("u", 256),
				"listener":   listener,
			}
			if d, ok := l["duration_ms"].(float64); !ok || d < 0 {
				t.Errorf("duration_ms = %v, want a non-negative number", l["duration_ms"])
			}
			delete(l, "duration_ms")
			delete(l, "time")
			if diff := cmp.Diff(want, l); diff != "" {
				t.Errorf("access log mismatch (-want +got):\n%s", diff)
			}
			if out := logs.String(); strings.Contains(out, "supersecret") || strings.Contains(out, "x=1") {
				t.Errorf("query string was logged: %s", out)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly", 7, "exactly"},
		{"toolong", 3, "too"},
		{"héllo wörld", 4, "héll"},
		{"日本語テキスト", 3, "日本語"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := truncate(tt.in, tt.n); got != tt.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
		})
	}
}

func TestMetrics(t *testing.T) {
	s, _ := newTestServer(t, nil)
	block, entered := make(chan struct{}), make(chan struct{})
	s.Handle("GET /v1/items/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "block" {
			close(entered)
			<-block
		}
		w.WriteHeader(http.StatusCreated)
	}))

	serve(s.publicHandler, http.MethodGet, "/v1/items/1", nil)
	serve(s.publicHandler, http.MethodGet, "/v1/items/2", nil)
	serve(s.publicHandler, "PURGE", "/v1/items/3", nil)
	serve(s.publicHandler, http.MethodGet, "/nope", nil)
	serve(s.publicHandler, http.MethodGet, "/healthz", nil)
	serve(s.adminHandler, http.MethodGet, "/metrics", nil)

	tests := []struct {
		labels []string
		want   float64
	}{
		{[]string{"public", "GET /v1/items/{id}", "GET", "201"}, 2},
		{[]string{"public", "unmatched", "OTHER", "404"}, 1},
		{[]string{"public", "unmatched", "GET", "404"}, 1},
		{[]string{"public", "GET /healthz", "GET", "200"}, 1},
		{[]string{"admin", "GET /metrics", "GET", "200"}, 1},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.labels, ","), func(t *testing.T) {
			if got := testutil.ToFloat64(s.metrics.requests.WithLabelValues(tt.labels...)); got != tt.want {
				t.Fatalf("requests_total%v = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
	if n := testutil.CollectAndCount(s.metrics.requests); n != len(tests) {
		t.Errorf("requests_total has %d series, want %d", n, len(tests))
	}
	// One histogram per (listener, route, method).
	if n := testutil.CollectAndCount(s.metrics.duration); n != 5 {
		t.Errorf("request_duration_seconds has %d series, want 5", n)
	}

	inFlight := s.metrics.inFlight.WithLabelValues("public")
	done := make(chan struct{})
	go func() {
		defer close(done)
		serve(s.publicHandler, http.MethodGet, "/v1/items/block", nil)
	}()
	recv(t, entered, "handler to start")
	if got := testutil.ToFloat64(inFlight); got != 1 {
		t.Errorf("in-flight while a request runs = %v, want 1", got)
	}
	close(block)
	recv(t, done, "request to finish")
	if got := testutil.ToFloat64(inFlight); got != 0 {
		t.Errorf("in-flight after the request = %v, want 0", got)
	}

	// The metrics are on the registry served at admin /metrics.
	body := serve(s.adminHandler, http.MethodGet, "/metrics", nil).Body.String()
	for _, want := range []string{
		`switchyard_http_requests_total{code="201",listener="public",method="GET",route="GET /v1/items/{id}"} 3`,
		`switchyard_http_request_duration_seconds_bucket{listener="public",method="GET",route="GET /v1/items/{id}",le="120"}`,
		`switchyard_http_requests_in_flight{listener="admin"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}

func TestNormalizeMethod(t *testing.T) {
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "CONNECT", "OPTIONS", "TRACE"} {
		if got := normalizeMethod(m); got != m {
			t.Errorf("normalizeMethod(%q) = %q", m, got)
		}
	}
	for _, m := range []string{"get", "PURGE", "PROPFIND", ""} {
		if got := normalizeMethod(m); got != "OTHER" {
			t.Errorf("normalizeMethod(%q) = %q, want OTHER", m, got)
		}
	}
}

func TestStreaming(t *testing.T) {
	s, logs := newTestServer(t, nil)
	release := make(chan struct{})
	s.Handle("GET /stream", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		_, _ = io.WriteString(w, "data: first\n\n")
		if err := rc.Flush(); err != nil {
			t.Errorf("Flush: %v", err)
		}
		if err := rc.SetWriteDeadline(time.Now().Add(testTimeout)); err != nil {
			t.Errorf("SetWriteDeadline through the wrapper: %v", err)
		}
		<-release
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	startServer(t, s)

	resp, err := newClient(t).Get("http://" + s.PublicAddr() + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The first event arrives while the handler is still blocked, so it was
	// flushed through the middleware.
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first line = %q, %v", line, err)
	}
	close(release)
	rest, err := io.ReadAll(br)
	if err != nil || string(rest) != "\ndata: second\n\n" {
		t.Fatalf("rest = %q, %v", rest, err)
	}

	// The access log line is written after the handler returns, which can
	// be just after the client has read the last byte.
	deadline := time.Now().Add(testTimeout)
	for len(logs.find(t, "request")) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	lines := logs.find(t, "request")
	if len(lines) != 1 || lines[0]["bytes"] != float64(len("data: first\n\ndata: second\n\n")) || lines[0]["status"] != float64(200) {
		t.Errorf("access log = %v", lines)
	}
}

func TestResponseWriterFlushWithoutFlusher(t *testing.T) {
	// A writer that cannot flush: Flush must not panic, FlushError reports it.
	rw := &responseWriter{ResponseWriter: struct{ http.ResponseWriter }{httptest.NewRecorder()}}
	rw.Flush()
	if err := rw.FlushError(); !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("FlushError = %v, want ErrNotSupported", err)
	}
	if rw.HeaderWritten() {
		t.Fatal("a failed flush marked the headers as written")
	}
}

func TestListeningLogAndAddrs(t *testing.T) {
	s, logs := newTestServer(t, nil)
	if s.PublicAddr() != "" || s.AdminAddr() != "" {
		t.Fatal("addresses set before Start")
	}
	startServer(t, s)

	lines := logs.find(t, "listening")
	if len(lines) != 1 {
		t.Fatalf("got %d listening lines, want 1", len(lines))
	}
	if lines[0]["public_addr"] != s.PublicAddr() || lines[0]["admin_addr"] != s.AdminAddr() {
		t.Errorf("listening line %v does not match %s, %s", lines[0], s.PublicAddr(), s.AdminAddr())
	}
	for _, addr := range []string{s.PublicAddr(), s.AdminAddr()} {
		if _, port, err := net.SplitHostPort(addr); err != nil || port == "0" {
			t.Errorf("address %q is not a bound host:port", addr)
		}
	}

	c := newClient(t)
	if r := do(t, c, http.MethodGet, "http://"+s.PublicAddr()+"/healthz", nil); r.status != http.StatusOK {
		t.Errorf("public /healthz = %d", r.status)
	}
	if r := do(t, c, http.MethodGet, "http://"+s.AdminAddr()+"/metrics", nil); r.status != http.StatusOK {
		t.Errorf("admin /metrics = %d", r.status)
	}
	if r := do(t, c, http.MethodGet, "http://"+s.PublicAddr()+"/metrics", nil); r.status != http.StatusNotFound {
		t.Errorf("public /metrics = %d, want 404", r.status)
	}
	if err := s.Start(); err == nil {
		t.Error("second Start succeeded")
	}
}

func TestRegistrationAfterStartPanics(t *testing.T) {
	s, _ := newTestServer(t, nil)
	startServer(t, s)
	tests := []struct {
		name string
		fn   func()
	}{
		{"Handle", func() { s.Handle("GET /late", http.NotFoundHandler()) }},
		{"HandleAdmin", func() { s.HandleAdmin("GET /late", http.NotFoundHandler()) }},
		{"AddReadinessCheck", func() { s.AddReadinessCheck("late", func(context.Context) error { return nil }) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("no panic")
				}
			}()
			tt.fn()
		})
	}
}

func TestHandleAdmin(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.HandleAdmin("GET /admin/v1/ping", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}))
	if rec := serve(s.adminHandler, http.MethodGet, "/admin/v1/ping", nil); rec.Body.String() != "pong" {
		t.Fatalf("admin route body = %q", rec.Body.String())
	}
	if rec := serve(s.publicHandler, http.MethodGet, "/admin/v1/ping", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("admin route on public listener = %d, want 404", rec.Code)
	}
}

func TestStartBindFailureClosesOtherListener(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	s, _ := newTestServer(t, func(o *Options) { o.AdminListen = busy.Addr().String() })
	var public net.Listener
	s.listen = func(network, addr string) (net.Listener, error) {
		ln, err := net.Listen(network, addr)
		if addr == "127.0.0.1:0" {
			public = ln
		}
		return ln, err
	}

	err = s.Start()
	if err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("Start error = %v, want an admin listen error", err)
	}
	if public == nil {
		t.Fatal("public listener was never opened")
	}
	if _, err := public.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("public listener Accept = %v, want net.ErrClosed", err)
	}
	if s.PublicAddr() != "" {
		t.Errorf("PublicAddr = %q after a failed Start", s.PublicAddr())
	}
}

// failingListener fails Accept with a permanent error.
type failingListener struct{ err error }

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (l failingListener) Close() error              { return nil }
func (l failingListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}

func TestRunReturnsListenerError(t *testing.T) {
	errAccept := errors.New("accept failed")
	s, logs := newTestServer(t, func(o *Options) {
		o.Listen = "fake:1"
		o.ShutdownDelay = time.Hour // skipped after a listener failure
	})
	s.listen = func(network, addr string) (net.Listener, error) {
		if addr == "fake:1" {
			return failingListener{err: errAccept}, nil
		}
		return net.Listen(network, addr)
	}

	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background(), context.Background()) }()
	err := recv(t, done, "Run to return")
	if !errors.Is(err, errAccept) {
		t.Fatalf("Run = %v, want the accept error", err)
	}
	if len(logs.find(t, "listener failed; shutting down")) != 1 || len(logs.find(t, "shutdown complete")) != 1 {
		t.Errorf("missing failure or shutdown log lines: %s", logs.String())
	}
}

func TestRunStopsWhenContextDone(t *testing.T) {
	s, logs := newTestServer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx, context.Background()); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	for _, msg := range []string{"listening", "shutdown started", "shutdown complete"} {
		if len(logs.find(t, msg)) != 1 {
			t.Errorf("want one %q log line: %s", msg, logs.String())
		}
	}
}

func TestRunStartError(t *testing.T) {
	s, _ := newTestServer(t, func(o *Options) { o.Listen = "256.0.0.1:0" })
	if err := s.Run(context.Background(), context.Background()); err == nil || !strings.Contains(err.Error(), "public") {
		t.Fatalf("Run = %v, want a public listen error", err)
	}
}

// TestHandlerLoggerIsRequestScoped runs concurrent requests and checks that
// each handler's log lines carry that request's own ID.
func TestHandlerLoggerIsRequestScoped(t *testing.T) {
	s, logs := newTestServer(t, nil)
	s.Handle("GET /log", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reqctx.Logger(r.Context()).Info("handler", "own_id", reqctx.RequestID(r.Context()))
	}))

	const n = 20
	done := make(chan struct{}, n)
	for range n {
		go func() {
			serve(s.publicHandler, http.MethodGet, "/log", nil)
			done <- struct{}{}
		}()
	}
	for range n {
		recv(t, done, "requests")
	}
	lines := logs.find(t, "handler")
	if len(lines) != n {
		t.Fatalf("got %d handler lines, want %d", len(lines), n)
	}
	for _, l := range lines {
		if l["request_id"] == nil || l["request_id"] != l["own_id"] {
			t.Errorf("line %v carries the wrong request_id", l)
		}
	}
}

// TestBodyLimitStalledBody checks that a Content-Length over the limit gets
// its 413 at once, even when the client then stalls its body, and that the
// connection is closed rather than held while net/http drains the body.
func TestBodyLimitStalledBody(t *testing.T) {
	s, _ := newTestServer(t, func(o *Options) { o.MaxRequestBytes = 16 })
	s.Handle("POST /echo", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler ran for a body over the limit")
	}))
	startServer(t, s)

	conn, err := net.Dial("tcp", s.PublicAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Over the limit but under the 256 KiB net/http would wait for.
	if _, err := io.WriteString(conn, "POST /echo HTTP/1.1\r\nHost: x\r\nContent-Length: 100000\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("no response while the body is stalled: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read the 413 body: %v", err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !resp.Close {
		t.Errorf("status %d, close %v; want 413 and Connection: close", resp.StatusCode, resp.Close)
	}
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("after the 413, read = %v; want io.EOF (connection closed)", err)
	}
	waitFor(t, "no request in flight", func() bool { return s.inFlight.Load() == 0 })
}

// TestAccessLogAnnotations checks that what handlers annotate lands on the
// request's one access-log line, after the server's own fields.
func TestAccessLogAnnotations(t *testing.T) {
	s, logs := newTestServer(t, nil)
	s.Handle("POST /v1/work", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqctx.Annotate(r.Context(), slog.String("tenant", "t1"))
		// A derived context, as middleware passes on, still reaches it.
		ctx := reqctx.WithLogger(r.Context(), reqctx.Logger(r.Context()).With("x", 1))
		reqctx.Annotate(ctx, slog.Int64("prompt_tokens", 5), slog.String("error_code", ""))
		w.WriteHeader(http.StatusAccepted)
	}))
	rec := serve(s.publicHandler, http.MethodPost, "/v1/work", strings.NewReader("{}"))

	lines := logs.find(t, "request")
	if len(lines) != 1 {
		t.Fatalf("got %d access log lines, want 1: %s", len(lines), logs)
	}
	l := lines[0]
	for k, want := range map[string]any{
		"request_id": rec.Header().Get("X-Request-Id"), "status": float64(http.StatusAccepted), "path": "/v1/work",
		"tenant": "t1", "prompt_tokens": float64(5), "error_code": "",
	} {
		if l[k] != want {
			t.Errorf("%s = %v, want %v (line %v)", k, l[k], want, l)
		}
	}
	if n := len(logs.records(t)); n != 1 {
		t.Errorf("got %d log lines, want only the access line", n)
	}
}
