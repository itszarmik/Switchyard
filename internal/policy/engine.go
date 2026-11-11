package policy

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"switchyard/internal/config"
)

// defaultPolicyID is the policy a tenant without a policy uses, if defined.
const defaultPolicyID = "default"

// Warnings for header values that were ignored (DESIGN §5).
const (
	warnProfileNotAllowed = HeaderProfile + " ignored: the profile is not allowed by this policy"
	warnProfileModel      = HeaderProfile + " ignored: the model field already selects a profile"
	warnProfileBudget     = HeaderProfile + " ignored: the budget's soft profile is in force"
	warnMaxCost           = HeaderMaxCostUSD + " ignored: a header may only lower the policy's cost cap"
	warnMinQuality        = HeaderMinQuality + " ignored: a header may only raise the policy's quality bar"
	warnMinQualityCeiling = HeaderMinQuality + " ignored: it is above the quality bar of every profile this policy allows"
	warnMinQualityBudget  = HeaderMinQuality + " ignored: it is above the quality bar of the budget's soft profile"
	warnMaxTTFT           = HeaderMaxTTFTMs + " ignored: a header may only lower the policy's latency target"
	// warnProfileRule is followed by the rule's name.
	warnProfileRule = "the requested profile is replaced by the profile of policy rule "
)

// Engine resolves the effective policy of a request (DESIGN §13). It is
// immutable and safe for concurrent use; build a new one on reload.
type Engine struct {
	profiles       map[string]config.Profile
	profileNames   []string
	defaultProfile string
	policies       map[string]*compiled
	builtin        *compiled
}

// compiled is a policy with its defaults applied and its money converted.
type compiled struct {
	id                               string
	profile                          string
	allowedProfiles                  []string
	allow, deny, prefer, requireTags []string
	substitution                     string
	maxQualityDrop                   float64
	routingMode                      string
	baseline                         string
	capNano                          int64
	onUnmet                          string
	// governance and quality are the policy's rules of each layer, in
	// order (see config.IsQualityRule).
	governance, quality []rule
	// maxSelectableQuality is the highest min_quality among the profiles
	// callers may select, the most X-Switchyard-Min-Quality may ask for.
	maxSelectableQuality float64
}

// rule is a policy rule with its cost cap converted.
type rule struct {
	*config.Rule
	// capNano is the rule's max_cost_per_request_usd in nano-USD, or 0
	// when the rule keeps the policy's cap.
	capNano int64
}

// New builds an engine from a configuration. A loaded configuration has
// already been validated; New still reports the problems that would make
// resolution wrong, such as a reference to an unknown profile, so that a
// configuration built by hand fails early. Missing routing defaults
// (profiles, default_profile) and policy defaults are filled in.
func New(cfg *config.Config) (*Engine, error) {
	profiles := cfg.Routing.Profiles
	if len(profiles) == 0 {
		profiles = config.DefaultProfiles()
	}
	e := &Engine{
		profiles:       make(map[string]config.Profile, len(profiles)),
		defaultProfile: cfg.Routing.DefaultProfile,
		policies:       make(map[string]*compiled, len(cfg.Policies)),
	}
	for name, p := range profiles {
		if p.Objective == "" {
			p.Objective = ObjectiveCost
		}
		e.profiles[name] = p
		e.profileNames = append(e.profileNames, name)
	}
	slices.Sort(e.profileNames)
	if e.defaultProfile == "" {
		e.defaultProfile = config.DefaultProfileName
	}
	var errs []error
	if _, ok := e.profiles[e.defaultProfile]; !ok {
		errs = append(errs, fmt.Errorf("routing.default_profile %q is not a profile", e.defaultProfile))
	}
	e.builtin = &compiled{
		substitution:   SubstitutionPinned,
		maxQualityDrop: config.DefaultMaxQualityDrop,
		routingMode:    ModeEnforce,
		onUnmet:        OnUnmetBestEffort,
	}
	e.builtin.maxSelectableQuality = e.maxQuality(e.allowed(e.builtin))
	for i := range cfg.Policies {
		c, err := e.compile(&cfg.Policies[i])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, dup := e.policies[c.id]; dup {
			errs = append(errs, fmt.Errorf("policy %q is defined twice", c.id))
			continue
		}
		e.policies[c.id] = c
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	return e, nil
}

// compile checks p's references and fills its defaults.
func (e *Engine) compile(p *config.Policy) (*compiled, error) {
	if p.ID == "" {
		return nil, errors.New("a policy has no id")
	}
	c := &compiled{
		id:              p.ID,
		profile:         p.Profile,
		allowedProfiles: p.AllowedProfiles,
		allow:           p.Allow,
		deny:            p.Deny,
		prefer:          p.Prefer,
		requireTags:     p.RequireTags,
		substitution:    cmpOr(p.Substitution.Mode, SubstitutionPinned),
		maxQualityDrop:  p.Substitution.MaxQualityDrop,
		routingMode:     cmpOr(p.RoutingMode, ModeEnforce),
		baseline:        p.BaselineModel,
		capNano:         priceNano(p.MaxCostPerRequestUSD),
		onUnmet:         cmpOr(p.OnUnmet, OnUnmetBestEffort),
	}
	if p.Substitution.Mode == "" && p.Substitution.MaxQualityDrop == 0 {
		// Not loaded through config.Load, which always sets the mode.
		c.maxQualityDrop = config.DefaultMaxQualityDrop
	}
	refs := []struct{ what, name string }{{"profile", p.Profile}}
	for _, a := range p.AllowedProfiles {
		refs = append(refs, struct{ what, name string }{"allowed_profiles entry", a})
	}
	for i := range p.Rules {
		r := rule{Rule: &p.Rules[i], capNano: priceNano(p.Rules[i].Set.MaxCostPerRequestUSD)}
		refs = append(refs, struct{ what, name string }{"rule " + r.Name + " profile", r.Set.Profile})
		if !config.IsQualityRule(&r.Match) {
			c.governance = append(c.governance, r)
			continue
		}
		s := &r.Set
		if s.Profile != "" || s.Allow != nil || s.Deny != nil || s.RequireTags != nil || s.Substitution != (config.Substitution{}) ||
			s.RoutingMode != "" || r.capNano != 0 || s.OnUnmet != "" {
			return nil, fmt.Errorf("policy %q: rule %s matches on task, difficulty or input size, so it may only set min_quality and prefer", p.ID, r.Name)
		}
		c.quality = append(c.quality, r)
	}
	for _, ref := range refs {
		if _, ok := e.profiles[ref.name]; ref.name != "" && !ok {
			return nil, fmt.Errorf("policy %q: %s %q is not a profile", p.ID, ref.what, ref.name)
		}
	}
	c.maxSelectableQuality = e.maxQuality(e.allowed(c))
	return c, nil
}

// maxQuality returns the highest min_quality among the named profiles that
// exist.
func (e *Engine) maxQuality(names []string) float64 {
	q := 0.0
	for _, n := range names {
		if p, ok := e.profiles[n]; ok {
			q = max(q, p.MinQuality)
		}
	}
	return q
}

// cmpOr returns v, or def if v is empty.
func cmpOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ProfileNames returns the names of every profile in force, sorted.
func (e *Engine) ProfileNames() []string { return slices.Clone(e.profileNames) }

// SelectableProfiles returns the profiles the caller (tenant and key) may
// select with "auto:<profile>", sorted: the base policy's allowed_profiles,
// or every profile when it sets none. It is empty when the base policy has
// no baseline_model, since auto routing is then disabled. The gateway lists
// them as virtual models in /v1/models.
func (e *Engine) SelectableProfiles(tenant config.Tenant, keyID string) ([]string, error) {
	pol, err := e.base(tenant, keyID)
	if err != nil {
		return nil, err
	}
	if pol.baseline == "" {
		return nil, nil
	}
	return e.allowed(pol), nil
}

// allowed returns the profiles callers may select under pol, sorted.
func (e *Engine) allowed(pol *compiled) []string {
	if len(pol.allowedProfiles) == 0 {
		return slices.Clone(e.profileNames)
	}
	out := slices.Clone(pol.allowedProfiles)
	slices.Sort(out)
	return out
}

// selectable reports whether name exists and pol lets callers select it.
func (e *Engine) selectable(pol *compiled, name string) (exists, allowed bool) {
	if _, ok := e.profiles[name]; !ok {
		return false, false
	}
	return true, len(pol.allowedProfiles) == 0 || slices.Contains(pol.allowedProfiles, name)
}

// Resolve returns the effective policy for request r from the key keyID of
// tenant (DESIGN §13):
//
//   - the base policy is the key's policy, else the tenant's, else the
//     policy named "default", else the built-in policy;
//   - its rules form two layers (see config.IsQualityRule): the first
//     governance rule whose conditions all hold applies its overrides, and
//     then the first quality rule whose conditions all hold applies its
//     min_quality and prefer on top, so that a quality rule, which the
//     request's content or the client's hints can reach, never stops a
//     governance rule from applying;
//   - the profile is the governance rule's, which replaces the one the
//     request selects with a warning; else the one the request selects
//     (r.Profile, from "auto:<profile>", else the X-Switchyard-Profile
//     header); else the policy's; else routing.default_profile;
//   - constraints come from that profile, then the rules' min_quality, then
//     header tightening, which can only narrow them. X-Switchyard-Min-Quality
//     may ask for at most the highest min_quality among the profiles the
//     caller may select.
//
// r.Profile must name a profile the policy allows, or Resolve returns a
// *ModelNotSupportedError. A malformed policy header is an
// *InvalidHeaderError. A header value that would loosen the policy, or go
// beyond what the caller may select, or an X-Switchyard-Profile the policy
// does not allow, is ignored and recorded in Effective.Warnings. If keyID
// is empty, r.KeyID is used.
//
// Resolve does not check that the policy allows auto routing at all; the
// gateway calls CheckAuto for auto requests.
func (e *Engine) Resolve(tenant config.Tenant, keyID string, r Request) (Effective, error) {
	return e.resolve(tenant, keyID, r, "")
}

// ResolveDowngraded is Resolve for an auto request whose budget's soft
// threshold has been crossed (DESIGN §9.5): softProfile replaces the
// selected, rule and policy profiles. Rule min_quality and header
// tightening still apply on top of it, with X-Switchyard-Min-Quality
// asking for at most softProfile's min_quality.
func (e *Engine) ResolveDowngraded(tenant config.Tenant, keyID string, r Request, softProfile string) (Effective, error) {
	if softProfile == "" {
		return Effective{}, errors.New("policy: empty soft profile")
	}
	return e.resolve(tenant, keyID, r, softProfile)
}

func (e *Engine) resolve(tenant config.Tenant, keyID string, r Request, soft string) (Effective, error) {
	pol, err := e.base(tenant, keyID)
	if err != nil {
		return Effective{}, err
	}
	hv, err := parseHeaders(r.Headers)
	if err != nil {
		return Effective{}, err
	}
	var warnings []string

	requested, fromHeader := "", false
	if r.Profile != "" {
		if exists, ok := e.selectable(pol, r.Profile); !ok {
			return Effective{}, profileError(r.Profile, exists, e.allowed(pol))
		}
		requested = r.Profile
	}
	if hv.hasProfile {
		_, ok := e.selectable(pol, hv.profile)
		switch {
		case requested != "":
			if hv.profile != requested {
				warnings = append(warnings, warnProfileModel)
			}
		case !ok:
			warnings = append(warnings, warnProfileNotAllowed+" (allowed: "+strings.Join(e.allowed(pol), ", ")+")")
		default:
			requested, fromHeader = hv.profile, true
		}
	}

	if keyID == "" {
		keyID = r.KeyID
	}
	gov := firstMatch(pol.governance, &r, keyID, requested)
	qual := firstMatch(pol.quality, &r, keyID, requested)

	var profile string
	switch {
	case soft != "":
		profile = soft
		if fromHeader && requested != soft {
			warnings = append(warnings, warnProfileBudget)
		}
	case gov != nil && gov.Set.Profile != "":
		// Rules are trusted operator config: a rule's profile wins over
		// the client's choice, which is only a request.
		profile = gov.Set.Profile
		if requested != "" && requested != profile {
			warnings = append(warnings, warnProfileRule+gov.Name)
		}
	case requested != "":
		profile = requested
	case pol.profile != "":
		profile = pol.profile
	default:
		profile = e.defaultProfile
	}
	prof, ok := e.profiles[profile]
	if !ok {
		return Effective{}, fmt.Errorf("policy: profile %q is not defined", profile)
	}
	ceiling, ceilingWarning := pol.maxSelectableQuality, warnMinQualityCeiling
	if soft != "" {
		ceiling, ceilingWarning = prof.MinQuality, warnMinQualityBudget
	}

	eff := Effective{
		PolicyID: pol.id,
		Profile:  profile,
		Constraints: Constraints{
			MinQuality: prof.MinQuality,
			Objective:  prof.Objective,
			MaxTTFT:    time.Duration(prof.MaxTTFTMs) * time.Millisecond,
		},
		AllowedProfiles:       slices.Clone(pol.allowedProfiles),
		Allow:                 slices.Clone(pol.allow),
		Deny:                  slices.Clone(pol.deny),
		Prefer:                slices.Clone(pol.prefer),
		RequireTags:           slices.Clone(pol.requireTags),
		Substitution:          pol.substitution,
		MaxQualityDrop:        pol.maxQualityDrop,
		RoutingMode:           pol.routingMode,
		BaselineModel:         pol.baseline,
		MaxCostPerRequestNano: pol.capNano,
		OnUnmet:               pol.onUnmet,
	}
	if gov != nil {
		applyOverride(&eff, gov)
	}
	if qual != nil {
		applyQualityOverride(&eff, qual)
	}
	warnings = append(warnings, tighten(&eff, hv, ceiling, ceilingWarning)...)
	eff.Warnings = warnings
	return eff, nil
}

// firstMatch returns the first of rules whose conditions all hold for r
// (see ruleMatches), or nil.
func firstMatch(rules []rule, r *Request, keyID, requested string) *rule {
	for i := range rules {
		if ruleMatches(&rules[i].Match, r, keyID, requested) {
			return &rules[i]
		}
	}
	return nil
}

// base returns the base policy of the key keyID of tenant.
func (e *Engine) base(tenant config.Tenant, keyID string) (*compiled, error) {
	id := tenant.Policy
	for i := range tenant.Keys {
		if k := &tenant.Keys[i]; k.ID == keyID && k.Policy != "" {
			id = k.Policy
			break
		}
	}
	if id == "" {
		if p := e.policies[defaultPolicyID]; p != nil {
			return p, nil
		}
		return e.builtin, nil
	}
	p := e.policies[id]
	if p == nil {
		return nil, fmt.Errorf("policy: tenant %q uses unknown policy %q", tenant.ID, id)
	}
	return p, nil
}

// applyOverride applies what the governance rule g sets to eff. Rules are
// trusted operator config, so they may loosen the policy as well as
// tighten it. Zero values and nil slices keep the policy's value; an empty
// list replaces it. The profile was chosen already.
func applyOverride(eff *Effective, g *rule) {
	s := &g.Set
	eff.MatchedRule = g.Name
	if s.MinQuality > 0 {
		eff.Constraints.MinQuality = s.MinQuality
		eff.Constraints.QualityFloor = s.MinQuality
	}
	if s.Prefer != nil {
		eff.Prefer = slices.Clone(s.Prefer)
	}
	if s.Allow != nil {
		eff.Allow = slices.Clone(s.Allow)
	}
	if s.Deny != nil {
		eff.Deny = slices.Clone(s.Deny)
	}
	if s.RequireTags != nil {
		eff.RequireTags = slices.Clone(s.RequireTags)
	}
	if s.Substitution.Mode != "" {
		eff.Substitution = s.Substitution.Mode
	}
	if s.Substitution.MaxQualityDrop > 0 {
		eff.MaxQualityDrop = s.Substitution.MaxQualityDrop
	}
	if s.RoutingMode != "" {
		eff.RoutingMode = s.RoutingMode
	}
	if g.capNano > 0 {
		eff.MaxCostPerRequestNano = g.capNano
	}
	if s.OnUnmet != "" {
		eff.OnUnmet = s.OnUnmet
	}
}

// applyQualityOverride applies what the quality rule q sets to eff, on top
// of the governance rule: min_quality and prefer, the only fields a
// quality rule may set.
func applyQualityOverride(eff *Effective, q *rule) {
	eff.MatchedQualityRule = q.Name
	if q.Set.MinQuality > 0 {
		eff.Constraints.MinQuality = q.Set.MinQuality
		eff.Constraints.QualityFloor = q.Set.MinQuality
	}
	if q.Set.Prefer != nil {
		eff.Prefer = slices.Clone(q.Set.Prefer)
	}
}

// tighten applies the header values that narrow eff and returns a warning
// for each one that would have loosened it. A value equal to the policy's
// changes nothing and is not a warning. X-Switchyard-Min-Quality may raise
// the bar up to ceiling; a higher value is ignored with ceilingWarning, so
// that it cannot ask for more than the profiles the caller may select. An
// accepted value also raises the quality floor.
func tighten(eff *Effective, hv headerValues, ceiling float64, ceilingWarning string) []string {
	var warnings []string
	if c := hv.maxCostNano; c > 0 {
		switch cur := eff.MaxCostPerRequestNano; {
		case cur == 0 || c < cur:
			eff.MaxCostPerRequestNano = c
		case c > cur:
			warnings = append(warnings, warnMaxCost)
		}
	}
	if hv.hasMinQuality {
		c := &eff.Constraints
		switch q := hv.minQuality; {
		case q < c.MinQuality:
			warnings = append(warnings, warnMinQuality)
		case q > c.MinQuality && q > ceiling:
			warnings = append(warnings, ceilingWarning)
		default:
			c.MinQuality = q
			c.QualityFloor = max(c.QualityFloor, q)
		}
	}
	if t := hv.maxTTFT; t > 0 {
		switch cur := eff.Constraints.MaxTTFT; {
		case cur == 0 || t < cur:
			eff.Constraints.MaxTTFT = t
		case t > cur:
			warnings = append(warnings, warnMaxTTFT)
		}
	}
	return warnings
}

// ruleMatches reports whether every condition of m holds for r, from the
// key keyID, with requested the profile the request selected ("" for
// none). Header names match case-insensitively; header and tag values match
// their globs case-sensitively.
func ruleMatches(m *config.RuleMatch, r *Request, keyID, requested string) bool {
	switch {
	case m.Model != "" && !glob(m.Model, r.Model),
		m.Profile != "" && m.Profile != requested,
		m.Task != "" && m.Task != r.Task,
		m.DifficultyGte > 0 && r.Difficulty < m.DifficultyGte,
		m.DifficultyLt > 0 && r.Difficulty >= m.DifficultyLt,
		m.Key != "" && m.Key != keyID,
		!boolMatches(m.Stream, r.Stream),
		!boolMatches(m.HasTools, r.HasTools),
		!boolMatches(m.HasImages, r.HasImages),
		m.EstInputTokensGte > 0 && r.EstInputTokens < m.EstInputTokensGte:
		return false
	}
	for _, t := range m.KeyTags {
		if !slices.Contains(r.KeyTags, t) {
			return false
		}
	}
	for name, g := range m.Header {
		if !slices.ContainsFunc(headerValuesOf(r.Headers, name), func(v string) bool { return glob(g, v) }) {
			return false
		}
	}
	for k, g := range m.Tags {
		v, ok := r.Tags[k]
		if !ok || !glob(g, v) {
			return false
		}
	}
	return true
}

// boolMatches reports whether v holds for a condition c of "", "true" or
// "false".
func boolMatches(c string, v bool) bool {
	switch c {
	case "true":
		return v
	case "false":
		return !v
	default:
		return true
	}
}

// glob reports whether s matches the path.Match pattern g. A malformed
// pattern, which validation rejects, matches nothing.
func glob(g, s string) bool {
	ok, err := path.Match(g, s)
	return err == nil && ok
}
