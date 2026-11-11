package usage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestNewPostgresLedgerErrorsHideTheDSN(t *testing.T) {
	tests := []struct {
		name, dsn string
	}{
		{"malformed", "postgres://user:hunter2@[::1:5432/x"},
		{"malformed key/value", "host=localhost password=hunter2 port=notaport"},
		// Nothing listens on port 1, so connecting fails at once.
		{"unreachable", "postgres://user:hunter2@127.0.0.1:1/x?connect_timeout=5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			l, err := NewPostgresLedger(ctx, tt.dsn, PostgresOptions{})
			if err == nil {
				_ = l.Close(ctx)
				t.Fatal("NewPostgresLedger succeeded")
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error reveals the password: %v", err)
			}
		})
	}
}

func TestLoadMigrations(t *testing.T) {
	ms, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 || ms[0].version != 1 || !strings.Contains(ms[0].sql, "PARTITION BY RANGE (ts)") {
		t.Errorf("embedded migrations = %+v", ms)
	}

	tests := []struct {
		name    string
		files   fstest.MapFS
		want    []int
		wantErr bool
	}{
		{"sorted by number", fstest.MapFS{
			"m/0010_c.sql": {Data: []byte("c")}, "m/0002_b.sql": {Data: []byte("b")}, "m/0001_a.sql": {Data: []byte("a")},
			"m/README": {Data: []byte("ignored")},
		}, []int{1, 2, 10}, false},
		{"duplicate version", fstest.MapFS{"m/0001_a.sql": {}, "m/1_b.sql": {}}, nil, true},
		{"no number", fstest.MapFS{"m/init.sql": {}}, nil, true},
		{"zero", fstest.MapFS{"m/0000_a.sql": {}}, nil, true},
		{"signed", fstest.MapFS{"m/+1_a.sql": {}}, nil, true},
		{"empty", fstest.MapFS{"m/README": {}}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms, err := loadMigrations(tt.files, "m")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			var got []int
			for _, m := range ms {
				got = append(got, m.version)
			}
			if !tt.wantErr && !equalInts(got, tt.want) {
				t.Errorf("versions = %v, want %v", got, tt.want)
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDBErrHidesMessages(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type uuid: "secret-value"`}
	err := dbErr("write events", pgErr)
	if strings.Contains(err.Error(), "secret-value") {
		t.Errorf("error reveals row data: %v", err)
	}
	if _, ok := errors.AsType[*permanentError](err); !ok {
		t.Errorf("a data exception is not permanent")
	}
	if !errors.Is(err, pgErr) {
		t.Errorf("the PgError is not wrapped")
	}
	transient := dbErr("write events", &pgconn.PgError{Code: "57P01", Message: "terminating connection"})
	if _, ok := errors.AsType[*permanentError](transient); ok {
		t.Errorf("an admin shutdown is permanent")
	}
}

func TestEventRowCleansText(t *testing.T) {
	ev := &Event{ID: "a\x00b", RequestedModel: "m\xff", Status: 1 << 40, Attempts: -1 << 40, Tags: map[string]string{"k\x00": "v\xfe"}}
	row := eventRow(ev)
	if row[0] != "ab" || row[5] != "m�" {
		t.Errorf("text not cleaned: %q %q", row[0], row[5])
	}
	if row[10] != int32(1<<31-1) || row[11] != int32(-1<<31) {
		t.Errorf("integers not clamped: %v %v", row[10], row[11])
	}
	if tags := row[29].(map[string]string); tags["k"] != "v�" {
		t.Errorf("tags not cleaned: %q", tags)
	}
	if len(row) != len(ledgerColumns) {
		t.Errorf("row has %d values for %d columns", len(row), len(ledgerColumns))
	}
}

func TestPartitionNaming(t *testing.T) {
	m := monthStart(time.Date(2026, 1, 1, 0, 30, 0, 0, time.FixedZone("x", 3600)))
	if got := partitionName(m); got != "usage_events_p202512" {
		t.Errorf("partition for 2026-01-01T00:30+01:00 = %s, want usage_events_p202512", got)
	}
}

// TestReportErr checks that a database program limit, such as too many
// columns, is reported as an invalid query, without the database's
// message, and that other errors stay database errors.
func TestReportErr(t *testing.T) {
	tests := []struct {
		code    string
		invalid bool
	}{
		{"54011", true}, // too_many_columns
		{"54001", true}, // statement_too_complex
		{"54000", true}, // program_limit_exceeded
		{"57014", false},
		{"08006", false},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			err := reportErr(&pgconn.PgError{Code: tt.code, Message: "target lists can have at most 1664 entries secret"})
			if errors.Is(err, ErrInvalidQuery) != tt.invalid {
				t.Errorf("reportErr(%s) = %v; invalid query %v", tt.code, err, !tt.invalid)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("the error reveals the database's message: %v", err)
			}
		})
	}
}

// TestReportLimiter checks the report slots: one per tenant at once, the
// admin report across every tenant counted as a tenant of its own, and
// four in all.
func TestReportLimiter(t *testing.T) {
	l := newReportLimiter(maxReportsPerTenant, maxReports)
	if !l.acquire("a") {
		t.Fatal("first report of a refused")
	}
	if l.acquire("a") {
		t.Error("a second concurrent report of a was allowed")
	}
	for _, tenant := range []string{"", "b", "c"} {
		if !l.acquire(tenant) {
			t.Errorf("report of %q refused", tenant)
		}
	}
	if l.acquire("d") {
		t.Error("a fifth concurrent report was allowed")
	}
	l.release("a")
	if !l.acquire("d") {
		t.Error("report of d refused after a slot was released")
	}
	if l.acquire("a") {
		t.Error("report of a allowed beyond the total")
	}
	for _, tenant := range []string{"", "b", "c", "d"} {
		l.release(tenant)
	}
	if l.total != 0 || len(l.byTenant) != 0 {
		t.Errorf("after every release: total %d, by tenant %v", l.total, l.byTenant)
	}
}
