package policy

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/config"
)

// catalog is the provider and model part of every test config.
const catalog = `version: 1
providers:
  - {id: a, type: openai, base_url: "https://a.example/v1"}
  - {id: b, type: openai, base_url: "https://b.example/v1"}
models:
  - id: m
    deployments:
      - {provider: a, upstream_model: m, pricing: {input: 1, output: 2}}
      - {provider: b, upstream_model: m, pricing: {input: 1, output: 2}}
`

// hash returns a well-formed key hash made of the hex digit c.
func hash(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// load loads a config from YAML text through config.Load, so defaults and
// validation apply as in production.
func load(t *testing.T, text string) *config.Config {
	t.Helper()
	files := map[string]string{"cfg.yaml": text}
	snap, err := config.Load("cfg.yaml", config.LoadOptions{
		LookupEnv: func(string) (string, bool) { return "", false },
		ReadFile: func(name string) ([]byte, error) {
			if s, ok := files[filepath.Clean(name)]; ok {
				return []byte(s), nil
			}
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		},
		Glob: func(string) ([]string, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("config.Load:\n%v", err)
	}
	return snap.Config()
}

// newEngine loads text and builds an engine from it.
func newEngine(t *testing.T, text string) (*Engine, *config.Config) {
	t.Helper()
	cfg := load(t, text)
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, cfg
}

// tenant returns the tenant with the given id.
func tenant(t *testing.T, cfg *config.Config, id string) config.Tenant {
	t.Helper()
	for _, tn := range cfg.Tenants {
		if tn.ID == id {
			return tn
		}
	}
	t.Fatalf("no tenant %q", id)
	return config.Tenant{}
}

// mainConfig exercises base policy selection, profiles and overrides.
const mainConfig = catalog + `
policies:
  - id: default
    baseline_model: m
    max_cost_per_request_usd: "0.50"
    allow: ["m@*"]
    deny: ["m@b"]
    prefer: ["m@a"]
    rules:
      - name: staging-is-cheap
        match: {header: {x-env: staging}}
        set: {profile: cheap}
      - name: eu-keys-stay-in-eu
        match: {key_tags: [eu]}
        set: {require_tags: [region_eu]}
      - name: code-needs-quality
        match: {task: coding, difficulty_gte: 0.5}
        set: {min_quality: 0.85}
      - name: loosen-everything
        match: {tags: {feature: "batch-*"}}
        set: {allow: [], deny: [], prefer: ["*@b"], max_cost_per_request_usd: 2, substitution: {mode: equivalent, max_quality_drop: 0.2}, routing_mode: observe, on_unmet: reject, min_quality: 0.5}
  - id: strict
    profile: cheap
    allowed_profiles: [cheap, balanced]
    baseline_model: m
    require_tags: [zdr]
    substitution: {mode: equivalent, max_quality_drop: 0.1}
    routing_mode: observe
    on_unmet: reject
  - id: nobase
routing:
  profiles:
    cheap: {min_quality: 0.65}
    balanced: {min_quality: 0.8}
    quality: {min_quality: 0.92}
    fast: {min_quality: 0.8, objective: latency, max_ttft_ms: 800}
tenants:
  - id: acme
    keys:
      - {id: acme-prod, hash: ` + "\"sha256:" + "1111111111111111111111111111111111111111111111111111111111111111" + `", tags: [prod]}
      - {id: acme-strict, hash: "` + "sha256:2222222222222222222222222222222222222222222222222222222222222222" + `", policy: strict}
  - id: corp
    policy: strict
    keys:
      - {id: corp-a, hash: "` + "sha256:3333333333333333333333333333333333333333333333333333333333333333" + `"}
      - {id: corp-b, hash: "` + "sha256:4444444444444444444444444444444444444444444444444444444444444444" + `", policy: default}
  - id: lab
    policy: nobase
    keys:
      - {id: lab-a, hash: "` + "sha256:5555555555555555555555555555555555555555555555555555555555555555" + `"}
`

func TestResolveBasePolicy(t *testing.T) {
	e, cfg := newEngine(t, mainConfig)
	defaultEff := Effective{
		PolicyID: "default", Profile: "balanced",
		Constraints: Constraints{MinQuality: 0.8, Objective: ObjectiveCost},
		Allow:       []string{"m@*"}, Deny: []string{"m@b"}, Prefer: []string{"m@a"},
		Substitution: SubstitutionPinned, MaxQualityDrop: 0.05, RoutingMode: ModeEnforce,
		BaselineModel: "m", MaxCostPerRequestNano: 500_000_000, OnUnmet: OnUnmetBestEffort,
	}
	strictEff := Effective{
		PolicyID: "strict", Profile: "cheap",
		Constraints:     Constraints{MinQuality: 0.65, Objective: ObjectiveCost},
		AllowedProfiles: []string{"cheap", "balanced"}, RequireTags: []string{"zdr"},
		Substitution: SubstitutionEquivalent, MaxQualityDrop: 0.1, RoutingMode: ModeObserve,
		BaselineModel: "m", OnUnmet: OnUnmetReject,
	}
	nobaseEff := Effective{
		PolicyID: "nobase", Profile: "balanced",
		Constraints:  Constraints{MinQuality: 0.8, Objective: ObjectiveCost},
		Substitution: SubstitutionPinned, MaxQualityDrop: 0.05, RoutingMode: ModeEnforce, OnUnmet: OnUnmetBestEffort,
	}
	tests := []struct {
		name   string
		tenant string
		key    string
		want   Effective
	}{
		{"tenant without policy uses default", "acme", "acme-prod", defaultEff},
		{"key policy replaces the default", "acme", "acme-strict", strictEff},
		{"tenant policy", "corp", "corp-a", strictEff},
		{"key policy replaces the tenant's", "corp", "corp-b", defaultEff},
		{"unknown key uses the tenant's", "corp", "nobody", strictEff},
		{"policy without baseline", "lab", "lab-a", nobaseEff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := e.Resolve(tenant(t, cfg, tt.tenant), tt.key, Request{Model: "auto"})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("effective (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResolveBuiltIn(t *testing.T) {
	e, cfg := newEngine(t, catalog+"tenants:\n  - {id: acme, keys: [{id: k, hash: \""+hash("a")+"\"}]}\n")
	got, err := e.Resolve(cfg.Tenants[0], "k", Request{Model: "auto"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := Effective{
		Profile:      "balanced",
		Constraints:  Constraints{MinQuality: 0.8, Objective: ObjectiveCost},
		Substitution: SubstitutionPinned, MaxQualityDrop: 0.05, RoutingMode: ModeEnforce, OnUnmet: OnUnmetBestEffort,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("built-in effective (-want +got):\n%s", diff)
	}
	var mns *ModelNotSupportedError
	if err := CheckAuto(got); !errors.As(err, &mns) || err.Error() != "this policy has no baseline_model; auto routing is disabled" {
		t.Errorf("CheckAuto(built-in) = %v, want the no-baseline error", err)
	}
	if got, _ := e.SelectableProfiles(cfg.Tenants[0], "k"); got != nil {
		t.Errorf("SelectableProfiles(built-in) = %v, want none", got)
	}
	if got, want := e.ProfileNames(), []string{"balanced", "cheap", "fast", "quality"}; !cmp.Equal(got, want) {
		t.Errorf("ProfileNames() = %v, want %v", got, want)
	}
}

func TestResolveProfile(t *testing.T) {
	e, cfg := newEngine(t, mainConfig)
	acme, corp := tenant(t, cfg, "acme"), tenant(t, cfg, "corp")
	hdr := func(kv ...string) map[string][]string {
		h := map[string][]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			h[kv[i]] = append(h[kv[i]], kv[i+1])
		}
		return h
	}
	tests := []struct {
		name        string
		tenant      config.Tenant
		key         string
		req         Request
		profile     string
		minQuality  float64
		maxTTFT     time.Duration
		objective   string
		rule        string
		warnings    []string
		errContains string
	}{
		{name: "default profile", tenant: acme, key: "acme-prod", req: Request{Model: "auto"},
			profile: "balanced", minQuality: 0.8, objective: ObjectiveCost},
		{name: "policy profile", tenant: corp, key: "corp-a", req: Request{Model: "auto"},
			profile: "cheap", minQuality: 0.65, objective: ObjectiveCost},
		{name: "model field profile", tenant: acme, key: "acme-prod", req: Request{Model: "auto:fast", Profile: "fast"},
			profile: "fast", minQuality: 0.8, maxTTFT: 800 * time.Millisecond, objective: ObjectiveLatency},
		{name: "rule profile", tenant: acme, key: "acme-prod", req: Request{Model: "auto", Headers: hdr("X-Env", "staging")},
			profile: "cheap", minQuality: 0.65, objective: ObjectiveCost, rule: "staging-is-cheap"},
		{name: "the rule's profile beats the model field's", tenant: acme, key: "acme-prod",
			req:     Request{Model: "auto:quality", Profile: "quality", Headers: hdr("X-Env", "staging")},
			profile: "cheap", minQuality: 0.65, objective: ObjectiveCost, rule: "staging-is-cheap",
			warnings: []string{warnProfileRule + "staging-is-cheap"}},
		{name: "the rule's profile beats the header's", tenant: acme, key: "acme-prod",
			req:     Request{Model: "auto", Headers: hdr("X-Env", "staging", "X-Switchyard-Profile", "quality")},
			profile: "cheap", minQuality: 0.65, objective: ObjectiveCost, rule: "staging-is-cheap",
			warnings: []string{warnProfileRule + "staging-is-cheap"}},
		{name: "the rule's profile chosen by the client too", tenant: acme, key: "acme-prod",
			req:     Request{Model: "auto:cheap", Profile: "cheap", Headers: hdr("X-Env", "staging")},
			profile: "cheap", minQuality: 0.65, objective: ObjectiveCost, rule: "staging-is-cheap"},
		{name: "header profile", tenant: acme, key: "acme-prod", req: Request{Model: "auto", Headers: hdr("X-Switchyard-Profile", "quality")},
			profile: "quality", minQuality: 0.92, objective: ObjectiveCost},
		{name: "header profile lower-case name", tenant: acme, key: "acme-prod", req: Request{Model: "auto", Headers: hdr("x-switchyard-profile", " quality ")},
			profile: "quality", minQuality: 0.92, objective: ObjectiveCost},
		{name: "header profile not allowed is ignored", tenant: corp, key: "corp-a", req: Request{Model: "auto", Headers: hdr("X-Switchyard-Profile", "quality")},
			profile: "cheap", minQuality: 0.65, objective: ObjectiveCost,
			warnings: []string{warnProfileNotAllowed + " (allowed: balanced, cheap)"}},
		{name: "header profile unknown is ignored", tenant: acme, key: "acme-prod", req: Request{Model: "auto", Headers: hdr("X-Switchyard-Profile", "turbo")},
			profile: "balanced", minQuality: 0.8, objective: ObjectiveCost,
			warnings: []string{warnProfileNotAllowed + " (allowed: balanced, cheap, fast, quality)"}},
		{name: "header profile allowed", tenant: corp, key: "corp-a", req: Request{Model: "auto", Headers: hdr("X-Switchyard-Profile", "balanced")},
			profile: "balanced", minQuality: 0.8, objective: ObjectiveCost},
		{name: "header profile beside the model field's", tenant: acme, key: "acme-prod",
			req:     Request{Model: "auto:cheap", Profile: "cheap", Headers: hdr("X-Switchyard-Profile", "quality")},
			profile: "cheap", minQuality: 0.65, objective: ObjectiveCost, warnings: []string{warnProfileModel}},
		{name: "same header and model field profile", tenant: acme, key: "acme-prod",
			req:     Request{Model: "auto:cheap", Profile: "cheap", Headers: hdr("X-Switchyard-Profile", "cheap")},
			profile: "cheap", minQuality: 0.65, objective: ObjectiveCost},
		{name: "model field profile not allowed", tenant: corp, key: "corp-a", req: Request{Model: "auto:quality", Profile: "quality"},
			errContains: `profile "quality" is not allowed by this policy; allowed profiles: balanced, cheap`},
		{name: "model field profile unknown", tenant: acme, key: "acme-prod", req: Request{Model: "auto:turbo", Profile: "turbo"},
			errContains: `unknown profile "turbo"; allowed profiles: balanced, cheap, fast, quality`},
		{name: "model field profile malformed is not echoed", tenant: acme, key: "acme-prod", req: Request{Model: "auto:<x>", Profile: "<x>"},
			errContains: "unknown profile; allowed profiles"},
		{name: "header profile malformed", tenant: acme, key: "acme-prod", req: Request{Model: "auto", Headers: hdr("X-Switchyard-Profile", "Not A Profile")},
			errContains: "invalid X-Switchyard-Profile header: must be a profile name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := e.Resolve(tt.tenant, tt.key, tt.req)
			if tt.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("Resolve error = %v, want one containing %q", err, tt.errContains)
				}
				if strings.Contains(tt.name, "header") {
					var ih *InvalidHeaderError
					if !errors.As(err, &ih) {
						t.Errorf("error type %T, want *InvalidHeaderError", err)
					}
				} else {
					var mns *ModelNotSupportedError
					if !errors.As(err, &mns) {
						t.Errorf("error type %T, want *ModelNotSupportedError", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			want := Constraints{MinQuality: tt.minQuality, Objective: tt.objective, MaxTTFT: tt.maxTTFT}
			if got.Profile != tt.profile || got.Constraints != want || got.MatchedRule != tt.rule {
				t.Errorf("profile %q, constraints %+v, rule %q; want %q, %+v, %q",
					got.Profile, got.Constraints, got.MatchedRule, tt.profile, want, tt.rule)
			}
			if diff := cmp.Diff(tt.warnings, got.Warnings); diff != "" {
				t.Errorf("warnings (-want +got):\n%s", diff)
			}
		})
	}
}

// ruleConfig returns a config whose default policy has one rule with the
// given match and a set that marks it.
func ruleConfig(match string) string {
	return catalog + `
policies:
  - id: default
    baseline_model: m
    rules:
      - name: r
        match: ` + match + `
        set: {min_quality: 0.99}
tenants:
  - id: acme
    keys: [{id: k1, hash: "` + hash("a") + `", tags: [prod, eu]}, {id: k2, hash: "` + hash("b") + `"}]
`
}

func TestRuleConditions(t *testing.T) {
	tests := []struct {
		name  string
		match string
		req   Request
		key   string
		want  bool
	}{
		{"empty match always holds", "{}", Request{}, "k1", true},
		{"model glob", `{model: "gpt-*"}`, Request{Model: "gpt-5-mini"}, "k1", true},
		{"model glob miss", `{model: "gpt-*"}`, Request{Model: "claude-x"}, "k1", false},
		{"model exact", `{model: auto}`, Request{Model: "auto"}, "k1", true},
		{"profile from the model field", `{profile: cheap}`, Request{Profile: "cheap"}, "k1", true},
		{"profile from the header", `{profile: cheap}`, Request{Headers: map[string][]string{"X-Switchyard-Profile": {"cheap"}}}, "k1", true},
		{"profile miss", `{profile: cheap}`, Request{Profile: "fast"}, "k1", false},
		{"profile none", `{profile: cheap}`, Request{}, "k1", false},
		{"task", `{task: coding}`, Request{Task: "coding"}, "k1", true},
		{"task miss", `{task: coding}`, Request{Task: "creative"}, "k1", false},
		{"difficulty_gte at the bound", `{difficulty_gte: 0.5}`, Request{Difficulty: 0.5}, "k1", true},
		{"difficulty_gte below", `{difficulty_gte: 0.5}`, Request{Difficulty: 0.49}, "k1", false},
		{"difficulty_lt below", `{difficulty_lt: 0.5}`, Request{Difficulty: 0.49}, "k1", true},
		{"difficulty_lt at the bound", `{difficulty_lt: 0.5}`, Request{Difficulty: 0.5}, "k1", false},
		{"difficulty band inside", `{difficulty_gte: 0.3, difficulty_lt: 0.6}`, Request{Difficulty: 0.45}, "k1", true},
		{"difficulty band above", `{difficulty_gte: 0.3, difficulty_lt: 0.6}`, Request{Difficulty: 0.6}, "k1", false},
		{"key", `{key: k1}`, Request{}, "k1", true},
		{"key from the request", `{key: k1}`, Request{KeyID: "k1"}, "", true},
		{"key miss", `{key: k1}`, Request{}, "k2", false},
		{"key_tags all present", `{key_tags: [prod, eu]}`, Request{KeyTags: []string{"eu", "prod", "x"}}, "k1", true},
		{"key_tags one missing", `{key_tags: [prod, eu]}`, Request{KeyTags: []string{"prod"}}, "k1", false},
		{"header glob", `{header: {X-Env: "stag*"}}`, Request{Headers: map[string][]string{"X-Env": {"staging"}}}, "k1", true},
		{"header name is case-insensitive", `{header: {x-env: staging}}`, Request{Headers: map[string][]string{"X-ENV": {"staging"}}}, "k1", true},
		{"header value is case-sensitive", `{header: {X-Env: staging}}`, Request{Headers: map[string][]string{"X-Env": {"Staging"}}}, "k1", false},
		{"header any value", `{header: {X-Env: staging}}`, Request{Headers: map[string][]string{"X-Env": {"prod", "staging"}}}, "k1", true},
		{"header absent", `{header: {X-Env: "*"}}`, Request{Headers: map[string][]string{"X-Other": {"staging"}}}, "k1", false},
		{"header present but empty matches *", `{header: {X-Env: "*"}}`, Request{Headers: map[string][]string{"X-Env": {""}}}, "k1", true},
		{"two headers both hold", `{header: {X-Env: staging, X-Team: "s*"}}`, Request{Headers: map[string][]string{"X-Env": {"staging"}, "X-Team": {"search"}}}, "k1", true},
		{"two headers one fails", `{header: {X-Env: staging, X-Team: "s*"}}`, Request{Headers: map[string][]string{"X-Env": {"staging"}, "X-Team": {"ads"}}}, "k1", false},
		{"tag glob", `{tags: {feature: "auto*"}}`, Request{Tags: map[string]string{"feature": "autocomplete"}}, "k1", true},
		{"tag value is case-sensitive", `{tags: {feature: "auto*"}}`, Request{Tags: map[string]string{"feature": "Autocomplete"}}, "k1", false},
		{"tag absent", `{tags: {feature: "*"}}`, Request{Tags: map[string]string{"team": "x"}}, "k1", false},
		{"stream true", `{stream: true}`, Request{Stream: true}, "k1", true},
		{"stream true miss", `{stream: true}`, Request{}, "k1", false},
		{"stream false", `{stream: false}`, Request{}, "k1", true},
		{"stream false miss", `{stream: false}`, Request{Stream: true}, "k1", false},
		{"has_tools", `{has_tools: true}`, Request{HasTools: true}, "k1", true},
		{"has_tools miss", `{has_tools: true}`, Request{}, "k1", false},
		{"has_images", `{has_images: "true"}`, Request{HasImages: true}, "k1", true},
		{"has_images false miss", `{has_images: "false"}`, Request{HasImages: true}, "k1", false},
		{"est_input_tokens_gte at the bound", `{est_input_tokens_gte: 1000}`, Request{EstInputTokens: 1000}, "k1", true},
		{"est_input_tokens_gte below", `{est_input_tokens_gte: 1000}`, Request{EstInputTokens: 999}, "k1", false},
		{"every condition", `{model: "gpt-*", task: coding, difficulty_gte: 0.5, key: k1, key_tags: [prod], header: {X-Env: prod}, tags: {team: search}, stream: true, has_tools: false, has_images: false, est_input_tokens_gte: 10}`,
			Request{Model: "gpt-5", Task: "coding", Difficulty: 0.7, KeyTags: []string{"prod"}, Headers: map[string][]string{"X-Env": {"prod"}},
				Tags: map[string]string{"team": "search"}, Stream: true, EstInputTokens: 20}, "k1", true},
		{"every condition but one", `{model: "gpt-*", task: coding, difficulty_gte: 0.5, key: k1, key_tags: [prod], header: {X-Env: prod}, tags: {team: search}, stream: true, has_tools: false, has_images: false, est_input_tokens_gte: 10}`,
			Request{Model: "gpt-5", Task: "coding", Difficulty: 0.7, KeyTags: []string{"prod"}, Headers: map[string][]string{"X-Env": {"prod"}},
				Tags: map[string]string{"team": "search"}, Stream: true, HasTools: true, EstInputTokens: 20}, "k1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, cfg := newEngine(t, ruleConfig(tt.match))
			got, err := e.Resolve(cfg.Tenants[0], tt.key, tt.req)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if matched := got.MatchedRule == "r" || got.MatchedQualityRule == "r"; matched != tt.want {
				t.Errorf("rule matched = %v, want %v", matched, tt.want)
			}
			if wantQ := map[bool]float64{true: 0.99, false: 0.8}[tt.want]; got.Constraints.MinQuality != wantQ {
				t.Errorf("min quality = %v, want %v", got.Constraints.MinQuality, wantQ)
			}
		})
	}
}

func TestRuleOverrides(t *testing.T) {
	e, cfg := newEngine(t, mainConfig)
	acme := tenant(t, cfg, "acme")
	base := Effective{
		PolicyID: "default", Profile: "balanced",
		Constraints: Constraints{MinQuality: 0.8, Objective: ObjectiveCost},
		Allow:       []string{"m@*"}, Deny: []string{"m@b"}, Prefer: []string{"m@a"},
		Substitution: SubstitutionPinned, MaxQualityDrop: 0.05, RoutingMode: ModeEnforce,
		BaselineModel: "m", MaxCostPerRequestNano: 500_000_000, OnUnmet: OnUnmetBestEffort,
	}
	tests := []struct {
		name string
		req  Request
		want func(e *Effective)
	}{
		{"no rule", Request{Model: "auto", Task: "coding", Difficulty: 0.4}, func(*Effective) {}},
		{"require_tags", Request{Model: "auto", KeyTags: []string{"eu"}}, func(e *Effective) {
			e.MatchedRule, e.RequireTags = "eu-keys-stay-in-eu", []string{"region_eu"}
		}},
		{"min_quality", Request{Model: "auto", Task: "coding", Difficulty: 0.55}, func(e *Effective) {
			e.MatchedQualityRule, e.Constraints.MinQuality, e.Constraints.QualityFloor = "code-needs-quality", 0.85, 0.85
		}},
		{"a quality rule applies on top of a governance rule", Request{Model: "auto", Task: "coding", Difficulty: 0.9, KeyTags: []string{"eu"}}, func(e *Effective) {
			e.MatchedRule, e.RequireTags = "eu-keys-stay-in-eu", []string{"region_eu"}
			e.MatchedQualityRule, e.Constraints.MinQuality, e.Constraints.QualityFloor = "code-needs-quality", 0.85, 0.85
		}},
		{"first governance match wins", Request{Model: "auto", KeyTags: []string{"eu"}, Tags: map[string]string{"feature": "batch-x"}}, func(e *Effective) {
			e.MatchedRule, e.RequireTags = "eu-keys-stay-in-eu", []string{"region_eu"}
		}},
		{"rules may loosen", Request{Model: "auto", Tags: map[string]string{"feature": "batch-nightly"}}, func(e *Effective) {
			e.MatchedRule = "loosen-everything"
			e.Allow, e.Deny, e.Prefer = []string{}, []string{}, []string{"*@b"}
			e.MaxCostPerRequestNano = 2_000_000_000
			e.Substitution, e.MaxQualityDrop = SubstitutionEquivalent, 0.2
			e.RoutingMode, e.OnUnmet = ModeObserve, OnUnmetReject
			e.Constraints.MinQuality, e.Constraints.QualityFloor = 0.5, 0.5
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := e.Resolve(acme, "acme-prod", tt.req)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			want := base
			want.Allow, want.Deny, want.Prefer = []string{"m@*"}, []string{"m@b"}, []string{"m@a"}
			tt.want(&want)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("effective (-want +got):\n%s", diff)
			}
		})
	}
}

// TestQualityRuleNeverShadowsGovernance checks the two rule layers: a
// quality rule, which the request's content or the client's hints reach,
// applies its min_quality and prefer on top of the first governance rule
// instead of stopping the evaluation before it, whatever their order.
func TestQualityRuleNeverShadowsGovernance(t *testing.T) {
	e, cfg := newEngine(t, catalog+`
policies:
  - id: default
    baseline_model: m
    rules:
      - name: coding-quality
        match: {task: coding}
        set: {min_quality: 0.85, prefer: ["m@b"]}
      - name: big-inputs
        match: {est_input_tokens_gte: 1000}
        set: {min_quality: 0.7}
      - name: prod-capped
        match: {key_tags: [prod]}
        set: {deny: ["m@a"], max_cost_per_request_usd: "0.01", prefer: ["m@a"]}
      - name: everyone
        set: {max_cost_per_request_usd: "1"}
tenants:
  - id: acme
    keys: [{id: k, hash: "`+hash("a")+`", tags: [prod]}, {id: j, hash: "`+hash("b")+`"}]
`)
	acme := cfg.Tenants[0]
	tests := []struct {
		name         string
		key          string
		req          Request
		gov, qual    string
		capNano      int64
		minQ, floor  float64
		deny, prefer []string
	}{
		{"governance only", "k", Request{Model: "m", Task: "general", KeyTags: []string{"prod"}}, "prod-capped", "", 10_000_000, 0.8, 0, []string{"m@a"}, []string{"m@a"}},
		{"a content-matched rule first", "k", Request{Model: "m", Task: "coding", KeyTags: []string{"prod"}}, "prod-capped", "coding-quality", 10_000_000, 0.85, 0.85, []string{"m@a"}, []string{"m@b"}},
		{"the first quality rule wins", "k", Request{Model: "m", Task: "coding", EstInputTokens: 5000, KeyTags: []string{"prod"}}, "prod-capped", "coding-quality", 10_000_000, 0.85, 0.85, []string{"m@a"}, []string{"m@b"}},
		{"another key", "j", Request{Model: "m", EstInputTokens: 5000}, "everyone", "big-inputs", 1_000_000_000, 0.7, 0.7, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := e.Resolve(acme, tt.key, tt.req)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.MatchedRule != tt.gov || got.MatchedQualityRule != tt.qual || got.MaxCostPerRequestNano != tt.capNano ||
				got.Constraints.MinQuality != tt.minQ || got.Constraints.QualityFloor != tt.floor ||
				!cmp.Equal(got.Deny, tt.deny) || !cmp.Equal(got.Prefer, tt.prefer) {
				t.Errorf("got rules %q/%q cap %d min quality %v floor %v deny %v prefer %v; want %q/%q %d %v %v %v %v",
					got.MatchedRule, got.MatchedQualityRule, got.MaxCostPerRequestNano, got.Constraints.MinQuality,
					got.Constraints.QualityFloor, got.Deny, got.Prefer,
					tt.gov, tt.qual, tt.capNano, tt.minQ, tt.floor, tt.deny, tt.prefer)
			}
		})
	}
}

// tightenConfig has a policy with every limit set, one without limits, and
// one that denies a model.
const tightenConfig = catalog + `
policies:
  - id: default
    baseline_model: m
    allowed_profiles: [cheap, balanced, fast]
    max_cost_per_request_usd: "0.50"
    deny: ["m@b"]
  - id: open
    baseline_model: m
routing:
  profiles:
    cheap: {min_quality: 0.65}
    balanced: {min_quality: 0.8, max_ttft_ms: 1000}
    fast: {min_quality: 0.8, objective: latency, max_ttft_ms: 500}
    quality: {min_quality: 0.92}
tenants:
  - id: acme
    keys: [{id: k, hash: "` + "sha256:1111111111111111111111111111111111111111111111111111111111111111" + `"}]
  - id: open
    policy: open
    keys: [{id: o, hash: "` + "sha256:2222222222222222222222222222222222222222222222222222222222222222" + `"}]
`

func TestHeaderTightening(t *testing.T) {
	e, cfg := newEngine(t, tightenConfig)
	acme, open := tenant(t, cfg, "acme"), tenant(t, cfg, "open")
	tests := []struct {
		name     string
		tenant   config.Tenant
		headers  map[string][]string
		model    string
		capNano  int64
		minQ     float64
		floor    float64
		ttft     time.Duration
		profile  string
		warnings []string
	}{
		{name: "no headers", tenant: acme, capNano: 500_000_000, minQ: 0.8, ttft: time.Second, profile: "balanced"},
		{name: "lower cost cap", tenant: acme, headers: map[string][]string{HeaderMaxCostUSD: {"0.05"}},
			capNano: 50_000_000, minQ: 0.8, ttft: time.Second, profile: "balanced"},
		{name: "cost cap below a micro-USD is exact", tenant: acme, headers: map[string][]string{HeaderMaxCostUSD: {"0.0000001"}},
			capNano: 100, minQ: 0.8, ttft: time.Second, profile: "balanced"},
		{name: "higher cost cap is ignored", tenant: acme, headers: map[string][]string{HeaderMaxCostUSD: {"5"}},
			capNano: 500_000_000, minQ: 0.8, ttft: time.Second, profile: "balanced", warnings: []string{warnMaxCost}},
		{name: "equal cost cap is a no-op", tenant: acme, headers: map[string][]string{HeaderMaxCostUSD: {"0.500"}},
			capNano: 500_000_000, minQ: 0.8, ttft: time.Second, profile: "balanced"},
		{name: "cost cap where the policy has none", tenant: open, headers: map[string][]string{HeaderMaxCostUSD: {"1.25"}},
			capNano: 1_250_000_000, minQ: 0.8, ttft: time.Second, profile: "balanced"},
		{name: "huge cost cap where the policy has none is clamped", tenant: open, headers: map[string][]string{HeaderMaxCostUSD: {"99999999999999"}},
			capNano: config.MaxCostPerRequestUSD * 1_000_000_000, minQ: 0.8, ttft: time.Second, profile: "balanced"},
		{name: "higher min quality", tenant: open, headers: map[string][]string{HeaderMinQuality: {"0.9"}},
			minQ: 0.9, floor: 0.9, ttft: time.Second, profile: "balanced"},
		{name: "min quality up to the best allowed profile's", tenant: open, headers: map[string][]string{HeaderMinQuality: {"0.92"}},
			minQ: 0.92, floor: 0.92, ttft: time.Second, profile: "balanced"},
		{name: "min quality above the best allowed profile's is ignored", tenant: open, headers: map[string][]string{HeaderMinQuality: {"0.93"}},
			minQ: 0.8, ttft: time.Second, profile: "balanced", warnings: []string{warnMinQualityCeiling}},
		{name: "min quality within the allowed profiles", tenant: acme, headers: map[string][]string{HeaderProfile: {"cheap"}, HeaderMinQuality: {"0.7"}},
			capNano: 500_000_000, minQ: 0.7, floor: 0.7, profile: "cheap"},
		{name: "min quality beyond the allowed profiles is ignored", tenant: acme, headers: map[string][]string{HeaderMinQuality: {"0.9"}},
			capNano: 500_000_000, minQ: 0.8, ttft: time.Second, profile: "balanced", warnings: []string{warnMinQualityCeiling}},
		{name: "lower min quality is ignored", tenant: acme, headers: map[string][]string{HeaderMinQuality: {"0.5"}},
			capNano: 500_000_000, minQ: 0.8, ttft: time.Second, profile: "balanced", warnings: []string{warnMinQuality}},
		{name: "equal min quality is a no-op", tenant: acme, headers: map[string][]string{HeaderMinQuality: {"0.80"}},
			capNano: 500_000_000, minQ: 0.8, floor: 0.8, ttft: time.Second, profile: "balanced"},
		{name: "min quality 1 is ignored", tenant: acme, headers: map[string][]string{HeaderMinQuality: {"1"}},
			capNano: 500_000_000, minQ: 0.8, ttft: time.Second, profile: "balanced", warnings: []string{warnMinQualityCeiling}},
		{name: "lower latency target", tenant: acme, headers: map[string][]string{HeaderMaxTTFTMs: {"250"}},
			capNano: 500_000_000, minQ: 0.8, ttft: 250 * time.Millisecond, profile: "balanced"},
		{name: "higher latency target is ignored", tenant: acme, headers: map[string][]string{HeaderMaxTTFTMs: {"5000"}},
			capNano: 500_000_000, minQ: 0.8, ttft: time.Second, profile: "balanced", warnings: []string{warnMaxTTFT}},
		{name: "latency target where the profile has none", tenant: acme, headers: map[string][]string{HeaderProfile: {"cheap"}, HeaderMaxTTFTMs: {"5000"}},
			capNano: 500_000_000, minQ: 0.65, ttft: 5 * time.Second, profile: "cheap"},
		{name: "canonical header key form", tenant: acme, headers: map[string][]string{"X-Switchyard-Max-Ttft-Ms": {"300"}},
			capNano: 500_000_000, minQ: 0.8, ttft: 300 * time.Millisecond, profile: "balanced"},
		{name: "headers combine with a selected profile", tenant: acme,
			headers: map[string][]string{HeaderProfile: {"fast"}, HeaderMinQuality: {"0.8"}, HeaderMaxTTFTMs: {"400"}, HeaderMaxCostUSD: {"0.1"}},
			capNano: 100_000_000, minQ: 0.8, floor: 0.8, ttft: 400 * time.Millisecond, profile: "fast"},
		{name: "every loosening header at once", tenant: acme,
			headers: map[string][]string{HeaderProfile: {"quality"}, HeaderMinQuality: {"0.1"}, HeaderMaxTTFTMs: {"60000"}, HeaderMaxCostUSD: {"100"}},
			capNano: 500_000_000, minQ: 0.8, ttft: time.Second, profile: "balanced",
			warnings: []string{warnProfileNotAllowed + " (allowed: balanced, cheap, fast)", warnMaxCost, warnMinQuality, warnMaxTTFT}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := cmpOr(tt.model, "auto")
			got, err := e.Resolve(tt.tenant, "", Request{Model: model, Headers: tt.headers})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.MaxCostPerRequestNano != tt.capNano || got.Constraints.MinQuality != tt.minQ ||
				got.Constraints.MaxTTFT != tt.ttft || got.Profile != tt.profile {
				t.Errorf("cap %d, min quality %v, ttft %v, profile %q; want %d, %v, %v, %q",
					got.MaxCostPerRequestNano, got.Constraints.MinQuality, got.Constraints.MaxTTFT, got.Profile,
					tt.capNano, tt.minQ, tt.ttft, tt.profile)
			}
			if got.Constraints.QualityFloor != tt.floor {
				t.Errorf("quality floor %v, want %v", got.Constraints.QualityFloor, tt.floor)
			}
			if diff := cmp.Diff(tt.warnings, got.Warnings); diff != "" {
				t.Errorf("warnings (-want +got):\n%s", diff)
			}
		})
	}
}

// TestHeadersNeverLoosen covers the P5 acceptance criterion: requests for
// a denied model, a higher cost cap, a disallowed profile or a lower
// quality bar are ignored with a warning, or rejected.
func TestHeadersNeverLoosen(t *testing.T) {
	e, cfg := newEngine(t, tightenConfig)
	acme := tenant(t, cfg, "acme")
	every := map[string][]string{
		HeaderProfile: {"quality"}, HeaderMaxCostUSD: {"10"}, HeaderMinQuality: {"0"}, HeaderMaxTTFTMs: {"999999"},
		// Headers no policy reads must not change anything either.
		"X-Switchyard-Allow": {"*"}, "X-Switchyard-Deny": {""}, "X-Switchyard-Routing-Mode": {"observe"},
	}
	tests := []struct {
		name     string
		req      Request
		wantErr  bool
		warnings int
	}{
		{"a denied model stays denied", Request{Model: "m@b", Headers: every}, false, 4},
		{"a higher cost cap", Request{Model: "auto", Headers: map[string][]string{HeaderMaxCostUSD: {"0.51"}}}, false, 1},
		{"a disallowed profile in the header", Request{Model: "auto", Headers: map[string][]string{HeaderProfile: {"quality"}}}, false, 1},
		{"a disallowed profile in the model field", Request{Model: "auto:quality", Profile: "quality"}, true, 0},
		{"a lower min quality", Request{Model: "auto", Headers: map[string][]string{HeaderMinQuality: {"0.79"}}}, false, 1},
	}
	plain, err := e.Resolve(acme, "k", Request{Model: "auto"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := e.Resolve(acme, "k", tt.req)
			if tt.wantErr {
				var mns *ModelNotSupportedError
				if !errors.As(err, &mns) {
					t.Fatalf("Resolve error = %v, want *ModelNotSupportedError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if len(got.Warnings) != tt.warnings {
				t.Errorf("warnings = %q, want %d", got.Warnings, tt.warnings)
			}
			got.Warnings = nil
			if diff := cmp.Diff(plain, got); diff != "" {
				t.Errorf("headers changed the policy (-plain +got):\n%s", diff)
			}
			if !cmp.Equal(got.Deny, []string{"m@b"}) {
				t.Errorf("deny = %v, want [m@b]", got.Deny)
			}
		})
	}
}

func TestMalformedHeaders(t *testing.T) {
	e, cfg := newEngine(t, tightenConfig)
	acme := tenant(t, cfg, "acme")
	const secretish = "sk-not-a-real-key-0123456789abcdef"
	tests := []struct {
		name   string
		header string
		values []string
	}{
		{"cost not a number", HeaderMaxCostUSD, []string{"cheap"}},
		{"cost negative", HeaderMaxCostUSD, []string{"-1"}},
		{"cost zero", HeaderMaxCostUSD, []string{"0"}},
		{"cost exponent", HeaderMaxCostUSD, []string{"1e-3"}},
		{"cost with a secret", HeaderMaxCostUSD, []string{secretish}},
		{"cost empty", HeaderMaxCostUSD, []string{"  "}},
		{"cost twice", HeaderMaxCostUSD, []string{"0.1", "0.2"}},
		{"quality above 1", HeaderMinQuality, []string{"1.01"}},
		{"quality negative", HeaderMinQuality, []string{"-0.5"}},
		{"quality exponent", HeaderMinQuality, []string{"9e-1"}},
		{"quality nan", HeaderMinQuality, []string{"NaN"}},
		{"quality hex", HeaderMinQuality, []string{"0x1p-1"}},
		{"quality with a secret", HeaderMinQuality, []string{secretish}},
		{"ttft zero", HeaderMaxTTFTMs, []string{"0"}},
		{"ttft negative", HeaderMaxTTFTMs, []string{"-5"}},
		{"ttft fraction", HeaderMaxTTFTMs, []string{"1.5"}},
		{"ttft unit", HeaderMaxTTFTMs, []string{"500ms"}},
		{"ttft too long", HeaderMaxTTFTMs, []string{"1234567890"}},
		{"ttft with a secret", HeaderMaxTTFTMs, []string{secretish}},
		{"profile with spaces", HeaderProfile, []string{"two words"}},
		{"profile with a secret", HeaderProfile, []string{secretish + "!"}},
		{"profile twice", HeaderProfile, []string{"cheap", "fast"}},
		{"profile present without a value", HeaderProfile, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.Resolve(acme, "k", Request{Model: "auto", Headers: map[string][]string{tt.header: tt.values}})
			var ih *InvalidHeaderError
			if !errors.As(err, &ih) {
				t.Fatalf("Resolve error = %v, want *InvalidHeaderError", err)
			}
			if ih.Header != tt.header {
				t.Errorf("Header = %q, want %q", ih.Header, tt.header)
			}
			for _, v := range tt.values {
				if v = strings.TrimSpace(v); len(v) > 1 && strings.Contains(err.Error(), v) {
					t.Errorf("error %q echoes the value %q", err, v)
				}
			}
		})
	}
}

func TestResolveDowngraded(t *testing.T) {
	e, cfg := newEngine(t, mainConfig)
	acme, corp := tenant(t, cfg, "acme"), tenant(t, cfg, "corp")
	tests := []struct {
		name     string
		tenant   config.Tenant
		req      Request
		soft     string
		profile  string
		minQ     float64
		rule     string
		warnings []string
	}{
		{"replaces the default profile", acme, Request{Model: "auto"}, "cheap", "cheap", 0.65, "", nil},
		{"replaces the requested profile", acme, Request{Model: "auto:quality", Profile: "quality"}, "cheap", "cheap", 0.65, "", nil},
		{"replaces a header profile with a warning", acme,
			Request{Model: "auto", Headers: map[string][]string{HeaderProfile: {"quality"}}}, "cheap", "cheap", 0.65, "", []string{warnProfileBudget}},
		{"replaces the policy profile", corp, Request{Model: "auto"}, "balanced", "balanced", 0.8, "", nil},
		{"replaces a rule profile", acme, Request{Model: "auto", Headers: map[string][]string{"X-Env": {"staging"}}}, "balanced", "balanced", 0.8, "staging-is-cheap", nil},
		{"quality rule min_quality still applies", acme, Request{Model: "auto", Task: "coding", Difficulty: 0.7}, "cheap", "cheap", 0.85, "code-needs-quality", nil},
		{"header tightening still applies", acme,
			Request{Model: "auto", Tags: map[string]string{"feature": "batch-x"}, Headers: map[string][]string{HeaderMinQuality: {"0.6"}}}, "cheap", "cheap", 0.6, "loosen-everything", nil},
		{"header min quality up to the soft profile's", acme,
			Request{Model: "auto", Headers: map[string][]string{HeaderMinQuality: {"0.65"}}}, "cheap", "cheap", 0.65, "", nil},
		{"header min quality above the soft profile's is ignored", acme,
			Request{Model: "auto", Headers: map[string][]string{HeaderMinQuality: {"0.7"}}}, "cheap", "cheap", 0.65, "", []string{warnMinQualityBudget}},
		{"soft profile need not be allowed", corp, Request{Model: "auto"}, "quality", "quality", 0.92, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := e.ResolveDowngraded(tt.tenant, "", tt.req, tt.soft)
			if err != nil {
				t.Fatalf("ResolveDowngraded: %v", err)
			}
			rule := got.MatchedRule + got.MatchedQualityRule
			if got.Profile != tt.profile || got.Constraints.MinQuality != tt.minQ || rule != tt.rule {
				t.Errorf("profile %q, min quality %v, rules %q; want %q, %v, %q", got.Profile, got.Constraints.MinQuality, rule, tt.profile, tt.minQ, tt.rule)
			}
			if diff := cmp.Diff(tt.warnings, got.Warnings); diff != "" {
				t.Errorf("warnings (-want +got):\n%s", diff)
			}
		})
	}
	if _, err := e.ResolveDowngraded(acme, "", Request{}, "free"); err == nil || !strings.Contains(err.Error(), `profile "free" is not defined`) {
		t.Errorf("unknown soft profile: err = %v", err)
	}
	if _, err := e.ResolveDowngraded(acme, "", Request{}, ""); err == nil {
		t.Error("empty soft profile: want an error")
	}
	var mns *ModelNotSupportedError
	if _, err := e.ResolveDowngraded(corp, "", Request{Model: "auto:quality", Profile: "quality"}, "cheap"); !errors.As(err, &mns) {
		t.Errorf("disallowed requested profile under downgrade: err = %v, want *ModelNotSupportedError", err)
	}
}

func TestSelectableProfiles(t *testing.T) {
	e, cfg := newEngine(t, mainConfig)
	tests := []struct {
		tenant, key string
		want        []string
	}{
		{"acme", "acme-prod", []string{"balanced", "cheap", "fast", "quality"}},
		{"acme", "acme-strict", []string{"balanced", "cheap"}},
		{"lab", "lab-a", nil},
	}
	for _, tt := range tests {
		got, err := e.SelectableProfiles(tenant(t, cfg, tt.tenant), tt.key)
		if err != nil || !cmp.Equal(got, tt.want) {
			t.Errorf("SelectableProfiles(%s, %s) = %v, %v; want %v", tt.tenant, tt.key, got, err, tt.want)
		}
	}
}

func TestCheckAuto(t *testing.T) {
	if err := CheckAuto(Effective{BaselineModel: "m"}); err != nil {
		t.Errorf("CheckAuto with a baseline = %v", err)
	}
	var mns *ModelNotSupportedError
	if err := CheckAuto(Effective{}); !errors.As(err, &mns) || !strings.Contains(err.Error(), "no baseline_model") {
		t.Errorf("CheckAuto without a baseline = %v", err)
	}
}

func TestResolveIsImmutable(t *testing.T) {
	e, cfg := newEngine(t, mainConfig)
	acme := tenant(t, cfg, "acme")
	first, err := e.Resolve(acme, "acme-prod", Request{Model: "auto"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	first.Allow[0], first.Deny[0], first.Prefer[0] = "x", "x", "x"
	second, err := e.Resolve(acme, "acme-prod", Request{Model: "auto"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if second.Allow[0] != "m@*" || second.Deny[0] != "m@b" || second.Prefer[0] != "m@a" || cfg.Policies[0].Allow[0] != "m@*" {
		t.Errorf("mutating a result changed the engine: %+v", second)
	}
}

func TestResolveConcurrent(t *testing.T) {
	e, cfg := newEngine(t, mainConfig)
	acme := tenant(t, cfg, "acme")
	req := Request{Model: "auto", Task: "coding", Difficulty: 0.7, Headers: map[string][]string{HeaderMinQuality: {"0.9"}}}
	want, err := e.Resolve(acme, "acme-prod", req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	done := make(chan Effective)
	for range 8 {
		go func() {
			got, _ := e.Resolve(acme, "acme-prod", req)
			done <- got
		}()
	}
	for range 8 {
		if got := <-done; !cmp.Equal(want, got) {
			t.Errorf("concurrent Resolve differs: %+v", got)
		}
	}
}

func TestResolveUnknownPolicy(t *testing.T) {
	e, _ := newEngine(t, mainConfig)
	_, err := e.Resolve(config.Tenant{ID: "ghost", Policy: "missing"}, "", Request{})
	if err == nil || !strings.Contains(err.Error(), `unknown policy "missing"`) {
		t.Errorf("err = %v, want unknown policy", err)
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.Config
		wantErr string
	}{
		{"empty config uses built-ins", config.Config{}, ""},
		{"hand-built policy gets defaults", config.Config{Policies: []config.Policy{{ID: "default"}}}, ""},
		{"unknown default profile", config.Config{Routing: config.Routing{DefaultProfile: "turbo"}}, `routing.default_profile "turbo" is not a profile`},
		{"configured profiles replace built-ins", config.Config{Routing: config.Routing{Profiles: map[string]config.Profile{"bulk": {}}}}, `routing.default_profile "balanced" is not a profile`},
		{"policy profile unknown", config.Config{Policies: []config.Policy{{ID: "p", Profile: "turbo"}}}, `policy "p": profile "turbo" is not a profile`},
		{"allowed profile unknown", config.Config{Policies: []config.Policy{{ID: "p", AllowedProfiles: []string{"cheap", "turbo"}}}}, `allowed_profiles entry "turbo"`},
		{"rule profile unknown", config.Config{Policies: []config.Policy{{ID: "p", Rules: []config.Rule{{Name: "r", Set: config.PolicyOverride{Profile: "turbo"}}}}}}, `rule r profile "turbo"`},
		{"quality rule sets a governance field", config.Config{Policies: []config.Policy{{ID: "p", Rules: []config.Rule{{Name: "q",
			Match: config.RuleMatch{Task: "coding"}, Set: config.PolicyOverride{MinQuality: 0.9, Deny: []string{}}}}}}},
			`policy "p": rule q matches on task, difficulty or input size, so it may only set min_quality and prefer`},
		{"duplicate policy", config.Config{Policies: []config.Policy{{ID: "p"}, {ID: "p"}}}, `policy "p" is defined twice`},
		{"policy without id", config.Config{Policies: []config.Policy{{}}}, "a policy has no id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := New(&tt.cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				got, err := e.Resolve(config.Tenant{ID: "t"}, "", Request{})
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				if got.Substitution != SubstitutionPinned || got.MaxQualityDrop != 0.05 || got.RoutingMode != ModeEnforce ||
					got.OnUnmet != OnUnmetBestEffort || got.Profile != "balanced" || got.Constraints.Objective != ObjectiveCost {
					t.Errorf("defaults not applied: %+v", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("New error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestConstantsMatchConfig keeps the policy constants in step with the
// values config validation accepts.
func TestConstantsMatchConfig(t *testing.T) {
	pairs := [][2]string{
		{ModeEnforce, config.RoutingModeEnforce},
		{ModeObserve, config.RoutingModeObserve},
		{SubstitutionPinned, config.SubstitutionPinned},
		{SubstitutionEquivalent, config.SubstitutionEquivalent},
		{OnUnmetBestEffort, config.OnUnmetBestEffort},
		{OnUnmetReject, config.OnUnmetReject},
		{ObjectiveCost, config.ObjectiveCost},
		{ObjectiveLatency, config.ObjectiveLatency},
	}
	for _, p := range pairs {
		if p[0] != p[1] {
			t.Errorf("policy constant %q != config constant %q", p[0], p[1])
		}
	}
}

func TestPriceNano(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"0", 0},
		{"0.000", 0},
		{"0.5", 500_000_000},
		{"0.0000001", 100},
		{"0.000000001", 1},
		{"0.0000011", 1100},
		{"1000000000", config.MaxCostPerRequestUSD * 1_000_000_000},
		{"1000000000000", config.MaxCostPerRequestUSD * 1_000_000_000},
	}
	for _, tt := range tests {
		var p config.Price
		if tt.in != "" {
			var err error
			if p, err = config.ParsePrice(tt.in); err != nil {
				t.Fatalf("ParsePrice(%q): %v", tt.in, err)
			}
		}
		if got := priceNano(p); got != tt.want {
			t.Errorf("priceNano(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
