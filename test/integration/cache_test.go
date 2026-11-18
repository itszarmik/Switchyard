//go:build integration

package integration

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"testing"
	"time"

	"switchyard/test/scenario"
)

// cacheRedisEnv is the URL of the test project's cache Redis (database
// 0, allkeys-lru, with the query engine), which only the cache uses.
const cacheRedisEnv = "SWITCHYARD_TEST_CACHE_REDIS_URL"

// cacheStorage is the storage section of a gateway with its cache in the
// dedicated cache Redis and its namespace versions and budgets in the
// state Redis; with ledger set, the Postgres ledger at $DATABASE_URL too.
func cacheStorage(t *testing.T, ledger bool) string {
	t.Helper()
	s := fmt.Sprintf(`storage:
  mode: redis
  redis: {cache_url: %q, state_url: %q}`, env(t, cacheRedisEnv), env(t, redisEnv))
	if ledger {
		s += `
  ledger: {driver: postgres, dsn: "${DATABASE_URL}", batch_size: 50, flush_interval: 100ms, queue_size: 10000}`
	}
	return s
}

// TestCacheLedgerPostgres runs the exact-hit ledger scenario with the
// cache in Redis and the Postgres ledger, whose report sums cache
// savings in SQL, then checks the hit's own row.
func TestCacheLedgerPostgres(t *testing.T) {
	s := newSchema(t)
	o := scenario.Options{Storage: cacheStorage(t, true), Env: []string{"DATABASE_URL=" + s.dsn}, Race: raceEnabled}
	scenario.CacheLedger(t, o)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT cache_status, cost_usd::text, cost_known, baseline_cost_usd::text, attempts
FROM usage_events ORDER BY ts, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		status, cost, baseline string
		known                  bool
		attempts               int
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.status, &r.cost, &r.known, &r.baseline, &r.attempts); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d usage rows, want 2: %+v", len(got), got)
	}
	hit := got[1]
	if got[0].status != "miss" || hit.status != "exact" || !hit.known || hit.attempts != 0 ||
		!usdEqual(t, hit.cost, "0") || !usdEqual(t, hit.baseline, "0.002") {
		t.Errorf("rows %+v: want a miss, then an exact hit costing 0 with baseline 0.002 and 0 attempts", got)
	}
	t.Logf("usage_events: %+v", got)
}

// usdEqual reports whether two decimal strings are the same amount.
func usdEqual(t *testing.T, a, b string) bool {
	t.Helper()
	ra, ok1 := new(big.Rat).SetString(a)
	rb, ok2 := new(big.Rat).SetString(b)
	if !ok1 || !ok2 {
		t.Fatalf("not decimals: %q, %q", a, b)
	}
	return ra.Cmp(rb) == 0
}

// TestCachePurgeRedis runs the purge scenario with exact entries and the
// semantic index in the cache Redis, and namespace versions in the state
// Redis.
func TestCachePurgeRedis(t *testing.T) {
	scenario.CachePurge(t, scenario.Options{Storage: cacheStorage(t, false), Race: raceEnabled})
}

// TestCacheRedisStopped stops the cache Redis container mid-run: requests
// keep succeeding as misses, the lookup and write errors are counted, and
// once the container is back the cache works again. Budgets and
// namespace versions are in the state Redis, which stays up.
func TestCacheRedisStopped(t *testing.T) {
	env(t, composeEnv)
	g := scenario.StartCache(t, scenario.Options{Storage: cacheStorage(t, false), Race: raceEnabled}, scenario.CachePolicy)
	chat := func(text string) (string, time.Duration) {
		start := time.Now()
		r := g.Chat(t, g.KeyA, `{"model":"auto:cheap","messages":[{"role":"user","content":"`+text+`"}]}`)
		if r.Status != http.StatusOK {
			t.Fatalf("%q: status %d %s", text, r.Status, r.Body)
		}
		return r.Header.Get("X-Switchyard-Cache"), time.Since(start)
	}
	if got, _ := chat(scenario.Question); got != "miss" {
		t.Fatalf("first: %q", got)
	}
	g.WaitWrites(t, "exact", 1)
	if got, _ := chat(scenario.Question); got != "exact" {
		t.Fatalf("before the stop: %q", got)
	}

	compose(t, "stop", "redis-cache")
	restarted := false
	t.Cleanup(func() {
		if !restarted {
			compose(t, "up", "-d", "--wait", "redis-cache")
		}
	})
	before := g.Metric(t, "switchyard_cache_lookups_total", `layer="exact"`, `result="error"`)
	const n = 20
	var durations []time.Duration
	for i := range n {
		got, d := chat(fmt.Sprintf("while down %d", i))
		if got != "miss" {
			t.Errorf("request %d while the cache Redis is down: %q, want miss", i, got)
		}
		durations = append(durations, d)
	}
	errs := g.Metric(t, "switchyard_cache_lookups_total", `layer="exact"`, `result="error"`) - before
	if errs != n {
		t.Errorf("%v exact lookup errors while down, want %d", errs, n)
	}
	semErrs := g.Metric(t, "switchyard_cache_lookups_total", `layer="semantic"`, `result="error"`)
	slices.Sort(durations)
	// Both layers are on: a lookup may wait for the exact timeout (25ms)
	// and then the semantic one (150ms).
	if slowest := durations[n-1]; slowest > 25*time.Millisecond+150*time.Millisecond+250*time.Millisecond {
		t.Errorf("slowest request %s while the cache Redis is down", slowest)
	}
	t.Logf("cache Redis stopped: %d requests served as misses; %v exact and %v semantic lookup errors counted; latency p50 %s, max %s",
		n, errs, semErrs, durations[n/2], durations[n-1])

	compose(t, "up", "-d", "--wait", "redis-cache")
	restarted = true
	// The entries were not persisted; the cache refills.
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, _ := chat(scenario.Question)
		if got == "exact" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cache did not recover after the restart: %q", got)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("after the restart the exact cache hits again")
}
