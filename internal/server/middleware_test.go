package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"switchyard/internal/config"
	"switchyard/internal/reqctx"
	"switchyard/internal/tracing"
	"switchyard/internal/version"
)

// TestRequestLoggerTraceIDs: with a valid trace context in the request
// context, as the tracing middleware outside the chain leaves it, the
// request logger and so the access log carry trace_id and span_id; without
// one, neither field appears.
func TestRequestLoggerTraceIDs(t *testing.T) {
	const (
		traceHex = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanHex  = "00f067aa0ba902b7"
	)
	traceID, err := trace.TraceIDFromHex(traceHex)
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex(spanHex)
	if err != nil {
		t.Fatal(err)
	}
	valid := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true})

	tests := []struct {
		name      string
		sc        trace.SpanContext
		wantTrace bool
	}{
		{name: "valid remote span context", sc: valid, wantTrace: true},
		{name: "no span context", sc: trace.SpanContext{}},
		{name: "invalid span context", sc: trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID}), wantTrace: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, logs := newTestServer(t, nil)
			s.Handle("GET /x", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				reqctx.Logger(r.Context()).Info("in handler")
			}))
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tt.sc.IsValid() || tt.sc.HasTraceID() {
				req = req.WithContext(trace.ContextWithRemoteSpanContext(req.Context(), tt.sc))
			}
			s.publicHandler.ServeHTTP(httptest.NewRecorder(), req)

			recs := logs.records(t)
			if len(recs) != 2 {
				t.Fatalf("got %d records, want the handler line and the access log: %v", len(recs), recs)
			}
			for _, r := range recs {
				gotTrace, hasTrace := r["trace_id"]
				gotSpan, hasSpan := r["span_id"]
				if tt.wantTrace {
					if gotTrace != traceHex || gotSpan != spanHex {
						t.Errorf("record %q: trace_id %v, span_id %v; want %s, %s", r["msg"], gotTrace, gotSpan, traceHex, spanHex)
					}
					continue
				}
				if hasTrace || hasSpan {
					t.Errorf("record %q has trace_id %v, span_id %v; want neither", r["msg"], gotTrace, gotSpan)
				}
			}
		})
	}
}

// TestChainTracing: with Options.Tracing, the tracing middleware is the
// outermost layer, so a client's traceparent reaches the request logger
// and the access log even with tracing disabled; without it, the header
// is not read.
func TestChainTracing(t *testing.T) {
	const (
		traceHex    = "4bf92f3577b34da6a3ce929d0e0e4736"
		traceparent = "00-" + traceHex + "-00f067aa0ba902b7-01"
	)
	prov, err := tracing.New(context.Background(), config.Tracing{}, version.Info{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		tracing   *tracing.Provider
		wantTrace bool
	}{
		{"with a provider", prov, true},
		{"without", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, logs := newTestServer(t, func(o *Options) { o.Tracing = tt.tracing })
			s.Handle("GET /x", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				reqctx.Logger(r.Context()).Info("in handler")
			}))
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Traceparent", traceparent)
			s.publicHandler.ServeHTTP(httptest.NewRecorder(), req)
			recs := logs.records(t)
			if len(recs) != 2 {
				t.Fatalf("got %d records, want 2: %v", len(recs), recs)
			}
			for _, r := range recs {
				if got, ok := r["trace_id"]; ok != tt.wantTrace || (ok && got != traceHex) {
					t.Errorf("record %q: trace_id %v (present %v), want present %v", r["msg"], got, ok, tt.wantTrace)
				}
			}
		})
	}
}

// TestMuxRoute: the tracing route function names the pattern a request
// matches, and nothing for the catch-all.
func TestMuxRoute(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc(catchAllPattern, func(http.ResponseWriter, *http.Request) {})
	route := muxRoute(mux)
	tests := []struct{ method, path, want string }{
		{http.MethodPost, "/v1/chat/completions", "POST /v1/chat/completions"},
		{http.MethodGet, "/nope", ""},
	}
	for _, tt := range tests {
		if got := route(httptest.NewRequest(tt.method, tt.path, nil)); got != tt.want {
			t.Errorf("%s %s: route %q, want %q", tt.method, tt.path, got, tt.want)
		}
	}
}
