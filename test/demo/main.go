// Command demo runs the mixed-workload routing demo (PLAN P5, `make
// demo-routing`).
//
// It starts two fake upstreams, an OpenAI-wire one and a Messages API one,
// and the gateway in front of them with a catalog of three tiers modeled
// on DESIGN §10.5: demo-s and demo-m on the OpenAI-wire fake, demo-l on
// the Anthropic one, at realistic prices. The tenant's policy routes
// auto:balanced with demo-l as the savings baseline, on the memory ledger.
// It sends -per-band easy, medium and hard prompts from the quality sanity
// set (internal/quality/testdata/sanity.json, labeled before any tuning)
// as auto:balanced, without hints, so the heuristic assessor decides.
//
// It prints how each band was routed and the usage report's cost, baseline
// and savings, and exits non-zero unless most easy prompts went to demo-s,
// most hard ones to demo-l, and the routing savings are above 0.
//
//	go run ./test/demo [-per-band 30] [-sanity FILE] [-bin PATH]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeanthropic"
	"switchyard/test/fakeupstream"
)

// Exit codes.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// providerKey is the fakes' API key.
const providerKey = "sk-demo-provider"

// The tiers, cheapest first.
var tiers = []string{"demo-s", "demo-m", "demo-l"}

// bands are the sanity set's difficulty bands.
var bands = []string{"easy", "medium", "hard"}

// demoCatalog is the config's providers, models and policy. Prices are
// USD per million tokens, in the range of 2026 small, mid-size and
// frontier models; the quality priors follow DESIGN §10.5 for coding and
// are set alike for the other categories.
const demoCatalog = `providers:
  - {id: oa, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}"}
  - {id: an, type: anthropic, base_url: %q, api_key: "${PROVIDER_KEY}"}
models:
  - id: demo-s
    capabilities: [streaming, stream_usage, tools, forced_tools, parallel_tools, json_mode, json_schema, sampling]
    quality: {general: 0.55, coding: 0.60, reasoning: 0.45, extraction: 0.70, creative: 0.55, tool_use: 0.50}
    deployments:
      - {provider: oa, upstream_model: demo-s-up, pricing: {input: "0.10", output: "0.40"}, latency_hint: {ttft_ms: 300}}
  - id: demo-m
    capabilities: [streaming, stream_usage, tools, forced_tools, parallel_tools, json_mode, json_schema, sampling]
    quality: {general: 0.72, coding: 0.75, reasoning: 0.68, extraction: 0.85, creative: 0.72, tool_use: 0.72}
    deployments:
      - {provider: oa, upstream_model: demo-m-up, pricing: {input: "0.50", output: "2.00"}, latency_hint: {ttft_ms: 450}}
  - id: demo-l
    capabilities: [streaming, stream_usage, tools, forced_tools, json_mode, json_schema]
    quality: {general: 0.95, coding: 0.95, reasoning: 0.95, extraction: 0.97, creative: 0.93, tool_use: 0.94}
    deployments:
      - {provider: an, upstream_model: demo-l-up, pricing: {input: "3", output: "15"}, latency_hint: {ttft_ms: 700}}
routing:
  drop_unsupported_params: true
policies:
  - {id: default, profile: balanced, baseline_model: demo-l}
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// options are the command-line flags.
type options struct {
	perBand int
	sanity  string
	bin     string
}

func run(args []string, stdout, stderr io.Writer) int {
	var o options
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.IntVar(&o.perBand, "per-band", 30, "prompts per difficulty band")
	fs.StringVar(&o.sanity, "sanity", "internal/quality/testdata/sanity.json", "the labeled sanity set")
	fs.StringVar(&o.bin, "bin", "", "prebuilt switchyard binary (default: go build switchyard/cmd/switchyard)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 || o.perBand < 1 {
		fmt.Fprintln(stderr, "usage: demo [-per-band N] [-sanity FILE] [-bin PATH]")
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := demo(ctx, o, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "demo: %v\n", err)
		return exitFail
	}
	return exitOK
}

// prompt is one labeled request of the sanity set.
type prompt struct {
	ID      string          `json:"id"`
	Band    string          `json:"band"`
	Request json.RawMessage `json:"request"`
}

// loadPrompts returns up to perBand prompts of each band, in file order.
func loadPrompts(path string, perBand int) (map[string][]prompt, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the operator names the file
	if err != nil {
		return nil, fmt.Errorf("read the sanity set: %w", err)
	}
	var all []prompt
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("parse the sanity set: %w", err)
	}
	out := map[string][]prompt{}
	for _, p := range all {
		if len(out[p.Band]) < perBand {
			out[p.Band] = append(out[p.Band], p)
		}
	}
	for _, b := range bands {
		if len(out[b]) == 0 {
			return nil, fmt.Errorf("the sanity set has no %s prompts", b)
		}
	}
	return out, nil
}

func demo(ctx context.Context, o options, stdout, stderr io.Writer) error {
	prompts, err := loadPrompts(o.sanity, o.perBand)
	if err != nil {
		return err
	}
	oa := fakeupstream.New(fakeupstream.Options{APIKey: providerKey, DisableRecording: true})
	defer oa.Close()
	an := fakeanthropic.New(fakeanthropic.Options{APIKey: providerKey})
	defer an.Close()

	dir, err := os.MkdirTemp("", "switchyard-demo-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
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
	cfg := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 5s}
logging: {level: info, format: json}
`+demoCatalog+`tenants:
  - {id: demo, keys: [{id: demo-key, hash: %q}]}
`, oa.BaseURL(), an.BaseURL(), hash)
	cfgPath := filepath.Join(dir, "switchyard.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		return err
	}
	g, err := harness.Start(ctx, bin, cfgPath, harness.StartOptions{Env: []string{"PROVIDER_KEY=" + providerKey}, DiscardLogs: true})
	if err != nil {
		return err
	}
	defer func() { _ = g.Stop(10 * time.Second) }()

	client := &http.Client{Timeout: 30 * time.Second}
	routed := map[string]map[string]int{}
	total := 0
	for _, band := range bands {
		routed[band] = map[string]int{}
		for _, p := range prompts[band] {
			model, err := send(ctx, client, g.PublicURL, key, p.Request)
			if err != nil {
				return fmt.Errorf("%s: %w", p.ID, err)
			}
			routed[band][model]++
			total++
		}
	}

	fmt.Fprintf(stdout, "Mixed workload: %d prompts from the quality sanity set, sent as auto:balanced (baseline demo-l)\n\n", total)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "band\tprompts\tdemo-s\tdemo-m\tdemo-l\t")
	for _, band := range bands {
		row := []string{band, fmt.Sprint(len(prompts[band]))}
		for _, tier := range tiers {
			row = append(row, fmt.Sprint(routed[band][tier]))
		}
		fmt.Fprintln(tw, strings.Join(row, "\t")+"\t")
	}
	_ = tw.Flush()

	rep, err := usageReport(ctx, client, g.PublicURL, key, total)
	if err != nil {
		return err
	}
	t := rep.Totals
	fmt.Fprintf(stdout, "\nUsage API (GET /switchyard/v1/usage), %d requests:\n", t.Requests)
	fmt.Fprintf(stdout, "  cost             %s USD\n", t.CostUSD)
	fmt.Fprintf(stdout, "  baseline         %s USD (every request on demo-l, the same tokens)\n", t.BaselineCostUSD)
	fmt.Fprintf(stdout, "  routing savings  %s USD (%s of the baseline)\n", t.SavingsUSD.Routing, percent(t.SavingsUSD.Routing, t.BaselineCostUSD))
	fmt.Fprintf(stdout, "  net savings      %s USD\n", t.SavingsUSD.Net)
	for _, gr := range rep.Groups {
		fmt.Fprintf(stdout, "  %-15s  %3v requests, cost %s USD, baseline %s USD\n", fmt.Sprint(gr["deployment"]), gr["requests"], gr["cost_usd"], gr["baseline_cost_usd"])
	}

	var failures []string
	easyS, hardL := share(routed["easy"]["demo-s"], len(prompts["easy"])), share(routed["hard"]["demo-l"], len(prompts["hard"]))
	if easyS <= 0.5 {
		failures = append(failures, fmt.Sprintf("only %.0f%% of easy prompts went to demo-s", 100*easyS))
	}
	if hardL <= 0.5 {
		failures = append(failures, fmt.Sprintf("only %.0f%% of hard prompts went to demo-l", 100*hardL))
	}
	if !positive(t.SavingsUSD.Routing) {
		failures = append(failures, "routing savings are not above 0: "+t.SavingsUSD.Routing)
	}
	if t.Requests != total {
		failures = append(failures, fmt.Sprintf("the usage API counts %d requests, %d were sent", t.Requests, total))
	}
	fmt.Fprintf(stdout, "\nChecks: %.0f%% of easy prompts on demo-s, %.0f%% of hard prompts on demo-l, savings above 0: ", 100*easyS, 100*hardL)
	if len(failures) > 0 {
		fmt.Fprintln(stdout, "FAIL")
		return errors.New(strings.Join(failures, "; "))
	}
	fmt.Fprintln(stdout, "PASS")
	return nil
}

// send posts one request as auto:balanced and returns the model that
// served it.
func send(ctx context.Context, client *http.Client, base, key string, request json.RawMessage) (string, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(request, &body); err != nil {
		return "", err
	}
	body["model"] = json.RawMessage(`"auto:balanced"`)
	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	return resp.Header.Get("X-Switchyard-Model"), nil
}

// report is the part of the usage report the demo prints.
type report struct {
	Totals struct {
		Requests        int    `json:"requests"`
		CostUSD         string `json:"cost_usd"`
		BaselineCostUSD string `json:"baseline_cost_usd"`
		SavingsUSD      struct {
			Routing string `json:"routing"`
			Net     string `json:"net"`
		} `json:"savings_usd"`
	} `json:"totals"`
	Groups []map[string]any `json:"groups"`
}

// usageReport reads the tenant's usage report, grouped by deployment,
// once it counts want requests.
func usageReport(ctx context.Context, client *http.Client, base, key string, want int) (*report, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/switchyard/v1/usage?group_by=deployment", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	// The memory ledger records each event as its request finishes; the
	// last one may land just after its response.
	for range 50 {
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("usage report: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("usage report: status %d: %s", resp.StatusCode, b)
		}
		var r report
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		if r.Totals.Requests >= want {
			return &r, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, fmt.Errorf("usage report: fewer than %d events recorded", want)
}

// share is n/of.
func share(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return float64(n) / float64(of)
}

// positive reports whether the decimal s is above 0.
func positive(s string) bool {
	r, ok := new(big.Rat).SetString(s)
	return ok && r.Sign() > 0
}

// percent formats part/whole, two decimals, as a percentage.
func percent(part, whole string) string {
	p, ok1 := new(big.Rat).SetString(part)
	w, ok2 := new(big.Rat).SetString(whole)
	if !ok1 || !ok2 || w.Sign() == 0 {
		return "n/a"
	}
	f, _ := new(big.Rat).Quo(p, w).Float64()
	return fmt.Sprintf("%.1f%%", 100*f)
}
