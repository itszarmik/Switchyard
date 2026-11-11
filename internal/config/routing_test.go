package config

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestRoutingDefaults(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want func(r *Routing)
	}{
		{"absent", "", func(*Routing) {}},
		{"empty sections", "routing: {fallback: {}, breaker: {}, rate_limit_cooldown: {}}\n", func(*Routing) {}},
		{"explicit zero retries", "routing: {fallback: {retries_per_deployment: 0}}\n",
			func(r *Routing) { r.Fallback.RetriesPerDeployment = 0 }},
		{"explicit zero budget min", "routing: {fallback: {retry_budget_min: 0}}\n",
			func(r *Routing) { r.Fallback.RetryBudgetMin = 0 }},
		{"null retries keeps the default", "routing: {fallback: {retries_per_deployment: ~}}\n", func(*Routing) {}},
		{"every key", `routing:
  request_timeout: 30s
  fallback: {max_deployments: 2, retries_per_deployment: 3, backoff_base: 10ms, backoff_max: 100ms, retry_budget_ratio: 0.5, retry_budget_min: 2}
  breaker: {consecutive_failures: 2, failure_ratio: 0.25, min_requests: 4, window: 5s, open_for: 1s, half_open_probes: 1}
  rate_limit_cooldown: {default: 2s, max: 4s}
  stream_keepalive: 15s
`, func(r *Routing) {
			r.RequestTimeout = Duration(30 * time.Second)
			r.Fallback = Fallback{
				MaxDeployments: 2, RetriesPerDeployment: 3, BackoffBase: Duration(10 * time.Millisecond),
				BackoffMax: Duration(100 * time.Millisecond), RetryBudgetRatio: 0.5, RetryBudgetMin: 2,
			}
			r.Breaker = Breaker{
				ConsecutiveFailures: 2, FailureRatio: 0.25, MinRequests: 4, Window: Duration(5 * time.Second),
				OpenFor: Duration(time.Second), HalfOpenProbes: 1,
			}
			r.RateLimitCooldown = RateLimitCooldown{Default: Duration(2 * time.Second), Max: Duration(4 * time.Second)}
			r.StreamKeepalive = Duration(15 * time.Second)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap, err := loadYAML(t, "version: 1\n"+tt.yaml, nil, nil)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			want := DefaultRouting()
			tt.want(&want)
			if diff := cmp.Diff(want, snap.Config().Routing); diff != "" {
				t.Errorf("routing (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDefaultRoutingMatchesDocumentation(t *testing.T) {
	r := DefaultRouting()
	f, b, c := r.Fallback, r.Breaker, r.RateLimitCooldown
	if r.RequestTimeout != Duration(10*time.Minute) || r.StreamKeepalive != 0 ||
		f.MaxDeployments != 3 || f.RetriesPerDeployment != 1 || f.BackoffBase != Duration(250*time.Millisecond) ||
		f.BackoffMax != Duration(2*time.Second) || f.RetryBudgetRatio != 0.2 || f.RetryBudgetMin != 5 ||
		b.ConsecutiveFailures != 5 || b.FailureRatio != 0.5 || b.MinRequests != 20 ||
		b.Window != Duration(30*time.Second) || b.OpenFor != Duration(30*time.Second) || b.HalfOpenProbes != 3 ||
		c.Default != Duration(time.Second) || c.Max != Duration(30*time.Second) {
		t.Errorf("DefaultRouting() = %+v", r)
	}
}

func TestRoutingValidation(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		wants []want
	}{
		{"keep-alive on", "routing: {stream_keepalive: 15s}\n", nil},
		{"one deployment, no retries", "routing: {fallback: {max_deployments: 1, retries_per_deployment: 0}}\n", nil},
		{"ratio 1", "routing: {fallback: {retry_budget_ratio: 1}, breaker: {failure_ratio: 1}}\n", nil},

		{"negative timeout", "routing: {request_timeout: -1s}\n",
			[]want{errAt("routing.request_timeout", "duration must not be negative")}},
		{"timeout too long", "routing: {request_timeout: 25h}\n",
			[]want{errAt("routing.request_timeout", "must be at most 24h")}},
		{"max_deployments 11", "routing: {fallback: {max_deployments: 11}}\n",
			[]want{errAt("routing.fallback.max_deployments", "must be between 1 and 10, got 11")}},
		{"max_deployments negative", "routing: {fallback: {max_deployments: -1}}\n",
			[]want{errAt("routing.fallback.max_deployments", "must be between 1 and 10, got -1")}},
		{"retries 6", "routing: {fallback: {retries_per_deployment: 6}}\n",
			[]want{errAt("routing.fallback.retries_per_deployment", "must be between 0 and 5, got 6")}},
		{"retries negative", "routing: {fallback: {retries_per_deployment: -1}}\n",
			[]want{errAt("routing.fallback.retries_per_deployment", "must be between 0 and 5, got -1")}},
		{"backoff_max below base", "routing: {fallback: {backoff_base: 1s, backoff_max: 500ms}}\n",
			[]want{errAt("routing.fallback.backoff_max", "must not be less than routing.fallback.backoff_base")}},
		{"backoff_base negative", "routing: {fallback: {backoff_base: -5ms}}\n",
			[]want{errAt("routing.fallback.backoff_base", "duration must not be negative")}},
		{"budget ratio above 1", "routing: {fallback: {retry_budget_ratio: 1.5}}\n",
			[]want{errAt("routing.fallback.retry_budget_ratio", "must be greater than 0 and at most 1, got 1.5")}},
		{"budget ratio negative", "routing: {fallback: {retry_budget_ratio: -0.1}}\n",
			[]want{errAt("routing.fallback.retry_budget_ratio", "got -0.1")}},
		{"budget min negative", "routing: {fallback: {retry_budget_min: -1}}\n",
			[]want{errAt("routing.fallback.retry_budget_min", "must be between 0 and 10000")}},
		{"breaker failures negative", "routing: {breaker: {consecutive_failures: -2}}\n",
			[]want{errAt("routing.breaker.consecutive_failures", "must be between 1 and 1000000")}},
		{"breaker ratio above 1", "routing: {breaker: {failure_ratio: 2}}\n",
			[]want{errAt("routing.breaker.failure_ratio", "at most 1, got 2")}},
		{"breaker min requests negative", "routing: {breaker: {min_requests: -1}}\n",
			[]want{errAt("routing.breaker.min_requests", "must be between 1")}},
		{"breaker window negative", "routing: {breaker: {window: -1s}}\n",
			[]want{errAt("routing.breaker.window", "duration must not be negative")}},
		{"breaker open_for negative", "routing: {breaker: {open_for: -1s}}\n",
			[]want{errAt("routing.breaker.open_for", "duration must not be negative")}},
		{"half-open probes negative", "routing: {breaker: {half_open_probes: -1}}\n",
			[]want{errAt("routing.breaker.half_open_probes", "must be between 1 and 1000")}},
		{"cooldown default above max", "routing: {rate_limit_cooldown: {default: 10s, max: 5s}}\n",
			[]want{errAt("routing.rate_limit_cooldown.default", "must not be greater than routing.rate_limit_cooldown.max")}},
		{"cooldown max negative", "routing: {rate_limit_cooldown: {max: -1s}}\n",
			[]want{errAt("routing.rate_limit_cooldown.max", "duration must not be negative")}},
		{"keep-alive negative", "routing: {stream_keepalive: -1s}\n",
			[]want{errAt("routing.stream_keepalive", "must not be negative")}},
		{"keep-alive too short", "routing: {stream_keepalive: 1ms}\n",
			[]want{errAt("routing.stream_keepalive", "must be 0s (off) or at least 10ms")}},
		{"unknown key", "routing: {fallback: {retries: 2}}\n",
			[]want{errAt("routing.fallback.retries", "unknown field")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkProblems(t, problemsOf(t, "version: 1\n"+tt.yaml, nil, nil), tt.wants...)
		})
	}
}
