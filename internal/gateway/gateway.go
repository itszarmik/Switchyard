// Package gateway runs the request pipeline for each endpoint. It is the
// only package that knows the order of the steps: decode and validate the
// request, resolve the model, call the provider adapter, respond, then
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
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/providers/openai"
)

// Endpoint labels, used in metrics and logs.
const (
	EndpointChatCompletions = "/v1/chat/completions"
	EndpointModels          = "/v1/models"
)

// Options configures a Gateway.
type Options struct {
	// Config is the validated configuration. The gateway only reads it.
	Config *config.Config
	// Logger receives the gateway's own logs. Request logs go to the
	// request-scoped logger from reqctx instead.
	Logger *slog.Logger
	// Registry receives the API and upstream metrics.
	Registry *prometheus.Registry
}

// Gateway serves the OpenAI-compatible endpoints. It is safe for concurrent
// use.
type Gateway struct {
	targets map[string]*target
	models  []Model
	metrics *metrics
}

// target is where a catalog model is served: in Phase 1, its first
// deployment.
type target struct {
	model         string // catalog id
	provider      string
	providerType  string
	upstreamModel string
	// adapter is nil when the provider's type has no adapter yet.
	adapter providers.Adapter
}

// deployment is the target's id, model@provider.
func (t *target) deployment() string { return t.model + "@" + t.provider }

// Model is one entry of the model list.
type Model struct {
	// ID is the catalog id clients send in the model field.
	ID string
	// Created is the Unix time the configuration was loaded.
	Created int64
}

// New builds the gateway: one adapter, with its own HTTP client, per
// provider, and the model lookup table. Providers of a type without an
// adapter yet (anthropic, until Phase 2) are logged and their models answer
// with an error. It registers the metrics on opts.Registry.
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

	type built struct {
		typ     string
		adapter providers.Adapter
	}
	adapters := make(map[string]built, len(cfg.Providers))
	for _, p := range cfg.Providers {
		switch p.Type {
		case "openai":
			a, err := openai.New(p)
			if err != nil {
				return nil, fmt.Errorf("gateway: %w", err)
			}
			adapters[p.ID] = built{typ: p.Type, adapter: a}
		default:
			adapters[p.ID] = built{typ: p.Type}
			opts.Logger.Warn("provider type not supported yet; its models will return errors",
				"provider", p.ID, "type", p.Type)
		}
	}

	created := time.Now().Unix()
	g := &Gateway{
		targets: make(map[string]*target, len(cfg.Models)),
		models:  make([]Model, 0, len(cfg.Models)),
	}
	for _, m := range cfg.Models {
		if len(m.Deployments) == 0 {
			return nil, fmt.Errorf("gateway: model %s has no deployments", m.ID)
		}
		d := m.Deployments[0]
		b, ok := adapters[d.Provider]
		if !ok {
			return nil, fmt.Errorf("gateway: model %s: unknown provider %s", m.ID, d.Provider)
		}
		g.targets[m.ID] = &target{
			model:         m.ID,
			provider:      d.Provider,
			providerType:  b.typ,
			upstreamModel: d.UpstreamModel,
			adapter:       b.adapter,
		}
		g.models = append(g.models, Model{ID: m.ID, Created: created})
	}

	met, err := newMetrics(opts.Registry)
	if err != nil {
		return nil, err
	}
	g.metrics = met
	return g, nil
}

// Info describes who served a request, for the X-Switchyard-* response
// headers. Fields are empty when not known yet.
type Info struct {
	// Model is the catalog model id.
	Model string
	// Provider is the provider id.
	Provider string
	// UpstreamRequestID is the upstream's x-request-id.
	UpstreamRequestID string
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
	// Send writes one SSE data event and flushes it.
	Send(data []byte) error
	// Done writes the final data: [DONE] event.
	Done() error
}
