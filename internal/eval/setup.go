package eval

import (
	"errors"
	"fmt"
	"slices"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/policy"
	"switchyard/internal/providers"
	"switchyard/internal/providers/anthropic"
	"switchyard/internal/providers/gemini"
	"switchyard/internal/providers/openai"
	"switchyard/internal/router"
	"switchyard/internal/schema"
)

// Adapters builds one adapter, with its own HTTP client, per provider of
// cfg, as the gateway does. Providers of an unsupported type are left out.
func Adapters(cfg *config.Config) (map[string]providers.Adapter, error) {
	out := make(map[string]providers.Adapter, len(cfg.Providers))
	for _, p := range cfg.Providers {
		var (
			a   providers.Adapter
			err error
		)
		switch p.Type {
		case "openai":
			a, err = openai.New(p, providers.Options{})
		case "anthropic":
			a, err = anthropic.New(p, providers.Options{})
		case "gemini":
			a, err = gemini.New(p, providers.Options{})
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("eval: %w", err)
		}
		out[p.ID] = a
	}
	return out, nil
}

// Pin resolves name to the one deployment an evaluation uses for it: a
// model@provider as is, and a model id or alias to its cheapest deployment
// with known prices, so that every prompt is answered by the same
// deployment. The model must be a chat model.
func Pin(cat *catalog.Catalog, name string) (*catalog.Deployment, error) {
	res, err := cat.Resolve(name)
	if err != nil {
		return nil, fmt.Errorf("eval: %w", err)
	}
	if res.Model.Type != config.ModelTypeChat {
		return nil, fmt.Errorf("eval: model %q is not a chat model", name)
	}
	if res.Pinned {
		return res.Candidates[0], nil
	}
	r, err := cat.Rank(res, schema.CapabilitySet{}, 1000, 500, true)
	if err != nil {
		return nil, fmt.Errorf("eval: model %q: %w", name, err)
	}
	if d := r.Candidates[0].Deployment; d.Pricing.Known {
		return d, nil
	}
	return nil, fmt.Errorf("eval: model %q has no deployment with known prices; name one as model@provider", name)
}

// ShadowJudge pins the shadow sampling judge, evaluation.judge.model, with
// JudgeDeployment. It returns nil and no error when no policy samples
// (shadow.sample_rate > 0). Both `switchyard serve` and `config validate`
// call it, so a config that would judge nothing is rejected before it
// runs.
func ShadowJudge(cat *catalog.Catalog, cfg *config.Config) (*catalog.Deployment, error) {
	if !slices.ContainsFunc(cfg.Policies, func(p config.Policy) bool { return p.Shadow.SampleRate > 0 }) {
		return nil, nil
	}
	return JudgeDeployment(cat, cfg, cfg.Evaluation.Judge.Model)
}

// JudgeDeployment pins the judge model name, as evaluation.judge.model, to
// one deployment that every policy of cfg that samples (shadow.sample_rate
// > 0) permits by its allow, deny and require_tags (router.PolicyReason):
// every sample goes to it, and a policy that refuses it would drop all of
// its samples. A model@provider is that deployment; a model id or alias
// is its cheapest deployment with known prices among those every sampling
// policy permits, as Pin ranks them. Prices only order the permitted
// deployments, so a price change (a dated one included) can move the pick
// between them but never make the configuration invalid. With no sampling
// policy it is Pin's choice. Shadow sampling and `switchyard eval`'s
// default judge both use it, so evaluation.judge.model means the same
// deployment offline and live.
func JudgeDeployment(cat *catalog.Catalog, cfg *config.Config, name string) (*catalog.Deployment, error) {
	refusal := func(d *catalog.Deployment) error {
		for i := range cfg.Policies {
			p := &cfg.Policies[i]
			if !(p.Shadow.SampleRate > 0) {
				continue
			}
			eff := policy.Effective{Allow: p.Allow, Deny: p.Deny, RequireTags: p.RequireTags}
			if why := router.PolicyReason(&eff, d); why != "" {
				return fmt.Errorf("evaluation.judge.model: %q runs on %s (the deployment shadow sampling pins it to), "+
					"which policy %q samples to but does not permit (%s); name a permitted deployment, as evaluation.judge.model: model@provider",
					name, d.ID, p.ID, why)
			}
		}
		return nil
	}
	res, err := cat.Resolve(name)
	if err == nil && !res.Pinned && res.Model.Type == config.ModelTypeChat {
		// The cheapest permitted deployment with known prices, ranked as Pin
		// ranks them.
		if r, err := cat.Rank(res, schema.CapabilitySet{}, 1000, 500, true); err == nil {
			for _, c := range r.Candidates {
				if c.Deployment.Pricing.Known && refusal(c.Deployment) == nil {
					return c.Deployment, nil
				}
			}
		}
	}
	// A pinned judge, or none permitted: Pin's choice and its error.
	d, err := Pin(cat, name)
	if err != nil {
		return nil, fmt.Errorf("evaluation.judge.model: %w", err)
	}
	if err := refusal(d); err != nil {
		return nil, err
	}
	return d, nil
}

// Target is a deployment and the adapter of its provider.
type Target struct {
	Deployment *catalog.Deployment
	Adapter    providers.Adapter
}

// NewTarget pins name (see Pin) and pairs it with its provider's adapter.
func NewTarget(cat *catalog.Catalog, adapters map[string]providers.Adapter, name string) (Target, error) {
	return NewTargetFor(adapters, func() (*catalog.Deployment, error) { return Pin(cat, name) })
}

// NewTargetFor pairs the deployment pin returns with its provider's
// adapter.
func NewTargetFor(adapters map[string]providers.Adapter, pin func() (*catalog.Deployment, error)) (Target, error) {
	d, err := pin()
	if err != nil {
		return Target{}, err
	}
	a := adapters[d.Provider]
	if a == nil {
		return Target{}, fmt.Errorf("eval: deployment %s: provider type %q is not supported", d.ID, d.ProviderType)
	}
	return Target{Deployment: d, Adapter: a}, nil
}

// errNoPrices is returned for a deployment whose cost cannot be estimated.
var errNoPrices = errors.New("its prices are unknown, so the cost cap cannot be enforced")
