package budget

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newMiniredis starts a miniredis server and returns it with a client.
func newMiniredis(t *testing.T, opts *redis.Options) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	if opts == nil {
		opts = &redis.Options{}
	}
	opts.Addr = mr.Addr()
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

func TestRedisStoreContract(t *testing.T) {
	testContract(t, func(t *testing.T) Store {
		_, client := newMiniredis(t, nil)
		return NewRedisStore(client)
	})
}

func TestRedisStoreKeys(t *testing.T) {
	ctx := context.Background()
	mr, client := newMiniredis(t, nil)
	mr.SetTime(noon)
	st := NewRedisStore(client)
	day := keyScope("acme", "k1", WindowDay, 1000, true)
	day.SoftAtMicro = 500
	month := scope("acme", WindowMonth, 1000, false)

	a := mustAdmit(t, st, []Scope{day, month}, 600, noon)
	tests := []struct {
		key   string
		value string
		ttl   time.Duration
	}{
		// The day ends at 2026-11-12 00:00, 12 h after noon, plus 48 h.
		{"sy:b:v1:acme/key:k1/day:20261111", "600", 60 * time.Hour},
		{"sy:b:v1:acme/key:k1/day:20261111:soft", "1", 60 * time.Hour},
		// The month ends at 2026-12-01 00:00, 19.5 days after noon, plus 48 h.
		{"sy:b:v1:acme/month:20261101", "600", (19*24+12)*time.Hour + 48*time.Hour},
	}
	for _, tt := range tests {
		got, err := mr.Get(tt.key)
		if err != nil || got != tt.value {
			t.Errorf("GET %s = %q, %v; want %q", tt.key, got, err, tt.value)
		}
		if ttl := mr.TTL(tt.key); ttl != tt.ttl {
			t.Errorf("TTL %s = %s, want %s", tt.key, ttl, tt.ttl)
		}
	}
	if mr.Exists("sy:b:v1:acme/month:20261101:soft") {
		t.Error("a soft flag was set for a scope without a soft threshold")
	}
	if n := len(mr.Keys()); n != 3 {
		t.Errorf("%d keys, want 3: %v", n, mr.Keys())
	}

	// A settlement that arrives after the counter expired recreates it
	// with the same expiry, not without one.
	mr.Del("sy:b:v1:acme/month:20261101")
	if err := st.Settle(ctx, a, 700); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got, _ := mr.Get("sy:b:v1:acme/month:20261101"); got != "100" {
		t.Errorf("recreated counter = %q, want 100", got)
	}
	if ttl := mr.TTL("sy:b:v1:acme/month:20261101"); ttl != (19*24+12)*time.Hour+48*time.Hour {
		t.Errorf("recreated counter TTL = %s", ttl)
	}
}

// Counters near config.MaxBudgetLimitUSD must stay exact: the script must
// pass amounts on as strings, since Redis may format a Lua number with an
// exponent.
func TestRedisStoreLargeAmountsAreExact(t *testing.T) {
	_, client := newMiniredis(t, nil)
	st := NewRedisStore(client)
	s := scope("acme", WindowMonth, 1_000_000_000_000_000, true)
	const est = 987_654_321_987_654
	a := mustAdmit(t, st, []Scope{s}, est, noon)
	if got := spent(t, st, s, noon); got != est {
		t.Errorf("spent = %d, want %d", got, est)
	}
	if err := st.Delta(context.Background(), &a, 12_345_678_012_346, noon); err != nil {
		t.Fatalf("Delta to exactly the limit: %v", err)
	}
	if got := spent(t, st, s, noon); got != 1_000_000_000_000_000 {
		t.Errorf("spent = %d, want the limit", got)
	}
	_, err := st.Admit(context.Background(), []Scope{s}, 1, noon)
	wantExhausted(t, err, "acme/month", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
}

func TestRedisStoreUnavailable(t *testing.T) {
	ctx := context.Background()
	mr, client := newMiniredis(t, &redis.Options{MaxRetries: -1, DialTimeout: time.Second})
	st := NewRedisStore(client)
	s := scope("acme", WindowDay, 1000, true)
	a := mustAdmit(t, st, []Scope{s}, 100, noon)
	mr.Close()

	_, err := st.Admit(ctx, []Scope{s}, 100, noon)
	checks := map[string]error{
		"Admit":  err,
		"Delta":  st.Delta(ctx, &a, 10, noon),
		"Settle": st.Settle(ctx, a, 50),
	}
	_, checks["Spent"] = st.Spent(ctx, s, noon)
	for op, err := range checks {
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s with Redis down = %v, want ErrUnavailable", op, err)
		}
		if errors.Is(err, ErrExhausted) {
			t.Errorf("%s with Redis down matches ErrExhausted", op)
		}
	}
	if a.Charged() != 100 {
		t.Errorf("charged after a failed Delta = %d, want 100", a.Charged())
	}
	// Admitting nothing needs no store.
	if _, err := st.Admit(ctx, nil, 100, noon); err != nil {
		t.Errorf("Admit(no scopes) with Redis down = %v, want nil", err)
	}
}

func TestRedisStoreCanceledContext(t *testing.T) {
	_, client := newMiniredis(t, nil)
	st := NewRedisStore(client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := st.Admit(ctx, []Scope{scope("acme", WindowDay, 1000, true)}, 1, noon)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Errorf("Admit with a canceled context = %v, want context.Canceled and not ErrUnavailable", err)
	}
}
