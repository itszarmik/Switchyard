package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"switchyard/internal/apierror"
	"switchyard/internal/budget"
	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/policy"
	"switchyard/internal/quality"
	"switchyard/internal/router"
	"switchyard/internal/schema"
	"switchyard/internal/usage"
)

// Routing (DESIGN §9): the request's assessment, its effective policy,
// the budget soft-threshold downgrade, the model field's resolution, and
// the router's decision, for chat completions, legacy completions and the
// explain endpoint.

// Request headers with the client's hints for the quality assessor
// (DESIGN §10.6).
const (
	HeaderTask       = "X-Switchyard-Task"
	HeaderDifficulty = "X-Switchyard-Difficulty"
)

// Difficulty hint words and their values.
var difficultyWords = map[string]float64{"low": 0.2, "medium": 0.5, "high": 0.85}

// Route reasons beyond those of exec.go, for X-Switchyard-Route-Reason.
const (
	reasonQualityRelaxed  = "quality_relaxed"
	reasonLatencyRelaxed  = "latency_relaxed"
	reasonObserve         = "observe"
	reasonBudgetDowngrade = "budget_downgrade"
	reasonPreferred       = "preferred"
)

// codeInvalidHeader is the error code of a malformed routing header.
const codeInvalidHeader = "invalid_header"

// routeRequest is what routing needs to know about one request.
type routeRequest struct {
	// model is the model field as the client sent it.
	model  string
	header http.Header
	// chat is the request the assessor reads: the request itself, or the
	// chat shim of a legacy completion; nil assesses nothing.
	chat *schema.ChatRequest
	// tenant and keyID identify the caller; tags are its attribution tags.
	tenant, keyID string
	tags          map[string]string
	// size is the body's length; images and inline its image parts and
	// their inline bytes.
	size, images, inline int
	// maxTokens (when hasMax) is the request's per-output cap, and outputs
	// the completions it generates.
	maxTokens int64
	hasMax    bool
	outputs   int64
	required  schema.CapabilitySet
	stream    bool
	// keep, if set, excludes deployments this endpoint cannot use.
	keep func(d *catalog.Deployment) string
	// dryRun is set for the explain endpoint: no metrics are counted.
	dryRun bool
}

// routed is the outcome of routing one request.
type routed struct {
	// requested is the client's model field after alias resolution, such
	// as "auto:cheap" or a catalog model id; alias is the alias the client
	// named, or "".
	requested, alias string
	// auto is set for "auto" and "auto:<profile>".
	auto bool
	// res is the catalog resolution of a concrete model field.
	res catalog.Resolution
	// baseline is the model the savings are measured against: the
	// requested model, or the policy's baseline_model for auto.
	baseline   *catalog.Model
	assessment quality.Assessment
	// eff is the effective policy, with the downgrade applied.
	eff policy.Effective
	// downgraded names the budget scope whose soft threshold switched the
	// profile, or "".
	downgraded string
	// inBytes and inImages are the request's input for the token
	// estimates (see inputSize); estIn is the input estimate at the
	// requested model's token_ratio, or 1 for auto, which rules match on.
	// estOut is the budget estimate of the output: max_tokens when the
	// request sets it. routeOut is the routing estimate of the output
	// (DESIGN §9.1).
	inBytes, inImages       int64
	estIn, estOut, routeOut int64
	// full is the router's decision for the request. served is the
	// decision the gateway executes: full in enforce mode, and in observe
	// mode pinned, the pinned decision for the requested or baseline model.
	full, pinned router.Decision
	served       *router.Decision
	// fullOK reports that the full decision chose a deployment.
	fullOK bool
	// shadow is, in observe mode, the deployment full chose, or nil.
	shadow *catalog.Deployment
	// reason is the route reason before any fallback.
	reason string
}

// observe reports whether the request runs in observe mode.
func (rt *routed) observe() bool { return rt.eff.RoutingMode == policy.ModeObserve }

// route routes rr (steps 1–9 of the pipeline): routePolicy, then
// routeDecide.
//
// The outcome is written to rt, which route returns with any error, so
// that the caller can send the headers known by then; it returns nil only
// when the model field could not be resolved. rt is the caller's, usually
// part of the request's record, to spare an allocation per request.
func (g *Gateway) route(ctx context.Context, endpoint string, rr *routeRequest, rt *routed) (*routed, *Error) {
	out, preq, e := g.routePolicy(endpoint, rr, rt)
	if e != nil {
		return out, e
	}
	return g.routeDecide(ctx, rr, rt, preq)
}

// routePolicy is the first half of routing (steps 4–7): it resolves the
// model field, assesses the request with the client's hints and resolves
// the effective policy with header tightening, and for auto the policy's
// baseline model. It reads no shared state, so that the cache lookup,
// which needs the policy, can come before anything that depends on the
// budgets or the deployments' health. Its results are written to rt, as
// for route; it also returns the policy request, which routeDecide needs.
func (g *Gateway) routePolicy(endpoint string, rr *routeRequest, rt *routed) (*routed, policy.Request, *Error) {
	*rt = routed{}
	rt.served = &rt.full
	target, virtual := g.catalog.VirtualTarget(rr.model)
	if virtual {
		rt.auto, rt.requested = true, target
		if target != rr.model {
			rt.alias = rr.model
		}
	} else {
		res, e := g.resolve(rr.model, endpoint, chatType)
		if e != nil {
			return nil, policy.Request{}, e
		}
		rt.res, rt.alias, rt.baseline, rt.requested = res, res.Alias, res.Model, res.Model.ID
	}

	hints, e := parseHints(rr.header)
	if e != nil {
		return rt, policy.Request{}, e
	}
	ratio := 1.0
	if !rt.auto {
		ratio = rt.res.Model.TokenRatio
	}
	rt.inBytes, rt.inImages = inputSize(rr.size, rr.images, rr.inline)
	rt.estIn = router.InputTokens(rt.inBytes, ratio) + rt.inImages
	rt.estOut = estimateOut(rr.maxTokens, rr.hasMax, rr.outputs)
	rt.routeOut = expectedOut(rr.maxTokens, rr.hasMax, rr.outputs, g.routing.ExpectedOutputTokens)
	if rr.chat != nil {
		rt.assessment = g.assessor.Assess(rr.chat, hints)
	} else {
		rt.assessment = quality.Assessment{Category: quality.CategoryGeneral}
	}

	tenant := g.tenants[rr.tenant]
	preq := policy.Request{
		Model:          rr.model,
		Task:           rt.assessment.Category,
		Difficulty:     rt.assessment.Difficulty,
		KeyID:          rr.keyID,
		KeyTags:        keyTags(tenant, rr.keyID),
		Headers:        rr.header,
		Tags:           rr.tags,
		Stream:         rr.stream,
		HasTools:       rr.required.Has(schema.CapTools),
		HasImages:      rr.images > 0 || rr.required.Has(schema.CapVision),
		EstInputTokens: rt.estIn,
	}
	if p, ok := strings.CutPrefix(rt.requested, "auto:"); rt.auto && ok {
		if p == "" {
			return rt, policy.Request{}, g.emptyProfile(tenant, rr)
		}
		preq.Profile = p
	}
	eff, err := g.policy.Resolve(tenant, rr.keyID, preq)
	if err != nil {
		return rt, policy.Request{}, policyError(rr.model, err)
	}
	rt.eff = eff
	if rt.auto {
		if err := policy.CheckAuto(eff); err != nil {
			return rt, policy.Request{}, policyError(rr.model, err)
		}
		if rt.baseline, e = g.baselineModel(eff.BaselineModel); e != nil {
			return rt, policy.Request{}, e
		}
	}
	return rt, preq, nil
}

// routeDecide is the second half of routing (step 9), after routePolicy
// has filled rt and returned preq: it downgrades the profile of an auto request whose
// budget's soft threshold has been crossed, and asks the router for a
// decision, which in observe mode is the shadow decision next to the
// pinned one that is served.
func (g *Gateway) routeDecide(ctx context.Context, rr *routeRequest, rt *routed, preq policy.Request) (*routed, *Error) {
	tenant := g.tenants[rr.tenant]
	remaining, soft := g.budgetState(ctx, rr.tenant, rr.keyID)
	downgrade := ""
	if rt.auto && soft != nil {
		eff, err := g.policy.ResolveDowngraded(tenant, rr.keyID, preq, soft.SoftProfile)
		if err != nil {
			return rt, policyError(rr.model, err)
		}
		rt.eff, rt.downgraded = eff, soft.Name
		downgrade = "budget " + soft.Name + " soft threshold crossed; profile " + soft.SoftProfile
		if !rr.dryRun {
			g.metrics.routeDowngrades.WithLabelValues(soft.Name).Inc()
		}
	}

	in := router.Input{
		Requested:            rt.requested,
		Resolution:           rt.res,
		Auto:                 rt.auto,
		Required:             rr.required,
		InputBytes:           rt.inBytes,
		ImageTokens:          rt.inImages,
		EstOut:               rt.routeOut,
		ContextOut:           rt.estOut,
		Drop:                 g.drop,
		Assessment:           rt.assessment,
		Policy:               rt.eff,
		BudgetRemainingMicro: remaining,
		Keep:                 g.keeper(rr.keep),
		Downgrade:            downgrade,
	}
	full, ferr := g.decide(in)
	rt.full = full
	rt.fullOK = ferr == nil
	if !rr.dryRun {
		for _, c := range full.Relaxed {
			g.metrics.routeRelaxations.WithLabelValues(c).Inc()
		}
	}
	if !rt.observe() || rr.dryRun {
		if ferr != nil {
			return rt, ferr
		}
		rt.reason = baseReason(rt)
		return rt, nil
	}

	// Observe mode: serve the requested model, or the baseline for auto,
	// pinned, and record what the full decision would have chosen.
	if rt.fullOK {
		rt.shadow = full.Plan[0].Deployment
	}
	if rt.auto {
		res, err := g.catalog.Resolve(rt.baseline.ID)
		if err != nil {
			return rt, internalError("Could not resolve the policy's baseline model.")
		}
		in.Resolution, in.Requested = res, res.Model.ID
	}
	in.Auto = false
	in.Policy.Substitution = policy.SubstitutionPinned
	var serr *Error
	rt.pinned, serr = g.decide(in)
	rt.served = &rt.pinned
	if serr != nil {
		return rt, serr
	}
	rt.reason = baseReason(rt)
	return rt, nil
}

// decide runs the router. When the remaining budget excludes every
// candidate, it decides again without it, so that the budget admission
// answers 429 budget_exceeded rather than 400 no_eligible_model: the
// remaining budget only steers the choice toward what fits.
func (g *Gateway) decide(in router.Input) (router.Decision, *Error) {
	dec, err := g.router.Decide(in)
	var ne *catalog.NoEligibleError
	if errors.As(err, &ne) && in.BudgetRemainingMicro >= 0 {
		in.BudgetRemainingMicro = -1
		if retry, rerr := g.router.Decide(in); rerr == nil && !retry.Unhealthy {
			return retry, nil
		}
	}
	switch {
	case errors.As(err, &ne):
		return dec, noEligibleModel(ne.Model, ne.Exclusions)
	case err != nil:
		return dec, internalError("Could not choose a deployment.")
	case dec.Unhealthy:
		return dec, noHealthyUpstream(in.Requested, exclusions(dec.Explanation))
	}
	return dec, nil
}

// emptyProfile is the answer for "auto:" with no profile name: 400
// model_not_supported, naming the profiles the caller may select.
func (g *Gateway) emptyProfile(tenant config.Tenant, rr *routeRequest) *Error {
	msg := "the auto: prefix needs a profile name"
	allowed, err := g.policy.SelectableProfiles(tenant, rr.keyID)
	if err == nil && len(allowed) > 0 {
		msg += "; allowed profiles: " + strings.Join(allowed, ", ")
	}
	return policyError(rr.model, &policy.ModelNotSupportedError{Message: msg, Allowed: allowed})
}

// keeper returns the router's Keep filter: deployments without an
// adapter, then those keep excludes.
func (g *Gateway) keeper(keep func(*catalog.Deployment) string) func(*catalog.Deployment) string {
	return func(d *catalog.Deployment) string {
		if g.adapters[d.Provider] == nil {
			return "provider type " + d.ProviderType + " is not supported"
		}
		if keep != nil {
			return keep(d)
		}
		return ""
	}
}

// baselineModel returns the catalog model named by a policy's
// baseline_model, which validation guarantees is a chat model.
func (g *Gateway) baselineModel(id string) (*catalog.Model, *Error) {
	res, err := g.catalog.Resolve(id)
	if err != nil || res.Model.Type != chatType {
		return nil, internalError("The policy's baseline model is not a chat model in the catalog.")
	}
	return res.Model, nil
}

// budgetState reads the budgets of the caller for routing: the least
// remaining room of its blocking budgets in micro-USD (-1 when unknown or
// none), and the first budget whose soft threshold has been crossed and
// that names a soft profile, if any. The store is read once, through the
// budget circuit; when it fails, the budgets are unknown and no profile is
// downgraded. An offline gateway has no store: it treats every budget as
// untouched, with its whole limit left and its soft threshold not
// crossed, as a fresh gateway would find it.
func (g *Gateway) budgetState(ctx context.Context, tenant, keyID string) (int64, *budget.Scope) {
	scopes := g.scopes[principal{tenant, keyID}]
	if len(scopes) == 0 {
		return -1, nil
	}
	var states []budget.State
	var err error
	if g.offline {
		states = make([]budget.State, len(scopes))
	} else {
		states, err = g.budgets.SoftCrossed(ctx, scopes, g.now())
	}
	if err != nil {
		if ctx.Err() == nil {
			g.metrics.budgetStoreErrors.Inc()
			warnStore(ctx, "budget store unavailable; routing without the budgets' state", err)
		}
		return -1, nil
	}
	remaining := int64(-1)
	var soft *budget.Scope
	for i, s := range scopes {
		if s.Block {
			if r := states[i].Remaining(s); remaining < 0 || r < remaining {
				remaining = r
			}
		}
		if soft == nil && states[i].SoftCrossed && s.SoftProfile != "" {
			soft = &scopes[i]
		}
	}
	return remaining, soft
}

// keyTags returns the tags of the key keyID of tenant.
func keyTags(t config.Tenant, keyID string) []string {
	for i := range t.Keys {
		if t.Keys[i].ID == keyID {
			return t.Keys[i].Tags
		}
	}
	return nil
}

// expectedOut is the routing estimate of the output tokens (DESIGN §9.1):
// per output, the smaller of max_tokens (when set) and
// routing.expected_output_tokens.
func expectedOut(maxTokens int64, hasMax bool, outputs, expected int64) int64 {
	if expected <= 0 {
		expected = defaultEstOut
	}
	if hasMax && maxTokens > 0 {
		maxTokens = min(maxTokens, expected)
	} else {
		maxTokens = expected
	}
	return estimateOut(maxTokens, true, outputs)
}

// parseHints reads X-Switchyard-Task and X-Switchyard-Difficulty. A task
// that is not a category, a difficulty that is neither low, medium, high
// nor a number in [0, 1], and either header given more than once, is 400
// invalid_header.
func parseHints(h http.Header) (quality.Hints, *Error) {
	var hints quality.Hints
	if v, ok, e := hintValue(h, HeaderTask); e != nil {
		return hints, e
	} else if ok {
		if !quality.IsCategory(v) {
			return hints, invalidHeader(HeaderTask, "must be one of general, coding, reasoning, extraction, creative, tool_use")
		}
		hints.Task = v
	}
	if v, ok, e := hintValue(h, HeaderDifficulty); e != nil {
		return hints, e
	} else if ok {
		d, word := difficultyWords[strings.ToLower(v)]
		if !word {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || len(v) > 32 || !(f >= 0 && f <= 1) || strings.ContainsAny(v, "eEnN") {
				return hints, invalidHeader(HeaderDifficulty, "must be low, medium, high or a number between 0 and 1")
			}
			d = f
		}
		hints.Difficulty, hints.HasDifficulty = d, true
	}
	return hints, nil
}

// hintValue returns the single trimmed value of header name.
func hintValue(h http.Header, name string) (string, bool, *Error) {
	values := h.Values(name)
	switch {
	case len(values) == 0:
		return "", false, nil
	case len(values) > 1:
		return "", false, invalidHeader(name, "must be given once")
	}
	v := strings.TrimSpace(values[0])
	if v == "" {
		return "", false, invalidHeader(name, "must not be empty")
	}
	return v, true, nil
}

// invalidHeader is the answer for a malformed routing header. It names the
// header but never echoes its value.
func invalidHeader(name, reason string) *Error {
	return &Error{
		Status: http.StatusBadRequest, Type: apierror.TypeInvalidRequest, Code: codeInvalidHeader, Param: name,
		Message: "The " + name + " header is invalid: it " + reason + ".",
	}
}

// policyError maps an error of the policy engine onto the answer.
func policyError(model string, err error) *Error {
	var ih *policy.InvalidHeaderError
	var mn *policy.ModelNotSupportedError
	switch {
	case errors.As(err, &ih):
		return invalidHeader(ih.Header, ih.Reason)
	case errors.As(err, &mn):
		return &Error{
			Status: http.StatusBadRequest, Type: apierror.TypeInvalidRequest, Code: codeModelNotSupport, Param: "model",
			Message: fmt.Sprintf("The model `%s` cannot be used: %s.", model, mn.Message),
		}
	}
	return internalError("Could not resolve the request's policy.")
}

// internalError is a 500 with a fixed message.
func internalError(msg string) *Error {
	return &Error{Status: http.StatusInternalServerError, Type: apierror.TypeServer, Code: "internal_error", Message: msg}
}

// exclusions lists the ineligible candidates of an explanation.
func exclusions(exp router.Explanation) []catalog.Exclusion {
	var out []catalog.Exclusion
	for _, c := range exp.Candidates {
		if !c.Eligible {
			out = append(out, catalog.Exclusion{Deployment: c.Deployment, Reason: c.ExcludedBy})
		}
	}
	return out
}

// baseReason is the route reason of a routed request before any fallback:
// observe, budget_downgrade, pinned (model@provider), quality_relaxed,
// latency_relaxed, preferred (a preference tier decided the primary), or
// cheapest_eligible.
func baseReason(rt *routed) string {
	dec := rt.served
	switch {
	case rt.observe():
		return reasonObserve
	case rt.downgraded != "":
		return reasonBudgetDowngrade
	case rt.res.Pinned:
		return reasonPinned
	case slices.Contains(dec.Relaxed, router.RelaxedQuality):
		return reasonQualityRelaxed
	case slices.Contains(dec.Relaxed, router.RelaxedLatency):
		return reasonLatencyRelaxed
	case preferred(dec, rt.eff):
		return reasonPreferred
	}
	return reasonCheapest
}

// preferred reports whether a preference tier decided the primary: an
// eligible candidate in a later tier would have won on the objective.
func preferred(dec *router.Decision, eff policy.Effective) bool {
	if len(eff.Prefer) == 0 || len(dec.Plan) == 0 {
		return false
	}
	cands := dec.Explanation.Candidates
	id := dec.Plan[0].Deployment.ID
	i := slices.IndexFunc(cands, func(c router.CandidateInfo) bool { return c.Deployment == id })
	if i < 0 || cands[i].Tier >= len(eff.Prefer) {
		return false
	}
	primary := &cands[i]
	for k := range cands {
		c := &cands[k]
		if c.Eligible && c.Tier > primary.Tier && better(c, primary, eff.Constraints.Objective) {
			return true
		}
	}
	return false
}

// better reports whether a beats b on the objective.
func better(a, b *router.CandidateInfo, objective string) bool {
	if objective == policy.ObjectiveLatency && a.EstTTFTMs != b.EstTTFTMs {
		return a.EstTTFTMs < b.EstTTFTMs
	}
	ca, okA := parseCost(a.EstCostUSD)
	cb, okB := parseCost(b.EstCostUSD)
	switch {
	case !okA:
		return false
	case !okB:
		return true
	}
	return ca.Cmp(cb) < 0
}

// parseCost parses an explanation's cost; "" is unknown.
func parseCost(s string) (usage.Money, bool) {
	if s == "" {
		return usage.Money{}, false
	}
	m, err := usage.ParseMoney(s)
	return m, err == nil
}

// usePlan makes the routed request's served decision rec's plan and sets
// what the response headers, logs and usage event say about it.
func (g *Gateway) usePlan(rec *record, rt *routed) {
	rec.useRoute(rt)
	rec.baseReason = rt.reason
	p := &rec.plan
	p.model, p.pinned = rt.requested, rt.res.Pinned
	p.entries = make([]planEntry, 0, len(rt.served.Plan))
	for _, c := range rt.served.Plan {
		p.entries = append(p.entries, planEntry{
			d: c.Deployment, dropped: c.Dropped,
			health: g.deps[c.Deployment.ID], prov: g.provs[c.Deployment.Provider],
		})
	}
	if len(p.entries) > 0 {
		rec.info.Model = p.entries[0].d.Model.ID
	}
	rec.excluded = len(exclusions(rt.served.Explanation))
	g.decideShadow(rec, rt)
}

// useRoute sets what the response headers, logs and usage event say
// about a request routed as far as rt: its policy, profile and baseline,
// and the cache status the cache layer gave it.
func (rec *record) useRoute(rt *routed) {
	rec.route = rt
	rec.alias = rt.alias
	rec.model = rt.baseline
	rec.inBytes, rec.inImages, rec.estOut = rt.inBytes, rt.inImages, rt.estOut
	rec.profile, rec.routingMode = rt.eff.Profile, rt.eff.RoutingMode
	rec.info = routeInfo(rt)
	rec.info.Cache = rec.cacheHeader()
}

// cacheHeader is the X-Switchyard-Cache response header of the request:
// its cache status when the cache layer saw it, else "".
func (rec *record) cacheHeader() string {
	if !rec.cache.eligible {
		return ""
	}
	return rec.cache.status
}

// routeInfo returns the response headers a routed request carries before
// its first attempt.
func routeInfo(rt *routed) Info {
	info := Info{Alias: rt.alias, Profile: rt.eff.Profile, PolicyWarning: strings.Join(rt.eff.Warnings, "; ")}
	if !rt.auto {
		info.Model = rt.res.Model.ID
	}
	if rt.shadow != nil {
		info.ShadowModel = rt.shadow.Model.ID
	}
	return info
}

// failRoute answers a request that could not be routed, with the headers
// known so far.
func (g *Gateway) failRoute(ex Exchange, rec *record, rt *routed, e *Error) {
	info := Info{}
	if rt != nil {
		rec.route = rt
		rec.alias = rt.alias
		rec.profile, rec.routingMode = rt.eff.Profile, rt.eff.RoutingMode
		info = routeInfo(rt)
		rec.excluded = len(exclusions(rt.served.Explanation))
	}
	info.Cache = rec.cacheHeader()
	rec.info = info
	rec.respondError(ex, info, e)
}
