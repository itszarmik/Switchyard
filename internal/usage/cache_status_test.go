package usage

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestIsCacheHit(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{CacheStatusExact, true},
		{CacheStatusSemantic, true},
		{CacheStatusMiss, false},
		{"", false},
		{"hit", false},
		{"Exact", false},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if got := IsCacheHit(tt.status); got != tt.want {
				t.Errorf("IsCacheHit(%q) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

// cacheEvents are one tenant's events with every cache status: a plain
// miss, an exact hit, a semantic hit whose embedding cost overhead, and a
// semantic-layer miss that paid for its embedding too.
func cacheEvents(t *testing.T) []*Event {
	t.Helper()
	m := func(s string) Money { return mustMoney(t, s) }
	ev := func(id, status, cost, baseline, overhead string) *Event {
		return &Event{
			ID: id, Time: day0, Tenant: "cachy", KeyID: "k1", Endpoint: "/v1/chat/completions",
			RequestedModel: "gpt", Status: 200, CacheStatus: status, Tokens: Tokens{Input: 10, Output: 5}, UsageKnown: true,
			Cost: m(cost), CostKnown: true, BaselineModel: "gpt", BaselineCost: m(baseline), OverheadCost: m(overhead),
		}
	}
	return []*Event{
		ev("c1", CacheStatusMiss, "1", "3", "0"),
		ev("c2", CacheStatusExact, "0", "2", "0"),
		ev("c3", CacheStatusSemantic, "0", "4", "0.001"),
		ev("c4", CacheStatusMiss, "1", "1", "0.002"),
	}
}

// checkCacheStatuses checks rep's report on exactly the cacheEvents: both
// hit statuses count as hits and as cache savings, never as routing
// savings, and the embedding overhead is subtracted from the net.
func checkCacheStatuses(t *testing.T, rep Reporter) {
	t.Helper()
	r, err := rep.Report(context.Background(), Query{Tenant: "cachy", From: handFrom, To: handTo, GroupBy: []string{GroupByCacheStatus}})
	if err != nil {
		t.Fatal(err)
	}
	m := func(s string) Money { return mustMoney(t, s) }
	want := Totals{
		Requests: 4, Tokens: Tokens{Input: 40, Output: 20},
		CostUSD: m("2.003"), BaselineCostUSD: m("10"), OverheadUSD: m("0.003"),
		CacheHitRate: 0.5,
	}
	want.SavingsUSD.Routing = m("2") // (3 - 1) + (1 - 1); hits are excluded
	want.SavingsUSD.Cache = m("6")   // the hits' baseline costs, 2 + 4
	want.SavingsUSD.Net = m("7.997") // 2 + 6 - 0.003
	if diff := cmp.Diff(want, r.Totals); diff != "" {
		t.Errorf("totals (-want +got):\n%s", diff)
	}
	got := map[string][3]string{}
	for _, g := range r.Groups {
		got[g.Key[GroupByCacheStatus]] = [3]string{
			g.Totals.SavingsUSD.Cache.String(), g.Totals.SavingsUSD.Routing.String(), g.Totals.OverheadUSD.String(),
		}
	}
	wantGroups := map[string][3]string{
		CacheStatusMiss:     {m("0").String(), m("2").String(), m("0.002").String()},
		CacheStatusExact:    {m("2").String(), m("0").String(), m("0").String()},
		CacheStatusSemantic: {m("4").String(), m("0").String(), m("0.001").String()},
	}
	if diff := cmp.Diff(wantGroups, got); diff != "" {
		t.Errorf("groups [cache, routing, overhead] (-want +got):\n%s", diff)
	}
}

func TestMemoryLedgerCacheStatuses(t *testing.T) {
	checkCacheStatuses(t, newMemoryWith(cacheEvents(t)))
}
