package usage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestWilsonLower(t *testing.T) {
	tests := []struct {
		acceptable, n int64
		want          float64
	}{
		{0, 0, 0},
		{0, 10, 0},
		{1, 1, 0.2698659487840541},
		{9, 10, 0.6522813326641418},
		{29, 30, 0.8635958578481829},
		{30, 30, 0.9172756918749005},
		{50, 100, 0.41884779611252954},
		{95, 100, 0.9008389147209472},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d/%d", tt.acceptable, tt.n), func(t *testing.T) {
			if got := WilsonLower(tt.acceptable, tt.n); math.Abs(got-tt.want) > 1e-12 {
				t.Errorf("WilsonLower(%d, %d) = %v, want %v", tt.acceptable, tt.n, got, tt.want)
			}
		})
	}
	// The bound never exceeds the pass rate and rises with it.
	for n := int64(1); n <= 200; n++ {
		prev := -1.0
		for a := int64(0); a <= n; a++ {
			w := WilsonLower(a, n)
			if w > float64(a)/float64(n)+1e-12 || w < prev {
				t.Fatalf("WilsonLower(%d, %d) = %v (previous %v)", a, n, w, prev)
			}
			prev = w
		}
	}
}

func TestVerificationThresholds(t *testing.T) {
	v := Verification{MinSamples: 30, MinPassRate: 0.9}
	tests := []struct {
		name               string
		acceptable, judged int64
		want               bool
	}{
		{"all pass but too few samples", 29, 29, false},
		{"enough samples, all pass", 30, 30, true},
		{"enough samples, bound just below", 29, 30, false},
		{"many samples at 95%", 95, 100, true},
		{"many samples at 94%", 94, 100, false},
		{"none", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := v.verified(tt.acceptable, tt.judged); got != tt.want {
				t.Errorf("verified(%d, %d) = %v (bound %v), want %v", tt.acceptable, tt.judged, got, WilsonLower(tt.acceptable, tt.judged), tt.want)
			}
		})
	}
	// Exactly at the threshold counts.
	at := Verification{MinSamples: 10, MinPassRate: WilsonLower(9, 10)}
	if !at.verified(9, 10) {
		t.Error("a bound equal to min_pass_rate is not verified")
	}
}

func TestFromNanoUSD(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 1: "0.000000001", 1_500_000_000: "1.5", -42: "-0.000000042"} {
		if got := FromNanoUSD(n).String(); got != want {
			t.Errorf("FromNanoUSD(%d) = %s, want %s", n, got, want)
		}
	}
}

// qualityFixture is a hand-computed data set of events, shadow
// evaluations and feedback in two tenants.
type qualityFixture struct {
	events   []*Event
	shadows  []*ShadowEval
	feedback []*Feedback
}

var qualityDay = time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)

func newQualityFixture(t *testing.T) qualityFixture {
	t.Helper()
	m := func(s string) Money { return mustMoney(t, s) }
	ev := func(id, tenant, cat, mode string, cost, base string) *Event {
		return &Event{
			ID: id, Time: qualityDay, Tenant: tenant, KeyID: "k1", Status: 200, CacheStatus: CacheStatusMiss,
			Category: cat, RoutingMode: mode, Cost: m(cost), BaselineCost: m(base), CostKnown: true,
		}
	}
	e2 := ev("e2", "acme", "coding", RoutingModeObserve, "3", "3")
	e2.ShadowDeployment, e2.ShadowCost, e2.ShadowCostKnown = "mini@openai", m("1"), true
	e4 := ev("e4", "acme", "coding", RoutingModeEnforce, "0", "3")
	e4.CacheStatus = CacheStatusHit
	f := qualityFixture{events: []*Event{
		ev("e1", "acme", "coding", RoutingModeEnforce, "1", "3"),
		e2,
		ev("e3", "acme", "general", RoutingModeEnforce, "1", "2"),
		e4,
		ev("e5", "acme", "", "", "1", "5"),
		ev("g1", "globex", "coding", RoutingModeEnforce, "1", "4"),
	}}
	se := func(id, tenant string, ok bool, errKind string) *ShadowEval {
		return &ShadowEval{
			RequestID: id, Tenant: tenant, Time: qualityDay, Category: "ignored", Acceptable: ok, ErrorKind: errKind,
			Outcome: map[bool]string{true: "equivalent", false: "reference_better"}[ok && errKind == ""], Cost: m("0.1"),
		}
	}
	f.shadows = []*ShadowEval{
		se("e1", "acme", true, ""), se("e2", "acme", true, ""), se("e3", "acme", false, ""),
		se("e4", "acme", false, "judge_failed"), se("e5", "acme", true, ""),
		// acme's evaluation of globex's request ID joins to nothing.
		se("g1", "acme", true, ""),
	}
	fb := func(id string, score int) *Feedback {
		return &Feedback{RequestID: id, Tenant: "acme", KeyID: "k1", Time: qualityDay, Score: score, Comment: "secret comment"}
	}
	f.feedback = []*Feedback{fb("e1", 1), fb("e2", -1), fb("e3", 0)}
	return f
}

// load records f into l, which must accept every row.
func (f qualityFixture) load(t *testing.T, l interface {
	Sink
	ShadowStore
	FeedbackStore
}) {
	t.Helper()
	ctx := context.Background()
	for _, ev := range f.events {
		l.Record(ev)
	}
	for _, s := range f.shadows {
		if err := l.RecordShadowEval(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	for _, fb := range f.feedback {
		if err := l.RecordFeedback(ctx, fb); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMemoryLedgerQuality(t *testing.T) {
	l := NewMemoryLedger()
	newQualityFixture(t).load(t, l)
	checkQualityFixture(t, l)
}

// checkQualityFixture checks the reports of a ledger holding the
// qualityFixture.
func checkQualityFixture(t *testing.T, rep Reporter) {
	t.Helper()
	ctx := context.Background()
	from, to := qualityDay.Add(-time.Hour), qualityDay.Add(time.Hour)
	loose := Verification{MinSamples: 2, MinPassRate: 0.2}
	codingVerified := CategoryQuality{Category: "coding", Judged: 2, Acceptable: 2, Errors: 1, PassRate: 1, WilsonLower: WilsonLower(2, 2), Verified: true}
	general := CategoryQuality{Category: "general", Judged: 1, PassRate: 0, WilsonLower: 0}

	t.Run("totals", func(t *testing.T) {
		r, err := rep.Report(ctx, Query{Tenant: "acme", From: from, To: to, Verification: loose})
		if err != nil {
			t.Fatal(err)
		}
		if r.Verification != loose {
			t.Errorf("verification = %+v, want %+v", r.Verification, loose)
		}
		want := []CategoryQuality{codingVerified, general}
		if diff := cmp.Diff(want, r.Totals.Quality); diff != "" {
			t.Errorf("quality (-want +got):\n%s", diff)
		}
		// coding: e1 routing 2, e2 projected 2, e4 a cache hit (0).
		if got := r.Totals.VerifiedSavingsUSD.String(); got != "4" {
			t.Errorf("verified savings = %s, want 4", got)
		}
		// The verified savings never replace the other totals.
		if r.Totals.SavingsUSD.Routing.String() != "7" || r.Totals.ProjectedSavingsUSD.String() != "2" {
			t.Errorf("routing savings %s, projected %s; want 7 and 2", r.Totals.SavingsUSD.Routing, r.Totals.ProjectedSavingsUSD)
		}
		if got := r.Totals.ShadowCostUSD.String(); got != "0.5" {
			t.Errorf("shadow cost = %s, want 0.5", got)
		}
		// Shadow spend is overhead (DESIGN §12.4): the fixture's events
		// have none of their own, so the overhead is the shadow cost, and
		// net savings and the cost include it.
		tot := r.Totals
		if tot.OverheadUSD.String() != "0.5" ||
			!tot.SavingsUSD.Net.Equal(tot.SavingsUSD.Routing.Add(tot.SavingsUSD.Cache).Sub(tot.OverheadUSD)) ||
			!tot.SavingsUSD.Net.Equal(tot.BaselineCostUSD.Sub(tot.CostUSD)) {
			t.Errorf("overhead %s, net %s, routing %s, cache %s, baseline %s, cost %s: the shadow cost must be overhead",
				tot.OverheadUSD, tot.SavingsUSD.Net, tot.SavingsUSD.Routing, tot.SavingsUSD.Cache, tot.BaselineCostUSD, tot.CostUSD)
		}
		if want := (FeedbackCounts{Negative: 1, Neutral: 1, Positive: 1}); r.Totals.Feedback != want {
			t.Errorf("feedback = %+v, want %+v", r.Totals.Feedback, want)
		}
	})

	t.Run("default verification needs 30 samples", func(t *testing.T) {
		r, err := rep.Report(ctx, Query{Tenant: "acme", From: from, To: to})
		if err != nil {
			t.Fatal(err)
		}
		if r.Verification != (Verification{MinSamples: 30, MinPassRate: 0.9}) {
			t.Errorf("verification = %+v", r.Verification)
		}
		if len(r.Totals.Quality) != 2 || r.Totals.Quality[0].Verified || !r.Totals.VerifiedSavingsUSD.IsZero() {
			t.Errorf("quality %+v, verified savings %s; want nothing verified", r.Totals.Quality, r.Totals.VerifiedSavingsUSD)
		}
	})

	t.Run("group by category", func(t *testing.T) {
		r, err := rep.Report(ctx, Query{Tenant: "acme", From: from, To: to, GroupBy: []string{GroupByCategory}, Verification: loose})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]Totals{}
		for _, g := range r.Groups {
			got[g.Key[GroupByCategory]] = g.Totals
		}
		if len(got) != 3 {
			t.Fatalf("groups = %v, want coding, general and none", r.Groups)
		}
		if diff := cmp.Diff([]CategoryQuality{codingVerified}, got["coding"].Quality); diff != "" {
			t.Errorf("coding quality (-want +got):\n%s", diff)
		}
		if got["coding"].VerifiedSavingsUSD.String() != "4" || !got["general"].VerifiedSavingsUSD.IsZero() {
			t.Errorf("verified savings: coding %s, general %s", got["coding"].VerifiedSavingsUSD, got["general"].VerifiedSavingsUSD)
		}
		if diff := cmp.Diff([]CategoryQuality{general}, got["general"].Quality); diff != "" {
			t.Errorf("general quality (-want +got):\n%s", diff)
		}
		if got[""].Quality != nil || got[""].ShadowCostUSD.String() != "0.1" {
			t.Errorf("uncategorized group: quality %v, shadow cost %s", got[""].Quality, got[""].ShadowCostUSD)
		}
		if got["coding"].Feedback != (FeedbackCounts{Negative: 1, Positive: 1}) || got["general"].Feedback != (FeedbackCounts{Neutral: 1}) {
			t.Errorf("feedback by category: coding %+v, general %+v", got["coding"].Feedback, got["general"].Feedback)
		}
	})

	t.Run("each group verifies from its own samples", func(t *testing.T) {
		r, err := rep.Report(ctx, Query{Tenant: "acme", From: from, To: to, GroupBy: []string{GroupByRoutingMode}, Verification: loose})
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range r.Groups {
			// No routing mode has 2 judged coding samples of its own.
			if !g.Totals.VerifiedSavingsUSD.IsZero() {
				t.Errorf("group %v: verified savings %s, want 0", g.Key, g.Totals.VerifiedSavingsUSD)
			}
		}
	})

	t.Run("tenant isolation", func(t *testing.T) {
		r, err := rep.Report(ctx, Query{Tenant: "globex", From: from, To: to, Verification: loose})
		if err != nil {
			t.Fatal(err)
		}
		if r.Totals.Requests != 1 || r.Totals.Quality != nil || !r.Totals.ShadowCostUSD.IsZero() || r.Totals.Feedback != (FeedbackCounts{}) {
			t.Errorf("globex totals = %+v; acme's rows must not join to its request", r.Totals)
		}
		all, err := rep.Report(ctx, Query{From: from, To: to, Verification: loose})
		if err != nil {
			t.Fatal(err)
		}
		if got := all.Totals.ShadowCostUSD.String(); got != "0.5" {
			t.Errorf("all tenants: shadow cost %s, want 0.5 (the cross-tenant row joins to nothing)", got)
		}
	})
}

func TestMemoryFeedbackStore(t *testing.T) {
	ctx := context.Background()
	l := NewMemoryLedger()
	l.Record(&Event{ID: "r1", Tenant: "acme", Time: qualityDay, Category: "general", CostKnown: true})
	l.Record(&Event{ID: "r2", Tenant: "globex", Time: qualityDay})

	tests := []struct {
		name string
		f    Feedback
		want error
	}{
		{"own request", Feedback{RequestID: "r1", Tenant: "acme", Score: 1}, nil},
		{"replaced", Feedback{RequestID: "r1", Tenant: "acme", Score: -1, Comment: "worse"}, nil},
		{"unknown request", Feedback{RequestID: "nope", Tenant: "acme", Score: 1}, ErrUnknownRequest},
		{"another tenant's request", Feedback{RequestID: "r2", Tenant: "acme", Score: 1}, ErrUnknownRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := l.RecordFeedback(ctx, &tt.f); !errors.Is(err, tt.want) {
				t.Errorf("RecordFeedback = %v, want %v", err, tt.want)
			}
		})
	}
	for _, bad := range []Feedback{
		{Tenant: "acme", Score: 1},
		{RequestID: "r1", Score: 1},
		{RequestID: "r1", Tenant: "acme", Score: 2},
		{RequestID: "r1", Tenant: "acme", Comment: strings.Repeat("é", MaxFeedbackComment+1)},
	} {
		err := l.RecordFeedback(ctx, &bad)
		if err == nil || errors.Is(err, ErrUnknownRequest) {
			t.Errorf("RecordFeedback(%+v) = %v, want a validation error", bad, err)
		}
		if err != nil && strings.Contains(err.Error(), "é") {
			t.Errorf("error %q quotes the comment", err)
		}
	}
	if err := l.RecordFeedback(ctx, &Feedback{RequestID: "r1", Tenant: "acme", Comment: strings.Repeat("é", MaxFeedbackComment)}); err != nil {
		t.Errorf("a %d-character comment: %v", MaxFeedbackComment, err)
	}
	r, err := l.Report(ctx, Query{Tenant: "acme", From: qualityDay.Add(-time.Hour), To: qualityDay.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Totals.Feedback != (FeedbackCounts{Neutral: 1}) {
		t.Errorf("feedback = %+v, want the latest score only", r.Totals.Feedback)
	}

	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordFeedback(ctx, &Feedback{RequestID: "r1", Tenant: "acme"}); err == nil {
		t.Error("RecordFeedback after Close succeeded")
	}
	if err := l.RecordShadowEval(ctx, &ShadowEval{RequestID: "r1", Tenant: "acme"}); err == nil {
		t.Error("RecordShadowEval after Close succeeded")
	}
}

func TestMemoryShadowStore(t *testing.T) {
	ctx := context.Background()
	l := NewMemoryLedger(WithMaxEvents(2))
	day := qualityDay
	for _, id := range []string{"a", "b", "c"} {
		l.Record(&Event{ID: id, Tenant: "acme", Time: day, Category: "coding", CostKnown: true})
	}
	// The first evaluation of a request is kept.
	for _, ok := range []bool{true, false} {
		if err := l.RecordShadowEval(ctx, &ShadowEval{RequestID: "c", Tenant: "acme", Acceptable: ok}); err != nil {
			t.Fatal(err)
		}
	}
	// The ledger keeps at most 2 evaluations: a and b push c out.
	for _, id := range []string{"a", "b"} {
		if err := l.RecordShadowEval(ctx, &ShadowEval{RequestID: id, Tenant: "acme", Acceptable: true}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := l.Report(ctx, Query{Tenant: "acme", From: day.Add(-time.Hour), To: day.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// Events b and c are kept; of their evaluations only b's.
	if len(r.Totals.Quality) != 1 || r.Totals.Quality[0].Judged != 1 || r.Totals.Quality[0].Acceptable != 1 {
		t.Errorf("quality = %+v, want b's evaluation only", r.Totals.Quality)
	}
	for _, bad := range []*ShadowEval{nil, {Tenant: "acme"}, {RequestID: "x"}} {
		if err := l.RecordShadowEval(ctx, bad); err == nil {
			t.Errorf("RecordShadowEval(%+v) succeeded", bad)
		}
	}
	// A stored evaluation is a copy.
	e := &ShadowEval{RequestID: "b2", Tenant: "acme", Acceptable: true}
	if err := l.RecordShadowEval(ctx, e); err != nil {
		t.Fatal(err)
	}
	e.Acceptable = false
	l.mu.Lock()
	kept := l.shadows.byKey[rowKey{"acme", "b2"}]
	l.mu.Unlock()
	if !kept.Acceptable {
		t.Error("the ledger shares the caller's evaluation")
	}
}

func TestReportVerificationQuery(t *testing.T) {
	l := NewMemoryLedger()
	from, to := qualityDay, qualityDay.Add(time.Hour)
	for _, v := range []Verification{{MinSamples: -1}, {MinPassRate: -0.1}, {MinPassRate: 1.1}, {MinPassRate: math.NaN()}} {
		if _, err := l.Report(context.Background(), Query{From: from, To: to, Verification: v}); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("verification %+v: err = %v, want ErrInvalidQuery", v, err)
		}
	}
	r, err := l.Report(context.Background(), Query{From: from, To: to, Verification: Verification{MinSamples: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Verification != (Verification{MinSamples: 5, MinPassRate: 0.9}) {
		t.Errorf("verification = %+v, want the default pass rate filled in", r.Verification)
	}
}

// addQualityRows gives events categories and returns shadow evaluations
// and feedback for some of them, for the memory and Postgres parity test.
// Some categories pass often enough to be verified, others do not. A few
// rows name another tenant's requests, which reports never join.
func addQualityRows(events []*Event, seed uint64) ([]*ShadowEval, []*Feedback) {
	rng := rand.New(rand.NewPCG(seed, 2))
	cats := []string{"coding", "creative", "extraction", "general", "reasoning", "tool_use", "", "other"}
	pass := map[string]float64{"coding": 0.99, "general": 0.97, "reasoning": 0.6, "creative": 0.9}
	var shadows []*ShadowEval
	var feedback []*Feedback
	for i, ev := range events {
		ev.Category = cats[rng.IntN(len(cats))]
		if rng.IntN(3) == 0 {
			s := &ShadowEval{
				RequestID: ev.ID, Time: ev.Time, Tenant: ev.Tenant, KeyID: ev.KeyID, Category: ev.Category,
				Difficulty: rng.Float64(), Profile: ev.Profile, Mode: ev.RoutingMode,
				CandidateDeployment: "mini@openai", ReferenceDeployment: "gpt@openai", JudgeModel: "judge@openai",
				Cost: FromNanoUSD(rng.Int64N(10_000_000)),
			}
			switch p := pass[ev.Category]; {
			case rng.IntN(20) == 0:
				s.ErrorKind = "judge_failed"
			case rng.Float64() < max(p, 0.5):
				s.Outcome, s.Acceptable = "equivalent", true
			default:
				s.Outcome = "reference_better"
			}
			if i%50 == 0 {
				s.Tenant = "nobody"
			}
			shadows = append(shadows, s)
		}
		if rng.IntN(5) == 0 {
			feedback = append(feedback, &Feedback{
				RequestID: ev.ID, Tenant: ev.Tenant, KeyID: ev.KeyID, Time: ev.Time, Score: rng.IntN(3) - 1, Comment: "c",
			})
		}
	}
	return shadows, feedback
}

func TestMemoryLedgerRandomQuality(t *testing.T) {
	events := randomEvents(t, 7, 2000)
	shadows, feedback := addQualityRows(events, 7)
	l := newMemoryWith(events)
	ctx := context.Background()
	for _, s := range shadows {
		if err := l.RecordShadowEval(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range feedback {
		if err := l.RecordFeedback(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	verified := false
	for _, q := range qualityQueries() {
		r, err := l.Report(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		checkQualityAgainstRows(t, q, r, events, shadows, feedback)
		for _, c := range r.Totals.Quality {
			verified = verified || c.Verified
		}
	}
	if !verified {
		t.Error("no category is verified in any report; the data set does not exercise verification")
	}
}

// qualityQueries are report queries over the random data that group by
// the quality dimension, with default and loose verification.
func qualityQueries() []Query {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	var qs []Query
	for _, tenant := range []string{"acme", "globex", "nobody", ""} {
		for _, v := range []Verification{{}, {MinSamples: 5, MinPassRate: 0.5}} {
			qs = append(qs,
				Query{Tenant: tenant, From: from, To: to, Verification: v},
				Query{Tenant: tenant, From: from, To: to, GroupBy: []string{GroupByCategory}, Verification: v},
				Query{Tenant: tenant, From: from, To: to, GroupBy: []string{GroupByCategory, GroupByRoutingMode}, Verification: v},
				Query{Tenant: tenant, From: from, To: to, GroupBy: []string{GroupByProfile}, Verification: v},
				Query{Tenant: tenant, From: from.AddDate(0, 0, 25), To: from.AddDate(0, 1, 5), GroupBy: []string{GroupByDay}, Verification: v},
			)
		}
	}
	return qs
}

// checkQualityAgainstRows recomputes the report's shadow totals from the
// rows, joined by tenant and request ID as the stores join them.
func checkQualityAgainstRows(t *testing.T, q Query, r *Report, events []*Event, shadows []*ShadowEval, feedback []*Feedback) {
	t.Helper()
	type key struct{ tenant, id string }
	se := map[key]*ShadowEval{}
	for _, s := range shadows {
		if _, dup := se[key{s.Tenant, s.RequestID}]; !dup {
			se[key{s.Tenant, s.RequestID}] = s
		}
	}
	fb := map[key]*Feedback{}
	for _, f := range feedback {
		fb[key{f.Tenant, f.RequestID}] = f
	}
	var cost Money
	var n, fbCount int64
	for _, ev := range events {
		if (q.Tenant != "" && ev.Tenant != q.Tenant) || ev.Time.Before(q.From) || !ev.Time.Before(q.To) {
			continue
		}
		if s := se[key{ev.Tenant, ev.ID}]; s != nil {
			cost = cost.Add(s.Cost)
			if s.ErrorKind == "" && categoryIndex(ev.Category) >= 0 {
				n++
			}
		}
		if fb[key{ev.Tenant, ev.ID}] != nil {
			fbCount++
		}
	}
	var gotN int64
	for _, c := range r.Totals.Quality {
		gotN += c.Judged
		if c.Verified != (c.Judged >= int64(r.Verification.MinSamples) && c.WilsonLower >= r.Verification.MinPassRate) {
			t.Errorf("category %+v: verified flag disagrees with %+v", c, r.Verification)
		}
	}
	f := r.Totals.Feedback
	if gotN != n || !r.Totals.ShadowCostUSD.Equal(cost) || f.Negative+f.Neutral+f.Positive != fbCount {
		t.Errorf("tenant %q, group by %v: judged %d, shadow cost %s, feedback %d; rows give %d, %s, %d",
			q.Tenant, q.GroupBy, gotN, r.Totals.ShadowCostUSD, f.Negative+f.Neutral+f.Positive, n, cost, fbCount)
	}
}
