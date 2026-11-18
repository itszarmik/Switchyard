package usage

import (
	"context"
	"errors"
	"time"

	"switchyard/internal/config"
)

// Cache statuses recorded in Event.CacheStatus, the same values as the
// X-Switchyard-Cache response header (internal/cache). Reports count both
// CacheStatusExact and CacheStatusSemantic as cache hits (see IsCacheHit);
// any other value is not one.
const (
	// CacheStatusMiss: the response came from an upstream, or the request
	// was not looked up.
	CacheStatusMiss = "miss"
	// CacheStatusExact: served from the exact cache.
	CacheStatusExact = "exact"
	// CacheStatusSemantic: served from the semantic cache.
	CacheStatusSemantic = "semantic"
)

// IsCacheHit reports whether status is a cache hit: CacheStatusExact or
// CacheStatusSemantic.
func IsCacheHit(status string) bool {
	return status == CacheStatusExact || status == CacheStatusSemantic
}

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
	// CacheStatus is CacheStatusMiss, CacheStatusExact or
	// CacheStatusSemantic. A hit costs 0, and its baseline cost, the hit's
	// tokens at the baseline model's price, counts as cache savings
	// (DESIGN §12.4).
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
	// without Switchyard's routing (DESIGN §12.4): the requested model for
	// a concrete model name, or the policy's baseline_model for auto, with
	// the same tokens priced at its cheapest priced deployment.
	// OverheadCost is the gateway's own spend on the request (0 until the
	// gateway makes calls of its own).
	BaselineModel string
	BaselineCost  Money
	OverheadCost  Money

	// Profile is the routing profile in force for a routed request (chat
	// and legacy completions), or "". RoutingMode is the policy's routing
	// mode, RoutingModeEnforce or RoutingModeObserve, or "" when no policy
	// applied (a request rejected before routing).
	Profile, RoutingMode string
	// ShadowDeployment is, in observe mode, the deployment the router
	// would have chosen (DESIGN §9.6), or "". ShadowCost is the request's
	// tokens priced at that deployment, the same approximation as
	// BaselineCost; ShadowCostKnown is false when the request's cost or the
	// shadow deployment's prices are unknown, and ShadowCost is then 0.
	// Reports sum BaselineCost − ShadowCost over observe-mode events whose
	// cost and shadow cost are known as the projected savings.
	ShadowDeployment string
	ShadowCost       Money
	ShadowCostKnown  bool

	// Category is the quality category the request was assessed in
	// (DESIGN §10.3), or "" for a request that was not routed. Reports
	// attribute shadow verification and verified savings by it.
	Category string

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

// Routing modes recorded in Event.RoutingMode.
const (
	RoutingModeEnforce = "enforce"
	RoutingModeObserve = "observe"
)

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
	GroupByProfile     = "profile"      // Event.Profile
	GroupByRoutingMode = "routing_mode" // Event.RoutingMode
	GroupByCategory    = "category"     // Event.Category
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
	// Verification decides which categories' savings count as verified.
	// Zero fields take the defaults of evaluation.shadow
	// (config.DefaultVerifiedMinSamples and DefaultVerifiedMinPassRate);
	// MinSamples must not be negative and MinPassRate must be at most 1.
	Verification Verification
}

// Report is the result of a Query.
type Report struct {
	// From, To and GroupBy echo the query, with times in UTC, and
	// Verification echoes it with the defaults applied.
	From, To     time.Time
	GroupBy      []string
	Verification Verification
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
	// CostUSD is the known upstream cost plus overhead (ShadowCostUSD
	// included), so that SavingsUSD.Net = BaselineCostUSD - CostUSD
	// (DESIGN §12.5).
	CostUSD Money
	// BaselineCostUSD sums the baseline cost of events with a known cost.
	BaselineCostUSD Money
	// OverheadUSD sums the gateway's own spend: the events' overhead and
	// ShadowCostUSD.
	OverheadUSD Money
	// SavingsUSD splits the savings (DESIGN §12.4). Routing is baseline
	// minus cost over priced events that were not cache hits, and can be
	// negative; Cache is the baseline cost of cache hits; Net is Routing +
	// Cache - OverheadUSD.
	SavingsUSD struct{ Routing, Cache, Net Money }
	// ProjectedSavingsUSD is what observe mode estimates routing would
	// have saved (DESIGN §12.4): BaselineCost − ShadowCost over the
	// observe-mode events whose cost and shadow cost are known. It is
	// projected, not realized: the shadow deployment never served those
	// requests, and its quality is unverified. It is not part of
	// SavingsUSD.
	ProjectedSavingsUSD Money
	// CacheHitRate and FallbackRate are the fractions of requests that were
	// cache hits and that fell back to another deployment; 0 without
	// requests.
	CacheHitRate, FallbackRate float64

	// Quality is the shadow verification of these requests (DESIGN §10.7),
	// per category in category order, for every category with a shadow
	// evaluation among them; nil when there is none. A shadow evaluation
	// counts in the category of its request's event.
	Quality []CategoryQuality
	// VerifiedSavingsUSD is the part of the savings that shadow sampling
	// verified: SavingsUSD.Routing plus ProjectedSavingsUSD, summed over
	// the requests whose category is Verified in Quality. It is reported
	// beside those totals and never replaces them. Each set of totals (the
	// report's, or a group's) decides verification from its own samples.
	VerifiedSavingsUSD Money
	// ShadowCostUSD sums the cost of these requests' shadow evaluations
	// (comparison and judge calls). It is the part of OverheadUSD (and so
	// of CostUSD) that shadow sampling spent, broken out.
	ShadowCostUSD Money
	// Feedback counts the feedback given on these requests, by score.
	Feedback FeedbackCounts
}

// Group is the totals of the events that share one key.
type Group struct {
	// Key maps each GroupBy dimension to this group's value.
	Key    map[string]string
	Totals Totals
}
