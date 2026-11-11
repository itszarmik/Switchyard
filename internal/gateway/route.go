package gateway

import (
	"errors"
	"math"
	"net/http"
	"slices"

	"switchyard/internal/apierror"
	"switchyard/internal/catalog"
	"switchyard/internal/config"
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

// estimateOut is the output-token estimate: the request's cap if it sets
// one, at most maxEstOut, else defaultEstOut.
func estimateOut(maxTokens int64, ok bool) int64 {
	if ok && maxTokens > 0 {
		return min(maxTokens, maxEstOut)
	}
	return defaultEstOut
}

// filter returns why the gateway itself cannot use deployment d for a
// request, or "" if it can. It runs before the catalog's hard filters.
type filter func(d *catalog.Deployment) string

// choose picks the deployment that serves a request among res.Candidates:
// first those that pass keep (and have an adapter), then the catalog's
// hard filters and ranking. Every excluded candidate is reported, in
// candidate order, in Selection.Exclusions or in the no_eligible_model
// error.
func (g *Gateway) choose(res catalog.Resolution, required schema.CapabilitySet, estIn, estOut int64, keep filter) (catalog.Selection, *Error) {
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
	sel, err := g.catalog.Select(sub, required, estIn, estOut, g.drop)
	var ne *catalog.NoEligibleError
	if err != nil && !errors.As(err, &ne) {
		return sel, &Error{Status: http.StatusInternalServerError, Type: apierror.TypeServer, Code: "internal_error", Message: "Could not choose a deployment."}
	}
	exclusions := sel.Exclusions
	if ne != nil {
		exclusions = ne.Exclusions
	}
	if len(pre) > 0 {
		exclusions = inCandidateOrder(res.Candidates, append(pre, exclusions...))
	}
	if ne != nil {
		return catalog.Selection{Exclusions: exclusions}, noEligibleModel(res.Model.ID, exclusions)
	}
	sel.Exclusions = exclusions
	return sel, nil
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
