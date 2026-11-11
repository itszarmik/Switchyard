package gateway

import (
	"context"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// reasonNoEmbeddings excludes a deployment whose adapter cannot embed.
const reasonNoEmbeddings = "provider does not serve embeddings"

// Embeddings serves one POST /v1/embeddings request. The model must be an
// embedding model. Among its deployments, the one with the lowest input
// price is chosen (the embeddings_by_input selection: no output tokens),
// so the provider may change but the model never does: candidates are only
// ever deployments of the requested model. The request is forwarded
// losslessly with the model replaced, and the answer returned byte for
// byte.
func (g *Gateway) Embeddings(ctx context.Context, ex Exchange) {
	rec := g.newRecord(ctx, EndpointEmbeddings)
	defer rec.finish()

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

	res, e := g.resolve(req.Model(), rec.endpoint, config.ModelTypeEmbedding)
	if e != nil {
		rec.respondError(ex, Info{}, e)
		return
	}
	info := Info{Model: res.Model.ID, Alias: res.Alias}
	rec.alias = res.Alias
	// The context filter applies per input, so the estimate is the
	// largest input's.
	sel, e := g.choose(res, schema.CapabilitySet{}, req.LargestInputTokens(res.Model.TokenRatio), 0,
		func(d *catalog.Deployment) string {
			if _, ok := g.adapters[d.Provider].(providers.Embedder); !ok {
				return reasonNoEmbeddings
			}
			return ""
		})
	rec.excluded = len(sel.Exclusions)
	if e != nil {
		rec.respondError(ex, info, e)
		return
	}
	d := sel.Deployment
	rec.deployment = d
	info.Provider = d.Provider

	embedder := g.adapters[d.Provider].(providers.Embedder)
	rec.upstreamStart = time.Now()
	resp, err := embedder.Embed(ctx, &providers.EmbedRequest{
		Embedding: req, UpstreamModel: d.UpstreamModel, Header: ex.Header(),
	})
	g.serveResponse(ex, rec, resp, err, info, nil)
}
