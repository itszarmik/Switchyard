package usage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Limits on the reports of a PostgresLedger, so that no tenant can tie up
// the database with them.
const (
	// reportPoolConns caps the connections of the reports' own pool.
	reportPoolConns = 2
	// maxReportsPerTenant and maxReports cap the reports running at once
	// for one tenant (or for the admin report across every tenant) and in
	// all; a report beyond them fails with ErrReportBusy at once.
	maxReportsPerTenant = 1
	maxReports          = 4
	// reportStatementTimeout bounds each statement of a report in the
	// database, which then cancels it even if the client never does.
	reportStatementTimeout = "25s"
)

// reportLimiter counts the reports running, per tenant and in all.
type reportLimiter struct {
	mu        sync.Mutex
	total     int
	byTenant  map[string]int
	perTenant int
	max       int
}

func newReportLimiter(perTenant, maxTotal int) *reportLimiter {
	return &reportLimiter{byTenant: map[string]int{}, perTenant: perTenant, max: maxTotal}
}

// acquire takes a slot for a report of tenant ("" for every tenant), and
// reports false, taking nothing, when either limit is reached.
func (l *reportLimiter) acquire(tenant string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.max || l.byTenant[tenant] >= l.perTenant {
		return false
	}
	l.total++
	l.byTenant[tenant]++
	return true
}

// release returns a slot acquire took.
func (l *reportLimiter) release(tenant string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if l.byTenant[tenant]--; l.byTenant[tenant] <= 0 {
		delete(l.byTenant, tenant)
	}
}

// aggregateSQL selects the sums an accumulator needs, in scanAccumulator's
// order. Money sums are exact NUMERIC values, read as text.
var aggregateSQL = strings.Join([]string{
	"count(*)",
	"count(*) FILTER (WHERE status >= 400)",
	"count(*) FILTER (WHERE NOT cost_known)",
	"count(*) FILTER (WHERE cache_status = '" + CacheStatusHit + "')",
	"count(*) FILTER (WHERE fallback_reason <> '')",
	"COALESCE(sum(input_tokens), 0)::bigint",
	"COALESCE(sum(cached_input_tokens), 0)::bigint",
	"COALESCE(sum(cache_write_tokens), 0)::bigint",
	"COALESCE(sum(output_tokens), 0)::bigint",
	"COALESCE(sum(cost_usd) FILTER (WHERE cost_known), 0)::text",
	"COALESCE(sum(baseline_cost_usd) FILTER (WHERE cost_known), 0)::text",
	"COALESCE(sum(overhead_cost_usd), 0)::text",
	"COALESCE(sum(baseline_cost_usd - cost_usd) FILTER (WHERE cost_known AND cache_status <> '" + CacheStatusHit + "'), 0)::text",
	"COALESCE(sum(baseline_cost_usd) FILTER (WHERE cost_known AND cache_status = '" + CacheStatusHit + "'), 0)::text",
	// The sort key: known cost plus overhead, as Totals.CostUSD.
	"COALESCE(sum(cost_usd) FILTER (WHERE cost_known), 0) + COALESCE(sum(overhead_cost_usd), 0) AS sort_cost",
}, ",\n\t")

// aggregateColumns is the number of columns aggregateSQL selects.
const aggregateColumns = 15

// dimensionSQL returns the SQL expression for the validated dimension g. A
// tag dimension's key is bound as argument number arg.
func dimensionSQL(g string, arg int) string {
	switch g {
	case GroupByDay:
		return "to_char(ts AT TIME ZONE 'UTC', 'YYYY-MM-DD')"
	case GroupByModel:
		return "requested_model"
	case GroupByDeployment:
		return "deployment"
	case GroupByProvider:
		return "provider"
	case GroupByKey:
		return "key_id"
	case GroupByEndpoint:
		return "endpoint"
	case GroupByStatusClass:
		return "CASE WHEN status BETWEEN 100 AND 599 THEN (status / 100)::text || 'xx' ELSE 'other' END"
	case GroupByCacheStatus:
		return "cache_status"
	}
	return "COALESCE(tags ->> $" + strconv.Itoa(arg) + ", '')"
}

// Report implements Reporter. The totals and the groups are read in one
// read-only, repeatable-read transaction, so they agree. When q.Tenant is
// set, both queries filter on it.
//
// Reports run on a pool of their own, at most maxReportsPerTenant at once
// per tenant and maxReports in all; beyond that Report fails with
// ErrReportBusy. Each statement is limited to reportStatementTimeout in the
// database. A query the database finds too complex fails with
// ErrInvalidQuery.
func (l *PostgresLedger) Report(ctx context.Context, q Query) (*Report, error) {
	q, err := normalizeQuery(q)
	if err != nil {
		return nil, err
	}
	if !l.limiter.acquire(q.Tenant) {
		return nil, ErrReportBusy
	}
	defer l.limiter.release(q.Tenant)
	where := "ts >= $1 AND ts < $2"
	args := []any{q.From, q.To}
	if q.Tenant != "" {
		args = append(args, q.Tenant)
		where += " AND tenant = $" + strconv.Itoa(len(args))
	}

	r := &Report{From: q.From, To: q.To, GroupBy: q.GroupBy}
	err = pgx.BeginTxFunc(ctx, l.reports, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+reportStatementTimeout+"'"); err != nil {
			return err
		}
		var total accumulator
		row := tx.QueryRow(ctx, "SELECT "+aggregateSQL+"\nFROM usage_events WHERE "+where, args...)
		if err := scanAccumulator(row, nil, &total); err != nil {
			return err
		}
		r.Totals = total.totals()
		if len(q.GroupBy) == 0 {
			return nil
		}
		groups, err := queryGroups(ctx, tx, q, where, args)
		if err != nil {
			return err
		}
		r.Groups = groups
		return nil
	})
	if err != nil {
		return nil, reportErr(err)
	}
	return r, nil
}

// reportErr wraps an error of Report. A program limit the database hit
// (SQLSTATE class 54: too many columns, a statement too complex) is the
// query's fault, so it wraps ErrInvalidQuery; anything else is a database
// error.
func reportErr(err error) error {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && strings.HasPrefix(pgErr.Code, "54") {
		return fmt.Errorf("%w: the report is too complex for the database; group by fewer dimensions", ErrInvalidQuery)
	}
	return dbErr("report", err)
}

// queryGroups runs the grouped query, sorted and limited as Report
// requires. Group values are compared with the "C" collation, bytewise,
// as MemoryLedger does.
func queryGroups(ctx context.Context, tx pgx.Tx, q Query, where string, args []any) ([]Group, error) {
	args = append([]any(nil), args...)
	dims := make([]string, len(q.GroupBy))
	order := []string{"sort_cost DESC"}
	for i, g := range q.GroupBy {
		if tag, ok := strings.CutPrefix(g, GroupByTagPrefix); ok {
			args = append(args, tag)
		}
		dims[i] = fmt.Sprintf("%s AS g%d", dimensionSQL(g, len(args)), i)
		order = append(order, fmt.Sprintf(`g%d COLLATE "C"`, i))
	}
	groupBy := make([]string, len(dims))
	for i := range dims {
		groupBy[i] = strconv.Itoa(i + 1)
	}
	args = append(args, q.Limit)
	sql := "SELECT * FROM (SELECT " + strings.Join(dims, ", ") + ",\n\t" + aggregateSQL +
		"\nFROM usage_events WHERE " + where +
		"\nGROUP BY " + strings.Join(groupBy, ", ") + ") AS grouped" +
		"\nORDER BY " + strings.Join(order, ", ") +
		"\nLIMIT $" + strconv.Itoa(len(args))
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []Group
	for rows.Next() {
		values := make([]string, len(q.GroupBy))
		var a accumulator
		if err := scanAccumulator(rows, values, &a); err != nil {
			return nil, err
		}
		key := make(map[string]string, len(q.GroupBy))
		for i, g := range q.GroupBy {
			key[g] = values[i]
		}
		groups = append(groups, Group{Key: key, Totals: a.totals()})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if groups == nil {
		groups = []Group{}
	}
	return groups, nil
}

// scanAccumulator scans a row of len(values) group values followed by the
// aggregateSQL columns into values and a.
func scanAccumulator(row pgx.Row, values []string, a *accumulator) error {
	var cost, baseline, overhead, routing, cache string
	var sortCost any
	dest := make([]any, 0, len(values)+aggregateColumns)
	for i := range values {
		dest = append(dest, &values[i])
	}
	dest = append(dest,
		&a.requests, &a.errors, &a.unpriced, &a.cacheHits, &a.fallbacks,
		&a.tokens.Input, &a.tokens.CachedInput, &a.tokens.CacheWrite, &a.tokens.Output,
		&cost, &baseline, &overhead, &routing, &cache, &sortCost)
	if err := row.Scan(dest...); err != nil {
		return err
	}
	for _, f := range []struct {
		text string
		dst  *Money
	}{
		{cost, &a.cost}, {baseline, &a.baseline}, {overhead, &a.overhead}, {routing, &a.routing}, {cache, &a.cache},
	} {
		m, err := ParseMoney(f.text)
		if err != nil {
			return fmt.Errorf("usage: report: decode a NUMERIC sum: %w", err)
		}
		*f.dst = m
	}
	return nil
}
