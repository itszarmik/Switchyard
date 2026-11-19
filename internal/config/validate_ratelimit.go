package config

import "fmt"

// Rate-limit settings (DESIGN §7 step 3, §17).
const (
	// RateLimitFailOpen serves requests while the rate-limit store is
	// unavailable.
	RateLimitFailOpen = "open"
	// RateLimitFailClosed rejects requests with 503 while the rate-limit
	// store is unavailable.
	RateLimitFailClosed = "closed"
	// AuthFailuresDisabled, as rate_limits.auth_failures_per_minute, turns
	// the per-IP limit on failed authentications off.
	AuthFailuresDisabled = -1
)

// Upper bounds of the rate-limit settings. They keep every GCRA quantity,
// in microseconds, far inside the range where the Redis script's Lua
// arithmetic (IEEE doubles) is exact.
const (
	// MaxRequestsPerMinute is the largest requests_per_minute: one request
	// every 60 µs.
	MaxRequestsPerMinute = 1_000_000
	// MaxRateLimitBurst is the largest burst.
	MaxRateLimitBurst = 1_000_000
	// MaxConcurrentStreams is the largest max_concurrent_streams.
	MaxConcurrentStreams = 1_000_000
	// MaxAuthFailuresPerMinute is the largest auth_failures_per_minute.
	MaxAuthFailuresPerMinute = 1_000_000
)

// msgBurstWithoutRate warns about a burst that cannot apply.
const msgBurstWithoutRate = "has no effect while requests_per_minute is 0 (unlimited)"

// validateRateLimits checks rate_limits and the rate_limit of every tenant
// and key.
func (st *loadState) validateRateLimits(c *Config) {
	st.checkOneOf("rate_limits.fail_mode", c.RateLimits.FailMode, RateLimitFailOpen, RateLimitFailClosed)
	if v := c.RateLimits.AuthFailuresPerMinute; v != AuthFailuresDisabled && (v < 1 || v > MaxAuthFailuresPerMinute) {
		st.errorAt("rate_limits.auth_failures_per_minute",
			fmt.Sprintf("must be -1 (disabled) or between 1 and %d, got %d", MaxAuthFailuresPerMinute, v))
	}
	for i := range c.Tenants {
		t := &c.Tenants[i]
		base := indexPath("tenants", i)
		st.checkRateLimit(base+".rate_limit", t.RateLimit)
		for j := range t.Keys {
			if l := t.Keys[j].RateLimit; l != nil {
				st.checkRateLimit(indexPath(base+".keys", j)+".rate_limit", *l)
			}
		}
	}
}

// checkRateLimit checks the rate_limit block l at path base.
func (st *loadState) checkRateLimit(base string, l RateLimit) {
	st.checkIntRange(base+".requests_per_minute", l.RequestsPerMinute, 0, MaxRequestsPerMinute)
	st.checkIntRange(base+".burst", l.Burst, 0, MaxRateLimitBurst)
	st.checkIntRange(base+".max_concurrent_streams", l.MaxConcurrentStreams, 0, MaxConcurrentStreams)
	if l.RequestsPerMinute == 0 && l.Burst > 0 {
		st.warnAt(base+".burst", msgBurstWithoutRate)
	}
}
