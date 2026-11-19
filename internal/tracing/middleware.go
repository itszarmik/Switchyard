package tracing

import (
	"context"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// traceparentHeader is the canonical form of the W3C traceparent header.
const traceparentHeader = "Traceparent"

// Middleware returns middleware that starts the root server span of every
// request on listener, as a child of the request's W3C traceparent when it
// has one. route, which may be nil, returns the request's route pattern
// (such as the ServeMux pattern "POST /v1/chat/completions"), or "" when no
// route matches; the span is named "<method> <route>". The span records the
// HTTP method, route and response status code, never the URL or its query,
// and an error status for a 5xx or an aborted handler.
//
// With tracing disabled, a request without a traceparent passes through
// untouched and without allocating, and one with a traceparent only gets
// the remote span context, so its trace ID reaches the logs.
//
// Put it outside the middleware that builds the request logger, so that
// logger can add the IDs from IDs.
func (p *Provider) Middleware(listener string, route func(*http.Request) string) func(http.Handler) http.Handler {
	if p.tp == nil {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if len(r.Header[traceparentHeader]) == 0 {
					next.ServeHTTP(w, r)
					return
				}
				ctx := p.prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		}
	}
	listenerAttr := AttrListener.String(listener)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := p.prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			method := methodAttr(r.Method)
			name := method.Value.AsString()
			if name == "_OTHER" {
				name = "HTTP"
			}
			attrs := make([]attribute.KeyValue, 0, 3)
			attrs = append(attrs, method, listenerAttr)
			if route != nil {
				if rt := httpRoute(route(r)); rt != "" {
					name += " " + rt
					attrs = append(attrs, semconv.HTTPRoute(rt))
				}
			}
			ctx, span := p.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
			r = r.WithContext(ctx)
			if !span.IsRecording() {
				span.End()
				next.ServeHTTP(w, r)
				return
			}

			sw := &statusWriter{ResponseWriter: w}
			completed := false
			// Deferred so that a handler aborted with a panic, such as
			// http.ErrAbortHandler, still ends its span.
			defer func() {
				status := sw.status
				if !sw.wroteHeader && completed {
					status = http.StatusOK // net/http sends 200 for an empty response
				}
				if status != 0 {
					span.SetAttributes(semconv.HTTPResponseStatusCode(status))
				}
				switch {
				case !completed:
					span.SetStatus(codes.Error, "handler aborted")
				case status >= http.StatusInternalServerError:
					span.SetStatus(codes.Error, "")
				}
				span.End()
			}()
			next.ServeHTTP(sw, r)
			completed = true
		})
	}
}

// methodAttr returns the http.request.method attribute: the method when it
// is a well-known one, otherwise _OTHER, so clients cannot grow the
// attribute's cardinality.
func methodAttr(m string) attribute.KeyValue {
	switch m {
	case http.MethodGet:
		return semconv.HTTPRequestMethodGet
	case http.MethodPost:
		return semconv.HTTPRequestMethodPost
	case http.MethodHead:
		return semconv.HTTPRequestMethodHead
	case http.MethodPut:
		return semconv.HTTPRequestMethodPut
	case http.MethodDelete:
		return semconv.HTTPRequestMethodDelete
	case http.MethodPatch:
		return semconv.HTTPRequestMethodPatch
	case http.MethodOptions:
		return semconv.HTTPRequestMethodOptions
	case http.MethodConnect:
		return semconv.HTTPRequestMethodConnect
	case http.MethodTrace:
		return semconv.HTTPRequestMethodTrace
	default:
		return semconv.HTTPRequestMethodOther
	}
}

// httpRoute returns the path template of a ServeMux pattern, without its
// method: "POST /v1/chat/completions" becomes "/v1/chat/completions".
func httpRoute(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = strings.TrimLeft(pattern[i+1:], " \t")
	}
	return pattern
}

// statusWriter records the response status for the server span. It
// implements Flush and FlushError, and Unwrap for http.ResponseController,
// so streaming works through it.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// WriteHeader records the final status code and passes it on.
func (w *statusWriter) WriteHeader(code int) {
	// 1xx responses other than 101 are informational; the final status
	// follows later.
	if !w.wroteHeader && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.status, w.wroteHeader = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write sends b, implicitly with status 200 if no status was set.
func (w *statusWriter) Write(b []byte) (int, error) {
	w.markWritten()
	return w.ResponseWriter.Write(b)
}

// Flush sends buffered data to the client.
func (w *statusWriter) Flush() { _ = w.FlushError() }

// FlushError sends buffered data to the client and reports any error. It
// returns http.ErrNotSupported if the underlying writer cannot flush.
func (w *statusWriter) FlushError() error {
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		return err
	}
	w.markWritten()
	return nil
}

// HeaderWritten reports whether the response headers have been sent, for
// apierror.
func (w *statusWriter) HeaderWritten() bool { return w.wroteHeader }

// Unwrap returns the wrapped writer, for http.ResponseController.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) markWritten() {
	if !w.wroteHeader {
		w.status, w.wroteHeader = http.StatusOK, true
	}
}

// Transport returns base, or http.DefaultTransport when base is nil, made
// to send the W3C traceparent of the request context's span when
// propagate is true, and only when that span is one this gateway created
// (it has tracing enabled): a client's own trace context, which is all a
// request carries with tracing disabled, is never forwarded. tracestate
// and baggage are never sent: providers are outside the trust boundary,
// and both are the client's. When propagate is false it returns base
// itself, which leaves every header untouched.
func Transport(base http.RoundTripper, propagate bool) http.RoundTripper {
	if !propagate {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &propagatingTransport{base: base}
}

// propagatingTransport injects the trace context into outgoing requests.
type propagatingTransport struct {
	base http.RoundTripper
}

// RoundTrip sends req with the traceparent of its context's span, on a
// copy of req, since a RoundTripper must not modify the request.
func (t *propagatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	sc := trace.SpanContextFromContext(req.Context())
	if !sc.IsValid() || sc.IsRemote() {
		return t.base.RoundTrip(req)
	}
	// Inject into a scratch carrier and copy traceparent alone: Inject
	// would also send the span's tracestate, inherited from the client.
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(trace.ContextWithSpanContext(context.Background(), sc), carrier)
	out := req.Clone(req.Context())
	out.Header.Set(traceparentHeader, carrier.Get(strings.ToLower(traceparentHeader)))
	return t.base.RoundTrip(out)
}

// CloseIdleConnections closes the base transport's idle connections, so
// http.Client.CloseIdleConnections keeps working through the wrapper.
func (t *propagatingTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}
