package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// routeConfig has two tiers, a default policy with a baseline, and tenant
// t with key k.
const routeConfig = `version: 1
providers:
  - {id: a, type: openai, base_url: 'https://a.example/v1', api_key: '${A_KEY}'}
models:
  - id: small
    quality: {general: 0.6}
    deployments: [{provider: a, upstream_model: s, pricing: {input: "0.1", output: "0.4"}}]
  - id: large
    quality: {general: 0.95}
    deployments: [{provider: a, upstream_model: l, pricing: {input: "3", output: "15"}}]
policies:
  - {id: default, baseline_model: large}
tenants:
  - {id: t, keys: [{id: k, hash: 'sha256:0000000000000000000000000000000000000000000000000000000000000000'}]}
`

func runExplain(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runRoute(args, strings.NewReader(stdin), &out, &errOut, env{"A_KEY": "sk-route-test-0123456789abcdef"}.lookup)
	return code, out.String(), errOut.String()
}

func TestRouteExplain(t *testing.T) {
	cfg := writeConfig(t, routeConfig)
	req := `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`
	reqFile := filepath.Join(t.TempDir(), "req.json")
	if err := os.WriteFile(reqFile, []byte(req), 0o600); err != nil {
		t.Fatal(err)
	}

	code, first, errOut := runExplain(t, req, "explain", "-c", cfg, "--tenant", "t", "--key", "k", "--header", "X-Switchyard-Difficulty: high")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var exp struct {
		Policy     string   `json:"policy"`
		Plan       []string `json:"plan"`
		Assessment struct {
			Difficulty float64 `json:"difficulty"`
			Hinted     bool    `json:"hinted"`
		} `json:"assessment"`
	}
	if err := json.Unmarshal([]byte(first), &exp); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, first)
	}
	if exp.Policy != "default" || len(exp.Plan) != 1 || exp.Plan[0] != "large@a" || !exp.Assessment.Hinted || exp.Assessment.Difficulty != 0.85 {
		t.Errorf("explanation %s", first)
	}
	// The same request from a file, with "-" for stdin, gives the same bytes.
	if code, out, errOut := runExplain(t, "", "explain", "-c", cfg, "--tenant", "t", "--key", "k", "--header", "X-Switchyard-Difficulty: high", reqFile); code != exitOK || out != first {
		t.Errorf("from a file: exit %d %s\n%s", code, errOut, out)
	}
	if code, out, _ := runExplain(t, req, "explain", "-c", cfg, "--tenant", "t", "-"); code != exitOK || !strings.Contains(out, `"small@a"`) {
		t.Errorf("from stdin with -: exit %d\n%s", code, out)
	}

	tests := []struct {
		name    string
		stdin   string
		args    []string
		code    int
		wantErr string
	}{
		{"no subcommand", req, nil, exitUsage, "missing subcommand"},
		{"unknown subcommand", req, []string{"plan"}, exitUsage, `unknown subcommand "plan"`},
		{"no tenant", req, []string{"explain", "-c", cfg}, exitUsage, "--tenant is required"},
		{"unknown tenant", req, []string{"explain", "-c", cfg, "--tenant", "nobody"}, exitError, `unknown tenant "nobody"`},
		{"unknown key", req, []string{"explain", "-c", cfg, "--tenant", "t", "--key", "x"}, exitError, `tenant "t" has no key "x"`},
		{"bad header flag", req, []string{"explain", "-c", cfg, "--tenant", "t", "--header", "nocolon"}, exitUsage, "want 'Name: value'"},
		{"two files", req, []string{"explain", "-c", cfg, "--tenant", "t", "a", "b"}, exitUsage, `unexpected argument "b"`},
		{"missing file", req, []string{"explain", "-c", cfg, "--tenant", "t", filepath.Join(t.TempDir(), "none.json")}, exitError, "read the request"},
		{"bad request", `{"model":`, []string{"explain", "-c", cfg, "--tenant", "t"}, exitError, "error: 400 invalid_json"},
		{"bad header value", req, []string{"explain", "-c", cfg, "--tenant", "t", "--header", "X-Switchyard-Max-Cost-Usd: free"}, exitError, "error: 400 invalid_header"},
		{"bad config", req, []string{"explain", "-c", filepath.Join(t.TempDir(), "none.yaml"), "--tenant", "t"}, exitError, "no such file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, errOut := runExplain(t, tt.stdin, tt.args...)
			if code != tt.code || !strings.Contains(errOut, tt.wantErr) {
				t.Errorf("exit %d, stderr %q; want %d containing %q", code, errOut, tt.code, tt.wantErr)
			}
			if out != "" {
				t.Errorf("stdout %q, want nothing", out)
			}
			if strings.Contains(errOut, "free") {
				t.Errorf("the error echoes the header value: %s", errOut)
			}
		})
	}
}

// TestRouteExplainExampleConfig explains requests with the shipped example
// config and an empty environment: route explain needs none of the
// config's secrets. The requests are autocomplete traffic that also claims
// hard coding work through the client's hints, and in one case staging:
// the quality rule that matches applies on top of the autocomplete
// governance rule, whose 0.01 USD cost cap still holds.
func TestRouteExplainExampleConfig(t *testing.T) {
	cfg := filepath.Join("..", "..", "configs", "switchyard.example.yaml")
	body, err := json.Marshal(map[string]any{
		"model": "auto", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "Complete this function:\n" + strings.Repeat("def f(x):\n    return x + 1\n", 800)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{nil, {"--header", "X-Env: staging"}} {
		args := append([]string{"explain", "-c", cfg, "--tenant", "acme", "--key", "acme-backend",
			"--header", "X-Switchyard-Tags: feature=autocomplete",
			"--header", "X-Switchyard-Task: coding", "--header", "X-Switchyard-Difficulty: 0.7"}, extra...)
		var out, errOut bytes.Buffer
		if code := runRoute(append(args, "-"), bytes.NewReader(body), &out, &errOut, env{}.lookup); code != exitOK {
			t.Fatalf("%v: exit %d: %s", extra, code, errOut.String())
		}
		var exp struct {
			MatchedRule        string  `json:"matched_rule"`
			MatchedQualityRule string  `json:"matched_quality_rule"`
			Profile            string  `json:"profile"`
			QualityBar         float64 `json:"quality_bar"`
			Candidates         []struct {
				Deployment string `json:"deployment"`
				EstCostUSD string `json:"est_cost_usd"`
				Eligible   bool   `json:"eligible"`
				ExcludedBy string `json:"excluded_by"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(out.Bytes(), &exp); err != nil {
			t.Fatalf("output is not JSON: %v\n%s", err, out.String())
		}
		if exp.MatchedRule != "autocomplete" || exp.MatchedQualityRule != "code-needs-quality" || exp.Profile != "fast" || exp.QualityBar != 0.85 {
			t.Errorf("%v: rules %q/%q profile %q bar %v", extra, exp.MatchedRule, exp.MatchedQualityRule, exp.Profile, exp.QualityBar)
		}
		capped := 0
		for _, c := range exp.Candidates {
			if strings.Contains(c.ExcludedBy, "> max_cost_per_request 0.01 USD") {
				capped++
			}
			if c.Eligible {
				if cost, err := strconv.ParseFloat(c.EstCostUSD, 64); err != nil || cost > 0.01 {
					t.Errorf("%v: %s is eligible at %s USD, over the autocomplete cap", extra, c.Deployment, c.EstCostUSD)
				}
			}
		}
		if capped == 0 {
			t.Errorf("%v: no candidate was excluded by the 0.01 USD cap:\n%s", extra, out.String())
		}
		if strings.Contains(out.String()+errOut.String(), "placeholder") {
			t.Errorf("the output mentions a placeholder secret:\n%s%s", out.String(), errOut.String())
		}
	}
}
