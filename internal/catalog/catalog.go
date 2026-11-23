// Package catalog is the routing view of the model catalog: which models
// exist, which deployments serve them, what each deployment can do and what
// it costs (DESIGN §5, §9.1–9.2).
//
// A Catalog is built once from a validated configuration and is immutable,
// so one value can serve every request of a config snapshot concurrently.
// It holds the prices in force when it was built (see NewAt): a dated
// price change takes effect in the next catalog built from or after its
// time, and NextPriceChange says when that is.
// Resolve maps a client's model field to candidate deployments, and Select
// applies the hard filters (capabilities, context window) and picks the
// cheapest eligible candidate.
package catalog

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"switchyard/internal/config"
	"switchyard/internal/schema"
)

// Errors returned by Resolve.
var (
	// ErrUnknownModel means the name is not a model, an alias or a
	// model@provider deployment in the catalog.
	ErrUnknownModel = errors.New("unknown model")
	// ErrVirtualModel means the name is a virtual model ("auto" or
	// "auto:<profile>"), which arrive in Phase 5.
	ErrVirtualModel = errors.New("virtual models are not supported yet")
)

// Catalog is an immutable index of the configured models and deployments.
// Its methods are safe for concurrent use. Callers must not modify the
// values it returns.
type Catalog struct {
	models      []*Model
	byID        map[string]*Model
	deployments map[string]*Deployment // by model@provider
	aliases     map[string]string
	// nextChange is the earliest dated price change after the build
	// time, or zero.
	nextChange time.Time
}

// Model is a logical model in the catalog.
type Model struct {
	// ID is the catalog model ID, the name clients send.
	ID string
	// Type is config.ModelTypeChat or config.ModelTypeEmbedding.
	Type string
	// ContextWindow is the most input plus output tokens, or 0 if unknown.
	ContextWindow int64
	// MaxOutputTokens is the most output tokens per response, or 0 if
	// unknown.
	MaxOutputTokens int64
	// Capabilities are the model's declared capabilities.
	Capabilities schema.CapabilitySet
	// Dimensions is the vector size of an embedding model.
	Dimensions int
	// TokenRatio scales the bytes/4 token estimate for this model's
	// tokenizer; it is 1 unless the catalog says otherwise.
	TokenRatio float64
	// Quality holds the model's quality priors in [0, 1] by category
	// (DESIGN §10.2), or nil if the catalog sets none. A category without
	// a prior falls back to general. The config's quality_overrides
	// replace the catalog's priors for their (model, category) pairs.
	Quality map[string]float64
	// Deployments serve the model, in config order.
	Deployments []*Deployment
}

// Deployment is a model served by one provider.
type Deployment struct {
	// ID is "model@provider".
	ID string
	// Model is the model this deployment serves.
	Model *Model
	// Provider is the provider ID.
	Provider string
	// ProviderType is the provider's wire protocol: openai or anthropic.
	ProviderType string
	// UpstreamModel is the model name sent to the provider.
	UpstreamModel string
	// Capabilities are the deployment's own list when the catalog sets one,
	// else the model's.
	Capabilities schema.CapabilitySet
	// Tags are the provider's tags followed by the deployment's, without
	// duplicates.
	Tags []string
	// Pricing is what the deployment costs, as floats for ranking: the
	// prices in force when the catalog was built.
	Pricing Pricing
	// Prices are the deployment's exact prices from the config, for cost
	// accounting: the prices in force when the catalog was built, with
	// every price change up to then applied and no Changes.
	Prices config.Pricing
	// LatencyHint is the operator's latency estimate, which seeds the
	// latency statistics until there are measurements.
	LatencyHint LatencyHint
	// QualityOffset adjusts the model's quality priors for this
	// deployment, in [-1, 1] (DESIGN §10.2).
	QualityOffset float64
	// ProviderMaxConcurrency is the provider's max_concurrency, 0 for
	// unlimited.
	ProviderMaxConcurrency int
}

// LatencyHint is an operator's latency estimate. Zero values are unknown.
type LatencyHint struct {
	TTFT         time.Duration
	TokensPerSec float64
}

// Pricing is in USD per million tokens.
type Pricing struct {
	Input, Output, CachedInput, CacheWrite float64
	// Known reports whether the prices routing needs are set: input and
	// output for a chat model, input for an embedding model. A zero price
	// that is set is known.
	Known bool
}

// Resolution is the outcome of Resolve.
type Resolution struct {
	// Model is the resolved catalog model.
	Model *Model
	// Candidates are the deployments that may serve the request: the pinned
	// deployment only, or all of the model's deployments in config order.
	// The slice belongs to the caller.
	Candidates []*Deployment
	// Pinned is true when the name, or the alias's target, was
	// model@provider.
	Pinned bool
	// Alias is the alias the name was, or "".
	Alias string
}

// Selection is the outcome of Select.
type Selection struct {
	// Deployment is the chosen deployment.
	Deployment *Deployment
	// Dropped lists, sorted, the droppable capabilities the request needs
	// but Deployment lacks, including sampling whatever the drop flag was.
	// The gateway removes the matching parameters before forwarding.
	Dropped []schema.Capability
	// Exclusions explains every candidate that was not eligible, in
	// candidate order.
	Exclusions []Exclusion
}

// Exclusion records why a candidate deployment was not eligible.
type Exclusion struct {
	// Deployment is the deployment ID, model@provider.
	Deployment string
	// Reason is, for example, "lacks capability n" or
	// "context 128000 < 150000 needed".
	Reason string
}

// NoEligibleError is returned by Select when no candidate passes the hard
// filters.
type NoEligibleError struct {
	// Model is the catalog model ID.
	Model string
	// Exclusions has one entry per candidate, in candidate order.
	Exclusions []Exclusion
}

// Error names every candidate and the reason it was excluded.
func (e *NoEligibleError) Error() string {
	parts := make([]string, len(e.Exclusions))
	for i, x := range e.Exclusions {
		parts[i] = x.Deployment + ": " + x.Reason
	}
	return fmt.Sprintf("no eligible deployment for model %q: %s", e.Model, strings.Join(parts, "; "))
}

// New builds the catalog from cfg with the prices in force now (see
// NewAt).
func New(cfg *config.Config) (*Catalog, error) { return NewAt(cfg, time.Now()) }

// NewAt builds the catalog from cfg, which must already be validated, with
// each deployment's prices in force at t: its pricing with every change
// whose from is at or before t applied (config.Pricing.At). Each of
// cfg.QualityOverrides replaces its model's prior for its category. NewAt
// returns an error only for inconsistencies that validation rejects, such
// as a deployment of an undefined provider or an override of an undefined
// model.
func NewAt(cfg *config.Config, t time.Time) (*Catalog, error) {
	providers := make(map[string]*config.Provider, len(cfg.Providers))
	for i := range cfg.Providers {
		providers[cfg.Providers[i].ID] = &cfg.Providers[i]
	}
	c := &Catalog{
		byID:        make(map[string]*Model, len(cfg.Models)),
		deployments: make(map[string]*Deployment),
		aliases:     maps.Clone(cfg.Aliases),
	}
	if c.aliases == nil {
		c.aliases = map[string]string{}
	}
	for i := range cfg.Models {
		cm := &cfg.Models[i]
		if _, dup := c.byID[cm.ID]; dup {
			return nil, fmt.Errorf("catalog: duplicate model id %q", cm.ID)
		}
		ratio := cm.TokenRatio
		if ratio == 0 {
			ratio = 1
		}
		m := &Model{
			ID:              cm.ID,
			Type:            cm.Type,
			ContextWindow:   cm.ContextWindow,
			MaxOutputTokens: cm.MaxOutputTokens,
			Capabilities:    capabilitySet(cm.Capabilities),
			Dimensions:      cm.Dimensions,
			TokenRatio:      ratio,
			Quality:         maps.Clone(cm.Quality),
		}
		if m.Type == "" {
			m.Type = config.ModelTypeChat
		}
		for j := range cm.Deployments {
			cd := &cm.Deployments[j]
			p := providers[cd.Provider]
			if p == nil {
				return nil, fmt.Errorf("catalog: model %q: unknown provider %q", cm.ID, cd.Provider)
			}
			prices := cd.Pricing.At(t)
			if _, next, ok := cd.Pricing.NextChange(t); ok && (c.nextChange.IsZero() || next.Before(c.nextChange)) {
				c.nextChange = next
			}
			d := &Deployment{
				ID:            cm.ID + "@" + cd.Provider,
				Model:         m,
				Provider:      cd.Provider,
				ProviderType:  p.Type,
				UpstreamModel: cd.UpstreamModel,
				Capabilities:  m.Capabilities,
				Tags:          mergeTags(p.Tags, cd.Tags),
				Pricing: Pricing{
					Input:       prices.Input.Float64(),
					Output:      prices.Output.Float64(),
					CachedInput: prices.CachedInput.Float64(),
					CacheWrite:  prices.CacheWrite.Float64(),
					Known:       config.PricingKnown(m.Type, prices),
				},
				Prices: prices,
				LatencyHint: LatencyHint{
					TTFT:         time.Duration(cd.LatencyHint.TTFTMs) * time.Millisecond,
					TokensPerSec: cd.LatencyHint.TokensPerSec,
				},
				QualityOffset:          cd.QualityOffset,
				ProviderMaxConcurrency: p.MaxConcurrency,
			}
			if cd.Capabilities != nil {
				d.Capabilities = capabilitySet(cd.Capabilities)
			}
			if _, dup := c.deployments[d.ID]; dup {
				return nil, fmt.Errorf("catalog: duplicate deployment %q", d.ID)
			}
			c.deployments[d.ID] = d
			m.Deployments = append(m.Deployments, d)
		}
		c.models = append(c.models, m)
		c.byID[m.ID] = m
	}
	// Measured priors replace the catalog's (DESIGN §10.7).
	for _, o := range cfg.QualityOverrides {
		m := c.byID[o.Model]
		if m == nil {
			return nil, fmt.Errorf("catalog: quality override: unknown model %q", o.Model)
		}
		if m.Quality == nil {
			m.Quality = make(map[string]float64, 1)
		}
		m.Quality[o.Category] = o.Value
	}
	for name, target := range c.aliases {
		if IsVirtual(target) {
			continue
		}
		if _, err := c.resolveTarget(target); err != nil {
			return nil, fmt.Errorf("catalog: alias %q: %w", name, err)
		}
	}
	return c, nil
}

// capabilitySet converts a validated capability list.
func capabilitySet(caps []string) schema.CapabilitySet {
	s := make(schema.CapabilitySet, len(caps))
	for _, c := range caps {
		s[schema.Capability(c)] = struct{}{}
	}
	return s
}

// mergeTags returns a followed by the tags of b not already in a.
func mergeTags(a, b []string) []string {
	out := slices.Clone(a)
	for _, t := range b {
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// IsVirtual reports whether name is a virtual model: "auto" or
// "auto:<profile>". Validation reserves these names, so no catalog model
// has one.
func IsVirtual(name string) bool {
	return name == "auto" || strings.HasPrefix(name, "auto:")
}

// VirtualTarget returns the virtual model that name stands for: name itself
// when IsVirtual(name), or the target of an alias whose target is virtual.
// It reports false for every other name, which Resolve handles.
func (c *Catalog) VirtualTarget(name string) (string, bool) {
	if target, ok := c.aliases[name]; ok {
		return target, IsVirtual(target)
	}
	return name, IsVirtual(name)
}

// Resolve maps a client's model field to candidate deployments. It accepts
// a catalog model ID; an alias, whose target is a model ID or
// model@provider; or model@provider, which pins the deployment. A name that
// is none of these and starts with "auto", or an alias whose target is a
// virtual model (see VirtualTarget), returns ErrVirtualModel; any other
// unknown name returns ErrUnknownModel. Both errors are wrapped with the
// name.
//
// Catalog names are matched first, so a model that is merely named with an
// "auto" prefix, such as "autopilot", still resolves; validation reserves
// only "auto" and "auto:…".
func (c *Catalog) Resolve(name string) (Resolution, error) {
	if target, ok := c.aliases[name]; ok {
		if IsVirtual(target) {
			return Resolution{}, fmt.Errorf("model %q: %w", name, ErrVirtualModel)
		}
		res, err := c.resolveTarget(target)
		if err != nil {
			return Resolution{}, fmt.Errorf("model %q: %w", name, err)
		}
		res.Alias = name
		return res, nil
	}
	res, err := c.resolveTarget(name)
	if errors.Is(err, ErrUnknownModel) && strings.HasPrefix(name, "auto") {
		return Resolution{}, fmt.Errorf("model %q: %w", name, ErrVirtualModel)
	}
	if err != nil {
		return Resolution{}, fmt.Errorf("model %q: %w", name, err)
	}
	return res, nil
}

// resolveTarget resolves a model ID or model@provider.
func (c *Catalog) resolveTarget(name string) (Resolution, error) {
	if m := c.byID[name]; m != nil {
		return Resolution{Model: m, Candidates: slices.Clone(m.Deployments)}, nil
	}
	if d := c.deployments[name]; d != nil {
		return Resolution{Model: d.Model, Candidates: []*Deployment{d}, Pinned: true}, nil
	}
	return Resolution{}, ErrUnknownModel
}

// Candidate is an eligible deployment, as ranked by Rank.
type Candidate struct {
	Deployment *Deployment
	// Dropped lists, sorted, the droppable capabilities the request needs
	// but the deployment lacks (see Selection.Dropped).
	Dropped []schema.Capability
	// Cost is the estimated cost in USD, or 0 if pricing is unknown.
	Cost float64
}

// Ranking is the outcome of Rank.
type Ranking struct {
	// Candidates are the eligible deployments, cheapest first.
	Candidates []Candidate
	// Exclusions explains every candidate that was not eligible, in
	// candidate order.
	Exclusions []Exclusion
}

// Rank applies the hard filters to every candidate of res and returns the
// eligible ones, cheapest first, with the exclusions.
//
// The hard filters are: every capability in required must be in the
// deployment's capabilities, except droppable ones (see
// schema.Capability.Droppable) when drop is true, and sampling (see
// schema.Capability.AlwaysDropped), which is dropped whatever drop says,
// together with temperature_2 when both are required; and, when the model's
// ContextWindow is known, estIn+estOut must not exceed it. Eligible
// candidates are ranked by estimated cost, estIn×Input/1e6 +
// estOut×Output/1e6; candidates with unknown pricing rank after every known
// price, and ties keep config order. estIn and estOut are token counts,
// and negative values count as 0; their sum saturates at math.MaxInt64.
//
// With no eligible candidate, Rank returns a *NoEligibleError with an
// Exclusion for every candidate.
func (c *Catalog) Rank(res Resolution, required schema.CapabilitySet, estIn, estOut int64, drop bool) (Ranking, error) {
	estIn, estOut = max(estIn, 0), max(estOut, 0)
	// The context the request needs. Both estimates are non-negative, so
	// a negative sum overflowed: the need saturates instead of wrapping
	// past every context window.
	needed := estIn + estOut
	if needed < 0 {
		needed = math.MaxInt64
	}
	need := required.Sorted()
	var out Ranking
	for _, d := range res.Candidates {
		var lacks, dropped []schema.Capability
		// Without sampling, temperature is removed anyway, so a need for
		// temperature_2 goes with it.
		noSampling := required.Has(schema.CapSampling) && !d.Capabilities.Has(schema.CapSampling)
		for _, c := range need {
			switch {
			case d.Capabilities.Has(c):
			case c.AlwaysDropped(),
				drop && c.Droppable(),
				noSampling && c == schema.CapTemperature2:
				dropped = append(dropped, c)
			default:
				lacks = append(lacks, c)
			}
		}
		var reasons []string
		if len(lacks) > 0 {
			reasons = append(reasons, lacksReason(lacks))
		}
		if cw := d.Model.ContextWindow; cw > 0 && needed > cw {
			reasons = append(reasons, "context "+strconv.FormatInt(cw, 10)+" < "+strconv.FormatInt(needed, 10)+" needed")
		}
		if len(reasons) > 0 {
			out.Exclusions = append(out.Exclusions, Exclusion{Deployment: d.ID, Reason: strings.Join(reasons, "; ")})
			continue
		}
		if out.Candidates == nil {
			out.Candidates = make([]Candidate, 0, len(res.Candidates))
		}
		out.Candidates = append(out.Candidates, Candidate{Deployment: d, Dropped: dropped, Cost: d.Pricing.estimate(estIn, estOut)})
	}
	if len(out.Candidates) == 0 {
		model := ""
		if res.Model != nil {
			model = res.Model.ID
		}
		return Ranking{}, &NoEligibleError{Model: model, Exclusions: out.Exclusions}
	}
	slices.SortStableFunc(out.Candidates, func(a, b Candidate) int {
		switch {
		case cheaper(a.Deployment, a.Cost, b.Deployment, b.Cost):
			return -1
		case cheaper(b.Deployment, b.Cost, a.Deployment, a.Cost):
			return 1
		}
		return 0
	})
	return out, nil
}

// Select returns the cheapest eligible candidate of res: the first of
// Rank's candidates, with the same filters, ranking and errors.
func (c *Catalog) Select(res Resolution, required schema.CapabilitySet, estIn, estOut int64, drop bool) (Selection, error) {
	r, err := c.Rank(res, required, estIn, estOut, drop)
	if err != nil {
		return Selection{}, err
	}
	best := r.Candidates[0]
	return Selection{Deployment: best.Deployment, Dropped: best.Dropped, Exclusions: r.Exclusions}, nil
}

// cheaper reports whether candidate d at cost ranks strictly before best at
// bestCost: a known price before an unknown one, then the lower cost.
func cheaper(d *Deployment, cost float64, best *Deployment, bestCost float64) bool {
	if d.Pricing.Known != best.Pricing.Known {
		return d.Pricing.Known
	}
	return d.Pricing.Known && cost < bestCost
}

// estimate returns the estimated cost in USD, or 0 if pricing is unknown.
func (p Pricing) estimate(in, out int64) float64 {
	if !p.Known {
		return 0
	}
	return float64(in)*p.Input/1e6 + float64(out)*p.Output/1e6
}

// lacksReason formats the exclusion reason for missing capabilities.
func lacksReason(caps []schema.Capability) string {
	if len(caps) == 1 {
		return "lacks capability " + string(caps[0])
	}
	names := make([]string, len(caps))
	for i, c := range caps {
		names[i] = string(c)
	}
	return "lacks capabilities " + strings.Join(names, ", ")
}

// Deployment returns the deployment with the given id, model@provider, or
// nil.
func (c *Catalog) Deployment(id string) *Deployment { return c.deployments[id] }

// NextPriceChange returns the time of the earliest dated price change
// after the catalog's build time, from which a newly built catalog has
// the changed prices, or the zero time if none is pending.
func (c *Catalog) NextPriceChange() time.Time { return c.nextChange }

// Models returns every model in catalog order. The slice belongs to the
// caller; the models do not.
func (c *Catalog) Models() []*Model { return slices.Clone(c.models) }

// Aliases returns a copy of the alias map, alias name to target.
func (c *Catalog) Aliases() map[string]string { return maps.Clone(c.aliases) }
