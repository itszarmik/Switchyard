// Package usage is Switchyard's cost accounting (DESIGN §12): exact money,
// token normalization and cost calculation, the usage event recorded for
// every request, the ledgers that store events, and the reports that
// aggregate them.
//
// Money is exact. Prices are the decimal text from the config, costs are
// computed in whole femto-USD (1e-15 USD) with math/big, the Postgres
// ledger stores NUMERIC(38,15) and sums in SQL, and reports return Money.
// Only metrics use floats.
//
// Two ledgers implement Sink and Reporter: MemoryLedger, for the quickstart
// and tests, and PostgresLedger, whose asynchronous batching writer keeps
// requests independent of the database: events are queued, written in
// batches with COPY, buffered and retried with backoff while the database
// is down, and the oldest are dropped and counted if the queue overflows.
//
// Reports filter on Query.Tenant in the store itself. Every caller acting
// for a tenant must set it; only the admin API may leave it empty.
package usage
