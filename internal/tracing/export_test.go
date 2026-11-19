package tracing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"switchyard/internal/config"
	"switchyard/internal/version"
)

// Credentials embedded in the collector URL by the tests.
const (
	collectorUser = "tenant-7f3a"
	collectorPass = "Zq8sUperSecretTracePass"
)

// containsAny reports whether s contains any of subs.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// discardExporter drops every span.
type discardExporter struct{}

func (discardExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (discardExporter) Shutdown(context.Context) error                             { return nil }

// collectorRequest is what the fake collector received in one export.
type collectorRequest struct {
	path, contentType, authorization, userAgent string
	// req is the decoded body. TracesData is wire-compatible with the
	// collector's ExportTraceServiceRequest.
	req *tracepb.TracesData
	err error
}

// fakeCollector is an OTLP/HTTP collector that decodes every export and
// answers with status.
func fakeCollector(t *testing.T, status int) (*httptest.Server, <-chan collectorRequest) {
	t.Helper()
	got := make(chan collectorRequest, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cr := collectorRequest{
			path: r.URL.Path, contentType: r.Header.Get("Content-Type"),
			authorization: r.Header.Get("Authorization"), userAgent: r.Header.Get("User-Agent"),
		}
		body, err := io.ReadAll(r.Body)
		if err == nil {
			cr.req = &tracepb.TracesData{}
			err = proto.Unmarshal(body, cr.req)
		}
		cr.err = err
		select {
		case got <- cr:
		default:
		}
		if status != http.StatusOK {
			http.Error(w, "rejected by the test collector", status)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// endpointWithCreds returns srv's URL with the test credentials and path.
func endpointWithCreds(srv *httptest.Server, path string) string {
	u, _ := url.Parse(srv.URL)
	u.User = url.UserPassword(collectorUser, collectorPass)
	u.Path = path
	return u.String()
}

func TestExportToCollector(t *testing.T) {
	srv, got := fakeCollector(t, http.StatusOK)
	cfg := config.Tracing{
		Enabled: true, Endpoint: config.NewSecret(endpointWithCreds(srv, "/otlp/")),
		SampleRatio: 1, ServiceName: testServiceName,
	}
	p, err := New(context.Background(), cfg, version.Info{Version: testServiceVersion}, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := p.Middleware("public", patternRoute("POST /v1/chat/completions"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, span := Start(r.Context(), SpanUsageRecord)
		span.End()
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if err := p.Shutdown(context.Background()); err != nil { // flushes the batch
		t.Fatalf("Shutdown: %v", err)
	}

	var cr collectorRequest
	select {
	case cr = <-got:
	default:
		t.Fatal("no export reached the collector by the end of Shutdown")
	}
	if cr.err != nil {
		t.Fatalf("decode the export: %v", cr.err)
	}
	if cr.path != "/otlp/v1/traces" || cr.contentType != "application/x-protobuf" {
		t.Errorf("export = %s (%s), want /otlp/v1/traces (application/x-protobuf)", cr.path, cr.contentType)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(collectorUser+":"+collectorPass))
	if cr.authorization != wantAuth {
		t.Errorf("Authorization = %q, want Basic credentials from the URL", cr.authorization)
	}
	if cr.userAgent != exporterUserAgent {
		t.Errorf("User-Agent = %q, want %q", cr.userAgent, exporterUserAgent)
	}
	if got := p.ExportStats(); got != (ExportStats{Exported: 2}) {
		t.Errorf("ExportStats = %+v, want 2 exported", got)
	}

	names := make(map[string]bool)
	res := make(map[string]string)
	for _, rs := range cr.req.GetResourceSpans() {
		for _, a := range rs.GetResource().GetAttributes() {
			res[a.GetKey()] = a.GetValue().GetStringValue()
		}
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				names[s.GetName()] = true
			}
		}
	}
	if !names["POST /v1/chat/completions"] || !names[SpanUsageRecord] {
		t.Errorf("spans received = %v, want the server span and %s", names, SpanUsageRecord)
	}
	if res["service.name"] != testServiceName || res["service.version"] != testServiceVersion || res["service.instance.id"] == "" {
		t.Errorf("resource = %v, want service.name, service.version and service.instance.id", res)
	}
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// syncBuffer is a bytes.Buffer safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestExportErrorsAreScrubbedAndRateLimited(t *testing.T) {
	srv, got := fakeCollector(t, http.StatusBadRequest) // not retried by the exporter
	u, _ := url.Parse(srv.URL)
	var logs syncBuffer
	clock := &fakeClock{now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	cfg := config.Tracing{
		Enabled: true, Endpoint: config.NewSecret(endpointWithCreds(srv, "/private-path")),
		SampleRatio: 1, ServiceName: testServiceName,
	}
	p, err := newProvider(context.Background(), cfg, version.Info{}, slog.New(slog.NewJSONHandler(&logs, nil)),
		options{syncExport: true, now: clock.Now})
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	export := func() {
		t.Helper()
		_, span := p.Tracer().Start(context.Background(), "root")
		span.End() // exported synchronously
		select {
		case <-got:
		default:
			t.Fatal("the export did not reach the collector")
		}
	}
	export()                        // logged
	export()                        // suppressed
	clock.Advance(errorLogInterval) // the next one is logged again
	export()

	var lines []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if strings.Contains(m["msg"].(string), "export failed") {
			lines = append(lines, m)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("logged %d export errors, want 2:\n%s", len(lines), logs.String())
	}
	if lines[0]["suppressed"] != 0.0 || lines[1]["suppressed"] != 1.0 {
		t.Errorf("suppressed = %v, %v; want 0, 1", lines[0]["suppressed"], lines[1]["suppressed"])
	}
	if !strings.Contains(lines[0]["error"].(string), "400") {
		t.Errorf("error = %q, want the collector's status", lines[0]["error"])
	}
	if out := logs.String(); containsAny(out, u.Host, u.Hostname(), collectorUser, collectorPass, "private-path") {
		t.Errorf("logs reveal the endpoint:\n%s", out)
	}
	if got := p.ExportStats(); got != (ExportStats{Dropped: 3}) {
		t.Errorf("ExportStats = %+v, want 3 dropped", got)
	}
}

// testSpans returns n ended spans from a throwaway SDK provider.
func testSpans(t *testing.T, n int) []sdktrace.ReadOnlySpan {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	for range n {
		_, span := tp.Tracer("t").Start(context.Background(), "s")
		span.End()
	}
	return rec.Ended()
}

func TestOTLPExporterOutcomes(t *testing.T) {
	// partialSuccess is an ExportTraceServiceResponse with partial_success
	// {rejected_spans: 1, error_message: "x"}: field 1 holding field 1 = 1
	// and field 2 = "x".
	partialSuccess := []byte{0x0a, 0x05, 0x08, 0x01, 0x12, 0x01, 'x'}
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		ctx        func() context.Context
		wantStats  ExportStats
		wantReport string // substring of the reported error; "" means none
	}{
		{"ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
			nil, ExportStats{Exported: 2}, ""},
		{"partial success is success", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", contentTypeProto)
			_, _ = w.Write(partialSuccess)
		}, nil, ExportStats{Exported: 2}, ""},
		{"server error, no retry", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
			nil, ExportStats{Dropped: 2}, "503 Service Unavailable"},
		{"redirect is not followed", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://elsewhere.invalid/v1/traces", http.StatusTemporaryRedirect)
		}, nil, ExportStats{Dropped: 2}, "307"},
		{"canceled context", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
			func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			}, ExportStats{Dropped: 2}, "context canceled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				tt.handler(w, r)
			}))
			defer srv.Close()
			ep, err := parseEndpoint(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			var reported []error
			e := newOTLPExporter(ep, func(err error) { reported = append(reported, err) })
			ctx := context.Background()
			if tt.ctx != nil {
				ctx = tt.ctx()
			}
			if err := e.ExportSpans(ctx, testSpans(t, 2)); err != nil {
				t.Errorf("ExportSpans returned %v; failures must be reported, not returned", err)
			}
			if got := e.stats(); got != tt.wantStats {
				t.Errorf("stats = %+v, want %+v", got, tt.wantStats)
			}
			switch {
			case tt.wantReport == "" && len(reported) != 0:
				t.Errorf("reported %v, want nothing", reported)
			case tt.wantReport != "" && (len(reported) != 1 || !strings.Contains(reported[0].Error(), tt.wantReport)):
				t.Errorf("reported %v, want one error containing %q", reported, tt.wantReport)
			}
			if n := calls.Load(); n > 1 {
				t.Errorf("the collector was called %d times, want at most 1 (no retries)", n)
			}
			if err := e.Shutdown(context.Background()); err != nil {
				t.Errorf("Shutdown: %v", err)
			}
		})
	}

	t.Run("after shutdown", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
		defer srv.Close()
		ep, _ := parseEndpoint(srv.URL)
		e := newOTLPExporter(ep, func(error) {})
		if err := e.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		_ = e.ExportSpans(context.Background(), testSpans(t, 3))
		if calls.Load() != 0 || e.stats() != (ExportStats{Dropped: 3}) {
			t.Errorf("after Shutdown: %d calls, stats %+v; want 0 calls, 3 dropped", calls.Load(), e.stats())
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := e.Shutdown(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("Shutdown(canceled ctx) = %v, want context.Canceled", err)
		}
		if err := e.ExportSpans(context.Background(), nil); err != nil {
			t.Errorf("ExportSpans(nil) = %v", err)
		}
	})
}

func TestErrorReporter(t *testing.T) {
	var logs syncBuffer
	clock := &fakeClock{now: time.Unix(0, 0)}
	ep, err := parseEndpoint("https://" + collectorUser + ":" + collectorPass + "@sink.example:4318/base")
	if err != nil {
		t.Fatal(err)
	}
	rep := newErrorReporter(slog.New(slog.NewTextHandler(&logs, nil)), clock.Now, ep.redactor)
	rep.report(nil) // ignored
	rep.report(errors.New(`Post "https://sink.example:4318/base/v1/traces": dial tcp sink.example:4318: refused; auth ` +
		ep.authorization + " user " + collectorUser))
	out := logs.String()
	if containsAny(out, "sink.example", collectorUser, collectorPass, "/base", ep.authorization) {
		t.Errorf("log reveals the endpoint: %s", out)
	}
	if !strings.Contains(out, "dial tcp") || !strings.Contains(out, redactedText) {
		t.Errorf("log lost the error: %s", out)
	}
	if got := rep.sanitize(errors.New("x sink.example:4318 y")).Error(); got != "x "+redactedText+" y" {
		t.Errorf("sanitize = %q", got)
	}
	if rep.sanitize(nil) != nil {
		t.Error("sanitize(nil) != nil")
	}
}

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		raw, url string
		auth     bool
	}{
		{"http://otel:4318", "http://otel:4318/v1/traces", false},
		{"https://otel.example", "https://otel.example/v1/traces", false},
		{"https://otel.example/", "https://otel.example/v1/traces", false},
		{"https://otel.example/otlp/", "https://otel.example/otlp/v1/traces", false},
		{"https://u:p@[::1]:4318/a", "https://[::1]:4318/a/v1/traces", true},
	}
	for _, tt := range tests {
		ep, err := parseEndpoint(tt.raw)
		if err != nil {
			t.Errorf("parseEndpoint(%q): %v", tt.raw, err)
			continue
		}
		if ep.url != tt.url || (ep.authorization != "") != tt.auth {
			t.Errorf("parseEndpoint(%q) = %+v, want url %s", tt.raw, ep, tt.url)
		}
	}
	for _, bad := range []string{"", "otel:4318", "grpc://otel:4317", "http://", "https://otel/?k=" + collectorPass, "https://otel/#" + collectorPass} {
		if _, err := parseEndpoint(bad); err == nil {
			t.Errorf("parseEndpoint(%q): want an error", bad)
		} else if strings.Contains(err.Error(), collectorPass) {
			t.Errorf("parseEndpoint error echoes the URL: %v", err)
		}
	}
}
