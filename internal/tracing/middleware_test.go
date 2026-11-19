package tracing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"switchyard/internal/config"
	"switchyard/internal/version"
)

// patternRoute is a route func that returns a fixed ServeMux pattern.
func patternRoute(p string) func(*http.Request) string {
	return func(*http.Request) string { return p }
}

func TestMiddlewareServerSpan(t *testing.T) {
	const query = "api_key=Zq8sUperSecretQuery"
	tests := []struct {
		name        string
		method      string
		route       func(*http.Request) string
		handler     http.HandlerFunc
		wantName    string
		wantMethod  string
		wantRoute   string // "" means no http.route attribute
		wantStatus  int64  // 0 means no status attribute
		wantCode    codes.Code
		wantDescr   string
		wantAborted bool
	}{
		{
			name: "ok", method: http.MethodPost, route: patternRoute("POST /v1/chat/completions"),
			handler:  func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) },
			wantName: "POST /v1/chat/completions", wantMethod: "POST", wantRoute: "/v1/chat/completions",
			wantStatus: 200, wantCode: codes.Unset,
		},
		{
			name: "empty response", method: http.MethodGet, route: patternRoute("GET /v1/models"),
			handler:  func(http.ResponseWriter, *http.Request) {},
			wantName: "GET /v1/models", wantMethod: "GET", wantRoute: "/v1/models", wantStatus: 200, wantCode: codes.Unset,
		},
		{
			name: "client error", method: http.MethodPost, route: patternRoute("POST /v1/chat/completions"),
			handler:  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
			wantName: "POST /v1/chat/completions", wantMethod: "POST", wantRoute: "/v1/chat/completions",
			wantStatus: 429, wantCode: codes.Unset,
		},
		{
			name: "server error", method: http.MethodPost, route: patternRoute("POST /v1/chat/completions"),
			handler:  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
			wantName: "POST /v1/chat/completions", wantMethod: "POST", wantRoute: "/v1/chat/completions",
			wantStatus: 503, wantCode: codes.Error,
		},
		{
			name: "unmatched route", method: http.MethodGet, route: patternRoute(""),
			handler:  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			wantName: "GET", wantMethod: "GET", wantStatus: 404, wantCode: codes.Unset,
		},
		{
			name: "nil route func and unknown method", method: "BREW", route: nil,
			handler:  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusMethodNotAllowed) },
			wantName: "HTTP", wantMethod: "_OTHER", wantStatus: 405, wantCode: codes.Unset,
		},
		{
			name: "aborted", method: http.MethodPost, route: patternRoute("POST /v1/chat/completions"),
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				panic(http.ErrAbortHandler)
			},
			wantName: "POST /v1/chat/completions", wantMethod: "POST", wantRoute: "/v1/chat/completions",
			wantStatus: 200, wantCode: codes.Error, wantDescr: "handler aborted", wantAborted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, exp := newTestProvider(t, 1)
			h := p.Middleware("public", tt.route)(tt.handler)
			r := httptest.NewRequest(tt.method, "/v1/chat/completions?"+query, nil)
			r.Header.Set("traceparent", sampledParent)
			func() {
				defer func() {
					if v := recover(); (v != nil) != tt.wantAborted {
						t.Errorf("panic = %v, want aborted=%v", v, tt.wantAborted)
					}
				}()
				h.ServeHTTP(httptest.NewRecorder(), r)
			}()

			spans := exp.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("exported %d spans, want 1", len(spans))
			}
			s := spans[0]
			if s.Name != tt.wantName || s.SpanKind != trace.SpanKindServer {
				t.Errorf("span = %q (%v), want %q (server)", s.Name, s.SpanKind, tt.wantName)
			}
			if s.SpanContext.TraceID().String() != parentTraceID || s.Parent.SpanID().String() != parentSpanID || !s.Parent.IsRemote() {
				t.Errorf("span is not a child of the incoming traceparent: trace %s, parent %s", s.SpanContext.TraceID(), s.Parent.SpanID())
			}
			if s.Status.Code != tt.wantCode || s.Status.Description != tt.wantDescr {
				t.Errorf("status = %v %q, want %v %q", s.Status.Code, s.Status.Description, tt.wantCode, tt.wantDescr)
			}
			attrs := attrMap(s.Attributes)
			want := map[attribute.Key]attribute.Value{
				"http.request.method": attribute.StringValue(tt.wantMethod),
				AttrListener:          attribute.StringValue("public"),
			}
			if tt.wantRoute != "" {
				want["http.route"] = attribute.StringValue(tt.wantRoute)
			}
			if tt.wantStatus != 0 {
				want["http.response.status_code"] = attribute.Int64Value(tt.wantStatus)
			}
			if len(attrs) != len(want) {
				t.Errorf("attributes = %v, want %v", attrs, want)
			}
			for k, v := range want {
				if attrs[k] != v {
					t.Errorf("%s = %q, want %q", k, attrs[k].String(), v.String())
				}
			}
			for k, v := range attrs {
				if strings.Contains(v.String(), "Zq8sUperSecretQuery") {
					t.Errorf("attribute %s carries the URL query", k)
				}
			}
		})
	}
}

func TestMiddlewareChildSpansAndStreaming(t *testing.T) {
	p, exp := newTestProvider(t, 1)
	var handlerIDs [2]string
	h := p.Middleware("public", patternRoute("POST /v1/chat/completions"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerIDs[0], handlerIDs[1], _ = IDs(r.Context())
		_, span := Start(r.Context(), SpanUpstreamAttempt, AttrAttemptNumber.Int(1))
		SetGenAI(span, GenAI{System: "openai", RequestModel: "m", InputTokens: 3, OutputTokens: 4})
		span.End()
		_, _ = w.Write([]byte("data: {}\n\n"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush through the middleware: %v", err)
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if !rec.Flushed {
		t.Error("the response was not flushed")
	}

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("exported %d spans, want 2", len(spans))
	}
	attempt, server := spans[0], spans[1]
	if attempt.Parent.SpanID() != server.SpanContext.SpanID() {
		t.Errorf("upstream.attempt is not a child of the server span")
	}
	if handlerIDs[0] != server.SpanContext.TraceID().String() || handlerIDs[1] != server.SpanContext.SpanID().String() {
		t.Errorf("IDs in the handler = %v, want the server span's", handlerIDs)
	}
	if server.Parent.IsValid() {
		t.Errorf("a request without traceparent got parent %s", server.Parent.SpanID())
	}
}

func TestMiddlewareDisabled(t *testing.T) {
	p, err := New(context.Background(), config.Tracing{SampleRatio: 1, ServiceName: "x"}, version.Info{}, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tests := []struct {
		name        string
		traceparent string
		wantOK      bool
	}{
		{"no traceparent", "", false},
		{"sampled traceparent", sampledParent, true},
		{"unsampled traceparent", unsampledParent, true},
		{"malformed traceparent", "00-zz-yy-01", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var traceID, spanID string
			var ok, recording bool
			h := p.Middleware("public", patternRoute("GET /x"))(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				traceID, spanID, ok = IDs(r.Context())
				_, span := Start(r.Context(), SpanAuth)
				recording = span.IsRecording()
				span.End()
			}))
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tt.traceparent != "" {
				r.Header.Set("traceparent", tt.traceparent)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			if ok != tt.wantOK {
				t.Fatalf("IDs ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && (traceID != parentTraceID || spanID != parentSpanID) {
				t.Errorf("IDs = %s, %s; want the incoming %s, %s", traceID, spanID, parentTraceID, parentSpanID)
			}
			if recording {
				t.Error("a span recorded with tracing disabled")
			}
		})
	}
}

// TestMiddlewareDisabledAllocs checks the disabled middleware adds no
// allocation to a request without a traceparent.
func TestMiddlewareDisabledAllocs(t *testing.T) {
	p, err := New(context.Background(), config.Tracing{SampleRatio: 1, ServiceName: "x"}, version.Info{}, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h := p.Middleware("public", patternRoute("POST /v1/chat/completions"))(next)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	base := testing.AllocsPerRun(1000, func() { next.ServeHTTP(w, r) })
	got := testing.AllocsPerRun(1000, func() { h.ServeHTTP(w, r) })
	if got > base {
		t.Errorf("disabled middleware: %.1f allocs per request, baseline %.1f", got, base)
	}
}

func BenchmarkMiddleware(b *testing.B) {
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	route := patternRoute("POST /v1/chat/completions")
	disabled, err := New(context.Background(), config.Tracing{SampleRatio: 1, ServiceName: "x"}, version.Info{}, discard())
	if err != nil {
		b.Fatal(err)
	}
	newEnabled := func(ratio float64) *Provider {
		p, err := newProvider(context.Background(), config.Tracing{
			Enabled: true, Endpoint: config.NewSecret(testCollectorURL), SampleRatio: ratio, ServiceName: "x",
		}, version.Info{}, discard(), options{exporter: discardExporter{}})
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = p.Shutdown(context.Background()) })
		return p
	}
	cases := []struct {
		name        string
		h           http.Handler
		traceparent string
	}{
		{"baseline", next, ""},
		{"disabled", disabled.Middleware("public", route)(next), ""},
		{"disabled_traceparent", disabled.Middleware("public", route)(next), sampledParent},
		{"enabled_sampled_out", newEnabled(0).Middleware("public", route)(next), ""},
		{"enabled_sampled", newEnabled(1).Middleware("public", route)(next), ""},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if c.traceparent != "" {
				r.Header.Set("traceparent", c.traceparent)
			}
			w := httptest.NewRecorder()
			b.ReportAllocs()
			for b.Loop() {
				c.h.ServeHTTP(w, r)
			}
		})
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// recordingTransport records the request it was sent.
type recordingTransport struct{ got *http.Request }

func (t *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.got = r
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
}

func TestTransport(t *testing.T) {
	p, _ := newTestProvider(t, 1)
	spanCtx, span := p.Tracer().Start(context.Background(), SpanUpstreamAttempt)
	defer span.End()
	member, err := baggage.NewMember("user", "alice")
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatal(err)
	}
	spanCtx = baggage.ContextWithBaggage(spanCtx, bag)
	sc := span.SpanContext()
	wantTP := "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-01"

	// A client's trace context, with a tracestate of its own: alone (all
	// a request carries with tracing disabled), and as the parent of a
	// span of ours, which inherits the tracestate.
	ts, err := trace.ParseTraceState("vendor=CLIENT-CONTROLLED")
	if err != nil {
		t.Fatal(err)
	}
	client := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1, 2, 3}, SpanID: trace.SpanID{4, 5, 6},
		TraceFlags: trace.FlagsSampled, TraceState: ts, Remote: true,
	})
	clientCtx := trace.ContextWithRemoteSpanContext(context.Background(), client)
	childCtx, child := p.Tracer().Start(clientCtx, SpanUpstreamAttempt)
	defer child.End()
	if child.SpanContext().TraceState().Len() == 0 {
		t.Fatal("the child span did not inherit the client's tracestate")
	}
	csc := child.SpanContext()
	wantChildTP := "00-" + csc.TraceID().String() + "-" + csc.SpanID().String() + "-01"

	tests := []struct {
		name      string
		ctx       context.Context
		propagate bool
		wantTP    string
	}{
		{"propagate with a span", spanCtx, true, wantTP},
		{"propagate without a span", context.Background(), true, ""},
		{"no propagation", spanCtx, false, ""},
		{"a client's trace context only", clientCtx, true, ""},
		{"a span of ours under a client's", childCtx, true, wantChildTP},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &recordingTransport{}
			rt := Transport(base, tt.propagate)
			if !tt.propagate && rt != http.RoundTripper(base) {
				t.Fatal("Transport(base, false) did not return base")
			}
			req, err := http.NewRequestWithContext(tt.ctx, http.MethodPost, "http://upstream.test/v1/chat/completions", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer upstream-key")
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			_ = resp.Body.Close()
			if got := base.got.Header.Get("traceparent"); got != tt.wantTP {
				t.Errorf("traceparent = %q, want %q", got, tt.wantTP)
			}
			if got := base.got.Header.Get("tracestate"); got != "" {
				t.Errorf("tracestate was sent upstream: %q", got)
			}
			if got := base.got.Header.Get("baggage"); got != "" {
				t.Errorf("baggage was sent upstream: %q", got)
			}
			if base.got.Header.Get("Authorization") != "Bearer upstream-key" {
				t.Error("the other headers were not kept")
			}
			if req.Header.Get("traceparent") != "" {
				t.Error("Transport modified the caller's request")
			}
		})
	}

	t.Run("nil base", func(t *testing.T) {
		if Transport(nil, false) != nil {
			t.Error("Transport(nil, false) != nil")
		}
		if rt, ok := Transport(nil, true).(*propagatingTransport); !ok || rt.base != http.DefaultTransport {
			t.Errorf("Transport(nil, true) = %#v, want http.DefaultTransport wrapped", rt)
		}
	})

	t.Run("errors pass through", func(t *testing.T) {
		wantErr := errors.New("dial failed")
		rt := Transport(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, wantErr }), true)
		req, _ := http.NewRequestWithContext(spanCtx, http.MethodGet, "http://upstream.test/", http.NoBody)
		resp, err := rt.RoundTrip(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, wantErr) {
			t.Errorf("RoundTrip error = %v, want %v", err, wantErr)
		}
	})
}

func TestHTTPRoute(t *testing.T) {
	tests := []struct{ in, want string }{
		{"POST /v1/chat/completions", "/v1/chat/completions"},
		{"/v1/models", "/v1/models"},
		{"GET  /x", "/x"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := httpRoute(tt.in); got != tt.want {
			t.Errorf("httpRoute(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
