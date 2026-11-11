package usage

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

var day0 = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

// handEvents is a small data set whose aggregates are computed by hand in
// TestMemoryLedgerHandComputed.
func handEvents(t testing.TB) []*Event {
	m := func(s string) Money { return mustMoney(t, s) }
	return []*Event{
		{ // e1: a plain priced request.
			ID: "e1", Time: day0, Tenant: "acme", KeyID: "k1", Endpoint: "/v1/chat/completions",
			RequestedModel: "gpt", Deployment: "gpt@openai", Provider: "openai", Status: 200, Attempts: 1,
			CacheStatus: CacheStatusMiss, Tokens: Tokens{Input: 1234, Output: 567}, UsageKnown: true,
			Cost: m("0.0005253"), CostKnown: true, Pricing: pricing(t, "0.15", "0.6", "", ""),
			BaselineModel: "gpt", BaselineCost: m("0.0005253"),
			Tags: map[string]string{"team": "search"},
		},
		{ // e2: fell back to a pricier provider, with overhead.
			ID: "e2", Time: day0.Add(time.Hour), Tenant: "acme", KeyID: "k2", Endpoint: "/v1/chat/completions",
			RequestedModel: "gpt", Deployment: "gpt@azure", Provider: "azure", Status: 200, Attempts: 2,
			FallbackReason: "server_error", CacheStatus: CacheStatusMiss,
			Tokens: Tokens{Input: 100, CachedInput: 50, CacheWrite: 25, Output: 10}, UsageKnown: true,
			Cost: m("0.001"), CostKnown: true, BaselineModel: "gpt", BaselineCost: m("0.0008"), OverheadCost: m("0.00001"),
			Tags: map[string]string{"team": "ads", "env": "prod"},
		},
		{ // e3: an embedding request that failed upstream.
			ID: "e3", Time: day0.Add(24 * time.Hour), Tenant: "acme", KeyID: "k1", Endpoint: "/v1/embeddings",
			RequestedModel: "emb", Deployment: "emb@openai", Provider: "openai", Status: 502, Attempts: 3,
			CacheStatus: CacheStatusMiss, CostKnown: true, BaselineModel: "emb",
		},
		{ // e4: unknown pricing.
			ID: "e4", Time: day0.Add(25 * time.Hour), Tenant: "acme", KeyID: "k1", Endpoint: "/v1/chat/completions",
			RequestedModel: "mystery", Deployment: "mystery@local", Provider: "local", Status: 200, Attempts: 1,
			CacheStatus: CacheStatusMiss, Tokens: Tokens{Input: 7, Output: 3}, UsageEstimated: true,
		},
		{ // e5: another tenant.
			ID: "e5", Time: day0, Tenant: "globex", KeyID: "kg", Endpoint: "/v1/chat/completions",
			RequestedModel: "gpt", Deployment: "gpt@openai", Provider: "openai", Status: 200, Attempts: 1,
			CacheStatus: CacheStatusMiss, Tokens: Tokens{Input: 1_000_000}, UsageKnown: true,
			Cost: m("5"), CostKnown: true, BaselineModel: "gpt", BaselineCost: m("6"),
			Tags: map[string]string{"team": "search"},
		},
		{ // e6: a cache hit.
			ID: "e6", Time: day0.Add(26 * time.Hour), Tenant: "acme", KeyID: "k2", Endpoint: "/v1/chat/completions",
			RequestedModel: "gpt", Status: 200, CacheStatus: CacheStatusHit, Tokens: Tokens{Input: 10, Output: 20},
			UsageKnown: true, CostKnown: true, BaselineModel: "gpt", BaselineCost: m("0.002"),
		},
		{ // e7: outside the queried range.
			ID: "e7", Time: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), Tenant: "acme", KeyID: "k1",
			Endpoint: "/v1/chat/completions", RequestedModel: "gpt", Status: 200,
			CacheStatus: CacheStatusMiss, Cost: m("100"), CostKnown: true, BaselineCost: m("100"),
		},
		{ // e8: exactly at the exclusive end of the range.
			ID: "e8", Time: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC), Tenant: "acme", KeyID: "k1",
			Endpoint: "/v1/chat/completions", RequestedModel: "gpt", Status: 200,
			CacheStatus: CacheStatusMiss, Cost: m("100"), CostKnown: true, BaselineCost: m("100"),
		},
	}
}

var (
	handFrom = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	handTo   = time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
)

func newMemoryWith(events []*Event) *MemoryLedger {
	l := NewMemoryLedger()
	for _, ev := range events {
		l.Record(ev)
	}
	return l
}

func TestMemoryLedgerHandComputed(t *testing.T) {
	checkHandComputed(t, newMemoryWith(handEvents(t)))
}

// checkHandComputed checks rep's report on exactly the handEvents against
// hand-computed values.
func checkHandComputed(t *testing.T, rep Reporter) {
	t.Helper()
	r, err := rep.Report(context.Background(), Query{Tenant: "acme", From: handFrom, To: handTo, GroupBy: []string{GroupByModel}})
	if err != nil {
		t.Fatal(err)
	}
	m := func(s string) Money { return mustMoney(t, s) }
	// acme in range: e1, e2, e3, e4, e6.
	want := Totals{
		Requests: 5, Errors: 1, UnpricedRequests: 1,
		Tokens:  Tokens{Input: 1234 + 100 + 7 + 10, CachedInput: 50, CacheWrite: 25, Output: 567 + 10 + 3 + 20},
		CostUSD: m("0.0015353"), // 0.0005253 + 0.001 + overhead 0.00001
		// 0.0005253 + 0.0008 + 0 + 0.002; e4 is unpriced and left out.
		BaselineCostUSD: m("0.0033253"),
		OverheadUSD:     m("0.00001"),
		CacheHitRate:    0.2,
		FallbackRate:    0.2,
	}
	want.SavingsUSD.Routing = m("-0.0002") // 0 + (0.0008 - 0.001) + 0
	want.SavingsUSD.Cache = m("0.002")
	want.SavingsUSD.Net = m("0.00179") // = baseline - cost
	if diff := cmp.Diff(want, r.Totals); diff != "" {
		t.Errorf("totals (-want +got):\n%s", diff)
	}
	if !r.Totals.SavingsUSD.Net.Equal(r.Totals.BaselineCostUSD.Sub(r.Totals.CostUSD)) {
		t.Errorf("net savings %s != baseline - cost", r.Totals.SavingsUSD.Net)
	}

	// gpt costs the most; emb and mystery tie at 0 and sort by name.
	var got []string
	for _, g := range r.Groups {
		got = append(got, fmt.Sprintf("%s:%d:%s", g.Key[GroupByModel], g.Totals.Requests, g.Totals.CostUSD))
	}
	if diff := cmp.Diff([]string{"gpt:3:0.0015353", "emb:1:0", "mystery:1:0"}, got); diff != "" {
		t.Errorf("groups (-want +got):\n%s", diff)
	}
	if !r.From.Equal(handFrom) || !r.To.Equal(handTo) || !slices.Equal(r.GroupBy, []string{GroupByModel}) {
		t.Errorf("report echo = %v %v %v", r.From, r.To, r.GroupBy)
	}
}

func TestMemoryLedgerEveryGroupBy(t *testing.T) {
	l := newMemoryWith(handEvents(t))
	tests := []struct {
		groupBy string
		want    map[string]int64 // group value -> requests, for acme in range
	}{
		{GroupByDay, map[string]int64{"2026-11-03": 2, "2026-11-04": 3}},
		{GroupByModel, map[string]int64{"gpt": 3, "emb": 1, "mystery": 1}},
		{GroupByDeployment, map[string]int64{"gpt@openai": 1, "gpt@azure": 1, "emb@openai": 1, "mystery@local": 1, "": 1}},
		{GroupByProvider, map[string]int64{"openai": 2, "azure": 1, "local": 1, "": 1}},
		{GroupByKey, map[string]int64{"k1": 3, "k2": 2}},
		{GroupByEndpoint, map[string]int64{"/v1/chat/completions": 4, "/v1/embeddings": 1}},
		{GroupByStatusClass, map[string]int64{"2xx": 4, "5xx": 1}},
		{GroupByCacheStatus, map[string]int64{"miss": 4, "hit": 1}},
		{"tag:team", map[string]int64{"search": 1, "ads": 1, "": 3}},
		{"tag:env", map[string]int64{"prod": 1, "": 4}},
	}
	for _, tt := range tests {
		t.Run(tt.groupBy, func(t *testing.T) {
			r, err := l.Report(context.Background(), Query{Tenant: "acme", From: handFrom, To: handTo, GroupBy: []string{tt.groupBy}})
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]int64{}
			for _, g := range r.Groups {
				if len(g.Key) != 1 {
					t.Errorf("group key %v has %d entries", g.Key, len(g.Key))
				}
				got[g.Key[tt.groupBy]] = g.Totals.Requests
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("requests by %s (-want +got):\n%s", tt.groupBy, diff)
			}
			checkGroupsSumToTotals(t, r)
		})
	}

	t.Run("two dimensions", func(t *testing.T) {
		r, err := l.Report(context.Background(), Query{Tenant: "acme", From: handFrom, To: handTo, GroupBy: []string{GroupByKey, GroupByDay}})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int64{}
		for _, g := range r.Groups {
			got[g.Key[GroupByKey]+"/"+g.Key[GroupByDay]] = g.Totals.Requests
		}
		want := map[string]int64{"k1/2026-11-03": 1, "k2/2026-11-03": 1, "k1/2026-11-04": 2, "k2/2026-11-04": 1}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("(-want +got):\n%s", diff)
		}
		checkGroupsSumToTotals(t, r)
	})
}

// checkGroupsSumToTotals checks that, without a limit cutting groups off,
// the groups' totals add up to the report's.
func checkGroupsSumToTotals(t *testing.T, r *Report) {
	t.Helper()
	var requests int64
	var cost Money
	for _, g := range r.Groups {
		requests += g.Totals.Requests
		cost = cost.Add(g.Totals.CostUSD)
	}
	if requests != r.Totals.Requests || !cost.Equal(r.Totals.CostUSD) {
		t.Errorf("groups sum to %d requests and %s, totals are %d and %s", requests, cost, r.Totals.Requests, r.Totals.CostUSD)
	}
}

func TestMemoryLedgerTenantIsolation(t *testing.T) {
	events := handEvents(t)
	l := newMemoryWith(events)
	dims := []string{GroupByDay, GroupByModel, GroupByDeployment, GroupByProvider, GroupByKey, GroupByEndpoint,
		GroupByStatusClass, GroupByCacheStatus, "tag:team"}
	for _, tenant := range []string{"acme", "globex", "nobody"} {
		for _, g := range append([]string{""}, dims...) {
			t.Run(tenant+"/"+g, func(t *testing.T) {
				q := Query{Tenant: tenant, From: handFrom, To: handTo}
				if g != "" {
					q.GroupBy = []string{g}
				}
				r, err := l.Report(context.Background(), q)
				if err != nil {
					t.Fatal(err)
				}
				var want accumulator
				for _, ev := range events {
					if ev.Tenant == tenant && !ev.Time.Before(handFrom) && ev.Time.Before(handTo) {
						want.add(ev)
					}
				}
				if diff := cmp.Diff(want.totals(), r.Totals); diff != "" {
					t.Errorf("totals include another tenant's events (-want +got):\n%s", diff)
				}
				if g != "" {
					checkGroupsSumToTotals(t, r)
				}
				if g == GroupByKey {
					for _, grp := range r.Groups {
						if (tenant == "acme") != (grp.Key[GroupByKey] != "kg") {
							t.Errorf("tenant %s sees key %s", tenant, grp.Key[GroupByKey])
						}
					}
				}
			})
		}
	}

	r, err := l.Report(context.Background(), Query{From: handFrom, To: handTo})
	if err != nil {
		t.Fatal(err)
	}
	if r.Totals.Requests != 6 {
		t.Errorf("all tenants: %d requests, want 6", r.Totals.Requests)
	}
}

func TestMemoryLedgerLimitAndOrder(t *testing.T) {
	l := NewMemoryLedger()
	for i := range 20 {
		l.Record(&Event{
			ID: strconv.Itoa(i), Time: day0, Tenant: "t", RequestedModel: fmt.Sprintf("m%02d", i), Status: 200,
			// Costs 0, 1, 2, 3, 4 repeating: several groups tie.
			Cost: mustMoney(t, strconv.Itoa(i%5)), CostKnown: true,
		})
	}
	r, err := l.Report(context.Background(), Query{Tenant: "t", From: handFrom, To: handTo, GroupBy: []string{GroupByModel}, Limit: 6})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range r.Groups {
		got = append(got, g.Key[GroupByModel]+"="+g.Totals.CostUSD.String())
	}
	want := []string{"m04=4", "m09=4", "m14=4", "m19=4", "m03=3", "m08=3"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("groups (-want +got):\n%s", diff)
	}
	if r.Totals.Requests != 20 {
		t.Errorf("totals cover %d requests, want all 20", r.Totals.Requests)
	}
}

func TestMemoryLedgerRetention(t *testing.T) {
	l := NewMemoryLedger(WithMaxEvents(3))
	for i := range 5 {
		l.Record(&Event{ID: strconv.Itoa(i), Time: day0, Tenant: "t", Cost: mustMoney(t, strconv.Itoa(1<<i)), CostKnown: true})
	}
	l.Record(nil)
	r, err := l.Report(context.Background(), Query{Tenant: "t", From: handFrom, To: handTo})
	if err != nil {
		t.Fatal(err)
	}
	// Events 0 and 1 (1 and 2 USD) were forgotten: 4 + 8 + 16.
	if r.Totals.Requests != 3 || r.Totals.CostUSD.String() != "28" {
		t.Errorf("got %d requests costing %s, want 3 costing 28", r.Totals.Requests, r.Totals.CostUSD)
	}
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.Record(&Event{ID: "late", Time: day0, Tenant: "t"})
	r, err = l.Report(context.Background(), Query{Tenant: "t", From: handFrom, To: handTo})
	if err != nil {
		t.Fatal(err)
	}
	if r.Totals.Requests != 3 {
		t.Errorf("an event recorded after Close was kept")
	}
}

func TestReportInvalidQuery(t *testing.T) {
	l := NewMemoryLedger()
	valid := Query{From: handFrom, To: handTo}
	tests := []struct {
		name string
		q    func(q Query) Query
	}{
		{"no from", func(q Query) Query { q.From = time.Time{}; return q }},
		{"no to", func(q Query) Query { q.To = time.Time{}; return q }},
		{"empty range", func(q Query) Query { q.To = q.From; return q }},
		{"reversed range", func(q Query) Query { q.From, q.To = q.To, q.From; return q }},
		{"negative limit", func(q Query) Query { q.Limit = -1; return q }},
		{"unknown dimension", func(q Query) Query { q.GroupBy = []string{"tenant"}; return q }},
		{"repeated dimension", func(q Query) Query { q.GroupBy = []string{GroupByDay, GroupByDay}; return q }},
		{"empty tag", func(q Query) Query { q.GroupBy = []string{"tag:"}; return q }},
		{"bad tag key", func(q Query) Query { q.GroupBy = []string{"tag:Team"}; return q }},
		{"long tag key", func(q Query) Query { q.GroupBy = []string{"tag:" + strings.Repeat("a", 33)}; return q }},
		{"nine dimensions", func(q Query) Query {
			q.GroupBy = []string{GroupByDay, GroupByModel, GroupByDeployment, GroupByProvider, GroupByKey,
				GroupByEndpoint, GroupByStatusClass, GroupByCacheStatus, "tag:a"}
			return q
		}},
		{"four tag dimensions", func(q Query) Query { q.GroupBy = []string{"tag:a", "tag:b", "tag:c", "tag:d"}; return q }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := l.Report(context.Background(), tt.q(valid))
			if !errors.Is(err, ErrInvalidQuery) {
				t.Errorf("err = %v, want ErrInvalidQuery", err)
			}
		})
	}
	if _, err := l.Report(context.Background(), valid); err != nil {
		t.Errorf("valid query: %v", err)
	}
	// The most a query may ask for: 8 dimensions, 3 of them tags.
	widest := valid
	widest.GroupBy = []string{GroupByDay, GroupByModel, GroupByDeployment, GroupByProvider, GroupByKey, "tag:a", "tag:b", "tag:c"}
	if _, err := l.Report(context.Background(), widest); err != nil {
		t.Errorf("8 dimensions with 3 tags: %v", err)
	}
}

// TestMemoryReportManyDims checks that a group_by of thousands of tag
// dimensions, which used to hold the ledger's lock for as long as it took
// to aggregate them, is refused before any work.
func TestMemoryReportManyDims(t *testing.T) {
	l := NewMemoryLedger()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for i := range 10_000 {
		l.Record(&Event{ID: fmt.Sprint(i), Time: now, Tenant: "attacker", Tags: map[string]string{"a": "1"}})
	}
	var dims []string
	for i := 0; len(strings.Join(dims, ",")) < 60000; i++ {
		dims = append(dims, fmt.Sprintf("tag:k%d", i))
	}
	_, err := l.Report(context.Background(), Query{Tenant: "attacker", From: now.Add(-time.Hour), To: now.Add(time.Hour), GroupBy: dims})
	if !errors.Is(err, ErrInvalidQuery) || strings.Contains(err.Error(), "k0") {
		t.Errorf("report over %d tag dimensions: err = %v, want ErrInvalidQuery without the input", len(dims), err)
	}
}

// blockingCtx is a context whose Err blocks, from its second call on,
// until release is closed, after closing entered once. Report checks its
// context once before collecting events and again while aggregating them,
// so it holds Report in the aggregation.
type blockingCtx struct {
	context.Context
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (c *blockingCtx) Err() error {
	if c.calls.Add(1) == 2 {
		close(c.entered)
		<-c.release
	}
	return c.Context.Err()
}

// TestMemoryReportDoesNotBlockRecord checks that Record goes ahead while a
// report aggregates: the ledger's lock is held only to collect the
// matching events.
func TestMemoryReportDoesNotBlockRecord(t *testing.T) {
	l := NewMemoryLedger()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for i := range 5000 {
		l.Record(&Event{ID: fmt.Sprint(i), Time: now, Tenant: "a", KeyID: strconv.Itoa(i % 7)})
	}
	ctx := &blockingCtx{Context: context.Background(), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan *Report, 1)
	go func() {
		r, err := l.Report(ctx, Query{Tenant: "a", From: now.Add(-time.Hour), To: now.Add(time.Hour), GroupBy: []string{GroupByKey}})
		if err != nil {
			t.Errorf("report: %v", err)
		}
		done <- r
	}()
	select {
	case <-ctx.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the report never checked its context while aggregating")
	}
	recorded := make(chan struct{})
	go func() {
		l.Record(&Event{ID: "victim", Time: now, Tenant: "b"})
		close(recorded)
	}()
	select {
	case <-recorded:
	case <-time.After(10 * time.Second):
		t.Fatal("Record blocked while a report aggregated")
	}
	close(ctx.release)
	if r := <-done; r == nil || r.Totals.Requests != 5000 {
		t.Errorf("report = %+v, want 5000 requests", r)
	}
}

// TestMemoryReportCanceled checks that a report stops once its context is
// done, while it aggregates.
func TestMemoryReportCanceled(t *testing.T) {
	l := NewMemoryLedger()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for i := range 3000 {
		l.Record(&Event{ID: fmt.Sprint(i), Time: now, Tenant: "a"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	bctx := &blockingCtx{Context: ctx, entered: make(chan struct{}), release: make(chan struct{})}
	go func() {
		<-bctx.entered
		cancel()
		close(bctx.release)
	}()
	_, err := l.Report(bctx, Query{Tenant: "a", From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestValidTagKey(t *testing.T) {
	for s, want := range map[string]bool{
		"team": true, "a": true, "0": true, "a.b-c_d": true, "abcdefghijklmnopqrstuvwxyz012345": true,
		"": false, "_a": false, ".a": false, "-a": false, "A": false, "a b": false, "a=b": false, "é": false,
		"abcdefghijklmnopqrstuvwxyz0123456": false,
	} {
		if got := ValidTagKey(s); got != want {
			t.Errorf("ValidTagKey(%q) = %v, want %v", s, got, want)
		}
	}
}

// randomEvents returns n deterministic pseudo-random events spread over
// three tenants and three months, with amounts at full femto precision.
func randomEvents(t testing.TB, seed uint64, n int) []*Event {
	rng := rand.New(rand.NewPCG(seed, 1))
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	pick := func(xs ...string) string { return xs[rng.IntN(len(xs))] }
	money := func() Money {
		// Up to 3 integer digits and 15 decimals.
		return mustMoney(t, fmt.Sprintf("%d.%015d", rng.IntN(1000), rng.Int64N(1_000_000_000_000_000)))
	}
	events := make([]*Event, n)
	for i := range events {
		// Times are whole microseconds, as Postgres keeps them.
		ev := &Event{
			ID:             fmt.Sprintf("019a0000-0000-7000-8000-%012d", i),
			Time:           start.Add(time.Duration(rng.Int64N(int64(90 * 24 * time.Hour)))).Truncate(time.Microsecond),
			Tenant:         pick("acme", "globex", "initech"),
			KeyID:          pick("k1", "k2", "k3"),
			Endpoint:       pick("/v1/chat/completions", "/v1/completions", "/v1/embeddings"),
			RequestedModel: pick("gpt", "claude", "emb", "Zeta", "zeta"),
			Provider:       pick("openai", "anthropic", ""),
			Stream:         rng.IntN(2) == 0,
			Status:         []int{200, 200, 200, 400, 429, 499, 500, 502, 0}[rng.IntN(9)],
			Attempts:       rng.IntN(4),
			CacheStatus:    pick(CacheStatusMiss, CacheStatusMiss, CacheStatusHit),
			Tokens: Tokens{
				Input: rng.Int64N(100_000), CachedInput: rng.Int64N(1000), CacheWrite: rng.Int64N(100), Output: rng.Int64N(10_000),
			},
			UsageKnown:    rng.IntN(5) != 0,
			CostKnown:     rng.IntN(7) != 0,
			BaselineModel: "gpt",
			LatencyMS:     rng.Float64() * 1000,
			ConfigVersion: "v1",
		}
		if ev.Provider != "" {
			ev.Deployment = ev.RequestedModel + "@" + ev.Provider
		}
		if rng.IntN(10) == 0 {
			ev.FallbackReason = "server_error"
		}
		if ev.CostKnown {
			ev.Cost, ev.BaselineCost = money(), money()
		}
		if rng.IntN(4) == 0 {
			ev.OverheadCost = money()
		}
		switch rng.IntN(3) {
		case 0:
			ev.Tags = map[string]string{"team": pick("search", "ads", "Search")}
		case 1:
			ev.Tags = map[string]string{"team": pick("search", "ads"), "env": pick("prod", "dev")}
		}
		events[i] = ev
	}
	return events
}

// reportQueries are the queries the memory and Postgres ledgers must
// answer identically.
func reportQueries() []Query {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	dims := []string{GroupByDay, GroupByModel, GroupByDeployment, GroupByProvider, GroupByKey, GroupByEndpoint,
		GroupByStatusClass, GroupByCacheStatus, "tag:team", "tag:env"}
	var qs []Query
	for _, tenant := range []string{"acme", "globex", "nobody", ""} {
		qs = append(qs, Query{Tenant: tenant, From: from, To: to})
		for _, d := range dims {
			qs = append(qs, Query{Tenant: tenant, From: from, To: to, GroupBy: []string{d}})
		}
		qs = append(qs,
			Query{Tenant: tenant, From: from, To: to, GroupBy: []string{GroupByModel, GroupByDay}, Limit: 25},
			Query{Tenant: tenant, From: from, To: to, GroupBy: []string{"tag:team", GroupByProvider, GroupByStatusClass}},
			// A narrower window, inside one month and across a month boundary.
			Query{Tenant: tenant, From: from.AddDate(0, 0, 3), To: from.AddDate(0, 0, 10), GroupBy: []string{GroupByKey}},
			Query{Tenant: tenant, From: from.AddDate(0, 0, 25), To: from.AddDate(0, 1, 5), GroupBy: []string{GroupByDay}},
		)
	}
	return qs
}

// checkTotalsAgainstEvents recomputes r's totals directly from events,
// with an independent loop, and compares the money sums exactly.
func checkTotalsAgainstEvents(t *testing.T, q Query, r *Report, events []*Event) {
	t.Helper()
	var n int64
	var cost, baseline, overhead Money
	for _, ev := range events {
		if (q.Tenant != "" && ev.Tenant != q.Tenant) || ev.Time.Before(q.From) || !ev.Time.Before(q.To) {
			continue
		}
		n++
		overhead = overhead.Add(ev.OverheadCost)
		if ev.CostKnown {
			cost = cost.Add(ev.Cost)
			baseline = baseline.Add(ev.BaselineCost)
		}
	}
	if r.Totals.Requests != n || !r.Totals.CostUSD.Equal(cost.Add(overhead)) ||
		!r.Totals.BaselineCostUSD.Equal(baseline) || !r.Totals.OverheadUSD.Equal(overhead) {
		t.Errorf("totals = %d requests, cost %s, baseline %s, overhead %s; Go sums are %d, %s, %s, %s",
			r.Totals.Requests, r.Totals.CostUSD, r.Totals.BaselineCostUSD, r.Totals.OverheadUSD,
			n, cost.Add(overhead), baseline, overhead)
	}
}

func TestMemoryLedgerRandomTotals(t *testing.T) {
	events := randomEvents(t, 7, 2000)
	l := newMemoryWith(events)
	for _, q := range reportQueries() {
		r, err := l.Report(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		checkTotalsAgainstEvents(t, q, r, events)
		if len(q.GroupBy) > 0 && q.Limit == 0 {
			checkGroupsSumToTotals(t, r)
		}
	}
}
