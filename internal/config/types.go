package config

import "time"

// This file defines the configuration schema: the shape of switchyard.yaml
// after includes are merged, ${…} references are expanded and defaults are
// applied. Field names and yaml tags are the operator-facing contract
// (docs/DESIGN.md §16); renaming one breaks existing config files.
// docs/configuration.md is generated from these comments (make
// docs-config).

// Config is the root of the configuration.
type Config struct {
	// Version is the schema version. Only 1 is defined, and it is required.
	Version int `yaml:"version"`
	// Include lists glob patterns of additional files, resolved relative to the
	// directory of the root file. Included files may only define providers,
	// models, aliases and quality_overrides; list entries are appended in
	// sorted file-name order, and a duplicate ID, or an alias defined
	// twice, in any files, is an error.
	Include []string `yaml:"include"`

	Server    Server     `yaml:"server"`
	Logging   Logging    `yaml:"logging"`
	Storage   Storage    `yaml:"storage"`
	Providers []Provider `yaml:"providers"`
	Models    []Model    `yaml:"models"`
	// Tenants own the API keys that clients use to call the gateway. With no
	// tenants configured, every /v1 request is rejected with 401.
	Tenants []Tenant `yaml:"tenants"`
	// Aliases maps virtual model names to a catalog model ID or a pinned
	// deployment (model@provider), or the virtual model "auto" or
	// "auto:<profile>" of a defined profile. Alias names follow Model.ID
	// rules and must not collide with a model ID.
	Aliases map[string]string `yaml:"aliases"`
	Routing Routing           `yaml:"routing"`
	Budgets Budgets           `yaml:"budgets"`
	// Policies are referenced by tenants and keys (DESIGN §13).
	Policies []Policy `yaml:"policies"`
	// QualityOverrides replace catalog quality priors with measured values,
	// usually written by `switchyard eval fit` into an included file.
	QualityOverrides []QualityOverride `yaml:"quality_overrides"`
	// Evaluation configures the judge and shadow sampling.
	Evaluation Evaluation `yaml:"evaluation"`
	// Cache holds gateway-wide response-cache limits (DESIGN §11). Whether
	// a request is cached is set per policy (Policy.Cache).
	Cache CacheConfig `yaml:"cache"`
	// SemanticCache configures the embedding model and lookup budget of
	// the semantic cache (DESIGN §11.3).
	SemanticCache SemanticCacheConfig `yaml:"semantic_cache"`
	// RateLimits holds gateway-wide rate-limit settings; the limits
	// themselves are per tenant and key.
	RateLimits RateLimits `yaml:"rate_limits"`
	// Telemetry configures tracing (DESIGN §15.3).
	Telemetry Telemetry `yaml:"telemetry"`
}

// Telemetry configures OpenTelemetry export.
type Telemetry struct {
	Tracing Tracing `yaml:"tracing"`
}

// Tracing configures OpenTelemetry traces, exported over OTLP/HTTP
// (protobuf). Switchyard accepts a W3C traceparent from clients either
// way; spans are recorded and exported only when Enabled.
type Tracing struct {
	// Enabled turns tracing on. Default false.
	Enabled bool `yaml:"enabled"`
	// Endpoint is the collector's OTLP/HTTP base URL, for example
	// http://otel-collector:4318; traces are posted to <endpoint>/v1/traces.
	// Required when Enabled. It may hold credentials, so it is a secret.
	Endpoint Secret `yaml:"endpoint"`
	// SampleRatio is the fraction of new traces sampled, in [0, 1]; a
	// request that arrives with a sampled parent is always sampled
	// (parent-based), so clients control sampling, and the trace ID of
	// their requests' spans and log lines, through the traceparent they
	// send. Default 0.05.
	SampleRatio float64 `yaml:"sample_ratio"`
	// ServiceName is the service.name resource attribute. Default
	// switchyard.
	ServiceName string `yaml:"service_name"`
	// PropagateUpstream sends the W3C traceparent of the gateway's
	// upstream attempt span to providers, only with tracing enabled;
	// tracestate and baggage are never sent, and nor is a client's own
	// trace context. Default false: provider calls leave the trust
	// boundary.
	PropagateUpstream bool `yaml:"propagate_upstream"`
}

// Server configures the public and admin HTTP listeners.
type Server struct {
	// Listen is the public API address (host:port). Default ":8080".
	Listen string `yaml:"listen"`
	// AdminListen is the address for /metrics and /admin/v1/*. Default
	// "127.0.0.1:9090". It must never be reachable from the internet.
	AdminListen string `yaml:"admin_listen"`
	// MaxRequestBytes caps request bodies; larger bodies get 413. Default 10MiB.
	MaxRequestBytes ByteSize `yaml:"max_request_bytes"`
	// MaxHeaderBytes caps request headers; larger headers get 431. Default
	// 64KiB. net/http allows up to 4 KiB beyond this before it rejects them.
	MaxHeaderBytes ByteSize `yaml:"max_header_bytes"`
	// ReadHeaderTimeout bounds how long a client may take to send headers. Default 10s.
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	// BodyReadTimeout bounds how long a client may take to send a request
	// body to an API endpoint; a slower one gets 408. Default 60s.
	BodyReadTimeout Duration `yaml:"body_read_timeout"`
	// WriteTimeout bounds each write to an API client: a whole JSON
	// response, or one streamed event. A client that stops reading cannot
	// hold its request, and the upstream call behind it, longer. Default
	// 60s.
	WriteTimeout Duration `yaml:"write_timeout"`
	// IdleTimeout closes idle keep-alive connections. Default 120s.
	IdleTimeout Duration `yaml:"idle_timeout"`
	// ShutdownDelay is how long the server keeps serving after SIGTERM while
	// /readyz already fails, so load balancers can stop routing to it first.
	// Default 0s.
	ShutdownDelay Duration `yaml:"shutdown_delay"`
	// ShutdownGrace bounds how long in-flight requests may take to finish once
	// the listeners have closed. Default 60s.
	ShutdownGrace Duration `yaml:"shutdown_grace"`
	// AdminToken, when set, must be sent as "Authorization: Bearer <token>"
	// on every /admin/v1/* route; /metrics and the health routes of the
	// admin listener stay open, for scrapers and probes. It is required
	// when AdminListen is not a loopback address. It must be at least 32
	// characters, without whitespace or control characters (`openssl rand
	// -base64 32` or `switchyard keys generate` make one). Failed admin
	// tokens count against the client address's
	// rate_limits.auth_failures_per_minute, as failed API keys do.
	AdminToken Secret `yaml:"admin_token"`
	// TLS, when CertFile and KeyFile are set, serves the public listener
	// over HTTPS. Both or neither must be set.
	TLS ServerTLS `yaml:"tls"`
}

// ServerTLS configures HTTPS on the public listener. The files are read at
// startup and on reload is reported as needing a restart.
type ServerTLS struct {
	// CertFile is a PEM certificate chain; KeyFile its PEM private key.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// MinVersion is 1.2 (default) or 1.3.
	MinVersion string `yaml:"min_version"`
}

// Logging configures the process logger.
type Logging struct {
	// Level is one of debug, info, warn, error. Default info.
	Level string `yaml:"level"`
	// Format is json or text. Default json.
	Format string `yaml:"format"`
}

// Storage selects the state backends: where budgets, rate limits, cache
// entries and namespace versions live, and the usage ledger.
type Storage struct {
	// Mode is memory (quickstart and dev only; state is lost on restart) or
	// redis. Default memory.
	Mode   string       `yaml:"mode"`
	Redis  RedisStorage `yaml:"redis"`
	Ledger Ledger       `yaml:"ledger"`
}

// RedisStorage holds the two Redis roles described in DESIGN §11.5. The URLs
// are secrets because they may embed passwords.
type RedisStorage struct {
	// CacheURL is the evictable cache instance (maxmemory-policy allkeys-lru).
	// Required when Storage.Mode is redis.
	CacheURL Secret `yaml:"cache_url"`
	// StateURL is the non-evictable state instance (maxmemory-policy
	// noeviction). Required when Storage.Mode is redis.
	StateURL Secret `yaml:"state_url"`
}

// Ledger configures where usage events are stored.
type Ledger struct {
	// Driver is memory or postgres. Default memory.
	Driver string `yaml:"driver"`
	// DSN is the Postgres connection string. Required when Driver is postgres.
	DSN Secret `yaml:"dsn"`
	// BatchSize is the most usage events written in one batch. Default 500.
	BatchSize int `yaml:"batch_size"`
	// FlushInterval is the longest an event waits before its batch is
	// written. Default 1s.
	FlushInterval Duration `yaml:"flush_interval"`
	// QueueSize bounds the in-memory queue of events waiting to be written.
	// When it is full the oldest event is dropped and counted (DESIGN §12.2).
	// Default 100000.
	QueueSize int `yaml:"queue_size"`
}

// Provider is an upstream endpoint plus its credentials.
type Provider struct {
	// ID names the provider in deployment IDs (model@provider).
	ID string `yaml:"id"`
	// Type is the wire protocol: openai (OpenAI and every OpenAI-compatible
	// upstream), anthropic (the Messages API) or gemini (Google's native
	// generateContent API; base_url is the API version root, such as
	// https://generativelanguage.googleapis.com/v1beta).
	Type string `yaml:"type"`
	// BaseURL is the API root, e.g. https://api.openai.com/v1. It must be an
	// absolute http(s) URL and must not embed credentials.
	BaseURL string `yaml:"base_url"`
	// APIKey is the upstream credential. Optional, for self-hosted upstreams.
	APIKey Secret `yaml:"api_key"`
	// Tags are matched by policies, e.g. self_hosted, zdr, region_eu.
	Tags []string `yaml:"tags"`
	// MaxConcurrency caps in-flight requests per replica; 0 means unlimited.
	MaxConcurrency int `yaml:"max_concurrency"`
	// ForwardHeaders lists client headers that may be forwarded upstream.
	// Authorization and Cookie can never be forwarded.
	ForwardHeaders []string         `yaml:"forward_headers"`
	Timeouts       ProviderTimeouts `yaml:"timeouts"`
}

// ProviderTimeouts bound each upstream call.
type ProviderTimeouts struct {
	// Connect bounds TCP and TLS connection setup. Default 5s.
	Connect Duration `yaml:"connect"`
	// FirstToken bounds a stream's wait for its first event. It applies to
	// streams only: a non-streaming response arrives only after the whole
	// generation, so Total and the request's deadline bound it instead.
	// Default 30s.
	FirstToken Duration `yaml:"first_token"`
	// Idle bounds the gap between streamed chunks, measured only while
	// the gateway waits for the upstream, not while it writes to its
	// client. Default 60s.
	Idle Duration `yaml:"idle"`
	// Total bounds the whole upstream call, including the full streamed
	// response. Default 10m.
	Total Duration `yaml:"total"`
}

// Model is a logical model in the catalog (DESIGN §16.4).
type Model struct {
	// ID is the name clients send in the model field. It must not contain "@"
	// and must not be "auto" or start with "auto:", which are reserved for
	// virtual models.
	ID string `yaml:"id"`
	// Type is chat (default) or embedding. Chat models serve
	// /v1/chat/completions and /v1/completions; embedding models serve
	// /v1/embeddings.
	Type string `yaml:"type"`
	// ContextWindow is the maximum number of input plus output tokens.
	// 0 means unknown, which disables the context-size filter for this model.
	ContextWindow int64 `yaml:"context_window"`
	// MaxOutputTokens is the most the model can generate in one response.
	// Adapters that require max_tokens (Anthropic) use it as the default.
	// 0 means unknown.
	MaxOutputTokens int64 `yaml:"max_output_tokens"`
	// Capabilities lists what the model supports, from a fixed vocabulary:
	// streaming, stream_usage, tools, parallel_tools, forced_tools, vision,
	// json_mode, json_schema, n, logprobs, seed, sampling, temperature_2,
	// penalties, completions, prompt_caching. json_schema means native
	// structured-output support: no deployment emulates it. A request
	// needing a capability that a deployment lacks is never sent to it
	// (DESIGN §6.2, §9.2), unless the parameters that need it are dropped
	// (Routing).
	Capabilities []string `yaml:"capabilities"`
	// Quality holds quality priors in [0, 1] per category: general, coding,
	// reasoning, extraction, creative, tool_use (DESIGN §10.2), used by
	// cost-aware routing; quality_overrides can replace them.
	Quality map[string]float64 `yaml:"quality"`
	// TokenRatio scales the bytes/4 token estimate for this model's
	// tokenizer. 0 means 1.
	TokenRatio float64 `yaml:"token_ratio"`
	// Dimensions is the vector size of an embedding model.
	Dimensions int `yaml:"dimensions"`
	// Deployments lists the providers that serve this model. At least one is
	// required, and each provider may appear at most once per model.
	Deployments []Deployment `yaml:"deployments"`
}

// Deployment is a model served by one provider. Its ID is model@provider.
type Deployment struct {
	// Provider references Provider.ID.
	Provider string `yaml:"provider"`
	// UpstreamModel is the model name sent to the provider.
	UpstreamModel string `yaml:"upstream_model"`
	// Pricing is what this deployment costs.
	Pricing Pricing `yaml:"pricing"`
	// Capabilities, when set, replaces the model's list for this deployment
	// (for example a host that lacks parallel tool calls).
	Capabilities []string `yaml:"capabilities"`
	// Tags are added to the provider's tags for this deployment.
	Tags []string `yaml:"tags"`
	// QualityOffset adjusts the model's quality priors for this deployment,
	// e.g. -0.02 for a quantized variant. Must be in [-1, 1].
	QualityOffset float64 `yaml:"quality_offset"`
	// LatencyHint seeds latency estimates until there are measurements.
	LatencyHint LatencyHint `yaml:"latency_hint"`
}

// Pricing is in USD per million tokens. A price that is not set is unknown;
// routing ranks deployments with unknown prices last, and `switchyard
// catalog lint` reports them.
type Pricing struct {
	// Input is the price of input tokens that are not read from a cache.
	// Every price is in USD per million tokens; one that is not set is
	// unknown.
	Input Price `yaml:"input"`
	// Output is the price of output tokens.
	Output Price `yaml:"output"`
	// CachedInput is the price of input tokens read from the provider's
	// prompt cache (a cache hit).
	CachedInput Price `yaml:"cached_input"`
	// CacheWrite is the price of input tokens written to the cache, where
	// the provider charges one per token.
	CacheWrite Price `yaml:"cache_write"`
	// Source is the URL the prices were checked against.
	Source string `yaml:"source"`
	// Checked is the date the prices were last checked, as YYYY-MM-DD.
	Checked string `yaml:"checked"`
	// Changes are announced price changes, in strictly increasing from
	// order. Each replaces the prices it gives, from its from time on; a
	// price it omits keeps its value from before the change. The
	// catalog resolves the prices in force whenever the gateway builds
	// its snapshot, and the gateway rebuilds it at each change, so costs,
	// budgets and routing switch on time without a reload. `switchyard
	// catalog lint` notes the next change and changes already in force,
	// which belong in the prices above.
	Changes []PriceChange `yaml:"changes"`
}

// PriceChange is a dated change of a deployment's prices, in USD per
// million tokens. It must set at least one price.
type PriceChange struct {
	// From is when the change takes effect: a date as YYYY-MM-DD, which
	// means 00:00 UTC, or an RFC 3339 timestamp with its offset, such as
	// 2026-12-01T00:00:00-08:00, for a provider that names a time zone.
	// Required.
	From string `yaml:"from"`
	// Input, when set, is the input price from From on; unset keeps the
	// one in force before the change. Output, CachedInput and CacheWrite
	// work the same way.
	Input Price `yaml:"input"`
	// Output, when set, is the output price from From on.
	Output Price `yaml:"output"`
	// CachedInput, when set, is the cache-read price from From on.
	CachedInput Price `yaml:"cached_input"`
	// CacheWrite, when set, is the cache-write price from From on.
	CacheWrite Price `yaml:"cache_write"`
}

// Price is a non-negative decimal amount of USD per million tokens, written
// as a YAML number or string, for example 0.15 or "0.0375". It keeps the
// exact decimal text so that cost accounting can avoid float rounding. The zero value means "not set".
type Price struct {
	text string
}

// LatencyHint is an operator's estimate of a deployment's latency.
type LatencyHint struct {
	// TTFTMs is the typical time to first token, in milliseconds.
	TTFTMs int `yaml:"ttft_ms"`
	// TokensPerSec is the typical output speed.
	TokensPerSec float64 `yaml:"tokens_per_sec"`
}

// Routing holds gateway-wide routing settings: profiles, fallback and
// retries, breakers, the assessor and the router's estimates.
type Routing struct {
	// DropUnsupportedParams lets a request reach a deployment that cannot
	// honor some optional parameters (n, logprobs, seed, penalties,
	// temperature above 1, parallel tool calls) by removing them, instead
	// of excluding the deployment. Default false. temperature and top_p are
	// removed for a deployment without capability sampling whatever this
	// says.
	DropUnsupportedParams bool `yaml:"drop_unsupported_params"`
	// RequestTimeout is the whole-request deadline, across every attempt,
	// backoff and the full streamed response. A client's
	// X-Switchyard-Timeout-Ms header can only lower it. Default 10m.
	RequestTimeout Duration `yaml:"request_timeout"`
	// Fallback bounds retries and fallback plans (DESIGN §14.2, §14.3).
	Fallback Fallback `yaml:"fallback"`
	// Breaker configures the per-deployment circuit breakers (§14.4).
	Breaker Breaker `yaml:"breaker"`
	// RateLimitCooldown is how long a deployment is skipped after a 429.
	RateLimitCooldown RateLimitCooldown `yaml:"rate_limit_cooldown"`
	// StreamKeepalive, when positive, commits a streaming response that
	// has not received its first upstream event after this long: the
	// gateway sends 200, the SSE headers and a ": keep-alive" comment,
	// repeated at this interval until the first event. After the first
	// keep-alive no fallback is possible, and a failure becomes an error
	// event in the stream. Default 0s (off).
	StreamKeepalive Duration `yaml:"stream_keepalive"`
	// DefaultProfile is the profile used when a policy names none and for
	// plain "auto". Default "balanced".
	DefaultProfile string `yaml:"default_profile"`
	// Profiles are routing objectives plus constraints, selected with
	// model "auto:<name>" (DESIGN §9.3). When none are configured the
	// built-in cheap, balanced, quality and fast profiles apply.
	Profiles map[string]Profile `yaml:"profiles"`
	// ExpectedOutputTokens is the output estimate when a request sets no
	// max_tokens. Default 500.
	ExpectedOutputTokens int64 `yaml:"expected_output_tokens"`
	// Assessor configures request difficulty assessment (DESIGN §10.3).
	Assessor AssessorConfig `yaml:"assessor"`
}

// Fallback bounds the attempts the gateway makes for one request.
type Fallback struct {
	// MaxDeployments is the plan length: the primary deployment plus its
	// fallbacks, 1 to 10. Default 3.
	MaxDeployments int `yaml:"max_deployments"`
	// RetriesPerDeployment is how many times a deployment is retried after
	// a retryable failure before the next one is tried, 0 to 5. Default 1.
	RetriesPerDeployment int `yaml:"retries_per_deployment"`
	// BackoffBase and BackoffMax shape the exponential backoff with full
	// jitter before retry n: a random sleep in [0, min(max, base×2^n)].
	// Defaults 250ms and 2s.
	BackoffBase Duration `yaml:"backoff_base"`
	BackoffMax  Duration `yaml:"backoff_max"`
	// RetryBudgetRatio caps the retries to one deployment at this fraction
	// of its requests over a sliding 10 s window, in (0, 1]. Default 0.2.
	RetryBudgetRatio float64 `yaml:"retry_budget_ratio"`
	// RetryBudgetMin is the number of retries per window that are always
	// allowed, whatever the ratio says. Default 5.
	RetryBudgetMin int `yaml:"retry_budget_min"`
}

// Breaker configures a deployment's circuit breaker. It opens after
// ConsecutiveFailures failures in a row, or when at least MinRequests
// requests within Window failed at a ratio of at least FailureRatio. It
// stays open for OpenFor, then lets HalfOpenProbes concurrent probes
// through: one success closes it, one failure opens it again.
type Breaker struct {
	// ConsecutiveFailures, default 5.
	ConsecutiveFailures int `yaml:"consecutive_failures"`
	// FailureRatio, in (0, 1]. Default 0.5.
	FailureRatio float64 `yaml:"failure_ratio"`
	// MinRequests, default 20.
	MinRequests int `yaml:"min_requests"`
	// Window, default 30s.
	Window Duration `yaml:"window"`
	// OpenFor, default 30s.
	OpenFor Duration `yaml:"open_for"`
	// HalfOpenProbes, default 3.
	HalfOpenProbes int `yaml:"half_open_probes"`
}

// RateLimitCooldown is how long a deployment is skipped after a 429:
// min(Retry-After, or Default without one, Max).
type RateLimitCooldown struct {
	// Default applies to a 429 without Retry-After. Default 1s.
	Default Duration `yaml:"default"`
	// Max caps the cooldown. Default 30s.
	Max Duration `yaml:"max"`
}

// Secret holds a sensitive value such as an API key, a DSN or a URL that may
// embed a password. It never reveals its contents when printed, logged or
// serialized; only Value returns the plaintext. The zero value is an empty
// secret.
type Secret struct {
	// v points at the plaintext, or is nil for an empty secret. The
	// indirection matters: when fmt prints a Secret by reflection, without
	// calling its methods (bad verbs such as %p, or a Secret in an
	// unexported field), it reaches only this address, never the text.
	v *string
}

// Duration is a time.Duration written in config as a Go duration string,
// such as "60s" or "2m30s".
type Duration time.Duration

// ByteSize is a size in bytes, written in config either as an integer or as a
// number with a unit: B, KB, MB, GB (powers of 1000) or KiB, MiB, GiB (powers
// of 1024), for example "10MiB".
type ByteSize int64

// Tenant is a customer, team or application that owns gateway API keys.
// Usage, budgets and caches are scoped by tenant (later phases).
type Tenant struct {
	// ID names the tenant in logs and reports. Same rules as Provider.ID.
	ID string `yaml:"id"`
	// Keys are the gateway API keys that authenticate as this tenant.
	Keys []APIKey `yaml:"keys"`
	// Budgets cap this tenant's spend (DESIGN §12.3). A budget with scope
	// key applies to each of the tenant's keys separately.
	Budgets []Budget `yaml:"budgets"`
	// Policy names the tenant's policy. Empty means the policy with ID
	// "default" if one exists, otherwise built-in defaults.
	Policy string `yaml:"policy"`
	// RateLimit applies to each of the tenant's keys separately, unless the
	// key sets its own. The zero value means no limit.
	RateLimit RateLimit `yaml:"rate_limit"`
}

// RateLimit limits one API key (DESIGN §7 step 3, §17). The counters live
// in the state Redis with storage.mode redis, so every replica shares
// them, and in memory otherwise.
type RateLimit struct {
	// RequestsPerMinute is the sustained request rate, enforced with GCRA.
	// 0 means unlimited.
	RequestsPerMinute int `yaml:"requests_per_minute"`
	// Burst is how many requests may arrive at once on top of the
	// sustained rate. Default: RequestsPerMinute / 6 (ten seconds' worth),
	// at least 1.
	Burst int `yaml:"burst"`
	// MaxConcurrentStreams caps the key's in-flight streaming requests.
	// 0 means unlimited.
	MaxConcurrentStreams int `yaml:"max_concurrent_streams"`
}

// RateLimits holds gateway-wide rate-limit settings.
type RateLimits struct {
	// FailMode decides what happens when the rate-limit store is
	// unavailable: open (serve, and alert; default) or closed (reject with
	// 503).
	FailMode string `yaml:"fail_mode"`
	// AuthFailuresPerMinute caps failed authentications per client IP;
	// once exceeded, that IP gets 429 whatever key it sends. Default
	// 30; -1 disables the limit.
	AuthFailuresPerMinute int `yaml:"auth_failures_per_minute"`
}

// Budget is a spend limit over a calendar window in UTC.
type Budget struct {
	// Scope is tenant (the tenant's total spend) or key (each key's spend).
	Scope string `yaml:"scope"`
	// Window is day or month, as calendar periods in UTC.
	Window string `yaml:"window"`
	// LimitUSD is the cap, in USD, as an exact decimal (for example "2000").
	LimitUSD Price `yaml:"limit_usd"`
	// SoftAt, when set, is a fraction in (0, 1) of the limit. Crossing it is
	// logged and counted once per window, and with SoftProfile it
	// downgrades "auto" requests.
	SoftAt float64 `yaml:"soft_at"`
	// OnExhausted is block (requests that would exceed the limit get 429
	// budget_exceeded) or notify (they are served, logged and counted).
	// Default block.
	OnExhausted string `yaml:"on_exhausted"`
	// SoftProfile, when set with SoftAt, is the profile "auto" requests use
	// once the soft threshold has been crossed in the current window.
	SoftProfile string `yaml:"soft_profile"`
}

// Budgets holds gateway-wide budget settings.
type Budgets struct {
	// FailMode decides what happens when the budget store is unavailable:
	// open (serve, still enforcing per-request caps, and alert) or closed
	// (reject with 503). Default open.
	FailMode string `yaml:"fail_mode"`
}

// APIKey is a gateway API key, stored only as a hash. Generate one with
// `switchyard keys generate`; the plaintext is shown once and never stored.
type APIKey struct {
	// ID names the key in logs and reports (never the key itself). It must be
	// unique across all tenants. Same rules as Provider.ID.
	ID string `yaml:"id"`
	// Hash is "sha256:" followed by 64 lowercase hex characters: the SHA-256
	// of the full plaintext key. Hashes must be unique across all tenants.
	Hash string `yaml:"hash"`
	// Tags are matched by policy rules (key_tags), e.g. prod, eu.
	Tags []string `yaml:"tags"`
	// Policy, when set, overrides the tenant's policy for this key.
	Policy string `yaml:"policy"`
	// RateLimit, when set, replaces the tenant's rate_limit for this key.
	RateLimit *RateLimit `yaml:"rate_limit"`
}

// Profile is a routing objective plus constraints (DESIGN §9.3).
type Profile struct {
	// MinQuality is the quality bar in [0, 1] (DESIGN §10.4).
	MinQuality float64 `yaml:"min_quality"`
	// Objective is cost (cheapest eligible) or latency (fastest eligible
	// by estimated time to first token). Default cost.
	Objective string `yaml:"objective"`
	// MaxTTFTMs, when > 0, is a soft latency target in milliseconds.
	MaxTTFTMs int64 `yaml:"max_ttft_ms"`
}

// AssessorConfig configures the request assessor.
type AssessorConfig struct {
	// Type is heuristic, the only kind so far. Default heuristic.
	Type string `yaml:"type"`
	// Weights overrides the heuristic's signal weights by signal name; see
	// internal/quality for the names and defaults.
	Weights map[string]float64 `yaml:"weights"`
}

// Policy is governance attached to tenants and keys (DESIGN §13).
type Policy struct {
	// ID names the policy. Same rules as Provider.ID.
	ID string `yaml:"id"`
	// Profile is the default profile for "auto" requests. Empty means
	// routing.default_profile.
	Profile string `yaml:"profile"`
	// AllowedProfiles lists the profiles callers may select by model name or
	// header. Empty means all.
	AllowedProfiles []string `yaml:"allowed_profiles"`
	// Allow and Deny are glob patterns over deployment IDs (model@provider).
	// Empty Allow means allow all; Deny wins over Allow.
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
	// Prefer lists ordered globs; earlier patterns form higher preference
	// tiers, and cost decides within a tier.
	Prefer []string `yaml:"prefer"`
	// RequireTags: a deployment must carry all of these tags (provider tags
	// plus deployment tags).
	RequireTags []string `yaml:"require_tags"`
	// Substitution says whether a concrete model name may be served by a
	// different model.
	Substitution Substitution `yaml:"substitution"`
	// RoutingMode is enforce or observe. Default enforce.
	RoutingMode string `yaml:"routing_mode"`
	// BaselineModel is the savings baseline for "auto" requests and the model
	// served for them in observe mode. Required when auto is allowed.
	BaselineModel string `yaml:"baseline_model"`
	// MaxCostPerRequestUSD is a hard cap on a request's estimated cost.
	MaxCostPerRequestUSD Price `yaml:"max_cost_per_request_usd"`
	// OnUnmet is best_effort or reject: what happens when no candidate meets
	// the soft constraints (quality, latency). Default best_effort.
	OnUnmet string `yaml:"on_unmet"`
	// Rules are evaluated in order; the first whose match holds applies its
	// set.
	Rules []Rule `yaml:"rules"`
	// Shadow configures shadow sampling for this policy's traffic.
	// It is off unless SampleRate > 0.
	Shadow ShadowPolicy `yaml:"shadow"`
	// Cache configures the response caches for this policy's chat
	// requests (DESIGN §11). Both caches are off unless enabled.
	Cache CachePolicy `yaml:"cache"`
}

// CachePolicy configures the exact and semantic response caches for a
// policy's traffic.
type CachePolicy struct {
	Exact    ExactCachePolicy    `yaml:"exact"`
	Semantic SemanticCachePolicy `yaml:"semantic"`
}

// Exact-cache scopes: which callers share entries. Every scope is within
// one tenant; tenants never share entries.
const (
	CacheScopeTenant = "tenant"
	CacheScopeKey    = "key"
	CacheScopeUser   = "user"
)

// ExactCachePolicy configures the exact cache (DESIGN §11.2).
type ExactCachePolicy struct {
	// Enabled turns the exact cache on. Default false.
	Enabled bool `yaml:"enabled"`
	// TTL is how long an entry is served. Default 24h.
	TTL Duration `yaml:"ttl"`
	// Scope is tenant (default), key or user: who shares entries. user
	// scopes by the request's user field, within the tenant; a request
	// without one is scoped by its key.
	Scope string `yaml:"scope"`
	// DeterministicOnly restricts caching to requests with temperature 0
	// or a seed, so that sampled requests always get a fresh answer.
	DeterministicOnly bool `yaml:"deterministic_only"`
}

// SemanticCachePolicy configures the semantic cache (DESIGN §11.3). It
// shares the exact cache's scope.
type SemanticCachePolicy struct {
	// Enabled turns the semantic cache on. Default false. It requires
	// semantic_cache.embedding_model.
	Enabled bool `yaml:"enabled"`
	// MinSimilarity is the cosine similarity a match needs, in (0, 1].
	// Default 0.95.
	MinSimilarity float64 `yaml:"min_similarity"`
	// TTL is how long an entry is served. Default 72h.
	TTL Duration `yaml:"ttl"`
	// MaxChars is the longest last user message eligible, in characters.
	// Default 2000.
	MaxChars int `yaml:"max_chars"`
}

// CacheConfig holds gateway-wide response-cache limits.
type CacheConfig struct {
	// LookupTimeout bounds an exact-cache lookup (namespace version and
	// entry); a lookup that takes longer is a miss. Default 25ms.
	LookupTimeout Duration `yaml:"lookup_timeout"`
	// MaxEntryBytes caps a cached response body; larger responses are not
	// cached. Default 1MiB.
	MaxEntryBytes int `yaml:"max_entry_bytes"`
	// MemoryMaxEntries caps each in-memory cache (storage.mode memory),
	// which evicts the least recently used entry. Default 10000.
	MemoryMaxEntries int `yaml:"memory_max_entries"`
}

// SemanticCacheConfig configures the semantic cache's embeddings.
type SemanticCacheConfig struct {
	// EmbeddingModel is the catalog embedding model, or model@provider,
	// that embeds last user messages. Required when any policy enables the
	// semantic cache. The message is sent to that deployment.
	EmbeddingModel string `yaml:"embedding_model"`
	// LookupTimeout bounds the embedding and the vector search together; a
	// lookup that takes longer is a miss. Default 150ms.
	LookupTimeout Duration `yaml:"lookup_timeout"`
}

// ShadowPolicy opts a policy's traffic into shadow sampling: a fraction of
// routed requests is also answered by the comparison model and judged, so
// quality can be measured on real traffic (DESIGN §10.7). Shadow calls send
// request content to the comparison and judge deployments, which must pass
// this policy's allow, deny and require_tags rules.
type ShadowPolicy struct {
	// SampleRate is the fraction of eligible requests sampled, in [0, 1].
	SampleRate float64 `yaml:"sample_rate"`
}

// Substitution configures cross-model substitution for concrete model names.
type Substitution struct {
	// Mode is pinned (only the requested model) or equivalent. Default
	// pinned.
	Mode string `yaml:"mode"`
	// MaxQualityDrop is how far below the requested model's estimate a
	// substitute may be, in [0, 1]. Default 0.05.
	MaxQualityDrop float64 `yaml:"max_quality_drop"`
}

// Rule is one policy rule.
type Rule struct {
	Name  string         `yaml:"name"`
	Match RuleMatch      `yaml:"match"`
	Set   PolicyOverride `yaml:"set"`
}

// RuleMatch holds when every condition that is set holds. An empty match is
// unconditional.
type RuleMatch struct {
	// Model is a glob over the requested model name.
	Model string `yaml:"model"`
	// Profile matches the requested profile.
	Profile string `yaml:"profile"`
	// Task matches the assessed (or hinted) category.
	Task string `yaml:"task"`
	// DifficultyGte and DifficultyLt bound the assessed difficulty; 0 means
	// unset.
	DifficultyGte float64 `yaml:"difficulty_gte"`
	DifficultyLt  float64 `yaml:"difficulty_lt"`
	// Key matches the API key ID; KeyTags requires all of these key tags.
	Key     string   `yaml:"key"`
	KeyTags []string `yaml:"key_tags"`
	// Header maps a header name to a glob its value must match.
	Header map[string]string `yaml:"header"`
	// Tags maps an X-Switchyard-Tags key to a glob its value must match.
	Tags map[string]string `yaml:"tags"`
	// Stream, HasTools and HasImages are "true" or "false"; empty means
	// either.
	Stream    string `yaml:"stream"`
	HasTools  string `yaml:"has_tools"`
	HasImages string `yaml:"has_images"`
	// EstInputTokensGte matches requests at least this large; 0 means unset.
	EstInputTokensGte int64 `yaml:"est_input_tokens_gte"`
}

// PolicyOverride is what a rule sets. Zero values and nil slices mean "keep
// the policy's value".
type PolicyOverride struct {
	Profile              string       `yaml:"profile"`
	MinQuality           float64      `yaml:"min_quality"`
	Prefer               []string     `yaml:"prefer"`
	Allow                []string     `yaml:"allow"`
	Deny                 []string     `yaml:"deny"`
	RequireTags          []string     `yaml:"require_tags"`
	Substitution         Substitution `yaml:"substitution"`
	RoutingMode          string       `yaml:"routing_mode"`
	MaxCostPerRequestUSD Price        `yaml:"max_cost_per_request_usd"`
	OnUnmet              string       `yaml:"on_unmet"`
	// Cache, when set, replaces the policy's whole cache block (with the
	// same defaults). Only governance rules may set it.
	Cache *CachePolicy `yaml:"cache"`
}

// QualityOverride is a measured quality prior for one model and category.
type QualityOverride struct {
	// Model is a catalog model ID.
	Model string `yaml:"model"`
	// Category is a quality category (general, coding, reasoning,
	// extraction, creative, tool_use).
	Category string `yaml:"category"`
	// Value replaces the catalog prior, in [0, 1].
	Value float64 `yaml:"value"`
	// CILow and CIHigh bound Value's confidence interval, in [0, 1].
	CILow  float64 `yaml:"ci_low"`
	CIHigh float64 `yaml:"ci_high"`
	// Samples is the number of judged comparisons behind Value.
	Samples int `yaml:"samples"`
	// Evaluated is the date of the evaluation, as YYYY-MM-DD.
	Evaluated string `yaml:"evaluated"`
	// Source describes the evaluation, e.g. the prompt set and judge model.
	Source string `yaml:"source"`
}

// Evaluation configures the LLM judge and shadow sampling.
type Evaluation struct {
	// Judge is the deployment that compares answers.
	Judge JudgeConfig `yaml:"judge"`
	// Shadow holds gateway-wide shadow-sampling limits.
	Shadow ShadowConfig `yaml:"shadow"`
}

// JudgeConfig names the judge model.
type JudgeConfig struct {
	// Model is a catalog chat model ID or model@provider. Required when any
	// policy enables shadow sampling.
	Model string `yaml:"model"`
}

// ShadowConfig bounds shadow sampling.
type ShadowConfig struct {
	// DailyBudgetUSD caps the combined cost of shadow and judge calls per
	// tenant per UTC day. Required when any policy enables shadow sampling.
	DailyBudgetUSD Price `yaml:"daily_budget_usd"`
	// Concurrency caps in-flight shadow evaluations per replica. Default 4.
	Concurrency int `yaml:"concurrency"`
	// MaxOutputTokens caps each shadow and judge completion. Default 1024.
	MaxOutputTokens int64 `yaml:"max_output_tokens"`
	// VerifiedMinSamples is how many judged samples a category needs before
	// its savings count as verified. Default 30.
	VerifiedMinSamples int `yaml:"verified_min_samples"`
	// VerifiedMinPassRate is the lower confidence bound the pass rate must
	// reach for verified savings, in (0, 1]. Default 0.9.
	VerifiedMinPassRate float64 `yaml:"verified_min_pass_rate"`
}
