package config

import (
	"errors"
	"time"
)

// errChangeFrom is the problem with a PriceChange.From that does not parse.
var errChangeFrom = errors.New("must be a date as YYYY-MM-DD (00:00 UTC) or an RFC 3339 timestamp such as 2026-12-01T00:00:00-08:00")

// ParseChangeFrom parses PriceChange.From: a date as YYYY-MM-DD, which
// means 00:00 UTC, or an RFC 3339 timestamp. Errors never include s.
func ParseChangeFrom(s string) (time.Time, error) {
	if t, err := time.Parse(DateLayout, s); err == nil {
		return t, nil // time.Parse gives UTC without a zone
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, errChangeFrom
	}
	return t, nil
}

// Sets reports whether the change gives at least one price.
func (c PriceChange) Sets() bool {
	return c.Input.IsSet() || c.Output.IsSet() || c.CachedInput.IsSet() || c.CacheWrite.IsSet()
}

// At returns the prices in force at t: p with every change whose From is
// at or before t applied in order, each replacing the prices it gives.
// The result has no Changes; Source and Checked are p's. A change whose
// From does not parse, which validation rejects, is ignored.
func (p Pricing) At(t time.Time) Pricing {
	out := p
	out.Changes = nil
	for _, c := range p.Changes {
		from, err := ParseChangeFrom(c.From)
		if err != nil || from.After(t) {
			continue
		}
		for _, f := range []struct{ dst, src *Price }{
			{&out.Input, &c.Input}, {&out.Output, &c.Output},
			{&out.CachedInput, &c.CachedInput}, {&out.CacheWrite, &c.CacheWrite},
		} {
			if f.src.IsSet() {
				*f.dst = *f.src
			}
		}
	}
	return out
}

// NextChange returns the index and time of the first change that takes
// effect after t, or ok false when there is none. Changes are in
// increasing From order, which validation enforces.
func (p Pricing) NextChange(t time.Time) (i int, from time.Time, ok bool) {
	for i, c := range p.Changes {
		if from, err := ParseChangeFrom(c.From); err == nil && from.After(t) {
			return i, from, true
		}
	}
	return -1, time.Time{}, false
}
