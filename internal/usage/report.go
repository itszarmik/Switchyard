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
	v := &q.Verification
	switch {
	case v.MinSamples < 0:
		return Query{}, fmt.Errorf("%w: the verification's minimum number of samples must not be negative", ErrInvalidQuery)
	case v.MinSamples == 0:
		v.MinSamples = defaultVerification.MinSamples
	}
	switch {
	case !(v.MinPassRate >= 0 && v.MinPassRate <= 1): // NaN fails both
		return Query{}, fmt.Errorf("%w: the verification's minimum pass rate must be between 0 and 1", ErrInvalidQuery)
	case v.MinPassRate == 0:
		v.MinPassRate = defaultVerification.MinPassRate
	}
	q.From, q.To = q.From.UTC(), q.To.UTC()
	q.GroupBy = slices.Clone(q.GroupBy)
	return q, nil
}

// validGroupBy reports whether g is a dimension Query.GroupBy accepts.
func validGroupBy(g string) bool {
	switch g {
	case GroupByDay, GroupByModel, GroupByDeployment, GroupByProvider, GroupByKey,
		GroupByEndpoint, GroupByStatusClass, GroupByCacheStatus, GroupByProfile, GroupByRoutingMode,
		GroupByCategory:
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
	case GroupByProfile:
		return ev.Profile
	case GroupByRoutingMode:
		return ev.RoutingMode
	case GroupByCategory:
		return ev.Category
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
	// projected sums the projected savings of observe-mode events.
	projected Money

	// cats holds the shadow verification sums of each of
	// reportCategories, by the category of the events.
	cats [len(reportCategories)]categorySums
	// shadowCost sums the joined shadow evaluations' cost.
	shadowCost Money
	// feedback counts the joined feedback by score + 1.
	feedback [3]int64
}

// categorySums are the sums behind one CategoryQuality.
type categorySums struct {
	judged, acceptable, errors int64
	// savings sums the routing and projected savings of the category's
	// events, which count as verified if the category is.
	savings Money
}

// addJoined adds one event and the shadow evaluation and feedback that
// join to it, either of which may be nil.
func (a *accumulator) addJoined(ev *Event, se *ShadowEval, fb *Feedback) {
	a.add(ev)
	ci := categoryIndex(ev.Category)
	if ci >= 0 && ev.CostKnown {
		var s Money
		if !IsCacheHit(ev.CacheStatus) {
			s = ev.BaselineCost.Sub(ev.Cost)
		}
		if ev.RoutingMode == RoutingModeObserve && ev.ShadowCostKnown {
			s = s.Add(ev.BaselineCost.Sub(ev.ShadowCost))
		}
		a.cats[ci].savings = a.cats[ci].savings.Add(s)
	}
	if se != nil {
		a.shadowCost = a.shadowCost.Add(se.Cost)
		if ci >= 0 {
			c := &a.cats[ci]
			switch {
			case se.ErrorKind != "":
				c.errors++
			case se.Acceptable:
				c.judged++
				c.acceptable++
			default:
				c.judged++
			}
		}
	}
	if fb != nil && fb.Score >= ScoreNegative && fb.Score <= ScorePositive {
		a.feedback[fb.Score+1]++
	}
}

// add adds one event.
func (a *accumulator) add(ev *Event) {
	a.requests++
	if ev.Status >= 400 {
		a.errors++
	}
	hit := IsCacheHit(ev.CacheStatus)
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
	if ev.RoutingMode == RoutingModeObserve && ev.ShadowCostKnown {
		a.projected = a.projected.Add(ev.BaselineCost.Sub(ev.ShadowCost))
	}
}

// totals returns the aggregate, with the categories that meet v verified.
func (a *accumulator) totals(v Verification) Totals {
	// Shadow and judge calls are the gateway's own spend (DESIGN §12.4):
	// they count as overhead, joined from the shadow evaluations at report
	// time because they finish after their request's event is written.
	overhead := a.overhead.Add(a.shadowCost)
	t := Totals{
		Requests:         a.requests,
		Errors:           a.errors,
		UnpricedRequests: a.unpriced,
		Tokens:           a.tokens,
		CostUSD:          a.cost.Add(overhead),
		BaselineCostUSD:  a.baseline,
		OverheadUSD:      overhead,
	}
	t.SavingsUSD.Routing = a.routing
	t.SavingsUSD.Cache = a.cache
	t.SavingsUSD.Net = a.routing.Add(a.cache).Sub(overhead)
	t.ProjectedSavingsUSD = a.projected
	if a.requests > 0 {
		t.CacheHitRate = float64(a.cacheHits) / float64(a.requests)
		t.FallbackRate = float64(a.fallbacks) / float64(a.requests)
	}
	for i, c := range a.cats {
		if c.judged == 0 && c.errors == 0 {
			continue
		}
		q := CategoryQuality{
			Category: reportCategories[i], Judged: c.judged, Acceptable: c.acceptable, Errors: c.errors,
			WilsonLower: WilsonLower(c.acceptable, c.judged), Verified: v.verified(c.acceptable, c.judged),
		}
		if c.judged > 0 {
			q.PassRate = float64(c.acceptable) / float64(c.judged)
		}
		if q.Verified {
			t.VerifiedSavingsUSD = t.VerifiedSavingsUSD.Add(c.savings)
		}
		t.Quality = append(t.Quality, q)
	}
	t.ShadowCostUSD = a.shadowCost
	t.Feedback = FeedbackCounts{Negative: a.feedback[0], Neutral: a.feedback[1], Positive: a.feedback[2]}
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
