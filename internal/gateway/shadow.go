package gateway

import (
	"encoding/json"
	"net/http"

	"switchyard/internal/catalog"
	"switchyard/internal/providers"
	"switchyard/internal/reqctx"
	"switchyard/internal/router"
	"switchyard/internal/schema"
	"switchyard/internal/shadow"
	"switchyard/internal/usage"
)

// Shadow sampling (DESIGN §10.7): a deterministic sample of each opted-in
// policy's chat and legacy completion requests is offered, once its
// response is complete, to a shadow runner that asks a comparison
// deployment for a second answer and has the judge compare the two.
//
// Whether a request is sampled is decided right after routing, so that
// only sampled streams collect their answer text; unsampled requests cost
// nothing beyond one comparison. The sample is offered in record.finish,
// after the client has its whole response, budget settlement and the usage
// event; Offer never blocks.

// Shadow takes the samples of shadow sampling. *shadow.Runner implements
// it.
type Shadow interface {
	// Offer queues s for evaluation without blocking, and reports whether
	// it was queued.
	Offer(s shadow.Sample) bool
}

// MaxShadowAnswerBytes bounds the served answer text a sampled request
// collects for the judge. A longer answer is not offered: judging a
// truncated answer would be unfair to it.
const MaxShadowAnswerBytes = 64 << 10

// finishContentFilter is the finish_reason of an answer a content filter
// stopped; such answers are not offered.
const finishContentFilter = "content_filter"

// SetShadow makes s take the shadow samples of policies with
// shadow.sample_rate > 0. It must be called before the gateway serves
// traffic. Nil turns shadow sampling off.
func (g *Gateway) SetShadow(s Shadow) { g.shadow = s }

// Adapter returns the adapter of the provider with the given id, or nil
// when the provider's type is not supported.
func (g *Gateway) Adapter(provider string) providers.Adapter {
	if a, ok := g.adapters[provider]; ok {
		return a
	}
	return nil
}

// Catalog returns the gateway's catalog.
func (g *Gateway) Catalog() *catalog.Catalog { return g.catalog }

// shadowState is what a sampled request collects for its sample.
type shadowState struct {
	// sampled is set when the request is in its policy's shadow sample.
	sampled bool
	// text collects a sampled stream's answer; body is a sampled
	// non-streaming answer's upstream body.
	text *shadow.TextBuilder
	body []byte
	// req is the chat request the last attempt sent, after dropped
	// parameters, and orig the request before them.
	req, orig *schema.ChatRequest
	// native is set when the last attempt was a native legacy completion,
	// whose answer is not a chat answer and is not offered.
	native bool
	// filtered is set when a stream chunk finished with content_filter.
	filtered bool
}

// decideShadow decides, right after routing, whether rec is in its
// policy's shadow sample: shadow sampling is on, the request is
// authenticated and served for real, its policy samples at a rate above 0,
// and the request ID is in that sample (shadow.Sampled). Only requests
// where the gateway chose the model are sampled, since only they have
// savings to verify: an observe-mode request whose router choice is
// missing or the served model itself is not sampled, and neither is an
// enforce-mode request pinned to a concrete model that every planned
// deployment serves as is (see offerShadow).
func (g *Gateway) decideShadow(rec *record, rt *routed) {
	rate := rt.eff.ShadowSampleRate
	switch {
	case g.shadow == nil || rec.dryRun || !rec.authenticated || !(rate > 0):
		return
	case rt.observe() && (rt.shadow == nil || rt.baseline == nil || rt.shadow.Model.ID == rt.baseline.ID):
		return
	case !rt.observe() && !rt.auto && !substitutes(rt):
		return
	case !shadow.Sampled(reqctx.RequestID(rec.ctx), rate):
		return
	}
	rec.shadow.sampled = true
	if rec.stream {
		rec.shadow.text = &shadow.TextBuilder{MaxBytes: MaxShadowAnswerBytes}
	}
}

// collectShadowChunk adds a sampled stream's chunk to its answer text.
func (r *record) collectShadowChunk(c *schema.ChatChunk) {
	s := &r.shadow
	if s.text == nil {
		return
	}
	s.text.Add(c)
	if !s.filtered && contentFiltered(c.Choices) {
		s.filtered = true
	}
}

// offerShadow offers a sampled request to the shadow runner, once its
// response is complete (see record.finish). It offers only a request that
// an upstream served with status 200 and a clean end, whose answer is a
// chat answer that no content filter stopped and that fits
// MaxShadowAnswerBytes, and whose comparison deployment is another model
// than the one that served it.
//
// The comparison is, in enforce mode, the reference model at its cheapest
// healthy, priced deployment that the policy permits, and the served
// answer is the candidate: for auto, the policy's baseline_model; for a
// request the gateway served with an equivalent substitute, the model the
// client asked for; a request served by the model it named is not offered.
// In observe mode, it is the deployment the router would have chosen,
// whose answer is the candidate against the served one, sent the request
// without the parameters the router dropped for it.
func (r *record) offerShadow() {
	s := &r.shadow
	rt := r.route
	d := r.deployment
	switch {
	case !s.sampled || r.dryRun || !r.authenticated || !r.served || rt == nil || d == nil:
		return
	case r.status != http.StatusOK || r.errorCode != "" || s.native || s.filtered || s.req == nil:
		return
	}
	var text string
	if r.stream {
		if s.text == nil || s.text.Truncated() {
			return
		}
		text = s.text.String()
	} else {
		var body struct {
			Choices []json.RawMessage `json:"choices"`
		}
		if json.Unmarshal(s.body, &body) != nil || contentFiltered(body.Choices) {
			return
		}
		t, ok := shadow.ResponseText(s.body)
		if !ok || len(t) > MaxShadowAnswerBytes {
			return
		}
		text = t
	}

	mode, req := shadow.ModeEnforce, s.req
	var comparison *catalog.Deployment
	switch {
	case rt.observe():
		mode, comparison = shadow.ModeObserve, rt.shadow
		if s.orig != nil && rt.fullOK {
			req, _ = s.orig.Without(dropFields(rt.full.Plan[0].Dropped)...)
		}
	case !rt.auto && d.Model.ID == rt.baseline.ID:
		return // pinned: the client chose the model
	default:
		comparison = r.g.baselineDeployment(r, rt)
	}
	if comparison == nil || comparison.Model.ID == d.Model.ID {
		return
	}
	eff := rt.eff
	r.g.shadow.Offer(shadow.Sample{
		RequestID: reqctx.RequestID(r.ctx), Tenant: r.tenant, KeyID: r.keyID, Time: r.start,
		Request:  req,
		Category: rt.assessment.Category, Difficulty: rt.assessment.Difficulty,
		Served:     shadow.Answer{Deployment: d.ID, Text: text},
		Comparison: comparison,
		Mode:       mode, Profile: rt.eff.Profile, Policy: &eff,
	})
}

// baselineDeployment returns the deployment that gives an enforce-mode
// sample's reference answer: the reference model rt.baseline (the
// policy's baseline_model for auto, the requested model otherwise) at its
// cheapest deployment, priced at the request's estimate, that has an adapter, is
// healthy, has known prices, has the capabilities the request needs (other
// than streaming, since the comparison call is not streamed) and that the
// policy permits. It returns nil when there is none.
func (g *Gateway) baselineDeployment(rec *record, rt *routed) *catalog.Deployment {
	if rt.baseline == nil {
		return nil
	}
	res, err := g.catalog.Resolve(rt.baseline.ID)
	if err != nil || res.Model.Type != chatType {
		return nil
	}
	required := rec.shadow.req.RequiredCapabilities()
	delete(required, schema.CapStreaming)
	delete(required, schema.CapStreamUsage)
	tokens := usage.Tokens{Input: rec.estIn(res.Model), Output: rt.routeOut}
	var (
		best     *catalog.Deployment
		bestCost usage.Money
	)
	for _, d := range res.Candidates {
		if g.adapters[d.Provider] == nil || router.PolicyReason(&rt.eff, d) != "" {
			continue
		}
		if h := g.deps[d.ID]; h != nil && h.Skip() != "" {
			continue
		}
		if !hasAll(d, required) {
			continue
		}
		c, ok := usage.Cost(tokens, d.Prices)
		if !ok {
			continue
		}
		if best == nil || c.Cmp(bestCost) < 0 {
			best, bestCost = d, c
		}
	}
	return best
}

// substitutes reports whether the plan of a non-auto request may serve it
// with another model than the requested one (an equivalent substitute).
func substitutes(rt *routed) bool {
	for _, c := range rt.served.Plan {
		if rt.baseline == nil || c.Deployment.Model.ID != rt.baseline.ID {
			return true
		}
	}
	return false
}

// hasAll reports whether d has every capability in required.
func hasAll(d *catalog.Deployment, required schema.CapabilitySet) bool {
	for c := range required {
		if !d.Capabilities.Has(c) {
			return false
		}
	}
	return true
}

// contentFiltered reports whether one of choices, the raw choices of a
// completion or stream chunk, finished with content_filter.
func contentFiltered(choices []json.RawMessage) bool {
	for _, raw := range choices {
		var c struct {
			FinishReason string `json:"finish_reason"`
		}
		if json.Unmarshal(raw, &c) == nil && c.FinishReason == finishContentFilter {
			return true
		}
	}
	return false
}
