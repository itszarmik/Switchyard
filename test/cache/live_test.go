//go:build semanticlive

package cachetest

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"switchyard/internal/cache"
	"switchyard/internal/cache/semantic"
	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/eval"
)

// configEnv names the config whose semantic_cache.embedding_model the live
// harness uses.
const configEnv = "SWITCHYARD_SEMANTIC_CONFIG"

// liveCallTimeout bounds each embedding call. It is generous: the harness
// measures accuracy, and reports latency against the configured lookup
// timeout separately.
const liveCallTimeout = 30 * time.Second

// Acceptance (PLAN P6): the paraphrase set hits, the near-miss set never
// does.
const (
	minParaphraseHitRate = 0.8
	maxNearMissHitRate   = 0
)

// TestSemanticLive runs both sets through the real embedding model of the
// config at SWITCHYARD_SEMANTIC_CONFIG (its provider keys come from the
// environment, as for `switchyard serve`), at the min_similarity of the
// first policy that enables the semantic cache, or the default 0.95. It
// spends a few hundred small embedding calls.
func TestSemanticLive(t *testing.T) {
	path := os.Getenv(configEnv)
	if path == "" {
		t.Skip(configEnv + " is not set; set it to a config whose semantic_cache.embedding_model names a real " +
			"embedding deployment, with that provider's key in the environment, to run the live semantic-cache evaluation")
	}
	snap, err := config.Load(path, config.LoadOptions{LookupEnv: os.LookupEnv})
	if err != nil {
		t.Fatalf("loading %s: %v", configEnv, err)
	}
	cfg := snap.Config()
	if cfg.SemanticCache.EmbeddingModel == "" {
		t.Fatal("the config sets no semantic_cache.embedding_model")
	}
	cat, err := catalog.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	adapters, err := eval.Adapters(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dep, adapter, err := semantic.EmbeddingDeployment(cat, adapters, cfg.SemanticCache.EmbeddingModel)
	if err != nil {
		t.Fatal(err)
	}
	threshold, source := liveThreshold(cfg)
	t.Logf("embedding deployment %s (%d dimensions), threshold %.4f (%s)", dep.ID, dep.Model.Dimensions, threshold, source)

	meter := &meteredEmbedder{Embedder: semantic.NewEmbedder(adapter, dep, dep.Model.Dimensions, liveCallTimeout)}
	run := func(file string) []outcome {
		pairs := loadPairs(t, file)
		c := semantic.New(meter, semantic.NewMemoryIndex(10*len(pairs), nil), semantic.Options{LookupTimeout: liveCallTimeout})
		return runSet(t, c, pairs, threshold)
	}
	para, near := run(paraphraseFile), run(nearMissFile)

	paraRate := report(t, "paraphrase", para, threshold)
	nearRate := report(t, "near miss", near, threshold)
	meter.report(t, cfg.SemanticCache.LookupTimeout.Std())

	if paraRate < minParaphraseHitRate {
		t.Errorf("paraphrase hit rate %.3f, want at least %.2f", paraRate, minParaphraseHitRate)
	}
	if nearRate > maxNearMissHitRate {
		t.Errorf("near-miss false-hit rate %.3f, want %v", nearRate, maxNearMissHitRate)
	}
	if n := countWrong(para) + countWrong(near); n > 0 {
		t.Errorf("%d lookups hit another pair's entry", n)
	}
}

// liveThreshold returns the min_similarity in force and where it came
// from.
func liveThreshold(cfg *config.Config) (float64, string) {
	for _, p := range cfg.Policies {
		if p.Cache.Semantic.Enabled && p.Cache.Semantic.MinSimilarity > 0 {
			return p.Cache.Semantic.MinSimilarity, "policy " + p.ID
		}
	}
	return defaultMinSimilarity, "default"
}

// report logs a set's hit rate (own-entry hits over pairs) and similarity
// distribution, and returns the hit rate. Similarities are the best
// match's, hit or not.
func report(t *testing.T, name string, outs []outcome, threshold float64) float64 {
	t.Helper()
	hits := 0
	sims := make([]float64, 0, len(outs))
	for _, o := range outs {
		if o.hit {
			hits++
		}
		if !math.IsNaN(o.similarity) {
			sims = append(sims, o.similarity)
		}
	}
	rate := float64(hits) / float64(len(outs))
	t.Logf("%s: %d/%d hits (rate %.3f) at %.4f, %d on another pair's entry", name, hits, len(outs), rate, threshold, countWrong(outs))
	slices.Sort(sims)
	if len(sims) == 0 {
		t.Logf("%s: no similarities", name)
		return rate
	}
	q := func(f float64) float64 { return sims[int(math.Round(f*float64(len(sims)-1)))] }
	t.Logf("%s similarity: min %.4f p10 %.4f p25 %.4f p50 %.4f p75 %.4f p90 %.4f max %.4f",
		name, sims[0], q(0.1), q(0.25), q(0.5), q(0.75), q(0.9), sims[len(sims)-1])
	bounds := []float64{0.80, 0.85, 0.90, 0.93, 0.95, 0.97, 0.99}
	var b strings.Builder
	lo := math.Inf(-1)
	for i := 0; i <= len(bounds); i++ {
		hi := math.Inf(1)
		if i < len(bounds) {
			hi = bounds[i]
		}
		n := 0
		for _, s := range sims {
			if s >= lo && s < hi {
				n++
			}
		}
		closing := ")"
		if i == len(bounds) {
			closing = "]"
		}
		fmt.Fprintf(&b, " [%.2f,%.2f%s:%d", max(lo, -1), min(hi, 1), closing, n)
		lo = hi
	}
	t.Logf("%s histogram:%s", name, b.String())

	// The pairs nearest the threshold, for tuning.
	sorted := slices.Clone(outs)
	slices.SortFunc(sorted, func(x, y outcome) int {
		return cmpFloat(math.Abs(x.similarity-threshold), math.Abs(y.similarity-threshold))
	})
	for _, o := range sorted[:min(5, len(sorted))] {
		t.Logf("%s near the threshold: %.4f hit=%v %q / %q", name, o.similarity, o.hit, o.pair.A, o.pair.B)
	}
	return rate
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func countWrong(outs []outcome) int {
	n := 0
	for _, o := range outs {
		if o.wrong {
			n++
		}
	}
	return n
}

// meteredEmbedder records each embedding call's latency and cost.
type meteredEmbedder struct {
	*semantic.Embedder
	latencies []time.Duration
	costNano  int64
	unknown   bool
}

// Embed embeds text through the wrapped Embedder. runSet calls it from one
// goroutine.
func (m *meteredEmbedder) Embed(ctx context.Context, text string) (cache.Embedding, error) {
	start := time.Now()
	e, err := m.Embedder.Embed(ctx, text)
	m.latencies = append(m.latencies, time.Since(start))
	if e.CostNano < 0 {
		m.unknown = true
	} else {
		m.costNano += e.CostNano
	}
	return e, err
}

// report logs the calls' latency against the configured lookup timeout,
// and their total cost.
func (m *meteredEmbedder) report(t *testing.T, lookupTimeout time.Duration) {
	t.Helper()
	if len(m.latencies) == 0 {
		return
	}
	lat := slices.Clone(m.latencies)
	slices.Sort(lat)
	over := 0
	for _, d := range lat {
		if lookupTimeout > 0 && d > lookupTimeout {
			over++
		}
	}
	cost := fmt.Sprintf("$%.6f", float64(m.costNano)/1e9)
	if m.unknown {
		cost += " (some prices unknown)"
	}
	t.Logf("embedding calls: %d, latency p50 %v p95 %v max %v, %d over the %v lookup timeout; cost %s",
		len(lat), lat[len(lat)/2], lat[len(lat)*95/100], lat[len(lat)-1], over, lookupTimeout, cost)
}
