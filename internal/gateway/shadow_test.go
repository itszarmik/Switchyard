package gateway

import (
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/auth"
	"switchyard/internal/config"
	"switchyard/internal/reqctx"
	"switchyard/internal/shadow"
)

// captureShadow is a Shadow that keeps every offered sample.
type captureShadow struct {
	mu      sync.Mutex
	samples []shadow.Sample
}

func (c *captureShadow) Offer(s shadow.Sample) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, s)
	return true
}

func (c *captureShadow) all() []shadow.Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]shadow.Sample(nil), c.samples...)
}

// shadowEnv is a gateway with a cheap model on provider cheap and the
// policy's baseline on provider rich, tenant t1 under policy p, and a
// capturing Shadow.
type shadowEnv struct {
	g           *Gateway
	cheap, rich *chatAdapter
	sink        *captureSink
	sh          *captureShadow
	logs        *logBuffer
}

func newShadowEnv(t *testing.T, mode string, rate float64, edits ...func(*config.Config)) *shadowEnv {
	t.Helper()
	caps := []string{"streaming", "stream_usage", "sampling"}
	cfg := &config.Config{
		Providers: []config.Provider{
			{ID: "cheap", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"},
			{ID: "rich", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"},
		},
		Models: []config.Model{
			{ID: "small", Capabilities: caps, Quality: map[string]float64{"general": 0.9},
				Deployments: []config.Deployment{{Provider: "cheap", UpstreamModel: "small-up", Pricing: price("0.1", "0.4")}}},
			{ID: "big", Capabilities: caps, Quality: map[string]float64{"general": 0.97},
				Deployments: []config.Deployment{{Provider: "rich", UpstreamModel: "big-up", Pricing: price("3", "15")}}},
		},
		Policies: []config.Policy{{ID: "p", BaselineModel: "big", RoutingMode: mode, Shadow: config.ShadowPolicy{SampleRate: rate}}},
		Tenants:  []config.Tenant{{ID: "t1", Policy: "p", Keys: []config.APIKey{{ID: "k1"}}}},
		Routing:  fastRouting(),
	}
	for _, edit := range edits {
		edit(cfg)
	}
	logs := &logBuffer{}
	sink := &captureSink{}
	g, err := New(Options{Config: cfg, Logger: slog.New(slog.NewJSONHandler(logs, nil)), Registry: prometheus.NewRegistry(), Ledger: sink})
	if err != nil {
		t.Fatal(err)
	}
	e := &shadowEnv{g: g, cheap: &chatAdapter{name: "cheap"}, rich: &chatAdapter{name: "rich"}, sink: sink, sh: &captureShadow{}, logs: logs}
	g.adapters["cheap"], g.adapters["rich"] = e.cheap, e.rich
	if _, ok := g.adapters["mid"]; ok {
		g.adapters["mid"] = &chatAdapter{name: "mid"}
	}
	g.SetShadow(e.sh)
	return e
}

// do serves a chat request as t1 (anonymous when anon is set) with the
// given request ID.
func (e *shadowEnv) do(id, body string, anon bool) exchangeResult {
	ctx, ann := annotated(e.logs)
	ctx = reqctx.WithRequestID(ctx, id)
	if !anon {
		ctx = auth.WithPrincipal(ctx, auth.Principal{TenantID: "t1", KeyID: "k1"})
	}
	x := &recExchange{body: []byte(body)}
	e.g.ChatCompletions(ctx, x)
	x.res.ann = annotations(ann)
	return x.res
}

const shadowMsgs = `"messages":[{"role":"user","content":"Say hello."}]`

func TestShadowSampling(t *testing.T) {
	filtered := `{"id":"c","object":"chat.completion","created":7,"model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"no"},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	big := `{"id":"c","object":"chat.completion.chunk","created":7,"model":"up","choices":[{"index":0,"delta":{"content":"` +
		strings.Repeat("x", MaxShadowAnswerBytes+1) + `"},"finish_reason":"stop"}]}`
	tests := []struct {
		name   string
		mode   string
		rate   float64
		body   string
		anon   bool
		script func(e *shadowEnv)
		// want is the expected sample, by its fields checked here; nil
		// expects none.
		want *shadow.Sample
	}{
		{name: "enforce, non-streaming", rate: 1, body: `{"model":"auto",` + shadowMsgs + `}`,
			want: &shadow.Sample{Mode: shadow.ModeEnforce, Served: shadow.Answer{Deployment: "small@cheap", Text: "hello"}, Category: "general"}},
		{name: "enforce, streaming", rate: 1, body: `{"model":"auto","stream":true,` + shadowMsgs + `}`,
			want: &shadow.Sample{Mode: shadow.ModeEnforce, Served: shadow.Answer{Deployment: "small@cheap", Text: "hello"}, Category: "general"}},
		{name: "observe: the router's choice against the served baseline", mode: "observe", rate: 1, body: `{"model":"auto",` + shadowMsgs + `}`,
			want: &shadow.Sample{Mode: shadow.ModeObserve, Served: shadow.Answer{Deployment: "big@rich", Text: "hello"}, Category: "general"}},
		{name: "served by the baseline model", rate: 1, body: `{"model":"big",` + shadowMsgs + `}`},
		{name: "observe, served model is the router's choice", mode: "observe", rate: 1, body: `{"model":"big",` + shadowMsgs + `}`},
		{name: "rate 0", rate: 0, body: `{"model":"auto",` + shadowMsgs + `}`},
		{name: "unauthenticated", rate: 1, anon: true, body: `{"model":"small",` + shadowMsgs + `}`},
		{name: "content filter", rate: 1, body: `{"model":"auto",` + shadowMsgs + `}`,
			script: func(e *shadowEnv) { e.cheap.body = filtered }},
		{name: "stream over the answer cap", rate: 1, body: `{"model":"auto","stream":true,` + shadowMsgs + `}`,
			script: func(e *shadowEnv) { e.cheap.chunks = []string{big} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newShadowEnv(t, tt.mode, tt.rate)
			if tt.script != nil {
				tt.script(e)
			}
			res := e.do("req-1", tt.body, tt.anon)
			if res.status != 200 {
				t.Fatalf("status %d (%v)", res.status, res.err)
			}
			got := e.sh.all()
			if tt.want == nil {
				if len(got) != 0 {
					t.Fatalf("offered %d samples, want none", len(got))
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("offered %d samples, want 1", len(got))
			}
			s := got[0]
			wantComparison := "big@rich"
			if tt.want.Mode == shadow.ModeObserve {
				wantComparison = "small@cheap"
			}
			switch {
			case s.RequestID != "req-1" || s.Tenant != "t1" || s.KeyID != "k1" || s.Time.IsZero():
				t.Errorf("identity %q %q %q %v", s.RequestID, s.Tenant, s.KeyID, s.Time)
			case s.Mode != tt.want.Mode || s.Served != tt.want.Served || s.Category != tt.want.Category:
				t.Errorf("mode %q, served %+v, category %q; want %q, %+v, %q", s.Mode, s.Served, s.Category, tt.want.Mode, tt.want.Served, tt.want.Category)
			case s.Comparison == nil || s.Comparison.ID != wantComparison:
				t.Errorf("comparison %v, want %s", s.Comparison, wantComparison)
			case s.Request == nil || s.Policy == nil || s.Policy.PolicyID != "p" || s.Profile == "":
				t.Errorf("request %v, policy %v, profile %q", s.Request, s.Policy, s.Profile)
			}
			if ev := e.sink.only(t); ev.Category != "general" || ev.ID != "req-1" {
				t.Errorf("usage event category %q, id %q", ev.Category, ev.ID)
			}
		})
	}
}

// TestShadowSamplingRate: the sampled requests are exactly those
// shadow.Sampled picks, so every replica agrees.
func TestShadowSamplingRate(t *testing.T) {
	e := newShadowEnv(t, "", 0.3)
	want := 0
	for i := range 200 {
		id := "req-" + strings.Repeat("r", i%7) + string(rune('a'+i%26)) + strings.Repeat("z", i/26)
		if shadow.Sampled(id, 0.3) {
			want++
		}
		if res := e.do(id, `{"model":"auto",`+shadowMsgs+`}`, false); res.status != 200 {
			t.Fatalf("status %d", res.status)
		}
	}
	if got := len(e.sh.all()); got != want || want == 0 || want == 200 {
		t.Errorf("offered %d samples, shadow.Sampled picks %d", got, want)
	}
}

// TestShadowOfferWithNilRunner: a runner that takes no sample leaves the
// client's response unchanged.
func TestShadowOfferWithNilRunner(t *testing.T) {
	e := newShadowEnv(t, "", 1)
	var r *shadow.Runner // a nil runner samples nothing
	e.g.SetShadow(r)
	if res := e.do("req-1", `{"model":"auto",`+shadowMsgs+`}`, false); res.status != 200 || !strings.Contains(res.body, "hello") {
		t.Fatalf("status %d body %s", res.status, res.body)
	}
}

// withMid adds model mid, between small and big, on provider mid, and lets
// the policy substitute equivalent models.
func withMid(c *config.Config) {
	c.Providers = append(c.Providers, config.Provider{ID: "mid", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"})
	c.Models = append(c.Models, config.Model{ID: "mid", Capabilities: c.Models[0].Capabilities, Quality: map[string]float64{"general": 0.93},
		Deployments: []config.Deployment{{Provider: "mid", UpstreamModel: "mid-up", Pricing: price("1", "4")}}})
	c.Policies[0].Substitution = config.Substitution{Mode: config.SubstitutionEquivalent, MaxQualityDrop: 0.1}
}

// TestShadowEnforceRoles: enforce mode samples only requests where the
// gateway chose the model, and judges the served answer against the
// model the savings are measured against (correctness H1).
func TestShadowEnforceRoles(t *testing.T) {
	tests := []struct {
		name, body, served, comparison string
	}{
		// auto: the candidate is served, the reference the policy's
		// baseline_model.
		{"auto", `{"model":"auto",` + shadowMsgs + `}`, "small@cheap", "big@rich"},
		{"auto:profile", `{"model":"auto:cheap",` + shadowMsgs + `}`, "small@cheap", "big@rich"},
		// An equivalent substitute: the reference is the requested model,
		// not the policy's baseline.
		{"equivalent substitution", `{"model":"mid",` + shadowMsgs + `}`, "small@cheap", "mid@mid"},
		{"equivalent substitution for the baseline model", `{"model":"big",` + shadowMsgs + `}`, "small@cheap", "big@rich"},
		// Served by the model it named: nothing to verify, whether that
		// model is cheaper than the baseline or not.
		{"served as requested, cheaper than the baseline", `{"model":"small",` + shadowMsgs + `}`, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newShadowEnv(t, "", 1, withMid)
			res := e.do("req-1", tt.body, false)
			if res.status != 200 {
				t.Fatalf("status %d (%v)", res.status, res.err)
			}
			got := e.sh.all()
			if tt.served == "" {
				if len(got) != 0 {
					t.Fatalf("offered %d samples (served %s, comparison %s), want none", len(got), got[0].Served.Deployment, got[0].Comparison.ID)
				}
				return
			}
			if len(got) != 1 || got[0].Mode != shadow.ModeEnforce || got[0].Served.Deployment != tt.served || got[0].Comparison.ID != tt.comparison {
				t.Fatalf("samples %+v; want served %s against %s", got, tt.served, tt.comparison)
			}
		})
	}
	// With substitution pinned, a concrete model is served as requested and
	// not sampled, even when it is cheaper than the baseline.
	e := newShadowEnv(t, "", 1)
	if res := e.do("req-3", `{"model":"small",`+shadowMsgs+`}`, false); res.status != 200 || len(e.sh.all()) != 0 {
		t.Errorf("enforce, pinned small: status %d, %d samples", res.status, len(e.sh.all()))
	}
	// Observe mode is unchanged: a pinned request is served as is and has
	// nothing to compare.
	e = newShadowEnv(t, "observe", 1, withMid)
	if res := e.do("req-2", `{"model":"small",`+shadowMsgs+`}`, false); res.status != 200 || len(e.sh.all()) != 0 {
		t.Errorf("observe, pinned: status %d, %d samples", res.status, len(e.sh.all()))
	}
}

// TestShadowObserveDropsParams: in observe mode, the comparison request
// loses the parameters the router dropped for the comparison deployment,
// as the routed request would have, while the served baseline keeps them
// (correctness M2).
func TestShadowObserveDropsParams(t *testing.T) {
	e := newShadowEnv(t, "observe", 1, func(c *config.Config) {
		c.Models[0].Capabilities = []string{"streaming", "stream_usage"} // small lacks sampling
	})
	res := e.do("req-1", `{"model":"auto","temperature":0.7,"top_p":0.9,"max_tokens":50,`+shadowMsgs+`}`, false)
	if res.status != 200 {
		t.Fatalf("status %d (%v)", res.status, res.err)
	}
	got := e.sh.all()
	if len(got) != 1 || got[0].Comparison.ID != "small@cheap" || got[0].Served.Deployment != "big@rich" {
		t.Fatalf("samples %+v; want small@cheap against the served big@rich", got)
	}
	for _, f := range []string{"temperature", "top_p"} {
		if _, ok := got[0].Request.Field(f); ok {
			t.Errorf("the comparison request keeps %s, which small@cheap does not accept", f)
		}
	}
	if _, ok := got[0].Request.Field("max_tokens"); !ok {
		t.Error("the comparison request lost max_tokens, which was not dropped")
	}
	if n := len(e.rich.calls); n != 1 {
		t.Fatalf("%d calls to the served baseline", n)
	}
	if _, ok := e.rich.calls[0].req.Chat.Field("temperature"); !ok {
		t.Error("the served baseline lost temperature")
	}
}
