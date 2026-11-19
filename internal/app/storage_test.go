package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/auth"
	"switchyard/internal/budget"
	"switchyard/internal/config"
	"switchyard/internal/usage"
	"switchyard/test/fakeupstream"
)

// storageConfig is a config with one tenant, whose key hash is HASH, and
// the storage section STORAGE.
const storageConfig = `version: 1
server: {listen: 127.0.0.1:0, admin_listen: 127.0.0.1:0, shutdown_grace: 5s}
logging: {level: info, format: json}
STORAGE
providers:
  - {id: openai, type: openai, base_url: "https://api.openai.com/v1", api_key: "sk-unused"}
models:
  - id: gpt-5-mini
    deployments:
      - {provider: openai, upstream_model: gpt-5-mini, pricing: {input: "0.25", output: "2.00"}}
tenants:
  - id: test
    keys: [{id: test-key, hash: HASH}]
    budgets: [{scope: tenant, window: day, limit_usd: "1"}]
`

func storageYAML(storage, hash string) string {
	if hash == "" {
		hash = "sha256:0f7d5a33e1ad68ae7b2a4b139d4c5bf17737e0db942bfbecc5792bb3436d3e59"
	}
	return strings.NewReplacer("STORAGE", storage, "HASH", hash).Replace(storageConfig)
}

// TestPostgresUnreachableAtStartup checks that an unreachable Postgres
// stops New, since the ledger's migrations must run, with an error that
// names the setting and never the DSN or its password.
func TestPostgresUnreachableAtStartup(t *testing.T) {
	restoreDefaultLogger(t)
	const pass = "pgPassw0rd-unreachable"
	yaml := storageYAML(`storage:
  ledger: {driver: postgres, dsn: "postgres://switchyard:`+pass+`@127.0.0.1:1/switchyard?sslmode=disable&connect_timeout=2"}`, "")
	logs := newLogBuffer()
	_, err := New(loadSnapshot(t, yaml), logs, nil)
	if err == nil {
		t.Fatal("New succeeded with an unreachable Postgres")
	}
	msg := err.Error()
	t.Logf("New: %s", msg)
	if !strings.Contains(msg, "storage.ledger") || !strings.Contains(msg, "reachable at startup") {
		t.Errorf("error = %q, want it to name storage.ledger and the reason", msg)
	}
	for _, s := range []string{pass, "postgres://"} {
		if strings.Contains(msg, s) || strings.Contains(logs.String(), s) {
			t.Errorf("the error or the logs contain %q: %s\n%s", s, msg, logs.String())
		}
	}
}

// TestRedisBudgetStore runs the app with storage.mode redis: Redis down at
// startup is only a warning. With budgets.fail_mode closed, /readyz
// follows Redis through the budget_store check; with open, requests are
// served while Redis is down, so the replica stays ready, and
// GET /admin/v1/providers reports the store.
func TestRedisBudgetStore(t *testing.T) {
	for _, mode := range []string{config.BudgetFailOpen, config.BudgetFailClosed} {
		t.Run(mode, func(t *testing.T) {
			restoreDefaultLogger(t)
			mr := miniredis.NewMiniRedis()
			addr := "127.0.0.1:0"
			// Reserve an address, then stop Redis so that the app starts without it.
			if err := mr.StartAddr(addr); err != nil {
				t.Fatal(err)
			}
			addr = mr.Addr()
			mr.Close()

			yaml := storageYAML(`storage:
  mode: redis
  redis: {cache_url: "redis://`+addr+`/1", state_url: "redis://:statePass-77@`+addr+`/0"}
budgets: {fail_mode: `+mode+`}`, "")
			logs := newLogBuffer()
			a, err := New(loadSnapshot(t, yaml), logs, nil)
			if err != nil {
				t.Fatalf("New with Redis down: %v", err)
			}
			logs.attach(a)
			if !strings.Contains(logs.String(), "the Redis state instance is unreachable") {
				t.Errorf("no startup warning; logs:\n%s", logs.String())
			}
			r := runApp(t, a, logs)
			c := &http.Client{Timeout: 10 * time.Second}

			get := func(url string) (int, string) {
				t.Helper()
				resp, err := c.Get(url)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				return resp.StatusCode, string(b)
			}
			budgetStore := func() map[string]any {
				t.Helper()
				_, body := get(r.admin + "/admin/v1/providers")
				var rep struct {
					BudgetStore map[string]any `json:"budget_store"`
				}
				if err := json.Unmarshal([]byte(body), &rep); err != nil {
					t.Fatalf("providers report: %v: %s", err, body)
				}
				return rep.BudgetStore
			}
			code, body := get(r.public + "/readyz")
			if mode == config.BudgetFailClosed && (code != http.StatusServiceUnavailable || !strings.Contains(body, "budget_store")) {
				t.Errorf("/readyz with Redis down = %d %s, want 503 naming budget_store", code, body)
			}
			if mode == config.BudgetFailOpen && code != http.StatusOK {
				t.Errorf("/readyz with Redis down and fail_mode open = %d %s, want 200", code, body)
			}
			if got := budgetStore(); got["status"] != "unavailable" || got["fail_mode"] != mode {
				t.Errorf("budget_store with Redis down = %v", got)
			}
			if err := mr.StartAddr(addr); err != nil {
				t.Fatalf("restart Redis on %s: %v", addr, err)
			}
			defer mr.Close()
			mr.RequireAuth("statePass-77")
			if code, body := get(r.public + "/readyz"); code != http.StatusOK {
				t.Errorf("/readyz with Redis up = %d %s, want 200", code, body)
			}
			if got := budgetStore(); got["status"] != "ok" {
				t.Errorf("budget_store with Redis up = %v", got)
			}
			r.stop(t)
			if strings.Contains(logs.String(), "statePass-77") {
				t.Error("the logs contain the Redis password")
			}
		})
	}
}

// blackhole listens on a local address and accepts connections, but never
// answers: a Redis that hangs. It closes them when the test ends.
func blackhole(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

// TestBlackholedRedisBudgetStore checks that a Redis that never answers
// costs the first request about the budget store's call timeout, and the
// requests after it, while the store is marked unavailable, about
// nothing: fail_mode open serves them, closed answers 503 at once.
func TestBlackholedRedisBudgetStore(t *testing.T) {
	up := fakeupstream.New(fakeupstream.Options{})
	t.Cleanup(up.Close)
	for _, mode := range []string{config.BudgetFailOpen, config.BudgetFailClosed} {
		t.Run(mode, func(t *testing.T) {
			restoreDefaultLogger(t)
			key, hash, err := auth.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			addr := blackhole(t)
			yaml := strings.NewReplacer("BASE_URL", up.BaseURL(), "ADDR", addr, "MODE", mode, "HASH", hash).Replace(`version: 1
server: {listen: 127.0.0.1:0, admin_listen: 127.0.0.1:0, shutdown_grace: 5s}
logging: {level: info, format: json}
storage:
  mode: redis
  redis: {cache_url: "redis://ADDR/1", state_url: "redis://ADDR/0"}
budgets: {fail_mode: MODE}
providers:
  - {id: up, type: openai, base_url: "BASE_URL", api_key: "sk-unused"}
models:
  - id: m
    deployments:
      - {provider: up, upstream_model: m, pricing: {input: "1", output: "1"}}
tenants:
  - id: test
    keys: [{id: test-key, hash: HASH}]
    budgets: [{scope: tenant, window: day, limit_usd: "1"}]
`)
			logs := newLogBuffer()
			a, err := New(loadSnapshot(t, yaml), logs, nil)
			if err != nil {
				t.Fatal(err)
			}
			r := runApp(t, a, logs)
			c := &http.Client{Timeout: 30 * time.Second}
			chat := func() (int, time.Duration) {
				t.Helper()
				req, err := http.NewRequest(http.MethodPost, r.public+"/v1/chat/completions",
					strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+key)
				start := time.Now()
				resp, err := c.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				return resp.StatusCode, time.Since(start)
			}
			want := http.StatusOK
			if mode == config.BudgetFailClosed {
				want = http.StatusServiceUnavailable
			}
			code, first := chat()
			t.Logf("first request: %d after %v", code, first)
			if code != want || first > 2*time.Second {
				t.Errorf("first request: %d after %v; want %d within about the 1s call timeout", code, first, want)
			}
			for i := range 5 {
				code, d := chat()
				if code != want || d > 500*time.Millisecond {
					t.Errorf("request %d while the store is marked unavailable: %d after %v; want %d at once", i+2, code, d, want)
				}
			}
			r.stop(t)
			if n := strings.Count(logs.String(), `"msg":"budget store unavailable`); n != 1 {
				t.Errorf("logged the store failure %d times, want once:\n%s", n, logs.String())
			}
		})
	}
}

// runApp runs a until the test ends, as startApp does.
func runApp(t *testing.T, a *App, logs *logBuffer) *running {
	t.Helper()
	return startBuilt(t, a, logs)
}

// TestUsageEndpoints checks the wiring of the usage reports: the public
// one needs a key and reports the caller's tenant, the admin one any
// tenant, and an authenticated request appears in both.
func TestUsageEndpoints(t *testing.T) {
	restoreDefaultLogger(t)
	key, hash, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	logs := newLogBuffer()
	r := startApp(t, loadSnapshot(t, storageYAML("", hash)), logs)
	c := &http.Client{Timeout: 10 * time.Second}

	do := func(method, url, key, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	if code, _ := do(http.MethodGet, r.public+"/switchyard/v1/usage", "", ""); code != http.StatusUnauthorized {
		t.Errorf("usage without a key: %d, want 401", code)
	}
	// An authenticated request for an unknown model: one usage event.
	if code, _ := do(http.MethodPost, r.public+"/v1/chat/completions", key,
		`{"model":"nope","messages":[{"role":"user","content":"hi"}]}`); code != http.StatusNotFound {
		t.Fatalf("chat: %d, want 404", code)
	}

	type report struct {
		Tenant *string
		Totals struct {
			Requests, Errors int64
			CostUSD          string `json:"cost_usd"`
		}
		Groups []map[string]any
	}
	check := func(name, url, key, wantTenant string) {
		t.Helper()
		code, body := do(http.MethodGet, url, key, "")
		var rep report
		if code != http.StatusOK || json.Unmarshal(body, &rep) != nil {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		tenant := "<null>"
		if rep.Tenant != nil {
			tenant = *rep.Tenant
		}
		if tenant != wantTenant || rep.Totals.Requests != 1 || rep.Totals.Errors != 1 || rep.Totals.CostUSD != "0" ||
			len(rep.Groups) != 1 || rep.Groups[0]["status_class"] != "4xx" {
			t.Errorf("%s: %s", name, body)
		}
	}
	check("public", r.public+"/switchyard/v1/usage?group_by=status_class", key, "test")
	check("admin, all tenants", r.admin+"/admin/v1/usage?group_by=status_class", "", "<null>")
	check("admin, one tenant", r.admin+"/admin/v1/usage?tenant=test&group_by=status_class", "", "test")
	if code, _ := do(http.MethodGet, r.public+"/admin/v1/usage", key, ""); code != http.StatusNotFound {
		t.Errorf("admin report on the public listener: %d, want 404", code)
	}
	if code, body := do(http.MethodGet, r.public+"/switchyard/v1/usage?group_by=nope", key, ""); code != http.StatusBadRequest ||
		!strings.Contains(string(body), "invalid_query") {
		t.Errorf("bad group_by: %d %s", code, body)
	}
	r.stop(t)
}

// closeRecorder is a memory ledger that records how it was closed, and
// whether the app's tracing was still live then.
type closeRecorder struct {
	*usage.MemoryLedger
	app         *App
	closed      chan struct{}
	hasDeadline bool
	remaining   time.Duration
	tracingLive bool
}

func (c *closeRecorder) Close(ctx context.Context) error {
	dl, ok := ctx.Deadline()
	c.hasDeadline, c.remaining = ok, time.Until(dl)
	_, span := c.app.tracing.Tracer().Start(ctx, "probe")
	c.tracingLive = span.IsRecording()
	span.End()
	close(c.closed)
	return c.MemoryLedger.Close(ctx)
}

// TestLedgerClosedAtShutdown checks that Run closes the ledger once the
// server has drained, within the post-drain bound and not a fresh
// shutdown grace, and before it shuts tracing down: money first, so a
// slow collector cannot delay the ledger's flush.
func TestLedgerClosedAtShutdown(t *testing.T) {
	restoreDefaultLogger(t)
	led := &closeRecorder{MemoryLedger: usage.NewMemoryLedger(), closed: make(chan struct{})}
	logs := newLogBuffer()
	yaml := storageYAML("", "") + "telemetry: {tracing: {enabled: true, endpoint: 'http://127.0.0.1:9', sample_ratio: 1}}\n"
	a, err := newApp(loadSnapshot(t, yaml), logs, func(*config.Config, prometheus.Registerer, *slog.Logger) (storage, error) {
		return storage{ledger: led, budgets: budget.NewMemoryStore()}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	led.app = a
	r := startBuilt(t, a, logs)
	select {
	case <-led.closed:
		t.Fatal("the ledger was closed while serving")
	default:
	}
	r.stop(t)
	select {
	case <-led.closed:
	default:
		t.Fatal("Run returned without closing the ledger")
	}
	if !led.hasDeadline || led.remaining <= storageCloseTimeout-time.Second || led.remaining > storageCloseTimeout {
		t.Errorf("Close had deadline %v, %v left; want the %v storage bound", led.hasDeadline, led.remaining, storageCloseTimeout)
	}
	if !led.tracingLive {
		t.Error("tracing was shut down before the ledger was closed")
	}
}

// TestPostDrainBounds: the post-drain phases fit in PostDrainTimeout, and
// leave the ledger most of the storage bound.
func TestPostDrainBounds(t *testing.T) {
	tests := []struct {
		name string
		got  time.Duration
		max  time.Duration
	}{
		{"storage + tracing + logs", storageCloseTimeout + tracingShutdownTimeout + logCloseTimeout, PostDrainTimeout},
		{"shadow + cache within storage, 6s left for the ledger", shadowCloseTimeout + cacheCloseTimeout + 6*time.Second, storageCloseTimeout},
	}
	for _, tt := range tests {
		if tt.got > tt.max {
			t.Errorf("%s = %v, more than %v", tt.name, tt.got, tt.max)
		}
	}
}
