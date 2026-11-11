package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/config"
)

// PostgresOptions configures a PostgresLedger. Zero values take the
// defaults.
type PostgresOptions struct {
	// BatchSize is the most events written in one batch (DefaultBatchSize).
	BatchSize int
	// FlushInterval is the longest an event waits before its batch is
	// written (DefaultFlushInterval).
	FlushInterval time.Duration
	// QueueSize bounds the events waiting to be written (DefaultQueueSize).
	QueueSize int
	// RetryMin and RetryMax bound the exponential backoff between attempts
	// to write a batch while the database fails (DefaultRetryMin,
	// DefaultRetryMax).
	RetryMin, RetryMax time.Duration
	// Registerer receives the switchyard_usage_* metrics. Nil leaves them
	// unregistered. Close unregisters them.
	Registerer prometheus.Registerer
	// Logger receives the writer's warnings. Nil discards them. Log lines
	// carry event IDs and counts, never event contents.
	Logger *slog.Logger
	// Now is the clock used to create the current and next month's
	// partitions at startup. Nil means time.Now.
	Now func() time.Time
}

// PostgresOptionsFrom maps the storage.ledger config to PostgresOptions;
// zero config values take the defaults. The caller adds Registerer and
// Logger.
func PostgresOptionsFrom(c config.Ledger) PostgresOptions {
	return PostgresOptions{
		BatchSize:     c.BatchSize,
		FlushInterval: c.FlushInterval.Std(),
		QueueSize:     c.QueueSize,
	}
}

// PostgresLedger is a Sink and Reporter backed by Postgres. Record queues
// events for an asynchronous batching writer, which survives database
// outages by buffering (DESIGN §12.2, §14.7); Report aggregates with exact
// NUMERIC sums. It is safe for concurrent use.
type PostgresLedger struct {
	pool *pgxpool.Pool
	// reports is a small pool of its own for Report, so that reports can
	// never take the connections the writer needs.
	reports *pgxpool.Pool
	limiter *reportLimiter
	w       *writer
	m       *writerMetrics

	mu         sync.Mutex
	partitions map[time.Time]bool // month starts known to have a partition
}

var (
	_ Sink     = (*PostgresLedger)(nil)
	_ Reporter = (*PostgresLedger)(nil)
)

// NewPostgresLedger connects to the database at dsn, applies the embedded
// migrations, creates the partitions for the current and the next month,
// and starts the writer. Tables are created in the first schema of the
// connection's search_path. Errors never include the DSN.
//
// Reports use a second pool of at most reportPoolConns connections, so
// that they cannot starve the writer.
func NewPostgresLedger(ctx context.Context, dsn string, opts PostgresOptions) (*PostgresLedger, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// pgx redacts passwords only on a best-effort basis, so its message
		// is not passed on.
		return nil, errors.New("usage: the postgres DSN is not a valid connection string")
	}
	rcfg := cfg.Copy()
	rcfg.MaxConns, rcfg.MinConns = reportPoolConns, 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("usage: connect to postgres: %w", err)
	}
	reports, err := pgxpool.NewWithConfig(ctx, rcfg)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("usage: connect to postgres: %w", err)
	}
	l, err := newPostgresLedger(ctx, pool, reports, opts)
	if err != nil {
		pool.Close()
		reports.Close()
		return nil, err
	}
	return l, nil
}

// newPostgresLedger is NewPostgresLedger on existing pools: pool for the
// migrations and the writer, reports for Report.
func newPostgresLedger(ctx context.Context, pool, reports *pgxpool.Pool, opts PostgresOptions) (*PostgresLedger, error) {
	ms, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	if _, err := migrate(ctx, pool, ms); err != nil {
		return nil, err
	}
	l := &PostgresLedger{
		pool: pool, reports: reports, partitions: map[time.Time]bool{},
		limiter: newReportLimiter(maxReportsPerTenant, maxReports),
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	this := monthStart(now())
	if err := l.ensurePartitions(ctx, []time.Time{this, this.AddDate(0, 1, 0)}); err != nil {
		return nil, err
	}
	m, err := newWriterMetrics(opts.Registerer)
	if err != nil {
		return nil, err
	}
	l.m = m
	l.w = newWriter(l, writerConfig{
		batchSize:     opts.BatchSize,
		flushInterval: opts.FlushInterval,
		queueSize:     opts.QueueSize,
		retryMin:      opts.RetryMin,
		retryMax:      opts.RetryMax,
		logger:        opts.Logger,
		metrics:       m,
	})
	return l, nil
}

// Record implements Sink: it queues ev for the writer and never blocks.
func (l *PostgresLedger) Record(ev *Event) { l.w.Record(ev) }

// Close implements Sink: it writes the queued events, retrying while the
// database fails, until ctx is done, then closes the connection pools and
// unregisters the metrics.
func (l *PostgresLedger) Close(ctx context.Context) error {
	err := l.w.Close(ctx)
	l.pool.Close()
	l.reports.Close()
	l.m.unregister()
	return err
}

// monthStart returns the first instant of t's month in UTC.
func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// partitionName returns the name of the partition holding month.
func partitionName(month time.Time) string {
	return "usage_events_p" + month.Format("200601")
}

// ensurePartitions creates the monthly partitions for months that are not
// known to exist yet. Creation is idempotent and serialized across
// replicas by an advisory lock.
func (l *PostgresLedger) ensurePartitions(ctx context.Context, months []time.Time) error {
	for _, month := range months {
		l.mu.Lock()
		known := l.partitions[month]
		l.mu.Unlock()
		if known {
			continue
		}
		// The name and bounds are formatted from a time, so they are safe
		// to splice into the DDL, which cannot take parameters.
		ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF usage_events FOR VALUES FROM ('%s') TO ('%s')",
			pgx.Identifier{partitionName(month)}.Sanitize(),
			month.Format(time.RFC3339), month.AddDate(0, 1, 0).Format(time.RFC3339))
		err := pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", partitionLockKey); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, ddl)
			return err
		})
		if err != nil {
			return dbErr("create partition "+partitionName(month), err)
		}
		l.mu.Lock()
		l.partitions[month] = true
		l.mu.Unlock()
	}
	return nil
}

// forgetPartitions empties the partition cache, so that the next write
// checks again, for example after an operator dropped an old partition.
func (l *PostgresLedger) forgetPartitions() {
	l.mu.Lock()
	clear(l.partitions)
	l.mu.Unlock()
}

// ledgerColumns are the usage_events columns in the order eventRow
// produces them.
var ledgerColumns = []string{
	"id", "ts", "tenant", "key_id", "endpoint", "requested_model", "alias", "deployment", "provider",
	"stream", "status", "attempts", "fallback_reason", "route_reason", "cache_status",
	"input_tokens", "cached_input_tokens", "cache_write_tokens", "output_tokens",
	"usage_known", "usage_estimated", "cost_usd", "cost_known", "pricing",
	"baseline_model", "baseline_cost_usd", "overhead_cost_usd", "latency_ms", "ttft_ms",
	"tags", "config_version",
	"profile", "routing_mode", "shadow_deployment", "shadow_cost_usd", "shadow_cost_known",
}

// WriteBatch implements batchStore. It copies the batch into a temporary
// table and inserts it with ON CONFLICT DO NOTHING, in one transaction, so
// a batch retried after an ambiguous failure is never counted twice.
func (l *PostgresLedger) WriteBatch(ctx context.Context, events []*Event) error {
	var months []time.Time
	for _, ev := range events {
		if m := monthStart(ev.Time); !containsTime(months, m) {
			months = append(months, m)
		}
	}
	if err := l.ensurePartitions(ctx, months); err != nil {
		return err
	}
	err := pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "CREATE TEMPORARY TABLE usage_events_stage (LIKE usage_events) ON COMMIT DROP"); err != nil {
			return err
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"usage_events_stage"}, ledgerColumns,
			pgx.CopyFromSlice(len(events), func(i int) ([]any, error) { return eventRow(events[i]), nil })); err != nil {
			return err
		}
		cols := strings.Join(ledgerColumns, ", ")
		_, err := tx.Exec(ctx, "INSERT INTO usage_events ("+cols+") SELECT "+cols+" FROM usage_events_stage ON CONFLICT DO NOTHING")
		return err
	})
	if err != nil {
		l.forgetPartitions()
		return dbErr("write events", err)
	}
	return nil
}

func containsTime(ts []time.Time, t time.Time) bool {
	for _, u := range ts {
		if u.Equal(t) {
			return true
		}
	}
	return false
}

// pricingSnapshot is the JSON form of the prices in usage_events.pricing.
type pricingSnapshot struct {
	Input       string `json:"input,omitempty"`
	Output      string `json:"output,omitempty"`
	CachedInput string `json:"cached_input,omitempty"`
	CacheWrite  string `json:"cache_write,omitempty"`
}

// eventRow converts ev to a row in ledgerColumns order. Text is made valid
// UTF-8 without NUL characters and integers are clamped to their column
// types, so that no event can make a batch fail forever.
func eventRow(ev *Event) []any {
	tags := make(map[string]string, len(ev.Tags))
	for k, v := range ev.Tags {
		tags[cleanText(k)] = cleanText(v)
	}
	return []any{
		cleanText(ev.ID), ev.Time.UTC(), cleanText(ev.Tenant), cleanText(ev.KeyID), cleanText(ev.Endpoint),
		cleanText(ev.RequestedModel), cleanText(ev.Alias), cleanText(ev.Deployment), cleanText(ev.Provider),
		ev.Stream, clampInt32(ev.Status), clampInt32(ev.Attempts),
		cleanText(ev.FallbackReason), cleanText(ev.RouteReason), cleanText(ev.CacheStatus),
		ev.Tokens.Input, ev.Tokens.CachedInput, ev.Tokens.CacheWrite, ev.Tokens.Output,
		ev.UsageKnown, ev.UsageEstimated, numeric(ev.Cost), ev.CostKnown,
		pricingSnapshot{
			Input:       ev.Pricing.Input.String(),
			Output:      ev.Pricing.Output.String(),
			CachedInput: ev.Pricing.CachedInput.String(),
			CacheWrite:  ev.Pricing.CacheWrite.String(),
		},
		cleanText(ev.BaselineModel), numeric(ev.BaselineCost), numeric(ev.OverheadCost),
		ev.LatencyMS, ev.TTFTMS, tags, cleanText(ev.ConfigVersion),
		cleanText(ev.Profile), cleanText(ev.RoutingMode), cleanText(ev.ShadowDeployment),
		numeric(ev.ShadowCost), ev.ShadowCostKnown,
	}
}

// cleanText returns s as valid UTF-8 without NUL characters, which Postgres
// text and jsonb reject.
func cleanText(s string) string {
	if utf8.ValidString(s) && !strings.Contains(s, "\x00") {
		return s
	}
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
}

// clampInt32 clamps n to the range of a Postgres integer.
func clampInt32(n int) int32 {
	return int32(max(min(n, math.MaxInt32), math.MinInt32))
}

// numeric encodes m for a NUMERIC(38,15) column.
func numeric(m Money) pgtype.Numeric {
	return pgtype.Numeric{Int: new(big.Int).Set(m.int()), Exp: -femtoDigits, Valid: true}
}

// dbError is a database error whose message carries only the operation and
// the SQLSTATE code: a Postgres message can quote the values in a row, and
// event contents must not reach the logs.
type dbError struct {
	op, code string
	err      error
}

func (e *dbError) Error() string {
	return fmt.Sprintf("usage: %s: postgres error SQLSTATE %s", e.op, e.code)
}

func (e *dbError) Unwrap() error { return e.err }

// dbErr wraps err from operation op. A Postgres error becomes a dbError;
// a data exception (SQLSTATE class 22), which retrying cannot fix, is also
// marked permanent.
func dbErr(op string, err error) error {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		e := &dbError{op: op, code: pgErr.Code, err: err}
		if strings.HasPrefix(pgErr.Code, "22") {
			return &permanentError{err: e}
		}
		return e
	}
	return fmt.Errorf("usage: %s: %w", op, err)
}
