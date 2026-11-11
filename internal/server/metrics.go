package server

import (
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
)

// httpMetrics are the HTTP server metrics shared by both listeners.
type httpMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight *prometheus.GaugeVec
}

// newHTTPMetrics creates the HTTP metrics and registers them on reg. If one
// fails to register, those already registered are removed again.
func newHTTPMetrics(reg prometheus.Registerer) (*httpMetrics, error) {
	m := &httpMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchyard_http_requests_total",
			Help: "HTTP requests served, by listener, matched route pattern, method and status code.",
		}, []string{"listener", "route", "method", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "switchyard_http_request_duration_seconds",
			Help: "Time from receiving a request until its handler returned.",
			// From sub-millisecond gateway-only responses up to
			// two-minute streaming completions.
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"listener", "route", "method"}),
		inFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "switchyard_http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}, []string{"listener"}),
	}
	collectors := []prometheus.Collector{m.requests, m.duration, m.inFlight}
	for i, c := range collectors {
		if err := reg.Register(c); err != nil {
			for _, prev := range collectors[:i] {
				reg.Unregister(prev)
			}
			return nil, fmt.Errorf("server: register HTTP metrics: %w", err)
		}
	}
	return m, nil
}

// normalizeMethod maps the request method onto a bounded label set.
func normalizeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}
