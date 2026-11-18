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

// semanticPolicyConfig has an embedding model with a cheap untagged
// deployment and a pricier one tagged eu, and a policy that requires eu
// and turns the semantic cache on.
func semanticPolicyConfig(model string) string {
	return fmt.Sprintf(`version: 1
providers:
  - {id: us, type: openai, base_url: "http://127.0.0.1:1/v1", api_key: '${FAKE_KEY:-}'}
  - {id: eu, type: openai, base_url: "http://127.0.0.1:1/v1", api_key: '${FAKE_KEY:-}', tags: [eu]}
models:
  - id: chat
    deployments: [{provider: eu, upstream_model: chat-up, pricing: {input: "3", output: "15"}}]
  - id: emb
    type: embedding
    dimensions: 8
    deployments:
      - {provider: us, upstream_model: emb-up, pricing: {input: "0.01"}}
      - {provider: eu, upstream_model: emb-up, pricing: {input: "0.02"}}
semantic_cache: {embedding_model: %s}
policies:
  - {id: default, require_tags: [eu], cache: {semantic: {enabled: true}}}
tenants:
  - {id: t, keys: [{id: k, hash: 'sha256:0000000000000000000000000000000000000000000000000000000000000000'}]}
`, model)
}

// TestValidateSemanticEmbeddingPolicy: config validate checks the exact
// deployment the semantic cache embeds with, as the runtime picks it,
// against every policy that enables the semantic cache, and names it
// (security review M1).
func TestValidateSemanticEmbeddingPolicy(t *testing.T) {
	code, out, errOut := runCLI(t, env{}, "config", "validate", "-c", writeConfig(t, semanticPolicyConfig("emb")))
	if code != exitError || !strings.Contains(errOut, "emb@us") || !strings.Contains(errOut, `policy "default"`) ||
		!strings.Contains(errOut, "lacks required tag eu") {
		t.Errorf("embedding deployment the policy refuses: exit %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut := runCLI(t, env{}, "config", "validate", "-c", writeConfig(t, semanticPolicyConfig("emb@eu"))); code != exitOK {
		t.Errorf("emb@eu: exit %d\n%s\n%s", code, out, errOut)
	}
}
