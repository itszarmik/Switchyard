package usage

import (
	"context"
	"errors"
	"time"

	"switchyard/internal/config"
)

// Cache statuses recorded in Event.CacheStatus. Until Phase 6 adds the
// caches, every event is a miss. Reports count CacheStatusHit as a cache
// hit; any other value is not.
const (
	CacheStatusMiss = "miss"
	CacheStatusHit  = "hit"
)

// Event is the ledger record of one gateway request (DESIGN §12.2),
// written once when the request completes, whatever its outcome.
type Event struct {
	// ID is the gateway request ID, a UUIDv7.
	ID string
	// Time is when the request arrived. Reports bucket and filter on it in
	// UTC.
	Time time.Time

	// Tenant and KeyID identify the caller.
	Tenant, KeyID string
	// Endpoint is the API endpoint, for example "/v1/chat/completions".
	Endpoint string

	// RequestedModel is the model field the client sent. Alias is the alias
	// it resolved through, or "".
	RequestedModel, Alias string
	// Deployment (model@provider) and Provider served the request. A
	// request no upstream served has the deployment and provider of the
	// last attempt that reached an upstream and may have been billed, such
	// as one its client went away from, or "" if there was none.
	Deployment, Provider string

	// Stream reports whether the response was streamed.
	Stream bool
	// Status is the HTTP status the client received.
	Status int
	// Attempts is the number of upstream attempts, including retries and
	// fallbacks.
	Attempts int
	// FallbackReason is the failure kind that caused the first fallback,
	// or "" if the request did not fall back. RouteReason explains the
	// routing decision.
	FallbackReason, RouteReason string
	// CacheStatus is CacheStatusMiss until Phase 6.
	CacheStatus string

	// Tokens are the billed token counts. UsageKnown reports whether the
	// upstream reported its whole usage; UsageEstimated that Tokens are, at
	// least in part, an estimate: because it reported none (EstimateTokens),
	// only part of it (a stream that broke off), or never served the
	// request it may have billed.
	Tokens                     Tokens
	UsageKnown, UsageEstimated bool

	// Cost is what the request cost upstream. CostKnown is false when the
	// deployment's pricing is unknown, and Cost is then 0; and when an
	// upstream had the request but never served it, and Cost is then its
	// estimate. Reports count such events in Totals.UnpricedRequests, at 0.
	// A request no upstream had, or that every upstream refused, costs 0,
	// which is known.
	Cost      Money
	CostKnown bool
	// Pricing is a snapshot of the prices Cost was computed from, so later
	// catalog edits never rewrite history.
	Pricing config.Pricing

	// BaselineModel and BaselineCost are what the request would have cost
	// without Switchyard's routing (DESIGN §12.4): in Phase 4 the requested
	// model, priced at its cheapest priced deployment. OverheadCost is the
	// gateway's own spend on the request (0 in Phase 4).
	BaselineModel string
	BaselineCost  Money
	OverheadCost  Money

	// LatencyMS is the request's duration and TTFTMS its time to first
	// token, in milliseconds (0 when not applicable).
	LatencyMS, TTFTMS float64

	// Tags are the client's attribution tags (X-Switchyard-Tags).
	Tags map[string]string

	// ConfigVersion identifies the config snapshot that served the request.
	ConfigVersion string
}

// Sink records usage events. Implementations are safe for concurrent use.
type Sink interface {
	// Record queues ev and never blocks. When the queue is full the oldest
	// queued event is dropped and counted. Record takes ownership of ev:
	// the caller must not modify it, or its Tags, afterwards.
	Record(ev *Event)
	// Close writes the queued events and releases the sink's resources,
	// giving up when ctx is done. Events recorded after Close are dropped.
	Close(ctx context.Context) error
}

// Reporter aggregates recorded usage.
type Reporter interface {
	// Report aggregates the events matching q. It returns an error wrapping
	// ErrInvalidQuery when q is malformed, and ErrReportBusy when the store
	// is running as many reports as it allows.
	Report(ctx context.Context, q Query) (*Report, error)
}

// Group-by dimensions accepted in Query.GroupBy. A tag dimension is
// GroupByTagPrefix followed by a tag key, for example "tag:team".
const (
	GroupByDay         = "day"          // the UTC date of Event.Time, YYYY-MM-DD
	GroupByModel       = "model"        // Event.RequestedModel
	GroupByDeployment  = "deployment"   // Event.Deployment
	GroupByProvider    = "provider"     // Event.Provider
	GroupByKey         = "key"          // Event.KeyID
	GroupByEndpoint    = "endpoint"     // Event.Endpoint
	GroupByStatusClass = "status_class" // "2xx", "4xx", "5xx", …, or "other"
	GroupByCacheStatus = "cache_status" // Event.CacheStatus
	GroupByTagPrefix   = "tag:"         // the tag's value, or "" if absent
)

// DefaultReportLimit is the number of groups a Query with Limit 0 returns,
// and MaxReportLimit the most any Query returns.
const (
	DefaultReportLimit = 1000
	MaxReportLimit     = 10000
)

// MaxGroupBy is the most dimensions a Query may group by, and
// MaxGroupByTags the most of them that may be tag dimensions. They bound
// the work one report can ask of the store.
const (
	MaxGroupBy     = 8
	MaxGroupByTags = 3
)

// ErrInvalidQuery is wrapped by the errors Report returns for a malformed
// Query.
var ErrInvalidQuery = errors.New("usage: invalid query")

// ErrReportBusy is returned by Report when too many reports are running
// already; the caller may retry later.
var ErrReportBusy = errors.New("usage: too many usage reports are running")

// Query selects and groups events for a Report.
type Query struct {
	// Tenant restricts the report to one tenant. "" means every tenant,
	// which only the admin API may ask for; every tenant-facing caller
	// must set it.
	Tenant string
	// From and To bound Event.Time to [From, To). Both are required and
	// From must be before To.
	From, To time.Time
	// GroupBy lists the dimensions to group by, at most once each: any of
	// the GroupBy constants, and tag dimensions. Empty means totals only.
	// At most MaxGroupBy dimensions, of which at most MaxGroupByTags are
	// tag dimensions.
	GroupBy []string
	// Limit caps the number of groups: DefaultReportLimit when 0, at most
	// MaxReportLimit.
	Limit int
}

// Report is the result of a Query.
type Report struct {
	// From, To and GroupBy echo the query, with times in UTC.
	From, To time.Time
	GroupBy  []string
	// Totals cover every matching event, including those in groups beyond
	// the limit.
	Totals Totals
	// Groups are sorted by Totals.CostUSD descending, then by their key
	// values in GroupBy order, compared bytewise. Nil without GroupBy.
	Groups []Group
}

// Totals aggregates a set of events.
type Totals struct {
	// Requests counts the events; Errors those with status 400 or above;
	// UnpricedRequests those whose cost is unknown.
	Requests, Errors, UnpricedRequests int64
	// Tokens sums the token counts.
	Tokens Tokens
	// CostUSD is the known upstream cost plus overhead, so that
	// SavingsUSD.Net = BaselineCostUSD - CostUSD (DESIGN §12.5).
	CostUSD Money
	// BaselineCostUSD sums the baseline cost of events with a known cost.
	BaselineCostUSD Money
	// OverheadUSD sums the gateway's own spend.
	OverheadUSD Money
	// SavingsUSD splits the savings (DESIGN §12.4). Routing is baseline
	// minus cost over priced events that were not cache hits, and can be
	// negative; Cache is the baseline cost of cache hits; Net is Routing +
	// Cache - OverheadUSD.
	SavingsUSD struct{ Routing, Cache, Net Money }
	// CacheHitRate and FallbackRate are the fractions of requests that were
	// cache hits and that fell back to another deployment; 0 without
	// requests.
	CacheHitRate, FallbackRate float64
}

// Group is the totals of the events that share one key.
type Group struct {
	// Key maps each GroupBy dimension to this group's value.
	Key    map[string]string
	Totals Totals
}
