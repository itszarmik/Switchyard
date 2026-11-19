// Package tracing wires Switchyard to OpenTelemetry (DESIGN §15.3).
//
// New builds a Provider from telemetry.tracing. When tracing is enabled,
// spans are sampled ParentBased(TraceIDRatioBased(sample_ratio)), queued in
// a batch span processor with a bounded queue, and posted as OTLP/HTTP
// protobuf to <endpoint>/v1/traces by the package's own exporter, which
// links no gRPC code. An export is a single attempt: a batch that fails is
// dropped, counted in ExportStats and logged, rate limited. When it is disabled, the
// Provider records nothing, but its Middleware still honors an incoming
// W3C traceparent, so the client's trace ID reaches the logs through IDs.
//
// # Span tree
//
// Provider.Middleware starts the root server span of every HTTP request.
// The layers that serve the request add children with Start, using the span
// names below (SpanAuth, SpanRateLimit, ...), so one trace shows auth, rate
// limit, policy, cache, route, budget, every upstream attempt and usage
// recording. Spans carry metadata only (model, provider, token counts,
// status and cost), never prompt or response content.
//
// # How Start finds its tracer
//
// There is no global tracer provider. Start creates a child of the span in
// ctx with the tracer provider that created that span, so it only records
// under a span from a Provider (Middleware, or Provider.Tracer). With no
// recording span in ctx (tracing disabled, a sampled-out trace, or code
// outside a request) Start returns ctx and its span unchanged, which costs
// nothing and keeps the trace and span IDs that IDs reports for logs. Tests
// therefore need no global state: each builds its own Provider.
//
// # Globals
//
// New installs the W3C tracecontext and baggage propagators as the
// OpenTelemetry global propagator, and, when tracing is enabled, an
// OpenTelemetry global error handler that logs through the Provider's
// logger with the endpoint scrubbed, rate limited. Those are the only
// process-wide side effects; the most recent New wins.
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"switchyard/internal/config"
	"switchyard/internal/version"
)

// scopeName is the instrumentation scope of every Switchyard span.
const scopeName = "switchyard"

// Span names of the request span tree (DESIGN §15.3). The root server span
// is named after the HTTP method and route by Provider.Middleware.
const (
	SpanAuth            = "auth"
	SpanRateLimit       = "ratelimit"
	SpanPolicy          = "policy"
	SpanCacheExact      = "cache.exact"
	SpanCacheSemantic   = "cache.semantic"
	SpanEmbed           = "embed"  // child of SpanCacheSemantic
	SpanSearch          = "search" // child of SpanCacheSemantic
	SpanRoute           = "route"
	SpanBudgetAdmit     = "budget.admit"
	SpanUpstreamAttempt = "upstream.attempt" // one per attempt; set AttrAttemptNumber
	SpanUsageRecord     = "usage.record"
)

// Switchyard span attributes. AttrKeyID is the API key's ID, never the key.
const (
	AttrTenant        = attribute.Key("switchyard.tenant")
	AttrKeyID         = attribute.Key("switchyard.key_id")
	AttrProfile       = attribute.Key("switchyard.profile")
	AttrCacheStatus   = attribute.Key("switchyard.cache_status")
	AttrRouteReason   = attribute.Key("switchyard.route_reason")
	AttrCostUSD       = attribute.Key("switchyard.cost_usd")
	AttrAttempts      = attribute.Key("switchyard.attempts")
	AttrListener      = attribute.Key("switchyard.listener")
	AttrAttemptNumber = attribute.Key("attempt.number")
	// The attributes of an upstream.attempt span besides the GenAI ones:
	// the provider and deployment IDs, the attempt's outcome (ok, or the
	// failure kind) and, on a retry, the failure kind that caused it.
	AttrProvider    = attribute.Key("switchyard.provider")
	AttrDeployment  = attribute.Key("switchyard.deployment")
	AttrOutcome     = attribute.Key("switchyard.outcome")
	AttrRetryReason = attribute.Key("switchyard.retry_reason")
)

// Span processor limits (batchProcessor): spans wait at most
// batchTimeout, and once maxQueueSize spans are queued new ones are
// dropped, and counted, rather than blocking a request. Each export is
// bounded by exportTimeout.
const (
	maxQueueSize       = 2048
	maxExportBatchSize = 512
	batchTimeout       = 5 * time.Second
)

// errorLogInterval is the minimum time between two logged export errors;
// the errors in between are counted and reported with the next line.
const errorLogInterval = time.Minute

// Provider is Switchyard's tracer provider. Create it with New; the zero
// value is not usable. It is safe for concurrent use.
type Provider struct {
	// tp is nil when tracing is disabled.
	tp     *sdktrace.TracerProvider
	tracer trace.Tracer
	prop   propagation.TextMapPropagator
	// rep scrubs and logs errors; nil when tracing is disabled.
	rep *errorReporter
	// otlp is the exporter, for its counters; nil when tracing is disabled
	// or a test replaced it.
	otlp *otlpExporter
	// bsp is the span processor, for its drop count; nil when tracing is
	// disabled or a test exports synchronously.
	bsp *batchProcessor
}

// options are the test hooks of newProvider.
type options struct {
	// exporter, when set, replaces the OTLP exporter.
	exporter sdktrace.SpanExporter
	// syncExport exports each span as it ends instead of in batches.
	syncExport bool
	// now is the clock of the error rate limiter.
	now func() time.Time
}

// New returns a Provider for cfg. When cfg.Enabled is false it records
// nothing and returns at once. Errors never include the endpoint, which is
// a secret.
func New(ctx context.Context, cfg config.Tracing, info version.Info, log *slog.Logger) (*Provider, error) {
	return newProvider(ctx, cfg, info, log, options{now: time.Now})
}

func newProvider(ctx context.Context, cfg config.Tracing, info version.Info, log *slog.Logger, o options) (*Provider, error) {
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	otel.SetTextMapPropagator(prop)
	if !cfg.Enabled {
		return &Provider{tracer: noop.NewTracerProvider().Tracer(scopeName), prop: prop}, nil
	}
	if log == nil {
		log = slog.Default()
	}
	if o.now == nil {
		o.now = time.Now
	}
	if !(cfg.SampleRatio >= 0 && cfg.SampleRatio <= 1) { // also rejects NaN
		return nil, fmt.Errorf("tracing: sample ratio must be between 0 and 1, got %g", cfg.SampleRatio)
	}

	ep, err := parseEndpoint(cfg.Endpoint.Value())
	if err != nil {
		return nil, err
	}
	rep := newErrorReporter(log, o.now, ep.redactor)
	var otlp *otlpExporter
	exp := o.exporter
	if exp == nil {
		otlp = newOTLPExporter(ep, rep.report)
		exp = otlp
	}

	// OTEL_RESOURCE_ATTRIBUTES may add attributes; the service attributes
	// set here take precedence.
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(info.Version),
			semconv.ServiceInstanceID(uuid.NewString()),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: build resource: %w", err)
	}
	var (
		processor sdktrace.TracerProviderOption
		bsp       *batchProcessor
	)
	if o.syncExport {
		processor = sdktrace.WithSyncer(exp)
	} else {
		bsp = newBatchProcessor(exp, maxQueueSize, maxExportBatchSize, batchTimeout, rep.report)
		processor = sdktrace.WithSpanProcessor(bsp)
	}
	tp := sdktrace.NewTracerProvider(
		processor,
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(rep.report))
	log.Info("tracing enabled", "service_name", cfg.ServiceName, "sample_ratio", cfg.SampleRatio,
		"propagate_upstream", cfg.PropagateUpstream)
	return &Provider{tp: tp, tracer: tp.Tracer(scopeName), prop: prop, rep: rep, otlp: otlp, bsp: bsp}, nil
}

// Tracer returns the Provider's tracer: a no-op tracer when tracing is
// disabled. Use it to start a root span outside an HTTP request; inside one,
// use Start.
func (p *Provider) Tracer() trace.Tracer { return p.tracer }

// ExportStats returns how many spans have been exported and dropped, for
// metrics. It is zero when tracing is disabled.
func (p *Provider) ExportStats() ExportStats {
	var s ExportStats
	if p.otlp != nil {
		s = p.otlp.stats()
	}
	if p.bsp != nil {
		s.Dropped += p.bsp.Dropped()
	}
	return s
}

// Shutdown flushes the queued spans and stops the exporter, waiting at most
// until ctx is done. It does nothing when tracing is disabled, and later
// calls return nil.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p.tp == nil {
		return nil
	}
	if err := p.tp.Shutdown(ctx); err != nil {
		return fmt.Errorf("tracing: shutdown: %w", p.rep.sanitize(err))
	}
	return nil
}

// Start starts a child of the span in ctx, named name, with attrs, using
// the tracer provider that created that span (see the package
// documentation). When ctx holds no recording span it returns ctx and
// ctx's span unchanged; ending that span does nothing. The caller must end
// the returned span.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	parent := trace.SpanFromContext(ctx)
	if !parent.IsRecording() {
		return ctx, parent
	}
	return parent.TracerProvider().Tracer(scopeName).Start(ctx, name, trace.WithAttributes(attrs...))
}

// IDs returns the hex trace and span IDs of the span in ctx, for log lines.
// ok is false when ctx carries no valid span context, local or remote.
func IDs(ctx context.Context) (traceID, spanID string, ok bool) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return "", "", false
	}
	return sc.TraceID().String(), sc.SpanID().String(), true
}

// GenAI attribute keys (OpenTelemetry GenAI semantic conventions). The Go
// semconv packages no longer ship them, so they are spelled out here.
// gen_ai.system is the older name of gen_ai.provider.name; both are set, so
// tools that read either understand the traces.
const (
	genAISystem        = attribute.Key("gen_ai.system")
	genAIProviderName  = attribute.Key("gen_ai.provider.name")
	genAIRequestModel  = attribute.Key("gen_ai.request.model")
	genAIResponseModel = attribute.Key("gen_ai.response.model")
	genAIInputTokens   = attribute.Key("gen_ai.usage.input_tokens")
	genAIOutputTokens  = attribute.Key("gen_ai.usage.output_tokens")
)

// GenAI holds the GenAI semantic-convention attributes of a span.
type GenAI struct {
	// System is the provider, such as openai or anthropic. It sets both
	// gen_ai.system and gen_ai.provider.name.
	System string
	// RequestModel is the model the request asked for; ResponseModel the
	// one that answered, as the provider reported it.
	RequestModel  string
	ResponseModel string
	// InputTokens and OutputTokens are the usage counts. They are set only
	// when at least one is non-zero, since zero usage means "unknown" for
	// a request that never reached a provider.
	InputTokens  int64
	OutputTokens int64
}

// SetGenAI sets a's non-empty fields on span as GenAI semantic-convention
// attributes. It does nothing when span is not recording.
func SetGenAI(span trace.Span, a GenAI) {
	if !span.IsRecording() {
		return
	}
	attrs := make([]attribute.KeyValue, 0, 6)
	if a.System != "" {
		attrs = append(attrs, genAISystem.String(a.System), genAIProviderName.String(a.System))
	}
	if a.RequestModel != "" {
		attrs = append(attrs, genAIRequestModel.String(a.RequestModel))
	}
	if a.ResponseModel != "" {
		attrs = append(attrs, genAIResponseModel.String(a.ResponseModel))
	}
	if a.InputTokens != 0 || a.OutputTokens != 0 {
		attrs = append(attrs, genAIInputTokens.Int64(a.InputTokens), genAIOutputTokens.Int64(a.OutputTokens))
	}
	span.SetAttributes(attrs...)
}
