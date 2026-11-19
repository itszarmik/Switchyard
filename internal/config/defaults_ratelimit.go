package config

// Rate-limit defaults (DESIGN §7 step 3), documented on the fields in
// types.go.
const (
	// DefaultRateLimitFailMode is rate_limits.fail_mode.
	DefaultRateLimitFailMode = RateLimitFailOpen
	// DefaultAuthFailuresPerMinute is rate_limits.auth_failures_per_minute.
	DefaultAuthFailuresPerMinute = 30
	// burstDivisor makes the default burst ten seconds' worth of the
	// sustained rate.
	burstDivisor = 6
)

// pathAuthFailures is the config path of rate_limits.auth_failures_per_minute.
const pathAuthFailures = "rate_limits.auth_failures_per_minute"

// DefaultBurst is the burst of a rate limit of rpm requests per minute
// that sets none: rpm/6 (ten seconds' worth), at least 1, or 0 when rpm is
// 0 (unlimited).
func DefaultBurst(rpm int) int {
	if rpm <= 0 {
		return 0
	}
	return max(rpm/burstDivisor, 1)
}

// applyRateLimitDefaults fills rate_limits and the burst of every tenant's
// and key's rate_limit. A burst or auth_failures_per_minute of 0 that the
// file sets explicitly is kept: burst 0 means no burst, and
// auth_failures_per_minute 0 is reported by validation. explicit may be
// nil, for a configuration built by hand.
func applyRateLimitDefaults(c *Config, explicit func(path string) bool) {
	isSet := func(path string) bool { return explicit != nil && explicit(path) }
	setDefault(&c.RateLimits.FailMode, DefaultRateLimitFailMode)
	if !isSet(pathAuthFailures) {
		setDefault(&c.RateLimits.AuthFailuresPerMinute, DefaultAuthFailuresPerMinute)
	}
	burst := func(path string, l *RateLimit) {
		if !isSet(path + ".burst") {
			setDefault(&l.Burst, DefaultBurst(l.RequestsPerMinute))
		}
	}
	for i := range c.Tenants {
		t := &c.Tenants[i]
		base := indexPath("tenants", i)
		burst(base+".rate_limit", &t.RateLimit)
		for j := range t.Keys {
			if l := t.Keys[j].RateLimit; l != nil {
				burst(indexPath(base+".keys", j)+".rate_limit", l)
			}
		}
	}
}
