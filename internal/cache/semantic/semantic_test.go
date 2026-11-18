package semantic_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"switchyard/internal/cache"
	"switchyard/internal/cache/semantic"
)

// scriptedEmbedder embeds the strings of vecs; any other string fails.
type scriptedEmbedder struct {
	vecs map[string][]float32
	cost int64
	// block makes Embed wait for its context to end.
	block bool
	calls atomic.Int64
}

func (e *scriptedEmbedder) Embed(ctx context.Context, text string) (cache.Embedding, error) {
	e.calls.Add(1)
	if e.block {
		<-ctx.Done()
		return cache.Embedding{CostNano: e.cost}, ctx.Err()
	}
	v, ok := e.vecs[text]
	if !ok {
		return cache.Embedding{CostNano: e.cost}, errors.New("scripted embedder: unknown text")
	}
	return cache.Embedding{Vector: v, Tokens: 1, CostNano: e.cost}, nil
}

func (e *scriptedEmbedder) Model() string   { return "scripted@test" }
func (e *scriptedEmbedder) Dimensions() int { return 2 }

// failingIndex fails every call, or blocks until the context ends.
type failingIndex struct{ block bool }

func (ix failingIndex) Search(ctx context.Context, _ string, _ []float32) (*cache.Match, error) {
	if ix.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, errors.New("index down")
}

func (ix failingIndex) Add(context.Context, string, []float32, *cache.Entry, time.Duration) error {
	return errors.New("index down")
}

func TestLookup(t *testing.T) {
	// cos(stored, query): "same" 1, "close" 0.97, "far" 0.85.
	vecs := map[string][]float32{
		"stored": {1, 0},
		"same":   {2, 0},
		"close":  {0.97, float32(math.Sqrt(1 - 0.97*0.97))},
		"far":    {0.85, float32(math.Sqrt(1 - 0.85*0.85))},
	}
	tests := []struct {
		name      string
		emb       *scriptedEmbedder
		idx       cache.VectorIndex // nil: a memory index holding "stored" in p
		partition string
		text      string
		min       float64
		timeout   time.Duration
		wantHit   bool
		wantSim   float64 // NaN: none
		wantVec   bool
		wantErr   string
		wantTO    bool
	}{
		{name: "identical", partition: "p", text: "same", min: 0.95, wantHit: true, wantSim: 1, wantVec: true},
		{name: "paraphrase", partition: "p", text: "close", min: 0.95, wantHit: true, wantSim: 0.97, wantVec: true},
		{name: "near miss", partition: "p", text: "far", min: 0.95, wantSim: 0.85, wantVec: true},
		{name: "threshold inclusive", partition: "p", text: "same", min: 1, wantHit: true, wantSim: 1, wantVec: true},
		{name: "other partition", partition: "q", text: "same", min: 0.5, wantSim: math.NaN(), wantVec: true},
		{name: "embedding fails", partition: "p", text: "unknown", min: 0.5, wantSim: math.NaN(), wantErr: "embedding"},
		{
			name: "search fails", idx: failingIndex{}, partition: "p", text: "same", min: 0.5,
			wantSim: math.NaN(), wantVec: true, wantErr: "index down",
		},
		{
			name: "embedding times out", emb: &scriptedEmbedder{block: true, cost: 7}, partition: "p", text: "same",
			min: 0.5, timeout: 20 * time.Millisecond, wantSim: math.NaN(), wantErr: "embedding", wantTO: true,
		},
		{
			name: "search times out", idx: failingIndex{block: true}, partition: "p", text: "same",
			min: 0.5, timeout: 20 * time.Millisecond, wantSim: math.NaN(), wantVec: true, wantErr: "search", wantTO: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			emb := tc.emb
			if emb == nil {
				emb = &scriptedEmbedder{vecs: vecs, cost: 7}
			}
			idx := tc.idx
			if idx == nil {
				mem := semantic.NewMemoryIndex(10, nil)
				if err := mem.Add(context.Background(), "p", vecs["stored"], &cache.Entry{Body: []byte("cached")}, time.Hour); err != nil {
					t.Fatal(err)
				}
				idx = mem
			}
			c := semantic.New(emb, idx, semantic.Options{LookupTimeout: tc.timeout})
			res := c.Lookup(context.Background(), tc.partition, tc.text, tc.min)

			if hit := res.Entry != nil; hit != tc.wantHit {
				t.Errorf("hit %v, want %v", hit, tc.wantHit)
			}
			if tc.wantHit && string(res.Entry.Body) != "cached" {
				t.Errorf("entry %q", res.Entry.Body)
			}
			if math.IsNaN(tc.wantSim) != math.IsNaN(res.Similarity) ||
				!math.IsNaN(tc.wantSim) && math.Abs(res.Similarity-tc.wantSim) > 1e-6 {
				t.Errorf("similarity %v, want %v", res.Similarity, tc.wantSim)
			}
			if (res.Vector != nil) != tc.wantVec {
				t.Errorf("vector %v, want one: %v", res.Vector, tc.wantVec)
			}
			if res.EmbedCostNano != 7 {
				t.Errorf("embed cost %d, want 7 (charged on every outcome)", res.EmbedCostNano)
			}
			if tc.wantErr == "" {
				if res.Err != nil {
					t.Errorf("error %v", res.Err)
				}
			} else if res.Err == nil || !strings.Contains(res.Err.Error(), tc.wantErr) {
				t.Errorf("error %v, want one containing %q", res.Err, tc.wantErr)
			}
			if got := errors.Is(res.Err, semantic.ErrTimeout); got != tc.wantTO {
				t.Errorf("errors.Is(ErrTimeout) = %v, want %v (error %v)", got, tc.wantTO, res.Err)
			}
		})
	}
}

// TestLookupCallerCanceled checks that the caller's own cancellation is a
// failure but not a lookup timeout.
func TestLookupCallerCanceled(t *testing.T) {
	c := semantic.New(&scriptedEmbedder{block: true}, semantic.NewMemoryIndex(1, nil), semantic.Options{LookupTimeout: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := c.Lookup(ctx, "p", "x", 0.5)
	if res.Err == nil || errors.Is(res.Err, semantic.ErrTimeout) || !errors.Is(res.Err, context.Canceled) {
		t.Errorf("error %v, want context.Canceled and not ErrTimeout", res.Err)
	}
}

func TestStore(t *testing.T) {
	emb := &scriptedEmbedder{vecs: map[string][]float32{"q": {0.6, 0.8}}}
	idx := semantic.NewMemoryIndex(10, nil)
	created := time.Unix(1767225600, 0)
	c := semantic.New(emb, idx, semantic.Options{Clock: func() time.Time { return created }})
	ctx := context.Background()

	miss := c.Lookup(ctx, "p", "q", 0.95)
	if miss.Entry != nil || miss.Err != nil || miss.Vector == nil {
		t.Fatalf("first lookup %+v, want a clean miss with a vector", miss)
	}
	e := &cache.Entry{Body: []byte("answer"), Deployment: "m@p", Model: "m"}
	if err := c.Store(ctx, "p", miss.Vector, e, time.Hour); err != nil {
		t.Fatal(err)
	}
	if !e.Created.IsZero() {
		t.Error("Store modified the caller's entry")
	}
	if emb.calls.Load() != 1 {
		t.Errorf("%d embeddings, want 1: Store reuses the lookup's vector", emb.calls.Load())
	}
	hit := c.Lookup(ctx, "p", "q", 0.95)
	if hit.Entry == nil || string(hit.Entry.Body) != "answer" || !hit.Entry.Created.Equal(created) || hit.Entry.Deployment != "m@p" {
		t.Errorf("second lookup %+v, want the stored entry stamped %v", hit.Entry, created)
	}

	stamped := &cache.Entry{Body: []byte("x"), Created: created.Add(time.Minute)}
	if err := c.Store(ctx, "p2", miss.Vector, stamped, time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := c.Lookup(ctx, "p2", "q", 0.95); got.Entry == nil || !got.Entry.Created.Equal(created.Add(time.Minute)) {
		t.Errorf("an entry's own Created was not kept: %+v", got.Entry)
	}

	for name, call := range map[string]func() error{
		"nil vector": func() error { return c.Store(ctx, "p", nil, e, time.Hour) },
		"nil entry":  func() error { return c.Store(ctx, "p", miss.Vector, nil, time.Hour) },
		"index failed": func() error {
			return semantic.New(emb, failingIndex{}, semantic.Options{}).Store(ctx, "p", miss.Vector, e, time.Hour)
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: Store succeeded", name)
		}
	}
}

// TestCacheOverFakeUpstream runs the cache end to end: the real embedder
// over the OpenAI adapter and the fake upstream, and the memory index.
func TestCacheOverFakeUpstream(t *testing.T) {
	vecs := map[string][]float32{
		"What is the capital of France?":   {1, 0, 0},
		"What's the capital of France?":    {0.99, 0.141, 0},
		"What is the capital of Germany?":  {0.85, 0, 0.527},
		"Completely unrelated question...": {0, 0, 1},
	}
	env := newTestEnv(t, func(s string) []float32 { return vecs[s] })
	c := semantic.New(env.embedder(t, "emb@p1", testTimeout), semantic.NewMemoryIndex(100, nil), semantic.Options{LookupTimeout: testTimeout})
	ctx := context.Background()

	first := c.Lookup(ctx, "part", "What is the capital of France?", 0.95)
	if first.Err != nil || first.Entry != nil || !math.IsNaN(first.Similarity) {
		t.Fatalf("first lookup %+v, want a clean miss", first)
	}
	if first.EmbedCostNano != 8*50 { // 30 bytes: 8 tokens at 50 nano-USD
		t.Errorf("embed cost %d, want %d", first.EmbedCostNano, 8*50)
	}
	if err := c.Store(ctx, "part", first.Vector, &cache.Entry{Body: []byte("Paris")}, time.Hour); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		text    string
		wantHit bool
	}{
		{"What is the capital of France?", true},
		{"What's the capital of France?", true},
		{"What is the capital of Germany?", false},
		{"Completely unrelated question...", false},
	}
	for _, tc := range tests {
		res := c.Lookup(ctx, "part", tc.text, 0.95)
		if res.Err != nil || (res.Entry != nil) != tc.wantHit {
			t.Errorf("Lookup(%q) = hit %v at %.3f (%v), want hit %v", tc.text, res.Entry != nil, res.Similarity, res.Err, tc.wantHit)
		}
	}
	if got := len(env.up1.Requests()); got != 1+len(tests) {
		t.Errorf("%d embedding calls, want %d", got, 1+len(tests))
	}
}
