package usage

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationFiles holds the ledger's schema migrations, named
// NNNN_description.sql and applied in version order.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// Advisory lock keys that serialize schema changes across gateway replicas
// sharing one database.
const (
	migrateLockKey   int64 = 0x5359_5553_4147_0001 // "SYUSAG" 1
	partitionLockKey int64 = 0x5359_5553_4147_0002
)

// migration is one numbered schema change.
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads every NNNN_description.sql file in the root of fsys
// (or its migrations directory) and returns them sorted by version. Each
// version must be positive and unique.
func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("usage: read migrations: %w", err)
	}
	var ms []migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		num, _, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 || !allDigits(num) {
			return nil, fmt.Errorf("usage: migration %s: name must be NNNN_description.sql", e.Name())
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("usage: migrations %s and %s have the same version", prev, e.Name())
		}
		seen[v] = e.Name()
		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("usage: read migration %s: %w", e.Name(), err)
		}
		ms = append(ms, migration{version: v, name: e.Name(), sql: string(body)})
	}
	if len(ms) == 0 {
		return nil, errors.New("usage: no migrations found")
	}
	slices.SortFunc(ms, func(a, b migration) int { return a.version - b.version })
	return ms, nil
}

// migrate applies the migrations not yet recorded in schema_migrations, in
// one transaction under an advisory lock, so that concurrent replicas
// apply each exactly once and a failed migration leaves no trace. Tables
// are created in the first schema of the connection's search_path. It
// returns the versions it applied.
func migrate(ctx context.Context, pool *pgxpool.Pool, ms []migration) ([]int, error) {
	var applied []int
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrateLockKey); err != nil {
			return dbErr("lock for migrations", err)
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    integer     PRIMARY KEY,
	name       text        NOT NULL,
	applied_at timestamptz NOT NULL DEFAULT now()
)`); err != nil {
			return dbErr("create schema_migrations", err)
		}
		rows, err := tx.Query(ctx, "SELECT version FROM schema_migrations")
		if err != nil {
			return dbErr("read schema_migrations", err)
		}
		done, err := pgx.CollectRows(rows, pgx.RowTo[int])
		if err != nil {
			return dbErr("read schema_migrations", err)
		}
		for _, m := range ms {
			if slices.Contains(done, m.version) {
				continue
			}
			// Without arguments, Exec uses the simple protocol, which allows
			// several statements in one migration.
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return dbErr("apply migration "+m.name, err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.version, m.name); err != nil {
				return dbErr("record migration "+m.name, err)
			}
			applied = append(applied, m.version)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return applied, nil
}
