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

	// Cost accounting and budgets (DESIGN §12, §15.2).
	cost                *prometheus.CounterVec
	tokens              *prometheus.CounterVec
	savings             *prometheus.GaugeVec
	savingsRouting      prometheus.Gauge
	budgetRejections    prometheus.Counter
	budgetFallbackSkips prometheus.Counter
	budgetStoreErrors   prometheus.Counter
	budgetNotices       *prometheus.CounterVec

	// Routing (DESIGN §15.2).
	routeDecisions   *prometheus.CounterVec
	routeRelaxations *prometheus.CounterVec
	routeDowngrades  *prometheus.CounterVec
	projectedSavings prometheus.Gauge

	// Response caches (DESIGN §11, §15.2).
	savingsCache       prometheus.Gauge
	cacheLookups       *prometheus.CounterVec
	cacheWrites        *prometheus.CounterVec
	cacheSimilarity    *prometheus.HistogramVec
	cacheLookupSeconds *prometheus.HistogramVec
	cacheCoalesced     prometheus.Counter
	overheadUncharged  prometheus.Counter
}

// similarityBuckets resolve the semantic similarities around the default
// threshold (0.95), where tuning happens.
var similarityBuckets = []float64{0.5, 0.6, 0.7, 0.8, 0.85, 0.88, 0.9, 0.92, 0.93, 0.94, 0.95, 0.96, 0.97, 0.98, 0.99, 0.995, 1}

// lookupBuckets cover an exact lookup (well under a millisecond on a local
// Redis) up to a semantic lookup at its timeout.
var lookupBuckets = []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.15, 0.25, 0.5, 1}

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
		Help: "Attempts refused because the provider was at its max_concurrency when they started, by provider; routing around a saturated provider is not counted.",
	}, []string{"provider"})
	m.budgetExhausted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_retry_budget_exhausted_total",
		Help: "Retries refused because the deployment's retry budget was spent, by deployment.",
	}, []string{"deployment"})
	m.cost = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_cost_usd_total",
		Help: "Upstream cost in USD of the requests with a known cost, by provider and catalog model. A float approximation of the usage ledger's exact sums.",
	}, []string{"provider", "model"})
	m.tokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_tokens_total",
		Help: "Billed tokens, by provider, catalog model and class (input, cached_input, cache_write, output). Input is uncached input; output includes reasoning tokens.",
	}, []string{"provider", "model", "class"})
	m.savings = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "switchyard_savings_usd_total",
		Help: "Cumulative savings in USD against the baseline, by source (routing, cache). Routing savings are negative when a fallback costs more than the baseline, so this running total can decrease.",
	}, []string{"source"})
	m.savingsRouting = m.savings.WithLabelValues("routing")
	m.savingsCache = m.savings.WithLabelValues("cache")
	m.cacheLookups = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_cache_lookups_total",
		Help: "Response cache lookups of chat requests whose policy enables the layer, by layer (exact, semantic) and result (hit, miss, error, skipped, policy, budget). error is a failure or timeout, served as a miss; skipped is a request not eligible for the layer; policy is an entry, or for semantic the embedding deployment, that the request's policy does not permit, served as a miss; budget is a semantic lookup skipped because a blocking budget had no room for the embedding.",
	}, []string{"layer", "result"})
	m.cacheWrites = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_cache_writes_total",
		Help: "Response cache writes after a miss, by layer (exact, semantic) and result (ok, error, dropped when the write pool was full, too_large, not_cacheable).",
	}, []string{"layer", "result"})
	m.cacheSimilarity = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "switchyard_cache_similarity",
		Help:    "Similarity of the nearest semantic cache entry, by outcome (hit, miss against min_similarity), for tuning the threshold.",
		Buckets: similarityBuckets,
	}, []string{"outcome"})
	m.cacheLookupSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "switchyard_cache_lookup_seconds",
		Help:    "Duration of response cache lookups, by layer: the namespace version and entry read for exact, the embedding and vector search for semantic.",
		Buckets: lookupBuckets,
	}, []string{"layer"})
	m.cacheCoalesced = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "switchyard_cache_coalesced_total",
		Help: "Identical concurrent non-streaming misses served from another request's upstream call (in-process coalescing).",
	})
	m.overheadUncharged = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "switchyard_cache_overhead_uncharged_total",
		Help: "Semantic cache embedding costs that could not be charged to the request's budgets, because the budget store failed or a blocking budget was already over its limit. The usage ledger still records them as overhead.",
	})
	m.budgetRejections = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "switchyard_budget_rejections_total",
		Help: "Requests answered 429 budget_exceeded because a blocking budget had no room for their estimated cost.",
	})
	m.budgetFallbackSkips = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "switchyard_budget_fallback_skips_total",
		Help: "Pricier fallback deployments passed over because a blocking budget had no room for the difference in their estimated cost.",
	})
	m.budgetStoreErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "switchyard_budget_store_errors_total",
		Help: "Budget store calls that failed, or were skipped while the store was marked unavailable after a failure; budgets.fail_mode decides what the request gets.",
	})
	m.budgetNotices = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_budget_notifications_total",
		Help: "Budget notifications, by budget scope and kind: soft_threshold when a window first reaches soft_at, exceeded for each request admitted over a notify budget's limit.",
	}, []string{"scope", "kind"})
	m.routeDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_route_decisions_total",
		Help: "Served chat and legacy completion requests, by routing profile, the catalog model that served them, and route reason (cheapest_eligible, pinned, preferred, quality_relaxed, latency_relaxed, budget_downgrade, observe, fallback:<kind>).",
	}, []string{"profile", "served_model", "reason"})
	m.routeRelaxations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_route_relaxations_total",
		Help: "Routing decisions that relaxed a soft constraint because no candidate met it (on_unmet best_effort), by constraint (latency, quality).",
	}, []string{"constraint"})
	m.routeDowngrades = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "switchyard_route_profile_downgrades_total",
		Help: "Auto requests routed with a budget's soft_profile because its soft threshold was crossed, by budget scope.",
	}, []string{"scope"})
	// A gauge, as savings_usd_total is: the projected savings of one
	// request are negative when the router would have chosen a deployment
	// pricier than the baseline, so the running total can decrease, which
	// a counter must never do.
	m.projectedSavings = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "switchyard_routing_projected_savings_usd_total",
		Help: "Cumulative projected routing savings in USD of observe-mode requests: the baseline cost minus the cost of the same tokens on the deployment the router would have chosen. Projected, not realized; it can decrease.",
	})
	collectors := []prometheus.Collector{
		m.requests, m.requestDuration, m.overhead, m.upstreamRequests, m.upstreamTTFT, m.upstreamDuration,
		m.retries, m.fallbacks, m.transitions, m.cooldowns, m.saturated, m.budgetExhausted,
		m.cost, m.tokens, m.savings, m.budgetRejections, m.budgetFallbackSkips, m.budgetStoreErrors, m.budgetNotices,
		m.routeDecisions, m.routeRelaxations, m.routeDowngrades, m.projectedSavings,
		m.cacheLookups, m.cacheWrites, m.cacheSimilarity, m.cacheLookupSeconds, m.cacheCoalesced, m.overheadUncharged,
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

// registerHealth registers the gauges read at scrape time: from the health
// tracker, the budgets' utilization from g, and the requests waiting on a
// coalesced call. If it fails, the other metrics are removed again.
func (m *metrics) registerHealth(reg prometheus.Registerer, t *health.Tracker, g *Gateway) error {
	waiting := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "switchyard_cache_coalesce_waiting",
		Help: "Requests waiting on another identical request's upstream call (in-process coalescing), at scrape time.",
	}, func() float64 { return float64(g.cache.coalescer.Followers()) })
	if err := registerAll(reg, []prometheus.Collector{healthCollector{t: t}, budgetCollector{g: g}, waiting}); err != nil {
		m.unregister(reg)
		return err
	}
	return nil
}

// unregister removes the metrics newMetrics registered, for a gateway that
// failed to start.
func (m *metrics) unregister(reg prometheus.Registerer) {
	for _, c := range m.registeredCounters {
		reg.Unregister(c)
	}
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
