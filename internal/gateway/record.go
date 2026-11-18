package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"switchyard/internal/budget"
	"switchyard/internal/catalog"
	"switchyard/internal/health"
	"switchyard/internal/providers"
	"switchyard/internal/reqctx"
	"switchyard/internal/router"
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
	served bool
	// sentTo is the deployment of the last attempt that reached the
	// upstream and may have been billed without serving the request (see
	// runAttempt), or nil. A request no upstream served is charged its
	// estimate if it is set, and nothing otherwise.
	sentTo *catalog.Deployment
	ttft   time.Duration
	usage  *schema.Usage
	// usagePartial is set when usage is what a stream reported before it
	// broke off, which may not count all its output.
	usagePartial      bool
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

	// tenant and keyID identify the caller; authenticated is set when the
	// auth middleware did. tags are its attribution tags.
	tenant, keyID string
	authenticated bool
	tags          map[string]string
	// status is the HTTP status the client was sent, once it was.
	status int
	// model is the resolved catalog model, the baseline of the savings.
	model *catalog.Model
	// inBytes and inImages are the request's input for the token
	// estimates (see inputSize); estIn gives the input estimate on a
	// model. estOut is the output token estimate for budget admission:
	// max_tokens when set.
	inBytes, inImages, estOut int64
	// adm is the request's budget admission, when admitted is set.
	adm      budget.Admission
	admitted bool
	// outBytes counts the output text relayed from a stream, for the
	// estimate when the upstream reports no usage.
	outBytes int64
	// acct is what the request used and cost, once computed (see account).
	acct *accounting

	// route is the routing outcome of a chat or legacy completion request,
	// once routed. profile and routingMode are what its usage event
	// records; baseReason is its route reason before any fallback.
	route                *routed
	rt                   routed // the storage of route
	profile, routingMode string
	baseReason           string
	// dryRun is set for the explain endpoint, which neither settles a
	// budget nor records usage.
	dryRun bool
	// shadow is what a request in its policy's shadow sample collects
	// (see decideShadow).
	shadow shadowState
	// cache is what the cache layer knows of a chat request.
	cache cacheState
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

// routeReason is the X-Switchyard-Route-Reason of an attempt on plan p:
// fallback:<kind> once a fallback happened, else the routing decision's
// reason (see baseReason), else pinned or cheapest_eligible.
func (r *record) routeReason(p *plan) string {
	switch {
	case r.fallbackReason != "":
		return reasonFallback + r.fallbackReason
	case r.baseReason != "":
		return r.baseReason
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
	r := &record{g: g, ctx: ctx, start: time.Now(), endpoint: endpoint}
	r.identify()
	return r
}

// respondError sends e and records it.
func (r *record) respondError(ex Exchange, info Info, e *Error) {
	if r.errorCode == "" {
		r.errorCode = e.Code
		if e.Code == "" && e.Body != nil {
			r.errorCode = codeUpstreamError
		}
	}
	r.status = e.Status
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

// done is deferred by each endpoint's handler: it finishes the record (see
// finish) and, when the handler is panicking, first records the 500 that
// the server's recovery answers, then lets the panic go on. A deliberate
// abort (http.ErrAbortHandler) keeps the status already sent.
func (r *record) done() {
	if v := recover(); v != nil {
		if err, ok := v.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
			r.status = http.StatusInternalServerError
		}
		r.finish()
		panic(v)
	}
	r.finish()
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
//
// Before that it settles the request's budget admission and records its
// usage event and cost metrics (see settle and recordUsage); cost_usd is
// logged when the cost is known and the upstream accepted the request.
func (r *record) finish() {
	latency := time.Since(r.start)
	if !r.dryRun {
		r.settle()
		r.recordUsage(latency)
		r.offerShadow()
	}
	overhead := max(latency-r.upstreamWait-r.clientWait-r.backoff, 0)
	r.g.metrics.overhead.WithLabelValues(r.endpoint).Observe(overhead.Seconds())

	deployment := ""
	switch {
	case r.deployment != nil:
		deployment = r.deployment.ID
	case r.cache.entry != nil:
		deployment = r.cache.entry.Deployment
	}

	attrs := make([]slog.Attr, 0, 32)
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
	if a := r.account(); (r.served || r.cache.entry != nil) && a.costKnown {
		attrs = append(attrs, slog.String("cost_usd", a.cost.String()))
	}
	if r.cache.status != "" {
		attrs = append(attrs, slog.String("cache", r.cache.status))
	}
	attrs = append(attrs,
		slog.String("upstream_request_id", r.upstreamRequestID),
		slog.String("error_code", r.errorCode),
	)
	if r.errorMessage != "" {
		attrs = append(attrs, slog.String("error", r.errorMessage))
	}
	if r.route != nil {
		attrs = r.routeAttrs(attrs)
	}
	reqctx.Annotate(r.ctx, attrs...)
}

// routeAttrs appends the routing fields of the access log, and counts the
// served decision in route_decisions_total: policy, matched_rule and
// matched_quality_rule (when set), profile, routing_mode, the assessment's category and difficulty, quality_bar,
// served_q (the served deployment's q̂), relaxed (the relaxed soft checks,
// comma-separated) and, in observe mode, shadow_deployment.
func (r *record) routeAttrs(attrs []slog.Attr) []slog.Attr {
	rt := r.route
	exp := &rt.served.Explanation
	// The policy and the rule are logged only when set, as the other
	// optional fields are: each attr costs the redacting handler.
	if rt.eff.PolicyID != "" {
		attrs = append(attrs, slog.String("policy", rt.eff.PolicyID))
	}
	if rt.eff.MatchedRule != "" {
		attrs = append(attrs, slog.String("matched_rule", rt.eff.MatchedRule))
	}
	if rt.eff.MatchedQualityRule != "" {
		attrs = append(attrs, slog.String("matched_quality_rule", rt.eff.MatchedQualityRule))
	}
	attrs = append(attrs,
		slog.String("profile", rt.eff.Profile),
		slog.String("routing_mode", rt.eff.RoutingMode),
		slog.String("category", rt.assessment.Category),
		slog.Float64("difficulty", exp.Assessment.Difficulty),
		slog.Float64("quality_bar", rt.full.Explanation.QualityBar),
	)
	if d := r.deployment; d != nil {
		for i := range exp.Candidates {
			if c := &exp.Candidates[i]; c.Deployment == d.ID {
				attrs = append(attrs, slog.Float64("served_q", c.Q))
				break
			}
		}
		if r.served && !r.dryRun {
			r.g.metrics.routeDecisions.WithLabelValues(rt.eff.Profile, d.Model.ID, r.info.RouteReason).Inc()
		}
	}
	if len(rt.full.Relaxed) > 0 {
		attrs = append(attrs, slog.String("relaxed", strings.Join(rt.full.Relaxed, ",")))
	}
	if rt.shadow != nil {
		attrs = append(attrs, slog.String("shadow_deployment", rt.shadow.ID))
	}
	return attrs
}

// estIn is the request's input-token estimate on a deployment of m, at m's
// token_ratio, as the router estimates it: a fallback to another model, or
// the usage estimated on the served one, uses that model's own estimate.
func (r *record) estIn(m *catalog.Model) int64 {
	return router.InputTokens(r.inBytes, m.TokenRatio) + r.inImages
}

// ms returns d in fractional milliseconds.
func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
