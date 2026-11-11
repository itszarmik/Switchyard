package budget

import (
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/config"
)

// price parses a config.Price or fails the test.
func price(t *testing.T, s string) config.Price {
	t.Helper()
	p, err := config.ParsePrice(s)
	if err != nil {
		t.Fatalf("ParsePrice(%q): %v", s, err)
	}
	return p
}

func TestScopesFor(t *testing.T) {
	tenant := config.Tenant{
		ID:   "acme",
		Keys: []config.APIKey{{ID: "k1"}},
		Budgets: []config.Budget{
			{Scope: "tenant", Window: "month", LimitUSD: price(t, "2000"), SoftAt: 0.8, OnExhausted: "block"},
			{Scope: "key", Window: "day", LimitUSD: price(t, "1.5"), OnExhausted: "notify"},
			{Scope: "tenant", Window: "day", LimitUSD: price(t, "0.0000001"), SoftAt: 0.5, OnExhausted: "block"},
		},
	}
	tests := []struct {
		name  string
		keyID string
		want  []Scope
	}{
		{"with a key", "k1", []Scope{
			{Tenant: "acme", Window: "month", LimitMicro: 2_000_000_000, SoftAtMicro: 1_600_000_000, Block: true, Name: "acme/month"},
			{Tenant: "acme", KeyID: "k1", Window: "day", LimitMicro: 1_500_000, Block: false, Name: "acme/key:k1/day"},
			{Tenant: "acme", Window: "day", LimitMicro: 1, SoftAtMicro: 1, Block: true, Name: "acme/day"},
		}},
		{"without a key", "", []Scope{
			{Tenant: "acme", Window: "month", LimitMicro: 2_000_000_000, SoftAtMicro: 1_600_000_000, Block: true, Name: "acme/month"},
			{Tenant: "acme", Window: "day", LimitMicro: 1, SoftAtMicro: 1, Block: true, Name: "acme/day"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, ScopesFor(tenant, tt.keyID)); diff != "" {
				t.Errorf("ScopesFor (-want +got):\n%s", diff)
			}
		})
	}
	if got := ScopesFor(config.Tenant{ID: "empty"}, "k"); len(got) != 0 {
		t.Errorf("ScopesFor(no budgets) = %v, want none", got)
	}
}

func TestMicroCeiling(t *testing.T) {
	tests := []struct {
		limit string
		soft  float64
		want  int64
		wantS int64
	}{
		{"1", 0, 1_000_000, 0},
		{"2000", 0.8, 2_000_000_000, 1_600_000_000},
		{"0.000001", 0, 1, 0},
		{"0.0000001", 0, 1, 0},
		{"0.000000001", 0, 1, 0},
		{"0.0000015", 0, 2, 0},
		{"1.000001", 0, 1_000_001, 0},
		{"1.000001001", 0.5, 1_000_002, 500_001},
		{"0.000003", 0.1, 3, 1},            // 0.3 rounds up
		{"10", 0.3, 10_000_000, 3_000_000}, // 0.3 is exactly 3/10, not the float nearest it
		{"1000000000", 0.999999, 1_000_000_000_000_000, 999_999_000_000_000},
	}
	for _, tt := range tests {
		t.Run(tt.limit, func(t *testing.T) {
			b := config.Budget{Scope: "tenant", Window: "day", LimitUSD: price(t, tt.limit), SoftAt: tt.soft}
			got := ScopesFor(config.Tenant{ID: "a", Budgets: []config.Budget{b}}, "")[0]
			if got.LimitMicro != tt.want || got.SoftAtMicro != tt.wantS {
				t.Errorf("limit %s soft %v = %d / %d micro-USD, want %d / %d", tt.limit, tt.soft, got.LimitMicro, got.SoftAtMicro, tt.want, tt.wantS)
			}
		})
	}
	huge, _ := new(big.Rat).SetString("100000000000000")
	if got := microCeil(huge); got != math.MaxInt64 {
		t.Errorf("microCeil(1e14 USD) = %d, want MaxInt64", got)
	}
}

func TestResetAt(t *testing.T) {
	tests := []struct {
		window string
		now    time.Time
		want   time.Time
	}{
		{WindowDay, time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)},
		{WindowDay, time.Date(2026, 11, 11, 23, 59, 59, 999_999_999, time.UTC), time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)},
		{WindowDay, time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{WindowMonth, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
		{WindowMonth, time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		{WindowMonth, time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		// 2026-12-01 03:00 at UTC+5 is still November in UTC.
		{WindowMonth, time.Date(2026, 12, 1, 3, 0, 0, 0, time.FixedZone("UTC+5", 5*3600)), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.window+" "+tt.now.Format(time.RFC3339Nano), func(t *testing.T) {
			got := ResetAt(Scope{Window: tt.window}, tt.now)
			if !got.Equal(tt.want) || got.Location() != time.UTC {
				t.Errorf("ResetAt = %s, want %s in UTC", got, tt.want)
			}
		})
	}
}

func TestExhaustedError(t *testing.T) {
	err := error(&ExhaustedError{Scope: Scope{Name: "acme/key:k1/day"}, ResetAt: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)})
	if !errors.Is(err, ErrExhausted) {
		t.Error("errors.Is(ExhaustedError, ErrExhausted) = false")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Error("errors.Is(ExhaustedError, ErrUnavailable) = true")
	}
	if want := "budget acme/key:k1/day exhausted until 2026-11-12T00:00:00Z"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}
