//go:build integration

package semantic

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/redis/go-redis/v9"

	"switchyard/internal/cache"
)

// waitFor bounds the polls for Redis background work (expiry and
// re-indexing). It is generous so a slow machine does not make the tests
// flaky; a passing poll returns as soon as the condition holds.
const waitFor = 10 * time.Second

// integrationClient connects to the real Redis named by
// SWITCHYARD_TEST_REDIS_URL (database 0: the query engine indexes no
// other), with the given protocol, or skips.
func integrationClient(t *testing.T, protocol int) *redis.Client {
	t.Helper()
	url := os.Getenv("SWITCHYARD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("SWITCHYARD_TEST_REDIS_URL is not set")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal("SWITCHYARD_TEST_REDIS_URL is not a valid Redis URL") // never echo it: it may hold a password
	}
	opts.Protocol = protocol
	opts.ContextTimeoutEnabled = true
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("Redis is not reachable: %v", err)
	}
	return client
}

// newTestIndex returns an index of its own (a random model name), dropped
// with its documents when the test ends.
func newTestIndex(t *testing.T, client *redis.Client, dims int) *RedisIndex {
	t.Helper()
	var b [6]byte
	_, _ = rand.Read(b[:])
	ix := NewRedisIndex(client, "test-"+hex.EncodeToString(b[:])+"@itest", dims)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Do(ctx, "FT.DROPINDEX", ix.Name(), "DD").Err()
		// Documents added while the index was dropped are not covered by
		// DD.
		iter := client.Scan(ctx, 0, ix.prefix+"*", 1000).Iterator()
		for iter.Next(ctx) {
			_ = client.Del(ctx, iter.Val()).Err()
		}
	})
	return ix
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	t.Cleanup(cancel)
	return ctx
}

func entry(body string) *cache.Entry {
	return &cache.Entry{
		Body: []byte(body), Deployment: "m@p", Model: "m",
		Created: time.Date(2026, 11, 17, 12, 0, 0, 123, time.UTC),
	}
}

// eventually polls cond until it holds or waitFor passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, waitFor)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRedisIndexIntegration(t *testing.T) {
	for _, protocol := range []int{2, 3} {
		t.Run("resp"+string(rune('0'+protocol)), func(t *testing.T) {
			client := integrationClient(t, protocol)
			testRedisIndex(t, client)
		})
	}
}

func testRedisIndex(t *testing.T, client *redis.Client) {
	t.Run("nearest neighbor", func(t *testing.T) {
		ctx := ctxT(t)
		ix := newTestIndex(t, client, 4)
		if m, err := ix.Search(ctx, "p1", []float32{1, 0, 0, 0}); err != nil || m != nil {
			t.Fatalf("empty index: %+v, %v; want nil, nil", m, err)
		}
		for body, vec := range map[string][]float32{
			"x":  {1, 0, 0, 0},
			"y":  {0, 1, 0, 0},
			"xy": {1, 1, 0, 0},
			"z":  {0, 0, 1, 0},
		} {
			if err := ix.Add(ctx, "p1", vec, entry(body), time.Hour); err != nil {
				t.Fatal(err)
			}
		}
		tests := []struct {
			query    []float32
			wantBody string
			wantSim  float64
		}{
			{[]float32{1, 0.1, 0, 0}, "x", 1 / math.Sqrt(1.01)},
			{[]float32{0.2, 1, 0, 0}, "y", 1 / math.Sqrt(1.04)},
			{[]float32{1, 0.9, 0, 0}, "xy", 1.9 / (math.Sqrt(2) * math.Sqrt(1.81))},
			{[]float32{0, 0, 3, 0}, "z", 1},
		}
		for _, tc := range tests {
			m, err := ix.Search(ctx, "p1", tc.query)
			if err != nil || m == nil {
				t.Fatalf("Search(%v): %+v, %v", tc.query, m, err)
			}
			if string(m.Entry.Body) != tc.wantBody || math.Abs(m.Similarity-tc.wantSim) > 1e-5 {
				t.Errorf("Search(%v) = %q at %.6f, want %q at %.6f", tc.query, m.Entry.Body, m.Similarity, tc.wantBody, tc.wantSim)
			}
		}
		// The entry round-trips.
		m, err := ix.Search(ctx, "p1", []float32{1, 0, 0, 0})
		if err != nil || m == nil {
			t.Fatal(m, err)
		}
		if diff := cmp.Diff(entry("x"), m.Entry); diff != "" {
			t.Errorf("entry (-want +got):\n%s", diff)
		}
	})

	t.Run("partition isolation", func(t *testing.T) {
		ctx := ctxT(t)
		ix := newTestIndex(t, client, 3)
		vec := []float32{0.3, 0.4, 0.5}
		if err := ix.Add(ctx, "tenant-b-partition", vec, entry("other tenant"), time.Hour); err != nil {
			t.Fatal(err)
		}
		// Hex partitions, and prefixes and case variants of one another.
		for _, p := range []string{"tenant-a-partition", "tenant-b", "TENANT-B-PARTITION", "tenant-b-partition-x"} {
			if m, err := ix.Search(ctx, p, vec); err != nil || m != nil {
				t.Errorf("Search(%q) with only another partition's identical vector = %+v, %v; want nil", p, m, err)
			}
		}
		if err := ix.Add(ctx, "tenant-a-partition", []float32{-1, 0, 0}, entry("far"), time.Hour); err != nil {
			t.Fatal(err)
		}
		m, err := ix.Search(ctx, "tenant-a-partition", vec)
		if err != nil || m == nil || string(m.Entry.Body) != "far" {
			t.Fatalf("Search = %+v, %v; want the partition's own far entry", m, err)
		}
		if m.Similarity >= 0 {
			t.Errorf("similarity %v, want negative", m.Similarity)
		}
		// A partition with TAG syntax characters is matched exactly.
		odd := `a{b}|c-d.e:f\g`
		if err := ix.Add(ctx, odd, vec, entry("odd"), time.Hour); err != nil {
			t.Fatal(err)
		}
		if m, err := ix.Search(ctx, odd, vec); err != nil || m == nil || string(m.Entry.Body) != "odd" {
			t.Errorf("Search(odd) = %+v, %v", m, err)
		}
		if m, err := ix.Search(ctx, "a", vec); err != nil || m != nil {
			t.Errorf("Search(a) = %+v, %v; want nil", m, err)
		}
	})

	t.Run("ttl", func(t *testing.T) {
		ctx := ctxT(t)
		ix := newTestIndex(t, client, 2)
		if err := ix.Add(ctx, "p", []float32{1, 0}, entry("short"), 300*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if err := ix.Add(ctx, "p", []float32{0, 1}, entry("long"), time.Hour); err != nil {
			t.Fatal(err)
		}
		keys, err := client.Keys(ctx, ix.prefix+"*").Result()
		if err != nil || len(keys) != 2 {
			t.Fatalf("keys %v, %v", keys, err)
		}
		for _, k := range keys {
			if ttl, err := client.PTTL(ctx, k).Result(); err != nil || ttl <= 0 || ttl > time.Hour {
				t.Errorf("PTTL(%s) = %v, %v; want in (0, 1h]", k, ttl, err)
			}
		}
		eventually(t, "the short entry's expiry", func() bool {
			m, err := ix.Search(ctx, "p", []float32{1, 0})
			if err != nil {
				t.Fatal(err)
			}
			return m != nil && string(m.Entry.Body) == "long"
		})
	})

	t.Run("index re-creation", func(t *testing.T) {
		ctx := ctxT(t)
		ix := newTestIndex(t, client, 2)
		if err := ix.Add(ctx, "p", []float32{1, 0}, entry("kept"), time.Hour); err != nil {
			t.Fatal(err)
		}
		if m, err := ix.Search(ctx, "p", []float32{1, 0}); err != nil || m == nil {
			t.Fatal(m, err)
		}
		// Dropped behind the index's back, keeping the documents.
		if err := client.Do(ctx, "FT.DROPINDEX", ix.Name()).Err(); err != nil {
			t.Fatal(err)
		}
		if _, err := ix.Search(ctx, "p", []float32{1, 0}); err != nil {
			t.Fatalf("Search after the index was dropped: %v", err)
		}
		if err := client.Do(ctx, "FT.INFO", ix.Name()).Err(); err != nil {
			t.Fatalf("index not re-created: %v", err)
		}
		eventually(t, "re-indexing the kept document", func() bool {
			m, err := ix.Search(ctx, "p", []float32{1, 0})
			if err != nil {
				t.Fatal(err)
			}
			return m != nil && string(m.Entry.Body) == "kept"
		})
	})

	t.Run("dimension mismatch", func(t *testing.T) {
		ctx := ctxT(t)
		ix := newTestIndex(t, client, 4)
		if err := ix.Add(ctx, "p", []float32{1, 0, 0}, entry("x"), time.Hour); err == nil {
			t.Error("Add of 3 dimensions to a 4-dimension index succeeded")
		}
		if _, err := ix.Search(ctx, "p", []float32{1, 0, 0, 0, 0}); err == nil {
			t.Error("Search with 5 dimensions in a 4-dimension index succeeded")
		}
		// The same model with another dimension is another index.
		other := NewRedisIndex(client, "m@p", 3)
		if other.Name() == NewRedisIndex(client, "m@p", 4).Name() {
			t.Error("indexes of different dimensions share a name")
		}
	})

	t.Run("concurrent first use", func(t *testing.T) {
		ctx := ctxT(t)
		ix := newTestIndex(t, client, 2)
		var wg sync.WaitGroup
		errs := make(chan error, 20)
		for i := range 20 {
			wg.Go(func() {
				var err error
				if i%2 == 0 {
					_, err = ix.Search(ctx, "p", []float32{1, 1})
				} else {
					err = ix.Add(ctx, "p", []float32{1, float32(i)}, entry("c"), time.Hour)
				}
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Error(err)
			}
		}
	})

	t.Run("deadline", func(t *testing.T) {
		ix := newTestIndex(t, client, 2)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := ix.Search(ctx, "p", []float32{1, 0}); !errors.Is(err, context.Canceled) {
			t.Errorf("Search with a canceled context: %v, want context.Canceled", err)
		}
	})
}
