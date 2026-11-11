package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/auth"
	"switchyard/internal/budget"
	"switchyard/internal/config"
	"switchyard/internal/telemetry"
	"switchyard/internal/usage"
)

// Secret values used by the tests. The Anthropic key matches no credential
// pattern, so only exact-value scrubbing can catch it.
const (
	openAIKey    = "sk-test-4f9a8b7c6d5e4f3a2b1c0d9e"
	anthropicKey = "Zq8pL2xW9vN4mK7jR3tY"
	redisPass    = "redisPassw0rd77"
	redisURL     = "redis://:" + redisPass + "@localhost:6379/0"
	statePass    = "statePassw0rd77"
	stateURL     = "redis://default:" + statePass + "@localhost:6380/0"
	dbPass       = "dbPassw0rd77"
	dsn          = "postgres://switchyard:" + dbPass + "@localhost:5432/switchyard?sslmode=disable"
)

// secrets lists every value that must never be logged, including the
// passwords on their own.
var secrets = []string{openAIKey, anthropicKey, redisURL, redisPass, stateURL, statePass, dsn, dbPass}

const secretConfig = `version: 1
server:
  listen: 127.0.0.1:0
  admin_listen: 127.0.0.1:0
  max_request_bytes: 1KiB
  shutdown_grace: 5s
logging:
  level: debug
  format: json
storage:
  mode: redis
  redis:
    cache_url: ${REDIS_CACHE_URL}
    state_url: ${REDIS_STATE_URL}
  ledger:
    driver: postgres
    dsn: ${DATABASE_URL}
providers:
  - id: openai
    type: openai
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}
  - id: anthropic
    type: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_API_KEY}
models:
  - id: gpt-5-mini
    deployments:
      - provider: openai
        upstream_model: gpt-5-mini
        pricing: {input: "0.25", output: "2.00"}
tenants:
  - id: test
    keys:
      - id: test-key
        hash: sha256:0f7d5a33e1ad68ae7b2a4b139d4c5bf17737e0db942bfbecc5792bb3436d3e59
`

// fakeEnv is the environment config.Load sees.
func fakeEnv(key string) (string, bool) {
	v, ok := map[string]string{
		"OPENAI_API_KEY":    openAIKey,
		"ANTHROPIC_API_KEY": anthropicKey,
		"REDIS_CACHE_URL":   redisURL,
		"REDIS_STATE_URL":   stateURL,
		"DATABASE_URL":      dsn,
	}[key]
	return v, ok
}

// logBuffer is a concurrency-safe log sink that signals when the server
// logs its bound addresses.
type logBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	listening chan string // receives the "listening" line
}

func newLogBuffer() *logBuffer { return &logBuffer{listening: make(chan string, 1)} }

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// slog handlers write one whole line per call.
	if bytes.Contains(p, []byte(`"msg":"listening"`)) {
		select {
		case b.listening <- string(p):
		default:
		}
	}
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func loadSnapshot(t *testing.T, yaml string) *config.Snapshot {
	t.Helper()
	path := filepath.Join(t.TempDir(), "switchyard.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := config.Load(path, config.LoadOptions{LookupEnv: fakeEnv})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return snap
}

// restoreDefaultLogger undoes the slog.SetDefault that New performs.
func restoreDefaultLogger(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// running is an app started by startApp.
type running struct {
	public, admin string // base URLs
	cancel        context.CancelFunc
	runErr        chan error
}

// startApp runs the app built from snap until the test ends or stop is
// called, and returns it once it is listening.
func startApp(t *testing.T, snap *config.Snapshot, logs *logBuffer) *running {
	t.Helper()
	a, err := testNew(snap, logs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return startBuilt(t, a, logs)
}

// startBuilt runs a until the test ends or stop is called, and returns it
// once it is listening.
func startBuilt(t *testing.T, a *App, logs *logBuffer) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel: cancel, runErr: make(chan error, 1)}
	go func() { r.runErr <- a.Run(ctx, context.Background()) }()
	t.Cleanup(cancel)

	select {
	case line := <-logs.listening:
		var m struct {
			PublicAddr string `json:"public_addr"`
			AdminAddr  string `json:"admin_addr"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil || m.PublicAddr == "" || m.AdminAddr == "" {
			t.Fatalf("listening line %q: %v", line, err)
		}
		r.public, r.admin = "http://"+m.PublicAddr, "http://"+m.AdminAddr
	case err := <-r.runErr:
		t.Fatalf("Run returned before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not log its addresses")
	}
	return r
}

// stop shuts the app down and checks that Run returned nil.
func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.runErr:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after ctx was canceled")
	}
}

// TestSecretsNeverLogged runs the whole app at debug level and drives every
// path that could log a secret, then scans the captured log output.
func TestSecretsNeverLogged(t *testing.T) {
	restoreDefaultLogger(t)
	snap := loadSnapshot(t, secretConfig)
	logs := newLogBuffer()
	r := startApp(t, snap, logs)
	public := r.public

	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	requests := []struct {
		name       string
		method     string
		path       string
		header     http.Header
		body       string
		wantStatus int
	}{
		{name: "bearer token", method: http.MethodGet, path: "/healthz",
			header: http.Header{"Authorization": {"Bearer " + openAIKey}}, wantStatus: http.StatusOK},
		{name: "api key header", method: http.MethodGet, path: "/readyz",
			header: http.Header{"X-Api-Key": {anthropicKey}}, wantStatus: http.StatusOK},
		{name: "secret in the path", method: http.MethodGet, path: "/v1/keys/" + anthropicKey,
			wantStatus: http.StatusNotFound},
		{name: "openai key in the path", method: http.MethodGet, path: "/v1/keys/" + openAIKey,
			wantStatus: http.StatusNotFound},
		{name: "secret in the query", method: http.MethodGet, path: "/healthz?key=" + anthropicKey + "&dsn=" + dbPass,
			wantStatus: http.StatusOK},
		{name: "foreign key on the API", method: http.MethodGet, path: "/v1/models",
			header: http.Header{"Authorization": {"Bearer " + openAIKey}}, wantStatus: http.StatusUnauthorized},
		{name: "no key on the API", method: http.MethodPost, path: "/v1/chat/completions",
			body:       `{"model":"gpt-5-mini","messages":[{"role":"user","content":"` + anthropicKey + `"}]}`,
			wantStatus: http.StatusUnauthorized},
		{name: "oversized body", method: http.MethodPost, path: "/v1/chat/completions",
			header: http.Header{"Authorization": {"Bearer " + openAIKey}},
			body:   strings.Repeat(anthropicKey, 200), wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, rq := range requests {
		t.Run(rq.name, func(t *testing.T) {
			reqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var body io.Reader
			if rq.body != "" {
				body = strings.NewReader(rq.body)
			}
			req, err := http.NewRequestWithContext(reqCtx, rq.method, public+rq.path, body)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range rq.header {
				req.Header[k] = v
			}
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != rq.wantStatus {
				t.Errorf("%s %s = %d, want %d", rq.method, rq.path, resp.StatusCode, rq.wantStatus)
			}
		})
	}

	// The default logger installed by New is redacted too.
	slog.Default().Info("a careless log line", "detail", "connecting to "+dsn, "key", anthropicKey)

	r.stop(t)

	out := logs.String()
	for _, msg := range []string{`"msg":"config loaded"`, `"msg":"effective config"`, `"msg":"switchyard starting"`,
		`"msg":"request"`, `"msg":"a careless log line"`, `"msg":"shutdown complete"`} {
		if !strings.Contains(out, msg) {
			t.Errorf("logs have no %s line", msg)
		}
	}
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("logs contain the secret %q", s)
		}
	}
	if !strings.Contains(out, telemetry.Redacted) {
		t.Errorf("logs contain no %s marker", telemetry.Redacted)
	}
	// The path is logged, with the secret scrubbed out of it.
	if !strings.Contains(out, `"path":"/v1/keys/`+telemetry.Redacted+`"`) {
		t.Errorf("logs have no access-log line with the redacted path")
	}
	if t.Failed() {
		t.Logf("logs:\n%s", out)
	}
}

func TestNew(t *testing.T) {
	t.Run("nil snapshot", func(t *testing.T) {
		if _, err := testNew(nil, io.Discard); err == nil {
			t.Fatal("testNew(nil) succeeded")
		}
	})
	t.Run("nil log output", func(t *testing.T) {
		restoreDefaultLogger(t)
		if _, err := testNew(loadSnapshot(t, secretConfig), nil); err == nil {
			t.Fatal("New with a nil writer succeeded")
		}
	})
	t.Run("startup logs", func(t *testing.T) {
		restoreDefaultLogger(t)
		// Binding every interface for the admin listener is a warning.
		snap := loadSnapshot(t, strings.Replace(secretConfig, "admin_listen: 127.0.0.1:0", "admin_listen: :0", 1))
		if len(snap.Warnings()) == 0 {
			t.Fatal("the test config has no warnings")
		}
		var buf bytes.Buffer
		if _, err := testNew(snap, &buf); err != nil {
			t.Fatalf("New: %v", err)
		}
		lines := map[string]map[string]any{}
		for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("log line %q is not JSON: %v", line, err)
			}
			lines[m["msg"].(string)] = m
		}
		loaded := lines["config loaded"]
		if loaded == nil || loaded["config_version"] != snap.Version() ||
			loaded["providers"] != float64(2) || loaded["models"] != float64(1) || loaded["sources"] == nil ||
			loaded["api_keys"] != float64(1) {
			t.Errorf("config loaded line = %v", loaded)
		}
		warn := lines["config warning"]
		w := snap.Warnings()[0]
		if w.File == "" || w.Line == 0 {
			t.Fatalf("warning %v has no file and line", w)
		}
		if warn == nil || warn["level"] != "WARN" || warn["file"] != w.File || warn["line"] != float64(w.Line) ||
			warn["path"] != w.Path || warn["problem"] != w.Message {
			t.Errorf("config warning line = %v, want %v", warn, w)
		}
		if dump := lines["effective config"]; dump == nil || dump["level"] != "DEBUG" || dump["config"] == nil {
			t.Errorf("effective config line = %v", dump)
		}
	})
	t.Run("info level skips the config dump", func(t *testing.T) {
		restoreDefaultLogger(t)
		snap := loadSnapshot(t, strings.Replace(secretConfig, "level: debug", "level: info", 1))
		var buf bytes.Buffer
		if _, err := testNew(snap, &buf); err != nil {
			t.Fatalf("New: %v", err)
		}
		if strings.Contains(buf.String(), "effective config") {
			t.Errorf("the config was dumped at info level:\n%s", buf.String())
		}
	})
}

func TestRunReportsListenErrors(t *testing.T) {
	restoreDefaultLogger(t)
	snap := loadSnapshot(t, strings.Replace(secretConfig, "listen: 127.0.0.1:0", "listen: 256.0.0.1:0", 1))
	a, err := testNew(snap, io.Discard)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Run(context.Background(), context.Background()); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want a listen error", err)
	}
}

// TestAPIRequestsCounted checks the wiring around the API routes: requests
// that authentication rejects count in the API metrics, and an accepted
// request's one access-log line carries its tenant, key and completion
// fields.
func TestAPIRequestsCounted(t *testing.T) {
	restoreDefaultLogger(t)
	key, hash, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(secretConfig,
		"sha256:0f7d5a33e1ad68ae7b2a4b139d4c5bf17737e0db942bfbecc5792bb3436d3e59", hash, 1)
	yaml = strings.Replace(yaml, "level: debug", "level: info", 1)
	logs := newLogBuffer()
	r := startApp(t, loadSnapshot(t, yaml), logs)

	c := &http.Client{Timeout: 10 * time.Second}
	post := func(key, model string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, r.public+"/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for range 3 {
		if got := post("sy_wrong", "gpt-5-mini"); got != http.StatusUnauthorized {
			t.Fatalf("bad key: status %d, want 401", got)
		}
	}
	if got := post(key, "no-such-model"); got != http.StatusNotFound {
		t.Fatalf("unknown model: status %d, want 404", got)
	}

	resp, err := c.Get(r.admin + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{
		`switchyard_requests_total{endpoint="/v1/chat/completions",status_class="4xx"} 4`,
		`switchyard_request_duration_seconds_count{endpoint="/v1/chat/completions",stream="false"} 4`,
		`switchyard_circuit_state{deployment="gpt-5-mini@openai"} 0`,
		`switchyard_provider_in_flight{provider="openai"} 0`,
	} {
		if !strings.Contains(string(metrics), want) {
			t.Errorf("/metrics has no %s", want)
		}
	}

	// The provider report is served on the admin listener only, without
	// secrets.
	resp, err = c.Get(r.admin + "/admin/v1/providers")
	if err != nil {
		t.Fatal(err)
	}
	report, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var rep struct {
		Providers []struct {
			ID          string
			Deployments []struct {
				ID      string
				Breaker struct{ State string }
			}
		}
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(report, &rep) != nil || len(rep.Providers) != 2 ||
		rep.Providers[1].ID != "openai" || len(rep.Providers[1].Deployments) != 1 ||
		rep.Providers[1].Deployments[0].Breaker.State != "closed" {
		t.Errorf("/admin/v1/providers: status %d, body %s", resp.StatusCode, report)
	}
	for _, secret := range secrets {
		if strings.Contains(string(report), secret) {
			t.Errorf("/admin/v1/providers contains a secret: %s", report)
		}
	}
	resp, err = c.Get(r.public + "/admin/v1/providers")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("public /admin/v1/providers: status %d, want 404", resp.StatusCode)
	}
	r.stop(t)

	var lines []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if m["msg"] == "completion" {
			t.Errorf("a separate completion line was logged: %s", line)
		}
		if m["msg"] == "request" && m["path"] == "/v1/chat/completions" {
			lines = append(lines, m)
		}
	}
	if len(lines) != 4 {
		t.Fatalf("got %d request lines, want 4", len(lines))
	}
	if l := lines[0]; l["status"] != float64(401) || l["tenant"] != nil {
		t.Errorf("rejected request line = %v", l)
	}
	l := lines[3]
	for k, want := range map[string]any{
		"status": float64(404), "tenant": "test", "key_id": "test-key",
		"requested_model": "no-such-model", "error_code": "model_not_found",
	} {
		if l[k] != want {
			t.Errorf("request line %s = %v, want %v (line %v)", k, l[k], want, l)
		}
	}
	if strings.Contains(logs.String(), key) {
		t.Error("the logs contain the client key")
	}
}

// testNew is New with a memory ledger and memory budgets whatever the
// config says, so that the tests need no Postgres or Redis.
func testNew(snap *config.Snapshot, logOutput io.Writer) (*App, error) {
	return newApp(snap, logOutput, func(*config.Config, prometheus.Registerer, *slog.Logger) (storage, error) {
		return storage{ledger: usage.NewMemoryLedger(), budgets: budget.NewMemoryStore()}, nil
	})
}
