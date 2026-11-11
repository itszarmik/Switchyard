package config

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"switchyard/internal/schema"
)

// Model types.
const (
	// ModelTypeChat serves /v1/chat/completions and /v1/completions.
	ModelTypeChat = "chat"
	// ModelTypeEmbedding serves /v1/embeddings.
	ModelTypeEmbedding = "embedding"
)

// Catalog limits and messages.
const (
	maxTokenRatio = 10
	// DateLayout is the layout of Pricing.Checked, YYYY-MM-DD.
	DateLayout = "2006-01-02"

	// MsgPricingUnknown is the warning for a deployment without the prices
	// routing needs: input and output for a chat model, input for an
	// embedding model.
	MsgPricingUnknown = "pricing unknown; this deployment is ranked last"
	msgVirtualTarget  = "virtual models arrive in Phase 5"
)

// qualityCategories are the keys Model.Quality accepts, in documentation
// order.
var qualityCategories = []string{"general", "coding", "reasoning", "extraction", "creative", "tool_use"}

// chatOnlyCapabilities are the capabilities an embedding model can never
// have.
var chatOnlyCapabilities = map[schema.Capability]bool{
	schema.CapTools:        true,
	schema.CapVision:       true,
	schema.CapJSONMode:     true,
	schema.CapJSONSchema:   true,
	schema.CapN:            true,
	schema.CapLogprobs:     true,
	schema.CapTemperature2: true,
	schema.CapPenalties:    true,
	schema.CapCompletions:  true,
	schema.CapSampling:     true,
	schema.CapForcedTools:  true,
}

// capabilityNames is the vocabulary as strings, for suggestions.
var capabilityNames = func() []string {
	var out []string
	for _, c := range schema.Vocabulary() {
		out = append(out, string(c))
	}
	return out
}()

// checkModelID validates the syntax of a model id, or of an alias name,
// which follows the same rules. It reports nothing for an empty id.
func (st *loadState) checkModelID(path, id string) {
	switch {
	case id == "":
	case strings.Contains(id, "@"):
		st.errorAt(path, `must not contain "@", which separates model and provider in deployment ids`)
	case id == "auto" || strings.HasPrefix(id, "auto:"):
		st.errorAt(path, `"auto" and the "auto:" prefix are reserved for virtual models`)
	case !modelIDRE.MatchString(id):
		st.errorAt(path, fmt.Sprintf("must match %s, got %s", modelIDRE, st.describe(path, id)))
	}
}

// validateModelCatalog checks the catalog data of model m at path base:
// type, limits, capabilities, quality priors, token ratio and dimensions.
func (st *loadState) validateModelCatalog(m *Model, base string) {
	typeOK := st.checkOneOf(base+".type", m.Type, ModelTypeChat, ModelTypeEmbedding)
	embedding := typeOK && m.Type == ModelTypeEmbedding

	if m.ContextWindow < 0 {
		st.errorAt(base+".context_window", "must not be negative; use 0 for unknown")
	}
	if m.MaxOutputTokens < 0 {
		st.errorAt(base+".max_output_tokens", "must not be negative; use 0 for unknown")
	}
	if m.ContextWindow > 0 && m.MaxOutputTokens > m.ContextWindow {
		st.errorAt(base+".max_output_tokens", fmt.Sprintf("must not exceed context_window (%d)", m.ContextWindow))
	}

	st.checkCapabilities(base+".capabilities", m.Capabilities, embedding)

	keys := make([]string, 0, len(m.Quality))
	for k := range m.Quality {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		qp := joinPath(base+".quality", shownKey(k))
		if !slices.Contains(qualityCategories, k) {
			msg := "unknown quality category"
			if shownKey(k) == k {
				if s := suggest(k, qualityCategories); s != "" {
					msg += fmt.Sprintf(" (did you mean %q?)", s)
				}
			}
			st.errorAt(qp, msg+"; use one of "+strings.Join(qualityCategories, ", "))
			continue
		}
		if q := m.Quality[k]; q < 0 || q > 1 {
			st.errorAt(qp, "must be between 0 and 1")
		}
	}

	if m.TokenRatio <= 0 || m.TokenRatio > maxTokenRatio {
		st.errorAt(base+".token_ratio", fmt.Sprintf("must be greater than 0 and at most %d", maxTokenRatio))
	}

	dp := base + ".dimensions"
	switch {
	case !typeOK:
	case embedding && m.Dimensions == 0:
		st.errorAt(dp, "required for an embedding model")
	case embedding && m.Dimensions < 0:
		st.errorAt(dp, "must be greater than 0")
	case !embedding && m.Dimensions != 0:
		st.errorAt(dp, "only allowed for an embedding model")
	}
}

// checkCapabilities validates a capability list at path: every entry must be
// in the vocabulary and appear once, and an embedding model's list must not
// contain chat-only capabilities.
func (st *loadState) checkCapabilities(path string, caps []string, embedding bool) {
	seen := make(map[string]bool)
	for j, c := range caps {
		cp := indexPath(path, j)
		switch {
		case !schema.Capability(c).Known():
			msg := "unknown capability " + st.describe(cp, c)
			if s := suggest(c, capabilityNames); s != "" && mayQuote(c) {
				msg += fmt.Sprintf(" (did you mean %q?)", s)
			}
			st.errorAt(cp, msg)
		case seen[c]:
			st.errorAt(cp, "duplicate capability "+st.describe(cp, c))
		case embedding && chatOnlyCapabilities[schema.Capability(c)]:
			st.errorAt(cp, fmt.Sprintf("%q is a chat capability; an embedding model cannot have it", c))
		}
		seen[c] = true
	}
}

// validateDeploymentCatalog checks the catalog data of deployment d of model
// m at path dp: capabilities, quality offset, latency hint and pricing.
func (st *loadState) validateDeploymentCatalog(m *Model, d *Deployment, dp string) {
	embedding := m.Type == ModelTypeEmbedding
	st.checkCapabilities(dp+".capabilities", d.Capabilities, embedding)
	st.checkTags(dp+".tags", d.Tags)
	if d.QualityOffset < -1 || d.QualityOffset > 1 {
		st.errorAt(dp+".quality_offset", "must be between -1 and 1")
	}
	if d.LatencyHint.TTFTMs < 0 {
		st.errorAt(dp+".latency_hint.ttft_ms", "must not be negative")
	}
	if d.LatencyHint.TokensPerSec < 0 {
		st.errorAt(dp+".latency_hint.tokens_per_sec", "must not be negative")
	}

	pp := dp + ".pricing"
	if c := d.Pricing.Checked; c != "" {
		if _, err := time.Parse(DateLayout, c); err != nil {
			st.errorAt(pp+".checked", "must be a date as YYYY-MM-DD, got "+st.describe(pp+".checked", c))
		}
	}
	if s := d.Pricing.Source; s != "" {
		// The URL is never echoed: a bad one may embed credentials.
		u, err := url.Parse(s)
		switch {
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			st.errorAt(pp+".source", "must be an absolute http:// or https:// URL with a host")
		case u.User != nil:
			st.errorAt(pp+".source", "must not embed credentials")
		}
	}
	// A price that failed to decode is already reported.
	if !PricingKnown(m.Type, d.Pricing) && !st.failed[pp+".input"] && !st.failed[pp+".output"] {
		st.warnAt(pp, MsgPricingUnknown)
	}
}

// PricingKnown reports whether p has the prices routing needs for a model of
// the given type: input and output for a chat model, input for an embedding
// model.
func PricingKnown(modelType string, p Pricing) bool {
	if modelType == ModelTypeEmbedding {
		return p.Input.IsSet()
	}
	return p.Input.IsSet() && p.Output.IsSet()
}

// validateAliases checks every alias name and target. models maps each
// model id to its first definition, and modelPaths to the path of its id.
func (st *loadState) validateAliases(aliases map[string]string, models map[string]*Model, modelPaths map[string]string) {
	names := make([]string, 0, len(aliases))
	for name := range aliases {
		names = append(names, name)
	}
	slices.Sort(names)
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	for _, name := range names {
		path := joinPath("aliases", shownKey(name))
		switch {
		case name == "":
			st.errorAt(path, "an alias name must not be empty")
		case models[name] != nil:
			st.errorAt(path, "alias name collides with the model id defined at "+st.where(modelPaths[name]))
		default:
			st.checkModelID(path, name)
		}

		target := aliases[name]
		switch modelID, provider, pinned := strings.Cut(target, "@"); {
		case target == "":
			st.errorAt(path, "required; set a model id or model@provider")
		case models[target] != nil:
		case pinned:
			m := models[modelID]
			switch {
			case m == nil:
				st.errorAt(path, "target "+st.describe(path, target)+" names an unknown model")
			case !slices.ContainsFunc(m.Deployments, func(d Deployment) bool { return d.Provider == provider }):
				st.errorAt(path, "target "+st.describe(path, target)+" names a provider that does not serve that model")
			}
		case hasKey(aliases, target):
			st.errorAt(path, "target "+st.describe(path, target)+" is another alias; aliases do not chain")
		case strings.HasPrefix(target, "auto"):
			st.errorAt(path, "target "+st.describe(path, target)+": "+msgVirtualTarget)
		default:
			msg := "target " + st.describe(path, target) + " is not a model id or model@provider"
			if s := suggest(target, ids); s != "" && mayQuote(target) {
				msg += " (did you mean " + st.describe(modelPaths[s], s) + "?)"
			}
			st.errorAt(path, msg)
		}
	}
}

// hasKey reports whether m has key k.
func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}
