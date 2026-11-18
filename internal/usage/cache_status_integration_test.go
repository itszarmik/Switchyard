//go:build integration

package usage

import "testing"

// TestPostgresCacheStatuses checks that the Postgres ledger reports the
// cache statuses exactly as the memory ledger does (checkCacheStatuses).
func TestPostgresCacheStatuses(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	l := openLedger(t, db.dsn, PostgresOptions{})
	for _, ev := range cacheEvents(t) {
		l.Record(ev)
	}
	closeLedger(t, l)

	pg := openLedger(t, db.dsn, PostgresOptions{})
	defer closeLedger(t, pg)
	checkCacheStatuses(t, pg)
}
