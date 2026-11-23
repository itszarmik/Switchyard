package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"switchyard/internal/cache"
	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/policy"
	"switchyard/internal/providers"
	"switchyard/internal/router"
	"switchyard/internal/schema"
	"switchyard/internal/usage"
)

// Embedder is the cache.Embedder of one catalog embedding deployment,
// called through its provider adapter like any embeddings request. It
// never logs the text, and its errors never contain it.
type Embedder struct {
	adapter providers.Embedder
	dep     *catalog.Deployment
	prices  func() config.Pricing
	dims    int
	timeout time.Duration
}

var _ cache.Embedder = (*Embedder)(nil)

// NewEmbedder returns the embedder of dep, called through adapter (dep's
// provider's). Vectors must have dims values. timeout, when positive,
// bounds each call on top of the caller's context.
func NewEmbedder(adapter providers.Embedder, dep *catalog.Deployment, dims int, timeout time.Duration) *Embedder {
	return &Embedder{adapter: adapter, dep: dep, prices: func() config.Pricing { return dep.Prices }, dims: dims, timeout: timeout}
}

// WithPrices makes the embedder price its calls at prices(), read at each
// call, instead of dep's prices, so that new prices (a reload, a dated
// price change) reach its costs at once. Call it before the embedder is
// used. It returns e.
func (e *Embedder) WithPrices(prices func() config.Pricing) *Embedder {
	e.prices = prices
	return e
}

// Model returns the deployment ID, model@provider.
func (e *Embedder) Model() string { return e.dep.ID }

// Dimensions returns the vector size.
func (e *Embedder) Dimensions() int { return e.dims }

// Embed embeds text with one /v1/embeddings call. Tokens are the
// upstream's prompt_tokens or, when it reports none, an estimate of one
// token per 4 bytes of text, scaled by the model's token ratio and rounded
// up. CostNano prices them at the deployment's input price, rounded up to
// whole nano-USD, or is -1 when that price is unknown. A failed call that
// may have been billed (providers.Kind.MayHaveBilled) reports the estimate
// and its cost along with the error; one that was refused reports 0. A
// vector of the wrong size is an error, still charged.
func (e *Embedder) Embed(ctx context.Context, text string) (cache.Embedding, error) {
	if e.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}
	body, err := json.Marshal(struct {
		Model          string `json:"model"`
		Input          string `json:"input"`
		EncodingFormat string `json:"encoding_format"`
	}{e.dep.UpstreamModel, text, "float"})
	if err != nil {
		return cache.Embedding{}, fmt.Errorf("semantic: encoding the embeddings request: %w", err)
	}
	req, err := schema.DecodeEmbeddingRequest(body)
	if err != nil {
		// Unreachable: the body is always a valid request. The schema
		// error never quotes a string input.
		return cache.Embedding{}, fmt.Errorf("semantic: building the embeddings request: %w", err)
	}

	estimate := e.estimateTokens(text)
	resp, err := e.adapter.Embed(ctx, &providers.EmbedRequest{Embedding: req, UpstreamModel: e.dep.UpstreamModel})
	if err != nil {
		var out cache.Embedding
		if mayHaveBilled(err) {
			out.Tokens, out.CostNano = estimate, e.costNano(estimate)
		}
		return out, fmt.Errorf("semantic: embedding with %s: %w", e.dep.ID, err)
	}

	tokens := estimate
	if resp.Usage != nil && resp.Usage.PromptTokens > 0 {
		tokens = resp.Usage.PromptTokens
	}
	out := cache.Embedding{Tokens: tokens, CostNano: e.costNano(tokens)}
	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return out, fmt.Errorf("semantic: %s returned an invalid embeddings response: %w", e.dep.ID, err)
	}
	if len(parsed.Data) != 1 {
		return out, fmt.Errorf("semantic: %s returned %d embeddings, want 1", e.dep.ID, len(parsed.Data))
	}
	vec := parsed.Data[0].Embedding
	if len(vec) != e.dims {
		return out, fmt.Errorf("semantic: %s returned a vector of %d dimensions, want %d", e.dep.ID, len(vec), e.dims)
	}
	out.Vector = vec
	return out, nil
}

// estimateTokens is the token estimate of text when the upstream reports
// no usage: bytes/4 times the model's token ratio, rounded up, and at
// least 1.
func (e *Embedder) estimateTokens(text string) int64 {
	ratio := 1.0
	if e.dep.Model != nil && e.dep.Model.TokenRatio > 0 {
		ratio = e.dep.Model.TokenRatio
	}
	return max(int64(math.Ceil(float64(len(text))/4*ratio)), 1)
}

// costNano prices tokens of input at the deployment's prices (see
// WithPrices), in nano-USD rounded up, or returns -1 when the price is
// unknown.
func (e *Embedder) costNano(tokens int64) int64 {
	m, ok := usage.EmbeddingCost(usage.Tokens{Input: tokens}, e.prices())
	if !ok {
		return -1
	}
	return m.NanoUSDCeil()
}

// mayHaveBilled reports whether a failed call may have been billed: as its
// providers.Error kind says, and yes for any other error (as for a
// transport failure).
func mayHaveBilled(err error) bool {
	var pe *providers.Error
	if errors.As(err, &pe) {
		return pe.Kind.MayHaveBilled()
	}
	return true
}

// EmbeddingDeployment resolves semantic_cache.embedding_model to the
// deployment that embeds for the semantic cache, and its adapter. A
// model@provider is used as is. A model ID or alias resolves to the
// deployment with the lowest known input price whose adapter serves
// embeddings, the first in config order on a tie, so that the choice (and
// with it the vector index) is stable. The model must be an embedding
// model with dimensions.
func EmbeddingDeployment(cat *catalog.Catalog, adapters map[string]providers.Adapter, name string) (*catalog.Deployment, providers.Embedder, error) {
	return embeddingDeployment(cat, adapters, name, nil)
}

// embeddingDeployment is EmbeddingDeployment among the deployments that
// permitted, when set, accepts.
func embeddingDeployment(cat *catalog.Catalog, adapters map[string]providers.Adapter, name string,
	permitted func(*catalog.Deployment) bool) (*catalog.Deployment, providers.Embedder, error) {
	res, err := cat.Resolve(name)
	if err != nil {
		return nil, nil, fmt.Errorf("semantic: embedding model %q: %w", name, err)
	}
	if res.Model.Type != config.ModelTypeEmbedding || res.Model.Dimensions <= 0 {
		return nil, nil, fmt.Errorf("semantic: %q is not an embedding model with dimensions", name)
	}
	var (
		best    *catalog.Deployment
		bestEmb providers.Embedder
	)
	for _, d := range res.Candidates {
		emb, ok := adapters[d.Provider].(providers.Embedder)
		if !ok {
			continue
		}
		if res.Pinned {
			return d, emb, nil
		}
		if !d.Pricing.Known || (permitted != nil && !permitted(d)) {
			continue
		}
		if best == nil || d.Pricing.Input < best.Pricing.Input {
			best, bestEmb = d, emb
		}
	}
	if best == nil {
		ids := make([]string, 0, len(res.Candidates))
		for _, d := range res.Candidates {
			ids = append(ids, d.ID)
		}
		slices.Sort(ids)
		return nil, nil, fmt.Errorf("semantic: embedding model %q has no deployment with a known input price whose provider serves embeddings (deployments: %v)", name, ids)
	}
	return best, bestEmb, nil
}

// ConfiguredDeployment returns the deployment the semantic cache embeds
// with, and its adapter: semantic_cache.embedding_model resolved by
// EmbeddingDeployment among the deployments that every policy enabling the
// semantic cache permits (see CheckPolicies), or, when it permits none,
// the error for the one EmbeddingDeployment picks. Prices only order the
// permitted deployments, so a price change (a dated one included) can move
// the pick between them but never make the configuration invalid. It
// returns nil and no error when no policy or rule enables it. Both
// `switchyard serve` and `config validate` call it, so a config that would
// send prompts where a policy forbids is rejected before it runs.
func ConfiguredDeployment(cfg *config.Config, cat *catalog.Catalog, adapters map[string]providers.Adapter) (*catalog.Deployment, providers.Embedder, error) {
	if !Enabled(cfg) {
		return nil, nil, nil
	}
	name := cfg.SemanticCache.EmbeddingModel
	if d, emb, err := embeddingDeployment(cat, adapters, name, func(d *catalog.Deployment) bool {
		return CheckPolicies(cfg, d) == nil
	}); err == nil {
		return d, emb, nil
	}
	d, emb, err := EmbeddingDeployment(cat, adapters, name)
	if err != nil {
		return nil, nil, fmt.Errorf("semantic_cache.embedding_model: %w", err)
	}
	if err := CheckPolicies(cfg, d); err != nil {
		return nil, nil, err
	}
	return d, emb, nil
}

// Enabled reports whether any policy, or any of its rules' set.cache,
// turns the semantic cache on.
func Enabled(cfg *config.Config) bool {
	for i := range cfg.Policies {
		p := &cfg.Policies[i]
		if p.Cache.Semantic.Enabled {
			return true
		}
		for j := range p.Rules {
			if c := p.Rules[j].Set.Cache; c != nil && c.Semantic.Enabled {
				return true
			}
		}
	}
	return false
}

// CheckPolicies returns an error naming d, the embedding deployment, when
// a policy that enables the semantic cache does not permit d by its allow,
// deny and require_tags (router.PolicyReason): each semantic lookup sends
// the request's last user message to d. It checks each policy whose own
// cache block enables the semantic cache against the policy's filters,
// and each governance rule whose set.cache enables it against the
// policy's filters with the rule's allow, deny and require_tags in their
// place, as the policy engine applies them. Rules that leave the cache
// block alone but tighten the filters are checked per request instead:
// such a request skips the semantic layer.
func CheckPolicies(cfg *config.Config, d *catalog.Deployment) error {
	for i := range cfg.Policies {
		p := &cfg.Policies[i]
		base := policy.Effective{Allow: p.Allow, Deny: p.Deny, RequireTags: p.RequireTags}
		if p.Cache.Semantic.Enabled {
			if why := router.PolicyReason(&base, d); why != "" {
				return policyError(cfg, d, fmt.Sprintf("policy %q", p.ID), why)
			}
		}
		for j := range p.Rules {
			r := &p.Rules[j]
			if r.Set.Cache == nil || !r.Set.Cache.Semantic.Enabled {
				continue
			}
			eff := base
			if r.Set.Allow != nil {
				eff.Allow = r.Set.Allow
			}
			if r.Set.Deny != nil {
				eff.Deny = r.Set.Deny
			}
			if r.Set.RequireTags != nil {
				eff.RequireTags = r.Set.RequireTags
			}
			if why := router.PolicyReason(&eff, d); why != "" {
				return policyError(cfg, d, fmt.Sprintf("policy %q, rule %q,", p.ID, r.Name), why)
			}
		}
	}
	return nil
}

// policyError is CheckPolicies' error for d, which who (a policy, or a
// policy's rule) enables the semantic cache for but does not permit.
func policyError(cfg *config.Config, d *catalog.Deployment, who, why string) error {
	return fmt.Errorf("semantic_cache.embedding_model: %q embeds on %s (the deployment the semantic cache uses), "+
		"which %s enables the semantic cache for but does not permit (%s); it would receive that policy's prompts. "+
		"Name a permitted deployment, as semantic_cache.embedding_model: model@provider, or turn the semantic cache off there",
		cfg.SemanticCache.EmbeddingModel, d.ID, who, why)
}
