package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/google/uuid"

	"switchyard/internal/apierror"
	"switchyard/internal/reqctx"
)

const (
	// requestIDHeader carries the gateway-assigned request ID in responses,
	// and an optional client-chosen ID in requests.
	requestIDHeader = "X-Request-Id"
	// maxClientRequestIDLen bounds a client-sent X-Request-Id that is logged.
	maxClientRequestIDLen = 128
	// maxUserAgentLen bounds the user agent in the access log, in characters.
	maxUserAgentLen = 256
	// unmatchedRoute is the route label for requests only the catch-all
	// handler matched.
	unmatchedRoute = "unmatched"
)

// chain wraps mux in the middleware, outermost first: request ID, access log
// and metrics, panic recovery, body limit.
func (s *Server) chain(listener string, mux *http.ServeMux) http.Handler {
	return s.withRequestID(s.withAccessLog(listener, mux, s.withRecover(s.withBodyLimit(mux))))
}

// withRequestID assigns every request a fresh UUIDv7, returns it in the
// X-Request-Id header and stores it, with a logger that carries it, in the
// request context. A client's own X-Request-Id is never adopted; when it is
// well-formed it is logged as client_request_id.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, err := uuid.NewV7()
		if err != nil {
			// Only possible if the system's random source fails.
			s.log.ErrorContext(r.Context(), "generate request ID", "error", err)
			apierror.Write(w, http.StatusInternalServerError, apierror.TypeServer, "internal_error",
				"The server could not assign a request ID.")
			return
		}
		id := uid.String()
		w.Header().Set(requestIDHeader, id)

		var logger *slog.Logger
		if cid := r.Header.Get(requestIDHeader); validClientRequestID(cid) {
			logger = s.log.With("request_id", id, "client_request_id", cid)
		} else {
			logger = s.log.With("request_id", id)
		}
		ctx := reqctx.WithLogger(reqctx.WithRequestID(r.Context(), id), logger)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// validClientRequestID reports whether id is 1–128 printable ASCII
// characters without spaces.
func validClientRequestID(id string) bool {
	if id == "" || len(id) > maxClientRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] <= ' ' || id[i] > '~' {
			return false
		}
	}
	return true
}

// withAccessLog records the HTTP metrics and logs one line per request. The
// route label is the ServeMux pattern the request matches, looked up before
// the request is served. Handlers add their own fields to the line with
// reqctx.Annotate.
func (s *Server) withAccessLog(listener string, mux *http.ServeMux, next http.Handler) http.Handler {
	inFlight := s.metrics.inFlight.WithLabelValues(listener)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		_, pattern := mux.Handler(r)
		route := routeLabel(pattern)
		method := normalizeMethod(r.Method)
		rw := &responseWriter{ResponseWriter: w}
		ann := &reqctx.Annotations{}
		r = r.WithContext(reqctx.WithAnnotations(r.Context(), ann))

		inFlight.Inc()
		s.inFlight.Add(1)
		completed := false
		// Deferred so that a request aborted with http.ErrAbortHandler,
		// which panics through this middleware, is still recorded.
		defer func() {
			s.inFlight.Add(-1)
			inFlight.Dec()
			elapsed := time.Since(start)
			status := rw.status
			if !rw.wroteHeader && completed {
				status = http.StatusOK // net/http sends 200 for an empty response
			}
			s.metrics.requests.WithLabelValues(listener, route, method, strconv.Itoa(status)).Inc()
			s.metrics.duration.WithLabelValues(listener, route, method).Observe(elapsed.Seconds())
			logAccess(r, listener, route, status, rw.bytes, elapsed, !completed, ann.Attrs())
		}()
		next.ServeHTTP(rw, r)
		completed = true
	})
}

// routeLabel maps a ServeMux pattern to the route label.
func routeLabel(pattern string) string {
	if pattern == "" || pattern == catchAllPattern {
		return unmatchedRoute
	}
	return pattern
}

// logAccess writes the access-log line: the server's fields, then the
// handlers' annotations. Probe and scrape requests are logged at DEBUG so
// they don't flood the logs.
func logAccess(r *http.Request, listener, route string, status int, bytes int64, elapsed time.Duration, aborted bool,
	annotations []slog.Attr) {
	ctx := r.Context()
	level := slog.LevelInfo
	if isProbe(listener, route) {
		level = slog.LevelDebug
	}
	logger := reqctx.Logger(ctx)
	if !logger.Enabled(ctx, level) {
		return
	}
	attrs := make([]slog.Attr, 0, 9+len(annotations))
	attrs = append(attrs,
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Int64("bytes", bytes),
		slog.Float64("duration_ms", float64(elapsed)/float64(time.Millisecond)),
		slog.String("remote_ip", remoteIP(r.RemoteAddr)),
		slog.String("user_agent", truncate(r.UserAgent(), maxUserAgentLen)),
		slog.String("listener", listener),
	)
	if aborted {
		attrs = append(attrs, slog.Bool("aborted", true))
	}
	attrs = append(attrs, annotations...)
	logger.LogAttrs(ctx, level, "request", attrs...)
}

func isProbe(listener, route string) bool {
	switch listener {
	case listenerPublic:
		return route == healthzPattern || route == readyzPattern
	case listenerAdmin:
		return route == metricsPattern
	}
	return false
}

// remoteIP returns the host part of a RemoteAddr. Forwarding headers are not
// trusted.
func remoteIP(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// truncate returns s cut to at most n characters.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// withRecover turns a handler panic into a logged error and, if the response
// has not started, a 500 JSON error. http.ErrAbortHandler is re-panicked so
// net/http aborts the connection as intended. A panic after the response has
// started also aborts the connection, so the client sees a broken response
// rather than a truncated one that looks complete.
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(p)
			}
			ctx := r.Context()
			reqctx.Logger(ctx).LogAttrs(ctx, slog.LevelError, "panic serving request",
				slog.String("panic", fmt.Sprint(p)),
				slog.String("stack", string(debug.Stack())),
			)
			if hw, ok := w.(interface{ HeaderWritten() bool }); ok && hw.HeaderWritten() {
				panic(http.ErrAbortHandler)
			}
			apierror.Write(w, http.StatusInternalServerError, apierror.TypeServer, "internal_error",
				"The server had an error while processing your request. Request ID: "+reqctx.RequestID(ctx))
		}()
		next.ServeHTTP(w, r)
	})
}

// withBodyLimit rejects requests whose Content-Length exceeds
// MaxRequestBytes without reading the body, and caps every other body with
// http.MaxBytesReader.
func (s *Server) withBodyLimit(next http.Handler) http.Handler {
	limit := s.opts.MaxRequestBytes
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > limit {
			AbandonBody(w, r)
			WriteRequestTooLarge(w, limit)
			return
		}
		if r.Body != nil && r.Body != http.NoBody {
			r2 := *r
			// MaxBytesReader tells the innermost writer, net/http's own, to
			// close the connection once the limit is hit; it cannot see that
			// method through our wrapper.
			r2.Body = http.MaxBytesReader(innermost(w), r.Body, limit)
			r = &r2
		}
		next.ServeHTTP(w, r)
	})
}

// innermost follows Unwrap to the writer that net/http created.
func innermost(w http.ResponseWriter) http.ResponseWriter {
	for {
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return w
		}
		w = u.Unwrap()
	}
}

// IsRequestTooLarge reports whether err comes from reading a request body
// past the MaxRequestBytes limit.
func IsRequestTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// AbandonBody prepares the response to a request whose body will not be read
// to the end, as when a request is rejected before its body is read. Call it
// before writing the response.
//
// Left alone, net/http reads up to 256 KiB of the unread body, before or
// after sending the response, so that it can reuse the connection. With no
// read timeout, a client that stalls its upload would hold that read, the
// handler's goroutine and the connection forever. AbandonBody sets the
// connection's read deadline to now, so that read gives up at once, and asks
// for the connection to be closed after the response. A request without a
// body is left alone and keeps its connection.
func AbandonBody(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength == 0 {
		return
	}
	// Closing the connection also keeps net/http from starting a
	// background read under the expired deadline, which would cancel the
	// context of the next request on a kept-alive connection.
	w.Header().Set("Connection", "close")
	_ = http.NewResponseController(w).SetReadDeadline(time.Now())
}

// WriteRequestTooLarge sends the 413 request_too_large error for a body over
// limit bytes, and asks net/http to close the connection rather than read
// the rest of the body.
func WriteRequestTooLarge(w http.ResponseWriter, limit int64) {
	w.Header().Set("Connection", "close")
	apierror.Write(w, http.StatusRequestEntityTooLarge, apierror.TypeInvalidRequest, "request_too_large",
		fmt.Sprintf("Request body too large: the limit is %d bytes.", limit))
}

// responseWriter records the status and byte count of a response. It
// implements Flush and FlushError, and Unwrap for http.ResponseController,
// so handlers can stream through it, and HeaderWritten for apierror.
type responseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

// WriteHeader records the final status code and passes it on.
func (w *responseWriter) WriteHeader(code int) {
	// 1xx responses other than 101 are informational; the final status
	// follows later.
	if !w.wroteHeader && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write sends b, implicitly with status 200 if no status was set.
func (w *responseWriter) Write(b []byte) (int, error) {
	w.markWritten()
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush sends buffered data to the client.
func (w *responseWriter) Flush() {
	_ = w.FlushError()
}

// FlushError sends buffered data to the client and reports any error. It
// returns http.ErrNotSupported if the underlying writer cannot flush.
func (w *responseWriter) FlushError() error {
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		return err
	}
	w.markWritten()
	return nil
}

// HeaderWritten reports whether the response headers have been sent.
func (w *responseWriter) HeaderWritten() bool { return w.wroteHeader }

// Unwrap returns the wrapped writer, for http.ResponseController.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseWriter) markWritten() {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
}
