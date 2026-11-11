package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"switchyard/internal/version"
)

// NewRegistry returns a new registry, never the global default, holding the
// Go runtime collector, the process collector and the gauge
// switchyard_build_info{version,commit,go_version} = 1.
func NewRegistry() *prometheus.Registry {
	info := version.Get()
	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "switchyard_build_info",
		Help: "Build metadata of the running binary; the value is always 1.",
		ConstLabels: prometheus.Labels{
			"version":    info.Version,
			"commit":     info.Commit,
			"go_version": info.GoVersion,
		},
	})
	buildInfo.Set(1)

	reg := prometheus.NewRegistry()
	// These collectors cannot conflict on a fresh registry, so MustRegister
	// can only panic on a programming error.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo,
	)
	return reg
}
