package catalog_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/policy"
	"switchyard/internal/quality"
	"switchyard/internal/router"
	"switchyard/internal/schema"
)

// overridesCatalogYAML has a cheap model whose coding prior is too low for
// hard coding requests at the balanced bar, and a pricier one that clears
// it. Model "bare" has no priors at all.
const overridesCatalogYAML = `version: 1
providers:
  - {id: p, type: openai, base_url: 'https://p.example/v1'}
models:
  - id: small
    quality: {general: 0.7, coding: 0.6}
    deployments:
      - {provider: p, upstream_model: small, pricing: {input: 0.1, output: 0.4}}
  - id: large
    quality: {general: 0.95, coding: 0.95}
    deployments:
      - {provider: p, upstream_model: large, pricing: {input: 3, output: 15}}
  - id: bare
    deployments:
      - {provider: p, upstream_model: bare, pricing: {input: 1, output: 1}}
`

// loadCatalog loads text, plus any included files, from a temporary
// directory and builds its catalog.
func loadCatalog(t *testing.T, text string, files map[string]string) *catalog.Catalog {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "switchyard.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := config.Load(path, config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cat, err := catalog.New(snap.Config())
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	return cat
}

// modelByID returns the catalog model id.
func modelByID(t *testing.T, cat *catalog.Catalog, id string) *catalog.Model {
	t.Helper()
	res, err := cat.Resolve(id)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", id, err)
	}
	return res.Model
}

// decideHardCoding routes a hard coding request for auto at the balanced
// bar and returns the primary deployment.
func decideHardCoding(t *testing.T, cat *catalog.Catalog) string {
	t.Helper()
	r := router.New(cat, nil, config.Routing{})
	dec, err := r.Decide(router.Input{
		Requested: "auto", Auto: true, Required: schema.CapabilitySet{},
		InputBytes: 4000, EstOut: 500,
		Assessment: quality.Assessment{Category: quality.CategoryCoding, Difficulty: 0.9},
		Policy: policy.Effective{
			Profile:      "balanced",
			Constraints:  policy.Constraints{MinQuality: 0.80, Objective: policy.ObjectiveCost},
			Substitution: policy.SubstitutionPinned, MaxQualityDrop: 0.05,
			RoutingMode: policy.ModeEnforce, OnUnmet: policy.OnUnmetBestEffort,
		},
		BudgetRemainingMicro: -1,
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(dec.Plan) == 0 {
		t.Fatal("Decide: empty plan")
	}
	return dec.Plan[0].Deployment.ID
}

func TestQualityOverridesReplacePriors(t *testing.T) {
	const overrides = `version: 1
quality_overrides:
  - {model: small, category: coding, value: 0.95, ci_low: 0.9, ci_high: 0.98, samples: 200, evaluated: 2026-11-12, source: test}
  - {model: bare, category: reasoning, value: 0.5, ci_low: 0.4, ci_high: 0.6, samples: 50, evaluated: 2026-11-12, source: test}
`
	base := loadCatalog(t, overridesCatalogYAML, nil)
	over := loadCatalog(t, overridesCatalogYAML+"include: [measured.yaml]\n", map[string]string{"measured.yaml": overrides})

	tests := []struct {
		model      string
		base, over map[string]float64
	}{
		{model: "small", base: map[string]float64{"general": 0.7, "coding": 0.6}, over: map[string]float64{"general": 0.7, "coding": 0.95}},
		{model: "large", base: map[string]float64{"general": 0.95, "coding": 0.95}, over: map[string]float64{"general": 0.95, "coding": 0.95}},
		{model: "bare", base: nil, over: map[string]float64{"reasoning": 0.5}},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if diff := cmp.Diff(tt.base, modelByID(t, base, tt.model).Quality); diff != "" {
				t.Errorf("without overrides (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.over, modelByID(t, over, tt.model).Quality); diff != "" {
				t.Errorf("with overrides (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("routing follows the measured prior", func(t *testing.T) {
		// q̂(small) = 1 − 0.9·(1 − 0.6) = 0.64 misses the 0.80 bar; with the
		// measured 0.95 it is 0.955, and small is the cheapest that clears it.
		if got := decideHardCoding(t, base); got != "large@p" {
			t.Errorf("without overrides: routed to %s, want large@p", got)
		}
		if got := decideHardCoding(t, over); got != "small@p" {
			t.Errorf("with overrides: routed to %s, want small@p", got)
		}
	})
}

func TestQualityOverrideOfUnknownModel(t *testing.T) {
	cfg := &config.Config{
		Providers:        []config.Provider{{ID: "p", Type: "openai"}},
		Models:           []config.Model{{ID: "m", Deployments: []config.Deployment{{Provider: "p", UpstreamModel: "m"}}}},
		QualityOverrides: []config.QualityOverride{{Model: "zz", Category: "general", Value: 0.5}},
	}
	if _, err := catalog.New(cfg); err == nil {
		t.Fatal("catalog.New: want an error for an override of an unknown model")
	}
}

func TestQualityOverridesDoNotAliasConfig(t *testing.T) {
	cfg := &config.Config{
		Providers:        []config.Provider{{ID: "p", Type: "openai"}},
		Models:           []config.Model{{ID: "m", Quality: map[string]float64{"general": 0.5}, Deployments: []config.Deployment{{Provider: "p", UpstreamModel: "m"}}}},
		QualityOverrides: []config.QualityOverride{{Model: "m", Category: "general", Value: 0.9}},
	}
	if _, err := catalog.New(cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Models[0].Quality["general"]; got != 0.5 {
		t.Errorf("config prior changed to %v; the catalog must not modify its config", got)
	}
}
