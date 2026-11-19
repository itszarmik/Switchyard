package config

// RestartOnly lists the config paths whose changes take effect only at
// startup, which a reload reports as restart_required: the listeners and
// HTTP server settings, TLS, storage, logging, telemetry, evaluation (the
// judge and the shadow runner), the response cache limits and the
// semantic cache's embedder, and the health tracker's settings. A path
// names a key and everything under it. server.admin_token and the rate
// limits, per key, per tenant and rate_limits.*, are reloaded.
func RestartOnly() []string {
	return []string{
		"server.listen", "server.admin_listen", "server.max_request_bytes", "server.max_header_bytes",
		"server.read_header_timeout", "server.body_read_timeout", "server.write_timeout",
		"server.idle_timeout", "server.shutdown_delay", "server.shutdown_grace", "server.tls",
		"logging", "storage", "telemetry", "evaluation", "cache", "semantic_cache",
		"routing.breaker", "routing.rate_limit_cooldown",
		"routing.fallback.retry_budget_ratio", "routing.fallback.retry_budget_min",
	}
}
