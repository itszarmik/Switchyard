package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"

	"switchyard/internal/apierror"
	"switchyard/internal/cache"
	"switchyard/internal/cache/semantic"
	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/policy"
	"switchyard/internal/reqctx"
	"switchyard/internal/router"
	"switchyard/internal/schema"
	"switchyard/internal/server"
	"switchyard/internal/tracing"
	"switchyard/internal/usage"
)

// Response caching (DESIGN §11) of chat completions: the lookup between
// policy resolution and routing (step 8), so that a hit is served even
// when the budget is exhausted or every deployment is unhealthy; serving
// a hit as JSON or as a replayed stream; coalescing identical concurrent
// non-streaming misses; and the asynchronous writes after a successful
// miss. Every cache failure is a miss: it is counted, logged at WARN at
// most once per cacheWarnEvery per layer, and never fails the request.
// Nothing here logs prompt or response content.

// HeaderCache is the X-Switchyard-Cache header. As a request header it
// narrows the policy for one request: off (no lookup, no write), no-store
// (look up, but don't write) or exact-only (skip the semantic layer). As a
// response header it is miss, exact or semantic, sent on every chat
// response of a request the cache layer saw, and omitted when caching is
// off for the request.
const HeaderCache = "X-Switchyard-Cache"

// Values of the X-Switchyard-Cache request header.
const (
	cacheHeaderOff       = "off"
	cacheHeaderNoStore   = "no-store"
	cacheHeaderExactOnly = "exact-only"
)

// Route reasons of cache hits.
const (
	reasonCacheExact    = "cache_exact"
	reasonCacheSemantic = "cache_semantic"
)

// Lookup results and write results, the metric labels. A lookup is
// counted policy when the request's effective policy forbids the deployment
// of the entry it found (see entryAllowed), or, for the semantic layer, the
// embedding deployment; and budget when the semantic layer is skipped
// because a blocking budget of the request has no room left.
const (
	resultHit          = "hit"
	resultMiss         = "miss"
	resultError        = "error"
	resultSkipped      = "skipped"
	resultPolicy       = "policy"
	resultBudget       = "budget"
	resultOK           = "ok"
	resultDropped      = "dropped"
	resultTooLarge     = "too_large"
	resultNotCacheable = "not_cacheable"
)

// CacheWriteTimeout bounds one asynchronous cache write. The cache Redis
// client's socket timeouts must not be shorter.
const CacheWriteTimeout = time.Second

const (
	// defaultCacheWriters bounds the cache writes in flight; a write that
	// finds them all busy is dropped and counted.
	defaultCacheWriters = 64
	// cacheWarnEvery is the most often a failing layer is logged.
	cacheWarnEvery = 10 * time.Second
	// purgeTimeout bounds the namespace bump of a purge.
	purgeTimeout = 2 * time.Second
	// maxPurgeBody bounds a purge request's body.
	maxPurgeBody = 4 << 10
	// auditPurge is the audit action of a purge.
	auditPurge = "cache.purge"
)

// CacheOptions are the gateway's response caches.
type CacheOptions struct {
	// Store holds the exact-cache entries: the cache Redis instance, or
	// in memory. Nil means a new in-memory store of
	// cache.memory_max_entries entries.
	Store cache.Store
	// Namespaces holds the tenants' namespace versions: the state Redis
	// instance, or in memory. Nil means new in-memory versions.
	Namespaces cache.Namespaces
	// Writers bounds the asynchronous writes in flight; 0 means 64.
	Writers int
}

// cacheLayer is the gateway's response caches and their settings.
type cacheLayer struct {
	store cache.Store
	ns    cache.Namespaces
	// sem is the semantic cache, or nil (see SetSemanticCache), and
	// embedding the deployment it embeds with.
	sem           *semantic.Cache
	embedding     *catalog.Deployment
	lookupTimeout time.Duration
	maxEntryBytes int
	coalescer     cache.Coalescer
	writes        writePool
	// warnAt holds, per layer (exact, semantic), the Unix time in
	// nanoseconds before which a failure is not logged again.
	warnAt [2]atomic.Int64
}

// newCacheLayer returns the caches of opts, with cfg's limits.
func newCacheLayer(cfg config.CacheConfig, opts CacheOptions, now func() time.Time) *cacheLayer {
	c := &cacheLayer{
		store: opts.Store, ns: opts.Namespaces,
		lookupTimeout: cfg.LookupTimeout.Std(), maxEntryBytes: cfg.MaxEntryBytes,
	}
	if c.lookupTimeout <= 0 {
		c.lookupTimeout = config.DefaultCacheLookupTimeout
	}
	if c.maxEntryBytes <= 0 {
		c.maxEntryBytes = config.DefaultCacheMaxEntryBytes
	}
	if c.store == nil {
		n := cfg.MemoryMaxEntries
		if n <= 0 {
			n = config.DefaultCacheMemoryMaxEntries
		}
		c.store = cache.NewMemoryStore(n, now)
	}
	if c.ns == nil {
		c.ns = cache.NewMemoryNamespaces()
	}
	writers := opts.Writers
	if writers <= 0 {
		writers = defaultCacheWriters
	}
	c.writes.slots = make(chan struct{}, writers)
	return c
}

// SetSemanticCache makes s the semantic cache of policies that enable it;
// embedding is the deployment s embeds with, which a request's effective
// policy must permit (by its allow, deny and require_tags) for its last
// user message to be sent there. It must be called before the gateway
// serves traffic. Without it, the semantic layer is skipped.
func (g *Gateway) SetSemanticCache(s *semantic.Cache, embedding *catalog.Deployment) {
	g.cache.sem, g.cache.embedding = s, embedding
}

// CloseCache stops taking cache writes and waits for those in flight,
// until ctx is done. It is called at shutdown, once the server has
// drained and before the usage ledger is flushed.
func (g *Gateway) CloseCache(ctx context.Context) error { return g.cache.writes.close(ctx) }

// cacheState is what the cache layer knows of one chat request.
type cacheState struct {
	// eligible is set when the cache layer saw the request: its policy
	// enables a layer, its header is not off and deterministic_only
	// passes. Its responses then carry X-Switchyard-Cache.
	eligible bool
	// status is the request's cache status: miss, exact or semantic; ""
	// for a request the cache layer did not see.
	status string
	// noStore and exactOnly are the request header's narrowing.
	noStore, exactOnly bool
	pol                config.CachePolicy
	// key is the exact key, when the exact layer is on and the namespace
	// version was read; partition and vec are the semantic partition and
	// the embedding, when a semantic lookup embedded the message.
	key       string
	partition string
	vec       []float32
	// overhead is the embedding's cost, charged as the request's overhead.
	overhead usage.Money
	// entry is the cached response of a hit.
	entry *cache.Entry
	// body is a successful non-streaming miss's upstream body. agg folds a
	// streamed miss's chunks, while a write may happen; aggBytes counts the
	// raw size of the chunks it took, and aggOver is set once that passed
	// max_entry_bytes, which stops the aggregation for good.
	body     []byte
	agg      *cache.Aggregator
	aggBytes int64
	aggOver  bool
}

// parseCacheHeader reads the X-Switchyard-Cache request header: "" when
// absent, else off, no-store or exact-only, given once. Anything else is
// 400 invalid_header.
func parseCacheHeader(h http.Header) (string, *Error) {
	values := h.Values(HeaderCache)
	switch {
	case len(values) == 0:
		return "", nil
	case len(values) > 1:
		return "", invalidHeader(HeaderCache, "must be given once")
	}
	switch v := strings.ToLower(strings.TrimSpace(values[0])); v {
	case cacheHeaderOff, cacheHeaderNoStore, cacheHeaderExactOnly:
		return v, nil
	}
	return "", invalidHeader(HeaderCache, "must be off, no-store or exact-only")
}

// cacheTarget is the resolved target in a request's cache key: the
// catalog model ID, or for auto
//
//	auto:<profile>@<policy>#q=<quality floor>,t=<max TTFT ms>,c=<max cost nano-USD>
//
// with every constraint of the effective policy that a rule or a header
// can tighten, so that a request that demands more (a higher quality
// floor, a shorter TTFT target, a lower cost cap) never shares an entry
// with one that did not. The profile is the one the policy resolved,
// before any budget downgrade, so a lookup never waits for the budget
// store; a downgraded answer is never written (see cacheEntry). A concrete
// model's key needs none of these: the model is fixed, and entryAllowed
// checks the entry's deployment against the request's policy.
func cacheTarget(rt *routed) string {
	if !rt.auto {
		return rt.requested
	}
	c := &rt.eff.Constraints
	return "auto:" + rt.eff.Profile + "@" + rt.eff.PolicyID +
		"#q=" + strconv.FormatFloat(c.QualityFloor, 'g', -1, 64) +
		",t=" + strconv.FormatInt(c.MaxTTFT.Milliseconds(), 10) +
		",c=" + strconv.FormatInt(rt.eff.MaxCostPerRequestNano, 10)
}

// entryAllowed reports whether the request routed as rt could have been
// served by the deployment that produced e under its own effective policy,
// as a cached answer must: the deployment is still in the catalog and
// passes the policy's allow, deny and require_tags (router.PolicyReason),
// and a concrete model routed pinned (substitution pinned, a
// model@provider, or observe mode) is answered only by one of its own
// candidates. Under substitution equivalent, and for auto, any deployment
// the filters permit will do, except that auto in observe mode is served
// by the baseline model. Health and budgets are not checked: a hit is
// served whatever they say.
func (g *snapshot) entryAllowed(rt *routed, e *cache.Entry) bool {
	res, err := g.catalog.Resolve(e.Deployment)
	if err != nil || !res.Pinned || len(res.Candidates) != 1 || res.Candidates[0].ID != e.Deployment {
		return false
	}
	d := res.Candidates[0]
	if d.Model.Type != chatType || router.PolicyReason(&rt.eff, d) != "" {
		return false
	}
	switch {
	case rt.auto && rt.observe():
		return rt.baseline != nil && d.Model.ID == rt.baseline.ID
	case rt.auto:
		return true
	case rt.res.Pinned || rt.observe() || rt.eff.Substitution != policy.SubstitutionEquivalent:
		return slices.ContainsFunc(rt.res.Candidates, func(c *catalog.Deployment) bool { return c.ID == d.ID })
	}
	return true
}

// requestUser returns the request's user field, or "" when it is absent
// or not a string.
func requestUser(req *schema.ChatRequest) string {
	raw, ok := req.Field("user")
	if !ok {
		return ""
	}
	var u string
	if json.Unmarshal(raw, &u) != nil {
		return ""
	}
	return u
}

// lookupCache looks the request up in its policy's caches (step 8): the
// exact cache, then, on an exact miss, the semantic cache. A hit is left
// in rec.cache.entry. It answers 400 invalid_header for a malformed
// X-Switchyard-Cache header, whatever the policy, and otherwise never
// fails: a lookup error or timeout is a miss.
//
// Only an authenticated request is looked up, since every key starts with
// its tenant. Each layer looked up is traced: tracing.SpanCacheExact, and
// tracing.SpanCacheSemantic with the semantic cache's embed and search
// spans under it.
func (g *snapshot) lookupCache(ctx context.Context, ex Exchange, rec *record, req *schema.ChatRequest, rt *routed) *Error {
	mode, e := parseCacheHeader(ex.Header())
	if e != nil {
		return e
	}
	pol := rt.eff.Cache
	if !rec.authenticated || (!pol.Exact.Enabled && !pol.Semantic.Enabled) {
		return nil
	}
	m := g.metrics
	if mode == cacheHeaderOff || (pol.Exact.DeterministicOnly && !cache.Deterministic(req)) {
		if pol.Exact.Enabled {
			m.cacheLookups.WithLabelValues(cache.LayerExact, resultSkipped).Inc()
		}
		if pol.Semantic.Enabled {
			m.cacheLookups.WithLabelValues(cache.LayerSemantic, resultSkipped).Inc()
		}
		return nil
	}
	c := g.cache
	st := &rec.cache
	st.eligible, st.status, st.pol = true, cache.StatusMiss, pol
	st.noStore, st.exactOnly = mode == cacheHeaderNoStore, mode == cacheHeaderExactOnly
	target := cacheTarget(rt)
	scope := cache.ScopeID(pol.Exact.Scope, rec.tenant, rec.keyID, requestUser(req))

	// The exact layer: the namespace version and the entry, both within
	// cache.lookup_timeout. The version is read even when only the
	// semantic layer is on, since its partitions include it.
	start := time.Now()
	ectx, espan := ctx, trace.Span(nil)
	if pol.Exact.Enabled {
		ectx, espan = tracing.Start(ctx, tracing.SpanCacheExact)
	}
	lctx, cancel := context.WithTimeout(ectx, c.lookupTimeout)
	nsver, err := c.ns.Version(lctx, rec.tenant)
	nsOK, storeFailed := err == nil, false
	switch {
	case err != nil:
		g.cacheFailed(ctx, cache.LayerExact, "cache namespace version unavailable; serving as a miss", err)
		if pol.Exact.Enabled {
			m.cacheLookups.WithLabelValues(cache.LayerExact, resultError).Inc()
		}
	case pol.Exact.Enabled:
		st.entry, storeFailed = g.lookupExact(ctx, lctx, rec, req, rt, target, scope, nsver)
	}
	cancel()
	if st.entry != nil {
		st.status = cache.StatusExact
	}
	if pol.Exact.Enabled {
		m.cacheLookupSeconds.WithLabelValues(cache.LayerExact).Observe(time.Since(start).Seconds())
		endCacheSpan(espan, st.entry != nil)
	}
	if st.entry != nil {
		return nil
	}

	// The semantic layer, on an exact miss, reusing the version. When the
	// exact read failed, the cache instance, which holds the vector index
	// too, is down or slow: the semantic layer is skipped, without paying
	// for an embedding.
	if !pol.Semantic.Enabled || c.sem == nil {
		return nil
	}
	if !nsOK || storeFailed {
		m.cacheLookups.WithLabelValues(cache.LayerSemantic, resultError).Inc()
		return nil
	}
	partition, text, ok := "", "", false
	if !st.exactOnly {
		partition, text, ok = cache.Partition(req, target, scope, nsver, pol.Semantic.MaxChars)
	}
	switch {
	case !ok:
		m.cacheLookups.WithLabelValues(cache.LayerSemantic, resultSkipped).Inc()
		return nil
	case c.embedding == nil || router.PolicyReason(&rt.eff, c.embedding) != "":
		// The request's policy does not let its message go to the
		// embedding deployment.
		m.cacheLookups.WithLabelValues(cache.LayerSemantic, resultPolicy).Inc()
		return nil
	case g.budgetExhausted(ctx, rec):
		// The embedding would be spend that the budget has no room for.
		m.cacheLookups.WithLabelValues(cache.LayerSemantic, resultBudget).Inc()
		return nil
	}
	start = time.Now()
	sctx, sspan := tracing.Start(ctx, tracing.SpanCacheSemantic)
	res := c.sem.Lookup(sctx, partition, text, pol.Semantic.MinSimilarity)
	m.cacheLookupSeconds.WithLabelValues(cache.LayerSemantic).Observe(time.Since(start).Seconds())
	endCacheSpan(sspan, res.Entry != nil && res.Err == nil && g.entryAllowed(rt, res.Entry))
	if res.EmbedCostNano > 0 {
		st.overhead = usage.FromNanoUSD(res.EmbedCostNano)
	}
	st.partition, st.vec = partition, res.Vector
	if !math.IsNaN(res.Similarity) {
		outcome := resultMiss
		if res.Entry != nil {
			outcome = resultHit
		}
		m.cacheSimilarity.WithLabelValues(outcome).Observe(res.Similarity)
	}
	switch {
	case res.Err != nil:
		if ctx.Err() == nil {
			g.cacheFailed(ctx, cache.LayerSemantic, "semantic cache lookup failed; serving as a miss", res.Err)
		}
		m.cacheLookups.WithLabelValues(cache.LayerSemantic, resultError).Inc()
	case res.Entry != nil && !g.entryAllowed(rt, res.Entry):
		m.cacheLookups.WithLabelValues(cache.LayerSemantic, resultPolicy).Inc()
	case res.Entry != nil:
		m.cacheLookups.WithLabelValues(cache.LayerSemantic, resultHit).Inc()
		st.entry, st.status = res.Entry, cache.StatusSemantic
	default:
		m.cacheLookups.WithLabelValues(cache.LayerSemantic, resultMiss).Inc()
	}
	return nil
}

// endCacheSpan ends the span of a cache layer's lookup, with its result,
// hit or miss, as switchyard.cache_status. It records nothing else: no
// key, partition or content.
func endCacheSpan(span trace.Span, hit bool) {
	if span.IsRecording() {
		status := cache.StatusMiss
		if hit {
			status = "hit"
		}
		span.SetAttributes(tracing.AttrCacheStatus.String(status))
	}
	span.End()
}

// lookupExact computes the request's exact key into rec.cache.key and
// reads it within lctx, returning the entry of a hit, and whether the
// store failed. A failure is counted and logged, and is a miss; so is an
// entry the request's policy forbids (see entryAllowed), counted policy.
func (g *snapshot) lookupExact(ctx, lctx context.Context, rec *record, req *schema.ChatRequest, rt *routed, target, scope string, nsver int64) (*cache.Entry, bool) {
	m := g.metrics
	canonical, err := cache.Canonical(req, target)
	if err != nil {
		// A field that is not valid JSON or UTF-8, or that has a lone
		// surrogate escape, cannot be keyed safely; the request is not
		// cached.
		m.cacheLookups.WithLabelValues(cache.LayerExact, resultSkipped).Inc()
		return nil, false
	}
	rec.cache.key = cache.ExactKey(scope, nsver, canonical)
	e, err := g.cache.store.Get(lctx, rec.cache.key)
	switch {
	case err != nil:
		g.cacheFailed(ctx, cache.LayerExact, "exact cache lookup failed; serving as a miss", err)
		m.cacheLookups.WithLabelValues(cache.LayerExact, resultError).Inc()
		return nil, true
	case e == nil:
		m.cacheLookups.WithLabelValues(cache.LayerExact, resultMiss).Inc()
		return nil, false
	case !g.entryAllowed(rt, e):
		m.cacheLookups.WithLabelValues(cache.LayerExact, resultPolicy).Inc()
		return nil, false
	}
	m.cacheLookups.WithLabelValues(cache.LayerExact, resultHit).Inc()
	return e, false
}

// budgetExhausted reports whether a blocking budget of the request's
// tenant or key has no room left, as routing reads the budgets (see
// budgetState): the semantic layer's embedding is then skipped, since it
// would be spend the budget cannot take. Only a request that reaches the
// semantic layer under a blocking budget reads the store; when the store
// fails, the budgets are unknown and the lookup goes ahead.
func (g *snapshot) budgetExhausted(ctx context.Context, rec *record) bool {
	if !blocks(g.scopes[principal{rec.tenant, rec.keyID}]) {
		return false
	}
	remaining, _ := g.budgetState(ctx, rec.tenant, rec.keyID)
	return remaining == 0
}

// cacheFailed logs a cache failure of layer at WARN, at most once per
// cacheWarnEvery per layer. The error never holds content: the stores and
// the embedder never put prompts or responses in their errors.
func (g *snapshot) cacheFailed(ctx context.Context, layer, msg string, err error) {
	i := 0
	if layer == cache.LayerSemantic {
		i = 1
	}
	now := g.now().UnixNano()
	next := g.cache.warnAt[i].Load()
	if now < next || !g.cache.warnAt[i].CompareAndSwap(next, now+int64(cacheWarnEvery)) {
		return
	}
	reqctx.Logger(ctx).WarnContext(ctx, msg, "layer", layer, "error", err)
}

// serveHit answers a request from its cache hit, rec.cache.entry: the
// cached body, or for a stream its replay, with the entry's model and
// provider, attempts 0, cost 0 (non-streaming only, as for any stream),
// the route reason cache_exact or cache_semantic, and X-Switchyard-Cache.
// No budget is admitted or charged, no upstream is called, and the
// request is not shadow sampled. It returns false, having answered
// nothing, when a cached stream body cannot be replayed: the request is
// then a miss.
func (g *snapshot) serveHit(ex Exchange, rec *record, req *schema.ChatRequest, rt *routed) bool {
	st := &rec.cache
	e := st.entry
	var events [][]byte
	if req.Stream() {
		var err error
		if events, err = cache.Replay(e.Body, req.IncludeUsage()); err != nil {
			layer := cache.LayerExact
			if st.status == cache.StatusSemantic {
				layer = cache.LayerSemantic
			}
			g.cacheFailed(rec.ctx, layer, "cached entry cannot be replayed; serving as a miss", err)
			st.entry, st.status = nil, cache.StatusMiss
			return false
		}
	}
	rec.useRoute(rt)
	info := &rec.info
	info.Model, info.Provider = e.Model, entryProvider(e.Deployment)
	// The cached body reflects the parameters the miss dropped, so the
	// hit reports them as the miss did.
	info.DroppedParams, rec.dropped = e.DroppedParams, e.DroppedParams
	info.RouteReason, info.Cache = reasonCacheSemantic, st.status
	if st.status == cache.StatusExact {
		info.RouteReason = reasonCacheExact
	}
	rec.baseReason = info.RouteReason
	rec.status = http.StatusOK
	if !req.Stream() {
		info.CostUSD = "0"
		start := time.Now()
		ex.Respond(*info, http.StatusOK, e.Body)
		rec.wroteClient(start)
		return true
	}
	start := time.Now()
	defer func() { rec.wroteClient(start) }()
	rec.committed = true
	if ex.StartStream(*info) != nil {
		rec.errorCode = codeClientGone
		return true
	}
	for _, data := range events {
		if ex.Send(data) != nil {
			rec.errorCode = codeClientGone
			return true
		}
	}
	if ex.Done() != nil {
		rec.errorCode = codeClientGone
	}
	return true
}

// entryProvider returns the provider of a deployment ID, model@provider.
func entryProvider(deployment string) string {
	if i := strings.LastIndexByte(deployment, '@'); i >= 0 {
		return deployment[i+1:]
	}
	return ""
}

// coalesces reports whether a request that missed shares its upstream
// call with identical concurrent misses: a non-streaming request with an
// exact key.
func (st *cacheState) coalesces(stream bool) bool {
	return st.eligible && !stream && st.key != ""
}

// startAggregating starts folding a streamed miss's chunks into a body to
// cache, when a write may happen: the request is eligible, its header
// permits storing, and it has an exact key or a semantic embedding.
func (st *cacheState) startAggregating(stream bool) {
	if st.eligible && stream && !st.noStore && (st.key != "" || st.vec != nil) {
		st.agg = cache.NewAggregator()
	}
}

// aggregate folds one chunk of a streamed miss into the body to cache.
// The aggregation is bounded by the raw size of the chunks it takes, which
// bounds what it holds whatever they carry (text, tool calls, logprobs):
// once that passes maxBytes, or a chunk does not fold, aggregation stops
// for good and nothing is cached.
func (st *cacheState) aggregate(c *schema.ChatChunk, maxBytes int) {
	if st.agg == nil {
		return
	}
	st.aggBytes += int64(len(c.Raw))
	if st.aggBytes > int64(maxBytes) {
		st.agg, st.aggOver = nil, true
		return
	}
	if st.agg.Add(c) != nil {
		st.agg = nil
	}
}

// cacheEntry returns the entry a request that missed would cache, and the
// write result when it has none: the upstream's 200 body, or a stream's
// aggregated body when it ended cleanly with [DONE], if cache.Cacheable
// accepts it within max_entry_bytes, with the deployment that served it
// and the parameters it dropped. Result "" means the request was not
// served, so there is nothing to count. An answer routed with a budget's
// soft profile is not cacheable: its key names the profile before the
// downgrade (see cacheTarget), which it does not answer for.
func (g *snapshot) cacheEntry(rec *record) (*cache.Entry, string) {
	st := &rec.cache
	if !rec.served || rec.deployment == nil {
		return nil, ""
	}
	if rec.status != http.StatusOK || rec.errorCode != "" || (rec.route != nil && rec.route.downgraded != "") {
		return nil, resultNotCacheable
	}
	body := st.body
	if rec.stream {
		if st.aggOver {
			return nil, resultTooLarge
		}
		var ok bool
		if st.agg == nil {
			return nil, resultNotCacheable
		}
		if body, ok = st.agg.Body(); !ok {
			return nil, resultNotCacheable
		}
	}
	limit := g.cache.maxEntryBytes
	switch {
	case len(body) > limit:
		return nil, resultTooLarge
	case !cache.Cacheable(body, limit):
		return nil, resultNotCacheable
	}
	d := rec.deployment
	return &cache.Entry{Body: body, Deployment: d.ID, Model: d.Model.ID, DroppedParams: slices.Clone(rec.dropped), Created: g.now()}, ""
}

// writeCache stores a miss's response, asynchronously, in the layers it
// was looked up in (see cacheEntry): under its exact key with the exact
// TTL, and in its semantic partition with the lookup's embedding and the
// semantic TTL. A request with X-Switchyard-Cache: no-store is not
// written. It never delays the response: a write that finds the pool full
// is dropped and counted.
func (g *snapshot) writeCache(rec *record) {
	st := &rec.cache
	if !st.eligible || st.noStore || st.entry != nil {
		return
	}
	exact := st.pol.Exact.Enabled && st.key != ""
	sem := st.vec != nil && st.partition != "" && g.cache.sem != nil
	if !exact && !sem {
		return
	}
	e, result := g.cacheEntry(rec)
	if e == nil {
		if result != "" {
			if exact {
				g.metrics.cacheWrites.WithLabelValues(cache.LayerExact, result).Inc()
			}
			if sem {
				g.metrics.cacheWrites.WithLabelValues(cache.LayerSemantic, result).Inc()
			}
		}
		return
	}
	if exact {
		key, ttl := st.key, st.pol.Exact.TTL.Std()
		g.submitWrite(cache.LayerExact, func(ctx context.Context) error { return g.cache.store.Set(ctx, key, e, ttl) })
	}
	if sem {
		partition, vec, ttl := st.partition, st.vec, st.pol.Semantic.TTL.Std()
		g.submitWrite(cache.LayerSemantic, func(ctx context.Context) error { return g.cache.sem.Store(ctx, partition, vec, e, ttl) })
	}
}

// submitWrite runs write in the write pool, within cacheWriteTimeout, and
// counts its result; it counts dropped when the pool is full or closed.
func (g *snapshot) submitWrite(layer string, write func(ctx context.Context) error) {
	ok := g.cache.writes.submit(func() {
		ctx, cancel := context.WithTimeout(context.Background(), CacheWriteTimeout)
		defer cancel()
		result := resultOK
		if err := write(ctx); err != nil {
			result = resultError
			g.cacheFailed(ctx, layer, "cache write failed", err)
		}
		g.metrics.cacheWrites.WithLabelValues(layer, result).Inc()
	})
	if !ok {
		g.metrics.cacheWrites.WithLabelValues(layer, resultDropped).Inc()
	}
}

// writePool runs the asynchronous cache writes, at most cap(slots) at a
// time.
type writePool struct {
	slots chan struct{}
	mu    sync.Mutex
	// closed is set by close, after which nothing is submitted.
	closed bool
	wg     sync.WaitGroup
}

// submit runs fn on a goroutine of its own when a slot is free and the
// pool is open, and reports whether it did. It never blocks.
func (p *writePool) submit(fn func()) bool {
	select {
	case p.slots <- struct{}{}:
	default:
		return false
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.slots
		return false
	}
	p.wg.Add(1)
	p.mu.Unlock()
	go func() {
		defer func() {
			<-p.slots
			p.wg.Done()
		}()
		fn()
	}()
	return true
}

// close stops taking writes and waits for those in flight until ctx is
// done.
func (p *writePool) close(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.New("gateway: cache writes still in flight at shutdown were abandoned")
	}
}

// cachedUsage returns the usage of a cached chat.completion body, or nil.
func cachedUsage(body []byte) *schema.Usage {
	var r struct {
		Usage *schema.Usage `json:"usage"`
	}
	if json.Unmarshal(body, &r) != nil {
		return nil
	}
	return r.Usage
}

// purgeRequest is the body of POST /admin/v1/cache/purge.
type purgeRequest struct {
	Tenant string `json:"tenant"`
}

// purgeResponse is its answer.
type purgeResponse struct {
	Tenant           string `json:"tenant"`
	NamespaceVersion int64  `json:"namespace_version"`
}

// purge serves POST /admin/v1/cache/purge (see CachePurgeHandler).
func (g *snapshot) purge(w http.ResponseWriter, r *http.Request) {
	var req purgeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPurgeBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		apierror.WriteParam(w, http.StatusBadRequest, apierror.TypeInvalidRequest, "invalid_request",
			`The body must be a JSON object {"tenant": "<tenant id>"}.`, "")
		server.Audit(r, auditPurge, server.AuditRejected, slog.String("reason", "invalid_body"))
		return
	}
	if _, ok := g.tenants[req.Tenant]; !ok || req.Tenant == "" {
		apierror.WriteParam(w, http.StatusBadRequest, apierror.TypeInvalidRequest, "unknown_tenant",
			"The tenant is missing or is not configured.", "tenant")
		// An unknown tenant id is client input; it is not logged.
		server.Audit(r, auditPurge, server.AuditRejected, slog.String("reason", "unknown_tenant"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), purgeTimeout)
	defer cancel()
	v, err := g.cache.ns.Bump(ctx, req.Tenant)
	if err != nil {
		reqctx.Logger(r.Context()).WarnContext(r.Context(), "cache purge failed: the namespace store is unavailable", "error", err)
		apierror.WriteParam(w, http.StatusServiceUnavailable, apierror.TypeServer, "cache_unavailable",
			"The cache namespace store is unavailable; the tenant was not purged. Please retry.", "")
		server.Audit(r, auditPurge, server.AuditError, slog.String("tenant", req.Tenant), slog.String("reason", "namespace_store_unavailable"))
		return
	}
	server.Audit(r, auditPurge, server.AuditOK, slog.String("tenant", req.Tenant), slog.Int64("namespace_version", v))
	data, _ := json.Marshal(purgeResponse{Tenant: req.Tenant, NamespaceVersion: v})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
