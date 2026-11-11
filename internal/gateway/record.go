package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/providers"
	"switchyard/internal/reqctx"
	"switchyard/internal/schema"
)

// record collects what one request did, for its metrics and its
// access-log annotations.
type record struct {
	g        *Gateway
	ctx      context.Context
	start    time.Time
	endpoint string

	requestedModel string
	stream         bool
	alias          string
	deployment     *catalog.Deployment // the chosen deployment, once chosen
	dropped        []string
	excluded       int
	errorCode      string
	errorMessage   string

	upstreamStart    time.Time
	upstreamWait     time.Duration // time spent blocked on the upstream
	upstreamDuration time.Duration
	clientWait       time.Duration // time spent writing to the client
	outcome          string        // upstream outcome; "" if no upstream call
	// served is set once the upstream accepted the request: a 2xx answer,
	// or a stream whose first event arrived. Tokens were spent from then
	// on, so a missing usage is unknown, not zero.
	served            bool
	ttft              time.Duration
	usage             *schema.Usage
	upstreamRequestID string
}

func (g *Gateway) newRecord(ctx context.Context, endpoint string) *record {
	return &record{g: g, ctx: ctx, start: time.Now(), endpoint: endpoint}
}

// respondError sends e and records it.
func (r *record) respondError(ex Exchange, info Info, e *Error) {
	if r.errorCode == "" {
		r.errorCode = e.Code
		if e.Code == "" && e.Body != nil {
			r.errorCode = codeUpstreamError
		}
	}
	start := time.Now()
	ex.RespondError(info, e)
	r.wroteClient(start)
}

// wroteClient records a write to the client that began at start. That
// time is the client's, not the gateway's overhead.
func (r *record) wroteClient(start time.Time) {
	r.clientWait += time.Since(start)
}

// upstreamEnd records the end of an upstream call that the request spent
// waiting on from upstreamStart to now, with result err.
func (r *record) upstreamEnd(err error) {
	r.upstreamDuration = time.Since(r.upstreamStart)
	r.upstreamWait = r.upstreamDuration
	r.outcome = providers.Outcome(err)
}

// upstreamFailed records the details of an adapter error.
func (r *record) upstreamFailed(err error) {
	var pe *providers.Error
	if errors.As(err, &pe) {
		r.upstreamRequestID = pe.UpstreamRequestID
		r.errorMessage = pe.Message
		if pe.Kind == providers.KindCanceled {
			r.errorCode = codeCanceled
		}
	}
}

// finish records the metrics and annotates the access-log line with the
// completion fields. The status is the access line's own. Overhead is the
// request's time minus the time spent waiting on the upstream and writing
// to the client. Instrument records the request count and duration.
//
// alias and dropped_params (the removed parameters, comma-separated) are
// logged only when set; excluded counts the deployments that could not
// serve the request.
//
// prompt_tokens and completion_tokens are the usage the upstream reported.
// They are 0 when the upstream never accepted the request (it was rejected
// before or by the upstream), and omitted when the upstream served it but
// reported no usage, for example a stream from a deployment without
// stream_usage: the count is then unknown, not zero.
func (r *record) finish() {
	latency := time.Since(r.start)
	overhead := max(latency-r.upstreamWait-r.clientWait, 0)
	m := r.g.metrics
	m.overhead.WithLabelValues(r.endpoint).Observe(overhead.Seconds())

	deployment := ""
	if d := r.deployment; d != nil {
		deployment = d.ID
		if r.outcome != "" {
			m.upstreamRequests.WithLabelValues(d.Provider, d.Model.ID, r.outcome).Inc()
			m.upstreamDuration.WithLabelValues(d.Provider, d.Model.ID).Observe(r.upstreamDuration.Seconds())
			if r.ttft > 0 {
				m.upstreamTTFT.WithLabelValues(d.Provider, d.Model.ID).Observe(r.ttft.Seconds())
			}
		}
	}

	attrs := []slog.Attr{
		slog.String("requested_model", r.requestedModel),
		slog.String("deployment", deployment),
		slog.Int("excluded", r.excluded),
		slog.Bool("stream", r.stream),
		slog.Float64("ttft_ms", ms(r.ttft)),
		slog.Float64("latency_ms", ms(latency)),
		slog.Float64("overhead_ms", ms(overhead)),
	}
	// alias and dropped_params appear only when set: every attr costs
	// the redacting handler and the shared log writer on the hot path.
	if r.alias != "" {
		attrs = append(attrs, slog.String("alias", r.alias))
	}
	if len(r.dropped) > 0 {
		// A string, not a list: the redacting handler inspects every
		// KindAny value by reflection.
		attrs = append(attrs, slog.String("dropped_params", strings.Join(r.dropped, ",")))
	}
	switch {
	case r.usage != nil:
		attrs = append(attrs,
			slog.Int64("prompt_tokens", r.usage.PromptTokens),
			slog.Int64("completion_tokens", r.usage.CompletionTokens))
	case !r.served:
		attrs = append(attrs, slog.Int64("prompt_tokens", 0), slog.Int64("completion_tokens", 0))
	}
	attrs = append(attrs,
		slog.String("upstream_request_id", r.upstreamRequestID),
		slog.String("error_code", r.errorCode),
	)
	if r.errorMessage != "" {
		attrs = append(attrs, slog.String("error", r.errorMessage))
	}
	reqctx.Annotate(r.ctx, attrs...)
}

// ms returns d in fractional milliseconds.
func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
