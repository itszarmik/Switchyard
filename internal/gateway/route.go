package gateway

import (
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"

	"switchyard/internal/apierror"
	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/health"
	"switchyard/internal/schema"
)

// defaultEstOut is the output estimate, in tokens, for a request that sets
// no max_tokens (DESIGN §9.1).
const defaultEstOut = 500

// droppedFields maps each droppable capability to the request parameters
// that need it. When the chosen deployment lacks the capability (and it may
// be dropped), these parameters are removed before forwarding.
var droppedFields = map[schema.Capability][]string{
	schema.CapSampling:      {"temperature", "top_p"},
	schema.CapTemperature2:  {"temperature"},
	schema.CapN:             {"n"},
	schema.CapLogprobs:      {"logprobs", "top_logprobs"},
	schema.CapSeed:          {"seed"},
	schema.CapPenalties:     {"frequency_penalty", "presence_penalty"},
	schema.CapParallelTools: {"parallel_tool_calls"},
}

// dropFields returns the parameters to remove for the dropped capabilities,
// without duplicates.
func dropFields(dropped []schema.Capability) []string {
	var out []string
	for _, c := range dropped {
		for _, f := range droppedFields[c] {
			if !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	return out
}

// resolve maps the client's model field to its catalog resolution and
// checks the model's type against the endpoint's.
func (g *Gateway) resolve(name, endpoint, wantType string) (catalog.Resolution, *Error) {
	res, err := g.catalog.Resolve(name)
	switch {
	case errors.Is(err, catalog.ErrVirtualModel):
		return res, virtualModel(name)
	case err != nil:
		return res, modelNotFound(name)
	case res.Model.Type != wantType:
		return res, wrongModelType(name, res.Model.Type, endpoint)
	}
	return res, nil
}

// imageTokens is the input-token estimate of one image part, whatever its
// size: providers bill an image by its resolution (about 1,600 tokens for
// a large one), not by the bytes of its encoding.
const imageTokens = 1600

// estimateIn is the input-token estimate of a request body of n bytes, of
// which inline bytes are image data in images image parts: the text at
// bytes/4, scaled by the model's token ratio, plus imageTokens per image.
func estimateIn(n, images, inline int, m *catalog.Model) int64 {
	text := max(n-inline, 0)
	return int64(float64(text)/4*m.TokenRatio) + int64(images)*imageTokens
}

// maxEstOut bounds the output-token estimate. It is far above any context
// window, so a larger max_tokens is excluded by the context filter all the
// same, and sums and products of estimates cannot overflow.
const maxEstOut = math.MaxInt32

// estimateOut is the output-token estimate of a request that generates
// outputs completions: per completion, the request's cap if it sets one,
// else defaultEstOut; in all, at most maxEstOut. outputs below 1 count as
// 1.
func estimateOut(maxTokens int64, ok bool, outputs int64) int64 {
	per := int64(defaultEstOut)
	if ok && maxTokens > 0 {
		per = min(maxTokens, maxEstOut)
	}
	outputs = max(outputs, 1)
	if per > maxEstOut/outputs {
		return maxEstOut
	}
	return per * outputs
}

// chatOutputs is the number of completions a chat request generates: n,
// at least 1.
func chatOutputs(req *schema.ChatRequest) int64 {
	n, _ := req.N()
	return int64(max(n, 1))
}

// completionOutputs is the number of completions a legacy completion
// request generates: max(n, best_of, 1) for each of its prompts.
func completionOutputs(req *schema.CompletionRequest) int64 {
	n, _ := req.N()
	bestOf, _ := req.BestOf()
	return int64(max(n, bestOf, 1)) * int64(max(len(req.Prompts()), 1))
}

// filter returns why the gateway itself cannot use deployment d for a
// request, or "" if it can. It runs before the catalog's hard filters.
type filter func(d *catalog.Deployment) string

// maxPlan is the longest plan, routing.fallback.max_deployments at most.
const maxPlan = 10

// planEntry is one deployment of a plan.
type planEntry struct {
	d *catalog.Deployment
	// dropped lists the capabilities whose parameters are removed for d.
	dropped []schema.Capability
	health  *health.Deployment
	prov    *health.Provider
}

// plan is the ordered list of deployments a request may be sent to: the
// primary and its fallbacks.
type plan struct {
	entries []planEntry
	model   string
	pinned  bool
	// exclusions explains every candidate that is not in the plan because
	// it is ineligible or unhealthy, in candidate order.
	exclusions []catalog.Exclusion
}

// buildPlan plans a request among res.Candidates:
//
//  1. candidates that fail keep (or have no adapter) are excluded, then
//     the catalog's hard filters; with nothing left the answer is 400
//     no_eligible_model;
//  2. eligible candidates whose breaker is open, that are in a rate-limit
//     cooldown or whose provider is at max_concurrency are skipped; with
//     nothing left the answer is 503 no_healthy_upstream;
//  3. the plan takes the cheapest remaining candidate, then up to
//     max_deployments−1 fallbacks, each the cheapest remaining candidate
//     of a provider not yet in the plan while there is one.
//
// Every excluded candidate is reported, in candidate order, in
// plan.exclusions or in the error.
//
// The plan is built into p, which the request's record holds.
func (g *Gateway) buildPlan(p *plan, res catalog.Resolution, required schema.CapabilitySet, estIn, estOut int64, keep filter) *Error {
	// Candidates excluded before the catalog's filters. The candidate
	// list is copied only when one is.
	var pre []catalog.Exclusion
	cands := res.Candidates
	for i, d := range res.Candidates {
		reason := ""
		if g.adapters[d.Provider] == nil {
			reason = "provider type " + d.ProviderType + " is not supported"
		} else if keep != nil {
			reason = keep(d)
		}
		switch {
		case reason != "":
			if len(pre) == 0 {
				cands = slices.Clone(res.Candidates[:i])
			}
			pre = append(pre, catalog.Exclusion{Deployment: d.ID, Reason: reason})
		case len(pre) > 0:
			cands = append(cands, d)
		}
	}
	sub := res
	sub.Candidates = cands
	ranking, err := g.catalog.Rank(sub, required, estIn, estOut, g.drop)
	p.model, p.pinned = res.Model.ID, res.Pinned
	if err != nil {
		var ne *catalog.NoEligibleError
		if !errors.As(err, &ne) {
			return &Error{Status: http.StatusInternalServerError, Type: apierror.TypeServer, Code: "internal_error", Message: "Could not choose a deployment."}
		}
		p.exclusions = merge(res.Candidates, pre, ne.Exclusions)
		return noEligibleModel(res.Model.ID, p.exclusions)
	}

	// Health: skip what cannot take the request now. The skip reasons
	// join the exclusions.
	var healthy [maxCandidates]int
	nh := 0
	var skipped []catalog.Exclusion
	for i, c := range ranking.Candidates {
		h, pv := g.deps[c.Deployment.ID], g.provs[c.Deployment.Provider]
		reason := h.Skip()
		if reason == "" && pv.Saturated() {
			reason = "provider at max_concurrency " + strconv.Itoa(pv.Max())
		}
		if reason != "" {
			skipped = append(skipped, catalog.Exclusion{Deployment: c.Deployment.ID, Reason: reason})
			continue
		}
		if nh < len(healthy) {
			healthy[nh] = i
			nh++
		}
	}
	p.exclusions = merge(res.Candidates, pre, ranking.Exclusions, skipped)
	if nh == 0 {
		return noHealthyUpstream(res.Model.ID, p.exclusions)
	}

	// The plan, preferring provider diversity.
	limit := min(g.routing.Fallback.MaxDeployments, nh, maxPlan)
	if res.Pinned {
		limit = 1
	}
	p.entries = make([]planEntry, 0, limit)
	for len(p.entries) < limit {
		var pick *catalog.Candidate
		for _, idx := range healthy[:nh] {
			c := &ranking.Candidates[idx]
			if p.has(c.Deployment) {
				continue
			}
			if pick == nil {
				pick = c // the cheapest remaining, if no provider is new
			}
			if !p.hasProvider(c.Deployment.Provider) {
				pick = c
				break
			}
		}
		if pick == nil {
			break
		}
		p.entries = append(p.entries, planEntry{
			d: pick.Deployment, dropped: pick.Dropped,
			health: g.deps[pick.Deployment.ID], prov: g.provs[pick.Deployment.Provider],
		})
	}
	return nil
}

// maxCandidates bounds the candidates a plan is chosen from; a model with
// more deployments than this plans among its cheapest maxCandidates
// healthy ones.
const maxCandidates = 64

// has reports whether d is in the plan.
func (p *plan) has(d *catalog.Deployment) bool {
	for i := range p.entries {
		if p.entries[i].d == d {
			return true
		}
	}
	return false
}

// hasProvider reports whether the plan already has a deployment of
// provider.
func (p *plan) hasProvider(provider string) bool {
	for i := range p.entries {
		if p.entries[i].d.Provider == provider {
			return true
		}
	}
	return false
}

// merge returns the exclusions of the lists, one per deployment, in the
// order of cands. It returns a single list as is.
func merge(cands []*catalog.Deployment, lists ...[]catalog.Exclusion) []catalog.Exclusion {
	var only []catalog.Exclusion
	n := 0
	for _, l := range lists {
		if len(l) > 0 {
			only = l
			n++
		}
	}
	if n <= 1 {
		return only
	}
	var all []catalog.Exclusion
	for _, l := range lists {
		all = append(all, l...)
	}
	return inCandidateOrder(cands, all)
}

// inCandidateOrder sorts exclusions, one per deployment, into the order of
// cands.
func inCandidateOrder(cands []*catalog.Deployment, exclusions []catalog.Exclusion) []catalog.Exclusion {
	out := make([]catalog.Exclusion, 0, len(exclusions))
	for _, d := range cands {
		for _, x := range exclusions {
			if x.Deployment == d.ID {
				out = append(out, x)
				break
			}
		}
	}
	return out
}

// chatType is the model type of chat and legacy completion requests.
const chatType = config.ModelTypeChat
