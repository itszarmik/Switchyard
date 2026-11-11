// Package integration runs the Phase 4 acceptance scenarios through the
// gateway binary against a real Postgres ledger and real Redis budgets,
// and the Postgres outage test. Its tests are behind the integration build
// tag and need the compose services that make test-integration starts:
//
//	make test-integration
//
// They read SWITCHYARD_TEST_POSTGRES_DSN, SWITCHYARD_TEST_REDIS_URL and,
// for the outage test, SWITCHYARD_TEST_COMPOSE, the compose command of the
// test project (for example "docker-compose -p switchyard-test -f
// deploy/compose.yaml"), and skip without them.
package integration
