package config

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// catalogYAML returns a config with providers a and b and the given models
// section and extra top-level text.
func catalogYAML(models, extra string) string {
	return "version: 1\nproviders:\n" + prov("a") + prov("b") + "models:\n" + models + extra
}

// chatModel returns a models entry with id m served by provider a at a
// known price, with the model fields and deployment fields given.
func chatModel(modelFields, deploymentFields string) string {
	s := "  - id: m\n"
	if modelFields != "" {
		s += "    " + modelFields + "\n"
	}
	s += "    deployments:\n      - provider: a\n        upstream_model: up\n        pricing: {input: 1, output: 2}\n"
	if deploymentFields != "" {
		s += "        " + deploymentFields + "\n"
	}
	return s
}

// embeddingModel returns an embedding models entry with id e.
func embeddingModel(modelFields string) string {
	s := "  - id: e\n    type: embedding\n"
	if modelFields != "" {
		s += "    " + modelFields + "\n"
	}
	return s + "    deployments: [{provider: a, upstream_model: up, pricing: {input: 0.02}}]\n"
}

func TestCatalogValidationRules(t *testing.T) {
	const pin = "  - id: n\n    deployments:\n      - {provider: a, upstream_model: n, pricing: {input: 1, output: 1}}\n      - {provider: b, upstream_model: n, pricing: {input: 1, output: 1}}\n"
	env := map[string]string{"CAP": "tolls", "TARGET": "nope", "DATE": "yesterday"}
	tests := []struct {
		name  string
		yaml  string
		wants []want
	}{
		// model type
		{"chat by default", catalogYAML(chatModel("", ""), ""), nil},
		{"type chat", catalogYAML(chatModel("type: chat", ""), ""), nil},
		{"type unknown", catalogYAML(chatModel("type: rerank", ""), ""), []want{errAt("models[0].type", `must be one of chat, embedding, got "rerank"`)}},

		// limits
		{"limits valid", catalogYAML(chatModel("context_window: 128000\n    max_output_tokens: 16384", ""), ""), nil},
		{"limits equal", catalogYAML(chatModel("context_window: 1000\n    max_output_tokens: 1000", ""), ""), nil},
		{"output only", catalogYAML(chatModel("max_output_tokens: 1000", ""), ""), nil},
		{"context negative", catalogYAML(chatModel("context_window: -1", ""), ""), []want{errAt("models[0].context_window", "must not be negative")}},
		{"output negative", catalogYAML(chatModel("max_output_tokens: -1", ""), ""), []want{errAt("models[0].max_output_tokens", "must not be negative")}},
		{"output above context", catalogYAML(chatModel("context_window: 1000\n    max_output_tokens: 1001", ""), ""),
			[]want{errAt("models[0].max_output_tokens", "must not exceed context_window (1000)")}},

		// capabilities
		{"every capability", catalogYAML(chatModel("capabilities: [streaming, stream_usage, tools, parallel_tools, forced_tools, vision, json_mode, json_schema, n, logprobs, seed, sampling, temperature_2, penalties, completions, prompt_caching]", ""), ""), nil},
		{"unknown capability with suggestion", catalogYAML(chatModel("capabilities: [streaming, tolls]", ""), ""),
			[]want{errAt("models[0].capabilities[1]", `unknown capability "tolls" (did you mean "tools"?)`)}},
		{"unknown capability without suggestion", catalogYAML(chatModel("capabilities: [telepathy]", ""), ""),
			[]want{errAt("models[0].capabilities[0]", `unknown capability "telepathy"`)}},
		{"capability from a reference", catalogYAML(chatModel("capabilities: ['${CAP}']", ""), ""),
			[]want{errAt("models[0].capabilities[0]", "unknown capability the value of ${CAP}")}},
		{"duplicate capability", catalogYAML(chatModel("capabilities: [tools, vision, tools]", ""), ""),
			[]want{errAt("models[0].capabilities[2]", `duplicate capability "tools"`)}},
		{"deployment capabilities", catalogYAML(chatModel("", "capabilities: [streaming, tools]"), ""), nil},
		{"deployment unknown capability", catalogYAML(chatModel("", "capabilities: [streamin]"), ""),
			[]want{errAt("models[0].deployments[0].capabilities[0]", `unknown capability "streamin" (did you mean "streaming"?)`)}},
		{"deployment duplicate capability", catalogYAML(chatModel("", "capabilities: [n, n]"), ""),
			[]want{errAt("models[0].deployments[0].capabilities[1]", `duplicate capability "n"`)}},

		// quality
		{"quality valid", catalogYAML(chatModel("quality: {general: 0, coding: 1, reasoning: 0.5, extraction: 0.75, creative: 0.1, tool_use: 1.0}", ""), ""), nil},
		{"quality above 1", catalogYAML(chatModel("quality: {coding: 1.01}", ""), ""), []want{errAt("models[0].quality.coding", "must be between 0 and 1")}},
		{"quality below 0", catalogYAML(chatModel("quality: {general: -0.1}", ""), ""), []want{errAt("models[0].quality.general", "must be between 0 and 1")}},
		{"quality unknown category", catalogYAML(chatModel("quality: {codng: 0.5}", ""), ""),
			[]want{errAt("models[0].quality.codng", `unknown quality category (did you mean "coding"?); use one of general, coding`)}},
		{"quality duplicate key", catalogYAML(chatModel("quality: {coding: 0.5, coding: 0.6}", ""), ""),
			[]want{errAt("models[0].quality.coding", "duplicate key (first set at line 11)")}},
		{"quality not a number", catalogYAML(chatModel("quality: {coding: high}", ""), ""),
			[]want{errAt("models[0].quality.coding", `expected a number, got "high"`)}},
		{"quality nan", catalogYAML(chatModel("quality: {coding: .nan}", ""), ""),
			[]want{errAt("models[0].quality.coding", "expected a finite number")}},
		{"quality not a mapping", catalogYAML(chatModel("quality: [0.5]", ""), ""),
			[]want{errAt("models[0].quality", "expected a mapping, got a sequence")}},
		{"quality several problems", catalogYAML(chatModel("quality: {zzz: 0.5, coding: 2, general: 3}", ""), ""), []want{
			errAt("models[0].quality.coding", "must be between 0 and 1"),
			errAt("models[0].quality.general", "must be between 0 and 1"),
			errAt("models[0].quality.zzz", "unknown quality category; use one of"),
		}},

		// token ratio
		{"token ratio valid", catalogYAML(chatModel("token_ratio: 1.05", ""), ""), nil},
		{"token ratio integer", catalogYAML(chatModel("token_ratio: 10", ""), ""), nil},
		{"token ratio zero means 1", catalogYAML(chatModel("token_ratio: 0", ""), ""), nil},
		{"token ratio negative", catalogYAML(chatModel("token_ratio: -0.5", ""), ""), []want{errAt("models[0].token_ratio", "must be greater than 0 and at most 10")}},
		{"token ratio too large", catalogYAML(chatModel("token_ratio: 10.5", ""), ""), []want{errAt("models[0].token_ratio", "at most 10")}},
		{"token ratio infinite", catalogYAML(chatModel("token_ratio: .inf", ""), ""), []want{errAt("models[0].token_ratio", "expected a finite number")}},

		// embedding models
		{"embedding valid", catalogYAML(embeddingModel("dimensions: 1536\n    capabilities: [prompt_caching]"), ""), nil},
		{"embedding without dimensions", catalogYAML(embeddingModel(""), ""), []want{errAt("models[0].dimensions", "required for an embedding model")}},
		{"embedding negative dimensions", catalogYAML(embeddingModel("dimensions: -3"), ""), []want{errAt("models[0].dimensions", "must be greater than 0")}},
		{"chat with dimensions", catalogYAML(chatModel("dimensions: 1536", ""), ""), []want{errAt("models[0].dimensions", "only allowed for an embedding model")}},
		{"embedding with chat capabilities", catalogYAML(embeddingModel("dimensions: 8\n    capabilities: [streaming, tools, vision, json_mode, json_schema, n, logprobs, temperature_2, penalties, completions, seed, sampling, forced_tools]"), ""), []want{
			errAt("models[0].capabilities[1]", `"tools" is a chat capability; an embedding model cannot have it`),
			errAt("models[0].capabilities[2]", `"vision" is a chat capability`),
			errAt("models[0].capabilities[3]", `"json_mode" is a chat capability`),
			errAt("models[0].capabilities[4]", `"json_schema" is a chat capability`),
			errAt("models[0].capabilities[5]", `"n" is a chat capability`),
			errAt("models[0].capabilities[6]", `"logprobs" is a chat capability`),
			errAt("models[0].capabilities[7]", `"temperature_2" is a chat capability`),
			errAt("models[0].capabilities[8]", `"penalties" is a chat capability`),
			errAt("models[0].capabilities[9]", `"completions" is a chat capability`),
			errAt("models[0].capabilities[11]", `"sampling" is a chat capability`),
			errAt("models[0].capabilities[12]", `"forced_tools" is a chat capability`),
		}},
		{"embedding deployment with chat capabilities", catalogYAML("  - id: e\n    type: embedding\n    dimensions: 8\n    deployments: [{provider: a, upstream_model: up, pricing: {input: 1}, capabilities: [tools]}]\n", ""),
			[]want{errAt("models[0].deployments[0].capabilities[0]", `"tools" is a chat capability`)}},

		// deployments
		{"deployment fields valid", catalogYAML(chatModel("", "quality_offset: -0.02\n        tags: [quantized]\n        latency_hint: {ttft_ms: 900, tokens_per_sec: 70.5}"), ""), nil},
		{"quality offset bounds", catalogYAML(chatModel("", "quality_offset: 1"), ""), nil},
		{"quality offset too low", catalogYAML(chatModel("", "quality_offset: -1.5"), ""), []want{errAt("models[0].deployments[0].quality_offset", "must be between -1 and 1")}},
		{"quality offset too high", catalogYAML(chatModel("", "quality_offset: 1.01"), ""), []want{errAt("models[0].deployments[0].quality_offset", "must be between -1 and 1")}},
		{"latency negative", catalogYAML(chatModel("", "latency_hint: {ttft_ms: -1, tokens_per_sec: -0.5}"), ""), []want{
			errAt("models[0].deployments[0].latency_hint.ttft_ms", "must not be negative"),
			errAt("models[0].deployments[0].latency_hint.tokens_per_sec", "must not be negative"),
		}},
		{"deployment tag invalid", catalogYAML(chatModel("", "tags: [Bad]"), ""), []want{errAt("models[0].deployments[0].tags[0]", "must match")}},

		// pricing
		{"pricing fully documented", catalogYAML("  - id: m\n    deployments:\n      - provider: a\n        upstream_model: up\n        pricing: {input: 3, output: 15, cached_input: 0.30, cache_write: 3.75, source: 'https://example.com/pricing#models', checked: 2026-11-11}\n", ""), nil},
		{"pricing checked invalid", catalogYAML("  - id: m\n    deployments: [{provider: a, upstream_model: up, pricing: {input: 1, output: 1, checked: 2026-13-01}}]\n", ""),
			[]want{errAt("models[0].deployments[0].pricing.checked", `must be a date as YYYY-MM-DD, got "2026-13-01"`)}},
		{"pricing checked not padded", catalogYAML("  - id: m\n    deployments: [{provider: a, upstream_model: up, pricing: {input: 1, output: 1, checked: 2026-1-5}}]\n", ""),
			[]want{errAt("models[0].deployments[0].pricing.checked", "must be a date as YYYY-MM-DD")}},
		{"pricing checked from a reference", catalogYAML("  - id: m\n    deployments: [{provider: a, upstream_model: up, pricing: {input: 1, output: 1, checked: '${DATE}'}}]\n", ""),
			[]want{errAt("models[0].deployments[0].pricing.checked", "got the value of ${DATE}")}},
		{"pricing source not http", catalogYAML("  - id: m\n    deployments: [{provider: a, upstream_model: up, pricing: {input: 1, output: 1, source: 'ftp://example.com/p'}}]\n", ""),
			[]want{errAt("models[0].deployments[0].pricing.source", "must be an absolute http:// or https:// URL with a host")}},
		{"pricing source relative", catalogYAML("  - id: m\n    deployments: [{provider: a, upstream_model: up, pricing: {input: 1, output: 1, source: '/pricing'}}]\n", ""),
			[]want{errAt("models[0].deployments[0].pricing.source", "absolute")}},
		{"pricing source with credentials", catalogYAML("  - id: m\n    deployments: [{provider: a, upstream_model: up, pricing: {input: 1, output: 1, source: 'https://u:p@example.com/'}}]\n", ""),
			[]want{errAt("models[0].deployments[0].pricing.source", "must not embed credentials")}},
		{"chat pricing missing", catalogYAML("  - id: m\n    deployments: [{provider: a, upstream_model: up}]\n", ""),
			[]want{warnAt("models[0].deployments[0].pricing", MsgPricingUnknown)}},
		{"chat output price missing", catalogYAML("  - id: m\n    deployments: [{provider: a, upstream_model: up, pricing: {input: 1}}]\n", ""),
			[]want{warnAt("models[0].deployments[0].pricing", MsgPricingUnknown)}},
		{"chat input price missing", catalogYAML("  - id: m\n    deployments: [{provider: a, upstream_model: up, pricing: {output: 1}}]\n", ""),
			[]want{warnAt("models[0].deployments[0].pricing", MsgPricingUnknown)}},
		{"zero prices are known", catalogYAML("  - id: m\n    deployments: [{provider: a, upstream_model: up, pricing: {input: 0, output: 0}}]\n", ""), nil},
		{"embedding needs only input", catalogYAML(embeddingModel("dimensions: 8"), ""), nil},
		{"embedding input missing", catalogYAML("  - id: e\n    type: embedding\n    dimensions: 8\n    deployments: [{provider: a, upstream_model: up, pricing: {output: 1}}]\n", ""),
			[]want{warnAt("models[0].deployments[0].pricing", MsgPricingUnknown)}},
		{"bad price is not also unknown", catalogYAML("  - id: m\n    deployments: [{provider: a, upstream_model: up, pricing: {input: -1, output: 1}}]\n", ""),
			[]want{errAt("models[0].deployments[0].pricing.input", "must not be negative")}},

		// aliases
		{"aliases valid", catalogYAML(chatModel("", "")+pin, "aliases:\n  fast: m\n  pinned: n@b\n  org/x.y:z: m\n"), nil},
		{"alias empty map", catalogYAML(chatModel("", ""), "aliases: {}\n"), nil},
		{"alias null", catalogYAML(chatModel("", ""), "aliases:\n"), nil},
		{"alias collides with a model", catalogYAML(chatModel("", "")+pin, "aliases:\n  n: m\n"),
			[]want{errAt("aliases.n", "alias name collides with the model id defined at cfg.yaml:")}},
		{"alias name with @", catalogYAML(chatModel("", ""), "aliases:\n  x@a: m\n"), []want{errAt("aliases.x@a", `must not contain "@"`)}},
		{"alias name auto", catalogYAML(chatModel("", ""), "aliases:\n  auto: m\n"), []want{errAt("aliases.auto", "reserved for virtual models")}},
		{"alias name auto prefix", catalogYAML(chatModel("", ""), "aliases:\n  'auto:cheap': m\n"), []want{errAt("aliases.auto:cheap", "reserved for virtual models")}},
		{"alias name autopilot is fine", catalogYAML(chatModel("", ""), "aliases:\n  autopilot: m\n"), nil},
		{"alias name bad chars", catalogYAML(chatModel("", ""), "aliases:\n  'my alias': m\n"), []want{errAt("aliases.my alias", "must match")}},
		{"alias name credential-like is hidden", catalogYAML(chatModel("", ""), "aliases:\n  sk-proj-abcdefghijklmnopqrstuvwxyz0123: zz\n"),
			[]want{errAt("aliases.<hidden key>", `target "zz" is not a model id`)}},
		{"alias target missing", catalogYAML(chatModel("", ""), "aliases:\n  x: ''\n"), []want{errAt("aliases.x", "required; set a model id or model@provider")}},
		{"alias target unknown with suggestion", catalogYAML(chatModel("", "")+pin, "aliases:\n  x: nn\n"),
			[]want{errAt("aliases.x", `target "nn" is not a model id or model@provider (did you mean "n"?)`)}},
		{"alias target from a reference", catalogYAML(chatModel("", ""), "aliases:\n  x: '${TARGET}'\n"),
			[]want{errAt("aliases.x", "target the value of ${TARGET} is not a model id")}},
		{"alias target unknown pinned model", catalogYAML(chatModel("", ""), "aliases:\n  x: zz@a\n"),
			[]want{errAt("aliases.x", `target "zz@a" names an unknown model`)}},
		{"alias target unknown pinned provider", catalogYAML(chatModel("", ""), "aliases:\n  x: m@b\n"),
			[]want{errAt("aliases.x", `target "m@b" names a provider that does not serve that model`)}},
		{"alias target auto", catalogYAML(chatModel("", ""), "aliases:\n  x: 'auto:quality'\n"),
			[]want{errAt("aliases.x", `target "auto:quality": virtual models arrive in Phase 5`)}},
		{"alias target plain auto", catalogYAML(chatModel("", ""), "aliases:\n  x: auto\n"),
			[]want{errAt("aliases.x", "virtual models arrive in Phase 5")}},
		{"alias chain", catalogYAML(chatModel("", ""), "aliases:\n  x: m\n  y: x\n"),
			[]want{errAt("aliases.y", `target "x" is another alias; aliases do not chain`)}},
		{"alias duplicate", catalogYAML(chatModel("", ""), "aliases:\n  x: m\n  x: m\n"),
			[]want{errAt("aliases.x", "duplicate key (first set at line 16)")}},
		{"aliases not a mapping", catalogYAML(chatModel("", ""), "aliases: [m]\n"),
			[]want{errAt("aliases", "expected a mapping, got a sequence")}},
		{"alias value not a string", catalogYAML(chatModel("", ""), "aliases:\n  x: {a: b}\n"),
			[]want{errAt("aliases.x", "expected a string, got a mapping")}},

		// routing
		{"routing", catalogYAML(chatModel("", ""), "routing: {drop_unsupported_params: true}\n"), nil},
		{"routing unknown field", catalogYAML(chatModel("", ""), "routing: {drop_unsuported_params: true}\n"),
			[]want{errAt("routing.drop_unsuported_params", `did you mean "drop_unsupported_params"?`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkProblems(t, problemsOf(t, tt.yaml, env, nil), tt.wants...)
		})
	}
}

func TestCatalogDecoding(t *testing.T) {
	root := catalogYAML(`  - id: m
    context_window: 200000
    max_output_tokens: 64000
    capabilities: [streaming, tools]
    quality: {general: 0.9, coding: 0.95}
    token_ratio: 1.05
    deployments:
      - provider: a
        upstream_model: up
        pricing: {input: 3, output: "15.00", cached_input: 0.30, cache_write: 3.75, source: 'https://example.com/p', checked: 2026-11-11}
        capabilities: [streaming]
        tags: [zdr]
        quality_offset: -0.02
        latency_hint: {ttft_ms: 900, tokens_per_sec: 70}
  - id: e
    type: embedding
    dimensions: 1536
    deployments: [{provider: b, upstream_model: e1, pricing: {input: 0.02}}]
`, "aliases:\n  fast: m\n  pinned: m@a\nrouting:\n  drop_unsupported_params: true\n")
	cfg := mustLoad(t, root, nil, nil).Config()
	want := []Model{
		{
			ID: "m", Type: "chat", ContextWindow: 200000, MaxOutputTokens: 64000,
			Capabilities: []string{"streaming", "tools"},
			Quality:      map[string]float64{"general": 0.9, "coding": 0.95},
			TokenRatio:   1.05,
			Deployments: []Deployment{{
				Provider: "a", UpstreamModel: "up",
				Pricing: Pricing{
					Input: mustPrice(t, "3"), Output: mustPrice(t, "15.00"),
					CachedInput: mustPrice(t, "0.30"), CacheWrite: mustPrice(t, "3.75"),
					Source: "https://example.com/p", Checked: "2026-11-11",
				},
				Capabilities: []string{"streaming"}, Tags: []string{"zdr"}, QualityOffset: -0.02,
				LatencyHint: LatencyHint{TTFTMs: 900, TokensPerSec: 70},
			}},
		},
		{
			ID: "e", Type: "embedding", Dimensions: 1536, TokenRatio: 1,
			Deployments: []Deployment{{Provider: "b", UpstreamModel: "e1", Pricing: Pricing{Input: mustPrice(t, "0.02")}}},
		},
	}
	if diff := cmp.Diff(want, cfg.Models); diff != "" {
		t.Errorf("models (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]string{"fast": "m", "pinned": "m@a"}, cfg.Aliases); diff != "" {
		t.Errorf("aliases (-want +got):\n%s", diff)
	}
	if !cfg.Routing.DropUnsupportedParams {
		t.Error("routing.drop_unsupported_params = false, want true")
	}
}

func TestMapMergeKeys(t *testing.T) {
	root := catalogYAML(chatModel("quality: {<<: {general: 0.5, coding: 0.6}, coding: 0.7}", ""),
		"x-base: &base {fast: m}\n")
	// x-base is an unknown field, but the map merge still works elsewhere.
	ve := mustFail(t, root+"aliases:\n  <<: *base\n  quick: m\n", nil, nil)
	checkProblems(t, ve.Problems, errAt("x-base", "unknown field"))

	cfg := mustLoad(t, catalogYAML(chatModel("quality: {<<: {general: 0.5, coding: 0.6}, coding: 0.7}", ""), ""), nil, nil).Config()
	if diff := cmp.Diff(map[string]float64{"general": 0.5, "coding": 0.7}, cfg.Models[0].Quality); diff != "" {
		t.Errorf("quality (-want +got):\n%s", diff)
	}
}

func TestIncludedAliases(t *testing.T) {
	models := "models:\n" + chatModel("", "")
	tests := []struct {
		name  string
		root  string
		files fakeFS
		want  map[string]string
		wants []want
	}{
		{
			name:  "aliases from an include are merged",
			root:  catalogYAML(chatModel("", ""), "include: [inc/*.yaml]\naliases:\n  root-alias: m\n"),
			files: fakeFS{"inc/a.yaml": "aliases:\n  a-alias: m\n", "inc/b.yaml": "aliases: {b-alias: m}\n"},
			want:  map[string]string{"root-alias": "m", "a-alias": "m", "b-alias": "m"},
		},
		{
			name:  "aliases only in an include",
			root:  "version: 1\ninclude: [a.yaml]\nproviders:\n" + prov("a"),
			files: fakeFS{"a.yaml": models + "aliases:\n  x: m\n"},
			want:  map[string]string{"x": "m"},
		},
		{
			name:  "root aliases null is extended",
			root:  "version: 1\ninclude: [a.yaml]\nproviders:\n" + prov("a") + models + "aliases:\n",
			files: fakeFS{"a.yaml": "aliases:\n  x: m\n"},
			want:  map[string]string{"x": "m"},
		},
		{
			name:  "null aliases in an include",
			root:  catalogYAML(chatModel("", ""), "include: [a.yaml]\n"),
			files: fakeFS{"a.yaml": "aliases:\n"},
		},
		{
			name:  "duplicate alias across files",
			root:  catalogYAML(chatModel("", ""), "include: [a.yaml]\naliases:\n  x: m\n"),
			files: fakeFS{"a.yaml": "# comment\naliases:\n  x: m\n"},
			wants: []want{{sev: SeverityError, file: "a.yaml", line: 3, path: "aliases.x", msg: "duplicate key (first set at cfg.yaml:17)"}},
		},
		{
			name:  "duplicate alias across included files",
			root:  catalogYAML(chatModel("", ""), "include: [a.yaml, b.yaml]\n"),
			files: fakeFS{"a.yaml": "aliases:\n  x: m\n", "b.yaml": "aliases:\n  x: m\n"},
			wants: []want{{sev: SeverityError, file: "b.yaml", line: 2, path: "aliases.x", msg: "duplicate key (first set at a.yaml:2)"}},
		},
		{
			name:  "included aliases must be a mapping",
			root:  catalogYAML(chatModel("", ""), "include: [a.yaml]\n"),
			files: fakeFS{"a.yaml": "aliases: [x]\n"},
			wants: []want{{sev: SeverityError, file: "a.yaml", line: 1, path: "aliases", msg: "expected a mapping, got a sequence"}},
		},
		{
			name:  "an included alias is validated",
			root:  catalogYAML(chatModel("", ""), "include: [a.yaml]\n"),
			files: fakeFS{"a.yaml": "aliases:\n  x: zz\n"},
			wants: []want{{sev: SeverityError, file: "a.yaml", line: 2, path: "aliases.x", msg: `target "zz" is not a model id`}},
		},
		{
			name:  "tenants stay root-only",
			root:  catalogYAML(chatModel("", ""), "include: [a.yaml]\n"),
			files: fakeFS{"a.yaml": "tenants: []\n"},
			wants: []want{{sev: SeverityError, file: "a.yaml", line: 1, path: "tenants", msg: `"tenants" is not allowed in included file a.yaml; only version, providers, models and aliases are`},
				warnAt("tenants", msgNoTenants)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap, err := loadYAML(t, tt.root, nil, tt.files)
			if len(tt.wants) > 0 {
				checkProblems(t, problemsOf(t, tt.root, nil, tt.files), tt.wants...)
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			checkProblems(t, snap.Warnings())
			if diff := cmp.Diff(tt.want, snap.Config().Aliases, cmpEmptyMap); diff != "" {
				t.Errorf("aliases (-want +got):\n%s", diff)
			}
		})
	}
}

// cmpEmptyMap treats nil and empty maps as equal.
var cmpEmptyMap = cmp.FilterValues(func(a, b map[string]string) bool { return len(a) == 0 && len(b) == 0 }, cmp.Comparer(func(_, _ map[string]string) bool { return true }))

func TestCatalogFingerprint(t *testing.T) {
	base := catalogYAML(chatModel("quality: {general: 0.5, coding: 0.6}", ""), "aliases: {x: m, y: m}\n")
	v := mustLoad(t, base, nil, nil).Version()
	for i := range 3 {
		if again := mustLoad(t, base, nil, nil).Version(); again != v {
			t.Fatalf("load %d: version %s, want %s; map order must not matter", i, again, v)
		}
	}
	tests := []struct {
		name string
		yaml string
		same bool
	}{
		{"map keys reordered", catalogYAML(chatModel("quality: {coding: 0.6, general: 0.5}", ""), "aliases: {y: m, x: m}\n"), true},
		{"quality value changes", catalogYAML(chatModel("quality: {general: 0.5, coding: 0.61}", ""), "aliases: {x: m, y: m}\n"), false},
		{"quality key added", catalogYAML(chatModel("quality: {general: 0.5, coding: 0.6, creative: 0}", ""), "aliases: {x: m, y: m}\n"), false},
		{"alias added", catalogYAML(chatModel("quality: {general: 0.5, coding: 0.6}", ""), "aliases: {x: m, y: m, z: m}\n"), false},
		{"price text changes", strings.Replace(base, "output: 2}", "output: 2.0}", 1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mustLoad(t, tt.yaml, nil, nil).Version(); (got == v) != tt.same {
				t.Errorf("version %s vs %s; want same = %v", got, v, tt.same)
			}
		})
	}
}

func TestSnapshotLocate(t *testing.T) {
	root := catalogYAML(chatModel("quality: {coding: 0.6}", ""), "aliases:\n  fast: m\n")
	snap, err := Load(rootFile, LoadOptions{
		LookupEnv: envMap(nil), ReadFile: fakeFS{rootFile: root}.ReadFile, Glob: fakeFS{}.Glob,
		Overrides: map[string]string{"logging.level": "debug"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path     string
		wantFile string
		wantLine int
	}{
		{"models[0].quality.coding", rootFile, 11},
		{"models[0].deployments[0].pricing", rootFile, 15},
		{"models[0].deployments[0].pricing.source", rootFile, 15}, // unset: its parent
		{"aliases.fast", rootFile, 17},
		{"server.listen", rootFile, 0}, // a default
		{"logging.level", "", 0},       // an override
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if f, l := snap.Locate(tt.path); f != tt.wantFile || l != tt.wantLine {
				t.Errorf("Locate(%q) = %q, %d; want %q, %d", tt.path, f, l, tt.wantFile, tt.wantLine)
			}
		})
	}
}

// TestFingerprintNilVersusEmpty checks that an absent list or map and an
// empty one give different versions: a deployment without capabilities
// inherits the model's, and one with capabilities: [] has none.
func TestFingerprintNilVersusEmpty(t *testing.T) {
	const head = `version: 1
providers:
  - {id: p, type: openai, base_url: "https://x.example/v1"}
models:
  - id: m
    capabilities: [streaming, tools]
`
	dep := func(extra string) string {
		return head + "    deployments:\n      - provider: p\n        upstream_model: up\n" + extra +
			"        pricing: {input: \"1\", output: \"1\"}\n"
	}
	tests := []struct {
		name string
		a, b string
		same bool
	}{
		{"capabilities absent vs empty", dep(""), dep("        capabilities: []\n"), false},
		{"capabilities empty twice", dep("        capabilities: []\n"), dep("        capabilities: []\n"), true},
		{"tags absent vs empty", dep(""), dep("        tags: []\n"), false},
		{"model capabilities empty vs one", dep("        capabilities: []\n"), dep("        capabilities: [streaming]\n"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := mustLoad(t, tt.a, nil, nil), mustLoad(t, tt.b, nil, nil)
			if (a.Version() == b.Version()) != tt.same {
				t.Errorf("versions %s and %s; want same = %v", a.Version(), b.Version(), tt.same)
			}
		})
	}
	// The nil-ness the versions must reflect.
	a := mustLoad(t, dep(""), nil, nil).Config().Models[0].Deployments[0]
	b := mustLoad(t, dep("        capabilities: []\n"), nil, nil).Config().Models[0].Deployments[0]
	if a.Capabilities != nil || b.Capabilities == nil {
		t.Fatalf("decoded capabilities %#v and %#v; want nil and empty", a.Capabilities, b.Capabilities)
	}
}
