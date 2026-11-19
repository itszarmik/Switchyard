package config

// Tracing defaults (DESIGN §15.3), documented on the fields of Tracing.
const (
	// DefaultTracingSampleRatio is telemetry.tracing.sample_ratio.
	DefaultTracingSampleRatio = 0.05
	// DefaultTracingServiceName is telemetry.tracing.service_name.
	DefaultTracingServiceName = "switchyard"
)

// pathTracingSampleRatio is the one tracing setting whose zero value is
// valid: 0 samples only the traces whose parent is sampled.
const pathTracingSampleRatio = "telemetry.tracing.sample_ratio"

// applyTracingDefaults fills telemetry.tracing. sample_ratio may be 0, so
// its default applies only when explicit, which may be nil, reports that
// the file did not set it.
func applyTracingDefaults(t *Tracing, explicit func(path string) bool) {
	if explicit == nil || !explicit(pathTracingSampleRatio) {
		setDefault(&t.SampleRatio, DefaultTracingSampleRatio)
	}
	setDefault(&t.ServiceName, DefaultTracingServiceName)
}
