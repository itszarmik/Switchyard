//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeupstream"
	"switchyard/test/scenario"
)

const (
	dsnEnv     = "SWITCHYARD_TEST_POSTGRES_DSN"
	redisEnv   = "SWITCHYARD_TEST_REDIS_URL"
	composeEnv = "SWITCHYARD_TEST_COMPOSE"
)

func env(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skip(name + " is not set; run make test-integration")
	}
	return v
}

// schema is a fresh Postgres schema for one gateway's ledger.
type schema struct {
	name string
	// dsn is the test DSN with search_path set to the schema, so the
	// ledger creates its tables there.
	dsn  string
	pool *pgxpool.Pool
}

func newSchema(t *testing.T) *schema {
	t.Helper()
	base := env(t, dsnEnv)
	ctx := context.Background()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	s := &schema{name: "sy_it_" + hex.EncodeToString(b[:])}
	root, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(root.Close)
	if _, err := root.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{s.name}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := root.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{s.name}.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s must be a URL", dsnEnv)
	}
	q := u.Query()
	q.Set("search_path", s.name)
	u.RawQuery = q.Encode()
	s.dsn = u.String()
	if s.pool, err = pgxpool.New(ctx, s.dsn); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.pool.Close)
	return s
}

// count returns the number of rows in the schema's usage_events.
func (s *schema) count(t *testing.T) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM usage_events").Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// postgresStorage is the storage section of a gateway with a Postgres
// ledger whose DSN is $DATABASE_URL.
func postgresStorage(queueSize int) string {
	return fmt.Sprintf(`storage:
  ledger: {driver: postgres, dsn: "${DATABASE_URL}", batch_size: 50, flush_interval: 100ms, queue_size: %d}`, queueSize)
}

// TestPostgresLedgerRows runs the exact-cost scenario with the Postgres
// ledger, then checks each request's own row: its exact NUMERIC cost, its
// tokens and the price snapshot.
func TestPostgresLedgerRows(t *testing.T) {
	s := newSchema(t)
	cases := scenario.ExactCost(t, scenario.Options{
		Storage: postgresStorage(100_000), Env: []string{"DATABASE_URL=" + s.dsn}, Race: raceEnabled,
	})
	ctx := context.Background()
	for _, c := range cases {
		var cost, provider, pricing string
		var known, estimated bool
		err := s.pool.QueryRow(ctx,
			"SELECT cost_usd::text, cost_known, usage_estimated, provider, pricing::text FROM usage_events WHERE id = $1", c.RequestID).
			Scan(&cost, &known, &estimated, &provider, &pricing)
		if err != nil {
			t.Errorf("%s: row %s: %v", c.Name, c.RequestID, err)
			continue
		}
		got, _ := new(big.Rat).SetString(cost)
		want, _ := new(big.Rat).SetString(c.Want)
		if got == nil || got.Cmp(want) != 0 || !known || estimated || provider == "" || !strings.Contains(pricing, `"input"`) {
			t.Errorf("%s: row cost %s known %v estimated %v provider %q pricing %s; want cost %s", c.Name, cost, known, estimated, provider, pricing, c.Want)
		}
		t.Logf("%s: ledger row cost_usd %s (want %s)", c.Name, cost, c.Want)
	}
}

// TestRedisBudgetAcceptance runs the 1,000-request budget scenario with
// the budget counters in the real Redis state instance.
func TestRedisBudgetAcceptance(t *testing.T) {
	redisURL := env(t, redisEnv)
	u, err := url.Parse(redisURL)
	if err != nil {
		t.Fatalf("%s must be a URL", redisEnv)
	}
	cache := *u
	cache.Path = "/1"
	storage := fmt.Sprintf(`storage:
  mode: redis
  redis: {cache_url: %q, state_url: %q}`, cache.String(), redisURL)
	for _, tc := range []struct {
		name  string
		extra int
	}{{"exact estimates", 0}, {"under-estimates", 37}} {
		t.Run(tc.name, func(t *testing.T) {
			scenario.BudgetConcurrency(t, scenario.Options{Storage: storage, Race: raceEnabled}, 1000, tc.extra)
		})
	}
}

// compose runs the test project's compose command with args.
func compose(t *testing.T, args ...string) {
	t.Helper()
	parts := strings.Fields(env(t, composeEnv))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, parts[0], append(parts[1:], args...)...) // #nosec G204 -- the test's own compose command
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", strings.Join(parts, " "), strings.Join(args, " "), err, out)
	}
}

// metric scrapes one metric's value from a gateway's admin /metrics.
func metric(t *testing.T, g *harness.Gateway, name string) float64 {
	t.Helper()
	resp, err := http.Get(g.AdminURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, name+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("%s: %v", line, err)
			}
			return f
		}
	}
	t.Fatalf("/metrics has no %s", name)
	return 0
}

// outageGateway is a gateway with a Postgres ledger under load.
type outageGateway struct {
	name     string
	g        *harness.Gateway
	db       *schema
	key      string
	sent     atomic.Int64
	failures atomic.Int64
	mu       sync.Mutex
	firstErr string
}

// TestPostgresOutage stops Postgres for 60 s while two gateways serve
// load: one with the default queue, which must lose nothing, and one with
// a 5-event queue, which must overflow and count the dropped events. No
// request may fail, and once Postgres is back every queued event must
// land.
func TestPostgresOutage(t *testing.T) {
	env(t, composeEnv)
	const outage = 60 * time.Second
	up := fakeupstream.New(fakeupstream.Options{DisableRecording: true})
	t.Cleanup(up.Close)

	gws := []*outageGateway{{name: "default queue"}, {name: "tiny queue"}}
	for i, o := range gws {
		o.db = newSchema(t)
		key, hash, err := harness.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		o.key = key
		queue := 100_000
		if i == 1 {
			queue = 5
		}
		cfg := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 20s}
logging: {level: info, format: json}
%s
providers:
  - {id: up, type: openai, base_url: %q}
models:
  - id: m
    deployments:
      - {provider: up, upstream_model: m-up, pricing: {input: "1", output: "2"}}
tenants:
  - id: outage
    keys: [{id: k1, hash: %q}]
`, postgresStorage(queue), up.BaseURL(), hash)
		o.g = harness.RunYAML(t, []byte(cfg), harness.RunOptions{Race: raceEnabled, Env: []string{"DATABASE_URL=" + o.db.dsn}})
	}

	// Load: four closed-loop workers per gateway, about 100 requests per
	// second each, until stopped.
	client := &http.Client{Timeout: 30 * time.Second}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	body := `{"model":"m","messages":[{"role":"user","content":"outage"}]}`
	for _, o := range gws {
		for range 4 {
			wg.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					req, _ := http.NewRequest(http.MethodPost, o.g.PublicURL+"/v1/chat/completions", strings.NewReader(body))
					req.Header.Set("Authorization", "Bearer "+o.key)
					resp, err := client.Do(req)
					o.sent.Add(1)
					status := 0
					if err == nil {
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
						status = resp.StatusCode
					}
					if err != nil || status != http.StatusOK {
						o.failures.Add(1)
						o.mu.Lock()
						if o.firstErr == "" {
							o.firstErr = fmt.Sprintf("status %d, error %v", status, err)
						}
						o.mu.Unlock()
					}
					time.Sleep(10 * time.Millisecond)
				}
			})
		}
	}
	stopLoad := sync.OnceFunc(func() { close(stop); wg.Wait() })
	t.Cleanup(stopLoad)

	time.Sleep(2 * time.Second)
	before := gws[0].db.count(t)
	t.Logf("before the outage: %d events written by the default-queue gateway", before)
	if before == 0 {
		t.Error("no events were written before the outage")
	}

	compose(t, "stop", "postgres")
	stopped := time.Now()
	// Make sure Postgres comes back whatever happens next.
	restarted := false
	t.Cleanup(func() {
		if !restarted {
			compose(t, "up", "-d", "--wait", "postgres")
		}
	})
	time.Sleep(outage / 2)
	depth := metric(t, gws[0].g, "switchyard_usage_queue_depth")
	errs := metric(t, gws[0].g, "switchyard_usage_write_errors_total")
	t.Logf("%s into the outage: queue depth %v, write errors %v", time.Since(stopped).Round(time.Second), depth, errs)
	if depth == 0 || errs == 0 {
		t.Errorf("during the outage the default-queue gateway has queue depth %v and %v write errors; want both > 0", depth, errs)
	}
	time.Sleep(time.Until(stopped.Add(outage)))
	compose(t, "up", "-d", "--wait", "postgres")
	restarted = true
	t.Logf("Postgres was down for %s", time.Since(stopped).Round(time.Second))
	time.Sleep(2 * time.Second)
	stopLoad()

	for _, o := range gws {
		if n := o.failures.Load(); n > 0 {
			t.Errorf("%s: %d of %d requests failed; first: %s", o.name, n, o.sent.Load(), o.firstErr)
		}
	}

	// The default queue loses nothing: every event lands, within the
	// writer's 10 s maximum backoff and a few flushes.
	a := gws[0]
	sent := a.sent.Load()
	deadline := time.Now().Add(60 * time.Second)
	var rows int64
	for {
		if rows = a.db.count(t); rows >= sent || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	dropped := metric(t, a.g, "switchyard_usage_events_dropped_total")
	t.Logf("%s: %d requests, %d events written, %d dropped", a.name, sent, rows, int64(dropped))
	if rows != sent || dropped != 0 {
		t.Errorf("%s: %d requests but %d events written and %v dropped", a.name, sent, rows, dropped)
	}

	// The tiny queue overflows: what is not written is counted as dropped.
	b := gws[1]
	sent = b.sent.Load()
	deadline = time.Now().Add(60 * time.Second)
	for {
		rows = b.db.count(t)
		dropped = metric(t, b.g, "switchyard_usage_events_dropped_total")
		if rows+int64(dropped) >= sent || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("%s: %d requests, %d events written, %d dropped", b.name, sent, rows, int64(dropped))
	if dropped == 0 || rows+int64(dropped) != sent {
		t.Errorf("%s: %d requests, %d events written and %v dropped; want drops, and written + dropped = requests", b.name, sent, rows, dropped)
	}
}
