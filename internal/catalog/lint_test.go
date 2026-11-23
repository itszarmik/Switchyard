package catalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/config"
)

// loadSnapshot writes yaml to a file in a temporary directory and loads it.
func loadSnapshot(t *testing.T, yaml string) (*config.Snapshot, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := config.Load(path, config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return snap, path
}

var lintNow = time.Date(2026, 11, 11, 15, 4, 5, 0, time.UTC)

func TestLint(t *testing.T) {
	const head = "version: 1\nproviders:\n  - {id: a, type: openai, base_url: 'https://a.example'}\n  - {id: b, type: openai, base_url: 'https://b.example'}\nmodels:\n"
	const documented = "source: 'https://a.example/pricing', checked: 2026-11-01"
	tests := []struct {
		name   string
		models string
		want   []string // "LINE: LEVEL: PATH: MESSAGE", the file name stripped
	}{
		{
			name: "clean",
			models: "  - id: m\n    capabilities: [streaming]\n    deployments:\n" +
				"      - {provider: a, upstream_model: u, pricing: {input: 1, output: 2, " + documented + "}}\n" +
				"  - id: e\n    type: embedding\n    dimensions: 8\n    deployments:\n" +
				"      - {provider: a, upstream_model: u, pricing: {input: 0.02, " + documented + "}}\n",
		},
		{
			name: "missing prices, source and date",
			models: "  - id: m\n    capabilities: [streaming]\n    deployments:\n" +
				"      - {provider: a, upstream_model: u}\n" +
				"      - {provider: b, upstream_model: u, pricing: {input: 1}}\n",
			want: []string{
				"9: warning: models[0].deployments[0].pricing: " + config.MsgPricingUnknown,
				"9: warning: models[0].deployments[0].pricing.checked: missing; record the date the prices were checked, as YYYY-MM-DD",
				"9: warning: models[0].deployments[0].pricing.source: missing; record the URL the prices were checked against",
				"10: warning: models[0].deployments[1].pricing: " + config.MsgPricingUnknown,
				"10: warning: models[0].deployments[1].pricing.checked: missing; record the date the prices were checked, as YYYY-MM-DD",
				"10: warning: models[0].deployments[1].pricing.source: missing; record the URL the prices were checked against",
			},
		},
		{
			name: "only checked missing",
			models: "  - id: m\n    capabilities: [streaming]\n    deployments:\n" +
				"      - provider: a\n        upstream_model: u\n        pricing:\n          input: 1\n          output: 2\n          source: 'https://a.example/p'\n",
			want: []string{"12: warning: models[0].deployments[0].pricing.checked: missing; record the date the prices were checked, as YYYY-MM-DD"},
		},
		{
			name: "stale and fresh dates",
			models: "  - id: m\n    capabilities: [streaming]\n    deployments:\n" +
				"      - provider: a\n        upstream_model: u\n        pricing: {input: 1, output: 2, source: 'https://a.example/p', checked: 2026-08-12}\n" +
				"      - provider: b\n        upstream_model: u\n        pricing: {input: 1, output: 2, source: 'https://a.example/p', checked: 2026-08-13}\n",
			want: []string{"11: warning: models[0].deployments[0].pricing.checked: prices were checked 91 days ago, more than 90; check them again"},
		},
		{
			name: "notices",
			models: "  - id: m\n    deployments:\n" +
				"      - {provider: a, upstream_model: u, pricing: {input: 2, output: 1, " + documented + "}}\n" +
				"      - {provider: b, upstream_model: u, pricing: {input: 2, output: 2, " + documented + "}}\n" +
				"  - id: n\n    capabilities: [streaming]\n    deployments:\n" +
				"      - {provider: a, upstream_model: u, capabilities: [tools], pricing: {input: 1, output: 1, " + documented + "}}\n" +
				"      - {provider: b, upstream_model: u, pricing: {input: 1, output: 1, " + documented + "}}\n" +
				"  - id: e\n    type: embedding\n    dimensions: 8\n    deployments:\n" +
				"      - {provider: a, upstream_model: u, pricing: {input: 0.02, output: 0.01, " + documented + "}}\n",
			want: []string{
				"6: notice: models[0].capabilities: a chat model without capability streaming; streamed requests never go to it",
				"8: notice: models[0].deployments[0].pricing.output: unusual: output cheaper than input",
				"13: notice: models[1].deployments[0].capabilities: a chat deployment without capability streaming; streamed requests never go to it",
			},
		},
		{
			name: "price changes",
			models: "  - id: m\n    capabilities: [streaming]\n    deployments:\n" +
				"      - provider: a\n        upstream_model: u\n        pricing:\n          input: 1\n          output: 2\n" +
				"          source: 'https://a.example/pricing'\n          checked: 2026-11-01\n          changes:\n" +
				"            - {from: 2026-11-01, input: 1.5}\n" +
				"            - {from: '2026-11-11T15:04:05Z', output: 3}\n" +
				"            - {from: 2026-12-01, input: 4, output: \"20.00\", cached_input: 0.2}\n" +
				"            - {from: 2027-01-01, output: 25}\n" +
				"      - {provider: b, upstream_model: u, pricing: {input: 1, output: 2, " + documented + ", changes: [{from: '2026-12-01T00:00:00-08:00', cache_write: 1.25}]}}\n" +
				"  - id: n\n    capabilities: [streaming]\n    deployments:\n" +
				"      - {provider: a, upstream_model: u, pricing: {input: 1, output: 2, " + documented + ", changes: [{from: 2026-11-11T15:04:06Z, input: 2}]}}\n",
			want: []string{
				"17: notice: models[0].deployments[0].pricing.changes[0].from: in force since 2026-11-01 00:00 UTC; fold it into the base prices and remove it when convenient",
				"18: notice: models[0].deployments[0].pricing.changes[1].from: in force since 2026-11-11 15:04:05 UTC; fold it into the base prices and remove it when convenient",
				"19: notice: models[0].deployments[0].pricing.changes[2]: next price change at 2026-12-01 00:00 UTC: input 4, output 20.00, cached_input 0.2",
				"21: notice: models[0].deployments[1].pricing.changes[0]: next price change at 2026-12-01 08:00 UTC: cache_write 1.25",
				"25: notice: models[1].deployments[0].pricing.changes[0]: next price change at 2026-11-11 15:04:06 UTC: input 2",
			},
		},
		{
			// P10 review M2: known or not is as of the prices in force at
			// now.
			name: "a change completes the prices",
			models: "  - id: m\n    capabilities: [streaming]\n    deployments:\n" +
				"      - {provider: a, upstream_model: u, pricing: {input: 1, " + documented + ", changes: [{from: 2026-11-01, output: 2}]}}\n" +
				"      - {provider: b, upstream_model: u, pricing: {input: 1, " + documented + ", changes: [{from: 2026-12-01, output: 2}]}}\n",
			want: []string{
				"9: notice: models[0].deployments[0].pricing.changes[0].from: in force since 2026-11-01 00:00 UTC; fold it into the base prices and remove it when convenient",
				"10: warning: models[0].deployments[1].pricing: " + config.MsgPricingUnknown,
				"10: notice: models[0].deployments[1].pricing.changes[0]: next price change at 2026-12-01 00:00 UTC: output 2",
			},
		},
		{
			// P10 review L2: every dated price set is checked.
			name: "a change makes output cheaper than input",
			models: "  - id: m\n    capabilities: [streaming]\n    deployments:\n" +
				"      - {provider: a, upstream_model: u, pricing: {input: 1, output: 2, " + documented + ", changes: [{from: 2026-12-01, output: 0.5}]}}\n" +
				"      - {provider: b, upstream_model: u, pricing: {input: 2, output: 1, " + documented + ", changes: [{from: 2026-12-01, cached_input: 0.1}]}}\n",
			want: []string{
				"9: notice: models[0].deployments[0].pricing.changes[0]: next price change at 2026-12-01 00:00 UTC: output 0.5",
				"9: notice: models[0].deployments[0].pricing.changes[0]: unusual: output cheaper than input from 2026-12-01 00:00 UTC",
				"10: notice: models[0].deployments[1].pricing.changes[0]: next price change at 2026-12-01 00:00 UTC: cached_input 0.1",
				"10: notice: models[0].deployments[1].pricing.output: unusual: output cheaper than input",
			},
		},
		{
			name: "an override with streaming silences the model notice",
			models: "  - id: m\n    deployments:\n" +
				"      - {provider: a, upstream_model: u, capabilities: [streaming], pricing: {input: 1, output: 1, " + documented + "}}\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap, path := loadSnapshot(t, head+tt.models+"tenants: [{id: t, keys: [{id: k, hash: 'sha256:"+
				"0000000000000000000000000000000000000000000000000000000000000000'}]}]\n")
			var got []string
			for _, f := range Lint(snap, lintNow) {
				if f.File != path {
					t.Errorf("finding in file %q, want %q", f.File, path)
				}
				f.File = ""
				got = append(got, f.String()[len("line "):])
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("findings (-want +got):\n%s", diff)
			}
		})
	}
}

// Config warnings outside the catalog are not lint findings.
func TestLintIgnoresOtherWarnings(t *testing.T) {
	snap, _ := loadSnapshot(t, "version: 1\nserver: {admin_listen: ':9090', admin_token: test.admin.token.for.the.lint.tests}\n")
	if len(snap.Warnings()) != 2 {
		t.Fatalf("warnings = %v, want the admin and no-tenants warnings", snap.Warnings())
	}
	if got := Lint(snap, lintNow); len(got) != 0 {
		t.Errorf("Lint = %v, want nothing", got)
	}
}

func TestFindingString(t *testing.T) {
	tests := []struct {
		f    Finding
		want string
	}{
		{Finding{Level: LevelWarning, File: "c.yaml", Line: 3, Path: "models[0]", Message: "m"}, "c.yaml:3: warning: models[0]: m"},
		{Finding{Level: LevelNotice, File: "c.yaml", Path: "models", Message: "m"}, "c.yaml: notice: models: m"},
		{Finding{Level: LevelNotice, Line: 2, Message: "m"}, "line 2: notice: m"},
		{Finding{Level: Level(7)}, "Level(7)"},
	}
	for _, tt := range tests {
		if got := tt.f.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}
