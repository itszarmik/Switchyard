package config

import (
	"fmt"
	"math/big"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"switchyard/internal/quality"
)

// Profile objectives (DESIGN §9.3).
const (
	// ObjectiveCost ranks eligible deployments by estimated cost.
	ObjectiveCost = "cost"
	// ObjectiveLatency ranks eligible deployments by estimated time to
	// first token.
	ObjectiveLatency = "latency"
)

// AssessorHeuristic is the only assessor type in Phase 5 (DESIGN §10.3).
const AssessorHeuristic = "heuristic"

// Policy settings (DESIGN §13). internal/policy defines the same values.
const (
	// SubstitutionPinned serves a concrete model name only with that model.
	SubstitutionPinned = "pinned"
	// SubstitutionEquivalent lets another model serve a concrete model name
	// when its estimated quality is close enough.
	SubstitutionEquivalent = "equivalent"
	// RoutingModeEnforce serves the routing decision.
	RoutingModeEnforce = "enforce"
	// RoutingModeObserve serves what was asked for and records the routing
	// decision it would have made.
	RoutingModeObserve = "observe"
	// OnUnmetBestEffort relaxes the soft constraints when nothing meets them.
	OnUnmetBestEffort = "best_effort"
	// OnUnmetReject rejects a request when nothing meets the soft
	// constraints.
	OnUnmetReject = "reject"
)

// autoModel is the virtual model that routes across every allowed model;
// "auto:<profile>" selects a profile.
const (
	autoModel  = "auto"
	autoPrefix = "auto:"
)

// MaxCostPerRequestUSD is the largest max_cost_per_request_usd a policy or
// rule may set, the same bound as a budget's limit_usd, so the cap in
// micro-USD always fits an int64.
const MaxCostPerRequestUSD = MaxBudgetLimitUSD

// assessorSignalNames are the signal names routing.assessor.weights
// accepts: the heuristic assessor's (quality.SignalNames).
var assessorSignalNames = quality.SignalNames()

// tagKeyRE is the syntax of an X-Switchyard-Tags key, as the gateway
// accepts it.
var tagKeyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)

// Messages of policy validation.
const (
	msgUnreachableRule = "unreachable: an earlier governance rule has an empty match and always applies first"
	msgNoBaseline      = "allows auto routing (no allowed_profiles restriction) but has no baseline_model; auto requests will be rejected"
	msgQualityRuleSet  = "a quality rule (one whose match uses task, difficulty_gte, difficulty_lt or est_input_tokens_gte) may only set min_quality and prefer"
)

// IsQualityRule reports whether a rule with the match m is a quality rule:
// one whose match uses a condition the request's content or the client's
// hints decide (task, difficulty_gte, difficulty_lt or
// est_input_tokens_gte). Every other rule is a governance rule.
//
// The two kinds form separate layers (DESIGN §13): the first matching
// governance rule applies, and then the first matching quality rule
// applies its min_quality and prefer on top, so that a request cannot
// steer past a governance rule through its content. A quality rule may set
// only min_quality and prefer.
func IsQualityRule(m *RuleMatch) bool {
	return m.Task != "" || m.DifficultyGte != 0 || m.DifficultyLt != 0 || m.EstInputTokensGte != 0
}

// policyRefs holds what policy validation resolves references against.
type policyRefs struct {
	// profiles are the profile names in force (configured or built in).
	profiles map[string]bool
	// policies maps each policy id to its first definition.
	policies map[string]*Policy
	// policyPaths maps each policy id to the path of its id.
	policyPaths map[string]string
}

// validateProfiles checks routing.profiles, default_profile,
// expected_output_tokens and assessor, and returns the profile names in
// force.
func (st *loadState) validateProfiles(r *Routing) map[string]bool {
	names := make([]string, 0, len(r.Profiles))
	for n := range r.Profiles {
		names = append(names, n)
	}
	slices.Sort(names)
	profiles := make(map[string]bool, len(names))
	for _, n := range names {
		p := r.Profiles[n]
		pp := joinPath("routing.profiles", shownKey(n))
		if !idRE.MatchString(n) {
			st.errorAt(pp, fmt.Sprintf("profile name must match %s", idRE))
		}
		profiles[n] = true
		st.checkUnit(pp+".min_quality", p.MinQuality)
		st.checkOneOf(pp+".objective", p.Objective, ObjectiveCost, ObjectiveLatency)
		if p.MaxTTFTMs < 0 {
			st.errorAt(pp+".max_ttft_ms", "must not be negative; use 0 for no latency target")
		}
	}
	if r.DefaultProfile != "" && !profiles[r.DefaultProfile] {
		msg := "unknown profile " + st.describe("routing.default_profile", r.DefaultProfile)
		if st.explicit("routing.profiles") {
			msg += "; configured profiles replace the built-in ones"
		}
		st.errorAt("routing.default_profile", msg+" (known profiles: "+strings.Join(names, ", ")+")")
	}
	if r.ExpectedOutputTokens < 0 {
		st.errorAt("routing.expected_output_tokens", "must be greater than 0")
	}

	st.checkOneOf("routing.assessor.type", r.Assessor.Type, AssessorHeuristic)
	keys := make([]string, 0, len(r.Assessor.Weights))
	for k := range r.Assessor.Weights {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		wp := joinPath("routing.assessor.weights", shownKey(k))
		if !slices.Contains(assessorSignalNames, k) {
			msg := "unknown signal"
			if shownKey(k) == k {
				if s := suggest(k, assessorSignalNames); s != "" {
					msg += fmt.Sprintf(" (did you mean %q?)", s)
				}
			}
			st.errorAt(wp, msg+"; use one of "+strings.Join(assessorSignalNames, ", "))
			continue
		}
		if w := r.Assessor.Weights[k]; w < -1 || w > 1 {
			st.errorAt(wp, "must be between -1 and 1, got "+strconv.FormatFloat(w, 'g', -1, 64))
		}
	}
	return profiles
}

// checkUnit reports an error unless 0 <= v <= 1.
func (st *loadState) checkUnit(path string, v float64) {
	if v < 0 || v > 1 {
		st.errorAt(path, "must be between 0 and 1, got "+strconv.FormatFloat(v, 'g', -1, 64))
	}
}

// checkProfileRef reports an error unless name, when set, is a profile in
// force.
func (st *loadState) checkProfileRef(path, name string, refs *policyRefs) {
	if name == "" || refs.profiles[name] {
		return
	}
	st.errorAt(path, "unknown profile "+st.describe(path, name)+" (known profiles: "+strings.Join(sortedKeys(refs.profiles), ", ")+")")
}

// validatePolicies checks every policy and its rules. models maps each
// model id to its first definition and aliases holds the aliases. It
// returns the references tenants, keys and budgets resolve against.
func (st *loadState) validatePolicies(ps []Policy, profiles map[string]bool, models map[string]*Model, aliases map[string]string) *policyRefs {
	refs := &policyRefs{profiles: profiles, policies: make(map[string]*Policy), policyPaths: make(map[string]string)}
	for i := range ps {
		p := &ps[i]
		base := indexPath("policies", i)
		before := len(refs.policyPaths)
		st.checkID(base+".id", "policy", p.ID, refs.policyPaths)
		if len(refs.policyPaths) > before {
			refs.policies[p.ID] = p
		}
		st.checkProfileRef(base+".profile", p.Profile, refs)
		seen := make(map[string]bool)
		for j, name := range p.AllowedProfiles {
			ap := indexPath(base+".allowed_profiles", j)
			if seen[name] {
				st.errorAt(ap, "duplicate profile "+st.describe(ap, name))
			}
			seen[name] = true
			if name == "" {
				st.errorAt(ap, msgRequired)
				continue
			}
			st.checkProfileRef(ap, name, refs)
		}
		st.checkGlobs(base+".allow", p.Allow)
		st.checkGlobs(base+".deny", p.Deny)
		st.checkGlobs(base+".prefer", p.Prefer)
		st.checkTags(base+".require_tags", p.RequireTags)
		st.checkSubstitution(base+".substitution", p.Substitution, false)
		st.checkOneOf(base+".routing_mode", p.RoutingMode, RoutingModeEnforce, RoutingModeObserve)
		st.checkBaselineModel(base+".baseline_model", p.BaselineModel, models, aliases)
		st.checkCostCap(base+".max_cost_per_request_usd", p.MaxCostPerRequestUSD)
		st.checkOneOf(base+".on_unmet", p.OnUnmet, OnUnmetBestEffort, OnUnmetReject)
		st.validateRules(base, p.Rules, refs)
	}
	return refs
}

// checkGlobs reports every entry of a glob list at path that is empty or
// not valid path.Match syntax.
func (st *loadState) checkGlobs(listPath string, globs []string) {
	for j, g := range globs {
		st.checkGlob(indexPath(listPath, j), g)
	}
}

// checkGlob reports an error unless g is a non-empty, valid path.Match
// pattern.
func (st *loadState) checkGlob(p, g string) {
	if g == "" {
		st.errorAt(p, "a glob pattern must not be empty")
		return
	}
	if _, err := path.Match(g, ""); err != nil {
		st.errorAt(p, "invalid glob pattern "+st.describe(p, g)+"; use *, ? and [...] as in path.Match")
	}
}

// checkSubstitution validates a substitution block. In a rule override an
// empty mode means "keep the policy's".
func (st *loadState) checkSubstitution(p string, s Substitution, override bool) {
	if !override || s.Mode != "" {
		st.checkOneOf(p+".mode", s.Mode, SubstitutionPinned, SubstitutionEquivalent)
	}
	st.checkUnit(p+".max_quality_drop", s.MaxQualityDrop)
}

// checkBaselineModel reports an error unless id, when set, is the id of a
// chat model in the catalog.
func (st *loadState) checkBaselineModel(p, id string, models map[string]*Model, aliases map[string]string) {
	if id == "" {
		return
	}
	m := models[id]
	switch {
	case m != nil && m.Type == ModelTypeChat:
	case m != nil:
		st.errorAt(p, "model "+st.describe(p, id)+" is not a chat model")
	case strings.Contains(id, "@"):
		st.errorAt(p, "must be a model id, not a deployment id (model@provider)")
	case hasKey(aliases, id):
		st.errorAt(p, st.describe(p, id)+" is an alias; use a model id")
	case id == autoModel || strings.HasPrefix(id, autoPrefix):
		st.errorAt(p, "must be a concrete model id, not a virtual model")
	default:
		ids := make([]string, 0, len(models))
		for k := range models {
			ids = append(ids, k)
		}
		slices.Sort(ids)
		msg := "unknown model " + st.describe(p, id)
		if s := suggest(id, ids); s != "" && mayQuote(id) {
			msg += fmt.Sprintf(" (did you mean %q?)", s)
		}
		st.errorAt(p, msg)
	}
}

// checkCostCap reports an error unless c, when set, is greater than 0 and
// at most MaxCostPerRequestUSD.
func (st *loadState) checkCostCap(p string, c Price) {
	switch {
	case !c.IsSet():
	case priceRat(c).Sign() <= 0:
		st.errorAt(p, "must be greater than 0; leave it unset for no cap")
	case new(big.Rat).SetInt64(MaxCostPerRequestUSD).Cmp(priceRat(c)) < 0:
		st.errorAt(p, fmt.Sprintf("must be at most %d", MaxCostPerRequestUSD))
	}
}

// validateRules checks the rules of the policy at base.
func (st *loadState) validateRules(base string, rules []Rule, refs *policyRefs) {
	names := make(map[string]string) // name -> path of its first definition
	unconditional := ""              // path of the first rule with an empty match
	for i := range rules {
		r := &rules[i]
		rp := indexPath(base+".rules", i)
		np := rp + ".name"
		switch {
		case r.Name == "":
			st.errorAt(np, msgRequired)
		case !idRE.MatchString(r.Name):
			st.errorAt(np, fmt.Sprintf("must match %s, got %s", idRE, st.describe(np, r.Name)))
		}
		if r.Name != "" {
			if prev, dup := names[r.Name]; dup {
				st.errorAt(np, fmt.Sprintf("duplicate rule name %s in this policy; first defined at %s", st.describe(np, r.Name), st.where(prev)))
			} else {
				names[r.Name] = np
			}
		}
		quality := IsQualityRule(&r.Match)
		if unconditional != "" && !quality {
			st.warnAt(rp, msgUnreachableRule+" ("+st.where(unconditional)+")")
		}
		st.validateRuleMatch(rp+".match", &r.Match, refs)
		st.validateOverride(rp+".set", &r.Set, refs)
		if quality {
			for _, f := range governanceOnlySet(&r.Set) {
				st.errorAt(rp+".set."+f, msgQualityRuleSet)
			}
		}
		if unconditional == "" && emptyMatch(&r.Match) {
			unconditional = rp
		}
	}
}

// governanceOnlySet returns the yaml names of the fields o sets that only a
// governance rule may set: every field but min_quality and prefer.
func governanceOnlySet(o *PolicyOverride) []string {
	var out []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"profile", o.Profile != ""},
		{"allow", o.Allow != nil},
		{"deny", o.Deny != nil},
		{"require_tags", o.RequireTags != nil},
		{"substitution", o.Substitution != (Substitution{})},
		{"routing_mode", o.RoutingMode != ""},
		{"max_cost_per_request_usd", o.MaxCostPerRequestUSD.IsSet()},
		{"on_unmet", o.OnUnmet != ""},
	} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// emptyMatch reports whether m sets no condition, so it always holds.
func emptyMatch(m *RuleMatch) bool {
	return m.Model == "" && m.Profile == "" && m.Task == "" &&
		m.DifficultyGte == 0 && m.DifficultyLt == 0 &&
		m.Key == "" && len(m.KeyTags) == 0 && len(m.Header) == 0 && len(m.Tags) == 0 &&
		m.Stream == "" && m.HasTools == "" && m.HasImages == "" &&
		m.EstInputTokensGte == 0
}

// validateRuleMatch checks the conditions of a rule.
func (st *loadState) validateRuleMatch(mp string, m *RuleMatch, refs *policyRefs) {
	if m.Model != "" {
		st.checkGlob(mp+".model", m.Model)
	}
	st.checkProfileRef(mp+".profile", m.Profile, refs)
	if m.Task != "" && !slices.Contains(qualityCategories, m.Task) {
		msg := "unknown task " + st.describe(mp+".task", m.Task)
		if s := suggest(m.Task, qualityCategories); s != "" && mayQuote(m.Task) {
			msg += fmt.Sprintf(" (did you mean %q?)", s)
		}
		st.errorAt(mp+".task", msg+"; use one of "+strings.Join(qualityCategories, ", "))
	}
	st.checkUnit(mp+".difficulty_gte", m.DifficultyGte)
	st.checkUnit(mp+".difficulty_lt", m.DifficultyLt)
	if m.DifficultyGte > 0 && m.DifficultyLt > 0 && m.DifficultyGte >= m.DifficultyLt {
		st.errorAt(mp+".difficulty_lt", "must be greater than difficulty_gte")
	}
	if m.Key != "" && !idRE.MatchString(m.Key) {
		st.errorAt(mp+".key", fmt.Sprintf("must match %s, got %s", idRE, st.describe(mp+".key", m.Key)))
	}
	st.checkTags(mp+".key_tags", m.KeyTags)

	names := make([]string, 0, len(m.Header))
	for h := range m.Header {
		names = append(names, h)
	}
	slices.Sort(names)
	seen := make(map[string]bool)
	for _, h := range names {
		hp := joinPath(mp+".header", shownKey(h))
		lower := strings.ToLower(h)
		switch {
		case !validHeaderName(h):
			st.errorAt(hp, "must be a valid HTTP header name")
		case seen[lower]:
			st.errorAt(hp, "duplicate header; header names are case-insensitive")
		}
		seen[lower] = true
		st.checkGlob(hp, m.Header[h])
	}

	keys := make([]string, 0, len(m.Tags))
	for k := range m.Tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		tp := joinPath(mp+".tags", shownKey(k))
		if !tagKeyRE.MatchString(k) {
			st.errorAt(tp, fmt.Sprintf("tag key must match %s", tagKeyRE))
		}
		st.checkGlob(tp, m.Tags[k])
	}

	for _, f := range []struct{ name, v string }{{"stream", m.Stream}, {"has_tools", m.HasTools}, {"has_images", m.HasImages}} {
		if f.v != "" && f.v != "true" && f.v != "false" {
			fp := mp + "." + f.name
			st.errorAt(fp, "must be true or false, got "+st.describe(fp, f.v))
		}
	}
	if m.EstInputTokensGte < 0 {
		st.errorAt(mp+".est_input_tokens_gte", "must not be negative")
	}
}

// validateOverride checks what a rule sets. Zero values mean "keep the
// policy's value", so only set fields are checked.
func (st *loadState) validateOverride(sp string, o *PolicyOverride, refs *policyRefs) {
	st.checkProfileRef(sp+".profile", o.Profile, refs)
	st.checkUnit(sp+".min_quality", o.MinQuality)
	st.checkGlobs(sp+".prefer", o.Prefer)
	st.checkGlobs(sp+".allow", o.Allow)
	st.checkGlobs(sp+".deny", o.Deny)
	st.checkTags(sp+".require_tags", o.RequireTags)
	st.checkSubstitution(sp+".substitution", o.Substitution, true)
	if o.RoutingMode != "" {
		st.checkOneOf(sp+".routing_mode", o.RoutingMode, RoutingModeEnforce, RoutingModeObserve)
	}
	st.checkCostCap(sp+".max_cost_per_request_usd", o.MaxCostPerRequestUSD)
	if o.OnUnmet != "" {
		st.checkOneOf(sp+".on_unmet", o.OnUnmet, OnUnmetBestEffort, OnUnmetReject)
	}
}

// validatePolicyRefs checks the policy references of tenants and keys, and
// the soft profiles of budgets.
func (st *loadState) validatePolicyRefs(ts []Tenant, refs *policyRefs) {
	for i := range ts {
		t := &ts[i]
		base := indexPath("tenants", i)
		eff, effPath := st.checkPolicyRef(base+".policy", t.Policy, refs)
		if t.Policy == "" {
			if p := refs.policies["default"]; p != nil {
				eff, effPath = p, base
			}
		}
		if eff != nil && len(eff.AllowedProfiles) == 0 && eff.BaselineModel == "" {
			st.warnAt(effPath, "policy "+strconv.Quote(eff.ID)+" "+msgNoBaseline)
		}
		for j := range t.Keys {
			st.checkPolicyRef(indexPath(base+".keys", j)+".policy", t.Keys[j].Policy, refs)
		}
		for j := range t.Budgets {
			b := &t.Budgets[j]
			if b.SoftProfile == "" {
				continue
			}
			sp := indexPath(base+".budgets", j) + ".soft_profile"
			if b.SoftAt == 0 {
				st.errorAt(sp, "requires soft_at")
			}
			st.checkProfileRef(sp, b.SoftProfile, refs)
		}
	}
}

// checkPolicyRef reports an error unless name, when set, is a defined
// policy id. It returns the policy and path when it is.
func (st *loadState) checkPolicyRef(p, name string, refs *policyRefs) (*Policy, string) {
	if name == "" {
		return nil, ""
	}
	if pol := refs.policies[name]; pol != nil {
		return pol, p
	}
	ids := make([]string, 0, len(refs.policies))
	for id := range refs.policies {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	msg := "unknown policy " + st.describe(p, name)
	switch s := suggest(name, ids); {
	case s != "" && mayQuote(name):
		msg += fmt.Sprintf(" (did you mean %q?)", s)
	case len(ids) == 0:
		msg += " (no policies are defined)"
	}
	st.errorAt(p, msg)
	return nil, ""
}
