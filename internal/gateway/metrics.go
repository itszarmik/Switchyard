package gateway

import (
	"fmt"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/health"
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

	retries            *prometheus.CounterVec
	fallbacks          *prometheus.CounterVec
	transitions        *prometheus.CounterVec
	cooldowns          *prometheus.CounterVec
	saturated          *prometheus.CounterVec
	budgetExhausted    *prometheus.CounterVec
	registeredCounters []prometheus.Collector
}

var _ health.Observer = (*metrics)(nil)

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
			Help: "Upstream attempts, by provider, catalog model and outcome (ok, transport, timeout, server_error, rate_limited, auth, not_found, context_length, content_filter, client_error, canceled).",
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
	m.retries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_retries_total",
		Help: "Retries of the same deployment, by deployment and the failure kind that caused them.",
	}, []string{"deployment", "kind"})
	m.fallbacks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_fallbacks_total",
		Help: "Moves from one deployment of a plan to the next, by the deployment that failed, the next one, and the failure kind.",
	}, []string{"from", "to", "kind"})
	m.transitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_circuit_transitions_total",
		Help: "Circuit breaker state changes, by deployment and new state (closed, half_open, open).",
	}, []string{"deployment", "to"})
	m.cooldowns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_rate_limit_cooldowns_total",
		Help: "Rate-limit cooldowns started after an upstream 429, by deployment.",
	}, []string{"deployment"})
	m.saturated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_provider_saturated_total",
		Help: "Requests turned away from a provider at its max_concurrency, by provider.",
	}, []string{"provider"})
	m.budgetExhausted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_retry_budget_exhausted_total",
		Help: "Retries refused because the deployment's retry budget was spent, by deployment.",
	}, []string{"deployment"})
	collectors := []prometheus.Collector{
		m.requests, m.requestDuration, m.overhead, m.upstreamRequests, m.upstreamTTFT, m.upstreamDuration,
		m.retries, m.fallbacks, m.transitions, m.cooldowns, m.saturated, m.budgetExhausted,
	}
	if err := registerAll(reg, collectors); err != nil {
		return nil, err
	}
	m.registeredCounters = collectors
	return m, nil
}

// registerAll registers every collector, or none: if one fails, those
// already registered are removed again.
func registerAll(reg prometheus.Registerer, collectors []prometheus.Collector) error {
	for i, c := range collectors {
		if err := reg.Register(c); err != nil {
			for _, prev := range collectors[:i] {
				reg.Unregister(prev)
			}
			return fmt.Errorf("gateway: register metrics: %w", err)
		}
	}
	return nil
}

// registerHealth registers the gauges read from the health tracker. If it
// fails, the other metrics are removed again.
func (m *metrics) registerHealth(reg prometheus.Registerer, t *health.Tracker) error {
	if err := registerAll(reg, []prometheus.Collector{healthCollector{t: t}}); err != nil {
		for _, c := range m.registeredCounters {
			reg.Unregister(c)
		}
		return err
	}
	return nil
}

// Transition implements health.Observer.
func (m *metrics) Transition(deployment string, to health.State) {
	m.transitions.WithLabelValues(deployment, to.String()).Inc()
}

// Cooldown implements health.Observer.
func (m *metrics) Cooldown(deployment string) { m.cooldowns.WithLabelValues(deployment).Inc() }

// Saturated implements health.Observer.
func (m *metrics) Saturated(provider string) { m.saturated.WithLabelValues(provider).Inc() }

// BudgetExhausted implements health.Observer.
func (m *metrics) BudgetExhausted(deployment string) {
	m.budgetExhausted.WithLabelValues(deployment).Inc()
}

var (
	circuitStateDesc = prometheus.NewDesc("switchyard_circuit_state",
		"Circuit breaker state by deployment: 0 closed, 1 half-open, 2 open.", []string{"deployment"}, nil)
	inFlightDesc = prometheus.NewDesc("switchyard_provider_in_flight",
		"Upstream requests in flight, by provider.", []string{"provider"}, nil)
)

// healthCollector reports the breaker states and in-flight counts as they
// are at scrape time, so an open breaker whose time is up shows as
// half-open even without traffic.
type healthCollector struct{ t *health.Tracker }

// Describe implements prometheus.Collector.
func (c healthCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- circuitStateDesc
	ch <- inFlightDesc
}

// Collect implements prometheus.Collector.
func (c healthCollector) Collect(ch chan<- prometheus.Metric) {
	for _, p := range c.t.Snapshot().Providers {
		ch <- prometheus.MustNewConstMetric(inFlightDesc, prometheus.GaugeValue, float64(p.InFlight), p.ID)
		for _, d := range p.Deployments {
			ch <- prometheus.MustNewConstMetric(circuitStateDesc, prometheus.GaugeValue, float64(stateValue(d.Breaker.State)), d.ID)
		}
	}
}

// stateValue maps a snapshot's state name to the gauge value.
func stateValue(s string) health.State {
	switch s {
	case health.StateOpen.String():
		return health.StateOpen
	case health.StateHalfOpen.String():
		return health.StateHalfOpen
	}
	return health.StateClosed
}

// statusClass maps a status code to its class label, such as "2xx".
func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "other"
	}
	return strconv.Itoa(status/100) + "xx"
}
