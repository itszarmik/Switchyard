// Package usageapi serves the usage reports over HTTP (DESIGN §12.5):
// GET /switchyard/v1/usage on the public listener, for the authenticated
// caller's own tenant, and GET /admin/v1/usage on the admin listener, for
// any tenant or all of them.
//
// Both take the same query parameters:
//
//   - from and to bound the report to [from, to), as RFC 3339 times or
//     YYYY-MM-DD dates in UTC. A date as to includes that whole day. The
//     default is the 7 days before now, and the span may not exceed 400
//     days.
//   - group_by is a comma-separated list of dimensions: day, model,
//     deployment, provider, key, endpoint, status_class, cache_status,
//     profile, routing_mode and tag:<name>.
//   - limit caps the number of groups (default 1000, at most 10000).
//
// group_by may list at most 8 dimensions, at most 3 of them tags; more is
// a 400 invalid_query. A store that limits concurrent reports answers 429
// report_busy beyond its limit.
//
// The admin endpoint also takes tenant, a tenant id; without it the report
// covers every tenant. Money is serialized as exact decimal strings.
// totals.projected_savings_usd is observe mode's projected routing
// savings, which never ran and are reported apart from savings_usd.
package usageapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"switchyard/internal/apierror"
	"switchyard/internal/auth"
	"switchyard/internal/config"
	"switchyard/internal/reqctx"
	"switchyard/internal/usage"
)

// Query limits.
const (
	// DefaultSpan is the report period when the query sets neither from
	// nor to, ending now.
	DefaultSpan = 7 * 24 * time.Hour
	// MaxSpan is the longest period a report may cover.
	MaxSpan = 400 * 24 * time.Hour
	// reportTimeout bounds the store's work on one report.
	reportTimeout = 30 * time.Second
)

// Error codes.
const (
	codeInvalidQuery = "invalid_query"
	codeUnavailable  = "usage_unavailable"
	codeBusy         = "report_busy"
	codeAuth         = "invalid_api_key"
	// typeRateLimit is OpenAI's error type for request-rate 429s.
	typeRateLimit = "requests"
)

// Options configures the handlers.
type Options struct {
	// Now is the clock for the default period; nil means time.Now.
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

// TenantHandler serves GET /switchyard/v1/usage: the report of the
// authenticated caller's tenant, from the principal that the auth
// middleware stored in the request context. A request without one gets
// 401, so the handler never reports without a tenant.
func TenantHandler(r usage.Reporter, opts Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p, ok := auth.FromContext(req.Context())
		if !ok || p.TenantID == "" {
			apierror.Write(w, http.StatusUnauthorized, apierror.TypeInvalidRequest, codeAuth, "Incorrect API key provided.")
			return
		}
		serve(w, req, r, opts, p.TenantID)
	})
}

// AdminHandler serves GET /admin/v1/usage: the report of the tenant named
// by the tenant parameter, or of every tenant without it. It belongs on
// the admin listener.
func AdminHandler(r usage.Reporter, opts Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		tenant := req.URL.Query().Get("tenant")
		if tenant != "" && !config.ValidID(tenant) {
			badRequest(w, "tenant", "tenant must be a tenant id: 1 to 63 characters from a-z, 0-9, '_' and '-'.")
			return
		}
		serve(w, req, r, opts, tenant)
	})
}

// serve parses the query, runs the report for tenant ("" for every tenant)
// and writes it.
func serve(w http.ResponseWriter, req *http.Request, r usage.Reporter, opts Options, tenant string) {
	q, param, msg := parseQuery(req, opts.now())
	if msg != "" {
		badRequest(w, param, msg)
		return
	}
	q.Tenant = tenant
	ctx, cancel := context.WithTimeout(req.Context(), reportTimeout)
	defer cancel()
	rep, err := r.Report(ctx, q)
	switch {
	case errors.Is(err, usage.ErrInvalidQuery):
		badRequest(w, "", queryMessage(err))
		return
	case errors.Is(err, usage.ErrReportBusy):
		w.Header().Set("Retry-After", "1")
		apierror.Write(w, http.StatusTooManyRequests, typeRateLimit, codeBusy,
			"Too many usage reports are running; only one at a time is allowed per tenant. Please retry later.")
		return
	case err != nil:
		rctx := req.Context()
		reqctx.Logger(rctx).ErrorContext(rctx, "usage report failed", "error", err)
		apierror.Write(w, http.StatusServiceUnavailable, apierror.TypeServer, codeUnavailable,
			"The usage report could not be computed. Please retry later.")
		return
	}
	data, err := encodeReport(rep, tenant)
	if err != nil {
		rctx := req.Context()
		reqctx.Logger(rctx).ErrorContext(rctx, "encode the usage report", "error", err)
		apierror.Write(w, http.StatusInternalServerError, apierror.TypeServer, "internal_error", "Could not encode the usage report.")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	// The only request value in the body is a validated tenant id, JSON
	// encoded and served as application/json with nosniff.
	_, _ = w.Write(data) //nolint:gosec // G705: not HTML; see above
}

// parseQuery reads from, to, group_by and limit. On a malformed parameter
// it returns the parameter's name and a message, which never echoes the
// value.
func parseQuery(req *http.Request, now time.Time) (q usage.Query, param, msg string) {
	v := req.URL.Query()
	var err error
	if s := v.Get("to"); s != "" {
		if q.To, err = parseTime(s, true); err != nil {
			return q, "to", "to must be an RFC 3339 time or a YYYY-MM-DD date."
		}
	} else {
		q.To = now
	}
	if s := v.Get("from"); s != "" {
		if q.From, err = parseTime(s, false); err != nil {
			return q, "from", "from must be an RFC 3339 time or a YYYY-MM-DD date."
		}
	} else {
		q.From = q.To.Add(-DefaultSpan)
	}
	switch {
	case !q.From.Before(q.To):
		return q, "from", "from must be before to."
	case q.To.Sub(q.From) > MaxSpan:
		return q, "from", "A report may cover at most 400 days."
	}
	if s := v.Get("group_by"); s != "" {
		for g := range strings.SplitSeq(s, ",") {
			g = strings.TrimSpace(g)
			if g == "" {
				return q, "group_by", "group_by must be a comma-separated list of dimensions without empty entries."
			}
			q.GroupBy = append(q.GroupBy, g)
		}
	}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return q, "limit", "limit must be a positive whole number."
		}
		q.Limit = n
	}
	return q, "", ""
}

// parseTime parses an RFC 3339 time or a YYYY-MM-DD date, which is
// midnight UTC; as the end of a period (isTo), a date means the midnight
// after it, so that the period includes the whole day.
func parseTime(s string, isTo bool) (time.Time, error) {
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		if isTo {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// queryMessage turns a usage.ErrInvalidQuery error, which never echoes
// client input, into a message for the client.
func queryMessage(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, usage.ErrInvalidQuery.Error()+": "); i >= 0 {
		msg = msg[i+len(usage.ErrInvalidQuery.Error())+2:]
	}
	return "Invalid query: " + msg + ". The group_by dimensions are day, model, deployment, provider, key, endpoint, status_class, cache_status, profile, routing_mode and tag:<name>, each at most once, at most 8 of them and at most 3 tags."
}

// badRequest writes a 400 invalid_query error about param.
func badRequest(w http.ResponseWriter, param, msg string) {
	apierror.WriteParam(w, http.StatusBadRequest, apierror.TypeInvalidRequest, codeInvalidQuery, msg, param)
}

// Wire shapes (DESIGN §12.5).
type (
	reportJSON struct {
		Object  string            `json:"object"`
		Period  periodJSON        `json:"period"`
		Tenant  *string           `json:"tenant"`
		GroupBy []string          `json:"group_by"`
		Totals  totalsJSON        `json:"totals"`
		Groups  []json.RawMessage `json:"groups"`
	}
	periodJSON struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	}
	totalsJSON struct {
		Requests         int64       `json:"requests"`
		Errors           int64       `json:"errors"`
		UnpricedRequests int64       `json:"unpriced_requests"`
		Tokens           tokensJSON  `json:"tokens"`
		CostUSD          usage.Money `json:"cost_usd"`
		BaselineCostUSD  usage.Money `json:"baseline_cost_usd"`
		OverheadUSD      usage.Money `json:"overhead_usd"`
		SavingsUSD       savingsJSON `json:"savings_usd"`
		// ProjectedSavingsUSD is observe mode's estimate, never realized.
		ProjectedSavingsUSD usage.Money `json:"projected_savings_usd"`
		CacheHitRate        float64     `json:"cache_hit_rate"`
		FallbackRate        float64     `json:"fallback_rate"`
	}
	tokensJSON struct {
		Input       int64 `json:"input"`
		CachedInput int64 `json:"cached_input"`
		CacheWrite  int64 `json:"cache_write"`
		Output      int64 `json:"output"`
	}
	savingsJSON struct {
		Routing usage.Money `json:"routing"`
		Cache   usage.Money `json:"cache"`
		Net     usage.Money `json:"net"`
	}
)

// totals converts t to its wire shape.
func totals(t usage.Totals) totalsJSON {
	return totalsJSON{
		Requests: t.Requests, Errors: t.Errors, UnpricedRequests: t.UnpricedRequests,
		Tokens: tokensJSON{
			Input: t.Tokens.Input, CachedInput: t.Tokens.CachedInput,
			CacheWrite: t.Tokens.CacheWrite, Output: t.Tokens.Output,
		},
		CostUSD: t.CostUSD, BaselineCostUSD: t.BaselineCostUSD, OverheadUSD: t.OverheadUSD,
		SavingsUSD:          savingsJSON{Routing: t.SavingsUSD.Routing, Cache: t.SavingsUSD.Cache, Net: t.SavingsUSD.Net},
		ProjectedSavingsUSD: t.ProjectedSavingsUSD,
		CacheHitRate:        t.CacheHitRate, FallbackRate: t.FallbackRate,
	}
}

// encodeReport encodes rep. Each group is one flat object: its dimension
// values, in group_by order, followed by its totals. tenant is null for a
// report across every tenant.
func encodeReport(rep *usage.Report, tenant string) ([]byte, error) {
	out := reportJSON{
		Object:  "usage_report",
		Period:  periodJSON{From: rep.From, To: rep.To},
		GroupBy: rep.GroupBy,
		Totals:  totals(rep.Totals),
		Groups:  make([]json.RawMessage, 0, len(rep.Groups)),
	}
	if out.GroupBy == nil {
		out.GroupBy = []string{}
	}
	if tenant != "" {
		out.Tenant = &tenant
	}
	for _, g := range rep.Groups {
		obj, err := groupJSON(rep.GroupBy, g)
		if err != nil {
			return nil, err
		}
		out.Groups = append(out.Groups, obj)
	}
	return json.Marshal(out)
}

// groupJSON encodes one group as a flat object: the dimension values, then
// the totals' fields. Dimension names never clash with totals fields.
func groupJSON(groupBy []string, g usage.Group) (json.RawMessage, error) {
	t, err := json.Marshal(totals(g.Totals))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for _, dim := range groupBy {
		k, _ := json.Marshal(dim)
		v, _ := json.Marshal(g.Key[dim]) // strings; cannot fail
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
		b.WriteByte(',')
	}
	b.Write(t[1:]) // the totals without their opening brace
	return b.Bytes(), nil
}
