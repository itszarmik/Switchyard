package config

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// rateLimitYAML returns a config with tenant t, whose rate_limit block is
// tenantLimit and whose key k has the rate_limit block keyLimit; an empty
// block is left out. extra is appended at the top level.
func rateLimitYAML(tenantLimit, keyLimit, extra string) string {
	key := "{id: k, hash: " + keyHash("a")
	if keyLimit != "" {
		key += ", rate_limit: " + keyLimit
	}
	key += "}"
	tenant := "{id: t, keys: [" + key + ", {id: k2, hash: " + keyHash("b") + "}]"
	if tenantLimit != "" {
		tenant += ", rate_limit: " + tenantLimit
	}
	tenant += "}"
	return "version: 1\n" + tenantsYAML(tenant) + extra
}

func TestRateLimitDefaults(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantGlobal  RateLimits
		wantTenant  RateLimit
		wantKey     *RateLimit
		wantNoLimit bool // the second key, which sets no rate_limit, keeps nil
	}{
		{
			name:       "nothing set",
			yaml:       rateLimitYAML("", "", ""),
			wantGlobal: RateLimits{FailMode: RateLimitFailOpen, AuthFailuresPerMinute: 30},
		},
		{
			name:       "burst is a sixth of the rate",
			yaml:       rateLimitYAML("{requests_per_minute: 600}", "{requests_per_minute: 60, max_concurrent_streams: 2}", ""),
			wantGlobal: RateLimits{FailMode: RateLimitFailOpen, AuthFailuresPerMinute: 30},
			wantTenant: RateLimit{RequestsPerMinute: 600, Burst: 100},
			wantKey:    &RateLimit{RequestsPerMinute: 60, Burst: 10, MaxConcurrentStreams: 2},
		},
		{
			name:       "burst is at least 1",
			yaml:       rateLimitYAML("{requests_per_minute: 5}", "{max_concurrent_streams: 3}", ""),
			wantGlobal: RateLimits{FailMode: RateLimitFailOpen, AuthFailuresPerMinute: 30},
			wantTenant: RateLimit{RequestsPerMinute: 5, Burst: 1},
			// A key's block replaces the tenant's: no rate, so no burst.
			wantKey: &RateLimit{MaxConcurrentStreams: 3},
		},
		{
			name:       "set values are kept, explicit burst 0 too",
			yaml:       rateLimitYAML("{requests_per_minute: 600, burst: 0}", "{requests_per_minute: 60, burst: 7}", "rate_limits: {fail_mode: closed, auth_failures_per_minute: -1}\n"),
			wantGlobal: RateLimits{FailMode: RateLimitFailClosed, AuthFailuresPerMinute: AuthFailuresDisabled},
			wantTenant: RateLimit{RequestsPerMinute: 600},
			wantKey:    &RateLimit{RequestsPerMinute: 60, Burst: 7},
		},
		{
			name:       "null takes the default",
			yaml:       rateLimitYAML("{requests_per_minute: 60, burst: null}", "", "rate_limits: {auth_failures_per_minute: null}\n"),
			wantGlobal: RateLimits{FailMode: RateLimitFailOpen, AuthFailuresPerMinute: 30},
			wantTenant: RateLimit{RequestsPerMinute: 60, Burst: 10},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustLoad(t, tt.yaml, nil, nil).Config()
			if diff := cmp.Diff(tt.wantGlobal, cfg.RateLimits); diff != "" {
				t.Errorf("rate_limits (-want +got):\n%s", diff)
			}
			tenant := cfg.Tenants[0]
			if diff := cmp.Diff(tt.wantTenant, tenant.RateLimit); diff != "" {
				t.Errorf("tenant rate_limit (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantKey, tenant.Keys[0].RateLimit); diff != "" {
				t.Errorf("key rate_limit (-want +got):\n%s", diff)
			}
			if tenant.Keys[1].RateLimit != nil {
				t.Errorf("key without rate_limit got %+v", *tenant.Keys[1].RateLimit)
			}
		})
	}
}

func TestDefaultBurst(t *testing.T) {
	tests := []struct{ rpm, want int }{{-1, 0}, {0, 0}, {1, 1}, {6, 1}, {11, 1}, {12, 2}, {600, 100}}
	for _, tt := range tests {
		if got := DefaultBurst(tt.rpm); got != tt.want {
			t.Errorf("DefaultBurst(%d) = %d, want %d", tt.rpm, got, tt.want)
		}
	}
}

func TestRateLimitValidation(t *testing.T) {
	const (
		tenantRPM = "tenants[0].rate_limit.requests_per_minute"
		keyBase   = "tenants[0].keys[0].rate_limit"
	)
	tests := []struct {
		name  string
		yaml  string
		wants []want
	}{
		{"valid", rateLimitYAML("{requests_per_minute: 1000000, burst: 1000000, max_concurrent_streams: 1000000}",
			"{requests_per_minute: 1}", "rate_limits: {fail_mode: closed, auth_failures_per_minute: 1000000}\n"), nil},
		{"negative rate", rateLimitYAML("{requests_per_minute: -1}", "", ""),
			[]want{errAt(tenantRPM, "must be between 0 and 1000000, got -1")}},
		{"rate too high", rateLimitYAML("{requests_per_minute: 1000001}", "", ""),
			[]want{errAt(tenantRPM, "must be between 0 and 1000000")}},
		{"negative key burst and streams", rateLimitYAML("", "{requests_per_minute: 60, burst: -2, max_concurrent_streams: -3}", ""),
			[]want{
				errAt(keyBase+".burst", "must be between 0 and 1000000, got -2"),
				errAt(keyBase+".max_concurrent_streams", "must be between 0 and 1000000, got -3"),
			}},
		{"burst without a rate", rateLimitYAML("{burst: 5}", "", ""),
			[]want{warnAt("tenants[0].rate_limit.burst", msgBurstWithoutRate)}},
		{"unknown fail mode", rateLimitYAML("", "", "rate_limits: {fail_mode: maybe}\n"),
			[]want{errAt("rate_limits.fail_mode", "must be one of open, closed")}},
		{"auth failures 0", rateLimitYAML("", "", "rate_limits: {auth_failures_per_minute: 0}\n"),
			[]want{errAt(pathAuthFailures, "must be -1 (disabled) or between 1 and 1000000, got 0")}},
		{"auth failures -2", rateLimitYAML("", "", "rate_limits: {auth_failures_per_minute: -2}\n"),
			[]want{errAt(pathAuthFailures, "got -2")}},
		{"unknown key", rateLimitYAML("{rpm: 5}", "", ""),
			[]want{errAt("tenants[0].rate_limit.rpm", "unknown")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkProblems(t, problemsOf(t, tt.yaml, nil, nil), tt.wants...)
		})
	}
}
