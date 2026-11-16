package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"switchyard/internal/eval"
	"switchyard/test/eval/fakejudge"
)

// starterPrompts is the shipped starter prompt set.
var starterPrompts = filepath.Join("..", "..", "test", "eval", "prompts", "starter.jsonl")

// evalConfig serves small, large and judge from the fake at baseURL. Small
// has a low coding prior, so hard coding requests go to large; extra is
// appended, for an include.
func evalConfig(baseURL, extra string) string {
	return fmt.Sprintf(`version: 1
providers:
  - {id: fake, type: openai, base_url: %q, api_key: '${FAKE_KEY:-}'}
models:
  - id: small
    capabilities: [tools, forced_tools, json_mode, json_schema]
    quality: {general: 0.7, coding: 0.6}
    deployments: [{provider: fake, upstream_model: small-up, pricing: {input: "0.1", output: "0.4"}}]
  - id: large
    capabilities: [tools, forced_tools, json_mode, json_schema]
    quality: {general: 0.95, coding: 0.95}
    deployments: [{provider: fake, upstream_model: large-up, pricing: {input: "3", output: "15"}}]
  - id: judge
    deployments: [{provider: fake, upstream_model: judge-up, pricing: {input: "1", output: "2"}}]
evaluation:
  judge: {model: judge}
policies:
  - {id: default, baseline_model: large}
tenants:
  - {id: t, keys: [{id: k, hash: 'sha256:0000000000000000000000000000000000000000000000000000000000000000'}]}
%s`, baseURL, extra)
}

// equalAnswers makes every model answer alike, so the judge ties.
func equalAnswers(r fakejudge.Request) fakejudge.Reply {
	if r.Judge {
		return fakejudge.Reply{Content: "```json\n" + fakejudge.Verdict("tie") + "\n```"}
	}
	return fakejudge.Reply{Content: "the answer: secret-answer-text"}
}

// explainPlan returns the plan route explain gives for a hard coding
// request under cfg.
func explainPlan(t *testing.T, cfg string) []string {
	t.Helper()
	req := `{"model":"auto","messages":[{"role":"user","content":"Fix this function"}]}`
	code, out, errOut := runExplain(t, req, "explain", "-c", cfg, "--tenant", "t",
		"--header", "X-Switchyard-Task: coding", "--header", "X-Switchyard-Difficulty: high")
	if code != exitOK {
		t.Fatalf("route explain: exit %d: %s", code, errOut)
	}
	var exp struct {
		Plan []string `json:"plan"`
	}
	if err := json.Unmarshal([]byte(out), &exp); err != nil {
		t.Fatalf("route explain output: %v\n%s", err, out)
	}
	return exp.Plan
}

// TestEvalEndToEnd is the P8 acceptance case: running eval on the starter
// prompt set produces a report and an overrides file, and including the
// overrides changes routing.
func TestEvalEndToEnd(t *testing.T) {
	srv := fakejudge.New(equalAnswers)
	defer srv.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "switchyard.yaml")
	if err := os.WriteFile(cfg, []byte(evalConfig(srv.BaseURL(), "")), 0o600); err != nil {
		t.Fatal(err)
	}
	results := filepath.Join(dir, "results.jsonl")
	runArgs := []string{"eval", "run", "-c", cfg, "--prompts", starterPrompts, "--candidates", "small",
		"--reference", "large", "--max-cost-usd", "5", "--out", results, "--concurrency", "8"}

	// The dry run plans 105 pairs and calls nothing.
	code, out, errOut := runCLI(t, env{}, append(runArgs, "--dry-run")...)
	if code != exitOK || !strings.Contains(out, "dry run: 105 pairs, 0 already done") || !strings.Contains(out, "105 reference answers, 105 candidate answers, 105 judgments") ||
		!strings.Contains(out, "the $5 cap covers the expected cost but not every judge retry") {
		t.Fatalf("dry run: exit %d\n%s\n%s", code, out, errOut)
	}
	if j, a := srv.Counts(); j+a != 0 {
		t.Fatalf("the dry run made %d calls", j+a)
	}

	code, out, errOut = runCLI(t, env{}, runArgs...)
	if code != exitOK || !strings.Contains(out, "105 pairs: 105 judged (105 acceptable), 0 skipped") {
		t.Fatalf("eval run: exit %d\n%s\n%s", code, out, errOut)
	}
	if j, a := srv.Counts(); j != 210 || a != 210 {
		t.Errorf("%d judge and %d answer calls, want 210 and 210", j, a)
	}
	data, err := os.ReadFile(results)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-answer-text", "capital of Australia", "Pride and Prejudice", "scripted"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("results contain content %q", secret)
		}
	}

	// Rerunning resumes: every pair is done, nothing is called.
	code, out, _ = runCLI(t, env{}, runArgs...)
	if j, a := srv.Counts(); code != exitOK || j != 210 || a != 210 || !strings.Contains(out, "0 judged (0 acceptable), 105 skipped") ||
		!strings.Contains(out, "105 pairs already judged; earlier runs recorded $") || !strings.Contains(out, "covers this run's new spend only") {
		t.Errorf("resumed run: exit %d, %d calls\n%s", code, j+a, out)
	}

	// The report and the fit.
	overrides := filepath.Join(dir, "quality-overrides.yaml")
	code, out, errOut = runCLI(t, env{}, "eval", "fit", "--results", results, "-c", cfg, "--out", overrides, "--date", "2026-11-12")
	if code != exitOK {
		t.Fatalf("eval fit: exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"MODEL", "N_INFORMATIVE", "small", "coding", "1.000", "0.600", "+0.400", "insufficient", "wrote "} {
		if !strings.Contains(out, want) {
			t.Errorf("fit output lacks %q:\n%s", want, out)
		}
	}
	text, err := os.ReadFile(overrides)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("eval fit:\n%s\n%s", out, text)
	if !strings.Contains(string(text), "category: coding") || strings.Contains(string(text), "category: tool_use") ||
		!strings.Contains(string(text), "prompts=starter judge=judge@fake reference=large@fake") {
		t.Errorf("overrides (tool_use has only 9 samples and must not be written):\n%s", text)
	}
	code, report, _ := runCLI(t, env{}, "eval", "report", "--results", results, "-c", cfg)
	if header, _, _ := strings.Cut(report, "\n"); code != exitOK || header == "" || !strings.HasPrefix(out, header) {
		t.Errorf("eval report: exit %d\n%s", code, report)
	}

	// Routing before and after including the overrides.
	if plan := explainPlan(t, cfg); len(plan) == 0 || plan[0] != "large@fake" {
		t.Errorf("before: plan %v, want large@fake first", plan)
	}
	withOverrides := filepath.Join(dir, "with-overrides.yaml")
	if err := os.WriteFile(withOverrides, []byte(evalConfig(srv.BaseURL(), "include: [quality-overrides.yaml]\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runCLI(t, env{}, "config", "validate", "-c", withOverrides); code != exitOK {
		t.Fatalf("config with overrides: exit %d\n%s\n%s", code, out, errOut)
	}
	if plan := explainPlan(t, withOverrides); len(plan) == 0 || plan[0] != "small@fake" {
		t.Errorf("after: plan %v, want small@fake first", plan)
	}
}

// TestEvalRunCostCap: the cap is per invocation, and the stop message,
// the resume and the summary say so with this run's spend and the file's.
func TestEvalRunCostCap(t *testing.T) {
	srv := fakejudge.New(equalAnswers)
	defer srv.Close()
	cfg := writeConfig(t, evalConfig(srv.BaseURL(), ""))
	results := filepath.Join(t.TempDir(), "results.jsonl")
	args := []string{"eval", "run", "-c", cfg, "--prompts", starterPrompts, "--candidates", "small",
		"--reference", "large", "--max-cost-usd", "0.15", "--out", results}
	code, out, errOut := runCLI(t, env{}, args...)
	if code != exitIncomplete || !strings.Contains(errOut, "past --max-cost-usd 0.15") ||
		!strings.Contains(errOut, "The cap is per invocation") || strings.Contains(errOut, "higher cap") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	res, _, err := eval.LoadResults(results)
	if err != nil || len(res) == 0 || len(res) >= 105 {
		t.Fatalf("%d results, %v; want a partial run", len(res), err)
	}
	first := eval.Spent(res)
	if !strings.Contains(out, "this run spent $"+first.String()+" of its $0.15 cap") || !strings.Contains(out, "records $"+first.String()+" in all") {
		t.Errorf("summary does not give this run's and the file's spend ($%s):\n%s", first, out)
	}

	// The same cap again: it covers new spend only, and the file total
	// grows past it.
	code, out, errOut = runCLI(t, env{}, args...)
	res2, _, err := eval.LoadResults(results)
	if err != nil {
		t.Fatal(err)
	}
	total := eval.Spent(res2)
	if code != exitIncomplete || !strings.Contains(out, "earlier runs recorded $"+first.String()) ||
		!strings.Contains(errOut, "records $"+total.String()+" in all") || total.Cmp(first) <= 0 {
		t.Errorf("second run: exit %d, file total %s after %s\n%s\n%s", code, total, first, out, errOut)
	}
}

// TestEvalRunTornLine: a results file whose last line was torn by an
// interrupted write is resumed: the torn line is reported without its
// content, cut off before appending, and every line stays valid.
func TestEvalRunTornLine(t *testing.T) {
	srv := fakejudge.New(equalAnswers)
	defer srv.Close()
	dir := t.TempDir()
	cfg := writeConfig(t, evalConfig(srv.BaseURL(), ""))
	prompts := filepath.Join(dir, "prompts.jsonl")
	if err := os.WriteFile(prompts, []byte(
		`{"id":"p1","request":{"model":"x","messages":[{"role":"user","content":"What is 2+2?"}]}}`+"\n"+
			`{"id":"p2","request":{"model":"x","messages":[{"role":"user","content":"Name a color."}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	results := filepath.Join(dir, "results.jsonl")
	args := []string{"eval", "run", "-c", cfg, "--prompts", prompts, "--candidates", "small",
		"--reference", "large", "--max-cost-usd", "1", "--out", results}
	if code, out, errOut := runCLI(t, env{}, args...); code != exitOK {
		t.Fatalf("first run: exit %d\n%s\n%s", code, out, errOut)
	}
	data, err := os.ReadFile(results)
	if err != nil {
		t.Fatal(err)
	}
	// Keep p1's line and tear p2's.
	lines := strings.SplitAfter(string(data), "\n")
	keep, torn := lines[0], lines[1][:len(lines[1])/2]
	if strings.Contains(lines[1], `"id":"p1"`) {
		keep, torn = lines[1], lines[0][:len(lines[0])/2]
	}
	const marker = "TORN-CONTENT"
	torn = strings.Replace(torn, `"id":"`, `"id":"`+marker, 1)
	if err := os.WriteFile(results, []byte(keep+torn), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, sub := range [][]string{{"eval", "report", "--results", results}} {
		code, _, errOut := runCLI(t, env{}, sub...)
		if code != exitOK || !strings.Contains(errOut, "ends with an incomplete line") || strings.Contains(errOut, marker) {
			t.Errorf("%v: exit %d\n%s", sub, code, errOut)
		}
	}
	code, out, errOut := runCLI(t, env{}, args...)
	if code != exitOK || !strings.Contains(errOut, "ends with an incomplete line") || strings.Contains(errOut, marker) ||
		!strings.Contains(out, "1 judged") {
		t.Fatalf("resumed run: exit %d\n%s\n%s", code, out, errOut)
	}
	res, torn2, err := eval.LoadResults(results)
	if err != nil || torn2 != 0 || len(res) != 2 || len(eval.Completed(res)) != 2 {
		t.Fatalf("after the resume: %d results, torn %d, %v; want 2 valid lines", len(res), torn2, err)
	}
	if data, _ := os.ReadFile(results); bytes.Contains(data, []byte(marker)) {
		t.Error("the torn line was not cut off")
	}
	// A later run, fit and report all still read the file.
	if code, out, errOut := runCLI(t, env{}, "eval", "report", "--results", results); code != exitOK || strings.Contains(errOut, "incomplete") {
		t.Errorf("report after the resume: exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestEvalUsageErrors(t *testing.T) {
	cfg := writeConfig(t, evalConfig("http://127.0.0.1:1/v1", ""))
	base := []string{"eval", "run", "-c", cfg, "--prompts", starterPrompts, "--candidates", "small", "--reference", "large"}
	tests := []struct {
		name    string
		args    []string
		code    int
		wantErr string
	}{
		{"no subcommand", []string{"eval"}, exitUsage, "missing subcommand"},
		{"unknown subcommand", []string{"eval", "score"}, exitUsage, `unknown subcommand "score"`},
		{"no cap", base, exitUsage, "--max-cost-usd is required"},
		{"bad cap", append(base, "--max-cost-usd", "lots"), exitUsage, "--max-cost-usd must be a positive decimal"},
		{"zero cap", append(base, "--max-cost-usd", "0"), exitUsage, "--max-cost-usd must be a positive decimal"},
		{"no prompts", []string{"eval", "run", "-c", cfg, "--candidates", "small", "--reference", "large", "--max-cost-usd", "1"}, exitUsage, "--prompts is required"},
		{"unknown candidate", []string{"eval", "run", "-c", cfg, "--prompts", starterPrompts, "--candidates", "tiny", "--reference", "large", "--max-cost-usd", "1"}, exitError, `unknown model`},
		{"bad concurrency", append(base, "--max-cost-usd", "1", "--concurrency", "0"), exitUsage, "--concurrency must be between"},
		{"fit without out", []string{"eval", "fit", "--results", "r.jsonl"}, exitUsage, "--out is required"},
		{"fit bad date", []string{"eval", "fit", "--results", "r.jsonl", "--out", "o.yaml", "--date", "tomorrow"}, exitUsage, "--date must be a date"},
		{"report missing file", []string{"eval", "report", "--results", filepath.Join(t.TempDir(), "none.jsonl")}, exitError, "open the results"},
		{"fit-weights without labeled", []string{"eval", "fit-weights", "--out", "w.yaml"}, exitUsage, "--labeled is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, errOut := runCLI(t, env{}, tt.args...)
			if code != tt.code || !strings.Contains(errOut, tt.wantErr) {
				t.Errorf("exit %d, stderr %q; want %d containing %q", code, errOut, tt.code, tt.wantErr)
			}
		})
	}
}

func TestEvalFitWeights(t *testing.T) {
	out := filepath.Join(t.TempDir(), "weights.yaml")
	code, stdout, errOut := runCLI(t, env{}, "eval", "fit-weights", "--labeled", filepath.Join("..", "..", "internal", "quality", "testdata", "sanity.json"), "--out", out)
	if code != exitOK || !strings.Contains(stdout, "in-band accuracy on 105 labeled prompts") {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, errOut)
	}
	text, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// The section is valid config.
	cfg := writeConfig(t, evalConfig("http://127.0.0.1:1/v1", string(text)))
	if code, o, e := runCLI(t, env{}, "config", "validate", "-c", cfg); code != exitOK {
		t.Errorf("config with fitted weights: exit %d\n%s\n%s\n%s", code, o, e, text)
	}
	t.Logf("%s", stdout)
}
