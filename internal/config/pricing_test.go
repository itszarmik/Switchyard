package config

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestParseChangeFrom(t *testing.T) {
	tests := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"2026-12-01", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), true},
		{"2026-12-01T00:00:00Z", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), true},
		{"2026-12-01T00:00:00-08:00", time.Date(2026, 12, 1, 8, 0, 0, 0, time.UTC), true},
		{"2027-01-01T09:00:00+09:00", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{"2026-12-01T12:30:00.5Z", time.Date(2026, 12, 1, 12, 30, 0, 5e8, time.UTC), true},
		{"", time.Time{}, false},
		{"2026-13-01", time.Time{}, false},
		{"2026-12-1", time.Time{}, false},
		{"2026-12-01 00:00:00", time.Time{}, false},
		{"2026-12-01T00:00:00", time.Time{}, false}, // no offset
		{"01/12/2026", time.Time{}, false},
		{"tomorrow", time.Time{}, false},
	}
	for _, tt := range tests {
		got, err := ParseChangeFrom(tt.in)
		if (err == nil) != tt.ok || !got.Equal(tt.want) {
			t.Errorf("ParseChangeFrom(%q) = %v, %v; want %v, ok %v", tt.in, got, err, tt.want, tt.ok)
		}
		if err != nil && err.Error() != errChangeFrom.Error() {
			t.Errorf("ParseChangeFrom(%q) error %q", tt.in, err)
		}
	}
}

func TestPricingAt(t *testing.T) {
	p := Pricing{
		Input: mustPrice(t, "2.00"), Output: mustPrice(t, "10.00"), CachedInput: mustPrice(t, "0.10"),
		Source: "https://example.com/p", Checked: "2026-11-11",
		Changes: []PriceChange{
			{From: "2026-12-01", Input: mustPrice(t, "4.00"), Output: mustPrice(t, "20.00")},
			{From: "2027-01-01T00:00:00+01:00", CacheWrite: mustPrice(t, "1")},
			{From: "2027-02-01", Output: mustPrice(t, "30")},
		},
	}
	base := Pricing{Input: p.Input, Output: p.Output, CachedInput: p.CachedInput, Source: p.Source, Checked: p.Checked}
	first := base
	first.Input, first.Output = mustPrice(t, "4.00"), mustPrice(t, "20.00")
	second := first
	second.CacheWrite = mustPrice(t, "1")
	third := second
	third.Output = mustPrice(t, "30")
	tests := []struct {
		at       time.Time
		want     Pricing
		nextIdx  int
		nextTime time.Time
	}{
		{time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC), base, 0, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), first, 1, time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC), second, 2, time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC), third, -1, time.Time{}},
	}
	for _, tt := range tests {
		if diff := cmp.Diff(tt.want, p.At(tt.at)); diff != "" {
			t.Errorf("At(%v) (-want +got):\n%s", tt.at, diff)
		}
		i, next, ok := p.NextChange(tt.at)
		if i != tt.nextIdx || !next.Equal(tt.nextTime) || ok != (tt.nextIdx >= 0) {
			t.Errorf("NextChange(%v) = %d, %v, %v; want %d, %v", tt.at, i, next, ok, tt.nextIdx, tt.nextTime)
		}
	}
	// No changes: the prices themselves, and no next change.
	if diff := cmp.Diff(base, base.At(time.Now())); diff != "" {
		t.Errorf("At without changes (-want +got):\n%s", diff)
	}
	if _, _, ok := base.NextChange(time.Time{}); ok {
		t.Error("NextChange without changes reports one")
	}
}
