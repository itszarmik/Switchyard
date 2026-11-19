package config

import (
	"net/url"
	"strings"
)

// pathTracingEndpoint is the secret holding the collector URL, which may
// embed credentials.
const pathTracingEndpoint = "telemetry.tracing.endpoint"

// validateTracing checks telemetry.tracing: enabled requires an endpoint,
// which, when set, must be an absolute http(s) URL with a host and no query
// or fragment (the exporter would drop them); sample_ratio must lie in
// [0, 1]; and service_name must not be blank. The endpoint is a secret and
// is never echoed. Credentials in it are sent as HTTP Basic
// authentication, so a warning flags them over plain http.
func (st *loadState) validateTracing(t *Tracing) {
	switch {
	case t.Endpoint.IsSet():
		st.checkTracingEndpoint(pathTracingEndpoint, t.Endpoint)
	case t.Enabled:
		st.errorAt(pathTracingEndpoint, "required when telemetry.tracing.enabled is true")
	}
	st.checkUnit(pathTracingSampleRatio, t.SampleRatio)
	if strings.TrimSpace(t.ServiceName) == "" {
		st.errorAt("telemetry.tracing.service_name", "must not be blank")
	}
}

// checkTracingEndpoint validates an OTLP/HTTP base URL without echoing it.
func (st *loadState) checkTracingEndpoint(path string, s Secret) {
	raw := s.Value()
	u, err := url.Parse(raw)
	switch {
	case err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "":
		st.errorAt(path, "must be an absolute http:// or https:// URL with a host, such as http://otel-collector:4318")
	case u.RawQuery != "" || u.ForceQuery:
		st.errorAt(path, "must not have a query string; the OTLP exporter does not send it")
	case strings.Contains(raw, "#"):
		st.errorAt(path, "must not have a fragment")
	case u.User != nil && u.Scheme == "http":
		st.warnAt(path, "sends its credentials over plain http; use https")
	}
}
