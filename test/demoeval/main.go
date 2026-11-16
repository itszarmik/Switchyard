// Command demoeval runs the quality evaluation demo (PLAN P8, `make
// demo-eval`): `switchyard eval` measures the tiers on a prompt set, its
// overrides file is included in the config, and routing changes.
//
// One scripted fake upstream (test/eval/fakejudge) plays every model: the
// candidates eval-s and eval-m, the reference eval-l and the judge
// eval-judge. Each answers "Answer by <model>."; the judge prefers the
// reference over eval-s on coding prompts (it recognizes the coding rubric)
// and calls every other pair a tie. So eval-s is measured unacceptable on
// coding and acceptable elsewhere, and eval-m acceptable everywhere.
//
// The catalog's hand-set priors overrate eval-s on coding (0.85), so
// before the evaluation every coding prompt routes to it under
// auto:balanced. The demo then
//
//  1. prints the `eval run --dry-run` plan, and runs `eval run` on the
//     starter prompt set (test/eval/prompts/starter.jsonl) under a cost cap;
//  2. runs `eval fit`, which prints the report and writes the overrides;
//  3. starts the gateway without, then with, the overrides included, and
//     sends the starter set's coding prompts to both as auto:balanced, with
//     X-Switchyard-Task: coding (the category the evaluation measured them
//     in), and prints how each gateway routed them.
//
// It exits non-zero unless eval-s's coding prior went down, the overrides
// file has an eval-s coding entry, and the share of coding prompts served
// by eval-s dropped while eval-m's rose.
//
//	go run ./test/demoeval [-prompts FILE] [-bin PATH] [-keep DIR]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"go.yaml.in/yaml/v3"

	"switchyard/test/conformance/harness"
	"switchyard/test/eval/fakejudge"
)

// Exit codes.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// providerKey is the fake's API key, as the config references it.
const providerKey = "sk-demo-eval-provider"

// The tiers, cheapest first, and the judge.
var tiers = []string{"eval-s", "eval-m", "eval-l"}

const judgeModel = "eval-judge"

// codingRubric is a line of the judge's coding rubric (internal/judge),
// by which the fake judge recognizes a coding prompt.
const codingRubric = "The code is correct"

// catalog is the config's provider, models and policy. The priors are
// hand-set "starting estimates" that overrate eval-s on coding.
const catalog = `providers:
  - {id: fake, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}"}
models:
  - id: eval-s
    capabilities: [sampling, json_mode, json_schema, tools, forced_tools, parallel_tools]
    quality: {general: 0.60, coding: 0.85, reasoning: 0.50, extraction: 0.70, creative: 0.60, tool_use: 0.55}
    deployments:
      - {provider: fake, upstream_model: eval-s, pricing: {input: "0.10", output: "0.40"}}
  - id: eval-m
    capabilities: [sampling, json_mode, json_schema, tools, forced_tools, parallel_tools]
    quality: {general: 0.75, coding: 0.75, reasoning: 0.70, extraction: 0.85, creative: 0.72, tool_use: 0.72}
    deployments:
      - {provider: fake, upstream_model: eval-m, pricing: {input: "0.50", output: "2.00"}}
  - id: eval-l
    capabilities: [sampling, json_mode, json_schema, tools, forced_tools, parallel_tools]
    quality: {general: 0.95, coding: 0.95, reasoning: 0.95, extraction: 0.97, creative: 0.93, tool_use: 0.94}
    deployments:
      - {provider: fake, upstream_model: eval-l, pricing: {input: "3", output: "15"}}
  - id: eval-judge
    capabilities: [sampling, json_mode, json_schema, tools, forced_tools, parallel_tools]
    quality: {general: 0.97}
    deployments:
      - {provider: fake, upstream_model: eval-judge, pricing: {input: "5", output: "25"}}
policies:
  - {id: default, profile: balanced, baseline_model: eval-l}
evaluation:
  judge: {model: eval-judge}
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// options are the command-line flags.
type options struct {
	prompts, bin, keep string
}

func run(args []string, stdout, stderr io.Writer) int {
	var o options
	fs := flag.NewFlagSet("demoeval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.prompts, "prompts", "test/eval/prompts/starter.jsonl", "the prompt set")
	fs.StringVar(&o.bin, "bin", "", "prebuilt switchyard binary (default: go build switchyard/cmd/switchyard)")
	fs.StringVar(&o.keep, "keep", "", "write the config, results and overrides to this directory and keep them")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: demoeval [-prompts FILE] [-bin PATH] [-keep DIR]")
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := demo(ctx, o, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "demoeval: %v\n", err)
		return exitFail
	}
	return exitOK
}

// script plays every model: the answers name their model, and the judge
// prefers the reference over eval-s on coding prompts and ties otherwise.
func script(r fakejudge.Request) fakejudge.Reply {
	if !r.Judge {
		return fakejudge.Reply{Content: "Answer by " + r.Model + "."}
	}
	coding := strings.Contains(r.System, codingRubric)
	return fakejudge.Reply{Content: fakejudge.Prefer(func(answer string) int {
		if coding && strings.Contains(answer, "eval-s") {
			return 0
		}
		return 1
	}, r)}
}

// prompt is one line of the prompt set.
type prompt struct {
	ID       string          `json:"id"`
	Category string          `json:"category"`
	Request  json.RawMessage `json:"request"`
}

func demo(ctx context.Context, o options, stdout, stderr io.Writer) error {
	coding, total, err := codingPrompts(o.prompts)
	if err != nil {
		return err
	}
	fake := fakejudge.New(script)
	defer fake.Close()

	dir := o.keep
	if dir == "" {
		if dir, err = os.MkdirTemp("", "switchyard-demoeval-"); err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
	} else if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	bin := o.bin
	if bin == "" {
		fmt.Fprintln(stderr, "building the gateway...")
		bctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		bin, err = harness.Build(bctx, dir, false)
		cancel()
		if err != nil {
			return err
		}
	}
	key, hash, err := harness.GenerateKey()
	if err != nil {
		return err
	}
	base := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 5s}
logging: {level: info, format: json}
`+catalog+`tenants:
  - {id: demo, keys: [{id: demo-key, hash: %q}]}
`, fake.BaseURL(), hash)
	plain := filepath.Join(dir, "switchyard.yaml")
	if err := os.WriteFile(plain, []byte(base), 0o600); err != nil {
		return err
	}
	results := filepath.Join(dir, "results.jsonl")
	overrides := filepath.Join(dir, "quality-overrides.yaml")
	env := []string{"PROVIDER_KEY=" + providerKey}

	fmt.Fprintf(stdout, "== Quality evaluation demo: %d prompts (%d coding), candidates eval-s and eval-m, reference eval-l, judge %s\n\n",
		total, len(coding), judgeModel)
	runArgs := []string{"eval", "run", "-c", plain, "--prompts", o.prompts, "--candidates", "eval-s,eval-m",
		"--reference", "eval-l", "--judge", judgeModel, "--max-cost-usd", "2", "--out", results}
	fmt.Fprintln(stdout, "$ switchyard eval run ... --max-cost-usd 2 --dry-run")
	if err := cli(ctx, bin, env, stdout, append(runArgs, "--dry-run")...); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "\n$ switchyard eval run ... --max-cost-usd 2")
	if err := cli(ctx, bin, env, stdout, runArgs...); err != nil {
		return err
	}
	judged, answers := fake.Counts()
	fmt.Fprintf(stdout, "(the fake upstream served %d answers and %d judge calls; no real provider was called)\n", answers, judged)
	fmt.Fprintln(stdout, "\n$ switchyard eval fit --results results.jsonl -c switchyard.yaml --out quality-overrides.yaml")
	if err := cli(ctx, bin, env, stdout, "eval", "fit", "--results", results, "-c", plain, "--out", overrides); err != nil {
		return err
	}
	prior, measured, err := codingOverride(overrides, "eval-s")
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\noverrides file: eval-s coding prior %.2f -> measured %.3f\n", prior, measured)

	withOverrides := filepath.Join(dir, "switchyard-with-overrides.yaml")
	if err := os.WriteFile(withOverrides, []byte(base+"include: [quality-overrides.yaml]\n"), 0o600); err != nil {
		return err
	}
	before, err := route(ctx, bin, plain, env, key, coding)
	if err != nil {
		return fmt.Errorf("gateway without overrides: %w", err)
	}
	after, err := route(ctx, bin, withOverrides, env, key, coding)
	if err != nil {
		return fmt.Errorf("gateway with overrides: %w", err)
	}

	fmt.Fprintf(stdout, "\n== Routing of the %d coding prompts as auto:balanced (X-Switchyard-Task: coding)\n\n", len(coding))
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CONFIG\t"+strings.Join(tiers, "\t"))
	for _, row := range []struct {
		name string
		got  map[string]int
	}{{"catalog priors", before}, {"with quality-overrides.yaml", after}} {
		cells := []string{row.name}
		for _, tier := range tiers {
			cells = append(cells, fmt.Sprintf("%d (%.0f%%)", row.got[tier], share(row.got[tier], len(coding))*100))
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	sBefore, sAfter := share(before["eval-s"], len(coding)), share(after["eval-s"], len(coding))
	mBefore, mAfter := share(before["eval-m"], len(coding)), share(after["eval-m"], len(coding))
	fmt.Fprintf(stdout, "\neval-s share of coding: %.0f%% -> %.0f%%; eval-m share: %.0f%% -> %.0f%%\n", sBefore*100, sAfter*100, mBefore*100, mAfter*100)
	var problems []string
	if !(measured < prior) {
		problems = append(problems, fmt.Sprintf("the measured eval-s coding prior %.3f is not below the hand-set %.2f", measured, prior))
	}
	if !(sAfter < sBefore) {
		problems = append(problems, "the overrides did not lower eval-s's share of coding prompts")
	}
	if !(mAfter > mBefore) {
		problems = append(problems, "the overrides did not move coding prompts to eval-m")
	}
	if len(problems) > 0 {
		fmt.Fprintln(stdout, "FAIL: "+strings.Join(problems, "; "))
		return errors.New("routing did not change as expected")
	}
	fmt.Fprintln(stdout, "OK: the measured priors moved coding prompts off eval-s, to eval-m")
	return nil
}

// codingPrompts returns the requests of the prompt set's coding prompts,
// and the set's size.
func codingPrompts(path string) ([]json.RawMessage, int, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the operator names the file
	if err != nil {
		return nil, 0, fmt.Errorf("read the prompt set: %w", err)
	}
	var out []json.RawMessage
	n := 0
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var p prompt
		if err := json.Unmarshal(line, &p); err != nil {
			return nil, 0, fmt.Errorf("parse the prompt set: %w", err)
		}
		n++
		if p.Category == "coding" {
			out = append(out, p.Request)
		}
	}
	if len(out) == 0 {
		return nil, 0, errors.New("the prompt set has no coding prompts")
	}
	return out, n, nil
}

// cli runs the switchyard binary with args, copying its output to stdout.
func cli(ctx context.Context, bin string, env []string, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- the binary this demo built
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	_, _ = stdout.Write(out.Bytes())
	if err != nil {
		return fmt.Errorf("switchyard %s: %w", strings.Join(args[:2], " "), err)
	}
	return nil
}

// codingOverride returns the catalog prior of model's coding category and
// the value the overrides file measured.
func codingOverride(path, model string) (prior, measured float64, err error) {
	data, err := os.ReadFile(path) // #nosec G304 -- written by this demo
	if err != nil {
		return 0, 0, fmt.Errorf("read the overrides: %w", err)
	}
	var f struct {
		QualityOverrides []struct {
			Model, Category string
			Value           float64
		} `yaml:"quality_overrides"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		return 0, 0, fmt.Errorf("parse the overrides: %w", err)
	}
	for _, o := range f.QualityOverrides {
		if o.Model == model && o.Category == "coding" {
			return 0.85, o.Value, nil
		}
	}
	return 0, 0, fmt.Errorf("the overrides file has no %s coding entry", model)
}

// route starts a gateway on cfg, sends the coding prompts and counts the
// models that served them.
func route(ctx context.Context, bin, cfg string, env []string, key string, prompts []json.RawMessage) (map[string]int, error) {
	g, err := harness.Start(ctx, bin, cfg, harness.StartOptions{Env: env, DiscardLogs: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = g.Stop(10 * time.Second) }()
	client := &http.Client{Timeout: 30 * time.Second}
	got := map[string]int{}
	for _, raw := range prompts {
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		body["model"] = "auto:balanced"
		b, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.PublicURL+"/v1/chat/completions", bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Switchyard-Task", "coding")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		msg, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, msg)
		}
		got[resp.Header.Get("X-Switchyard-Model")]++
	}
	return got, nil
}

// share returns n / of, or 0.
func share(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return float64(n) / float64(of)
}
