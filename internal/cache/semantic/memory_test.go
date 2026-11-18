package semantic

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"switchyard/internal/cache"
)

// fakeClock is a settable clock, safe for concurrent use.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1767225600, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// add is one MemoryIndex.Add.
type add struct {
	partition string
	vec       []float32
	body      string
	ttl       time.Duration
}

// search is one MemoryIndex.Search and what it should return: wantBody
// "" for no match.
type search struct {
	partition string
	vec       []float32
	wantBody  string
	wantSim   float64
	wantErr   bool
}

func TestMemoryIndex(t *testing.T) {
	tests := []struct {
		name    string
		max     int
		adds    []add
		advance time.Duration // after the adds
		// touch is searched between the adds and the late add, to make
		// an entry recently used.
		touch    *search
		lateAdd  *add
		searches []search
		wantLen  int
		wantEvic int64
	}{
		{
			name: "nearest in partition",
			max:  10,
			adds: []add{
				{"p", []float32{1, 0, 0}, "x", time.Hour},
				{"p", []float32{0, 1, 0}, "y", time.Hour},
				{"p", []float32{1, 1, 0}, "xy", time.Hour},
			},
			searches: []search{
				{partition: "p", vec: []float32{1, 0.1, 0}, wantBody: "x", wantSim: 1 / math.Sqrt(1.01)},
				{partition: "p", vec: []float32{2, 1.8, 0}, wantBody: "xy", wantSim: 3.8 / (math.Sqrt(2) * math.Sqrt(7.24))},
				{partition: "q", vec: []float32{1, 0, 0}},
			},
			wantLen: 3,
		},
		{
			name: "partition isolation",
			max:  10,
			adds: []add{
				{"tenant-a", []float32{0.3, 0.4}, "a", time.Hour},
				{"tenant-b", []float32{-1, 0}, "b", time.Hour},
			},
			searches: []search{
				{partition: "tenant-b", vec: []float32{0.3, 0.4}, wantBody: "b", wantSim: -0.6},
				{partition: "tenant-c", vec: []float32{0.3, 0.4}},
				{partition: "tenant", vec: []float32{0.3, 0.4}},
			},
			wantLen: 2,
		},
		{
			name: "ttl",
			max:  10,
			adds: []add{
				{"p", []float32{1, 0}, "short", time.Minute},
				{"p", []float32{0, 1}, "long", time.Hour},
				{"q", []float32{1, 0}, "gone", time.Minute},
			},
			advance: time.Minute,
			searches: []search{
				{partition: "p", vec: []float32{1, 0}, wantBody: "long", wantSim: 0},
				{partition: "q", vec: []float32{1, 0}},
			},
			wantLen: 1,
		},
		{
			name: "lru cap",
			max:  2,
			adds: []add{
				{"p", []float32{1, 0}, "first", time.Hour},
				{"p", []float32{0, 1}, "second", time.Hour},
			},
			// first becomes the most recently used, so second goes.
			touch:   &search{partition: "p", vec: []float32{1, 0}, wantBody: "first", wantSim: 1},
			lateAdd: &add{"q", []float32{1, 1}, "third", time.Hour},
			searches: []search{
				{partition: "p", vec: []float32{0, 1}, wantBody: "first", wantSim: 0},
				{partition: "q", vec: []float32{1, 1}, wantBody: "third", wantSim: 1},
			},
			wantLen:  2,
			wantEvic: 1,
		},
		{
			name: "dimension mismatch",
			max:  10,
			adds: []add{{"p", []float32{1, 0}, "x", time.Hour}},
			searches: []search{
				{partition: "p", vec: []float32{1, 0, 0}, wantErr: true},
				{partition: "q", vec: []float32{1, 0, 0}},
			},
			wantLen: 1,
		},
		{
			name: "invalid queries",
			max:  10,
			searches: []search{
				{partition: "p", vec: []float32{0, 0}, wantErr: true},
				{partition: "p", vec: nil, wantErr: true},
				{partition: "p", vec: []float32{float32(math.NaN())}, wantErr: true},
				{partition: "", vec: []float32{1}, wantErr: true},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			ix := NewMemoryIndex(tc.max, clock.Now)
			ctx := context.Background()
			doAdd := func(a add) {
				t.Helper()
				if err := ix.Add(ctx, a.partition, a.vec, &cache.Entry{Body: []byte(a.body)}, a.ttl); err != nil {
					t.Fatalf("Add(%s): %v", a.body, err)
				}
			}
			doSearch := func(s search) {
				t.Helper()
				m, err := ix.Search(ctx, s.partition, s.vec)
				if (err != nil) != s.wantErr {
					t.Fatalf("Search(%q, %v) error %v, want error %v", s.partition, s.vec, err, s.wantErr)
				}
				switch {
				case s.wantErr:
				case s.wantBody == "":
					if m != nil {
						t.Errorf("Search(%q, %v) = %q, want none", s.partition, s.vec, m.Entry.Body)
					}
				case m == nil:
					t.Errorf("Search(%q, %v) = none, want %q", s.partition, s.vec, s.wantBody)
				case string(m.Entry.Body) != s.wantBody || math.Abs(m.Similarity-s.wantSim) > 1e-6:
					t.Errorf("Search(%q, %v) = %q at %v, want %q at %v", s.partition, s.vec, m.Entry.Body, m.Similarity, s.wantBody, s.wantSim)
				}
			}
			for _, a := range tc.adds {
				doAdd(a)
			}
			clock.Advance(tc.advance)
			if tc.touch != nil {
				doSearch(*tc.touch)
			}
			if tc.lateAdd != nil {
				doAdd(*tc.lateAdd)
			}
			for _, s := range tc.searches {
				doSearch(s)
			}
			if ix.Len() != tc.wantLen || ix.Evictions() != tc.wantEvic {
				t.Errorf("Len %d, Evictions %d; want %d, %d", ix.Len(), ix.Evictions(), tc.wantLen, tc.wantEvic)
			}
		})
	}
}

func TestMemoryIndexAddValidatesAndCopies(t *testing.T) {
	ix := NewMemoryIndex(0, nil) // clamped to 1 entry, real clock
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"nil entry": func() error { return ix.Add(ctx, "p", []float32{1}, nil, time.Hour) },
		"zero ttl":  func() error { return ix.Add(ctx, "p", []float32{1}, &cache.Entry{}, 0) },
		"zero vec":  func() error { return ix.Add(ctx, "p", []float32{0}, &cache.Entry{}, time.Hour) },
		"partition": func() error { return ix.Add(ctx, "a b", []float32{1}, &cache.Entry{}, time.Hour) },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: Add succeeded", name)
		}
	}

	body := []byte("original")
	vec := []float32{1, 0}
	if err := ix.Add(ctx, "p", vec, &cache.Entry{Body: body, Model: "m"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	copy(body, "mutated!")
	vec[0], vec[1] = 0, 1
	m, err := ix.Search(ctx, "p", []float32{1, 0})
	if err != nil || m == nil || string(m.Entry.Body) != "original" || m.Entry.Model != "m" || m.Similarity != 1 {
		t.Errorf("Search = %+v, %v; want the original entry at similarity 1", m, err)
	}
	if err := ix.Add(ctx, "p", []float32{1, 0}, &cache.Entry{Body: []byte("next")}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if ix.Len() != 1 || ix.Evictions() != 1 {
		t.Errorf("Len %d, Evictions %d; want 1, 1 (maxEntries 0 is 1)", ix.Len(), ix.Evictions())
	}
}

// TestMemoryIndexConcurrent exercises the index from many goroutines, for
// the race detector.
func TestMemoryIndexConcurrent(t *testing.T) {
	ix := NewMemoryIndex(50, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			for j := range 100 {
				p := string(rune('a' + (i+j)%4))
				_ = ix.Add(ctx, p, []float32{1, float32(j)}, &cache.Entry{Body: []byte("x")}, time.Hour)
				if _, err := ix.Search(ctx, p, []float32{float32(i), 1}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if ix.Len() != 50 {
		t.Errorf("Len %d, want 50", ix.Len())
	}
}

// TestMemoryIndexLookupDeadline: lookups over one large partition (10,000
// entries of 1,536 dimensions, as a single-turn FAQ bot builds) run
// concurrently, not one after another, and each honors the lookup
// timeout: it returns within the timeout plus a margin, with ErrTimeout
// when its scan did not finish (security review H1).
func TestMemoryIndexLookupDeadline(t *testing.T) {
	const (
		dims, entries, lookups = 1536, 10_000, 50
		timeout                = 150 * time.Millisecond
	)
	// margin covers the last searchCheckEvery vectors and the scheduling
	// of 50 busy goroutines. Serialized, as they were behind one mutex,
	// the last lookup would end after 50 full scans: several times this
	// without the race detector, and tens of seconds with it.
	margin := 100 * time.Millisecond
	if raceEnabled {
		margin = 2 * time.Second
	}
	// Atomic coverage counters (make cover) in the scan's inner loop are
	// shared by all 50 goroutines, which then contend on them for every
	// vector element: the latencies measure that, not the index.
	timed := testing.CoverMode() == ""
	r := rand.New(rand.NewPCG(1, 2))
	vector := func() []float32 {
		v := make([]float32, dims)
		for i := range v {
			v[i] = r.Float32() - 0.5
		}
		return v
	}
	const part = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ix := NewMemoryIndex(entries, nil)
	ctx := context.Background()
	for range entries {
		if err := ix.Add(ctx, part, vector(), &cache.Entry{Body: []byte("answer")}, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	c := New(constEmbedder{vector()}, ix, Options{LookupTimeout: timeout})
	type outcome struct {
		took time.Duration
		err  error
	}
	out := make([]outcome, lookups)
	var wg sync.WaitGroup
	for i := range lookups {
		wg.Go(func() {
			start := time.Now()
			res := c.Lookup(ctx, part, "question", 0.99)
			out[i] = outcome{time.Since(start), res.Err}
		})
	}
	wg.Wait()
	timedOut := 0
	for i, o := range out {
		if timed && o.took > timeout+margin {
			t.Errorf("lookup %d took %s, more than the %s timeout plus %s", i, o.took, timeout, margin)
		}
		switch {
		case o.err == nil:
		case errors.Is(o.err, ErrTimeout) && errors.Is(o.err, context.DeadlineExceeded):
			timedOut++
		default:
			t.Errorf("lookup %d: %v, want nil or ErrTimeout", i, o.err)
		}
	}
	var slowest time.Duration
	for _, o := range out {
		slowest = max(slowest, o.took)
	}
	t.Logf("%d concurrent lookups over %d entries: %d timed out, the slowest took %s", lookups, entries, timedOut, slowest)

	t.Run("a done context stops the scan", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if m, err := ix.Search(ctx, part, vector()); !errors.Is(err, context.Canceled) || m != nil {
			t.Errorf("Search with a canceled context = %v, %v; want context.Canceled", m, err)
		}
	})
}

// constEmbedder embeds every text as v.
type constEmbedder struct{ v []float32 }

func (c constEmbedder) Embed(context.Context, string) (cache.Embedding, error) {
	return cache.Embedding{Vector: c.v, CostNano: 1}, nil
}
func (constEmbedder) Model() string     { return "e@p" }
func (c constEmbedder) Dimensions() int { return len(c.v) }

// TestMemoryIndexByteCap: beyond maxEntries, the index holds at most
// maxBytes (cache.MemoryMaxBytes) of entries, vectors and partitions,
// evicting the least recently used.
func TestMemoryIndexByteCap(t *testing.T) {
	ctx := context.Background()
	body := func(n int) []byte { return make([]byte, n) }
	vec := []float32{1, 0}
	// An entry is its body, the overhead, its 2-float vector and partition.
	size := func(n int) int64 { return (&cache.Entry{Body: body(n), Model: "x"}).Size() + 8 + 2 }
	tests := []struct {
		name     string
		maxBytes int64
		bodies   []int // of entries a, b, c, ... in partition p
		touch    string
		wantKept []string
	}{
		{"under the cap", 3 * size(100), []int{100, 100, 100}, "", []string{"a", "b", "c"}},
		{"the oldest goes first", 2 * size(100), []int{100, 100, 100}, "", []string{"b", "c"}},
		{"a hit is kept", 2 * size(100), []int{100, 100, 100}, "a", []string{"a", "c"}},
		{"a large entry evicts several", 3 * size(100), []int{100, 100, 100, 250}, "", []string{"c", "d"}},
		{"an entry alone over the cap is kept", size(10), []int{10, 500}, "", []string{"b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ix := NewMemoryIndex(1000, newFakeClock().Now)
			if ix.maxBytes != cache.MemoryMaxBytes {
				t.Fatalf("maxBytes %d, want cache.MemoryMaxBytes", ix.maxBytes)
			}
			ix.maxBytes = tt.maxBytes
			for i, n := range tt.bodies {
				if i == len(tt.bodies)-1 && tt.touch != "" {
					// Partition p+name holds only that entry.
					if m, err := ix.Search(ctx, "p"+tt.touch, vec); err != nil || m == nil {
						t.Fatalf("touch: %v, %v", m, err)
					}
				}
				name := string(rune('a' + i))
				if err := ix.Add(ctx, "p"+name, vec, &cache.Entry{Body: body(n), Model: name}, time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			var want int64
			for i, n := range tt.bodies {
				name := string(rune('a' + i))
				m, _ := ix.Search(ctx, "p"+name, vec)
				if kept := slices.Contains(tt.wantKept, name); kept != (m != nil) {
					t.Errorf("entry %s kept %v, want %v", name, m != nil, kept)
				}
				if m != nil {
					want += size(n)
				}
			}
			if got := ix.Bytes(); got != want || ix.Evictions() != int64(len(tt.bodies)-len(tt.wantKept)) {
				t.Errorf("Bytes %d, Evictions %d; want %d, %d", got, ix.Evictions(), want, len(tt.bodies)-len(tt.wantKept))
			}
		})
	}
}
