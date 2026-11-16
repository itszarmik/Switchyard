package config

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// overrideYAML is a quality_overrides section with one entry of model m
// with the given fields, or the default valid entry when fields is "".
func overrideYAML(fields string) string {
	if fields == "" {
		fields = "{model: m, category: coding, value: 0.7, ci_low: 0.6, ci_high: 0.8, samples: 40, evaluated: 2026-11-12, source: starter}"
	}
	return "quality_overrides:\n  - " + fields + "\n"
}

func TestQualityOverrideValidation(t *testing.T) {
	const ok = "model: m, category: coding, value: 0.7, ci_low: 0.6, ci_high: 0.8, samples: 40, evaluated: 2026-11-12"
	tests := []struct {
		name  string
		extra string
		wants []want
	}{
		{name: "valid", extra: overrideYAML("")},
		{name: "valid bounds", extra: overrideYAML("{model: m, category: general, value: 1, ci_low: 1, ci_high: 1, samples: 0, evaluated: 2026-01-01}")},
		{
			name:  "unknown model",
			extra: overrideYAML("{model: mm, category: coding, value: 0.7, ci_low: 0.6, ci_high: 0.8, samples: 1, evaluated: 2026-11-12}"),
			wants: []want{errAt("quality_overrides[0].model", `unknown model "mm" (did you mean "m"?)`)},
		},
		{
			name:  "missing model and category",
			extra: overrideYAML("{value: 0.7, ci_low: 0.6, ci_high: 0.8, evaluated: 2026-11-12}"),
			wants: []want{errAt("quality_overrides[0].model", "required"), errAt("quality_overrides[0].category", "required; use one of general")},
		},
		{
			name:  "unknown category",
			extra: overrideYAML("{model: m, category: codign, value: 0.7, ci_low: 0.6, ci_high: 0.8, evaluated: 2026-11-12}"),
			wants: []want{errAt("quality_overrides[0].category", `unknown quality category "codign" (did you mean "coding"?)`)},
		},
		{
			name:  "values out of range",
			extra: overrideYAML("{model: m, category: coding, value: 1.5, ci_low: -0.1, ci_high: 2, evaluated: 2026-11-12}"),
			wants: []want{
				errAt("quality_overrides[0].value", "must be between 0 and 1, got 1.5"),
				errAt("quality_overrides[0].ci_low", "must be between 0 and 1, got -0.1"),
				errAt("quality_overrides[0].ci_high", "must be between 0 and 1, got 2"),
			},
		},
		{
			name:  "interval does not contain the value",
			extra: overrideYAML("{model: m, category: coding, value: 0.5, ci_low: 0.6, ci_high: 0.4, evaluated: 2026-11-12}"),
			wants: []want{
				errAt("quality_overrides[0].ci_low", "must not be greater than value (0.5)"),
				errAt("quality_overrides[0].ci_high", "must not be less than value (0.5)"),
			},
		},
		{
			name:  "negative samples",
			extra: overrideYAML("{model: m, category: coding, value: 0.7, ci_low: 0.6, ci_high: 0.8, samples: -1, evaluated: 2026-11-12}"),
			wants: []want{errAt("quality_overrides[0].samples", "must not be negative")},
		},
		{
			name:  "missing date",
			extra: overrideYAML("{model: m, category: coding, value: 0.7, ci_low: 0.6, ci_high: 0.8}"),
			wants: []want{errAt("quality_overrides[0].evaluated", "required")},
		},
		{
			name:  "bad date",
			extra: overrideYAML("{model: m, category: coding, value: 0.7, ci_low: 0.6, ci_high: 0.8, evaluated: 12/11/2026}"),
			wants: []want{errAt("quality_overrides[0].evaluated", `must be a date as YYYY-MM-DD, got "12/11/2026"`)},
		},
		{
			name:  "duplicate pair",
			extra: overrideYAML("{"+ok+"}") + "  - {" + ok + "}\n  - {model: m, category: general, value: 0.7, ci_low: 0.6, ci_high: 0.8, evaluated: 2026-11-12}\n",
			wants: []want{errAt("quality_overrides[1]", `duplicate quality override for model "m", category "coding"; first defined at cfg.yaml:`)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := catalogYAML(chatModel("", ""), tt.extra)
			checkProblems(t, problemsOf(t, root, nil, nil), tt.wants...)
		})
	}
}

func TestQualityOverridesFromInclude(t *testing.T) {
	root := catalogYAML(chatModel("", ""), "include: [overrides/*.yaml]\n"+overrideYAML(""))
	files := fakeFS{"overrides/q.yaml": "version: 1\n" + overrideYAML("{model: m, category: general, value: 0.9, ci_low: 0.85, ci_high: 0.95, samples: 120, evaluated: 2026-11-10, source: \"prompts=starter judge=m@a reference=m\"}")}
	snap := mustLoad(t, root, nil, files)
	wantOverrides := []QualityOverride{
		{Model: "m", Category: "coding", Value: 0.7, CILow: 0.6, CIHigh: 0.8, Samples: 40, Evaluated: "2026-11-12", Source: "starter"},
		{Model: "m", Category: "general", Value: 0.9, CILow: 0.85, CIHigh: 0.95, Samples: 120, Evaluated: "2026-11-10", Source: "prompts=starter judge=m@a reference=m"},
	}
	if diff := cmp.Diff(wantOverrides, snap.Config().QualityOverrides); diff != "" {
		t.Errorf("quality_overrides (-want +got):\n%s", diff)
	}

	t.Run("an included override is validated where it is", func(t *testing.T) {
		files := fakeFS{"overrides/q.yaml": "# measured\n" + overrideYAML("{model: m, category: coding, value: 0.7, ci_low: 0.6, ci_high: 0.8, evaluated: 2026-11-12}")}
		checkProblems(t, problemsOf(t, root, nil, files), want{
			sev: SeverityError, file: "overrides/q.yaml", line: 3, path: "quality_overrides[1]",
			msg: `duplicate quality override for model "m", category "coding"; first defined at cfg.yaml:`,
		})
	})
	t.Run("included overrides must be a sequence", func(t *testing.T) {
		root := catalogYAML(chatModel("", ""), "include: [a.yaml]\n")
		files := fakeFS{"a.yaml": "quality_overrides: {model: m}\n"}
		checkProblems(t, problemsOf(t, root, nil, files), want{
			sev: SeverityError, file: "a.yaml", line: 1, path: "quality_overrides", msg: "expected a sequence, got a mapping",
		})
	})
}

func TestJudgeModelValidation(t *testing.T) {
	models := chatModel("", "") + embeddingModel("dimensions: 8")
	tests := []struct {
		name  string
		judge string
		wants []want
	}{
		{name: "unset"},
		{name: "model id", judge: "m"},
		{name: "deployment", judge: "m@a"},
		{name: "unknown model", judge: "mx", wants: []want{errAt("evaluation.judge.model", `unknown model "mx"; use a chat model id or model@provider (did you mean "m"?)`)}},
		{name: "unknown deployment model", judge: "zz@a", wants: []want{errAt("evaluation.judge.model", `unknown model "zz@a"`)}},
		{name: "provider does not serve it", judge: "m@b", wants: []want{errAt("evaluation.judge.model", `deployment "m@b" names a provider that does not serve that model`)}},
		{name: "embedding model", judge: "e", wants: []want{errAt("evaluation.judge.model", `model "e" has type embedding; the judge must be a chat model`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extra := ""
			if tt.judge != "" {
				extra = "evaluation:\n  judge:\n    model: " + tt.judge + "\n"
			}
			checkProblems(t, problemsOf(t, catalogYAML(models, extra), nil, nil), tt.wants...)
		})
	}
}
