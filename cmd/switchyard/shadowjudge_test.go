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

// TestValidateShadowJudgePin: shadow sampling pins the judge to its
// cheapest deployment that every sampling policy permits, so a price
// change can never make the config invalid; config validate rejects a
// judge none of whose deployments a sampling policy permits, naming the
// cheapest one and why (both reviews' M1, P10 review H1).
func TestValidateShadowJudgePin(t *testing.T) {
	for _, judge := range []string{"judge", "judge@eu"} {
		if code, out, errOut := runCLI(t, env{}, "config", "validate", "-c", writeConfig(t, shadowJudgeConfig(judge))); code != exitOK {
			t.Errorf("%s: exit %d\n%s\n%s", judge, code, out, errOut)
		}
	}
	// Each sampling policy permits a judge deployment, but none permits
	// both.
	refused := strings.Replace(shadowJudgeConfig("judge"), "shadow: {sample_rate: 0.1}}\n",
		"shadow: {sample_rate: 0.1}}\n  - {id: other, baseline_model: big, deny: [judge@eu], shadow: {sample_rate: 0.1}}\n", 1)
	code, out, errOut := runCLI(t, env{}, "config", "validate", "-c", writeConfig(t, refused))
	if code != exitError || !strings.Contains(errOut, "judge@us") || !strings.Contains(errOut, `policy "default"`) ||
		!strings.Contains(errOut, "lacks required tag eu") || !strings.Contains(errOut, "evaluation.judge.model: model@provider") {
		t.Errorf("no permitted judge deployment: exit %d\n%s\n%s", code, out, errOut)
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

// TestValidateSemanticEmbeddingPolicy: the semantic cache embeds with the
// cheapest deployment that every policy enabling it permits; config
// validate rejects an embedding model none of whose deployments such a
// policy permits, naming the cheapest one and why (security review M1,
// P10 review H1).
func TestValidateSemanticEmbeddingPolicy(t *testing.T) {
	for _, model := range []string{"emb", "emb@eu"} {
		if code, out, errOut := runCLI(t, env{}, "config", "validate", "-c", writeConfig(t, semanticPolicyConfig(model))); code != exitOK {
			t.Errorf("%s: exit %d\n%s\n%s", model, code, out, errOut)
		}
	}
	// Each policy permits an embedding deployment, but none permits both.
	refused := strings.Replace(semanticPolicyConfig("emb"), "cache: {semantic: {enabled: true}}}\n",
		"cache: {semantic: {enabled: true}}}\n  - {id: other, deny: [emb@eu], cache: {semantic: {enabled: true}}}\n", 1)
	code, out, errOut := runCLI(t, env{}, "config", "validate", "-c", writeConfig(t, refused))
	if code != exitError || !strings.Contains(errOut, "emb@us") || !strings.Contains(errOut, `policy "default"`) ||
		!strings.Contains(errOut, "lacks required tag eu") {
		t.Errorf("no permitted embedding deployment: exit %d\n%s\n%s", code, out, errOut)
	}
}
