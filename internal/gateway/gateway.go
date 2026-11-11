// Package gateway runs the request pipeline for each endpoint. It is the
// only package that knows the order of the steps: decode and validate the
// request, resolve the model through the catalog and plan the deployments
// that can honor the request (the cheapest first, skipping those that are
// unhealthy), then execute the plan: call each deployment's adapter, with
// the parameters it may drop removed, retrying and falling back as the
// error taxonomy allows, within the request's deadline. Then respond,
// record metrics and annotate the request's access-log line with the
// completion fields.
//
// The pipeline is transport-neutral: it talks to the HTTP layer
// (api/openaiapi) through the Exchange interface, which reads the request
// and writes responses and stream events. Instrument, the one piece of
// HTTP middleware here, counts every API request, including those rejected
// before they reach the pipeline.
package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/health"
	"switchyard/internal/providers"
	"switchyard/internal/providers/anthropic"
	"switchyard/internal/providers/openai"
)

// Endpoint labels, used in metrics and logs.
const (
	EndpointChatCompletions = "/v1/chat/completions"
	EndpointCompletions     = "/v1/completions"
	EndpointEmbeddings      = "/v1/embeddings"
	EndpointModels          = "/v1/models"
)

// HeaderTimeoutMs is the request header with which a client lowers its
// request's deadline, in milliseconds. It can never raise it above
// routing.request_timeout.
const HeaderTimeoutMs = "X-Switchyard-Timeout-Ms"

// Options configures a Gateway.
type Options struct {
	// Config is the validated configuration. The gateway only reads it.
	Config *config.Config
	// Logger receives the gateway's own logs. Request logs go to the
	// request-scoped logger from reqctx instead.
	Logger *slog.Logger
	// Registry receives the API and upstream metrics.
	Registry *prometheus.Registry
	// Clock is the health tracker's clock, for breakers, cooldowns and
	// retry budgets; nil means time.Now.
	Clock func() time.Time
}

// Gateway serves the OpenAI-compatible endpoints. It is safe for concurrent
// use.
type Gateway struct {
	catalog *catalog.Catalog
	// adapters holds one adapter per provider, by provider id.
	adapters map[string]providers.Adapter
	// drop is routing.drop_unsupported_params.
	drop    bool
	created int64
	metrics *metrics
	// routing holds the routing settings, with defaults for zero values.
	routing config.Routing
	// health is the replica's breaker, cooldown, budget and concurrency
	// state; deps and provs are its handles, by deployment and provider
	// id.
	health *health.Tracker
	deps   map[string]*health.Deployment
	provs  map[string]*health.Provider
	// jitter returns a random duration in [0, d], for the backoff.
	jitter func(d time.Duration) time.Duration
}

// Model is one entry of the model list.
type Model struct {
	// ID is the catalog id or alias clients send in the model field.
	ID string
	// Created is the Unix time the configuration was loaded.
	Created int64
}

// New builds the gateway: the catalog, and one adapter, with its own HTTP
// client, per provider. It registers the metrics on opts.Registry.
func New(opts Options) (*Gateway, error) {
	switch {
	case opts.Config == nil:
		return nil, errors.New("gateway: missing Config")
	case opts.Logger == nil:
		return nil, errors.New("gateway: missing Logger")
	case opts.Registry == nil:
		return nil, errors.New("gateway: missing Registry")
	}
	cfg := opts.Config

	cat, err := catalog.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("gateway: %w", err)
	}
	g := &Gateway{
		catalog:  cat,
		adapters: make(map[string]providers.Adapter, len(cfg.Providers)),
		drop:     cfg.Routing.DropUnsupportedParams,
		created:  time.Now().Unix(),
	}
	for _, p := range cfg.Providers {
		var (
			a   providers.Adapter
			err error
		)
		switch p.Type {
		case "openai":
			a, err = openai.New(p)
		case "anthropic":
			a, err = anthropic.New(p)
		default:
			// Validation rejects other types; their deployments are
			// excluded from every request.
			opts.Logger.Warn("provider type not supported; its deployments are never chosen",
				"provider", p.ID, "type", p.Type)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("gateway: %w", err)
		}
		g.adapters[p.ID] = a
	}

	g.routing = withRoutingDefaults(cfg.Routing)
	g.jitter = fullJitter
	met, err := newMetrics(opts.Registry)
	if err != nil {
		return nil, err
	}
	g.metrics = met
	g.health = health.New(health.Options{Config: trackerConfig(g.routing), Observer: met, Now: opts.Clock})
	g.deps = make(map[string]*health.Deployment)
	g.provs = make(map[string]*health.Provider, len(cfg.Providers))
	for _, p := range cfg.Providers {
		g.provs[p.ID] = g.health.Provider(p.ID, p.MaxConcurrency)
	}
	for _, m := range cat.Models() {
		for _, d := range m.Deployments {
			g.deps[d.ID] = g.health.Deployment(d.ID, d.Provider,
				health.LatencyHint{TTFT: d.LatencyHint.TTFT, TokensPerSec: d.LatencyHint.TokensPerSec})
		}
	}
	if err := met.registerHealth(opts.Registry, g.health); err != nil {
		return nil, err
	}
	return g, nil
}

// withRoutingDefaults fills the routing fields whose zero value is
// invalid with their defaults, for configurations built by hand rather
// than loaded (which have them already). Zero retries and a zero retry
// budget minimum are valid and kept.
func withRoutingDefaults(r config.Routing) config.Routing {
	def := config.DefaultRouting()
	set := func(p *config.Duration, d config.Duration) {
		if *p <= 0 {
			*p = d
		}
	}
	setInt := func(p *int, d int) {
		if *p <= 0 {
			*p = d
		}
	}
	setFloat := func(p *float64, d float64) {
		if *p <= 0 {
			*p = d
		}
	}
	set(&r.RequestTimeout, def.RequestTimeout)
	setInt(&r.Fallback.MaxDeployments, def.Fallback.MaxDeployments)
	set(&r.Fallback.BackoffBase, def.Fallback.BackoffBase)
	set(&r.Fallback.BackoffMax, def.Fallback.BackoffMax)
	setFloat(&r.Fallback.RetryBudgetRatio, def.Fallback.RetryBudgetRatio)
	setInt(&r.Breaker.ConsecutiveFailures, def.Breaker.ConsecutiveFailures)
	setFloat(&r.Breaker.FailureRatio, def.Breaker.FailureRatio)
	setInt(&r.Breaker.MinRequests, def.Breaker.MinRequests)
	set(&r.Breaker.Window, def.Breaker.Window)
	set(&r.Breaker.OpenFor, def.Breaker.OpenFor)
	setInt(&r.Breaker.HalfOpenProbes, def.Breaker.HalfOpenProbes)
	set(&r.RateLimitCooldown.Default, def.RateLimitCooldown.Default)
	set(&r.RateLimitCooldown.Max, def.RateLimitCooldown.Max)
	r.Fallback.MaxDeployments = min(r.Fallback.MaxDeployments, maxPlan)
	r.Fallback.RetriesPerDeployment = max(r.Fallback.RetriesPerDeployment, 0)
	r.Fallback.RetryBudgetMin = max(r.Fallback.RetryBudgetMin, 0)
	return r
}

// trackerConfig is the health tracker's view of the routing settings.
func trackerConfig(r config.Routing) health.Config {
	return health.Config{
		ConsecutiveFailures: r.Breaker.ConsecutiveFailures,
		FailureRatio:        r.Breaker.FailureRatio,
		MinRequests:         r.Breaker.MinRequests,
		Window:              r.Breaker.Window.Std(),
		OpenFor:             r.Breaker.OpenFor.Std(),
		HalfOpenProbes:      r.Breaker.HalfOpenProbes,
		CooldownDefault:     r.RateLimitCooldown.Default.Std(),
		CooldownMax:         r.RateLimitCooldown.Max.Std(),
		BudgetRatio:         r.Fallback.RetryBudgetRatio,
		BudgetMin:           r.Fallback.RetryBudgetMin,
	}
}

// fullJitter returns a uniformly random duration in [0, d].
func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return rand.N(d + 1) //nolint:gosec // G404: backoff jitter needs no cryptographic randomness
}

// Info describes who served a request, for the X-Switchyard-* response
// headers. Fields are empty when not known yet.
type Info struct {
	// Model is the catalog model id.
	Model string
	// Provider is the provider id.
	Provider string
	// Alias is the alias the client named, or "".
	Alias string
	// DroppedParams lists the request parameters removed because the
	// chosen deployment cannot honor them.
	DroppedParams []string
	// Shim names the translation that served the request, such as "chat"
	// for a legacy completion served through Chat Completions, or "".
	Shim string
	// UpstreamRequestID is the upstream's x-request-id, from the attempt
	// that answered.
	UpstreamRequestID string
	// Attempts is the number of upstream attempts so far; 0 when none was
	// made.
	Attempts int
	// RouteReason says why the provider was chosen: cheapest_eligible,
	// pinned, or fallback:<kind> when a fallback answered, kind being the
	// failure that caused the first fallback. Empty when no deployment
	// was chosen.
	RouteReason string
}

// Error is a failed request, ready to be sent in the OpenAI error
// envelope.
type Error struct {
	// Status is the HTTP status.
	Status int
	// Type, Code, Param and Message are the envelope fields. Empty Code
	// and Param are sent as null.
	Type, Code, Param, Message string
	// RetryAfter, when positive, is sent as the Retry-After header.
	RetryAfter time.Duration
	// Body, when set, is an upstream error envelope sent verbatim instead
	// of the fields above.
	Body []byte
}

// Error returns the message.
func (e *Error) Error() string { return e.Message }

// Exchange is one HTTP request and its response, as the pipeline sees it.
// The HTTP layer implements it. Methods are called from one goroutine.
type Exchange interface {
	// Body returns the request body, or the error to answer with when it
	// cannot be read (for example 413 when it is too large).
	Body() ([]byte, *Error)
	// Header returns the client's request headers.
	Header() http.Header
	// Respond sends a complete JSON response with the given status.
	Respond(info Info, status int, body []byte)
	// RespondError sends an error response.
	RespondError(info Info, e *Error)
	// StartStream commits the response: status 200 and the SSE headers.
	StartStream(info Info) error
	// Comment writes one SSE comment line (": text") and flushes it, as a
	// keep-alive. Only valid after StartStream.
	Comment(text string) error
	// Send writes one SSE data event and flushes it.
	Send(data []byte) error
	// Done writes the final data: [DONE] event.
	Done() error
}
