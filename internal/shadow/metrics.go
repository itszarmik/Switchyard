package shadow

import (
	"errors"
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics are the shadow sampling metrics (DESIGN §15.2).
type metrics struct {
	samples *prometheus.CounterVec
	cost    prometheus.Counter
	ratio   *prometheus.GaugeVec
	reg     prometheus.Registerer

	// mu guards the per-category counts behind ratio.
	mu         sync.Mutex
	judged     map[string]int64
	acceptable map[string]int64
}

// newMetrics creates the metrics and registers them on reg, if set.
func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		samples: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchyard_shadow_samples_total",
			Help: "Shadow samples by outcome: judged, dropped_budget, dropped_queue, dropped_policy or error.",
		}, []string{"outcome"}),
		cost: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "switchyard_shadow_cost_usd_total",
			Help: "Cost of the shadow comparison and judge calls, in USD (exact where usage was reported, else the reserved estimate).",
		}),
		ratio: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "switchyard_shadow_acceptable_ratio",
			Help: "Fraction of judged shadow samples found acceptable since the replica started, by quality category.",
		}, []string{"category"}),
		reg:        reg,
		judged:     map[string]int64{},
		acceptable: map[string]int64{},
	}
	for _, o := range []string{OutcomeJudged, OutcomeDroppedBudget, OutcomeDroppedQueue, OutcomeDroppedPolicy, OutcomeError} {
		m.samples.WithLabelValues(o)
	}
	if reg == nil {
		return m, nil
	}
	var done []prometheus.Collector
	for _, c := range []prometheus.Collector{m.samples, m.cost, m.ratio} {
		if err := reg.Register(c); err != nil {
			for _, d := range done {
				reg.Unregister(d)
			}
			var are prometheus.AlreadyRegisteredError
			if errors.As(err, &are) {
				return nil, errors.New("shadow: the shadow metrics are already registered; close the previous runner first")
			}
			return nil, fmt.Errorf("shadow: register metrics: %w", err)
		}
		done = append(done, c)
	}
	return m, nil
}

// count counts one sample with outcome.
func (m *metrics) count(outcome string) { m.samples.WithLabelValues(outcome).Inc() }

// observe records a judged sample of category and updates its ratio.
func (m *metrics) observe(category string, acceptable bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.judged[category]++
	if acceptable {
		m.acceptable[category]++
	}
	m.ratio.WithLabelValues(category).Set(float64(m.acceptable[category]) / float64(m.judged[category]))
}

// unregister removes the metrics from their registerer.
func (m *metrics) unregister() {
	if m.reg == nil {
		return
	}
	m.reg.Unregister(m.samples)
	m.reg.Unregister(m.cost)
	m.reg.Unregister(m.ratio)
}
