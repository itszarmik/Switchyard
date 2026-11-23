package eval_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/eval"
)

// judgeChangeYAML has a judge model on a (1/2, then 4/20 from 2026-12-01)
// and on b (3/6), and a sampling policy that permits judge@a only.
const judgeChangeYAML = `version: 1
providers:
  - {id: a, type: openai, base_url: "http://127.0.0.1:1/v1"}
  - {id: b, type: openai, base_url: "http://127.0.0.1:1/v1"}
models:
  - id: small
    deployments: [{provider: a, upstream_model: small, pricing: {input: "0.1", output: "0.4", source: 'https://a.example/p', checked: 2026-11-20}}]
  - id: large
    deployments: [{provider: a, upstream_model: large, pricing: {input: "1", output: "2", source: 'https://a.example/p', checked: 2026-11-20}}]
  - id: judge
    deployments:
      - {provider: a, upstream_model: judge, pricing: {input: "1", output: "2", source: 'https://a.example/p', checked: 2026-11-20, changes: [{from: 2026-12-01, input: "4", output: "20"}]}}
      - {provider: b, upstream_model: judge, pricing: {input: "3", output: "6", source: 'https://a.example/p', checked: 2026-11-20}}
evaluation:
  judge: {model: judge}
  shadow: {daily_budget_usd: "1"}
policies:
  - {id: default, baseline_model: large, allow: [small@a, large@a, judge@a], shadow: {sample_rate: 0.5}}
tenants:
  - {id: t, keys: [{id: k, hash: 'sha256:0000000000000000000000000000000000000000000000000000000000000000'}]}
`

// TestShadowJudgeAcrossPriceChange (P10 review H1): the judge is the
// cheapest deployment every sampling policy permits, so a dated change
// that makes a refused deployment the cheapest changes nothing; the config
// loads, and lint finds no warning, on both sides of the change.
func TestShadowJudgeAcrossPriceChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(judgeChangeYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{
		time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC),
	} {
		snap, err := config.Load(path, config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }, Now: func() time.Time { return at }})
		if err != nil {
			t.Fatalf("at %v: %v", at, err)
		}
		cat, err := catalog.NewAt(snap.Config(), at)
		if err != nil {
			t.Fatal(err)
		}
		d, err := eval.ShadowJudge(cat, snap.Config())
		if err != nil || d == nil || d.ID != "judge@a" {
			t.Errorf("at %v: judge %v, %v; want judge@a", at, d, err)
		}
		for _, f := range catalog.Lint(snap, at) {
			if f.Level == catalog.LevelWarning {
				t.Errorf("at %v: lint warning %s", at, f)
			}
		}
	}
	// Without the allow list the cheapest deployment judges: judge@b from
	// the change.
	snap, err := config.Load(path, config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	cfg := *snap.Config()
	cfg.Policies = []config.Policy{cfg.Policies[0]}
	cfg.Policies[0].Allow = nil
	cat, err := catalog.NewAt(&cfg, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if d, err := eval.ShadowJudge(cat, &cfg); err != nil || d.ID != "judge@b" {
		t.Errorf("without the allow list: %v, %v; want judge@b", d, err)
	}
}
