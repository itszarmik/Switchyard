// Package router chooses where a chat request goes (DESIGN §9): it builds
// the candidate deployments for the request's mode, applies the hard
// filters (policy, tags, capabilities, context, model type, cost caps,
// health), estimates quality, cost and latency, applies the soft checks
// with relaxation, ranks by preference tier and objective, and plans the
// primary deployment and its fallbacks. Every decision comes with a
// deterministic Explanation.
//
// A Router is built once per catalog snapshot and is safe for concurrent
// use.
package router

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/health"
	"switchyard/internal/policy"
	"switchyard/internal/quality"
	"switchyard/internal/schema"
)

// Candidate modes, as in Explanation.Mode.
const (
	// ModeAuto routes an auto request across every allowed chat model.
	ModeAuto = "auto"
	// ModePinned considers only the requested model's deployments, or
	// the single model@provider deployment.
	ModePinned = "pinned"
	// ModeEquivalent routes a concrete model name across every allowed
	// chat model whose q̂ is within MaxQualityDrop of the requested one's,
	// and at least the policy's quality floor.
	ModeEquivalent = "equivalent"
)

// Relaxations, as in Decision.Relaxed.
const (
	RelaxedLatency = "latency"
	RelaxedQuality = "quality"
)

// noPrior is q for a model with no quality prior for the category or for
// general.
const noPrior = 0.5

// defaultTTFT is est_ttft for a deployment with neither measurements nor a
// latency hint.
const defaultTTFT = 1000 * time.Millisecond

// qualityEpsilon absorbs float rounding in q̂ ≥ bar, so that a q̂ equal to
// the bar on paper is not excluded by 1e-16.
const qualityEpsilon = 1e-9

// Plan lengths: the default and the most routing.fallback.max_deployments
// allows.
const (
	defaultMaxPlan = 3
	maxPlan        = 10
)

// Router plans requests over one catalog snapshot.
type Router struct {
	cat     *catalog.Catalog
	maxPlan int
	deps    []dep          // every deployment, in catalog order
	byID    map[string]int // index into deps
}

// dep is a deployment with what Decide needs precomputed.
type dep struct {
	d               *catalog.Deployment
	chat            bool
	inNano, outNano uint64 // nano-USD per million tokens; 0 if unset
	health          *health.Deployment
	provider        *health.Provider
}

// New returns a Router over cat. tracker supplies breaker, cooldown,
// concurrency and latency state: the gateway's Tracker, or a Batch of it
// for a snapshot not yet in force. New takes every deployment's and
// provider's handle from it, as the gateway does. A nil tracker means
// every deployment is healthy and latency estimates come from the
// catalog's hints, which suits offline explanations. cfg supplies
// fallback.max_deployments; 0 means 3.
func New(cat *catalog.Catalog, tracker health.Registry, cfg config.Routing) *Router {
	r := &Router{cat: cat, maxPlan: cfg.Fallback.MaxDeployments, byID: map[string]int{}}
	if r.maxPlan <= 0 {
		r.maxPlan = defaultMaxPlan
	}
	r.maxPlan = min(r.maxPlan, maxPlan)
	for _, m := range cat.Models() {
		for _, d := range m.Deployments {
			x := dep{d: d, chat: m.Type == config.ModelTypeChat}
			x.inNano, _ = nanoPerMillion(d.Prices.Input)
			x.outNano, _ = nanoPerMillion(d.Prices.Output)
			if tracker != nil {
				x.health = tracker.Deployment(d.ID, d.Provider,
					health.LatencyHint{TTFT: d.LatencyHint.TTFT, TokensPerSec: d.LatencyHint.TokensPerSec})
				x.provider = tracker.Provider(d.Provider, d.ProviderMaxConcurrency)
			}
			r.byID[d.ID] = len(r.deps)
			r.deps = append(r.deps, x)
		}
	}
	return r
}

// Input is one request to route.
type Input struct {
	// Requested is the client's model field after alias resolution, such
	// as "auto:cheap", "gpt-6-luna" or "m@p". It names the request in
	// errors.
	Requested string
	// Resolution is the catalog's resolution of a concrete model; zero for
	// auto.
	Resolution catalog.Resolution
	// Auto is true for "auto" and "auto:<profile>".
	Auto bool
	// Required are the capabilities the request needs.
	Required schema.CapabilitySet
	// InputBytes is the size of the request's input in bytes, without the
	// bytes of inline images, and ImageTokens the token estimate of its
	// images. A candidate's input estimate is InputTokens(InputBytes, the
	// model's token_ratio) + ImageTokens, for its cost and its context
	// check, so that each model is priced at its own tokenization.
	InputBytes, ImageTokens int64
	// EstOut is the output token estimate for the cost estimates (DESIGN
	// §9.1: the smaller of max_tokens and routing.expected_output_tokens).
	EstOut int64
	// ContextOut, when greater than EstOut, is the output the context
	// filter reserves room for instead: max_tokens when the request sets
	// it, so that a deployment whose context window cannot hold the
	// output the client allows is excluded.
	ContextOut int64
	// Drop lets deployments that lack droppable capabilities through, with
	// the parameters dropped (routing.drop_unsupported_params).
	Drop bool
	// Assessment is the request's category and difficulty.
	Assessment quality.Assessment
	// Policy is the effective policy, with Profile and Constraints final
	// (rules, headers and budget downgrade applied).
	Policy policy.Effective
	// BudgetRemainingMicro is the least remaining budget of the request's
	// scopes in micro-USD; -1 (any negative value) means unknown or no
	// budget. 0 means exhausted: only free deployments fit.
	BudgetRemainingMicro int64
	// Keep, if set, excludes deployments the gateway itself cannot use: it
	// returns the reason, or "" to keep the deployment. It runs before
	// every other filter.
	Keep func(d *catalog.Deployment) string
	// Downgrade, if set, explains a budget soft-threshold profile
	// downgrade; it is copied to Explanation.Downgrade.
	Downgrade string
}

// InputTokens is the input-token estimate of n bytes of text for a model
// with the given token_ratio (DESIGN §9.1): n/4, scaled by the ratio. The
// gateway's budget estimates use it too, so that they agree with the
// router's.
func InputTokens(n int64, ratio float64) int64 {
	return int64(float64(max(n, 0)) / 4 * ratio)
}

// ImageTokens is the input-token estimate of one image part, whatever its
// size: providers bill an image by its resolution (about 1,600 tokens for
// a large one), not by the bytes of its encoding. The gateway's estimates
// and InputTokenBound both use it.
const ImageTokens = 1600

// InputTokenBound bounds the input tokens of req for the cost caps of
// background calls (shadow sampling, switchyard eval and the judge): one
// token per byte of the encoded request, which no tokenizer exceeds for
// text and which leaves room for the per-message framing, plus
// ImageTokens per image part in place of its inline bytes. Provider-side
// tokens the request does not show, such as a hidden system prompt, are
// not counted.
func InputTokenBound(req *schema.ChatRequest) int64 {
	body := len(req.EncodeFor(req.Model(), false))
	images, inline := req.ImageParts()
	return int64(max(body-inline, 0)) + int64(images)*ImageTokens
}

// Decision is the outcome of Decide.
type Decision struct {
	// Plan is the deployments to try, primary first, each with the
	// capabilities whose parameters are dropped for it. Empty when
	// Unhealthy.
	Plan []catalog.Candidate
	// Relaxed lists the soft checks dropped to find a candidate:
	// RelaxedLatency, then RelaxedQuality.
	Relaxed []string
	// Explanation explains the decision.
	Explanation Explanation
	// Unhealthy reports that every candidate that passed the other hard
	// filters was skipped for health (breaker, cooldown or provider
	// concurrency): the gateway answers 503 no_healthy_upstream.
	Unhealthy bool
}

// state is the working state of one candidate.
type state struct {
	dep     int // index into Router.deps
	reason  string
	hard    bool // passed every hard filter
	q       float64
	noPrior bool
	ttft    time.Duration
	noTTFT  bool
	cost    femto
	tier    int
	ranked  catalog.Candidate
	rank    int
}

// Decide routes in. It returns the decision, whose Explanation is filled
// even when it returns an error.
//
// When no candidate passes the hard filters, or none meets the soft
// constraints and the policy's on_unmet is reject, the error is a
// *catalog.NoEligibleError with one exclusion per candidate (the gateway
// answers 400 no_eligible_model). When candidates pass every hard filter
// but health, the decision is Unhealthy and the error nil.
func (r *Router) Decide(in Input) (Decision, error) {
	pol := &in.Policy
	mode := ModeAuto
	switch {
	case in.Auto:
	case in.Resolution.Model == nil:
		return Decision{}, errors.New("router: a concrete request needs a catalog resolution")
	case !in.Resolution.Pinned && pol.Substitution == policy.SubstitutionEquivalent:
		mode = ModeEquivalent
	default:
		mode = ModePinned
	}

	// 1. Candidates.
	var cands []state
	if mode == ModePinned {
		cands = make([]state, 0, len(in.Resolution.Candidates))
		for _, d := range in.Resolution.Candidates {
			i, ok := r.byID[d.ID]
			if !ok {
				return Decision{}, fmt.Errorf("router: deployment %q is not in the catalog", d.ID)
			}
			cands = append(cands, state{dep: i})
		}
	} else {
		cands = make([]state, len(r.deps))
		for i := range cands {
			cands[i].dep = i
		}
	}

	estOut := max(in.EstOut, 0)
	imageTokens := max(in.ImageTokens, 0)
	estIn := func(ratio float64) int64 { return InputTokens(in.InputBytes, ratio) + imageTokens }
	a := in.Assessment
	bar := 0.0
	switch mode {
	case ModeAuto:
		bar = pol.Constraints.MinQuality
	case ModeEquivalent:
		// The profile's bar does not apply, but a rule's or a header's
		// min_quality does.
		q, _ := prior(in.Resolution.Model, a.Category)
		bar = max(quality.Estimate(q, 0, a)-pol.MaxQualityDrop, pol.Constraints.QualityFloor, 0)
	}

	// 2. Hard filters before the catalog's: the gateway's own, model type,
	// policy allow and deny, required tags. 3. Estimates, for every
	// candidate, so that the explanation shows them for excluded ones too.
	survivors := make([]*catalog.Deployment, 0, len(cands))
	for k := range cands {
		c := &cands[k]
		x := &r.deps[c.dep]
		c.tier = tier(pol.Prefer, x.d.ID)
		if in.Keep != nil {
			c.reason = in.Keep(x.d)
		}
		if c.reason == "" {
			c.reason = r.policyReason(x, pol)
		}
		if c.reason == "" {
			survivors = append(survivors, x.d)
		}
		c.estimate(x, a, estIn(x.d.Model.TokenRatio), estOut)
	}

	// The catalog's capability and context filters, once per token ratio
	// among the survivors, since the context check needs each model's own
	// input estimate.
	var ratios [4]float64
	distinct := ratios[:0]
	for _, d := range survivors {
		if !slices.Contains(distinct, d.Model.TokenRatio) {
			distinct = append(distinct, d.Model.TokenRatio)
		}
	}
	// In auto and equivalent mode a candidate's position is its
	// deployment's index; a pinned request has few candidates.
	pos := func(id string) *state {
		i := r.byID[id]
		if mode != ModePinned {
			return &cands[i]
		}
		for k := range cands {
			if cands[k].dep == i {
				return &cands[k]
			}
		}
		return nil // unreachable: Rank returns only the survivors
	}
	for _, ratio := range distinct {
		group := survivors
		if len(distinct) > 1 {
			group = make([]*catalog.Deployment, 0, len(survivors))
			for _, d := range survivors {
				if d.Model.TokenRatio == ratio {
					group = append(group, d)
				}
			}
		}
		ranking, err := r.cat.Rank(catalog.Resolution{Model: in.Resolution.Model, Candidates: group}, in.Required, estIn(ratio), max(estOut, in.ContextOut), in.Drop)
		var ne *catalog.NoEligibleError
		switch {
		case errors.As(err, &ne):
			ranking.Exclusions = ne.Exclusions
		case err != nil:
			return Decision{}, fmt.Errorf("router: %w", err)
		}
		for _, x := range ranking.Exclusions {
			if c := pos(x.Deployment); c != nil {
				c.reason = x.Reason
			}
		}
		for _, rc := range ranking.Candidates {
			if c := pos(rc.Deployment.ID); c != nil {
				c.ranked, c.hard = rc, true
			}
		}
	}

	// Cost caps, then health, which comes last so that it is read only for
	// candidates that could otherwise take the request.
	capFemto, capKind := costCap(pol.MaxCostPerRequestNano, in.BudgetRemainingMicro)
	capText := ""
	if capKind != "" {
		capText = capKind + " " + capFemto.String() + " USD"
	}
	anyHealthOnly := false
	for k := range cands {
		c := &cands[k]
		if !c.hard {
			continue
		}
		x := &r.deps[c.dep]
		if capKind != "" {
			if c.reason = capReason(x.d.Pricing.Known, c.cost, capFemto, capText); c.reason != "" {
				c.hard = false
				continue
			}
		}
		if reason := healthReason(x); reason != "" {
			c.reason, c.hard = reason, false
			anyHealthOnly = true
		}
	}

	dec := Decision{}
	exp := &dec.Explanation
	*exp = Explanation{
		Policy:             pol.PolicyID,
		MatchedRule:        pol.MatchedRule,
		MatchedQualityRule: pol.MatchedQualityRule,
		Profile:            pol.Profile,
		Mode:               mode,
		Downgrade:          in.Downgrade,
		Assessment: AssessmentInfo{
			Category:   a.Category,
			Difficulty: round4(a.Difficulty),
			Signals:    nonNil(a.Signals),
			Hinted:     a.Hinted,
		},
		Estimates:     Estimates{InputTokens: estIn(requestedRatio(in)), OutputTokens: estOut},
		QualityBar:    round4(bar),
		Plan:          []string{},
		Relaxed:       []string{},
		DroppedParams: []string{},
		Warnings:      nonNil(pol.Warnings),
	}

	hard := 0
	for k := range cands {
		if cands[k].hard {
			hard++
		}
	}
	if hard == 0 {
		r.explainCandidates(exp, cands)
		if anyHealthOnly {
			dec.Unhealthy = true
			return dec, nil
		}
		return dec, r.noEligible(in, cands)
	}

	// 4. Soft checks.
	passQ := func(c *state) bool { return mode == ModePinned || c.q >= bar-qualityEpsilon }
	maxTTFT := pol.Constraints.MaxTTFT
	passL := func(c *state) bool { return maxTTFT <= 0 || c.ttft <= maxTTFT }
	eligible := make([]int, 0, hard)
	collect := func(ok func(*state) bool) {
		eligible = eligible[:0]
		for k := range cands {
			if cands[k].hard && ok(&cands[k]) {
				eligible = append(eligible, k)
			}
		}
	}
	collect(func(c *state) bool { return passQ(c) && passL(c) })
	qualityRelaxed := false
	if len(eligible) == 0 {
		if pol.OnUnmet == policy.OnUnmetReject {
			for k := range cands {
				if c := &cands[k]; c.hard {
					c.reason = softReason(c, passQ(c), bar, maxTTFT)
				}
			}
			r.explainCandidates(exp, cands)
			return dec, r.noEligible(in, cands)
		}
		latencyFailed := false
		for k := range cands {
			if c := &cands[k]; c.hard && !passL(c) {
				latencyFailed = true
			}
		}
		if latencyFailed {
			dec.Relaxed = append(dec.Relaxed, RelaxedLatency)
		}
		collect(passQ)
		if len(eligible) == 0 {
			dec.Relaxed = append(dec.Relaxed, RelaxedQuality)
			qualityRelaxed = true
			collect(func(*state) bool { return true })
		}
	}
	relaxedLatency := slices.Contains(dec.Relaxed, RelaxedLatency)
	for k := range cands {
		c := &cands[k]
		if !c.hard {
			continue
		}
		okQ, okL := passQ(c) || qualityRelaxed, passL(c) || relaxedLatency
		if !okQ || !okL {
			c.reason = softReason(c, okQ, bar, maxTTFT)
		}
	}

	// 5. Ranking.
	latency := pol.Constraints.Objective == policy.ObjectiveLatency
	slices.SortFunc(eligible, func(i, j int) int {
		a, b := &cands[i], &cands[j]
		if qualityRelaxed {
			if a.q != b.q {
				if a.q > b.q {
					return -1
				}
				return 1
			}
		} else if a.tier != b.tier {
			return a.tier - b.tier
		}
		byCost, byTTFT := r.costOrder(a, b), compareDuration(a.ttft, b.ttft)
		if latency {
			byCost, byTTFT = byTTFT, byCost
		}
		if byCost != 0 {
			return byCost
		}
		if byTTFT != 0 {
			return byTTFT
		}
		return a.dep - b.dep
	})
	for n, k := range eligible {
		cands[k].rank = n + 1
	}

	// 6. Plan, preferring provider diversity.
	limit := min(r.maxPlan, len(eligible))
	if mode == ModePinned && in.Resolution.Pinned {
		limit = 1
	}
	dec.Plan = make([]catalog.Candidate, 0, limit)
	for len(dec.Plan) < limit {
		pick := -1
		for _, k := range eligible {
			d := cands[k].ranked.Deployment
			if planHas(dec.Plan, func(p *catalog.Candidate) bool { return p.Deployment == d }) {
				continue
			}
			if pick < 0 {
				pick = k // the best remaining, if no provider is new
			}
			if !planHas(dec.Plan, func(p *catalog.Candidate) bool { return p.Deployment.Provider == d.Provider }) {
				pick = k
				break
			}
		}
		if pick < 0 {
			break
		}
		dec.Plan = append(dec.Plan, cands[pick].ranked)
	}

	// 7. Explanation.
	r.explainCandidates(exp, cands)
	for _, p := range dec.Plan {
		exp.Plan = append(exp.Plan, p.Deployment.ID)
	}
	exp.Relaxed = append(exp.Relaxed, dec.Relaxed...)
	for _, c := range dec.Plan[0].Dropped {
		exp.DroppedParams = append(exp.DroppedParams, string(c))
	}
	return dec, nil
}

// estimate fills the cost estimate, and for a chat model q̂ and est_ttft.
func (c *state) estimate(x *dep, a quality.Assessment, estIn, estOut int64) {
	if x.d.Pricing.Known {
		c.cost = mulFemto(uint64(max(estIn, 0)), x.inNano).add(mulFemto(uint64(max(estOut, 0)), x.outNano))
	}
	if !x.chat {
		return
	}
	q, ok := prior(x.d.Model, a.Category)
	c.q, c.noPrior = quality.Estimate(q, x.d.QualityOffset, a), !ok
	if x.health != nil {
		c.ttft = x.health.EstimatedTTFT()
	} else {
		c.ttft = x.d.LatencyHint.TTFT
	}
	if c.ttft <= 0 {
		c.ttft, c.noTTFT = defaultTTFT, true
	}
}

// prior returns the model's quality prior for category, falling back to
// general, then to noPrior; ok is false for the last.
func prior(m *catalog.Model, category string) (q float64, ok bool) {
	if q, ok := m.Quality[category]; ok {
		return q, true
	}
	if q, ok := m.Quality[quality.CategoryGeneral]; ok {
		return q, true
	}
	return noPrior, false
}

// policyReason applies the model-type, deny, allow and require_tags
// filters, returning the first reason x is excluded, or "".
func (r *Router) policyReason(x *dep, pol *policy.Effective) string {
	if !x.chat {
		return "not a chat model"
	}
	return PolicyReason(pol, x.d)
}

// PolicyReason applies the policy's deny, allow and require_tags filters
// to d, returning the first reason it is excluded, or "" if the policy
// lets it serve. The gateway applies it to embedding deployments, which
// are never routed across models.
func PolicyReason(pol *policy.Effective, d *catalog.Deployment) string {
	id := d.ID
	for _, g := range pol.Deny {
		if match(g, id) {
			return "denied by policy (deny " + strconv.Quote(g) + ")"
		}
	}
	if len(pol.Allow) > 0 && !slices.ContainsFunc(pol.Allow, func(g string) bool { return match(g, id) }) {
		return "not allowed by policy"
	}
	var missing []string
	for _, t := range pol.RequireTags {
		if !slices.Contains(d.Tags, t) {
			missing = append(missing, t)
		}
	}
	switch len(missing) {
	case 0:
		return ""
	case 1:
		return "lacks required tag " + missing[0]
	default:
		return "lacks required tags " + strings.Join(missing, ", ")
	}
}

// match reports whether glob matches id. A malformed glob, which
// validation rejects, matches nothing.
func match(glob, id string) bool {
	ok, err := path.Match(glob, id)
	return err == nil && ok
}

// tier returns the index of the first prefer glob matching id, or
// len(prefer).
func tier(prefer []string, id string) int {
	for i, g := range prefer {
		if match(g, id) {
			return i
		}
	}
	return len(prefer)
}

// requestedRatio is the token_ratio of the input estimate the explanation
// shows: the requested model's, or 1 for auto.
func requestedRatio(in Input) float64 {
	if in.Auto || in.Resolution.Model == nil {
		return 1
	}
	return in.Resolution.Model.TokenRatio
}

// costCap returns the cost cap in force and its name, or "" for none: the
// lower of the per-request cap in nano-USD (0 means none) and the remaining
// budget in micro-USD (negative means unknown), compared exactly.
func costCap(maxCostNano, budgetMicro int64) (femto, string) {
	capFemto := nanoToFemto(maxCostNano)
	budgetFemto := microToFemto(budgetMicro)
	switch {
	case maxCostNano > 0 && (budgetMicro < 0 || capFemto.cmp(budgetFemto) <= 0):
		return capFemto, "max_cost_per_request"
	case budgetMicro >= 0:
		return budgetFemto, "remaining budget"
	}
	return femto{}, ""
}

// capReason returns why a candidate whose estimated cost is cost (when its
// pricing is known) is excluded by the cost cap capFemto, named capText,
// or "" if it fits.
func capReason(known bool, cost, capFemto femto, capText string) string {
	switch {
	case !known:
		return "cost unknown (pricing not set) under " + capText
	case cost.cmp(capFemto) > 0:
		return "est cost " + cost.String() + " USD > " + capText
	}
	return ""
}

// CostCapReason returns why the per-request cost cap of pol excludes d for
// a request of estIn input and estOut output tokens, in the words Decide
// uses, or "" when it fits or pol sets no cap. The gateway applies it to
// embedding deployments, which Decide does not route.
func CostCapReason(pol *policy.Effective, d *catalog.Deployment, estIn, estOut int64) string {
	capFemto, capKind := costCap(pol.MaxCostPerRequestNano, -1)
	if capKind == "" {
		return ""
	}
	var cost femto
	if d.Pricing.Known {
		in, _ := nanoPerMillion(d.Prices.Input)
		out, _ := nanoPerMillion(d.Prices.Output)
		cost = mulFemto(uint64(max(estIn, 0)), in).add(mulFemto(uint64(max(estOut, 0)), out))
	}
	return capReason(d.Pricing.Known, cost, capFemto, capKind+" "+capFemto.String()+" USD")
}

// healthReason returns why x cannot take a new request now, or "".
func healthReason(x *dep) string {
	if x.health != nil {
		if reason := x.health.Skip(); reason != "" {
			return reason
		}
	}
	if x.provider != nil && x.provider.IsSaturated() {
		return "provider at max_concurrency " + strconv.Itoa(x.provider.Max())
	}
	return ""
}

// softReason formats why c failed a soft check: quality when okQ is false,
// else latency.
func softReason(c *state, okQ bool, bar float64, maxTTFT time.Duration) string {
	if !okQ {
		q, b := formatPair(c.q, bar)
		return "quality " + q + " < " + b
	}
	return "est ttft " + strconv.FormatInt(c.ttft.Milliseconds(), 10) + "ms > " + strconv.FormatInt(maxTTFT.Milliseconds(), 10) + "ms"
}

// costOrder orders a before b by estimated cost: a known price before an
// unknown one, then the lower cost.
func (r *Router) costOrder(a, b *state) int {
	ka, kb := r.deps[a.dep].d.Pricing.Known, r.deps[b.dep].d.Pricing.Known
	switch {
	case ka != kb && ka:
		return -1
	case ka != kb:
		return 1
	case !ka:
		return 0
	}
	return a.cost.cmp(b.cost)
}

// compareDuration returns -1, 0 or +1 as a is shorter, equal or longer.
func compareDuration(a, b time.Duration) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// planHas reports whether a plan entry satisfies f.
func planHas(plan []catalog.Candidate, f func(*catalog.Candidate) bool) bool {
	for i := range plan {
		if f(&plan[i]) {
			return true
		}
	}
	return false
}

// explainCandidates fills exp.Candidates, in candidate (catalog) order.
func (r *Router) explainCandidates(exp *Explanation, cands []state) {
	exp.Candidates = make([]CandidateInfo, len(cands))
	for k := range cands {
		c := &cands[k]
		x := &r.deps[c.dep]
		info := CandidateInfo{
			Deployment: x.d.ID,
			Model:      x.d.Model.ID,
			Tier:       c.tier,
			Eligible:   c.rank > 0,
			Rank:       c.rank,
		}
		if !info.Eligible {
			info.ExcludedBy = c.reason
		}
		if x.chat {
			info.Q = round4(c.q)
			info.EstTTFTMs = c.ttft.Milliseconds()
			if c.noPrior {
				info.Notes = append(info.Notes, NoteNoQualityPrior)
			}
			if c.noTTFT {
				info.Notes = append(info.Notes, NoteNoLatency)
			}
		}
		if x.d.Pricing.Known {
			info.EstCostUSD = c.cost.String()
		} else {
			info.Notes = append(info.Notes, NotePricingUnknown)
		}
		exp.Candidates[k] = info
	}
}

// noEligible returns the error for a request no candidate may serve: one
// exclusion per candidate, in candidate order.
func (r *Router) noEligible(in Input, cands []state) error {
	name := in.Requested
	if name == "" && in.Resolution.Model != nil {
		name = in.Resolution.Model.ID
	}
	e := &catalog.NoEligibleError{Model: name, Exclusions: make([]catalog.Exclusion, len(cands))}
	for k := range cands {
		e.Exclusions[k] = catalog.Exclusion{Deployment: r.deps[cands[k].dep].d.ID, Reason: cands[k].reason}
	}
	return e
}

// nonNil returns s, or an empty slice for nil, so that JSON shows [].
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return slices.Clone(s)
}
