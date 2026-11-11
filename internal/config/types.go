package config

import "time"

// This file defines the configuration schema: the shape of switchyard.yaml
// after includes are merged, ${…} references are expanded and defaults are
// applied. Field names and yaml tags are the operator-facing contract
// (docs/DESIGN.md §16); renaming one breaks existing config files.
//
// Phase 0 defines the skeleton below. Later phases add sections (tenants,
// policies, routing, cache) and extend Model with catalog data.

// Config is the root of the configuration.
type Config struct {
	// Version is the schema version. Only 1 is defined, and it is required.
	Version int `yaml:"version"`
	// Include lists glob patterns of additional files, resolved relative to the
	// directory of the root file. Included files may only define providers,
	// models and aliases; list entries are appended in sorted file-name order,
	// and an alias defined twice, in any files, is an error.
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
	// deployment (model@provider). Alias names follow Model.ID rules and
	// must not collide with a model ID. Values starting with "auto" are
	// reserved for Phase 5.
	Aliases map[string]string `yaml:"aliases"`
	Routing Routing           `yaml:"routing"`
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
	// IdleTimeout closes idle keep-alive connections. Default 120s.
	IdleTimeout Duration `yaml:"idle_timeout"`
	// ShutdownDelay is how long the server keeps serving after SIGTERM while
	// /readyz already fails, so load balancers can stop routing to it first.
	// Default 0s.
	ShutdownDelay Duration `yaml:"shutdown_delay"`
	// ShutdownGrace bounds how long in-flight requests may take to finish once
	// the listeners have closed. Default 60s.
	ShutdownGrace Duration `yaml:"shutdown_grace"`
}

// Logging configures the process logger.
type Logging struct {
	// Level is one of debug, info, warn, error. Default info.
	Level string `yaml:"level"`
	// Format is json or text. Default json.
	Format string `yaml:"format"`
}

// Storage selects the state backends. Phase 0 validates these settings but
// does not connect to anything yet.
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
}

// Provider is an upstream endpoint plus its credentials.
type Provider struct {
	// ID names the provider in deployment IDs (model@provider).
	ID string `yaml:"id"`
	// Type is the wire protocol: openai (OpenAI and every OpenAI-compatible
	// upstream) or anthropic.
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
	// FirstToken bounds the wait for the first response byte. Default 30s.
	FirstToken Duration `yaml:"first_token"`
	// Idle bounds the gap between streamed chunks. Default 60s.
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
	// reasoning, extraction, creative, tool_use (DESIGN §10.2). Used from
	// Phase 5; validated now.
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
	Input       Price `yaml:"input"`
	Output      Price `yaml:"output"`
	CachedInput Price `yaml:"cached_input"`
	CacheWrite  Price `yaml:"cache_write"`
	// Source is the URL the prices were checked against.
	Source string `yaml:"source"`
	// Checked is the date the prices were last checked, as YYYY-MM-DD.
	Checked string `yaml:"checked"`
}

// Price is a non-negative decimal amount of USD per million tokens, written
// as a YAML number or string, for example 0.15 or "0.0375". It keeps the
// exact decimal text so that cost accounting (Phase 4) can avoid float
// rounding. The zero value means "not set".
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

// Routing holds gateway-wide routing settings. Phase 5 adds profiles and
// per-policy overrides.
type Routing struct {
	// DropUnsupportedParams lets a request reach a deployment that cannot
	// honor some optional parameters (n, logprobs, seed, penalties,
	// temperature above 1, parallel tool calls) by removing them, instead
	// of excluding the deployment. Default false. temperature and top_p are
	// removed for a deployment without capability sampling whatever this
	// says.
	DropUnsupportedParams bool `yaml:"drop_unsupported_params"`
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
	// Tags are matched by policies in later phases, e.g. prod, eu.
	Tags []string `yaml:"tags"`
}
