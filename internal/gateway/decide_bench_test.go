package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/config"
	"switchyard/internal/schema"
)

// routeBenchGateway is a gateway over 200 chat deployments (50 models on
// 4 of 8 providers each) with a default policy that routes auto, prefers
// some deployments, denies others and caps the cost, and a tenant budget.
func routeBenchGateway(tb testing.TB) *Gateway {
	tb.Helper()
	cfg := &config.Config{Routing: config.DefaultRouting()}
	for p := range 8 {
		cfg.Providers = append(cfg.Providers, config.Provider{ID: fmt.Sprintf("prov%d", p), Type: "openai", BaseURL: "http://127.0.0.1:1/v1"})
	}
	prices := []string{"0.05", "0.15", "0.5", "1", "2.5", "3", "10", "15"}
	for m := range 50 {
		cm := config.Model{
			ID:           fmt.Sprintf("model-%02d", m),
			Capabilities: []string{"streaming", "stream_usage", "tools", "json_mode", "sampling"},
			Quality:      map[string]float64{"general": 0.5 + float64(m%10)/20, "coding": 0.45 + float64(m%7)/14},
		}
		for k := range 4 {
			cm.Deployments = append(cm.Deployments, config.Deployment{
				Provider: fmt.Sprintf("prov%d", (m+k*2)%8), UpstreamModel: "u",
				Pricing:       price(prices[(m+k)%len(prices)], prices[(m+k+3)%len(prices)]),
				LatencyHint:   config.LatencyHint{TTFTMs: 200 + 100*((m+k)%15)},
				QualityOffset: -0.01 * float64(k),
			})
		}
		cfg.Models = append(cfg.Models, cm)
	}
	cfg.Policies = []config.Policy{{
		ID: "default", BaselineModel: "model-09", Prefer: []string{"model-1*@*", "*@prov3"},
		Deny: []string{"model-4*@prov7"}, MaxCostPerRequestUSD: mustPriceS("0.02"),
	}}
	cfg.Tenants = []config.Tenant{{ID: "t1", Keys: []config.APIKey{{ID: "k1"}}, Budgets: []config.Budget{{
		Scope: config.BudgetScopeTenant, Window: config.BudgetWindowMonth, LimitUSD: mustPriceS("1000"),
		SoftAt: 0.8, SoftProfile: "cheap", OnExhausted: config.BudgetBlock,
	}}}}
	g, err := New(Options{Config: cfg, Logger: slog.New(slog.DiscardHandler), Registry: prometheus.NewRegistry()})
	if err != nil {
		tb.Fatal(err)
	}
	return g
}

// routeBenchRequest is a coding request of about 2 KB with tools.
func routeBenchRequest(tb testing.TB) (*routeRequest, int) {
	tb.Helper()
	body := `{"model":"auto","tools":[{"type":"function","function":{"name":"run_tests"}}],"messages":[` +
		`{"role":"system","content":"You are a careful senior engineer."},` +
		`{"role":"user","content":"Fix the failing test and explain the trade-offs step by step:\n` +
		"```go\\nfunc Sum(xs []int) int {\\n\\tt := 0\\n\\tfor i := 1; i < len(xs); i++ {\\n\\t\\tt += xs[i]\\n\\t}\\n\\treturn t\\n}\\n```\\n" +
		`--- FAIL: TestSum (0.00s)\n    sum_test.go:9: Sum([1 2 3]) = 5, want 6"}]}`
	req, err := schema.DecodeChatRequest([]byte(body))
	if err != nil {
		tb.Fatal(err)
	}
	maxTokens, hasMax := req.MaxTokens()
	return &routeRequest{
		model: req.Model(), header: http.Header{}, chat: req, tenant: "t1", keyID: "k1",
		size: len(body), maxTokens: maxTokens, hasMax: hasMax, outputs: chatOutputs(req),
		required: req.RequiredCapabilities(),
	}, len(body)
}

// BenchmarkRoute200 measures the gateway's whole routing step for one
// auto request over a 200-deployment catalog: hints, assessment, policy,
// the budget state read, and the router's decision (P5 acceptance: ≤ 1 ms
// at p99).
func BenchmarkRoute200(b *testing.B) {
	g := routeBenchGateway(b)
	rr, _ := routeBenchRequest(b)
	ctx := context.Background()
	if rt, e := g.current().route(ctx, EndpointChatCompletions, rr, &routed{}); e != nil || len(rt.served.Plan) == 0 {
		b.Fatalf("route: %+v", e)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, e := g.current().route(ctx, EndpointChatCompletions, rr, &routed{}); e != nil {
			b.Fatal(e)
		}
	}
}

// TestRouteLatencyP99 guards BenchmarkRoute200: the p99 of 1000 routing
// steps stays under a bound that is generous for the 1 ms target, more so
// under the race detector.
func TestRouteLatencyP99(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	bound := 2 * time.Millisecond
	if raceEnabled {
		bound = 25 * time.Millisecond
	}
	g := routeBenchGateway(t)
	rr, _ := routeBenchRequest(t)
	ctx := context.Background()
	for range 100 {
		if _, e := g.current().route(ctx, EndpointChatCompletions, rr, &routed{}); e != nil {
			t.Fatal(e)
		}
	}
	const runs = 1000
	times := make([]time.Duration, runs)
	for i := range times {
		start := time.Now()
		_, e := g.current().route(ctx, EndpointChatCompletions, rr, &routed{})
		times[i] = time.Since(start)
		if e != nil {
			t.Fatal(e)
		}
	}
	slices.Sort(times)
	p50, p99 := times[runs/2], times[runs*99/100]
	t.Logf("route over 200 deployments: p50 %v, p99 %v (bound %v)", p50, p99, bound)
	if p99 > bound {
		t.Errorf("p99 = %v, want ≤ %v", p99, bound)
	}
}
