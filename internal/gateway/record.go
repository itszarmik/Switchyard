package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/health"
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
	deployment     *catalog.Deployment // the deployment of the last attempt
	dropped        []string
	excluded       int
	errorCode      string
	errorMessage   string
	// deadline is the request's timeout, once known.
	deadline time.Duration
	// info is what the response headers say; plan is the request's plan.
	info Info
	plan plan

	upstreamWait time.Duration // time spent blocked on the upstream
	clientWait   time.Duration // time spent writing to the client
	backoff      time.Duration // time spent waiting before retries
	// served is set once the upstream accepted the request: a 2xx answer,
	// or a stream whose first event arrived. Tokens were spent from then
	// on, so a missing usage is unknown, not zero.
	served            bool
	ttft              time.Duration
	usage             *schema.Usage
	upstreamRequestID string

	// attempts counts the upstream attempts; chain lists the first
	// maxChain of them as deployment:outcome, comma-separated.
	attempts int
	chain    string
	nchain   int
	// fallbackReason is the failure kind that caused the first fallback,
	// or "".
	fallbackReason string
	// committed is set once a stream's response has started.
	committed bool
	// curHealth and curAdmission are the deployment and admission of the
	// attempt in progress; reported is set once the admission has been
	// ended (see reportHealth and commitHealth).
	curHealth    *health.Deployment
	curAdmission health.Admission
	reported     bool
	// keepaliveDue is when the next keep-alive is due, once keep-alives
	// have started.
	keepaliveDue time.Time
}

// startAttempt makes the admitted attempt adm on d the attempt in
// progress.
func (r *record) startAttempt(d *health.Deployment, adm health.Admission) {
	r.curHealth, r.curAdmission, r.reported = d, adm, false
}

// reportHealth ends the admission of the attempt in progress with its
// result, unless it has been ended already: an admission is ended exactly
// once.
func (r *record) reportHealth(res health.Result) {
	if r.reported || r.curHealth == nil {
		return
	}
	r.reported = true
	r.curHealth.Done(r.curAdmission, res)
}

// commitHealth ends the admission of the attempt in progress at its commit
// point, once the upstream has answered after ttft (see
// health.Deployment.Commit). Its outcome is recorded when it ends.
func (r *record) commitHealth(ttft time.Duration) {
	if r.reported || r.curHealth == nil {
		return
	}
	r.reported = true
	r.curHealth.Commit(r.curAdmission, ttft)
}

// routeReason is the X-Switchyard-Route-Reason of an attempt on plan p.
func (r *record) routeReason(p *plan) string {
	switch {
	case r.fallbackReason != "":
		return reasonFallback + r.fallbackReason
	case p.pinned:
		return reasonPinned
	}
	return reasonCheapest
}

// addChain appends an attempt to the chain, up to maxChain of them.
func (r *record) addChain(deployment, outcome string) {
	switch {
	case r.nchain >= maxChain:
		return
	case r.nchain == 0:
		r.chain = deployment + ":" + outcome
	default:
		r.chain += "," + deployment + ":" + outcome
	}
	r.nchain++
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

// finish records the overhead metric and annotates the access-log line
// with the completion fields. The status is the access line's own.
// Overhead is the request's time minus the time spent waiting on the
// upstream, writing to the client and waiting before retries. Instrument
// records the request count and duration; the executor records each
// upstream attempt.
//
// alias and dropped_params (the removed parameters, comma-separated) are
// logged only when set; excluded counts the deployments that could not
// serve the request. attempts counts the upstream attempts, attempt_chain
// lists them (deployment:outcome, comma-separated, at most ten) when there
// were any, and fallback_reason is the failure kind that caused the first
// fallback, when one happened. deadline_ms is the request's deadline once
// known.
//
// prompt_tokens and completion_tokens are the usage the upstream reported.
// They are 0 when the upstream never accepted the request (it was rejected
// before or by the upstream), and omitted when the upstream served it but
// reported no usage, for example a stream from a deployment without
// stream_usage: the count is then unknown, not zero.
func (r *record) finish() {
	latency := time.Since(r.start)
	overhead := max(latency-r.upstreamWait-r.clientWait-r.backoff, 0)
	r.g.metrics.overhead.WithLabelValues(r.endpoint).Observe(overhead.Seconds())

	deployment := ""
	if d := r.deployment; d != nil {
		deployment = d.ID
	}

	attrs := make([]slog.Attr, 0, 20)
	attrs = append(attrs,
		slog.String("requested_model", r.requestedModel),
		slog.String("deployment", deployment),
		slog.Int("excluded", r.excluded),
		slog.Bool("stream", r.stream),
		slog.Int("attempts", r.attempts),
		slog.Float64("ttft_ms", ms(r.ttft)),
		slog.Float64("latency_ms", ms(latency)),
		slog.Float64("overhead_ms", ms(overhead)),
	)
	if r.deadline > 0 {
		attrs = append(attrs, slog.Int64("deadline_ms", r.deadline.Milliseconds()))
	}
	// The optional fields appear only when set: every attr costs the
	// redacting handler and the shared log writer on the hot path.
	if r.nchain > 0 {
		attrs = append(attrs, slog.String("attempt_chain", r.chain))
	}
	if r.fallbackReason != "" {
		attrs = append(attrs, slog.String("fallback_reason", r.fallbackReason))
	}
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
