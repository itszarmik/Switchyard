package gateway

import (
	"fmt"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// durationBuckets cover sub-millisecond rejections up to streams that run
// for the default ten-minute total timeout.
var durationBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

// overheadBuckets resolve the gateway's own time, which should stay well
// under the 5 ms p99 budget.
var overheadBuckets = []float64{0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25}

// ttftBuckets cover fast cached answers up to slow reasoning models.
var ttftBuckets = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// metrics are the API and upstream metrics.
type metrics struct {
	requests         *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	overhead         *prometheus.HistogramVec
	upstreamRequests *prometheus.CounterVec
	upstreamTTFT     *prometheus.HistogramVec
	upstreamDuration *prometheus.HistogramVec
}

// newMetrics creates the metrics and registers them on reg. If one fails to
// register, those already registered are removed again.
func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchyard_requests_total",
			Help: "API requests, by endpoint and status class (2xx, 4xx, 5xx).",
		}, []string{"endpoint", "status_class"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "switchyard_request_duration_seconds",
			Help:    "Time to serve an API request, including the upstream call and, for streams, the whole stream.",
			Buckets: durationBuckets,
		}, []string{"endpoint", "stream"}),
		overhead: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "switchyard_overhead_duration_seconds",
			Help:    "Time an API request spent in the gateway itself: its duration minus the time spent waiting on the upstream.",
			Buckets: overheadBuckets,
		}, []string{"endpoint"}),
		upstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchyard_upstream_requests_total",
			Help: "Upstream calls, by provider, catalog model and outcome (ok, client_error, rate_limited, auth, server_error, timeout, transport, canceled).",
		}, []string{"provider", "model", "outcome"}),
		upstreamTTFT: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "switchyard_upstream_ttft_seconds",
			Help:    "Time from sending an upstream request to its first token: the first event of a stream, or the response headers otherwise.",
			Buckets: ttftBuckets,
		}, []string{"provider", "model"}),
		upstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "switchyard_upstream_duration_seconds",
			Help:    "Duration of upstream calls, until the last byte of the response or stream.",
			Buckets: durationBuckets,
		}, []string{"provider", "model"}),
	}
	collectors := []prometheus.Collector{
		m.requests, m.requestDuration, m.overhead, m.upstreamRequests, m.upstreamTTFT, m.upstreamDuration,
	}
	for i, c := range collectors {
		if err := reg.Register(c); err != nil {
			for _, prev := range collectors[:i] {
				reg.Unregister(prev)
			}
			return nil, fmt.Errorf("gateway: register metrics: %w", err)
		}
	}
	return m, nil
}

// statusClass maps a status code to its class label, such as "2xx".
func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "other"
	}
	return strconv.Itoa(status/100) + "xx"
}
