package budget

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// noon is a fixed time in the middle of a UTC day and month. It is a century
// ahead so the Redis keys' EXPIREAT deadlines, computed from it, never fall
// into the real past (real Redis and miniredis expire them by wall clock).
var noon = time.Date(2126, 11, 11, 12, 0, 0, 0, time.UTC)

// scope returns a tenant scope named after tenant and window.
func scope(tenant, window string, limit int64, block bool) Scope {
	return Scope{Tenant: tenant, Window: window, LimitMicro: limit, Block: block, Name: tenant + "/" + window}
}

// keyScope returns a key scope of tenant for key.
func keyScope(tenant, key, window string, limit int64, block bool) Scope {
	return Scope{Tenant: tenant, KeyID: key, Window: window, LimitMicro: limit, Block: block, Name: tenant + "/key:" + key + "/" + window}
}

// spent returns the spend of s at now, failing the test on error.
func spent(t *testing.T, st Store, s Scope, now time.Time) int64 {
	t.Helper()
	v, err := st.Spent(context.Background(), s, now)
	if err != nil {
		t.Fatalf("Spent(%s): %v", s.Name, err)
	}
	return v
}

// mustAdmit admits est against scopes, failing the test on error.
func mustAdmit(t *testing.T, st Store, scopes []Scope, est int64, now time.Time) Admission {
	t.Helper()
	a, err := st.Admit(context.Background(), scopes, est, now)
	if err != nil {
		t.Fatalf("Admit(%d): %v", est, err)
	}
	return a
}

// wantExhausted checks that err is an *ExhaustedError for the scope named
// name that resets at reset.
func wantExhausted(t *testing.T, err error, name string, reset time.Time) {
	t.Helper()
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
	var ee *ExhaustedError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %T, want *ExhaustedError", err)
	}
	if ee.Scope.Name != name || !ee.ResetAt.Equal(reset) {
		t.Errorf("exhausted %s until %s, want %s until %s", ee.Scope.Name, ee.ResetAt, name, reset)
	}
}

// names returns the names of scopes.
func names(scopes []Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = s.Name
	}
	return out
}

// testContract runs the Store contract against stores made by newStore,
// which returns a fresh, empty store for each subtest.
func testContract(t *testing.T, newStore func(t *testing.T) Store) {
	ctx := context.Background()
	nextDay := time.Date(2126, 11, 12, 0, 0, 0, 0, time.UTC)
	nextMonth := time.Date(2126, 12, 1, 0, 0, 0, 0, time.UTC)

	t.Run("admit and settle", func(t *testing.T) {
		st := newStore(t)
		s := scope("acme", WindowDay, 1000, true)
		a := mustAdmit(t, st, []Scope{s}, 300, noon)
		if a.Charged() != 300 || len(a.Exceeded) != 0 || len(a.SoftCrossed) != 0 {
			t.Errorf("admission = %+v, want charged 300 and nothing reported", a)
		}
		if got := spent(t, st, s, noon); got != 300 {
			t.Errorf("spent after admit = %d, want 300", got)
		}
		if err := st.Settle(ctx, a, 120); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if got := spent(t, st, s, noon); got != 120 {
			t.Errorf("spent after settling below the estimate = %d, want 120", got)
		}
		b := mustAdmit(t, st, []Scope{s}, 100, noon)
		if err := st.Settle(ctx, b, 250); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if got := spent(t, st, s, noon); got != 370 {
			t.Errorf("spent after settling above the estimate = %d, want 370", got)
		}
		if err := st.Settle(ctx, mustAdmit(t, st, []Scope{s}, 30, noon), 30); err != nil {
			t.Fatalf("Settle at the estimate: %v", err)
		}
		if got := spent(t, st, s, noon); got != 400 {
			t.Errorf("spent after settling at the estimate = %d, want 400", got)
		}
	})

	t.Run("limit is inclusive", func(t *testing.T) {
		st := newStore(t)
		s := scope("acme", WindowDay, 1000, true)
		mustAdmit(t, st, []Scope{s}, 1000, noon)
		_, err := st.Admit(ctx, []Scope{s}, 1, noon)
		wantExhausted(t, err, "acme/day", nextDay)
		mustAdmit(t, st, []Scope{s}, 0, noon) // a free request still fits
		if got := spent(t, st, s, noon); got != 1000 {
			t.Errorf("spent = %d, want 1000", got)
		}
	})

	t.Run("block refuses and notify reports", func(t *testing.T) {
		st := newStore(t)
		block := scope("acme", WindowDay, 500, true)
		notify := scope("acme", WindowMonth, 500, false)
		mustAdmit(t, st, []Scope{notify}, 400, noon)
		a := mustAdmit(t, st, []Scope{notify}, 400, noon)
		if got := names(a.Exceeded); len(got) != 1 || got[0] != "acme/month" {
			t.Errorf("Exceeded = %v, want [acme/month]", got)
		}
		if got := spent(t, st, notify, noon); got != 800 {
			t.Errorf("notify spent = %d, want 800 (charged over the limit)", got)
		}

		_, err := st.Admit(ctx, []Scope{block}, 501, noon)
		wantExhausted(t, err, "acme/day", nextDay)
		if got := spent(t, st, block, noon); got != 0 {
			t.Errorf("block spent after refusal = %d, want 0", got)
		}
	})

	t.Run("refusal charges no scope", func(t *testing.T) {
		st := newStore(t)
		tenant := scope("acme", WindowMonth, 1_000_000, true)
		notify := scope("acme", WindowDay, 10, false)
		key := keyScope("acme", "k1", WindowDay, 100, true)
		scopes := []Scope{tenant, notify, key}
		mustAdmit(t, st, scopes, 100, noon)
		_, err := st.Admit(ctx, scopes, 1, noon)
		wantExhausted(t, err, "acme/key:k1/day", nextDay)
		for _, s := range scopes {
			if got := spent(t, st, s, noon); got != 100 {
				t.Errorf("%s spent = %d, want 100: a refused admission must charge nothing", s.Name, got)
			}
		}
		// Another key of the same tenant is unaffected.
		other := keyScope("acme", "k2", WindowDay, 100, true)
		mustAdmit(t, st, []Scope{tenant, other}, 100, noon)
		if got := spent(t, st, tenant, noon); got != 200 {
			t.Errorf("tenant spent = %d, want 200", got)
		}
	})

	t.Run("exhausted reports the scope that resets last", func(t *testing.T) {
		st := newStore(t)
		day := scope("acme", WindowDay, 100, true)
		month := scope("acme", WindowMonth, 100, true)
		mustAdmit(t, st, []Scope{day, month}, 100, noon)
		_, err := st.Admit(ctx, []Scope{day, month}, 1, noon)
		wantExhausted(t, err, "acme/month", nextMonth)
		_, err = st.Admit(ctx, []Scope{month, day}, 1, noon)
		wantExhausted(t, err, "acme/month", nextMonth)
	})

	t.Run("delta", func(t *testing.T) {
		st := newStore(t)
		s := scope("acme", WindowDay, 1000, true)
		notify := scope("acme", WindowMonth, 800, false)
		a := mustAdmit(t, st, []Scope{s, notify}, 400, noon)
		if err := st.Delta(ctx, &a, 400, noon); err != nil {
			t.Fatalf("Delta: %v", err)
		}
		if a.Charged() != 800 || len(a.Exceeded) != 0 {
			t.Errorf("after delta: charged %d, exceeded %v; want 800 and none", a.Charged(), names(a.Exceeded))
		}
		err := st.Delta(ctx, &a, 201, noon)
		wantExhausted(t, err, "acme/day", nextDay)
		if a.Charged() != 800 {
			t.Errorf("charged after refused delta = %d, want 800", a.Charged())
		}
		if err := st.Delta(ctx, &a, 0, noon); err != nil {
			t.Errorf("Delta(0): %v", err)
		}
		if err := st.Delta(ctx, &a, -50, noon); err != nil || a.Charged() != 800 {
			t.Errorf("Delta(-50) = %v, charged %d; want no change", err, a.Charged())
		}
		if err := st.Delta(ctx, &a, 100, noon); err != nil {
			t.Fatalf("Delta: %v", err)
		}
		if got := names(a.Exceeded); len(got) != 1 || got[0] != "acme/month" {
			t.Errorf("Exceeded after delta = %v, want [acme/month]", got)
		}
		if err := st.Settle(ctx, a, 650); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		for _, sc := range []Scope{s, notify} {
			if got := spent(t, st, sc, noon); got != 650 {
				t.Errorf("%s spent = %d, want 650", sc.Name, got)
			}
		}
	})

	t.Run("delta charges the admission's windows", func(t *testing.T) {
		st := newStore(t)
		s := scope("acme", WindowDay, 1000, true)
		late := time.Date(2126, 11, 11, 23, 59, 59, 0, time.UTC)
		a := mustAdmit(t, st, []Scope{s}, 100, late)
		if err := st.Delta(ctx, &a, 50, nextDay); err != nil {
			t.Fatalf("Delta: %v", err)
		}
		if err := st.Settle(ctx, a, 200); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if got := spent(t, st, s, late); got != 200 {
			t.Errorf("spent in the admitted day = %d, want 200", got)
		}
		if got := spent(t, st, s, nextDay); got != 0 {
			t.Errorf("spent in the next day = %d, want 0", got)
		}
	})

	t.Run("window rollover", func(t *testing.T) {
		tests := []struct {
			name           string
			window         string
			before, after  time.Time
			reset          time.Time
			sameWindowTime time.Time
		}{
			{"day", WindowDay,
				time.Date(2126, 11, 11, 23, 59, 59, 999_999_999, time.UTC), nextDay,
				nextDay, time.Date(2126, 11, 11, 0, 0, 0, 0, time.UTC)},
			{"month", WindowMonth,
				time.Date(2126, 11, 30, 23, 59, 59, 0, time.UTC), nextMonth,
				nextMonth, time.Date(2126, 11, 1, 0, 0, 0, 0, time.UTC)},
			{"year", WindowMonth,
				time.Date(2126, 12, 31, 23, 0, 0, 0, time.UTC), time.Date(2127, 1, 1, 0, 0, 0, 0, time.UTC),
				time.Date(2127, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2126, 12, 1, 0, 0, 0, 0, time.UTC)},
			{"leap day", WindowDay,
				time.Date(2128, 2, 29, 10, 0, 0, 0, time.UTC), time.Date(2128, 3, 1, 0, 0, 0, 0, time.UTC),
				time.Date(2128, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2128, 2, 29, 0, 0, 0, 0, time.UTC)},
			{"non-UTC clock", WindowDay,
				// 2126-11-12 01:00 at UTC+5 is 2126-11-11 20:00 UTC.
				time.Date(2126, 11, 12, 1, 0, 0, 0, time.FixedZone("UTC+5", 5*3600)), nextDay,
				nextDay, time.Date(2126, 11, 11, 0, 0, 0, 0, time.UTC)},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				st := newStore(t)
				s := scope("acme", tt.window, 100, true)
				mustAdmit(t, st, []Scope{s}, 100, tt.before)
				_, err := st.Admit(ctx, []Scope{s}, 1, tt.before)
				wantExhausted(t, err, s.Name, tt.reset)
				if got := spent(t, st, s, tt.sameWindowTime); got != 100 {
					t.Errorf("spent at the window start = %d, want 100", got)
				}
				if got := spent(t, st, s, tt.after); got != 0 {
					t.Errorf("spent in the next window = %d, want 0", got)
				}
				mustAdmit(t, st, []Scope{s}, 100, tt.after)
				if got := spent(t, st, s, tt.before); got != 100 {
					t.Errorf("spent in the old window = %d, want 100", got)
				}
			})
		}
	})

	t.Run("soft threshold is reported once per window", func(t *testing.T) {
		st := newStore(t)
		s := scope("acme", WindowDay, 1000, true)
		s.SoftAtMicro = 500
		notify := scope("acme", WindowMonth, 1000, false)
		notify.SoftAtMicro = 900
		scopes := []Scope{s, notify}
		steps := []struct {
			est  int64
			now  time.Time
			want []string
		}{
			{300, noon, nil},
			{199, noon, nil},
			{1, noon, []string{"acme/day"}}, // reaches exactly 500
			{300, noon, nil},
			{100, noon, []string{"acme/month"}},
			{50, nextDay, nil}, // the new day starts from 0; the month is past 900 already
			{450, nextDay, []string{"acme/day"}},
			{10, nextDay, nil},
		}
		for i, step := range steps {
			a := mustAdmit(t, st, scopes, step.est, step.now)
			if got := names(a.SoftCrossed); !slices.Equal(got, step.want) {
				t.Errorf("step %d: SoftCrossed = %v, want %v", i, got, step.want)
			}
		}
	})

	t.Run("soft threshold crossed by delta", func(t *testing.T) {
		st := newStore(t)
		s := scope("acme", WindowDay, 1000, true)
		s.SoftAtMicro = 500
		a := mustAdmit(t, st, []Scope{s}, 400, noon)
		if err := st.Delta(ctx, &a, 100, noon); err != nil {
			t.Fatalf("Delta: %v", err)
		}
		if got := names(a.SoftCrossed); !slices.Equal(got, []string{"acme/day"}) {
			t.Errorf("SoftCrossed = %v, want [acme/day]", got)
		}
		if err := st.Delta(ctx, &a, 100, noon); err != nil {
			t.Fatalf("Delta: %v", err)
		}
		if len(a.SoftCrossed) != 0 {
			t.Errorf("SoftCrossed after a second delta = %v, want none", names(a.SoftCrossed))
		}
	})

	t.Run("soft crossed state", func(t *testing.T) {
		st := newStore(t)
		day := scope("acme", WindowDay, 1000, true)
		day.SoftAtMicro = 500
		month := scope("acme", WindowMonth, 2000, false) // no soft threshold
		scopes := []Scope{day, month}
		state := func(now time.Time) []State {
			t.Helper()
			got, err := st.SoftCrossed(ctx, scopes, now)
			if err != nil {
				t.Fatalf("SoftCrossed: %v", err)
			}
			return got
		}
		if got := state(noon); !slices.Equal(got, []State{{}, {}}) {
			t.Errorf("before any charge: %+v, want zero states", got)
		}
		mustAdmit(t, st, scopes, 400, noon)
		if got := state(noon); !slices.Equal(got, []State{{Spent: 400}, {Spent: 400}}) {
			t.Errorf("below the threshold: %+v", got)
		}
		// A charge that crosses the threshold sets the flag, which stays
		// even when a settlement brings the spend back under it.
		b := mustAdmit(t, st, scopes, 200, noon)
		if got := state(noon); !slices.Equal(got, []State{{Spent: 600, SoftCrossed: true}, {Spent: 600}}) {
			t.Errorf("charged past the threshold: %+v", got)
		}
		if err := st.Settle(ctx, b, 0); err != nil {
			t.Fatal(err)
		}
		if got := state(noon); !slices.Equal(got, []State{{Spent: 400, SoftCrossed: true}, {Spent: 400}}) {
			t.Errorf("settled back under the threshold: %+v", got)
		}
		// A settlement that reaches the threshold sets no flag, but the
		// spend shows it.
		key := keyScope("acme", "k1", WindowDay, 1000, true)
		key.SoftAtMicro = 500
		c := mustAdmit(t, st, []Scope{key}, 100, noon)
		if err := st.Settle(ctx, c, 500); err != nil {
			t.Fatal(err)
		}
		if got, err := st.SoftCrossed(ctx, []Scope{key}, noon); err != nil || !slices.Equal(got, []State{{Spent: 500, SoftCrossed: true}}) {
			t.Errorf("settled to the threshold: %+v, %v", got, err)
		}
		if got := state(nextDay); !slices.Equal(got, []State{{}, {Spent: 400}}) {
			t.Errorf("next day: %+v, want a fresh day", got)
		}
		if got := (State{Spent: 1200}).Remaining(day); got != 0 {
			t.Errorf("Remaining over the limit = %d, want 0", got)
		}
		if got := (State{Spent: 300}).Remaining(day); got != 700 {
			t.Errorf("Remaining = %d, want 700", got)
		}
		if got, err := st.SoftCrossed(ctx, nil, noon); err != nil || len(got) != 0 {
			t.Errorf("SoftCrossed(no scopes) = %v, %v", got, err)
		}
	})

	t.Run("no scopes", func(t *testing.T) {
		st := newStore(t)
		a := mustAdmit(t, st, nil, 1_000_000, noon)
		if a.Charged() != 0 || len(a.Scopes()) != 0 {
			t.Errorf("admission = %+v, want empty", a)
		}
		if err := st.Delta(ctx, &a, 10, noon); err != nil {
			t.Errorf("Delta: %v", err)
		}
		if err := st.Settle(ctx, a, 10); err != nil {
			t.Errorf("Settle: %v", err)
		}
		if err := st.Settle(ctx, Admission{}, 10); err != nil {
			t.Errorf("Settle(zero Admission): %v", err)
		}
	})

	t.Run("invalid arguments", func(t *testing.T) {
		st := newStore(t)
		s := scope("acme", WindowDay, 1000, true)
		if _, err := st.Admit(ctx, []Scope{s}, -1, noon); err == nil || errors.Is(err, ErrUnavailable) {
			t.Errorf("Admit(-1) = %v, want an argument error", err)
		}
		bad := s
		bad.Window = "week"
		if _, err := st.Admit(ctx, []Scope{bad}, 1, noon); err == nil {
			t.Error("Admit with an unknown window succeeded")
		}
		a := mustAdmit(t, st, []Scope{s}, 1, noon)
		if err := st.Settle(ctx, a, -1); err == nil {
			t.Error("Settle(-1) succeeded")
		}
	})

	t.Run("concurrent admissions never overshoot", func(t *testing.T) {
		const (
			workers = 1000
			est     = 1_000
			limit   = 100_500 // room for 100 admissions
		)
		st := newStore(t)
		tenant := scope("acme", WindowMonth, 10*limit, true)
		key := keyScope("acme", "k1", WindowDay, limit, true)
		scopes := []Scope{tenant, key}
		var admitted, rejected atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, workers)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				a, err := st.Admit(ctx, scopes, est, noon)
				if err != nil {
					if errors.Is(err, ErrExhausted) {
						rejected.Add(1)
						return
					}
					errs <- err
					return
				}
				admitted.Add(1)
				if err := st.Settle(ctx, a, est); err != nil {
					errs <- err
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("unexpected error: %v", err)
		}
		if got := admitted.Load(); got != limit/est {
			t.Errorf("admitted %d, want %d", got, limit/est)
		}
		if got := rejected.Load(); got != workers-limit/est {
			t.Errorf("rejected %d, want %d", got, workers-limit/est)
		}
		if got := spent(t, st, key, noon); got > limit || got != admitted.Load()*est {
			t.Errorf("key spent = %d, want %d (<= limit %d)", got, admitted.Load()*est, limit)
		}
		if got := spent(t, st, tenant, noon); got != admitted.Load()*est {
			t.Errorf("tenant spent = %d, want %d", got, admitted.Load()*est)
		}
	})

	t.Run("overshoot is bounded by under-estimates", func(t *testing.T) {
		const (
			workers = 200
			est     = 1_000
			under   = 7 // each admitted request costs this much more than estimated
			limit   = 50_000
		)
		st := newStore(t)
		s := scope("acme", WindowDay, limit, true)
		var admitted atomic.Int64
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				a, err := st.Admit(ctx, []Scope{s}, est, noon)
				if err != nil {
					if !errors.Is(err, ErrExhausted) {
						t.Errorf("Admit: %v", err)
					}
					return
				}
				admitted.Add(1)
				if err := st.Settle(ctx, a, est+under); err != nil {
					t.Errorf("Settle: %v", err)
				}
			})
		}
		wg.Wait()
		n := admitted.Load()
		got := spent(t, st, s, noon)
		if got != n*(est+under) {
			t.Errorf("spent = %d, want %d", got, n*(est+under))
		}
		if over := got - limit; over > n*under {
			t.Errorf("overshoot %d exceeds the sum of under-estimates %d", over, n*under)
		}
	})
}

func TestMemoryStoreContract(t *testing.T) {
	testContract(t, func(*testing.T) Store { return NewMemoryStore() })
}

func TestMemoryStoreSweepsExpiredWindows(t *testing.T) {
	st := NewMemoryStore().(*MemoryStore)
	s := scope("acme", WindowDay, 1000, true)
	mustAdmit(t, st, []Scope{s}, 10, noon)
	// Two days after the window ends the counter is still kept.
	mustAdmit(t, st, []Scope{s}, 10, time.Date(2126, 11, 14, 0, 0, 0, 0, time.UTC))
	if n := len(st.counters); n != 2 {
		t.Fatalf("%d counters, want 2", n)
	}
	mustAdmit(t, st, []Scope{s}, 10, time.Date(2126, 11, 14, 1, 0, 1, 0, time.UTC))
	if n := len(st.counters); n != 1 {
		t.Fatalf("%d counters after the sweep, want 1", n)
	}
	if _, ok := st.counters[counterKey{s.Name, time.Date(2126, 11, 11, 0, 0, 0, 0, time.UTC)}]; ok {
		t.Error("the counter of 2126-11-11 was not swept")
	}
}
