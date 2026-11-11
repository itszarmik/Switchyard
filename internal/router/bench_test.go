package router

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/quality"
	"switchyard/internal/schema"
)

// benchCatalog has 200 chat deployments: 50 models on 4 of 8 providers
// each, with varied prices, priors, tags and latency hints.
func benchCatalog(tb testing.TB) *catalog.Catalog {
	var provs []pspec
	for p := range 8 {
		ps := pspec{id: fmt.Sprintf("prov%d", p)}
		if p%3 == 0 {
			ps.tags = []string{"zdr"}
		}
		provs = append(provs, ps)
	}
	prices := []string{"0.05", "0.15", "0.5", "1", "2.5", "3", "10", "15"}
	var models []mspec
	for m := range 50 {
		ms := mspec{
			id:      fmt.Sprintf("model-%02d", m),
			ctx:     []int64{0, 32000, 128000, 200000}[m%4],
			caps:    []string{"streaming", "tools", "json_mode", "sampling"},
			quality: map[string]float64{"general": 0.5 + float64(m%10)/20, "coding": 0.45 + float64(m%7)/14},
		}
		for k := range 4 {
			ms.deps = append(ms.deps, dspec{
				provider: provs[(m+k*2)%8].id,
				in:       prices[(m+k)%len(prices)], out: prices[(m+k+3)%len(prices)],
				ttftMs: 200 + 100*((m+k)%15), offset: -0.01 * float64(k),
			})
		}
		models = append(models, ms)
	}
	return buildCatalog(tb, provs, models)
}

// benchInput is an auto request with a realistic policy: preferences, a
// deny list, a cost cap, a latency target and a known budget.
func benchInput() Input {
	in := autoInput()
	in.Required = schema.CapabilitySet{schema.CapTools: {}, schema.CapStreaming: {}}
	in.InputBytes, in.EstOut = 12000, 500
	in.Assessment = quality.Assessment{Category: quality.CategoryCoding, Difficulty: 0.55, Signals: []string{"code_block"}}
	in.Policy.Prefer = []string{"model-1*@*", "*@prov3"}
	in.Policy.Deny = []string{"model-4*@prov7"}
	in.Policy.MaxCostPerRequestNano = 20_000_000
	in.Policy.Constraints.MaxTTFT = 1200 * time.Millisecond
	in.BudgetRemainingMicro = 5_000_000
	return in
}

// BenchmarkDecide measures one routing decision over a 200-deployment
// catalog (P5 acceptance: ≤ 1 ms at p99).
func BenchmarkDecide(b *testing.B) {
	r := New(benchCatalog(b), newTracker(), config.Routing{})
	in := benchInput()
	if dec, err := r.Decide(in); err != nil || len(dec.Plan) == 0 || len(dec.Explanation.Candidates) != 200 {
		b.Fatalf("Decide: plan %d, candidates %d, err %v", len(dec.Plan), len(dec.Explanation.Candidates), err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := r.Decide(in); err != nil {
			b.Fatal(err)
		}
	}
}

// TestDecideLatencyP99 guards against a regression of BenchmarkDecide: the
// p99 of 1000 decisions over the 200-deployment catalog stays under a
// bound that is generous for the 1 ms target, and more generous under the
// race detector, which slows code down several times.
func TestDecideLatencyP99(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	bound := 2 * time.Millisecond
	if raceEnabled {
		bound = 20 * time.Millisecond
	}
	r := New(benchCatalog(t), newTracker(), config.Routing{})
	in := benchInput()
	for range 100 { // warm up
		if _, err := r.Decide(in); err != nil {
			t.Fatal(err)
		}
	}
	const runs = 1000
	times := make([]time.Duration, runs)
	for i := range times {
		start := time.Now()
		_, err := r.Decide(in)
		times[i] = time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(times)
	p50, p99 := times[runs/2], times[runs*99/100]
	t.Logf("Decide over 200 deployments: p50 %v, p99 %v (bound %v)", p50, p99, bound)
	if p99 > bound {
		t.Errorf("p99 = %v, want ≤ %v", p99, bound)
	}
}
