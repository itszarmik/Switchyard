package semantic_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/cache/semantic"
	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/providers/openai"
	"switchyard/test/fakeupstream"
)

// testTimeout bounds every call that should succeed.
const testTimeout = 10 * time.Second

// secretText is the text embedded in the tests; no error may contain it.
const secretText = "what is the airspeed of an unladen swallow?" // 43 bytes: 11 tokens estimated

// testEnv is a catalog over two fake OpenAI-wire upstreams, and their
// adapters.
type testEnv struct {
	cat      *catalog.Catalog
	adapters map[string]providers.Adapter
	up1, up2 *fakeupstream.Server
}

// vec3 embeds every input as [1, 2, 3].
func vec3(string) []float32 { return []float32{1, 2, 3} }

func newTestEnv(t *testing.T, embed func(string) []float32) *testEnv {
	t.Helper()
	env := &testEnv{
		up1: fakeupstream.New(fakeupstream.Options{Embed: embed}),
		up2: fakeupstream.New(fakeupstream.Options{Embed: embed}),
	}
	t.Cleanup(env.up1.Close)
	t.Cleanup(env.up2.Close)
	text := fmt.Sprintf(`version: 1
providers:
  - {id: p1, type: openai, base_url: %q}
  - {id: p2, type: openai, base_url: %q}
models:
  - id: emb
    type: embedding
    dimensions: 3
    deployments:
      - {provider: p1, upstream_model: emb-up, pricing: {input: "0.05"}}
      - {provider: p2, upstream_model: emb-up2, pricing: {input: "0.02"}}
  - id: emb-ratio
    type: embedding
    dimensions: 3
    token_ratio: 1.5
    deployments:
      - {provider: p1, upstream_model: emb-up, pricing: {input: "0.02"}}
  - id: emb-unpriced
    type: embedding
    dimensions: 3
    deployments:
      - {provider: p1, upstream_model: emb-up}
  - id: chat
    deployments:
      - {provider: p1, upstream_model: chat-up, pricing: {input: 1, output: 2}}
`, env.up1.BaseURL(), env.up2.BaseURL())
	path := filepath.Join(t.TempDir(), "switchyard.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := config.Load(path, config.LoadOptions{LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := snap.Config()
	if env.cat, err = catalog.New(cfg); err != nil {
		t.Fatal(err)
	}
	env.adapters = map[string]providers.Adapter{}
	for _, p := range cfg.Providers {
		a, err := openai.New(p, providers.Options{})
		if err != nil {
			t.Fatal(err)
		}
		env.adapters[p.ID] = a
	}
	return env
}

// embedder returns the Embedder of name (see EmbeddingDeployment).
func (env *testEnv) embedder(t *testing.T, name string, timeout time.Duration) *semantic.Embedder {
	t.Helper()
	dep, emb, err := semantic.EmbeddingDeployment(env.cat, env.adapters, name)
	if err != nil {
		t.Fatal(err)
	}
	return semantic.NewEmbedder(emb, dep, dep.Model.Dimensions, timeout)
}

func TestEmbeddingDeployment(t *testing.T) {
	env := newTestEnv(t, vec3)
	tests := []struct {
		name    string
		want    string
		wantErr string
	}{
		{name: "emb", want: "emb@p2"}, // the cheaper deployment
		{name: "emb@p1", want: "emb@p1"},
		{name: "emb-unpriced@p1", want: "emb-unpriced@p1"},
		{name: "emb-unpriced", wantErr: "no deployment with a known input price"},
		{name: "chat", wantErr: "not an embedding model"},
		{name: "nope", wantErr: "nope"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dep, emb, err := semantic.EmbeddingDeployment(env.cat, env.adapters, tc.name)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || dep.ID != tc.want || emb == nil {
				t.Fatalf("= %v, %v, %v; want %s", dep, emb, err, tc.want)
			}
		})
	}
	// A provider whose adapter cannot embed is skipped.
	only1 := map[string]providers.Adapter{"p1": env.adapters["p1"]}
	if dep, _, err := semantic.EmbeddingDeployment(env.cat, only1, "emb"); err != nil || dep.ID != "emb@p1" {
		t.Errorf("without p2: %v, %v; want emb@p1", dep, err)
	}
	if _, _, err := semantic.EmbeddingDeployment(env.cat, map[string]providers.Adapter{}, "emb@p1"); err == nil {
		t.Error("a pinned deployment without an adapter resolved")
	}
}

func TestEmbedder(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		embed    func(string) []float32
		behavior fakeupstream.Behavior
		timeout  time.Duration
		wantVec  []float32
		// wantTokens and wantCost are the reported tokens and nano-USD.
		wantTokens int64
		wantCost   int64
		wantErr    string
		wantKind   providers.Kind
	}{
		{
			name: "usage reported", model: "emb@p1",
			behavior: fakeupstream.Behavior{Usage: &fakeupstream.Usage{PromptTokens: 7}},
			wantVec:  []float32{1, 2, 3}, wantTokens: 7, wantCost: 7 * 50, // $0.05/M = 50 nano-USD per token
		},
		{
			name: "fake's own usage", model: "emb",
			wantVec: []float32{1, 2, 3}, wantTokens: 11, wantCost: 11 * 20,
		},
		{
			name: "no usage: estimated", model: "emb@p1",
			behavior: fakeupstream.Behavior{Status: 200, Body: []byte(`{"object":"list","data":[{"embedding":[0.5,0.25,-1]}]}`)},
			wantVec:  []float32{0.5, 0.25, -1}, wantTokens: 11, wantCost: 11 * 50,
		},
		{
			name: "token ratio", model: "emb-ratio",
			behavior: fakeupstream.Behavior{Status: 200, Body: []byte(`{"data":[{"embedding":[1,1,1]}]}`)},
			wantVec:  []float32{1, 1, 1}, wantTokens: 17, wantCost: 17 * 20, // ceil(11 × 1.5)
		},
		{
			name: "rounded up to a nano-USD", model: "emb@p2",
			behavior: fakeupstream.Behavior{Usage: &fakeupstream.Usage{PromptTokens: 1}},
			wantVec:  []float32{1, 2, 3}, wantTokens: 1, wantCost: 20,
		},
		{
			name: "unknown price", model: "emb-unpriced@p1",
			wantVec: []float32{1, 2, 3}, wantTokens: 11, wantCost: -1,
		},
		{
			name: "server error may have billed", model: "emb@p1",
			behavior:   fakeupstream.Behavior{Status: 503},
			wantTokens: 11, wantCost: 11 * 50, wantErr: "server_error", wantKind: providers.KindServerError,
		},
		{
			name: "timeout may have billed", model: "emb@p1",
			behavior: fakeupstream.Behavior{Fault: fakeupstream.FaultHang}, timeout: 50 * time.Millisecond,
			wantTokens: 11, wantCost: 11 * 50, wantErr: "emb@p1",
		},
		{
			name: "refusal not billed", model: "emb@p1",
			behavior: fakeupstream.Behavior{Status: 400},
			wantErr:  "client_error", wantKind: providers.KindClientError,
		},
		{
			name: "auth failure not billed", model: "emb@p1",
			behavior: fakeupstream.Behavior{Status: 401},
			wantErr:  "auth", wantKind: providers.KindAuth,
		},
		{
			name: "wrong dimensions still charged", model: "emb@p1",
			embed:      func(string) []float32 { return []float32{1, 2} },
			wantTokens: 11, wantCost: 11 * 50, wantErr: "2 dimensions, want 3",
		},
		{
			name: "malformed body still charged", model: "emb@p1",
			behavior:   fakeupstream.Behavior{Status: 200, Body: []byte(`{"data":[{"embedding":"AAAA"}],"usage":{"prompt_tokens":4}}`)},
			wantTokens: 4, wantCost: 4 * 50, wantErr: "invalid embeddings response",
		},
		{
			name: "no embedding", model: "emb@p1",
			behavior:   fakeupstream.Behavior{Status: 200, Body: []byte(`{"data":[]}`)},
			wantTokens: 11, wantCost: 11 * 50, wantErr: "0 embeddings",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			embed := tc.embed
			if embed == nil {
				embed = vec3
			}
			env := newTestEnv(t, embed)
			env.up1.Script(tc.behavior)
			env.up2.Script(tc.behavior)
			timeout := tc.timeout
			if timeout == 0 {
				timeout = testTimeout
			}
			e := env.embedder(t, tc.model, timeout)
			got, err := e.Embed(context.Background(), secretText)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Embed: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Embed error %v, want one containing %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), secretText) || strings.Contains(fmt.Sprintf("%+v", err), "swallow") {
					t.Errorf("error %q contains the text", err)
				}
				var pe *providers.Error
				if tc.wantKind != "" && (!errors.As(err, &pe) || pe.Kind != tc.wantKind) {
					t.Errorf("error %v, want kind %s", err, tc.wantKind)
				}
			}
			if diff := cmp.Diff(tc.wantVec, got.Vector); diff != "" {
				t.Errorf("vector (-want +got):\n%s", diff)
			}
			if got.Tokens != tc.wantTokens || got.CostNano != tc.wantCost {
				t.Errorf("tokens %d cost %d, want %d and %d", got.Tokens, got.CostNano, tc.wantTokens, tc.wantCost)
			}
		})
	}
}

// TestEmbedderRequest checks what reaches the upstream: one float-encoded
// embeddings request of the deployment's upstream model.
func TestEmbedderRequest(t *testing.T) {
	env := newTestEnv(t, vec3)
	e := env.embedder(t, "emb@p1", testTimeout)
	if e.Model() != "emb@p1" || e.Dimensions() != 3 {
		t.Errorf("Model %q Dimensions %d, want emb@p1 and 3", e.Model(), e.Dimensions())
	}
	if _, err := e.Embed(context.Background(), secretText); err != nil {
		t.Fatal(err)
	}
	reqs := env.up1.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/v1/embeddings" {
		t.Fatalf("requests %+v", reqs)
	}
	var body map[string]any
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"model": "emb-up", "input": secretText, "encoding_format": "float"}
	if diff := cmp.Diff(want, body); diff != "" {
		t.Errorf("request body (-want +got):\n%s", diff)
	}
	if len(env.up2.Requests()) != 0 {
		t.Error("the pinned deployment's sibling was called")
	}
}

// TestEmbedderCallerCancel checks that the caller's context bounds the
// call too, and that a cancellation is charged as possibly billed.
func TestEmbedderCallerCancel(t *testing.T) {
	env := newTestEnv(t, vec3)
	release := make(chan struct{})
	defer close(release)
	env.up1.Script(fakeupstream.Behavior{Hold: release})
	e := env.embedder(t, "emb@p1", 0)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		wctx, wcancel := context.WithTimeout(context.Background(), testTimeout)
		defer wcancel()
		_ = env.up1.WaitReceived(wctx, 1)
		cancel()
	}()
	got, err := e.Embed(ctx, secretText)
	if err == nil {
		t.Fatal("Embed succeeded after its context was canceled")
	}
	if got.CostNano != 11*50 {
		t.Errorf("cost %d, want the estimate %d", got.CostNano, 11*50)
	}
}
