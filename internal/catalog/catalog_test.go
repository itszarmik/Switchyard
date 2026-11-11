package catalog

import (
	"errors"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/config"
	"switchyard/internal/schema"
)

// testCatalogYAML is the catalog most tests use. Model "llama" is offered
// by two providers at different prices; "gpt" has three deployments, one of
// them without prices but with json_schema; "claude" has no n or seed; "embed" is an embedding
// model.
const testCatalogYAML = `version: 1
providers:
  - {id: openai, type: openai, base_url: 'https://api.openai.example/v1'}
  - {id: anthropic, type: anthropic, base_url: 'https://api.anthropic.example', tags: [zdr]}
  - {id: together, type: openai, base_url: 'https://together.example/v1'}
  - {id: gpu, type: openai, base_url: 'http://vllm.internal:8000/v1', tags: [self_hosted, zdr]}
models:
  - id: llama
    context_window: 131072
    max_output_tokens: 8192
    capabilities: [streaming, tools, json_mode, n, seed]
    token_ratio: 1.05
    deployments:
      - provider: together
        upstream_model: meta-llama/Llama-3.3-70B-Instruct-Turbo
        pricing: {input: 0.88, output: 0.88}
      - provider: gpu
        upstream_model: meta-llama/Llama-3.3-70B-Instruct
        pricing: {input: 0.10, output: 0.10}
        capabilities: [streaming, tools, json_mode]
        tags: [zdr, quantized]
  - id: gpt
    capabilities: [streaming, tools, vision, n, seed, logprobs]
    deployments:
      - {provider: openai, upstream_model: gpt-x, capabilities: [streaming, tools, vision, n, seed, logprobs, json_schema]}
      - {provider: together, upstream_model: gpt-x-oss, pricing: {input: 2, output: 2}}
      - {provider: gpu, upstream_model: gpt-x-local, pricing: {input: 1, output: 3}}
  - id: claude
    context_window: 200000
    max_output_tokens: 64000
    capabilities: [streaming, tools, parallel_tools, vision, json_schema, prompt_caching]
    deployments:
      - provider: anthropic
        upstream_model: claude-x
        pricing: {input: 3, output: 15, cached_input: 0.30, cache_write: 3.75}
  - id: embed
    type: embedding
    dimensions: 1536
    deployments:
      - {provider: openai, upstream_model: text-embedding-x, pricing: {input: 0.02}}
      - {provider: together, upstream_model: text-embedding-y}
aliases:
  code: claude
  cheap-llama: llama@gpu
  autopilot-alias: gpt
`

// loadConfig loads YAML from a temporary file and fails the test on any
// error.
func loadConfig(t testing.TB, yaml string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "switchyard.yaml")
	files := map[string]string{path: yaml}
	snap, err := config.Load(path, config.LoadOptions{
		LookupEnv: func(string) (string, bool) { return "", false },
		ReadFile: func(name string) ([]byte, error) {
			if s, ok := files[name]; ok {
				return []byte(s), nil
			}
			return nil, errors.New("no such file")
		},
		Glob: func(string) ([]string, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return snap.Config()
}

// newTestCatalog returns the catalog of testCatalogYAML.
func newTestCatalog(t testing.TB) *Catalog {
	t.Helper()
	c, err := New(loadConfig(t, testCatalogYAML))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// caps builds a capability set.
func caps(cs ...schema.Capability) schema.CapabilitySet {
	s := schema.CapabilitySet{}
	for _, c := range cs {
		s[c] = struct{}{}
	}
	return s
}

// ids returns the IDs of ds.
func ids(ds []*Deployment) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

func TestNew(t *testing.T) {
	c := newTestCatalog(t)
	var got []string
	for _, m := range c.Models() {
		got = append(got, m.ID+":"+m.Type+":"+strings.Join(ids(m.Deployments), ","))
	}
	want := []string{
		"llama:chat:llama@together,llama@gpu",
		"gpt:chat:gpt@openai,gpt@together,gpt@gpu",
		"claude:chat:claude@anthropic",
		"embed:embedding:embed@openai,embed@together",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("models (-want +got):\n%s", diff)
	}

	llama := c.Models()[0]
	if llama.ContextWindow != 131072 || llama.MaxOutputTokens != 8192 || llama.TokenRatio != 1.05 {
		t.Errorf("llama limits = %d, %d, %v", llama.ContextWindow, llama.MaxOutputTokens, llama.TokenRatio)
	}
	if r := c.Models()[1].TokenRatio; r != 1 {
		t.Errorf("default token ratio = %v, want 1", r)
	}
	together, gpu := llama.Deployments[0], llama.Deployments[1]
	if together.Model != llama || together.Provider != "together" || together.ProviderType != "openai" ||
		together.UpstreamModel != "meta-llama/Llama-3.3-70B-Instruct-Turbo" {
		t.Errorf("together deployment = %+v", together)
	}
	// A deployment without its own list inherits the model's; one with a
	// list replaces it.
	if diff := cmp.Diff(llama.Capabilities.Sorted(), together.Capabilities.Sorted()); diff != "" {
		t.Errorf("inherited capabilities (-model +deployment):\n%s", diff)
	}
	wantGPU := []schema.Capability{schema.CapJSONMode, schema.CapStreaming, schema.CapTools}
	if diff := cmp.Diff(wantGPU, gpu.Capabilities.Sorted()); diff != "" {
		t.Errorf("override capabilities (-want +got):\n%s", diff)
	}
	// Provider tags come first; duplicates are dropped.
	if diff := cmp.Diff([]string{"self_hosted", "zdr", "quantized"}, gpu.Tags); diff != "" {
		t.Errorf("tags (-want +got):\n%s", diff)
	}
	if len(together.Tags) != 0 {
		t.Errorf("tags of an untagged deployment = %q", together.Tags)
	}

	claude := c.Models()[2].Deployments[0]
	if diff := cmp.Diff(Pricing{Input: 3, Output: 15, CachedInput: 0.30, CacheWrite: 3.75, Known: true}, claude.Pricing); diff != "" {
		t.Errorf("claude pricing (-want +got):\n%s", diff)
	}
	if p := c.Models()[1].Deployments[0].Pricing; p.Known {
		t.Errorf("gpt@openai has no prices, but Known = true")
	}
	embed := c.Models()[3]
	if embed.Dimensions != 1536 || !embed.Deployments[0].Pricing.Known || embed.Deployments[1].Pricing.Known {
		t.Errorf("embedding model = %+v; want dimensions 1536, input price known only for the first deployment", embed)
	}

	if diff := cmp.Diff(map[string]string{"code": "claude", "cheap-llama": "llama@gpu", "autopilot-alias": "gpt"}, c.Aliases()); diff != "" {
		t.Errorf("aliases (-want +got):\n%s", diff)
	}
}

// The catalog hands out copies of its collections.
func TestCollectionsAreCopies(t *testing.T) {
	c := newTestCatalog(t)
	c.Models()[0] = nil
	c.Aliases()["code"] = "gpt"
	res, err := c.Resolve("llama")
	if err != nil {
		t.Fatal(err)
	}
	res.Candidates[0] = nil
	if c.Models()[0] == nil || c.Aliases()["code"] != "claude" {
		t.Error("changing a returned collection changed the catalog")
	}
	if again, _ := c.Resolve("llama"); again.Candidates[0] == nil {
		t.Error("changing Resolution.Candidates changed the catalog")
	}
}

func TestNewRejectsInconsistentConfig(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{
			Providers: []config.Provider{{ID: "p", Type: "openai"}},
			Models:    []config.Model{{ID: "m", Type: "chat", Deployments: []config.Deployment{{Provider: "p", UpstreamModel: "u"}}}},
		}
	}
	tests := []struct {
		name   string
		modify func(*config.Config)
		want   string
	}{
		{"unknown provider", func(c *config.Config) { c.Models[0].Deployments[0].Provider = "q" }, `unknown provider "q"`},
		{"duplicate model", func(c *config.Config) { c.Models = append(c.Models, c.Models[0]) }, `duplicate model id "m"`},
		{"duplicate deployment", func(c *config.Config) {
			c.Models[0].Deployments = append(c.Models[0].Deployments, c.Models[0].Deployments[0])
		}, `duplicate deployment "m@p"`},
		{"bad alias", func(c *config.Config) { c.Aliases = map[string]string{"a": "zz"} }, `alias "a"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			if _, err := New(cfg); err != nil {
				t.Fatalf("New(base): %v", err)
			}
			tt.modify(cfg)
			_, err := New(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("New error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	c := newTestCatalog(t)
	tests := []struct {
		name       string
		model      string
		wantModel  string
		wantType   string
		wantCands  []string
		wantPinned bool
		wantAlias  string
		wantErr    error
	}{
		{name: "model id", model: "llama", wantModel: "llama", wantType: "chat", wantCands: []string{"llama@together", "llama@gpu"}},
		{name: "alias to a model id", model: "code", wantModel: "claude", wantType: "chat", wantCands: []string{"claude@anthropic"}, wantAlias: "code"},
		{name: "alias to model@provider", model: "cheap-llama", wantModel: "llama", wantType: "chat", wantCands: []string{"llama@gpu"}, wantPinned: true, wantAlias: "cheap-llama"},
		{name: "model@provider", model: "gpt@together", wantModel: "gpt", wantType: "chat", wantCands: []string{"gpt@together"}, wantPinned: true},
		{name: "alias starting with auto", model: "autopilot-alias", wantModel: "gpt", wantType: "chat", wantCands: []string{"gpt@openai", "gpt@together", "gpt@gpu"}, wantAlias: "autopilot-alias"},
		// Resolve does not check the endpoint; the caller compares Type.
		{name: "embedding model", model: "embed", wantModel: "embed", wantType: "embedding", wantCands: []string{"embed@openai", "embed@together"}},
		{name: "auto", model: "auto", wantErr: ErrVirtualModel},
		{name: "auto profile", model: "auto:cheap", wantErr: ErrVirtualModel},
		{name: "auto prefix", model: "autocomplete", wantErr: ErrVirtualModel},
		{name: "unknown", model: "gpt-9", wantErr: ErrUnknownModel},
		{name: "empty", model: "", wantErr: ErrUnknownModel},
		{name: "unknown provider", model: "llama@openai", wantErr: ErrUnknownModel},
		{name: "unknown model with provider", model: "nope@gpu", wantErr: ErrUnknownModel},
		{name: "alias with provider is not a pin", model: "code@anthropic", wantErr: ErrUnknownModel},
		{name: "case matters", model: "LLAMA", wantErr: ErrUnknownModel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := c.Resolve(tt.model)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Resolve(%q) error = %v, want %v", tt.model, err, tt.wantErr)
				}
				if !errors.Is(err, ErrVirtualModel) && !errors.Is(err, ErrUnknownModel) {
					t.Errorf("unexpected error kind %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tt.model, err)
			}
			if res.Model.ID != tt.wantModel || res.Model.Type != tt.wantType || res.Pinned != tt.wantPinned || res.Alias != tt.wantAlias {
				t.Errorf("Resolve(%q) = model %s (%s), pinned %v, alias %q; want %s (%s), %v, %q",
					tt.model, res.Model.ID, res.Model.Type, res.Pinned, res.Alias, tt.wantModel, tt.wantType, tt.wantPinned, tt.wantAlias)
			}
			if diff := cmp.Diff(tt.wantCands, ids(res.Candidates)); diff != "" {
				t.Errorf("candidates (-want +got):\n%s", diff)
			}
		})
	}
}

// A catalog model whose name starts with "auto" (but is not "auto" or
// "auto:…", which validation reserves) still resolves.
func TestResolveAutoNamedModel(t *testing.T) {
	cfg := loadConfig(t, `version: 1
providers: [{id: p, type: openai, base_url: 'https://p.example'}]
models: [{id: autopilot, deployments: [{provider: p, upstream_model: u, pricing: {input: 1, output: 1}}]}]
`)
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := c.Resolve("autopilot"); err != nil || res.Model.ID != "autopilot" {
		t.Errorf("Resolve(autopilot) = %v, %v", res.Model, err)
	}
	if _, err := c.Resolve("autopilot2"); !errors.Is(err, ErrVirtualModel) {
		t.Errorf("Resolve(autopilot2) error = %v, want ErrVirtualModel", err)
	}
}

func TestSelect(t *testing.T) {
	c := newTestCatalog(t)
	resolve := func(name string) Resolution {
		t.Helper()
		res, err := c.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	type excl = Exclusion
	tests := []struct {
		name       string
		model      string
		required   schema.CapabilitySet
		estIn      int64
		estOut     int64
		drop       bool
		want       string // the chosen deployment; "" means none is eligible
		wantDrop   []schema.Capability
		wantExcl   []Exclusion
		wantErrMsg string
	}{
		{name: "cheapest wins", model: "llama", want: "llama@gpu", estIn: 1000, estOut: 500},
		{name: "zero tokens tie keeps config order", model: "llama", want: "llama@together"},
		{name: "pinned provider", model: "llama@together", want: "llama@together", estIn: 1000, estOut: 500},
		{name: "unknown price ranks last", model: "gpt", want: "gpt@gpu", estIn: 1000, estOut: 100},
		{name: "unknown price ranks last for embeddings", model: "embed", estIn: 1000, want: "embed@openai"},
		{name: "input-heavy ranking", model: "gpt", want: "gpt@gpu", estIn: 10000, estOut: 0},
		// gpu costs 1 in + 3 out, together 2 + 2: with output-heavy
		// requests together is cheaper.
		{name: "output-heavy ranking", model: "gpt", want: "gpt@together", estIn: 0, estOut: 10000},
		{name: "tie keeps config order", model: "gpt", want: "gpt@together", estIn: 1000, estOut: 1000},
		{name: "unknown price is chosen when it is the only candidate", model: "gpt@openai", estIn: 1000, want: "gpt@openai"},
		{name: "unknown price is chosen when it is the only eligible", model: "gpt", required: caps(schema.CapJSONSchema), estIn: 1000,
			want: "gpt@openai", wantExcl: []excl{{"gpt@together", "lacks capability json_schema"}, {"gpt@gpu", "lacks capability json_schema"}}},
		{name: "capability exclusion", model: "llama", required: caps(schema.CapN), estIn: 10, estOut: 10,
			want: "llama@together", wantExcl: []excl{{"llama@gpu", "lacks capability n"}}},
		{name: "several missing capabilities", model: "llama", required: caps(schema.CapSeed, schema.CapN, schema.CapStreaming),
			want: "llama@together", wantExcl: []excl{{"llama@gpu", "lacks capabilities n, seed"}}},
		{name: "drop keeps the cheaper deployment", model: "llama", required: caps(schema.CapN, schema.CapSeed, schema.CapTools), drop: true,
			estIn: 10, estOut: 10, want: "llama@gpu", wantDrop: []schema.Capability{schema.CapN, schema.CapSeed}},
		{name: "dropped for the chosen deployment only", model: "llama", required: caps(schema.CapN), drop: true, estIn: 10,
			want: "llama@gpu", wantDrop: []schema.Capability{schema.CapN}},
		{name: "drop on a pinned deployment that supports it", model: "llama@together", required: caps(schema.CapN), drop: true,
			want: "llama@together"},
		{name: "hard capabilities are never dropped", model: "claude", required: caps(schema.CapN, schema.CapSeed, schema.CapLogprobs,
			schema.CapPenalties, schema.CapTemperature2, schema.CapCompletions), drop: true,
			wantExcl:   []excl{{"claude@anthropic", "lacks capability completions"}},
			wantErrMsg: `no eligible deployment for model "claude": claude@anthropic: lacks capability completions`},
		{name: "every droppable capability", model: "claude", required: caps(schema.CapN, schema.CapSeed, schema.CapLogprobs,
			schema.CapPenalties, schema.CapTemperature2, schema.CapVision), drop: true, want: "claude@anthropic",
			wantDrop: []schema.Capability{schema.CapLogprobs, schema.CapN, schema.CapPenalties, schema.CapSeed, schema.CapTemperature2}},
		{name: "droppable is excluded without drop", model: "claude", required: caps(schema.CapN),
			wantExcl:   []excl{{"claude@anthropic", "lacks capability n"}},
			wantErrMsg: `claude@anthropic: lacks capability n`},
		{name: "parallel tools dropped", model: "llama", required: caps(schema.CapTools, schema.CapParallelTools), drop: true,
			estIn: 10, want: "llama@gpu", wantDrop: []schema.Capability{schema.CapParallelTools}},
		{name: "vision is hard", model: "llama", required: caps(schema.CapVision), drop: true,
			wantExcl: []excl{{"llama@together", "lacks capability vision"}, {"llama@gpu", "lacks capability vision"}}},
		{name: "context fits exactly", model: "claude", estIn: 150000, estOut: 50000, want: "claude@anthropic"},
		{name: "context filter", model: "claude", estIn: 150001, estOut: 50000,
			wantExcl:   []excl{{"claude@anthropic", "context 200000 < 200001 needed"}},
			wantErrMsg: `claude@anthropic: context 200000 < 200001 needed`},
		{name: "context filter does not overflow", model: "claude", estIn: 1000, estOut: math.MaxInt64,
			wantExcl: []excl{{"claude@anthropic", "context 200000 < 9223372036854775807 needed"}}},
		{name: "context filter does not overflow with both at the maximum", model: "claude", estIn: math.MaxInt64, estOut: math.MaxInt64,
			wantExcl: []excl{{"claude@anthropic", "context 200000 < 9223372036854775807 needed"}}},
		{name: "unknown context window is not filtered", model: "gpt", estIn: 10_000_000, estOut: 1, want: "gpt@gpu"},
		{name: "capability and context reasons together", model: "claude", required: caps(schema.CapN), estIn: 300000,
			wantExcl: []excl{{"claude@anthropic", "lacks capability n; context 200000 < 300000 needed"}}},
		{name: "no eligible lists every candidate", model: "gpt", required: caps(schema.CapPromptCaching),
			wantExcl: []excl{{"gpt@openai", "lacks capability prompt_caching"}, {"gpt@together", "lacks capability prompt_caching"}, {"gpt@gpu", "lacks capability prompt_caching"}},
			wantErrMsg: `no eligible deployment for model "gpt": gpt@openai: lacks capability prompt_caching; ` +
				`gpt@together: lacks capability prompt_caching; gpt@gpu: lacks capability prompt_caching`},
		{name: "negative estimates count as zero", model: "claude", estIn: -1 << 62, estOut: -5, want: "claude@anthropic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel, err := c.Select(resolve(tt.model), tt.required, tt.estIn, tt.estOut, tt.drop)
			if tt.want == "" {
				var ne *NoEligibleError
				if !errors.As(err, &ne) {
					t.Fatalf("Select error = %v (selection %v), want *NoEligibleError", err, sel.Deployment)
				}
				if ne.Model != resolve(tt.model).Model.ID {
					t.Errorf("NoEligibleError.Model = %q", ne.Model)
				}
				if diff := cmp.Diff(tt.wantExcl, ne.Exclusions); diff != "" {
					t.Errorf("exclusions (-want +got):\n%s", diff)
				}
				if !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if sel.Deployment.ID != tt.want {
				t.Errorf("chose %s, want %s", sel.Deployment.ID, tt.want)
			}
			if diff := cmp.Diff(tt.wantDrop, sel.Dropped); diff != "" {
				t.Errorf("dropped (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantExcl, sel.Exclusions); diff != "" {
				t.Errorf("exclusions (-want +got):\n%s", diff)
			}
		})
	}
}

// sampling is dropped for a deployment without it, whatever the drop flag
// says; forced_tools is never dropped.
func TestSelectSampling(t *testing.T) {
	cfg := loadConfig(t, `version: 1
providers:
  - {id: fixed, type: anthropic, base_url: 'https://fixed.example'}
  - {id: free, type: openai, base_url: 'https://free.example'}
models:
  - id: m
    capabilities: [streaming, tools, forced_tools, sampling, temperature_2]
    deployments:
      - {provider: fixed, upstream_model: u, capabilities: [streaming, tools], pricing: {input: 1, output: 1}}
      - {provider: free, upstream_model: u, pricing: {input: 5, output: 5}}
`)
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	all, err := c.Resolve("m")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := c.Resolve("m@fixed")
	if err != nil {
		t.Fatal(err)
	}
	S, T2, FT := schema.CapSampling, schema.CapTemperature2, schema.CapForcedTools
	tests := []struct {
		name     string
		res      Resolution
		required schema.CapabilitySet
		drop     bool
		want     string
		wantDrop []schema.Capability
		wantExcl []Exclusion
	}{
		{name: "dropped without the drop flag", res: all, required: caps(S), want: "m@fixed", wantDrop: []schema.Capability{S}},
		{name: "dropped with the drop flag", res: all, required: caps(S), drop: true, want: "m@fixed", wantDrop: []schema.Capability{S}},
		{name: "not dropped on a deployment that has it", res: Resolution{Model: all.Model, Candidates: all.Candidates[1:]},
			required: caps(S), want: "m@free"},
		{name: "temperature above 1 goes with sampling", res: pinned, required: caps(S, T2), want: "m@fixed",
			wantDrop: []schema.Capability{S, T2}},
		{name: "temperature_2 alone still needs the flag", res: pinned, required: caps(T2),
			wantExcl: []Exclusion{{"m@fixed", "lacks capability temperature_2"}}},
		{name: "forced tools are never dropped", res: all, required: caps(S, FT), drop: true, want: "m@free",
			wantExcl: []Exclusion{{"m@fixed", "lacks capability forced_tools"}}},
		{name: "forced tools exclude a pinned deployment", res: pinned, required: caps(FT), drop: true,
			wantExcl: []Exclusion{{"m@fixed", "lacks capability forced_tools"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel, err := c.Select(tt.res, tt.required, 1000, 100, tt.drop)
			if tt.want == "" {
				var ne *NoEligibleError
				if !errors.As(err, &ne) {
					t.Fatalf("Select = %v, %v; want *NoEligibleError", sel.Deployment, err)
				}
				if diff := cmp.Diff(tt.wantExcl, ne.Exclusions); diff != "" {
					t.Errorf("exclusions (-want +got):\n%s", diff)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if sel.Deployment.ID != tt.want {
				t.Errorf("chose %s, want %s", sel.Deployment.ID, tt.want)
			}
			if diff := cmp.Diff(tt.wantDrop, sel.Dropped); diff != "" {
				t.Errorf("dropped (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantExcl, sel.Exclusions); diff != "" {
				t.Errorf("exclusions (-want +got):\n%s", diff)
			}
		})
	}
}

// A model offered by two providers is served by the cheaper one, and
// model@provider pins the other (PLAN P2 acceptance).
func TestCheaperProviderAndPin(t *testing.T) {
	c := newTestCatalog(t)
	for name, want := range map[string]string{"llama": "llama@gpu", "llama@together": "llama@together", "cheap-llama": "llama@gpu"} {
		res, err := c.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		sel, err := c.Select(res, caps(schema.CapStreaming), 2000, 500, false)
		if err != nil {
			t.Fatal(err)
		}
		if sel.Deployment.ID != want {
			t.Errorf("%s served by %s, want %s", name, sel.Deployment.ID, want)
		}
	}
}

// The droppable set is exactly the one the brief defines.
func TestDroppableVocabulary(t *testing.T) {
	var droppable []schema.Capability
	for _, c := range schema.Vocabulary() {
		if c.Droppable() {
			droppable = append(droppable, c)
		}
		if !c.Known() {
			t.Errorf("%s is in the vocabulary but not Known", c)
		}
	}
	slices.Sort(droppable)
	want := []schema.Capability{schema.CapLogprobs, schema.CapN, schema.CapParallelTools, schema.CapPenalties,
		schema.CapSampling, schema.CapSeed, schema.CapTemperature2}
	if diff := cmp.Diff(want, droppable); diff != "" {
		t.Errorf("droppable (-want +got):\n%s", diff)
	}
	for _, c := range schema.Vocabulary() {
		if c.AlwaysDropped() != (c == schema.CapSampling) {
			t.Errorf("%s.AlwaysDropped() = %v", c, c.AlwaysDropped())
		}
	}
	if len(schema.Vocabulary()) != 16 || schema.Capability("telepathy").Known() {
		t.Errorf("vocabulary = %v", schema.Vocabulary())
	}
}

func TestRank(t *testing.T) {
	c := newTestCatalog(t)
	tests := []struct {
		name     string
		model    string
		required schema.CapabilitySet
		drop     bool
		want     []string // candidate IDs, cheapest first
		dropped  [][]schema.Capability
		excluded []string
	}{
		{
			name: "cheapest first, unknown price last", model: "gpt",
			want:    []string{"gpt@gpu", "gpt@together", "gpt@openai"},
			dropped: [][]schema.Capability{nil, nil, nil},
		},
		{
			name: "two providers", model: "llama",
			want:    []string{"llama@gpu", "llama@together"},
			dropped: [][]schema.Capability{nil, nil},
		},
		{
			name: "excluded deployments are listed", model: "llama", required: caps(schema.CapN),
			want: []string{"llama@together"}, dropped: [][]schema.Capability{nil}, excluded: []string{"llama@gpu"},
		},
		{
			name: "each candidate has its own dropped list", model: "llama", required: caps(schema.CapN), drop: true,
			want:    []string{"llama@gpu", "llama@together"},
			dropped: [][]schema.Capability{{schema.CapN}, nil},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := c.Resolve(tt.model)
			if err != nil {
				t.Fatal(err)
			}
			r, err := c.Rank(res, tt.required, 100, 100, tt.drop)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			var dropped [][]schema.Capability
			for _, cand := range r.Candidates {
				got = append(got, cand.Deployment.ID)
				dropped = append(dropped, cand.Dropped)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("candidates (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.dropped, dropped); diff != "" {
				t.Errorf("dropped (-want +got):\n%s", diff)
			}
			var excluded []string
			for _, x := range r.Exclusions {
				excluded = append(excluded, x.Deployment)
			}
			if diff := cmp.Diff(tt.excluded, excluded); diff != "" {
				t.Errorf("exclusions (-want +got):\n%s", diff)
			}
			// Select agrees with the head of the ranking.
			sel, err := c.Select(res, tt.required, 100, 100, tt.drop)
			if err != nil || sel.Deployment != r.Candidates[0].Deployment {
				t.Errorf("Select = %v, %v; Rank's first is %s", sel.Deployment, err, r.Candidates[0].Deployment.ID)
			}
		})
	}

	t.Run("none eligible", func(t *testing.T) {
		res, _ := c.Resolve("claude")
		_, err := c.Rank(res, caps(schema.CapN), 1, 1, false)
		var ne *NoEligibleError
		if !errors.As(err, &ne) || len(ne.Exclusions) != 1 {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("ties keep config order", func(t *testing.T) {
		cfg := loadConfig(t, `version: 1
providers:
  - {id: a, type: openai, base_url: 'https://a.example/v1'}
  - {id: b, type: openai, base_url: 'https://b.example/v1'}
  - {id: c, type: openai, base_url: 'https://c.example/v1'}
models:
  - id: m
    deployments:
      - {provider: c, upstream_model: m, pricing: {input: 1, output: 1}}
      - {provider: a, upstream_model: m, pricing: {input: 1, output: 1}}
      - {provider: b, upstream_model: m, pricing: {input: 1, output: 1}, latency_hint: {ttft_ms: 400, tokens_per_sec: 80}}
`)
		cat, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		res, _ := cat.Resolve("m")
		r, err := cat.Rank(res, nil, 10, 10, false)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, cand := range r.Candidates {
			got = append(got, cand.Deployment.ID)
		}
		if diff := cmp.Diff([]string{"m@c", "m@a", "m@b"}, got); diff != "" {
			t.Errorf("order (-want +got):\n%s", diff)
		}
		if h := r.Candidates[2].Deployment.LatencyHint; h.TTFT != 400*time.Millisecond || h.TokensPerSec != 80 {
			t.Errorf("latency hint %+v", h)
		}
	})
}
