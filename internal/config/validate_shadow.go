package config

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Upper bounds of the evaluation.shadow settings.
const (
	maxShadowConcurrency       = 1024
	maxShadowOutputTokens      = 1_000_000
	maxShadowVerifiedMinSample = 1_000_000_000
)

// pathJudgeModel is the config path of the judge model.
const pathJudgeModel = "evaluation.judge.model"

// validateShadow checks policies' shadow settings and evaluation.shadow
// (Phase 8). A policy with shadow.sample_rate > 0 sends a sample of its
// requests, and the answers, to the comparison and judge deployments, so it
// needs evaluation.judge.model, evaluation.shadow.daily_budget_usd and its
// own baseline_model, and both its baseline and the judge must have a
// deployment that the policy's allow, deny and require_tags permit. Rules
// can change those filters per request; the shadow runner checks the
// effective policy again before each sample.
func (st *loadState) validateShadow(c *Config, models map[string]*Model) {
	s := &c.Evaluation.Shadow
	const base = "evaluation.shadow"
	if s.DailyBudgetUSD.IsSet() {
		st.checkBudgetLimit(base+".daily_budget_usd", s.DailyBudgetUSD)
	}
	st.checkIntRange(base+".concurrency", s.Concurrency, 1, maxShadowConcurrency)
	if s.MaxOutputTokens < 1 || s.MaxOutputTokens > maxShadowOutputTokens {
		st.errorAt(base+".max_output_tokens", fmt.Sprintf("must be between 1 and %d, got %d", maxShadowOutputTokens, s.MaxOutputTokens))
	}
	st.checkIntRange(base+".verified_min_samples", s.VerifiedMinSamples, 1, maxShadowVerifiedMinSample)
	if v := s.VerifiedMinPassRate; !(v > 0 && v <= 1) {
		st.errorAt(base+".verified_min_pass_rate", "must be greater than 0 and at most 1, got "+strconv.FormatFloat(v, 'g', -1, 64))
	}

	var sampling []int
	for i := range c.Policies {
		p := indexPath("policies", i) + ".shadow.sample_rate"
		switch r := c.Policies[i].Shadow.SampleRate; {
		case !(r >= 0 && r <= 1): // NaN fails both
			st.errorAt(p, "must be between 0 and 1, got "+strconv.FormatFloat(r, 'g', -1, 64))
		case r > 0:
			sampling = append(sampling, i)
		}
	}
	if len(sampling) == 0 {
		return
	}
	why := "required when shadow sampling is on (" + indexPath("policies", sampling[0]) + ".shadow.sample_rate > 0)"
	if !s.DailyBudgetUSD.IsSet() {
		st.errorAt(base+".daily_budget_usd", why)
	}

	tags := make(map[string][]string, len(c.Providers))
	for _, p := range c.Providers {
		if _, dup := tags[p.ID]; !dup {
			tags[p.ID] = p.Tags
		}
	}
	judge := c.Evaluation.Judge.Model
	var judgeDeps []namedDeployment
	if judge == "" {
		st.errorAt(pathJudgeModel, why)
	} else {
		var msg string
		judgeDeps, msg = chatDeployments(judge, models, tags)
		// validateEvaluation may have reported the judge model already.
		switch {
		case msg != "" && !st.hasProblemAt(pathJudgeModel):
			st.errorAt(pathJudgeModel, msg+" "+st.describe(pathJudgeModel, judge))
		case msg == "" && !slices.ContainsFunc(judgeDeps, func(d namedDeployment) bool { return d.priced }):
			st.errorAt(pathJudgeModel, "the judge model "+st.describe(pathJudgeModel, judge)+
				" has no deployment with input and output prices; shadow sampling charges the judge's calls to evaluation.shadow.daily_budget_usd")
		}
	}

	for _, i := range sampling {
		p := &c.Policies[i]
		pp := indexPath("policies", i)
		rate := pp + ".shadow.sample_rate"
		if p.BaselineModel == "" {
			st.errorAt(pp+".baseline_model", "required when shadow.sample_rate > 0: the baseline model gives the comparison answer in enforce mode")
		} else if deps, msg := chatDeployments(p.BaselineModel, models, tags); msg == "" && !anyPermitted(p, deps) {
			// An unknown or non-chat baseline is reported by policy
			// validation.
			st.errorAt(rate, "the policy's allow, deny and require_tags permit no deployment of its baseline_model "+
				st.describe(pp+".baseline_model", p.BaselineModel)+", which shadow sampling asks for the comparison answer")
		}
		if len(judgeDeps) > 0 && !anyPermitted(p, judgeDeps) {
			st.errorAt(rate, "the policy's allow, deny and require_tags permit no deployment of the judge model "+
				st.describe(pathJudgeModel, judge)+"; shadow sampling sends this policy's prompts and answers to the judge")
		}
	}
}

// namedDeployment is a deployment's id (model@provider), its tags (the
// provider's followed by the deployment's own), and whether its chat
// prices are known.
type namedDeployment struct {
	id     string
	tags   []string
	priced bool
}

// chatDeployments returns the deployments that ref, a chat model id or
// model@provider, names; or, when ref names none, a message saying why.
func chatDeployments(ref string, models map[string]*Model, providerTags map[string][]string) ([]namedDeployment, string) {
	id, provider, pinned := strings.Cut(ref, "@")
	m := models[id]
	switch {
	case m == nil:
		return nil, "unknown model"
	case m.Type != ModelTypeChat:
		return nil, "must be a chat model, not the embedding model"
	}
	var out []namedDeployment
	for _, d := range m.Deployments {
		if pinned && d.Provider != provider {
			continue
		}
		out = append(out, namedDeployment{
			id:     m.ID + "@" + d.Provider,
			tags:   append(slices.Clone(providerTags[d.Provider]), d.Tags...),
			priced: PricingKnown(ModelTypeChat, d.Pricing),
		})
	}
	if len(out) == 0 {
		return nil, "no deployment of the model is on the provider of"
	}
	return out, ""
}

// anyPermitted reports whether p's deny, allow and require_tags let one of
// deps serve, as the router applies them (router.PolicyReason).
func anyPermitted(p *Policy, deps []namedDeployment) bool {
	return slices.ContainsFunc(deps, func(d namedDeployment) bool {
		for _, g := range p.Deny {
			if globMatch(g, d.id) {
				return false
			}
		}
		if len(p.Allow) > 0 && !slices.ContainsFunc(p.Allow, func(g string) bool { return globMatch(g, d.id) }) {
			return false
		}
		for _, t := range p.RequireTags {
			if !slices.Contains(d.tags, t) {
				return false
			}
		}
		return true
	})
}

// globMatch reports whether the path.Match pattern g matches s; a
// malformed pattern, which policy validation reports, matches nothing.
func globMatch(g, s string) bool {
	ok, err := path.Match(g, s)
	return err == nil && ok
}

// hasProblemAt reports whether a problem has been recorded for path.
func (st *loadState) hasProblemAt(path string) bool {
	return slices.ContainsFunc(st.problems, func(p Problem) bool { return p.Path == path })
}
