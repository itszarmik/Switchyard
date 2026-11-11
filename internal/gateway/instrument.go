package gateway

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// Instrument wraps h, the handler of endpoint, so that every request it
// serves counts in switchyard_requests_total and
// switchyard_request_duration_seconds. The composition root wraps it
// outside authentication, so requests rejected there count too. A request
// whose handler panics is left to the server's HTTP metrics.
func (g *Gateway) Instrument(endpoint string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		iw := &instrumentedWriter{ResponseWriter: w}
		h.ServeHTTP(iw, r.WithContext(context.WithValue(r.Context(), instrumentedKey{}, iw)))
		status := iw.status
		if status == 0 {
			status = http.StatusOK // net/http sends 200 for an empty response
		}
		g.metrics.requests.WithLabelValues(endpoint, statusClass(status)).Inc()
		g.metrics.requestDuration.WithLabelValues(endpoint, strconv.FormatBool(iw.stream)).Observe(time.Since(start).Seconds())
	})
}

type instrumentedKey struct{}

// instrumentedWriter records the response status for Instrument, and
// whether the request asked for a stream. It passes everything else
// through; Unwrap lets http.ResponseController reach the writer below.
type instrumentedWriter struct {
	http.ResponseWriter
	status int
	stream bool
}

// WriteHeader records the final status code and passes it on.
func (w *instrumentedWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write sends b, implicitly with status 200 if no status was set.
func (w *instrumentedWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap returns the wrapped writer, for http.ResponseController.
func (w *instrumentedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// markStream records, for the request duration metric, whether the request
// in ctx asked for a stream. Outside Instrument it does nothing.
func markStream(ctx context.Context, stream bool) {
	if iw, ok := ctx.Value(instrumentedKey{}).(*instrumentedWriter); ok {
		iw.stream = stream
	}
}
