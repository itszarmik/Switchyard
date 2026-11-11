package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// lintClock is the fixed time catalog lint tests run at.
var lintClock = func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) }

// lintHead is a config prefix with two providers and a tenant, so that the
// only findings come from the models section that follows.
const lintHead = `version: 1
providers:
  - {id: a, type: openai, base_url: 'https://a.example/v1', api_key: '${A_KEY}'}
  - {id: b, type: anthropic, base_url: 'https://b.example'}
tenants:
  - {id: t, keys: [{id: k, hash: 'sha256:0000000000000000000000000000000000000000000000000000000000000000'}]}
`

// documented is pricing metadata that lint accepts at lintClock.
const documented = "source: 'https://a.example/pricing', checked: 2026-11-01"

// writeConfig writes a config file into a temporary directory and returns
// its path.
func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "switchyard.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runLint runs `catalog lint -c path` at lintClock.
func runLint(t *testing.T, e env, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = runCatalog(append([]string{"lint"}, args...), &out, &errOut, e.lookup, lintClock)
	return code, out.String(), errOut.String()
}

func TestCatalogLint(t *testing.T) {
	lintEnv := env{"A_KEY": "sk-lint-test-9f8e7d6c5b4a39281706"}
	tests := []struct {
		name     string
		models   string
		wantCode int
		// wantOut lists the stdout lines with the file name replaced by
		// FILE; wantErr is a regexp stderr must match ("" means empty).
		wantOut []string
		wantErr string
	}{
		{
			name: "clean",
			models: "models:\n  - id: m\n    capabilities: [streaming]\n    deployments:\n" +
				"      - {provider: a, upstream_model: u, pricing: {input: 1, output: 2, " + documented + "}}\n" +
				"aliases: {fast: m}\n",
			wantOut: []string{"FILE: catalog lint OK (1 model, 1 alias; 0 warnings, 0 notices)"},
		},
		{
			name: "notices do not fail",
			models: "models:\n  - id: m\n    deployments:\n" +
				"      - {provider: a, upstream_model: u, pricing: {input: 2, output: 1, " + documented + "}}\n",
			wantOut: []string{
				"FILE:8: notice: models[0].capabilities: a chat model without capability streaming; streamed requests never go to it",
				"FILE:10: notice: models[0].deployments[0].pricing.output: unusual: output cheaper than input",
				"FILE: catalog lint OK (1 model, 0 aliases; 0 warnings, 2 notices)",
			},
		},
		{
			name: "missing prices fail",
			models: "models:\n  - id: m\n    capabilities: [streaming]\n    deployments:\n" +
				"      - {provider: a, upstream_model: u, pricing: {" + documented + "}}\n" +
				"      - {provider: b, upstream_model: u, pricing: {input: 3, output: 15}}\n",
			wantCode: exitError,
			wantOut: []string{
				"FILE:11: warning: models[0].deployments[0].pricing: pricing unknown; this deployment is ranked last",
				"FILE:12: warning: models[0].deployments[1].pricing.checked: missing; record the date the prices were checked, as YYYY-MM-DD",
				"FILE:12: warning: models[0].deployments[1].pricing.source: missing; record the URL the prices were checked against",
				"FILE: catalog lint FAILED (1 model, 0 aliases; 3 warnings, 0 notices)",
			},
		},
		{
			name: "stale prices fail",
			models: "models:\n  - id: m\n    capabilities: [streaming]\n    deployments:\n" +
				"      - {provider: a, upstream_model: u, pricing: {input: 1, output: 2, source: 'https://a.example/p', checked: 2026-01-01}}\n",
			wantCode: exitError,
			wantOut: []string{
				"FILE:11: warning: models[0].deployments[0].pricing.checked: prices were checked 314 days ago, more than 90; check them again",
				"FILE: catalog lint FAILED (1 model, 0 aliases; 1 warning, 0 notices)",
			},
		},
		{
			name: "quality out of range is a config error",
			models: "models:\n  - id: m\n    capabilities: [streaming]\n    quality: {coding: 1.2}\n    deployments:\n" +
				"      - {provider: a, upstream_model: u, pricing: {input: 1, output: 2, " + documented + "}}\n",
			wantCode: exitError,
			wantErr:  `^FILE:10: error: models\[0\]\.quality\.coding: must be between 0 and 1\n1 error, 0 warnings\n$`,
		},
		{
			name: "unknown provider is a config error",
			models: "models:\n  - id: m\n    capabilities: [streaming]\n    deployments:\n" +
				"      - {provider: zzz, upstream_model: u, pricing: {input: 1, output: 2, " + documented + "}}\n",
			wantCode: exitError,
			wantErr:  `^FILE:11: error: models\[0\]\.deployments\[0\]\.provider: unknown provider "zzz" \(known providers: "a", "b"\)\n1 error, 0 warnings\n$`,
		},
		{
			name: "duplicate ids are a config error",
			models: "models:\n" +
				"  - {id: m, capabilities: [streaming], deployments: [{provider: a, upstream_model: u, pricing: {input: 1, output: 2, " + documented + "}}]}\n" +
				"  - {id: m, capabilities: [streaming], deployments: [{provider: a, upstream_model: u, pricing: {input: 1, output: 2, " + documented + "}}]}\n",
			wantCode: exitError,
			wantErr:  `^FILE:9: error: models\[1\]\.id: duplicate model id "m"; first defined at FILE:8\n1 error, 0 warnings\n$`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, lintHead+tt.models)
			code, stdout, stderr := runLint(t, lintEnv, "-c", path)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, tt.wantCode, stdout, stderr)
			}
			stdout = strings.ReplaceAll(stdout, path, "FILE")
			stderr = strings.ReplaceAll(stderr, path, "FILE")
			var got []string
			if stdout != "" {
				got = strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
			}
			if !equalLines(got, tt.wantOut) {
				t.Errorf("stdout =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tt.wantOut, "\n"))
			}
			if tt.wantErr == "" {
				if stderr != "" {
					t.Errorf("stderr = %q, want nothing", stderr)
				}
			} else if !regexp.MustCompile(tt.wantErr).MatchString(stderr) {
				t.Errorf("stderr = %q, want a match for %q", stderr, tt.wantErr)
			}
			if strings.Contains(stdout+stderr, lintEnv["A_KEY"]) {
				t.Errorf("output contains the API key:\n%s%s", stdout, stderr)
			}
		})
	}
}

func TestCatalogLintUsage(t *testing.T) {
	valid := writeConfig(t, lintHead)
	tests := []struct {
		name      string
		args      []string
		env       env
		wantCode  int
		wantOutRE string // "" means stdout must be empty
		wantErrRE string // "" means stderr must be empty
	}{
		{name: "catalog without subcommand", args: []string{"catalog"}, wantCode: exitUsage,
			wantErrRE: `(?s)^error: catalog: missing subcommand\n.*Usage:`},
		{name: "unknown catalog subcommand", args: []string{"catalog", "check"}, wantCode: exitUsage,
			wantErrRE: `(?s)^error: catalog: unknown subcommand "check"\n.*Usage:`},
		{name: "unknown flag", args: []string{"catalog", "lint", "--log-level", "debug"}, wantCode: exitUsage,
			wantErrRE: `flag provided but not defined: -log-level`},
		{name: "extra argument", args: []string{"catalog", "lint", "-c", valid, "extra"}, wantCode: exitUsage,
			wantErrRE: `(?s)^error: catalog lint: unexpected argument "extra"\nUsage: switchyard catalog lint \[-c FILE\]`},
		{name: "help", args: []string{"catalog", "lint", "-h"}, wantCode: exitOK,
			wantErrRE: `^Usage: switchyard catalog lint \[-c FILE\]`},
		{name: "missing file", args: []string{"catalog", "lint", "-c", "testdata/nope.yaml"}, wantCode: exitError,
			wantErrRE: `^error: testdata/nope.yaml: no such file\n$`},
		{name: "default path", args: []string{"catalog", "lint"}, wantCode: exitError,
			wantErrRE: `^error: switchyard.yaml: no such file\n$`},
		{name: "SWITCHYARD_CONFIG", args: []string{"catalog", "lint"}, env: env{"SWITCHYARD_CONFIG": valid, "A_KEY": "k"},
			wantOutRE: `^.*switchyard.yaml: catalog lint OK \(0 models, 0 aliases; 0 warnings, 0 notices\)\n$`},
		{name: "--config", args: []string{"catalog", "lint", "--config", valid}, env: env{"A_KEY": "k"},
			wantOutRE: `catalog lint OK`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env == nil {
				tt.env = env{}
			}
			// Through run, so the dispatch in main.go is covered too.
			code, stdout, stderr := runCLI(t, tt.env, tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, tt.wantCode, stdout, stderr)
			}
			for _, s := range []struct{ name, got, re string }{{"stdout", stdout, tt.wantOutRE}, {"stderr", stderr, tt.wantErrRE}} {
				if s.re == "" {
					if s.got != "" {
						t.Errorf("%s = %q, want nothing", s.name, s.got)
					}
				} else if !regexp.MustCompile(s.re).MatchString(s.got) {
					t.Errorf("%s = %q, want a match for %q", s.name, s.got, s.re)
				}
			}
		})
	}
}
