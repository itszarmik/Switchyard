package config

import (
	"slices"
	"time"
)

// Defaults documented on the fields in types.go.
const (
	defaultListen            = ":8080"
	defaultAdminListen       = "127.0.0.1:9090"
	defaultMaxRequestBytes   = 10 * mib
	defaultMaxHeaderBytes    = 64 * kib
	defaultReadHeaderTimeout = 10 * time.Second
	defaultBodyReadTimeout   = 60 * time.Second
	defaultWriteTimeout      = 60 * time.Second
	defaultIdleTimeout       = 120 * time.Second
	defaultShutdownGrace     = 60 * time.Second
	defaultLogLevel          = "info"
	defaultLogFormat         = "json"
	defaultStorageMode       = "memory"
	defaultLedgerDriver      = "memory"
	defaultLedgerBatchSize   = 500
	defaultLedgerFlush       = time.Second
	defaultLedgerQueueSize   = 100_000
	defaultConnectTimeout    = 5 * time.Second
	defaultFirstTokenTimeout = 30 * time.Second
	defaultIdleChunkTimeout  = 60 * time.Second
	defaultTotalTimeout      = 10 * time.Minute
	defaultModelType         = ModelTypeChat
	defaultTokenRatio        = 1.0
)

// Budget defaults (DESIGN §12.3).
const (
	defaultOnExhausted    = BudgetBlock
	defaultBudgetFailMode = BudgetFailOpen
)

// Routing defaults (DESIGN §14).
const (
	defaultRequestTimeout       = 10 * time.Minute
	defaultMaxDeployments       = 3
	defaultRetriesPerDeployment = 1
	defaultBackoffBase          = 250 * time.Millisecond
	defaultBackoffMax           = 2 * time.Second
	defaultRetryBudgetRatio     = 0.2
	defaultRetryBudgetMin       = 5
	defaultConsecutiveFailures  = 5
	defaultFailureRatio         = 0.5
	defaultBreakerMinRequests   = 20
	defaultBreakerWindow        = 30 * time.Second
	defaultBreakerOpenFor       = 30 * time.Second
	defaultHalfOpenProbes       = 3
	defaultCooldown             = time.Second
	defaultCooldownMax          = 30 * time.Second
)

// Routing profile, assessor and policy defaults (DESIGN §9.3, §13).
const (
	// DefaultProfileName is routing.default_profile when the file sets none.
	DefaultProfileName          = "balanced"
	defaultExpectedOutputTokens = 500
	defaultObjective            = ObjectiveCost
	defaultAssessorType         = AssessorHeuristic
	defaultSubstitutionMode     = SubstitutionPinned
	// DefaultMaxQualityDrop is substitution.max_quality_drop when a policy
	// sets none.
	DefaultMaxQualityDrop = 0.05
	defaultRoutingMode    = RoutingModeEnforce
	defaultOnUnmet        = OnUnmetBestEffort
)

// Config paths whose zero value is valid, so a default applies only when
// the file does not set them.
const (
	pathRetriesPerDeployment = "routing.fallback.retries_per_deployment"
	pathRetryBudgetMin       = "routing.fallback.retry_budget_min"
)

// DefaultProfiles returns the built-in profiles (DESIGN §9.3): cheap,
// balanced, quality and fast. They apply only when the file configures no
// profiles; configured profiles replace them entirely.
func DefaultProfiles() map[string]Profile {
	return map[string]Profile{
		"cheap":    {MinQuality: 0.65, Objective: ObjectiveCost},
		"balanced": {MinQuality: 0.80, Objective: ObjectiveCost},
		"quality":  {MinQuality: 0.92, Objective: ObjectiveCost},
		"fast":     {MinQuality: 0.80, Objective: ObjectiveLatency},
	}
}

// DefaultRouting returns the documented routing defaults. Code that builds
// a Config by hand rather than loading one can use it to fill the routing
// section.
func DefaultRouting() Routing {
	return Routing{
		RequestTimeout: Duration(defaultRequestTimeout),
		Fallback: Fallback{
			MaxDeployments:       defaultMaxDeployments,
			RetriesPerDeployment: defaultRetriesPerDeployment,
			BackoffBase:          Duration(defaultBackoffBase),
			BackoffMax:           Duration(defaultBackoffMax),
			RetryBudgetRatio:     defaultRetryBudgetRatio,
			RetryBudgetMin:       defaultRetryBudgetMin,
		},
		Breaker: Breaker{
			ConsecutiveFailures: defaultConsecutiveFailures,
			FailureRatio:        defaultFailureRatio,
			MinRequests:         defaultBreakerMinRequests,
			Window:              Duration(defaultBreakerWindow),
			OpenFor:             Duration(defaultBreakerOpenFor),
			HalfOpenProbes:      defaultHalfOpenProbes,
		},
		RateLimitCooldown: RateLimitCooldown{
			Default: Duration(defaultCooldown),
			Max:     Duration(defaultCooldownMax),
		},
		DefaultProfile:       DefaultProfileName,
		Profiles:             DefaultProfiles(),
		ExpectedOutputTokens: defaultExpectedOutputTokens,
		Assessor:             AssessorConfig{Type: defaultAssessorType},
	}
}

// applyDefaults fills zero-valued fields with their documented defaults.
// ShutdownDelay and StreamKeepalive default to 0, so they need no entry.
// For the fields whose zero value is valid (retries_per_deployment and
// retry_budget_min), the default applies only when explicit, which may be
// nil, reports that the file did not set them.
func applyDefaults(c *Config, explicit func(path string) bool) {
	setDefault(&c.Server.Listen, defaultListen)
	setDefault(&c.Server.AdminListen, defaultAdminListen)
	setDefault(&c.Server.MaxRequestBytes, defaultMaxRequestBytes)
	setDefault(&c.Server.MaxHeaderBytes, defaultMaxHeaderBytes)
	setDefault(&c.Server.ReadHeaderTimeout, Duration(defaultReadHeaderTimeout))
	setDefault(&c.Server.BodyReadTimeout, Duration(defaultBodyReadTimeout))
	setDefault(&c.Server.WriteTimeout, Duration(defaultWriteTimeout))
	setDefault(&c.Server.IdleTimeout, Duration(defaultIdleTimeout))
	setDefault(&c.Server.ShutdownGrace, Duration(defaultShutdownGrace))
	setDefault(&c.Logging.Level, defaultLogLevel)
	setDefault(&c.Logging.Format, defaultLogFormat)
	setDefault(&c.Storage.Mode, defaultStorageMode)
	setDefault(&c.Storage.Ledger.Driver, defaultLedgerDriver)
	setDefault(&c.Storage.Ledger.BatchSize, defaultLedgerBatchSize)
	setDefault(&c.Storage.Ledger.FlushInterval, Duration(defaultLedgerFlush))
	setDefault(&c.Storage.Ledger.QueueSize, defaultLedgerQueueSize)
	for i := range c.Providers {
		t := &c.Providers[i].Timeouts
		setDefault(&t.Connect, Duration(defaultConnectTimeout))
		setDefault(&t.FirstToken, Duration(defaultFirstTokenTimeout))
		setDefault(&t.Idle, Duration(defaultIdleChunkTimeout))
		setDefault(&t.Total, Duration(defaultTotalTimeout))
	}
	for i := range c.Models {
		m := &c.Models[i]
		setDefault(&m.Type, defaultModelType)
		setDefault(&m.TokenRatio, defaultTokenRatio)
	}
	applyRoutingDefaults(&c.Routing, explicit)
	applyBudgetDefaults(c)
	applyPolicyDefaults(c.Policies, explicit)
	applyEvaluationDefaults(&c.Evaluation)
}

// applyPolicyDefaults fills substitution, routing_mode and on_unmet on every
// policy. max_quality_drop may be 0, so its default applies only when the
// file does not set it. Rule overrides are left alone: their zero values
// mean "keep the policy's value".
func applyPolicyDefaults(ps []Policy, explicit func(path string) bool) {
	for i := range ps {
		p := &ps[i]
		setDefault(&p.Substitution.Mode, defaultSubstitutionMode)
		if explicit == nil || !explicit(indexPath("policies", i)+".substitution.max_quality_drop") {
			setDefault(&p.Substitution.MaxQualityDrop, DefaultMaxQualityDrop)
		}
		setDefault(&p.RoutingMode, defaultRoutingMode)
		setDefault(&p.OnUnmet, defaultOnUnmet)
	}
}

// applyBudgetDefaults fills on_exhausted on every tenant budget and
// budgets.fail_mode.
func applyBudgetDefaults(c *Config) {
	for i := range c.Tenants {
		for j := range c.Tenants[i].Budgets {
			setDefault(&c.Tenants[i].Budgets[j].OnExhausted, defaultOnExhausted)
		}
	}
	setDefault(&c.Budgets.FailMode, defaultBudgetFailMode)
}

// applyRoutingDefaults fills the routing section; see applyDefaults.
func applyRoutingDefaults(r *Routing, explicit func(path string) bool) {
	def := DefaultRouting()
	setDefault(&r.RequestTimeout, def.RequestTimeout)
	f, df := &r.Fallback, def.Fallback
	setDefault(&f.MaxDeployments, df.MaxDeployments)
	setDefault(&f.BackoffBase, df.BackoffBase)
	setDefault(&f.BackoffMax, df.BackoffMax)
	setDefault(&f.RetryBudgetRatio, df.RetryBudgetRatio)
	if explicit == nil || !explicit(pathRetriesPerDeployment) {
		setDefault(&f.RetriesPerDeployment, df.RetriesPerDeployment)
	}
	if explicit == nil || !explicit(pathRetryBudgetMin) {
		setDefault(&f.RetryBudgetMin, df.RetryBudgetMin)
	}
	b, db := &r.Breaker, def.Breaker
	setDefault(&b.ConsecutiveFailures, db.ConsecutiveFailures)
	setDefault(&b.FailureRatio, db.FailureRatio)
	setDefault(&b.MinRequests, db.MinRequests)
	setDefault(&b.Window, db.Window)
	setDefault(&b.OpenFor, db.OpenFor)
	setDefault(&b.HalfOpenProbes, db.HalfOpenProbes)
	setDefault(&r.RateLimitCooldown.Default, def.RateLimitCooldown.Default)
	setDefault(&r.RateLimitCooldown.Max, def.RateLimitCooldown.Max)
	setDefault(&r.DefaultProfile, def.DefaultProfile)
	setDefault(&r.ExpectedOutputTokens, def.ExpectedOutputTokens)
	setDefault(&r.Assessor.Type, def.Assessor.Type)
	if len(r.Profiles) == 0 {
		r.Profiles = def.Profiles
	}
	for name, p := range r.Profiles {
		if p.Objective == "" {
			p.Objective = defaultObjective
			r.Profiles[name] = p
		}
	}
}

// setDefault sets *p to def if *p is the zero value.
func setDefault[T comparable](p *T, def T) {
	var zero T
	if *p == zero {
		*p = def
	}
}

// applyOverrides applies LoadOptions.Overrides on top of the files and
// defaults. Keys are applied in sorted order so problems are deterministic.
func (st *loadState) applyOverrides(c *Config) {
	keys := make([]string, 0, len(st.opts.Overrides))
	for k := range st.opts.Overrides {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v := st.opts.Overrides[k]
		switch k {
		case "server.listen":
			c.Server.Listen = v
		case "server.admin_listen":
			c.Server.AdminListen = v
		case "logging.level":
			c.Logging.Level = v
		default:
			st.problems = append(st.problems, Problem{
				Severity: SeverityError, Path: k,
				Message: "unsupported override; only server.listen, server.admin_listen and logging.level can be overridden",
			})
			continue
		}
		st.overridden[k] = true
		delete(st.failed, k) // the override replaces whatever failed to decode
	}
}
