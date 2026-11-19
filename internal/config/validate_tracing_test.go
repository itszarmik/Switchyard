package config

import (
	"strings"
	"testing"
)

// tracingYAML returns a minimal config with the given telemetry.tracing
// flow mapping body.
func tracingYAML(body string) string {
	return "version: 1\ntelemetry: {tracing: {" + body + "}}\n"
}

func TestTracingDefaults(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want Tracing
	}{
		{"unset", "version: 1\n", Tracing{SampleRatio: 0.05, ServiceName: "switchyard"}},
		{"empty block", tracingYAML(""), Tracing{SampleRatio: 0.05, ServiceName: "switchyard"}},
		{"null ratio takes the default", tracingYAML("sample_ratio: null"), Tracing{SampleRatio: 0.05, ServiceName: "switchyard"}},
		{"explicit zero ratio is kept", tracingYAML("sample_ratio: 0"), Tracing{SampleRatio: 0, ServiceName: "switchyard"}},
		{"set values are kept", tracingYAML("enabled: true, endpoint: 'http://otel:4318', sample_ratio: 0.5, service_name: gw, propagate_upstream: true"),
			Tracing{Enabled: true, Endpoint: NewSecret("http://otel:4318"), SampleRatio: 0.5, ServiceName: "gw", PropagateUpstream: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustLoad(t, tt.yaml, nil, nil).Config().Telemetry.Tracing
			if got.Enabled != tt.want.Enabled || got.Endpoint.Value() != tt.want.Endpoint.Value() ||
				got.SampleRatio != tt.want.SampleRatio || got.ServiceName != tt.want.ServiceName ||
				got.PropagateUpstream != tt.want.PropagateUpstream {
				t.Errorf("telemetry.tracing = %+v (endpoint %q), want %+v (endpoint %q)",
					got, got.Endpoint.Value(), tt.want, tt.want.Endpoint.Value())
			}
		})
	}

	t.Run("hand-built config", func(t *testing.T) {
		var tr Tracing
		applyTracingDefaults(&tr, nil)
		if tr.SampleRatio != DefaultTracingSampleRatio || tr.ServiceName != DefaultTracingServiceName {
			t.Errorf("applyTracingDefaults(nil explicit) = %+v", tr)
		}
	})
}

func TestTracingValidation(t *testing.T) {
	const ep = pathTracingEndpoint
	env := map[string]string{
		"OTLP":       "http://trace-sink.internal:4318",
		"OTLP_CREDS": "http://tenant:Zq8sUperSecretTracePass@trace-sink.internal:4318",
		"OTLP_TLS":   "https://tenant:Zq8sUperSecretTracePass@otel.example/otlp",
		"OTLP_QUERY": "https://otel.example/otlp?token=Zq8sUperSecretTracePass",
	}
	tests := []struct {
		name  string
		yaml  string
		wants []want
	}{
		{"disabled without endpoint", tracingYAML("enabled: false"), nil},
		{"enabled with endpoint", tracingYAML("enabled: true, endpoint: '${OTLP}'"), nil},
		{"enabled with a path prefix", tracingYAML("enabled: true, endpoint: 'https://otel.example/otlp/'"), nil},
		{"credentials over https", tracingYAML("enabled: true, endpoint: '${OTLP_TLS}'"), nil},
		{"ratio bounds", tracingYAML("sample_ratio: 1"), nil},

		{"enabled without endpoint", tracingYAML("enabled: true"),
			[]want{errAt(ep, "required when telemetry.tracing.enabled is true")}},
		{"enabled with empty endpoint", tracingYAML("enabled: true, endpoint: ''"),
			[]want{errAt(ep, "required when telemetry.tracing.enabled is true")}},
		{"not a URL", tracingYAML("enabled: true, endpoint: 'trace-sink.internal:4318'"),
			[]want{errAt(ep, "must be an absolute http:// or https:// URL with a host")}},
		{"grpc scheme", tracingYAML("endpoint: 'grpc://otel:4317'"),
			[]want{errAt(ep, "must be an absolute http:// or https:// URL with a host")}},
		{"no host", tracingYAML("endpoint: 'http:///v1'"),
			[]want{errAt(ep, "must be an absolute http:// or https:// URL with a host")}},
		{"port without host", tracingYAML("endpoint: 'http://:4318'"),
			[]want{errAt(ep, "must be an absolute http:// or https:// URL with a host")}},
		{"query string", tracingYAML("enabled: true, endpoint: '${OTLP_QUERY}'"),
			[]want{errAt(ep, "must not have a query string")}},
		{"fragment", tracingYAML("endpoint: 'http://otel:4318/#x'"),
			[]want{errAt(ep, "must not have a fragment")}},
		{"credentials over http", tracingYAML("enabled: true, endpoint: '${OTLP_CREDS}'"),
			[]want{warnAt(ep, "sends its credentials over plain http; use https")}},
		{"literal password", tracingYAML("enabled: true, endpoint: 'https://tenant:Zq8sUperSecretTracePass@otel.example'"),
			[]want{warnAt(ep, msgLiteralPassword)}},
		{"ratio above 1", tracingYAML("sample_ratio: 1.5"),
			[]want{errAt(pathTracingSampleRatio, "must be between 0 and 1, got 1.5")}},
		{"ratio negative", tracingYAML("sample_ratio: -0.1"),
			[]want{errAt(pathTracingSampleRatio, "must be between 0 and 1, got -0.1")}},
		{"blank service name", tracingYAML("service_name: '  '"),
			[]want{errAt("telemetry.tracing.service_name", "must not be blank")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := problemsOf(t, tt.yaml, env, nil)
			checkProblems(t, got, tt.wants...)
			for _, p := range got {
				if s := p.String(); strings.Contains(s, "Zq8sUperSecretTracePass") || strings.Contains(s, "trace-sink") {
					t.Errorf("problem echoes the endpoint: %s", s)
				}
			}
		})
	}
}
