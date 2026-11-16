//go:build integration

package usage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// These tests need a real Postgres, at SWITCHYARD_TEST_POSTGRES_DSN in URL
// form (make test-integration starts one). Each test works in a schema of
// its own, so they can run in parallel with each other and with other
// packages' integration tests.

const dsnEnv = "SWITCHYARD_TEST_POSTGRES_DSN"

// testDB is a fresh schema in the test database.
type testDB struct {
	dsn    string        // the base DSN with search_path set to the schema
	admin  *pgxpool.Pool // on the schema, for assertions
	schema string
}

func newTestDB(t *testing.T) *testDB {
	t.Helper()
	base := os.Getenv(dsnEnv)
	if base == "" {
		t.Skip(dsnEnv + " is not set; run make test-integration")
	}
	ctx := context.Background()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	schema := "sy_usage_it_" + hex.EncodeToString(b[:])

	root, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(root.Close)
	if _, err := root.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := root.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s must be a URL", dsnEnv)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db := &testDB{dsn: u.String(), schema: schema}
	db.admin, err = pgxpool.New(ctx, db.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.admin.Close)
	return db
}

// withHost returns db's DSN pointed at addr instead.
func (db *testDB) withHost(t *testing.T, addr string) string {
	t.Helper()
	u, err := url.Parse(db.dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = addr
	return u.String()
}

func (db *testDB) count(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.admin.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (db *testDB) tableExists(t *testing.T, name string) bool {
	t.Helper()
	return db.count(t, "SELECT count(*) FROM pg_tables WHERE schemaname = $1 AND tablename = $2", db.schema, name) == 1
}

func openLedger(t *testing.T, dsn string, opts PostgresOptions) *PostgresLedger {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	l, err := NewPostgresLedger(ctx, dsn, opts)
	if err != nil {
		t.Fatalf("NewPostgresLedger: %v", err)
	}
	return l
}

func closeLedger(t *testing.T, l *PostgresLedger) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := l.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// eventually polls cond every 10ms until it holds, failing after timeout.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func metric(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			m := f.GetMetric()[0]
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s not registered", name)
	return 0
}

func TestPostgresMigrationsAreIdempotent(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	closeLedger(t, openLedger(t, db.dsn, PostgresOptions{}))
	// A second start, and a direct second run, apply nothing.
	closeLedger(t, openLedger(t, db.dsn, PostgresOptions{}))
	ms, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	applied, err := migrate(context.Background(), db.admin, ms)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Errorf("second migrate applied %v", applied)
	}
	if n := db.count(t, "SELECT count(*) FROM schema_migrations"); n != int64(len(ms)) {
		t.Errorf("schema_migrations has %d rows, want %d", n, len(ms))
	}
	if !db.tableExists(t, "usage_events") {
		t.Errorf("usage_events was not created in the test schema")
	}
	if n := db.count(t, "SELECT count(*) FROM pg_indexes WHERE schemaname = $1 AND indexname = 'usage_events_tenant_ts'", db.schema); n != 1 {
		t.Errorf("index usage_events_tenant_ts missing")
	}
}

func TestPostgresMigrationsConcurrentStart(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Go(func() {
			l, err := NewPostgresLedger(context.Background(), db.dsn, PostgresOptions{})
			if err != nil {
				errs <- err
				return
			}
			errs <- l.Close(context.Background())
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent start: %v", err)
		}
	}
	ms, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if n := db.count(t, "SELECT count(*) FROM schema_migrations"); n != int64(len(ms)) {
		t.Errorf("schema_migrations has %d rows, want %d", n, len(ms))
	}
}

// TestPostgresMigration0002KeepsOldRows: rows written before the routing
// columns existed stay readable after migration 0002, which is safe to run
// again, and reports count them with no profile, routing mode or
// projected savings.
func TestPostgresMigration0002KeepsOldRows(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := context.Background()
	ms, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) < 2 || ms[1].version != 2 {
		t.Fatalf("migrations = %v, want 0001 and 0002", ms)
	}
	if _, err := migrate(ctx, db.admin, ms[:1]); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, sql := range []string{
		"CREATE TABLE usage_events_p202611 PARTITION OF usage_events FOR VALUES FROM ('2026-11-01T00:00:00Z') TO ('2026-12-01T00:00:00Z')",
		`INSERT INTO usage_events (id, ts, tenant, key_id, endpoint, requested_model, alias, deployment, provider, stream, status,
			attempts, fallback_reason, route_reason, cache_status, input_tokens, cached_input_tokens, cache_write_tokens,
			output_tokens, usage_known, usage_estimated, cost_usd, cost_known, pricing, baseline_model, baseline_cost_usd,
			overhead_cost_usd, latency_ms, ttft_ms, tags, config_version)
		VALUES ('old', '2026-11-05T12:00:00Z', 'acme', 'k1', '/v1/chat/completions', 'gpt', '', 'gpt@openai', 'openai', false, 200,
			1, '', 'cheapest_eligible', 'miss', 10, 0, 0, 5, true, false, 0.25, true, '{}', 'gpt', 0.5, 0, 1, 1, '{}', 'v1')`,
	} {
		if _, err := db.admin.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	applied, err := migrate(ctx, db.admin, ms)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != len(ms)-1 || applied[0] != 2 {
		t.Errorf("applied %v, want every migration after 0001", applied)
	}
	// The migration itself is idempotent, even outside schema_migrations.
	if _, err := db.admin.Exec(ctx, ms[1].sql); err != nil {
		t.Errorf("re-running 0002: %v", err)
	}

	l := openLedger(t, db.dsn, PostgresOptions{})
	defer closeLedger(t, l)
	r, err := l.Report(ctx, Query{Tenant: "acme", From: ts.Add(-time.Hour), To: ts.Add(time.Hour), GroupBy: []string{GroupByProfile, GroupByRoutingMode}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Totals.Requests != 1 || r.Totals.CostUSD.String() != "0.25" || r.Totals.SavingsUSD.Routing.String() != "0.25" ||
		!r.Totals.ProjectedSavingsUSD.IsZero() {
		t.Errorf("totals of the old row = %+v", r.Totals)
	}
	if len(r.Groups) != 1 || r.Groups[0].Key[GroupByProfile] != "" || r.Groups[0].Key[GroupByRoutingMode] != "" {
		t.Errorf("groups = %+v, want one with an empty profile and routing mode", r.Groups)
	}
}

func TestPostgresPartitions(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	now := time.Date(2031, 12, 15, 12, 0, 0, 0, time.UTC)
	l := openLedger(t, db.dsn, PostgresOptions{Now: func() time.Time { return now }, FlushInterval: 10 * time.Millisecond})
	// The current and the next month exist from the start, across a year.
	for _, p := range []string{"usage_events_p203112", "usage_events_p203201"} {
		if !db.tableExists(t, p) {
			t.Errorf("partition %s was not created at startup", p)
		}
	}
	if db.tableExists(t, "usage_events_p203207") {
		t.Fatal("partition for July 2032 exists before any event")
	}
	l.Record(&Event{ID: "july", Time: time.Date(2032, 7, 31, 23, 59, 59, 0, time.UTC), Tenant: "acme"})
	l.Record(&Event{ID: "march", Time: time.Date(2033, 3, 1, 0, 0, 0, 0, time.FixedZone("x", 3600)), Tenant: "acme"})
	closeLedger(t, l)
	// 2033-03-01T00:00+01:00 is still February in UTC.
	for _, p := range []string{"usage_events_p203207", "usage_events_p203302"} {
		if !db.tableExists(t, p) {
			t.Errorf("partition %s was not created on write", p)
		}
	}
	if n := db.count(t, "SELECT count(*) FROM usage_events_p203207"); n != 1 {
		t.Errorf("July partition holds %d rows, want 1", n)
	}
	// Restarting with the partitions in place is fine.
	closeLedger(t, openLedger(t, db.dsn, PostgresOptions{Now: func() time.Time { return now }}))
}

func TestPostgresHandComputed(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	l := openLedger(t, db.dsn, PostgresOptions{})
	for _, ev := range handEvents(t) {
		l.Record(ev)
	}
	closeLedger(t, l)

	pg := openLedger(t, db.dsn, PostgresOptions{})
	defer closeLedger(t, pg)
	checkHandComputed(t, pg)

	var cost, pricing, tags string
	var in, out int64
	err := db.admin.QueryRow(context.Background(),
		"SELECT cost_usd::text, pricing::text, tags::text, input_tokens, output_tokens FROM usage_events WHERE id = 'e1'").
		Scan(&cost, &pricing, &tags, &in, &out)
	if err != nil {
		t.Fatal(err)
	}
	if cost != "0.000525300000000" || pricing != `{"input": "0.15", "output": "0.6"}` || tags != `{"team": "search"}` || in != 1234 || out != 567 {
		t.Errorf("e1 row: cost %s pricing %s tags %s tokens %d/%d", cost, pricing, tags, in, out)
	}
	if got := db.count(t, "SELECT count(*) FROM usage_events WHERE tags = '{}'::jsonb"); got != 5 {
		t.Errorf("%d events stored with empty tags, want 5", got)
	}

	// A batch retried after an ambiguous failure is not counted twice.
	if err := pg.WriteBatch(context.Background(), handEvents(t)); err != nil {
		t.Fatal(err)
	}
	if n := db.count(t, "SELECT count(*) FROM usage_events"); n != int64(len(handEvents(t))) {
		t.Errorf("usage_events has %d rows after a duplicate batch, want %d", n, len(handEvents(t)))
	}
}

func TestPostgresMatchesMemoryLedger(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	events := randomEvents(t, 11, 3000)
	shadows, feedback := addQualityRows(events, 11)
	// Text Postgres cannot store must not poison a batch; it is stored
	// cleaned.
	odd := &Event{
		ID: "odd\x00id", Time: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), Tenant: "acme", RequestedModel: "bad\xffutf8\x00",
		Status: 200, Tags: map[string]string{"team": "v\x00"}, CacheStatus: CacheStatusMiss,
	}
	l := openLedger(t, db.dsn, PostgresOptions{BatchSize: 250})
	for _, ev := range events {
		l.Record(ev)
	}
	l.Record(odd)
	closeLedger(t, l)
	if n := db.count(t, "SELECT count(*) FROM usage_events"); n != int64(len(events)+1) {
		t.Fatalf("usage_events has %d rows, want %d", n, len(events)+1)
	}
	// The random events span three months, so three partitions hold rows.
	if n := db.count(t, "SELECT count(DISTINCT tableoid) FROM usage_events"); n != 3 {
		t.Errorf("rows are spread over %d partitions, want 3", n)
	}

	all := append(append([]*Event(nil), events...), &Event{
		ID: "oddid", Time: odd.Time, Tenant: odd.Tenant, RequestedModel: cleanText(odd.RequestedModel),
		Status: odd.Status, Tags: map[string]string{"team": "v"}, CacheStatus: odd.CacheStatus,
	})
	mem := newMemoryWith(all)
	pg := openLedger(t, db.dsn, PostgresOptions{})
	defer closeLedger(t, pg)
	ctx := context.Background()
	for _, s := range shadows {
		for _, l := range []ShadowStore{mem, pg} {
			if err := l.RecordShadowEval(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, f := range feedback {
		for _, l := range []FeedbackStore{mem, pg} {
			if err := l.RecordFeedback(ctx, f); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Feedback on another tenant's request: the memory ledger refuses it,
	// Postgres stores it, and neither report joins it.
	cross := &Feedback{RequestID: events[0].ID, Tenant: "nobody", Score: 1}
	if err := mem.RecordFeedback(ctx, cross); !errors.Is(err, ErrUnknownRequest) {
		t.Errorf("memory: cross-tenant feedback = %v, want ErrUnknownRequest", err)
	}
	if err := pg.RecordFeedback(ctx, cross); err != nil {
		t.Fatal(err)
	}

	for _, q := range append(reportQueries(), qualityQueries()...) {
		want, err := mem.Report(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pg.Report(context.Background(), q)
		if err != nil {
			t.Fatalf("Report(tenant %q, group by %v): %v", q.Tenant, q.GroupBy, err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Report(tenant %q, group by %v) differs from the memory ledger (-memory +postgres):\n%s", q.Tenant, q.GroupBy, diff)
		}
		checkTotalsAgainstEvents(t, q, got, all, shadows...)
		checkQualityAgainstRows(t, q, got, all, shadows, feedback)
	}

	t.Run("tenant isolation in SQL", func(t *testing.T) {
		for _, tenant := range []string{"acme", "globex", "initech", "nobody"} {
			r, err := pg.Report(context.Background(), Query{
				Tenant: tenant, From: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
				GroupBy: []string{GroupByKey},
			})
			if err != nil {
				t.Fatal(err)
			}
			want := db.count(t, "SELECT count(*) FROM usage_events WHERE tenant = $1", tenant)
			var inGroups int64
			for _, g := range r.Groups {
				inGroups += g.Totals.Requests
			}
			if r.Totals.Requests != want || inGroups != want {
				t.Errorf("tenant %s: report counts %d requests (%d in groups), the table has %d", tenant, r.Totals.Requests, inGroups, want)
			}
			// The cost total equals a SQL sum restricted to the tenant: the
			// known cost and the overhead, which includes the cost of the
			// shadow evaluations that join to the tenant's events.
			var sqlSum string
			if err := db.admin.QueryRow(context.Background(),
				`SELECT (COALESCE(sum(cost_usd) FILTER (WHERE cost_known), 0) + COALESCE(sum(overhead_cost_usd), 0) +
	COALESCE((SELECT sum(s.shadow_cost_usd) FROM shadow_evals s JOIN usage_events e ON e.tenant = s.tenant AND e.id = s.request_id
		WHERE s.tenant = $1), 0))::text
FROM usage_events WHERE tenant = $1`,
				tenant).Scan(&sqlSum); err != nil {
				t.Fatal(err)
			}
			if !r.Totals.CostUSD.Equal(mustMoney(t, sqlSum)) {
				t.Errorf("tenant %s: report cost %s, SQL sum %s", tenant, r.Totals.CostUSD, sqlSum)
			}
		}
	})

	t.Run("exact NUMERIC sums", func(t *testing.T) {
		// A sum Postgres computes over NUMERIC equals the Go sum to the
		// femto-dollar.
		var sqlSum string
		if err := db.admin.QueryRow(context.Background(),
			"SELECT sum(cost_usd)::text FROM usage_events WHERE cost_known AND tenant = 'initech'").Scan(&sqlSum); err != nil {
			t.Fatal(err)
		}
		var goSum Money
		for _, ev := range events {
			if ev.CostKnown && ev.Tenant == "initech" {
				goSum = goSum.Add(ev.Cost)
			}
		}
		if got := mustMoney(t, sqlSum); !got.Equal(goSum) {
			t.Errorf("SQL sum %s != Go sum %s", got, goSum)
		}
		if _, frac, _ := strings.Cut(goSum.String(), "."); len(frac) < 12 {
			t.Errorf("Go sum %s has too few decimals to test exactness", goSum)
		}
	})
}

// proxy forwards TCP connections to a target until it is taken down, which
// closes every connection and refuses new ones until it is brought up.
type proxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	down  bool
	conns map[net.Conn]bool
	wg    sync.WaitGroup
}

func newProxy(t *testing.T, target string) *proxy {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{ln: ln, target: target, conns: map[net.Conn]bool{}}
	p.wg.Go(p.serve)
	t.Cleanup(func() {
		_ = ln.Close()
		p.setDown(true)
		p.wg.Wait()
	})
	return p
}

func (p *proxy) addr() string { return p.ln.Addr().String() }

func (p *proxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		down := p.down
		p.mu.Unlock()
		if down {
			_ = c.Close()
			continue
		}
		up, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		p.conns[c], p.conns[up] = true, true
		p.mu.Unlock()
		pipe := func(dst, src net.Conn) {
			_, _ = io.Copy(dst, src)
			_ = dst.Close()
			_ = src.Close()
			p.mu.Lock()
			delete(p.conns, dst)
			delete(p.conns, src)
			p.mu.Unlock()
		}
		p.wg.Go(func() { pipe(up, c) })
		p.wg.Go(func() { pipe(c, up) })
	}
}

func (p *proxy) setDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
	if down {
		for c := range p.conns {
			_ = c.Close()
		}
	}
}

func TestPostgresOutageBuffersThenFlushes(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	u, _ := url.Parse(db.dsn)
	p := newProxy(t, u.Host)
	reg := prometheus.NewRegistry()
	l := openLedger(t, db.withHost(t, p.addr()), PostgresOptions{
		FlushInterval: 20 * time.Millisecond, RetryMin: 10 * time.Millisecond, RetryMax: 100 * time.Millisecond,
		Registerer: reg,
	})
	defer closeLedger(t, l)
	rows := func() int64 { return db.count(t, "SELECT count(*) FROM usage_events") }

	for i := range 10 {
		l.Record(&Event{ID: "before-" + string(rune('a'+i)), Time: day0, Tenant: "acme"})
	}
	eventually(t, 30*time.Second, "the first events to be written", func() bool { return rows() == 10 })

	p.setDown(true)
	for i := range 50 {
		l.Record(&Event{ID: "during-" + hex.EncodeToString([]byte{byte(i)}), Time: day0, Tenant: "acme"})
	}
	// The writer fails, retries and keeps the events.
	eventually(t, 30*time.Second, "write errors during the outage", func() bool {
		return metric(t, reg, "switchyard_usage_write_errors_total") >= 3
	})
	if got := metric(t, reg, "switchyard_usage_queue_depth"); got != 50 {
		t.Errorf("queue depth during the outage = %v, want 50", got)
	}
	if n := rows(); n != 10 {
		t.Errorf("%d rows during the outage, want 10", n)
	}
	if got := metric(t, reg, "switchyard_usage_events_dropped_total"); got != 0 {
		t.Errorf("dropped = %v during the outage, want 0", got)
	}

	p.setDown(false)
	eventually(t, 30*time.Second, "the buffered events to be written", func() bool { return rows() == 60 })
	if got := metric(t, reg, "switchyard_usage_events_written_total"); got != 60 {
		t.Errorf("written = %v, want 60", got)
	}
	eventually(t, 5*time.Second, "the queue to drain", func() bool { return metric(t, reg, "switchyard_usage_queue_depth") == 0 })
}

func TestPostgresOutageOverflowDropsOldest(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	u, _ := url.Parse(db.dsn)
	p := newProxy(t, u.Host)
	reg := prometheus.NewRegistry()
	l := openLedger(t, db.withHost(t, p.addr()), PostgresOptions{
		QueueSize: 5, FlushInterval: 20 * time.Millisecond, RetryMin: 10 * time.Millisecond, RetryMax: 50 * time.Millisecond,
		Registerer: reg,
	})
	defer closeLedger(t, l)
	rows := func() int64 { return db.count(t, "SELECT count(*) FROM usage_events") }

	p.setDown(true)
	const recorded = 20
	for i := range recorded {
		l.Record(&Event{ID: "e" + hex.EncodeToString([]byte{byte(i)}), Time: day0, Tenant: "acme"})
	}
	// At most the queue (5) plus one in-flight batch (at most 5) survive.
	dropped := metric(t, reg, "switchyard_usage_events_dropped_total")
	if dropped < recorded-10 {
		t.Errorf("dropped = %v, want at least %d", dropped, recorded-10)
	}
	eventually(t, 30*time.Second, "a failed write", func() bool { return metric(t, reg, "switchyard_usage_write_errors_total") >= 1 })
	p.setDown(false)
	kept := int64(recorded - dropped)
	eventually(t, 30*time.Second, "the kept events to be written", func() bool { return rows() == kept })
	// The newest event always survives drop-oldest.
	if n := db.count(t, "SELECT count(*) FROM usage_events WHERE id = $1", "e"+hex.EncodeToString([]byte{recorded - 1})); n != 1 {
		t.Errorf("the newest event was dropped")
	}
	if got := metric(t, reg, "switchyard_usage_events_dropped_total"); got != dropped {
		t.Errorf("dropped changed after recovery: %v -> %v", dropped, got)
	}
}

// TestPostgresReportsDoNotStarveWriter runs heavy reports without pause
// while events are recorded: the reports run on a pool of their own and
// at most one at a time per tenant, so the writer keeps writing, and the
// reports beyond the limits are refused with ErrReportBusy.
func TestPostgresReportsDoNotStarveWriter(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	reg := prometheus.NewRegistry()
	l := openLedger(t, db.dsn, PostgresOptions{Registerer: reg, FlushInterval: 100 * time.Millisecond})
	defer closeLedger(t, l)
	if l.reports == l.pool || l.reports.Config().MaxConns != reportPoolConns {
		t.Fatalf("reports pool max conns %d, the same pool as the writer's: %v", l.reports.Config().MaxConns, l.reports == l.pool)
	}

	// Enough rows for each report to take a while.
	ctx := context.Background()
	now := time.Now().UTC()
	month := monthStart(now)
	if _, err := db.admin.Exec(ctx, "INSERT INTO usage_events ("+strings.Join(ledgerColumns, ", ")+`)
		SELECT 'seed-'||g, $1::timestamptz + (g % 86400) * interval '1 second', 'attacker', 'k', 'chat', 'm'||(g % 1000), '', 'd', 'p',
		false, 200, 1, '', '', 'miss', 10, 0, 0, 10, true, false, 0.000001, true, '{}', 'm', 0.000001, 0, 1, 1,
		jsonb_build_object('a', (g % 50000)::text, 'b', (g % 7)::text), 'v',
		'balanced', 'observe', 'm@p', 0.0000005, true, 'coding'
		FROM generate_series(1, 300000) g`, month); err != nil {
		t.Fatalf("seed: %v", err)
	}

	dims := []string{GroupByDay, GroupByModel, GroupByDeployment, GroupByProvider, GroupByKey, "tag:a", "tag:b", "tag:c"}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	outcomes := map[string]int{}
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tenant := "attacker"
			if i%4 == 3 {
				tenant = "" // the admin report across every tenant
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				_, err := l.Report(rctx, Query{Tenant: tenant, From: month, To: month.AddDate(0, 1, 0), GroupBy: dims, Limit: MaxReportLimit})
				cancel()
				outcome := "ok"
				switch {
				case errors.Is(err, ErrReportBusy):
					outcome = "busy"
					time.Sleep(time.Millisecond)
				case err != nil:
					outcome = "error"
				}
				mu.Lock()
				outcomes[outcome]++
				mu.Unlock()
			}
		}()
	}
	// Let the reports get going, then record.
	eventually(t, 30*time.Second, "a report refused as busy", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return outcomes["busy"] > 0
	})
	start := time.Now()
	for i := range 200 {
		l.Record(&Event{ID: fmt.Sprintf("victim-%d", i), Time: now, Tenant: "victim"})
	}
	eventually(t, 20*time.Second, "200 events written while reports run", func() bool {
		return metric(t, reg, "switchyard_usage_events_written_total") >= 200
	})
	t.Logf("200 events written %v after recording, while reports ran", time.Since(start))
	close(stop)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	t.Logf("report outcomes: %v", outcomes)
	if outcomes["error"] > 0 {
		t.Errorf("%d reports failed", outcomes["error"])
	}
}

// TestPostgresQuality: the hand-computed shadow and feedback reports of the
// memory ledger hold in Postgres, the stores keep the first evaluation and
// the latest feedback, and rows hold no content beyond the comment.
func TestPostgresQuality(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	f := newQualityFixture(t)
	l := openLedger(t, db.dsn, PostgresOptions{FlushInterval: 10 * time.Millisecond})
	f.load(t, l)
	ctx := context.Background()
	// A second evaluation is ignored and later feedback replaces earlier.
	if err := l.RecordShadowEval(ctx, &ShadowEval{RequestID: "e3", Tenant: "acme", Acceptable: true, Cost: mustMoney(t, "9")}); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordFeedback(ctx, &Feedback{RequestID: "e3", Tenant: "acme", KeyID: "k2", Time: qualityDay, Score: 0, Comment: "x\x00y"}); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordFeedback(ctx, &Feedback{RequestID: "e3", Tenant: "acme", Score: 5}); err == nil {
		t.Error("a score of 5 was stored")
	}
	closeLedger(t, l)

	pg := openLedger(t, db.dsn, PostgresOptions{})
	defer closeLedger(t, pg)
	checkQualityFixture(t, pg)

	if n := db.count(t, "SELECT count(*) FROM shadow_evals"); n != int64(len(f.shadows)) {
		t.Errorf("shadow_evals has %d rows, want %d", n, len(f.shadows))
	}
	var keyID, comment string
	if err := db.admin.QueryRow(ctx, "SELECT key_id, comment FROM feedback WHERE tenant = 'acme' AND request_id = 'e3'").Scan(&keyID, &comment); err != nil {
		t.Fatal(err)
	}
	if keyID != "k2" || comment != "xy" {
		t.Errorf("feedback e3 = key %q comment %q, want the latest, cleaned", keyID, comment)
	}
	var cost string
	if err := db.admin.QueryRow(ctx, "SELECT shadow_cost_usd::text FROM shadow_evals WHERE request_id = 'e3'").Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost != "0.100000000000000" {
		t.Errorf("shadow_evals e3 cost = %s, want the first evaluation's", cost)
	}
	var category string
	if err := db.admin.QueryRow(ctx, "SELECT category FROM usage_events WHERE id = 'e1'").Scan(&category); err != nil {
		t.Fatal(err)
	}
	if category != "coding" {
		t.Errorf("usage_events.category = %q, want coding", category)
	}
}

// TestPostgresMigration0003KeepsOldRows: events written before migration
// 0003 read as uncategorized, and the migration is safe to run again.
func TestPostgresMigration0003KeepsOldRows(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := context.Background()
	ms, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) < 3 || ms[2].version != 3 {
		t.Fatalf("migrations = %v, want 0001 to 0003", ms)
	}
	if _, err := migrate(ctx, db.admin, ms[:2]); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"CREATE TABLE usage_events_p202611 PARTITION OF usage_events FOR VALUES FROM ('2026-11-01T00:00:00Z') TO ('2026-12-01T00:00:00Z')",
		`INSERT INTO usage_events (id, ts, tenant, key_id, endpoint, requested_model, alias, deployment, provider, stream, status,
			attempts, fallback_reason, route_reason, cache_status, input_tokens, cached_input_tokens, cache_write_tokens,
			output_tokens, usage_known, usage_estimated, cost_usd, cost_known, pricing, baseline_model, baseline_cost_usd,
			overhead_cost_usd, latency_ms, ttft_ms, tags, config_version)
		VALUES ('old', '2026-11-05T12:00:00Z', 'acme', 'k1', '/v1/chat/completions', 'gpt', '', 'gpt@openai', 'openai', false, 200,
			1, '', 'cheapest_eligible', 'miss', 10, 0, 0, 5, true, false, 0.25, true, '{}', 'gpt', 0.5, 0, 1, 1, '{}', 'v1')`,
	} {
		if _, err := db.admin.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := migrate(ctx, db.admin, ms); err != nil {
		t.Fatal(err)
	}
	if _, err := db.admin.Exec(ctx, ms[2].sql); err != nil {
		t.Errorf("re-running 0003: %v", err)
	}
	l := openLedger(t, db.dsn, PostgresOptions{})
	defer closeLedger(t, l)
	ts := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	r, err := l.Report(ctx, Query{Tenant: "acme", From: ts.Add(-time.Hour), To: ts.Add(time.Hour), GroupBy: []string{GroupByCategory}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Totals.Requests != 1 || r.Totals.Quality != nil || len(r.Groups) != 1 || r.Groups[0].Key[GroupByCategory] != "" {
		t.Errorf("report of the old row = %+v", r)
	}
}
