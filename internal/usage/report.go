package usage

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ValidTagKey reports whether s is a valid attribution tag key: 1 to 32
// characters, a lowercase ASCII letter or digit followed by lowercase
// letters, digits, '_', '.' or '-' (^[a-z0-9][a-z0-9_.-]{0,31}$).
func ValidTagKey(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case i > 0 && (c == '_' || c == '.' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// normalizeQuery validates q and returns it with times in UTC and Limit
// resolved. Errors wrap ErrInvalidQuery and never echo client input.
func normalizeQuery(q Query) (Query, error) {
	if q.From.IsZero() || q.To.IsZero() {
		return Query{}, fmt.Errorf("%w: from and to are required", ErrInvalidQuery)
	}
	if !q.From.Before(q.To) {
		return Query{}, fmt.Errorf("%w: from must be before to", ErrInvalidQuery)
	}
	switch {
	case q.Limit < 0:
		return Query{}, fmt.Errorf("%w: limit must not be negative", ErrInvalidQuery)
	case q.Limit == 0:
		q.Limit = DefaultReportLimit
	case q.Limit > MaxReportLimit:
		q.Limit = MaxReportLimit
	}
	if len(q.GroupBy) > MaxGroupBy {
		return Query{}, fmt.Errorf("%w: group_by may list at most %d dimensions", ErrInvalidQuery, MaxGroupBy)
	}
	seen := make(map[string]bool, len(q.GroupBy))
	tags := 0
	for i, g := range q.GroupBy {
		if !validGroupBy(g) {
			return Query{}, fmt.Errorf("%w: group_by entry %d is not a known dimension", ErrInvalidQuery, i+1)
		}
		if seen[g] {
			return Query{}, fmt.Errorf("%w: group_by entry %d repeats a dimension", ErrInvalidQuery, i+1)
		}
		seen[g] = true
		if strings.HasPrefix(g, GroupByTagPrefix) {
			if tags++; tags > MaxGroupByTags {
				return Query{}, fmt.Errorf("%w: group_by may list at most %d tag dimensions", ErrInvalidQuery, MaxGroupByTags)
			}
		}
	}
	q.From, q.To = q.From.UTC(), q.To.UTC()
	q.GroupBy = slices.Clone(q.GroupBy)
	return q, nil
}

// validGroupBy reports whether g is a dimension Query.GroupBy accepts.
func validGroupBy(g string) bool {
	switch g {
	case GroupByDay, GroupByModel, GroupByDeployment, GroupByProvider, GroupByKey,
		GroupByEndpoint, GroupByStatusClass, GroupByCacheStatus:
		return true
	}
	tag, ok := strings.CutPrefix(g, GroupByTagPrefix)
	return ok && ValidTagKey(tag)
}

// groupValue returns ev's value for the validated dimension g.
func groupValue(ev *Event, g string) string {
	switch g {
	case GroupByDay:
		return ev.Time.UTC().Format(time.DateOnly)
	case GroupByModel:
		return ev.RequestedModel
	case GroupByDeployment:
		return ev.Deployment
	case GroupByProvider:
		return ev.Provider
	case GroupByKey:
		return ev.KeyID
	case GroupByEndpoint:
		return ev.Endpoint
	case GroupByStatusClass:
		return statusClass(ev.Status)
	case GroupByCacheStatus:
		return ev.CacheStatus
	}
	return ev.Tags[strings.TrimPrefix(g, GroupByTagPrefix)]
}

// statusClass maps an HTTP status to "1xx" … "5xx", or "other" outside
// 100–599. The Postgres report computes the same in SQL.
func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "other"
	}
	return strconv.Itoa(status/100) + "xx"
}

// accumulator sums events, or the per-group sums read from Postgres, into
// Totals. Money fields hold exact sums.
type accumulator struct {
	requests, errors, unpriced, cacheHits, fallbacks int64
	tokens                                           Tokens
	// cost and baseline sum events with a known cost; overhead sums all.
	cost, baseline, overhead Money
	routing, cache           Money
}

// add adds one event.
func (a *accumulator) add(ev *Event) {
	a.requests++
	if ev.Status >= 400 {
		a.errors++
	}
	hit := ev.CacheStatus == CacheStatusHit
	if hit {
		a.cacheHits++
	}
	if ev.FallbackReason != "" {
		a.fallbacks++
	}
	a.tokens = a.tokens.Add(ev.Tokens)
	a.overhead = a.overhead.Add(ev.OverheadCost)
	if !ev.CostKnown {
		a.unpriced++
		return
	}
	a.cost = a.cost.Add(ev.Cost)
	a.baseline = a.baseline.Add(ev.BaselineCost)
	if hit {
		a.cache = a.cache.Add(ev.BaselineCost)
	} else {
		a.routing = a.routing.Add(ev.BaselineCost.Sub(ev.Cost))
	}
}

// totals returns the aggregate.
func (a *accumulator) totals() Totals {
	t := Totals{
		Requests:         a.requests,
		Errors:           a.errors,
		UnpricedRequests: a.unpriced,
		Tokens:           a.tokens,
		CostUSD:          a.cost.Add(a.overhead),
		BaselineCostUSD:  a.baseline,
		OverheadUSD:      a.overhead,
	}
	t.SavingsUSD.Routing = a.routing
	t.SavingsUSD.Cache = a.cache
	t.SavingsUSD.Net = a.routing.Add(a.cache).Sub(a.overhead)
	if a.requests > 0 {
		t.CacheHitRate = float64(a.cacheHits) / float64(a.requests)
		t.FallbackRate = float64(a.fallbacks) / float64(a.requests)
	}
	return t
}

// sortGroups orders groups by cost descending, then by key values in
// groupBy order, bytewise.
func sortGroups(groups []Group, groupBy []string) {
	slices.SortFunc(groups, func(a, b Group) int {
		if c := b.Totals.CostUSD.Cmp(a.Totals.CostUSD); c != 0 {
			return c
		}
		for _, g := range groupBy {
			if c := cmp.Compare(a.Key[g], b.Key[g]); c != 0 {
				return c
			}
		}
		return 0
	})
}
