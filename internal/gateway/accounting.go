package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/apierror"
	"switchyard/internal/auth"
	"switchyard/internal/budget"
	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/reqctx"
	"switchyard/internal/schema"
	"switchyard/internal/usage"
)

// Cost accounting and budgets (DESIGN §12): the attribution tags, budget
// admission before the first attempt and before a pricier fallback,
// settlement and the usage event when the request ends.

// HeaderTags is the request header with which a client attaches
// attribution tags to its request: k=v pairs, comma-separated.
const HeaderTags = "X-Switchyard-Tags"

// Limits of X-Switchyard-Tags.
const (
	maxTags        = 10
	maxTagValueLen = 64
)

// Error codes of cost accounting and budgets.
const (
	codeInvalidTags       = "invalid_tags"
	codeBudgetExceeded    = "budget_exceeded"
	codeBudgetUnavailable = "budget_unavailable"
	// typeInsufficientQuota is OpenAI's error type for a spent quota.
	typeInsufficientQuota = "insufficient_quota"
)

// Kinds of the budget notification metric.
const (
	noticeSoftThreshold = "soft_threshold"
	noticeExceeded      = "exceeded"
)

// Timeouts of budget store calls made outside the request's own context.
const (
	// settleTimeout bounds a settlement, which runs after the request,
	// even if its client has gone away.
	settleTimeout = time.Second
	// utilizationTimeout bounds reading every budget for a scrape.
	utilizationTimeout = time.Second
)

// principal identifies a caller: a tenant and one of its keys.
type principal struct{ tenant, key string }

// budgetScopes returns the budget scopes of every tenant's keys, and every
// scope once, in config order.
func budgetScopes(tenants []config.Tenant) (map[principal][]budget.Scope, []budget.Scope) {
	byKey := make(map[principal][]budget.Scope)
	var all []budget.Scope
	seen := make(map[string]bool)
	for _, t := range tenants {
		if len(t.Budgets) == 0 {
			continue
		}
		for _, k := range t.Keys {
			scopes := budget.ScopesFor(t, k.ID)
			if len(scopes) == 0 {
				continue
			}
			byKey[principal{t.ID, k.ID}] = scopes
			for _, s := range scopes {
				if !seen[s.Name] {
					seen[s.Name] = true
					all = append(all, s)
				}
			}
		}
	}
	return byKey, all
}

// parseTags parses the X-Switchyard-Tags header: at most 10 k=v pairs,
// comma-separated, with optional spaces around each key and value. Keys
// match ^[a-z0-9][a-z0-9_.-]{0,31}$ and appear once; values are 1 to 64
// printable ASCII characters other than ',' and '='. Several header lines
// are joined with commas. It returns nil without the header, and false
// when the header is malformed.
func parseTags(h http.Header) (map[string]string, bool) {
	values := h.Values(HeaderTags)
	if len(values) == 0 {
		return nil, true
	}
	v := strings.Join(values, ",")
	if strings.TrimSpace(v) == "" {
		return nil, true
	}
	pairs := strings.Split(v, ",")
	if len(pairs) > maxTags {
		return nil, false
	}
	tags := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, val, ok := strings.Cut(p, "=")
		k, val = strings.TrimSpace(k), strings.TrimSpace(val)
		if !ok || !usage.ValidTagKey(k) || !validTagValue(val) {
			return nil, false
		}
		if _, dup := tags[k]; dup {
			return nil, false
		}
		tags[k] = val
	}
	return tags, true
}

// validTagValue reports whether v is 1 to 64 printable ASCII characters
// other than ',' and '='.
func validTagValue(v string) bool {
	if len(v) == 0 || len(v) > maxTagValueLen {
		return false
	}
	for i := range len(v) {
		if c := v[i]; c < 0x20 || c > 0x7e || c == ',' || c == '=' {
			return false
		}
	}
	return true
}

// invalidTags is the answer for a malformed X-Switchyard-Tags header. It
// never echoes the header.
func invalidTags() *Error {
	return &Error{
		Status: http.StatusBadRequest, Type: apierror.TypeInvalidRequest, Code: codeInvalidTags, Param: HeaderTags,
		Message: "The " + HeaderTags + " header must be at most 10 comma-separated key=value pairs, " +
			"each key unique, 1 to 32 characters of a-z, 0-9, '_', '.' and '-' starting with a letter or digit, " +
			"and each value 1 to 64 printable ASCII characters other than ',' and '='.",
	}
}

// budgetExceeded is the answer when a blocking budget has no room for the
// request: 429 with Retry-After until the budget's window resets.
func budgetExceeded(e *budget.ExhaustedError, now time.Time) *Error {
	return &Error{
		Status: http.StatusTooManyRequests, Type: typeInsufficientQuota, Code: codeBudgetExceeded,
		RetryAfter: max(e.ResetAt.Sub(now), time.Second),
		Message: fmt.Sprintf("The budget %s has no room for this request; it resets at %s.",
			e.Scope.Name, e.ResetAt.UTC().Format(time.RFC3339)),
	}
}

// budgetUnavailable is the answer when the budget store fails and
// budgets.fail_mode is closed.
func budgetUnavailable() *Error {
	return &Error{
		Status: http.StatusServiceUnavailable, Type: apierror.TypeServer, Code: codeBudgetUnavailable,
		Message: "The budget store is unavailable, so the request cannot be checked against its budgets. Please retry later.",
	}
}

// readTags reads the request's attribution tags into rec. When the header
// is malformed it answers the client and returns false.
func (g *Gateway) readTags(ex Exchange, rec *record) bool {
	tags, ok := parseTags(ex.Header())
	if !ok {
		rec.respondError(ex, Info{}, invalidTags())
		return false
	}
	rec.tags = tags
	return true
}

// estimate is the budget estimate of the request on d, in micro-USD,
// rounded up: its estimated input and output tokens (output at max_tokens
// when the request sets it) at d's prices. It is 0 when d's prices are
// unknown.
func (g *Gateway) estimate(rec *record, d *catalog.Deployment) int64 {
	c, ok := usage.CostFor(d.Model.Type, usage.Tokens{Input: rec.estIn(d.Model), Output: rec.estOut}, d.Prices)
	if !ok {
		return 0
	}
	return c.MicroUSDCeil()
}

// admitBudget admits the request against the budgets of its tenant and key
// (DESIGN §12.3) before its first attempt, at the estimated cost on the
// first deployment of its plan. A request with no budgets is not admitted
// at all. Phase 6's cache hits are answered before execute and never
// reach this point, so they are not admitted.
//
// When a blocking budget has no room it answers 429 budget_exceeded and
// returns false: no upstream is called. A request under a blocking budget
// is admitted at 1 micro-USD at least, so that an exhausted budget refuses
// even a deployment without prices, whose estimate is 0.
//
// When the store fails, fail_mode open serves the request unchecked, and
// closed answers 503 budget_unavailable. While the store is marked
// unavailable after a failure (see budget.Circuit), it is not called and
// the outcome is the same, at once.
func (g *Gateway) admitBudget(ctx context.Context, ex Exchange, rec *record) bool {
	scopes := g.scopes[principal{rec.tenant, rec.keyID}]
	if len(scopes) == 0 || len(rec.plan.entries) == 0 {
		return true
	}
	est := g.estimate(rec, rec.plan.entries[0].d)
	if est < 1 && blocks(scopes) {
		est = 1
	}
	now := g.now()
	adm, err := g.budgets.Admit(ctx, scopes, est, now)
	var ee *budget.ExhaustedError
	switch {
	case err == nil:
		rec.adm, rec.admitted = adm, true
		g.notifyBudget(ctx, adm)
		return true
	case errors.As(err, &ee):
		g.metrics.budgetRejections.Inc()
		rec.respondError(ex, rec.info, budgetExceeded(ee, now))
		return false
	case ctx.Err() != nil:
		g.answerFailure(ctx, ex, rec, failureState{})
		return false
	}
	g.metrics.budgetStoreErrors.Inc()
	if g.failClosed {
		warnStore(ctx, "budget store unavailable; rejecting the request (budgets.fail_mode closed)", err)
		rec.respondError(ex, rec.info, budgetUnavailable())
		return false
	}
	warnStore(ctx, "budget store unavailable; serving the request without budget checks (budgets.fail_mode open)", err)
	return true
}

// blocks reports whether any of scopes is a blocking budget.
func blocks(scopes []budget.Scope) bool {
	for _, s := range scopes {
		if s.Block {
			return true
		}
	}
	return false
}

// warnStore logs a failure of the budget store at WARN. A call skipped
// while the store is marked unavailable (budget.ErrCircuitOpen) is not
// logged: the failure that marked it was, so a store that is down is
// logged about once per budget.Circuit period rather than per request.
func warnStore(ctx context.Context, msg string, err error) {
	if errors.Is(err, budget.ErrCircuitOpen) {
		return
	}
	reqctx.Logger(ctx).WarnContext(ctx, msg, "error", err)
}

// chargeFallback admits the difference before an attempt on e when e's
// estimate is higher than what the request has been charged so far, as
// for a fallback to a pricier deployment. It returns why e must be passed
// over, or "" if it may be tried.
func (g *Gateway) chargeFallback(ctx context.Context, rec *record, e *planEntry) string {
	if !rec.admitted {
		return ""
	}
	extra := g.estimate(rec, e.d) - rec.adm.Charged()
	if extra <= 0 {
		return ""
	}
	err := g.budgets.Delta(ctx, &rec.adm, extra, g.now())
	var ee *budget.ExhaustedError
	switch {
	case err == nil:
		g.notifyBudget(ctx, rec.adm)
		return ""
	case errors.As(err, &ee):
		g.metrics.budgetFallbackSkips.Inc()
		return "budget " + ee.Scope.Name + " has no room for it"
	case ctx.Err() != nil:
		return "" // the executor answers the canceled request
	}
	g.metrics.budgetStoreErrors.Inc()
	if g.failClosed {
		warnStore(ctx, "budget store unavailable; skipping a pricier fallback (budgets.fail_mode closed)", err)
		return "budget store unavailable"
	}
	warnStore(ctx, "budget store unavailable; falling back without budget checks (budgets.fail_mode open)", err)
	return ""
}

// notifyBudget logs and counts what an admission found: soft thresholds
// reached for the first time in their window, and notify budgets over
// their limit. The downgrade to a budget's soft profile happens when the
// next request is routed (see budgetState).
func (g *Gateway) notifyBudget(ctx context.Context, adm budget.Admission) {
	for _, s := range adm.SoftCrossed {
		g.metrics.budgetNotices.WithLabelValues(s.Name, noticeSoftThreshold).Inc()
		reqctx.Logger(ctx).InfoContext(ctx, "budget soft threshold reached", "budget", s.Name)
	}
	for _, s := range adm.Exceeded {
		g.metrics.budgetNotices.WithLabelValues(s.Name, noticeExceeded).Inc()
		reqctx.Logger(ctx).InfoContext(ctx, "budget exceeded; serving the request (on_exhausted notify)", "budget", s.Name)
	}
}

// accounting is what a request used and cost.
type accounting struct {
	tokens                     usage.Tokens
	usageKnown, usageEstimated bool
	cost                       usage.Money
	costKnown                  bool
	// pricing is the served deployment's, or zero.
	pricing config.Pricing
}

// account returns what the request used and cost, computing it once the
// first time.
//
// A request no upstream served costs 0, which is known, unless an attempt
// reached the upstream and may have been billed (see runAttempt): then its
// usage and cost are estimated, as the input estimate and the output
// estimate at the prices of that attempt's deployment, and the cost is
// unknown.
//
// Otherwise the tokens are the upstream's usage, normalized, and the cost
// is those tokens at the served deployment's exact prices. A stream that
// broke off keeps the input classes it reported and counts its output as
// the larger of the reported output and the output relayed at one token
// per 4 bytes, estimated. Without any usage, the tokens are the input
// estimate and the output relayed, estimated too.
func (r *record) account() *accounting {
	if r.acct != nil {
		return r.acct
	}
	a := &accounting{costKnown: true}
	r.acct = a
	d := r.deployment
	if !r.served || d == nil {
		if s := r.sentTo; s != nil && !r.served {
			a.pricing = s.Prices
			a.tokens = usage.Tokens{Input: r.estIn(s.Model), Output: r.estOut}
			a.usageEstimated, a.costKnown = true, false
			if c, ok := usage.CostFor(s.Model.Type, a.tokens, s.Prices); ok {
				a.cost = c
			}
		}
		return a
	}
	a.pricing = d.Prices
	switch {
	case r.usage != nil && r.usagePartial:
		a.tokens, a.usageEstimated = usage.FromUsage(r.usage), true
		a.tokens.Output = max(a.tokens.Output, usage.EstimateTokens(0, r.outBytes).Output)
	case r.usage != nil:
		a.tokens, a.usageKnown = usage.FromUsage(r.usage), true
	default:
		a.tokens, a.usageEstimated = usage.EstimateTokens(r.estIn(d.Model), r.outBytes), true
	}
	a.cost, a.costKnown = usage.CostFor(d.Model.Type, a.tokens, d.Prices)
	return a
}

// costHeader returns the X-Switchyard-Cost-Usd value of a successful
// response: the exact cost when the usage and the prices are known, else
// "".
func (r *record) costHeader() string {
	a := r.account()
	if !a.usageKnown || !a.costKnown {
		return ""
	}
	return a.cost.String()
}

// settle charges the request's actual cost to the budgets it was admitted
// against: 0 if no attempt reached an upstream that may have billed it,
// the exact cost rounded up to micro-USD when known, and the admitted
// estimate otherwise, as for a request canceled after the upstream
// received it. It runs even when the client has gone away.
func (r *record) settle() {
	if !r.admitted {
		return
	}
	a := r.account()
	var actual int64
	switch {
	case !r.served && r.sentTo == nil:
	case a.costKnown:
		actual = a.cost.MicroUSDCeil()
	default:
		actual = r.adm.Charged()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), settleTimeout)
	defer cancel()
	if err := r.g.budgets.Settle(ctx, r.adm, actual); err != nil {
		r.g.metrics.budgetStoreErrors.Inc()
		warnStore(r.ctx, "budget settlement failed; the budgets keep the estimate", err)
	}
}

// recordUsage counts the request's tokens and cost in the metrics and
// records its usage event. A request without an authenticated tenant is
// not recorded.
func (r *record) recordUsage(latency time.Duration) {
	a := r.account()
	d := r.deployment
	if !r.served {
		d = nil
	}
	var baseline usage.Money
	if d != nil && a.costKnown {
		m := r.g.metrics
		m.cost.WithLabelValues(d.Provider, d.Model.ID).Add(a.cost.Float64())
		addTokens(m.tokens, d, a.tokens)
		if r.model != nil {
			prices := make([]config.Pricing, len(r.model.Deployments))
			for i, md := range r.model.Deployments {
				prices[i] = md.Prices
			}
			if b, ok := usage.BaselineCost(r.model.Type, a.tokens, prices); ok {
				baseline = b
			}
		}
		m.savingsRouting.Add(baseline.Sub(a.cost).Float64())
	} else if d != nil {
		addTokens(r.g.metrics.tokens, d, a.tokens)
	}
	// Observe mode: the request's tokens priced at the deployment the
	// router would have chosen, as the baseline is.
	var shadow *catalog.Deployment
	var shadowCost usage.Money
	shadowKnown := false
	if r.route != nil && r.route.shadow != nil {
		shadow = r.route.shadow
		if d != nil && a.costKnown {
			shadowCost, shadowKnown = usage.CostFor(shadow.Model.Type, a.tokens, shadow.Prices)
		}
		if shadowKnown {
			r.g.metrics.projectedSavings.Add(baseline.Sub(shadowCost).Float64())
		}
	}
	if r.g.ledger == nil || !r.authenticated {
		return
	}
	ev := &usage.Event{
		ID:             reqctx.RequestID(r.ctx),
		Time:           r.start,
		Tenant:         r.tenant,
		KeyID:          r.keyID,
		Endpoint:       r.endpoint,
		RequestedModel: r.requestedModel,
		Alias:          r.alias,
		Stream:         r.stream,
		Status:         r.status,
		Attempts:       r.attempts,
		FallbackReason: r.fallbackReason,
		RouteReason:    r.info.RouteReason,
		CacheStatus:    usage.CacheStatusMiss,
		Tokens:         a.tokens,
		UsageKnown:     a.usageKnown,
		UsageEstimated: a.usageEstimated,
		Cost:           a.cost,
		CostKnown:      a.costKnown,
		Pricing:        a.pricing,
		BaselineCost:   baseline,
		LatencyMS:      ms(latency),
		TTFTMS:         ms(r.ttft),
		Tags:           r.tags,
		ConfigVersion:  r.g.configVersion,
		Profile:        r.profile,
		RoutingMode:    r.routingMode,
	}
	if shadow != nil {
		ev.ShadowDeployment, ev.ShadowCost, ev.ShadowCostKnown = shadow.ID, shadowCost, shadowKnown
	}
	if r.route != nil {
		ev.Category = r.route.assessment.Category
	}
	switch {
	case d != nil:
		ev.Deployment, ev.Provider = d.ID, d.Provider
	case r.sentTo != nil:
		ev.Deployment, ev.Provider = r.sentTo.ID, r.sentTo.Provider
	}
	if r.model != nil {
		ev.BaselineModel = r.model.ID
	}
	r.g.ledger.Record(ev)
}

// addTokens counts t in the tokens metric of deployment d.
func addTokens(c *prometheus.CounterVec, d *catalog.Deployment, t usage.Tokens) {
	for _, class := range [...]struct {
		name string
		n    int64
	}{{"input", t.Input}, {"cached_input", t.CachedInput}, {"cache_write", t.CacheWrite}, {"output", t.Output}} {
		if class.n > 0 {
			c.WithLabelValues(d.Provider, d.Model.ID, class.name).Add(float64(class.n))
		}
	}
}

// identify records who is calling, from the principal the auth middleware
// stored in the context.
func (r *record) identify() {
	if p, ok := auth.FromContext(r.ctx); ok {
		r.tenant, r.keyID, r.authenticated = p.TenantID, p.KeyID, true
	}
}

// outputText holds the parts of a choice, in any of the response and chunk
// shapes, that count as output when estimating tokens.
type outputText struct {
	Text    string      `json:"text"`
	Message outputParts `json:"message"`
	Delta   outputParts `json:"delta"`
}

type outputParts struct {
	Content          string `json:"content"`
	Refusal          string `json:"refusal"`
	ReasoningContent string `json:"reasoning_content"`
	ToolCalls        []struct {
		Function struct {
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// bytes returns the length of the output text in o.
func (o *outputText) bytes() int64 {
	n := len(o.Text)
	for _, p := range [...]*outputParts{&o.Message, &o.Delta} {
		n += len(p.Content) + len(p.Refusal) + len(p.ReasoningContent)
		for _, tc := range p.ToolCalls {
			n += len(tc.Function.Arguments)
		}
	}
	return int64(n)
}

// choicesOutputBytes returns the bytes of output text in choices, the raw
// choices of a response or stream chunk. Choices that do not decode count
// as their raw size, which over-estimates.
func choicesOutputBytes(choices []json.RawMessage) int64 {
	var n int64
	for _, c := range choices {
		var o outputText
		if json.Unmarshal(c, &o) != nil {
			n += int64(len(c))
			continue
		}
		n += o.bytes()
	}
	return n
}

// responseOutputBytes returns the bytes of output text in a non-streaming
// completion body.
func responseOutputBytes(body []byte) int64 {
	var r struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if json.Unmarshal(body, &r) != nil {
		return int64(len(body))
	}
	return choicesOutputBytes(r.Choices)
}

// chunkOutputBytes returns the bytes of output text in a stream chunk.
func chunkOutputBytes(c *schema.ChatChunk) int64 {
	return choicesOutputBytes(c.Choices)
}

// budgetUtilizationDesc describes the utilization gauge.
var budgetUtilizationDesc = prometheus.NewDesc("switchyard_budget_utilization_ratio",
	"Spend in the current window as a fraction of the limit, by budget scope (for example acme/month or acme/key:k1/day).",
	[]string{"scope"}, nil)

// budgetCollector reports each budget's utilization as it is at scrape
// time, read from the budget store. If the store fails, the remaining
// budgets are left out of that scrape.
type budgetCollector struct{ g *Gateway }

// Describe implements prometheus.Collector.
func (c budgetCollector) Describe(ch chan<- *prometheus.Desc) { ch <- budgetUtilizationDesc }

// Collect implements prometheus.Collector.
func (c budgetCollector) Collect(ch chan<- prometheus.Metric) {
	g := c.g
	if len(g.allScopes) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), utilizationTimeout)
	defer cancel()
	now := g.now()
	for _, s := range g.allScopes {
		spent, err := g.budgets.Spent(ctx, s, now)
		if err != nil {
			g.metrics.budgetStoreErrors.Inc()
			return
		}
		ratio := 0.0
		if s.LimitMicro > 0 {
			ratio = float64(spent) / float64(s.LimitMicro)
		}
		ch <- prometheus.MustNewConstMetric(budgetUtilizationDesc, prometheus.GaugeValue, ratio, s.Name)
	}
}
