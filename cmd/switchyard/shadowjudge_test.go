package main

import (
	"fmt"
	"strings"
	"testing"
)

// shadowJudgeConfig has a judge model with a cheap untagged deployment and
// a pricier one tagged eu, and a sampling policy that requires eu.
func shadowJudgeConfig(judge string) string {
	return fmt.Sprintf(`version: 1
providers:
  - {id: us, type: openai, base_url: "http://127.0.0.1:1/v1", api_key: '${FAKE_KEY:-}'}
  - {id: eu, type: openai, base_url: "http://127.0.0.1:1/v1", api_key: '${FAKE_KEY:-}', tags: [eu]}
models:
  - id: big
    deployments: [{provider: eu, upstream_model: big-up, pricing: {input: "3", output: "15"}}]
  - id: judge
    deployments:
      - {provider: us, upstream_model: judge-up, pricing: {input: "1", output: "2"}}
      - {provider: eu, upstream_model: judge-up, pricing: {input: "2", output: "4"}}
evaluation:
  judge: {model: %s}
  shadow: {daily_budget_usd: "1"}
policies:
  - {id: default, baseline_model: big, require_tags: [eu], shadow: {sample_rate: 0.1}}
tenants:
  - {id: t, keys: [{id: k, hash: 'sha256:0000000000000000000000000000000000000000000000000000000000000000'}]}
`, judge)
}

// TestValidateShadowJudgePin: config validate checks the exact deployment
// shadow sampling pins the judge to, with the runtime's own pin, against
// every sampling policy, and names it (both reviews' M1).
func TestValidateShadowJudgePin(t *testing.T) {
	code, out, errOut := runCLI(t, env{}, "config", "validate", "-c", writeConfig(t, shadowJudgeConfig("judge")))
	if code != exitError || !strings.Contains(errOut, "judge@us") || !strings.Contains(errOut, `policy "default"`) ||
		!strings.Contains(errOut, "lacks required tag eu") || !strings.Contains(errOut, "evaluation.judge.model: model@provider") {
		t.Errorf("judge pinned to a deployment the policy refuses: exit %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut := runCLI(t, env{}, "config", "validate", "-c", writeConfig(t, shadowJudgeConfig("judge@eu"))); code != exitOK {
		t.Errorf("judge@eu: exit %d\n%s\n%s", code, out, errOut)
	}
}
