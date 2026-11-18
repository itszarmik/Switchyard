package semantic_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"switchyard/internal/cache/semantic"
	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/providers/openai"
)

// TestConfiguredDeployment: the deployment the semantic cache embeds with,
// as EmbeddingDeployment picks it, must pass the allow, deny and
// require_tags of every policy that enables the semantic cache, and of
// every rule whose set.cache enables it, with the rule's filters in place
// of the policy's (security review M1).
func TestConfiguredDeployment(t *testing.T) {
	const semOn = "{semantic: {enabled: true}}"
	tests := []struct {
		name     string
		model    string
		policies string
		want     string // the deployment, or "" for none
		wantErr  []string
	}{
		{"no policy enables it", "emb", `[{id: p, require_tags: [zdr]}]`, "", nil},
		{"the cheapest deployment is permitted", "emb", `[{id: p, cache: ` + semOn + `}]`, "emb@us", nil},
		{"the cheapest deployment lacks a required tag", "emb", `[{id: open}, {id: strict, require_tags: [zdr], cache: ` + semOn + `}]`, "",
			[]string{`"emb" embeds on emb@us`, `policy "strict"`, "lacks required tag zdr", "model@provider"}},
		{"a pinned permitted deployment", "emb@eu", `[{id: strict, require_tags: [zdr], cache: ` + semOn + `}]`, "emb@eu", nil},
		{"denied", "emb", `[{id: p, deny: ["emb@us"], cache: ` + semOn + `}]`, "", []string{"denied by policy"}},
		{"not allowed", "emb", `[{id: p, allow: ["chat@*"], cache: ` + semOn + `}]`, "", []string{"not allowed by policy"}},
		{"a policy without the semantic cache is not checked", "emb", `[{id: p, require_tags: [zdr], cache: {exact: {enabled: true}}}]`, "", nil},
		{"a rule enabling it, with its own filters", "emb",
			`[{id: p, rules: [{name: r, match: {key: k}, set: {require_tags: [zdr], cache: ` + semOn + `}}]}]`, "",
			[]string{`policy "p", rule "r"`, "lacks required tag zdr"}},
		{"a rule enabling it under the policy's filters", "emb",
			`[{id: p, deny: ["emb@*"], rules: [{name: r, match: {key: k}, set: {cache: ` + semOn + `}}]}]`, "",
			[]string{`rule "r"`, "denied by policy"}},
		{"a rule whose filters replace the policy's", "emb",
			`[{id: p, deny: ["emb@*"], rules: [{name: r, match: {key: k}, set: {deny: ["chat@*"], cache: ` + semOn + `}}]}]`, "emb@us", nil},
		{"a rule that turns it off is not checked", "emb",
			`[{id: p, rules: [{name: r, match: {key: k}, set: {require_tags: [zdr], cache: {exact: {enabled: true}}}}]}]`, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, cat, adapters := loadPolicyConfig(t, tt.model, tt.policies)
			d, emb, err := semantic.ConfiguredDeployment(cfg, cat, adapters)
			if len(tt.wantErr) > 0 {
				for _, w := range tt.wantErr {
					if err == nil || !strings.Contains(err.Error(), w) {
						t.Errorf("error %v, want one containing %q", err, w)
					}
				}
				return
			}
			switch {
			case err != nil:
				t.Fatal(err)
			case tt.want == "" && (d != nil || emb != nil):
				t.Errorf("= %v, want none", d)
			case tt.want != "" && (d == nil || d.ID != tt.want || emb == nil):
				t.Errorf("= %v, want %s", d, tt.want)
			}
		})
	}
}

// loadPolicyConfig loads a config with the embedding model emb on us
// (cheaper) and eu (tagged zdr), the given policies, and
// semantic_cache.embedding_model model.
func loadPolicyConfig(t *testing.T, model, policies string) (*config.Config, *catalog.Catalog, map[string]providers.Adapter) {
	t.Helper()
	text := `version: 1
providers:
  - {id: us, type: openai, base_url: "http://127.0.0.1:1/v1"}
  - {id: eu, type: openai, base_url: "http://127.0.0.1:1/v1", tags: [zdr]}
models:
  - id: emb
    type: embedding
    dimensions: 3
    deployments:
      - {provider: us, upstream_model: e, pricing: {input: "0.01"}}
      - {provider: eu, upstream_model: e, pricing: {input: "0.02"}}
  - id: chat
    deployments:
      - {provider: eu, upstream_model: c, pricing: {input: 1, output: 2}}
semantic_cache: {embedding_model: ` + model + `}
policies: ` + policies + `
tenants:
  - {id: t, keys: [{id: k, hash: 'sha256:0000000000000000000000000000000000000000000000000000000000000000'}]}
`
	path := filepath.Join(t.TempDir(), "switchyard.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := config.Load(path, config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := snap.Config()
	cat, err := catalog.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	adapters := map[string]providers.Adapter{}
	for _, p := range cfg.Providers {
		a, err := openai.New(p)
		if err != nil {
			t.Fatal(err)
		}
		adapters[p.ID] = a
	}
	return cfg, cat, adapters
}
