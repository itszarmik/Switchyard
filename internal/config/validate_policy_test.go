package config

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// policiesYAML returns a policies section with one flow-style line per
// policy.
func policiesYAML(policies ...string) string {
	s := "policies:\n"
	for _, p := range policies {
		s += "  - " + p + "\n"
	}
	return s
}

// withPolicies returns a config with chat model m, embedding model e and
// the given extra sections.
func withPolicies(extra ...string) string {
	return catalogYAML(chatModel("", "")+embeddingModel("dimensions: 8"), strings.Join(extra, ""))
}

func TestProfileValidation(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		wants []want
	}{
		{"built-ins", "", nil},
		{"every field", "routing:\n  default_profile: bulk\n  expected_output_tokens: 800\n" +
			"  profiles:\n    bulk: {min_quality: 0, objective: cost}\n    snappy: {min_quality: 1, objective: latency, max_ttft_ms: 400}\n", nil},
		{"min_quality above 1", "routing: {profiles: {balanced: {min_quality: 1.5}}}\n",
			[]want{errAt("routing.profiles.balanced.min_quality", "must be between 0 and 1, got 1.5")}},
		{"min_quality negative", "routing: {profiles: {balanced: {min_quality: -0.1}}}\n",
			[]want{errAt("routing.profiles.balanced.min_quality", "must be between 0 and 1, got -0.1")}},
		{"objective unknown", "routing: {profiles: {balanced: {objective: quality}}}\n",
			[]want{errAt("routing.profiles.balanced.objective", `must be one of cost, latency, got "quality"`)}},
		{"max_ttft_ms negative", "routing: {profiles: {balanced: {max_ttft_ms: -1}}}\n",
			[]want{errAt("routing.profiles.balanced.max_ttft_ms", "must not be negative")}},
		{"bad profile name", "routing: {default_profile: ok, profiles: {ok: {}, Bad Name: {}}}\n",
			[]want{errAt("routing.profiles.Bad Name", "profile name must match")}},
		{"default_profile unknown", "routing: {default_profile: turbo}\n",
			[]want{errAt("routing.default_profile", `unknown profile "turbo" (known profiles: balanced, cheap, fast, quality)`)}},
		{"configured profiles replace the built-ins", "routing: {profiles: {bulk: {min_quality: 0.5}}}\n",
			[]want{errAt("routing.default_profile", `unknown profile "balanced"; configured profiles replace the built-in ones (known profiles: bulk)`)}},
		{"expected_output_tokens negative", "routing: {expected_output_tokens: -1}\n",
			[]want{errAt("routing.expected_output_tokens", "must be greater than 0")}},
		{"assessor weights", "routing: {assessor: {type: heuristic, weights: {length: 0.3, simple_task: -1, code: 1}}}\n", nil},
		{"assessor weights creative and bias", "routing: {assessor: {weights: {creative: 0.2, bias: 0.3}}}\n", nil},
		{"assessor type unknown", "routing: {assessor: {type: llm}}\n",
			[]want{errAt("routing.assessor.type", `must be one of heuristic, got "llm"`)}},
		{"assessor weight unknown with suggestion", "routing: {assessor: {weights: {lengthh: 0.2}}}\n",
			[]want{errAt("routing.assessor.weights.lengthh", `unknown signal (did you mean "length"?)`)}},
		{"assessor weight out of range", "routing: {assessor: {weights: {code: 1.5, math: -2}}}\n",
			[]want{errAt("routing.assessor.weights.code", "must be between -1 and 1, got 1.5"),
				errAt("routing.assessor.weights.math", "must be between -1 and 1, got -2")}},
		{"unknown profile field", "routing: {profiles: {balanced: {min_qualty: 0.5}}}\n",
			[]want{errAt("routing.profiles.balanced.min_qualty", `unknown field (did you mean "min_quality"?)`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkProblems(t, problemsOf(t, "version: 1\n"+tt.yaml, nil, nil), tt.wants...)
		})
	}
}

func TestProfileDefaults(t *testing.T) {
	cfg := mustLoad(t, "version: 1\n", nil, nil).Config()
	r := cfg.Routing
	if r.DefaultProfile != "balanced" || r.ExpectedOutputTokens != 500 || r.Assessor.Type != "heuristic" {
		t.Errorf("routing = default_profile %q, expected_output_tokens %d, assessor %q; want balanced, 500, heuristic",
			r.DefaultProfile, r.ExpectedOutputTokens, r.Assessor.Type)
	}
	want := map[string]Profile{
		"cheap":    {MinQuality: 0.65, Objective: "cost"},
		"balanced": {MinQuality: 0.80, Objective: "cost"},
		"quality":  {MinQuality: 0.92, Objective: "cost"},
		"fast":     {MinQuality: 0.80, Objective: "latency"},
	}
	if diff := cmp.Diff(want, r.Profiles); diff != "" {
		t.Errorf("built-in profiles (-want +got):\n%s", diff)
	}

	cfg = mustLoad(t, "version: 1\nrouting: {default_profile: bulk, profiles: {bulk: {min_quality: 0.5}, snappy: {objective: latency, max_ttft_ms: 300}}}\n", nil, nil).Config()
	want = map[string]Profile{
		"bulk":   {MinQuality: 0.5, Objective: "cost"},
		"snappy": {Objective: "latency", MaxTTFTMs: 300},
	}
	if diff := cmp.Diff(want, cfg.Routing.Profiles); diff != "" {
		t.Errorf("configured profiles (-want +got):\n%s", diff)
	}
}

func TestPolicyDefaults(t *testing.T) {
	cfg := mustLoad(t, withPolicies(policiesYAML(
		"{id: a, baseline_model: m}",
		"{id: b, baseline_model: m, substitution: {mode: equivalent, max_quality_drop: 0}, routing_mode: observe, on_unmet: reject}",
		"{id: c, baseline_model: m, substitution: {max_quality_drop: 0.1}, rules: [{name: r, set: {min_quality: 0.9}}]}",
	)), nil, nil).Config()
	want := []Policy{
		{ID: "a", BaselineModel: "m", Substitution: Substitution{Mode: "pinned", MaxQualityDrop: 0.05}, RoutingMode: "enforce", OnUnmet: "best_effort"},
		{ID: "b", BaselineModel: "m", Substitution: Substitution{Mode: "equivalent"}, RoutingMode: "observe", OnUnmet: "reject"},
		{ID: "c", BaselineModel: "m", Substitution: Substitution{Mode: "pinned", MaxQualityDrop: 0.1}, RoutingMode: "enforce", OnUnmet: "best_effort",
			Rules: []Rule{{Name: "r", Set: PolicyOverride{MinQuality: 0.9}}}},
	}
	if diff := cmp.Diff(want, cfg.Policies); diff != "" {
		t.Errorf("policies (-want +got):\n%s", diff)
	}
}

func TestPolicyValidation(t *testing.T) {
	pol := func(fields string) string { return policiesYAML("{id: p, baseline_model: m, " + fields + "}") }
	tests := []struct {
		name  string
		yaml  string
		wants []want
	}{
		{"none", withPolicies(), nil},
		{"every field", withPolicies(policiesYAML(`{id: default, profile: cheap, allowed_profiles: [cheap, balanced],
      allow: ["m@*", "*"], deny: ["m@b"], prefer: ["m@a", "[a-m]*@?"], require_tags: [zdr, region_eu],
      substitution: {mode: equivalent, max_quality_drop: 0.1}, routing_mode: observe, baseline_model: m,
      max_cost_per_request_usd: "0.50", on_unmet: reject}`)), nil},
		{"id missing", withPolicies(policiesYAML("{baseline_model: m}")),
			[]want{errAt("policies[0].id", "required")}},
		{"id bad", withPolicies(policiesYAML("{id: Default, baseline_model: m}")),
			[]want{errAt("policies[0].id", `must match`)}},
		{"id duplicate", withPolicies(policiesYAML("{id: p, baseline_model: m}", "{id: p, baseline_model: m}")),
			[]want{errAt("policies[1].id", `duplicate policy id "p"; first defined at cfg.yaml:`)}},
		{"profile unknown", withPolicies(pol("profile: turbo")),
			[]want{errAt("policies[0].profile", `unknown profile "turbo" (known profiles: balanced, cheap, fast, quality)`)}},
		{"allowed profile unknown", withPolicies(pol("allowed_profiles: [cheap, turbo]")),
			[]want{errAt("policies[0].allowed_profiles[1]", `unknown profile "turbo"`)}},
		{"allowed profile duplicate", withPolicies(pol("allowed_profiles: [cheap, cheap]")),
			[]want{errAt("policies[0].allowed_profiles[1]", `duplicate profile "cheap"`)}},
		{"allowed profile empty", withPolicies(pol("allowed_profiles: ['']")),
			[]want{errAt("policies[0].allowed_profiles[0]", "required")}},
		{"bad globs", withPolicies(pol("allow: ['m@[a'], deny: [''], prefer: ['a\\']")),
			[]want{errAt("policies[0].allow[0]", `invalid glob pattern "m@[a"`),
				errAt("policies[0].deny[0]", "a glob pattern must not be empty"),
				errAt("policies[0].prefer[0]", "invalid glob pattern")}},
		{"bad require_tags", withPolicies(pol("require_tags: [EU, zdr, zdr]")),
			[]want{errAt("policies[0].require_tags[0]", "must match"), errAt("policies[0].require_tags[2]", `duplicate tag "zdr"`)}},
		{"substitution mode unknown", withPolicies(pol("substitution: {mode: any}")),
			[]want{errAt("policies[0].substitution.mode", `must be one of pinned, equivalent, got "any"`)}},
		{"max_quality_drop out of range", withPolicies(pol("substitution: {max_quality_drop: 1.5}")),
			[]want{errAt("policies[0].substitution.max_quality_drop", "must be between 0 and 1, got 1.5")}},
		{"routing_mode unknown", withPolicies(pol("routing_mode: shadow")),
			[]want{errAt("policies[0].routing_mode", `must be one of enforce, observe, got "shadow"`)}},
		{"on_unmet unknown", withPolicies(pol("on_unmet: fail")),
			[]want{errAt("policies[0].on_unmet", `must be one of best_effort, reject, got "fail"`)}},
		{"baseline unknown with suggestion", withPolicies(policiesYAML("{id: p, baseline_model: mm}")),
			[]want{errAt("policies[0].baseline_model", `unknown model "mm" (did you mean "m"?)`)}},
		{"baseline embedding", withPolicies(policiesYAML("{id: p, baseline_model: e}")),
			[]want{errAt("policies[0].baseline_model", `model "e" is not a chat model`)}},
		{"baseline alias", withPolicies("aliases: {x: m}\n", policiesYAML("{id: p, baseline_model: x}")),
			[]want{errAt("policies[0].baseline_model", `"x" is an alias; use a model id`)}},
		{"baseline deployment", withPolicies(policiesYAML("{id: p, baseline_model: m@a}")),
			[]want{errAt("policies[0].baseline_model", "not a deployment id")}},
		{"baseline auto", withPolicies(policiesYAML("{id: p, baseline_model: 'auto:cheap'}")),
			[]want{errAt("policies[0].baseline_model", "not a virtual model")}},
		{"cost cap zero", withPolicies(pol("max_cost_per_request_usd: 0")),
			[]want{errAt("policies[0].max_cost_per_request_usd", "must be greater than 0")}},
		{"cost cap too large", withPolicies(pol("max_cost_per_request_usd: '1000000000.5'")),
			[]want{errAt("policies[0].max_cost_per_request_usd", "must be at most 1000000000")}},
		{"cost cap negative", withPolicies(pol("max_cost_per_request_usd: -1")),
			[]want{errAt("policies[0].max_cost_per_request_usd", "must not be negative")}},
		{"unknown policy field", withPolicies(pol("budgets: []")),
			[]want{errAt("policies[0].budgets", "unknown field")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkProblems(t, problemsOf(t, tt.yaml, nil, nil), tt.wants...)
		})
	}
}

func TestRuleValidation(t *testing.T) {
	rules := func(rs ...string) string {
		return withPolicies(policiesYAML("{id: p, baseline_model: m, rules: [" + strings.Join(rs, ", ") + "]}"))
	}
	tests := []struct {
		name  string
		yaml  string
		wants []want
	}{
		{"every governance condition and override", rules(`{name: all, match: {model: "gpt-*", profile: cheap,
        key: acme-backend, key_tags: [prod], header: {X-Env: "stag*"},
        tags: {team: "search"}, stream: "true", has_tools: false, has_images: "false"},
      set: {profile: quality, min_quality: 0.85, prefer: ["m@*"], allow: ["*"], deny: ["m@b"], require_tags: [zdr],
        substitution: {mode: equivalent, max_quality_drop: 0.2}, routing_mode: observe,
        max_cost_per_request_usd: 0.1, on_unmet: reject}}`), nil},
		{"every quality condition", rules(`{name: all, match: {model: "gpt-*", profile: cheap, task: coding,
        difficulty_gte: 0.5, difficulty_lt: 0.9, key: acme-backend, key_tags: [prod], header: {X-Env: "stag*"},
        tags: {team: "search"}, stream: "true", has_tools: false, has_images: "false", est_input_tokens_gte: 1000},
      set: {min_quality: 0.85, prefer: ["m@*"]}}`), nil},
		{"a quality rule may only set min_quality and prefer", rules(`{name: q, match: {difficulty_gte: 0.5},
      set: {profile: quality, min_quality: 0.85, prefer: ["m@*"], allow: [], deny: ["m@b"], require_tags: [zdr],
        substitution: {max_quality_drop: 0.2}, routing_mode: observe, max_cost_per_request_usd: 0.1, on_unmet: reject}}`),
			[]want{
				errAt("policies[0].rules[0].set.profile", msgQualityRuleSet),
				errAt("policies[0].rules[0].set.allow", msgQualityRuleSet),
				errAt("policies[0].rules[0].set.deny", msgQualityRuleSet),
				errAt("policies[0].rules[0].set.require_tags", msgQualityRuleSet),
				errAt("policies[0].rules[0].set.substitution", msgQualityRuleSet),
				errAt("policies[0].rules[0].set.routing_mode", msgQualityRuleSet),
				errAt("policies[0].rules[0].set.max_cost_per_request_usd", msgQualityRuleSet),
				errAt("policies[0].rules[0].set.on_unmet", msgQualityRuleSet),
			}},
		{"each quality condition makes a quality rule", rules(
			"{name: a, match: {task: coding}, set: {on_unmet: reject}}",
			"{name: b, match: {difficulty_lt: 0.5}, set: {on_unmet: reject}}",
			"{name: c, match: {est_input_tokens_gte: 10}, set: {on_unmet: reject}}"),
			[]want{
				errAt("policies[0].rules[0].set.on_unmet", msgQualityRuleSet),
				errAt("policies[0].rules[1].set.on_unmet", msgQualityRuleSet),
				errAt("policies[0].rules[2].set.on_unmet", msgQualityRuleSet),
			}},
		{"empty set is fine", rules("{name: noop, match: {task: coding}}"), nil},
		{"name missing", rules("{match: {task: coding}}"),
			[]want{errAt("policies[0].rules[0].name", "required")}},
		{"name bad", rules("{name: 'No Spaces'}"),
			[]want{errAt("policies[0].rules[0].name", "must match")}},
		{"name duplicate", rules("{name: r, match: {task: coding}}", "{name: r, match: {task: creative}}"),
			[]want{errAt("policies[0].rules[1].name", `duplicate rule name "r" in this policy; first defined at cfg.yaml:`)}},
		{"same name in two policies", withPolicies(policiesYAML(
			"{id: p, baseline_model: m, rules: [{name: r, match: {task: coding}}]}",
			"{id: q, baseline_model: m, rules: [{name: r, match: {task: coding}}]}")), nil},
		{"unreachable after unconditional", rules("{name: a, match: {key: k}}", "{name: all}", "{name: b, match: {stream: true}}", "{name: c}"),
			[]want{warnAt("policies[0].rules[2]", msgUnreachableRule+" (cfg.yaml:"),
				warnAt("policies[0].rules[3]", msgUnreachableRule)}},
		{"empty match mapping is unconditional", rules("{name: all, match: {}}", "{name: b, match: {has_tools: true}}"),
			[]want{warnAt("policies[0].rules[1]", msgUnreachableRule)}},
		{"quality rules stay reachable after an unconditional rule", rules("{name: all}", "{name: b, match: {task: creative}}"), nil},
		{"model glob bad", rules("{name: r, match: {model: 'gpt-['}}"),
			[]want{errAt("policies[0].rules[0].match.model", "invalid glob pattern")}},
		{"profile unknown", rules("{name: r, match: {profile: turbo}}"),
			[]want{errAt("policies[0].rules[0].match.profile", `unknown profile "turbo"`)}},
		{"task unknown with suggestion", rules("{name: r, match: {task: codng}}"),
			[]want{errAt("policies[0].rules[0].match.task", `unknown task "codng" (did you mean "coding"?)`)}},
		{"difficulty out of range", rules("{name: r, match: {difficulty_gte: -0.5, difficulty_lt: 1.5}}"),
			[]want{errAt("policies[0].rules[0].match.difficulty_gte", "must be between 0 and 1"),
				errAt("policies[0].rules[0].match.difficulty_lt", "must be between 0 and 1")}},
		{"difficulty bounds reversed", rules("{name: r, match: {difficulty_gte: 0.6, difficulty_lt: 0.6}}"),
			[]want{errAt("policies[0].rules[0].match.difficulty_lt", "must be greater than difficulty_gte")}},
		{"key bad", rules("{name: r, match: {key: 'Not An ID'}}"),
			[]want{errAt("policies[0].rules[0].match.key", "must match")}},
		{"key_tags bad", rules("{name: r, match: {key_tags: [Prod]}}"),
			[]want{errAt("policies[0].rules[0].match.key_tags[0]", "must match")}},
		{"header name bad", rules("{name: r, match: {header: {'x env': a}}}"),
			[]want{errAt("policies[0].rules[0].match.header.x env", "must be a valid HTTP header name")}},
		{"header name duplicate", rules("{name: r, match: {header: {X-Env: a, x-env: b}}}"),
			[]want{errAt("policies[0].rules[0].match.header.x-env", "duplicate header")}},
		{"header glob bad", rules("{name: r, match: {header: {X-Env: '[a'}}}"),
			[]want{errAt("policies[0].rules[0].match.header.X-Env", "invalid glob pattern")}},
		{"header glob empty", rules("{name: r, match: {header: {X-Env: ''}}}"),
			[]want{errAt("policies[0].rules[0].match.header.X-Env", "must not be empty")}},
		{"tag key bad", rules("{name: r, match: {tags: {Team: a}}}"),
			[]want{errAt("policies[0].rules[0].match.tags.Team", "tag key must match")}},
		{"tag glob bad", rules("{name: r, match: {tags: {team: '[a'}}}"),
			[]want{errAt("policies[0].rules[0].match.tags.team", "invalid glob pattern")}},
		{"booleans bad", rules("{name: r, match: {stream: yes, has_tools: 1, has_images: TRUE}}"),
			[]want{errAt("policies[0].rules[0].match.stream", `must be true or false, got "yes"`),
				errAt("policies[0].rules[0].match.has_tools", `must be true or false, got "1"`),
				errAt("policies[0].rules[0].match.has_images", `must be true or false, got "TRUE"`)}},
		{"est_input_tokens_gte negative", rules("{name: r, match: {est_input_tokens_gte: -1}}"),
			[]want{errAt("policies[0].rules[0].match.est_input_tokens_gte", "must not be negative")}},
		{"set profile unknown", rules("{name: r, set: {profile: turbo}}"),
			[]want{errAt("policies[0].rules[0].set.profile", `unknown profile "turbo"`)}},
		{"set fields bad", rules(`{name: r, set: {min_quality: 2, prefer: ["["], allow: [""], deny: ["[x"], require_tags: [EU],
        substitution: {mode: any, max_quality_drop: -1}, routing_mode: shadow, max_cost_per_request_usd: 0, on_unmet: fail}}`),
			[]want{
				errAt("policies[0].rules[0].set.min_quality", "must be between 0 and 1, got 2"),
				errAt("policies[0].rules[0].set.prefer[0]", "invalid glob pattern"),
				errAt("policies[0].rules[0].set.allow[0]", "must not be empty"),
				errAt("policies[0].rules[0].set.deny[0]", "invalid glob pattern"),
				errAt("policies[0].rules[0].set.require_tags[0]", "must match"),
				errAt("policies[0].rules[0].set.substitution.mode", `must be one of pinned, equivalent, got "any"`),
				errAt("policies[0].rules[0].set.substitution.max_quality_drop", "must be between 0 and 1, got -1"),
				errAt("policies[0].rules[0].set.routing_mode", `must be one of enforce, observe, got "shadow"`),
				errAt("policies[0].rules[0].set.max_cost_per_request_usd", "must be greater than 0"),
				errAt("policies[0].rules[0].set.on_unmet", `must be one of best_effort, reject, got "fail"`),
			}},
		{"set budgets is unknown", rules("{name: r, set: {budgets: []}}"),
			[]want{errAt("policies[0].rules[0].set.budgets", "unknown field")}},
		{"unknown match field", rules("{name: r, match: {when: x}}"),
			[]want{errAt("policies[0].rules[0].match.when", "unknown field")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkProblems(t, problemsOf(t, tt.yaml, nil, nil), tt.wants...)
		})
	}
}

func TestPolicyReferences(t *testing.T) {
	key := func(id, c, extra string) string { return "{id: " + id + ", hash: " + keyHash(c) + extra + "}" }
	tests := []struct {
		name  string
		yaml  string
		wants []want
	}{
		{"tenant and key policies", withPolicies(
			policiesYAML("{id: strict, baseline_model: m}", "{id: loose, baseline_model: m}"),
			tenantsYAML("{id: acme, policy: strict, keys: ["+key("k", "a", ", policy: loose")+"]}")), nil},
		{"tenant policy unknown with suggestion", withPolicies(
			policiesYAML("{id: strict, baseline_model: m}"),
			tenantsYAML("{id: acme, policy: strct, keys: ["+key("k", "a", "")+"]}")),
			[]want{errAt("tenants[0].policy", `unknown policy "strct" (did you mean "strict"?)`)}},
		{"key policy unknown, none defined", withPolicies(
			tenantsYAML("{id: acme, keys: [" + key("k", "a", ", policy: loose") + "]}")),
			[]want{errAt("tenants[0].keys[0].policy", `unknown policy "loose" (no policies are defined)`)}},
		{"tenant policy without baseline", withPolicies(
			policiesYAML("{id: nobase}"),
			tenantsYAML("{id: acme, policy: nobase, keys: ["+key("k", "a", "")+"]}")),
			[]want{warnAt("tenants[0].policy", `policy "nobase" `+msgNoBaseline)}},
		{"default policy without baseline", withPolicies(
			policiesYAML("{id: default}"),
			tenantsYAML("{id: acme, keys: ["+key("k", "a", "")+"]}")),
			[]want{warnAt("tenants[0]", `policy "default" `+msgNoBaseline)}},
		{"restricted profiles without baseline is fine", withPolicies(
			policiesYAML("{id: default, allowed_profiles: [cheap]}"),
			tenantsYAML("{id: acme, keys: ["+key("k", "a", "")+"]}")), nil},
		{"built-in policy gets no warning", withPolicies(
			tenantsYAML("{id: acme, keys: [" + key("k", "a", "") + "]}")), nil},
		{"soft profile", withPolicies(
			tenantsYAML("{id: acme, keys: [" + key("k", "a", "") + "], budgets: [{scope: tenant, window: month, limit_usd: 10, soft_at: 0.8, soft_profile: cheap}]}")), nil},
		{"soft profile without soft_at", withPolicies(
			tenantsYAML("{id: acme, keys: [" + key("k", "a", "") + "], budgets: [{scope: tenant, window: month, limit_usd: 10, soft_profile: cheap}]}")),
			[]want{errAt("tenants[0].budgets[0].soft_profile", "requires soft_at")}},
		{"soft profile unknown", withPolicies(
			tenantsYAML("{id: acme, keys: [" + key("k", "a", "") + "], budgets: [{scope: tenant, window: month, limit_usd: 10, soft_at: 0.5, soft_profile: free}]}")),
			[]want{errAt("tenants[0].budgets[0].soft_profile", `unknown profile "free"`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkProblems(t, problemsOf(t, tt.yaml, nil, nil), tt.wants...)
		})
	}
}

func TestPolicyProblemLocation(t *testing.T) {
	root := withPolicies("policies:\n  - id: p\n    baseline_model: m\n    rules:\n      - name: r\n        match: {task: nope}\n")
	ve := mustFail(t, root, nil, nil)
	checkProblems(t, ve.Problems, want{sev: SeverityError, file: rootFile, line: 24, path: "policies[0].rules[0].match.task", msg: "unknown task"})
}
