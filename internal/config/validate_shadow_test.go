package config

import (
	"strings"
	"testing"
)

// shadowYAML returns a config with chat model m (on provider a), judge
// model j (on a, and on b with tag zdr), embedding model e, and the given
// extra sections.
func shadowYAML(extra ...string) string {
	const judge = "  - id: j\n    deployments:\n" +
		"      - {provider: a, upstream_model: j, pricing: {input: 1, output: 2}}\n" +
		"      - {provider: b, upstream_model: j, pricing: {input: 1, output: 2}, tags: [zdr]}\n"
	return catalogYAML(chatModel("", "")+judge+embeddingModel("dimensions: 8"), strings.Join(extra, ""))
}

const (
	evalOK     = "evaluation: {judge: {model: j}, shadow: {daily_budget_usd: 5}}\n"
	samplingOn = "{id: p, baseline_model: m, shadow: {sample_rate: 0.1}}"
)

func TestShadowDefaults(t *testing.T) {
	cfg := mustLoad(t, shadowYAML(), nil, nil).Config()
	got := cfg.Evaluation.Shadow
	want := ShadowConfig{Concurrency: 4, MaxOutputTokens: 1024, VerifiedMinSamples: 30, VerifiedMinPassRate: 0.9}
	if got != want {
		t.Errorf("evaluation.shadow defaults = %+v, want %+v", got, want)
	}
	// Set values are kept.
	cfg = mustLoad(t, shadowYAML("evaluation: {shadow: {concurrency: 2, max_output_tokens: 50, verified_min_samples: 5, verified_min_pass_rate: 0.5}}\n"), nil, nil).Config()
	got = cfg.Evaluation.Shadow
	want = ShadowConfig{Concurrency: 2, MaxOutputTokens: 50, VerifiedMinSamples: 5, VerifiedMinPassRate: 0.5}
	if got != want {
		t.Errorf("evaluation.shadow = %+v, want %+v", got, want)
	}
}

func TestShadowValidation(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		wants []want
	}{
		{"off by default", shadowYAML(policiesYAML("{id: p}")), nil},
		{"rate 0 needs nothing", shadowYAML(policiesYAML("{id: p, shadow: {sample_rate: 0}}")), nil},
		{"sampling with everything", shadowYAML(evalOK, policiesYAML(samplingOn)), nil},
		{"rate 1", shadowYAML(evalOK, policiesYAML("{id: p, baseline_model: m, shadow: {sample_rate: 1}}")), nil},
		{"judge pinned", shadowYAML("evaluation: {judge: {model: j@b}, shadow: {daily_budget_usd: 5}}\n", policiesYAML(samplingOn)), nil},
		{"observe mode", shadowYAML(evalOK, policiesYAML("{id: p, routing_mode: observe, baseline_model: m, shadow: {sample_rate: 0.5}}")), nil},

		{"rate above 1", shadowYAML(evalOK, policiesYAML("{id: p, baseline_model: m, shadow: {sample_rate: 1.5}}")),
			[]want{errAt("policies[0].shadow.sample_rate", "must be between 0 and 1, got 1.5")}},
		{"rate negative", shadowYAML(policiesYAML("{id: p, shadow: {sample_rate: -0.1}}")),
			[]want{errAt("policies[0].shadow.sample_rate", "must be between 0 and 1, got -0.1")}},
		{"rate NaN", shadowYAML(policiesYAML("{id: p, shadow: {sample_rate: .nan}}")),
			[]want{errAt("policies[0].shadow.sample_rate", "expected a finite number")}},

		{"sampling needs judge, budget and baseline", shadowYAML(policiesYAML("{id: q}", "{id: p, shadow: {sample_rate: 0.1}}")),
			[]want{
				errAt("evaluation.judge.model", "required when shadow sampling is on (policies[1].shadow.sample_rate > 0)"),
				errAt("evaluation.shadow.daily_budget_usd", "required when shadow sampling is on (policies[1].shadow.sample_rate > 0)"),
				errAt("policies[1].baseline_model", "required when shadow.sample_rate > 0"),
			}},
		{"budget zero", shadowYAML("evaluation: {judge: {model: j}, shadow: {daily_budget_usd: 0}}\n", policiesYAML(samplingOn)),
			[]want{errAt("evaluation.shadow.daily_budget_usd", "must be greater than 0")}},
		{"budget zero without sampling", shadowYAML("evaluation: {shadow: {daily_budget_usd: 0}}\n"),
			[]want{errAt("evaluation.shadow.daily_budget_usd", "must be greater than 0")}},

		{"judge denied", shadowYAML(evalOK, policiesYAML("{id: p, baseline_model: m, deny: ['j@*'], shadow: {sample_rate: 0.1}}")),
			[]want{errAt("policies[0].shadow.sample_rate", `permit no deployment of the judge model "j"; shadow sampling sends this policy's prompts and answers to the judge`)}},
		{"judge allowed on one deployment", shadowYAML(evalOK, policiesYAML("{id: p, baseline_model: m, deny: ['j@a'], shadow: {sample_rate: 0.1}}")), nil},
		{"require_tags excludes the baseline", shadowYAML(evalOK, policiesYAML("{id: p, baseline_model: m, require_tags: [zdr], shadow: {sample_rate: 0.1}}")),
			[]want{errAt("policies[0].shadow.sample_rate", `permit no deployment of its baseline_model "m"`)}},
		{"require_tags and a pinned judge without the tag", shadowYAML("evaluation: {judge: {model: j@a}, shadow: {daily_budget_usd: 5}}\n",
			policiesYAML("{id: p, baseline_model: j, require_tags: [zdr], shadow: {sample_rate: 0.1}}")),
			[]want{errAt("policies[0].shadow.sample_rate", `permit no deployment of the judge model "j@a"`)}},
		{"allow list without the judge", shadowYAML(evalOK, policiesYAML("{id: p, baseline_model: m, allow: ['m@*'], shadow: {sample_rate: 0.1}}")),
			[]want{errAt("policies[0].shadow.sample_rate", "judge model")}},
		{"judge without prices", catalogYAML(chatModel("", "")+"  - id: j\n    deployments: [{provider: a, upstream_model: j}]\n",
			evalOK+policiesYAML(samplingOn)),
			[]want{errAt("evaluation.judge.model", `the judge model "j" has no deployment with input and output prices`)}},
		{"policies without sampling are not checked", shadowYAML(evalOK, policiesYAML(samplingOn, "{id: q, deny: ['*']}")), nil},

		{"concurrency negative", shadowYAML("evaluation: {shadow: {concurrency: -1}}\n"),
			[]want{errAt("evaluation.shadow.concurrency", "must be between 1 and 1024, got -1")}},
		{"max_output_tokens too large", shadowYAML("evaluation: {shadow: {max_output_tokens: 2000000}}\n"),
			[]want{errAt("evaluation.shadow.max_output_tokens", "must be between 1 and 1000000, got 2000000")}},
		{"verified_min_samples negative", shadowYAML("evaluation: {shadow: {verified_min_samples: -3}}\n"),
			[]want{errAt("evaluation.shadow.verified_min_samples", "must be between 1 and 1000000000, got -3")}},
		{"verified_min_pass_rate above 1", shadowYAML("evaluation: {shadow: {verified_min_pass_rate: 1.2}}\n"),
			[]want{errAt("evaluation.shadow.verified_min_pass_rate", "must be greater than 0 and at most 1, got 1.2")}},
		{"verified_min_pass_rate negative", shadowYAML("evaluation: {shadow: {verified_min_pass_rate: -0.5}}\n"),
			[]want{errAt("evaluation.shadow.verified_min_pass_rate", "must be greater than 0 and at most 1, got -0.5")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkProblems(t, problemsOf(t, tt.yaml, nil, nil), tt.wants...)
		})
	}
}

// TestShadowJudgeModelChecks: with sampling on, a judge model that names no
// chat deployment is reported exactly once at evaluation.judge.model, by
// whichever validator reports it first (validateEvaluation checks the judge
// whenever it is set; validateShadow only adds the check when it did not).
func TestShadowJudgeModelChecks(t *testing.T) {
	for _, judge := range []string{"nope", "e", "j@zz"} {
		t.Run(judge, func(t *testing.T) {
			yaml := shadowYAML("evaluation: {judge: {model: "+judge+"}, shadow: {daily_budget_usd: 5}}\n", policiesYAML(samplingOn))
			var at []Problem
			for _, p := range problemsOf(t, yaml, nil, nil) {
				if p.Path == pathJudgeModel {
					at = append(at, p)
				} else if p.Severity == SeverityError {
					t.Errorf("unexpected problem: %s", p)
				}
			}
			if len(at) != 1 || at[0].Severity != SeverityError {
				t.Fatalf("problems at %s = %v, want one error", pathJudgeModel, at)
			}
		})
	}
}

// TestShadowJudgeMessages covers validateShadow's own judge messages, for
// when validateEvaluation has not reported the judge model.
func TestShadowJudgeMessages(t *testing.T) {
	models := map[string]*Model{
		"j": {ID: "j", Type: ModelTypeChat, Deployments: []Deployment{{Provider: "a", Tags: []string{"d"}}}},
		"e": {ID: "e", Type: ModelTypeEmbedding, Deployments: []Deployment{{Provider: "a"}}},
	}
	tags := map[string][]string{"a": {"p"}}
	tests := []struct{ ref, msg string }{
		{"nope", "unknown model"},
		{"e", "must be a chat model, not the embedding model"},
		{"j@zz", "no deployment of the model is on the provider of"},
	}
	for _, tt := range tests {
		if deps, msg := chatDeployments(tt.ref, models, tags); msg != tt.msg || deps != nil {
			t.Errorf("chatDeployments(%q) = %v, %q; want nil, %q", tt.ref, deps, msg, tt.msg)
		}
	}
	deps, msg := chatDeployments("j@a", models, tags)
	if msg != "" || len(deps) != 1 || deps[0].id != "j@a" || strings.Join(deps[0].tags, ",") != "p,d" {
		t.Errorf("chatDeployments(j@a) = %+v, %q", deps, msg)
	}
}
