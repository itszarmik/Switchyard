package gateway

import (
	"context"
	"strings"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/policy"
	"switchyard/internal/providers"
	"switchyard/internal/router"
	"switchyard/internal/schema"
)

// reasonNoEmbeddings excludes a deployment whose adapter cannot embed.
const reasonNoEmbeddings = "provider does not serve embeddings"

// Embeddings serves one POST /v1/embeddings request. The model must be an
// embedding model. Its deployments are planned by input price (the
// embeddings_by_input ranking: no output tokens), the cheapest first, and
// the plan executed as for chat completions, so the provider may change
// but the model never does: candidates are only ever deployments of the
// requested model. The policy's deny, allow and require_tags exclude
// deployments as they do for chat, and so does its per-request cost cap,
// after header tightening, against each deployment's estimated input
// cost; its profile, substitution and routing mode never apply. The request is forwarded losslessly with the model
// replaced, and the answer returned byte for byte.
func (g *Gateway) Embeddings(ctx context.Context, ex Exchange) {
	rec := g.newRecord(ctx, EndpointEmbeddings)
	defer rec.done()
	if !g.readTags(ex, rec) {
		return
	}

	body, berr := ex.Body()
	if berr != nil {
		rec.respondError(ex, Info{}, berr)
		return
	}
	req, err := schema.DecodeEmbeddingRequest(body)
	if err != nil {
		rec.respondError(ex, Info{}, invalidRequest(err))
		return
	}
	rec.requestedModel = req.Model()
	ctx, cancel, ok := g.withDeadline(ctx, ex, rec)
	defer cancel()
	if !ok {
		return
	}

	res, e := g.resolve(req.Model(), rec.endpoint, config.ModelTypeEmbedding)
	if e != nil {
		rec.respondError(ex, Info{}, e)
		return
	}
	rec.info = Info{Model: res.Model.ID, Alias: res.Alias}
	rec.alias = res.Alias
	rec.model = res.Model
	// Budgets and the cost cap estimate every input together, at bytes/4
	// of the body; embeddings have no output.
	rec.inBytes, rec.inImages = inputSize(len(body), 0, 0)
	estIn := rec.estIn(res.Model)
	// The policy's deny, allow, require_tags and cost cap apply; nothing
	// else of it does, since embeddings never change models.
	tenant := g.tenants[rec.tenant]
	eff, perr := g.policy.Resolve(tenant, rec.keyID, policy.Request{
		Model: req.Model(), KeyID: rec.keyID, KeyTags: keyTags(tenant, rec.keyID),
		Headers: ex.Header(), Tags: rec.tags, EstInputTokens: estIn,
	})
	if perr != nil {
		rec.respondError(ex, rec.info, policyError(req.Model(), perr))
		return
	}
	rec.routingMode = eff.RoutingMode
	rec.info.PolicyWarning = strings.Join(eff.Warnings, "; ")
	// The context filter applies per input, so the estimate is the
	// largest input's.
	e = g.buildPlan(&rec.plan, res, schema.CapabilitySet{}, req.LargestInputTokens(res.Model.TokenRatio), 0,
		func(d *catalog.Deployment) string {
			if _, ok := g.adapters[d.Provider].(providers.Embedder); !ok {
				return reasonNoEmbeddings
			}
			if reason := router.PolicyReason(&eff, d); reason != "" {
				return reason
			}
			return router.CostCapReason(&eff, d, estIn, 0)
		})
	rec.excluded = len(rec.plan.exclusions)
	if e != nil {
		rec.respondError(ex, rec.info, e)
		return
	}
	g.execute(ctx, ex, rec, func(ctx context.Context, e *planEntry, info *Info) attempt {
		d := e.d
		info.Provider = d.Provider
		embedder := g.adapters[d.Provider].(providers.Embedder)
		start := time.Now()
		resp, err := embedder.Embed(ctx, &providers.EmbedRequest{
			Embedding: req, UpstreamModel: d.UpstreamModel, Header: ex.Header(),
		})
		return g.responseAttempt(ex, rec, start, resp, err, info, nil)
	})
}
