package cachetest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"switchyard/internal/cache"
	"switchyard/internal/cache/semantic"
)

// The committed sets.
const (
	paraphraseFile = "paraphrase.jsonl"
	nearMissFile   = "nearmiss.jsonl"
)

// Limits every set obeys.
const (
	minPairs    = 40
	maxSetChars = 200
)

// defaultMinSimilarity is semantic.min_similarity's default (DESIGN
// §11.3).
const defaultMinSimilarity = 0.95

// pair is one line of a set.
type pair struct {
	A string `json:"a"`
	B string `json:"b"`
}

// loadPairs reads a set: one JSON object per line, with exactly the
// fields a and b.
func loadPairs(t *testing.T, name string) []pair {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var out []pair
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		dec := json.NewDecoder(strings.NewReader(sc.Text()))
		dec.DisallowUnknownFields()
		var p pair
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("%s:%d: %v", name, n, err)
		}
		out = append(out, p)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSets checks that each set is big enough and well formed: no empty,
// overlong or identical sides, and no string twice, so that a string
// names one question.
func TestSets(t *testing.T) {
	for _, name := range []string{paraphraseFile, nearMissFile} {
		t.Run(name, func(t *testing.T) {
			pairs := loadPairs(t, name)
			if len(pairs) < minPairs {
				t.Errorf("%d pairs, want at least %d", len(pairs), minPairs)
			}
			seen := map[string]int{}
			for i, p := range pairs {
				for _, s := range []string{p.A, p.B} {
					if strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > maxSetChars {
						t.Errorf("pair %d: %q is empty or longer than %d characters", i+1, s, maxSetChars)
					}
					if j, ok := seen[s]; ok {
						t.Errorf("pair %d: %q is already in pair %d", i+1, s, j)
					}
					seen[s] = i + 1
				}
				if strings.EqualFold(p.A, p.B) {
					t.Errorf("pair %d: both sides are %q", i+1, p.A)
				}
			}
		})
	}
}

// pairEmbedder is a cache.Embedder scripted with fixed vectors: pair i's a
// is the unit vector e(2i), and its b the unit vector at cosine sim from
// it towards e(2i+1). Vectors of different pairs are orthogonal.
type pairEmbedder struct {
	vecs map[string][]float32
	dims int
}

func newPairEmbedder(pairs []pair, sim float64) *pairEmbedder {
	e := &pairEmbedder{vecs: map[string][]float32{}, dims: 2 * len(pairs)}
	for i, p := range pairs {
		a := make([]float32, e.dims)
		a[2*i] = 1
		b := make([]float32, e.dims)
		b[2*i], b[2*i+1] = float32(sim), float32(math.Sqrt(1-sim*sim))
		e.vecs[p.A], e.vecs[p.B] = a, b
	}
	return e
}

func (e *pairEmbedder) Embed(_ context.Context, text string) (cache.Embedding, error) {
	v, ok := e.vecs[text]
	if !ok {
		return cache.Embedding{}, os.ErrNotExist
	}
	return cache.Embedding{Vector: slices.Clone(v), Tokens: 1, CostNano: 1}, nil
}

func (e *pairEmbedder) Model() string   { return "scripted@test" }
func (e *pairEmbedder) Dimensions() int { return e.dims }

// outcome is one b's lookup after every a was stored.
type outcome struct {
	pair       pair
	similarity float64
	// hit is a hit on the pair's own a; wrong is a hit on another
	// pair's a.
	hit, wrong bool
}

// runSet stores every pair's a in one partition, as a miss would, then
// looks up every b at threshold. The stored entry's body is its a, so a
// hit tells which entry it hit.
func runSet(t *testing.T, c *semantic.Cache, pairs []pair, threshold float64) []outcome {
	t.Helper()
	ctx := context.Background()
	const partition = "set"
	for _, p := range pairs {
		res := c.Lookup(ctx, partition, p.A, threshold)
		if res.Err != nil {
			t.Fatalf("lookup of %q: %v", p.A, res.Err)
		}
		if err := c.Store(ctx, partition, res.Vector, &cache.Entry{Body: []byte(p.A)}, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	out := make([]outcome, len(pairs))
	for i, p := range pairs {
		res := c.Lookup(ctx, partition, p.B, threshold)
		if res.Err != nil {
			t.Fatalf("lookup of %q: %v", p.B, res.Err)
		}
		out[i] = outcome{pair: p, similarity: res.Similarity}
		if res.Entry != nil {
			out[i].hit = string(res.Entry.Body) == p.A
			out[i].wrong = !out[i].hit
		}
	}
	return out
}

// TestSetsOffline checks the mechanics over the committed sets: with
// paraphrases scripted at similarity 0.97 and near misses at 0.85, every
// paraphrase hits its own entry at the default threshold and no near miss
// hits. It says nothing about a real embedding model: that is the live
// harness's job.
func TestSetsOffline(t *testing.T) {
	tests := []struct {
		file    string
		sim     float64
		wantHit bool
	}{
		{paraphraseFile, 0.97, true},
		{nearMissFile, 0.85, false},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			pairs := loadPairs(t, tc.file)
			c := semantic.New(newPairEmbedder(pairs, tc.sim), semantic.NewMemoryIndex(10*len(pairs), nil),
				semantic.Options{LookupTimeout: 10 * time.Second})
			for _, o := range runSet(t, c, pairs, defaultMinSimilarity) {
				if o.hit != tc.wantHit || o.wrong {
					t.Errorf("%q: hit %v (wrong entry %v) at %.4f, want hit %v", o.pair.B, o.hit, o.wrong, o.similarity, tc.wantHit)
				}
				if math.Abs(o.similarity-tc.sim) > 1e-6 {
					t.Errorf("%q: similarity %.6f, want %.2f", o.pair.B, o.similarity, tc.sim)
				}
			}
		})
	}
}
