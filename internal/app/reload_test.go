package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"switchyard/internal/api/openaiapi"
	"switchyard/internal/auth"
	"switchyard/internal/config"
	"switchyard/internal/gateway"
	"switchyard/test/fakeupstream"
)

// reloadYAML renders a config for the reload tests: provider up at
// baseURL, with api_key ${OPENAI_API_KEY}, serving the given models, and
// tenant test with one key per hash. extraServer is appended to the
// server section.
func reloadYAML(baseURL string, models []string, hashes []string, extraServer string) string {
	var b strings.Builder
	b.WriteString("version: 1\nserver:\n  listen: 127.0.0.1:0\n  admin_listen: 127.0.0.1:0\n  shutdown_grace: 5s\n")
	b.WriteString(extraServer)
	b.WriteString("logging: {level: info, format: json}\nproviders:\n")
	b.WriteString("  - {id: up, type: openai, base_url: \"" + baseURL + "\", api_key: \"${OPENAI_API_KEY}\"}\nmodels:\n")
	for _, m := range models {
		b.WriteString("  - id: " + m + "\n    capabilities: [streaming, stream_usage]\n    deployments:\n")
		b.WriteString("      - {provider: up, upstream_model: " + m + ", pricing: {input: \"1\", output: \"1\"}}\n")
	}
	b.WriteString("tenants:\n  - id: test\n    keys:\n")
	for i, h := range hashes {
		b.WriteString("      - {id: key-" + string(rune('a'+i)) + ", hash: " + h + "}\n")
	}
	return b.String()
}

// reloadEnv is a running app whose config file a test rewrites.
type reloadEnv struct {
	t    *testing.T
	path string
	app  *App
	run  *running
	logs *logBuffer
	up   *fakeupstream.Server
	c    *http.Client
}

func newReloadEnv(t *testing.T, yaml func(baseURL string) string) *reloadEnv {
	t.Helper()
	restoreDefaultLogger(t)
	up := fakeupstream.New(fakeupstream.Options{})
	t.Cleanup(up.Close)
	e := &reloadEnv{t: t, path: filepath.Join(t.TempDir(), "switchyard.yaml"), up: up, logs: newLogBuffer(),
		c: &http.Client{Timeout: 10 * time.Second}}
	e.write(yaml(up.BaseURL()))
	snap, err := config.Load(e.path, config.LoadOptions{LookupEnv: fakeEnv})
	if err != nil {
		t.Fatal(err)
	}
	a, err := testNew(snap, e.logs)
	if err != nil {
		t.Fatal(err)
	}
	a.reload.load = func() (*config.Snapshot, error) {
		return config.Load(e.path, config.LoadOptions{LookupEnv: fakeEnv})
	}
	e.app, e.run = a, startBuilt(t, a, e.logs)
	return e
}

func (e *reloadEnv) write(yaml string) {
	e.t.Helper()
	if err := os.WriteFile(e.path, []byte(yaml), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// do sends a request and returns its status and body.
func (e *reloadEnv) do(method, url, bearer, body string) (int, string) {
	e.t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := e.c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func (e *reloadEnv) chat(key, model string, stream bool) (int, string) {
	e.t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"stream":` + map[bool]string{true: "true", false: "false"}[stream] + `}`
	return e.do(http.MethodPost, e.run.public+"/v1/chat/completions", key, body)
}

// reload calls POST /admin/v1/reload and decodes the answer.
func (e *reloadEnv) reload(token string) (int, reloadResponse) {
	e.t.Helper()
	status, body := e.do(http.MethodPost, e.run.admin+"/admin/v1/reload", token, "")
	var r reloadResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		e.t.Fatalf("reload answer %q: %v", body, err)
	}
	return status, r
}

// audits returns the audit lines logged so far.
func (e *reloadEnv) audits() []map[string]any {
	var out []map[string]any
	for line := range strings.SplitSeq(e.logs.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "audit" {
			out = append(out, m)
		}
	}
	return out
}

func genKey(t *testing.T) (key, hash string) {
	t.Helper()
	key, hash, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key, hash
}

// TestReloadInFlightStream: a stream in flight when a reload removes its
// model completes normally; requests after the reload see the new config.
func TestReloadInFlightStream(t *testing.T) {
	key, hash := genKey(t)
	e := newReloadEnv(t, func(u string) string { return reloadYAML(u, []string{"m1", "m2"}, []string{hash}, "") })
	before := e.app.gw.ConfigVersion()

	release := make(chan struct{})
	e.up.Script(fakeupstream.Behavior{Hold: release})
	type result struct {
		status int
		body   string
	}
	done := make(chan result, 1)
	go func() {
		s, b := e.chat(key, "m2", true)
		done <- result{s, b}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.up.WaitReceived(ctx, 1); err != nil {
		t.Fatal(err)
	}

	e.write(reloadYAML(e.up.BaseURL(), []string{"m1"}, []string{hash}, ""))
	status, r := e.reload("")
	if status != http.StatusOK || r.Result != "ok" || r.Fingerprint == before || r.Fingerprint == "" || r.RestartRequired == nil {
		t.Fatalf("reload: %d %+v", status, r)
	}
	e.up.Script(fakeupstream.Behavior{})
	if s, b := e.chat(key, "m2", false); s != http.StatusNotFound || !strings.Contains(b, "model_not_found") {
		t.Errorf("m2 after the reload: %d %s; want 404 model_not_found", s, b)
	}
	if s, _ := e.chat(key, "m1", false); s != http.StatusOK {
		t.Errorf("m1 after the reload: %d", s)
	}

	close(release)
	res := <-done
	if res.status != http.StatusOK || !strings.Contains(res.body, "data: [DONE]") {
		t.Errorf("the stream in flight: %d %q; want 200 ending in [DONE]", res.status, res.body)
	}
	if _, b := e.do(http.MethodGet, e.run.public+"/readyz", "", ""); !strings.Contains(b, r.Fingerprint) {
		t.Errorf("/readyz after the reload: %s; want config_version %s", b, r.Fingerprint)
	}
	_, metrics := e.do(http.MethodGet, e.run.admin+"/metrics", "", "")
	for _, want := range []string{`switchyard_config_reloads_total{result="ok"} 1`, `switchyard_config_reloads_total{result="error"} 0`,
		"switchyard_config_last_reload_success_timestamp_seconds "} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	a := e.audits()
	if len(a) != 1 || a[0]["action"] != "config.reload" || a[0]["result"] != "ok" || a[0]["fingerprint"] != r.Fingerprint ||
		a[0]["previous_fingerprint"] != before || a[0]["remote_ip"] != "127.0.0.1" || a[0]["trigger"] != "admin" {
		t.Errorf("audit lines %v", a)
	}
}

// TestReloadRejected: an invalid config is rejected with its problems,
// the old config keeps serving, and nothing leaks a secret.
func TestReloadRejected(t *testing.T) {
	key, hash := genKey(t)
	e := newReloadEnv(t, func(u string) string { return reloadYAML(u, []string{"m1"}, []string{hash}, "") })
	before := e.app.gw.ConfigVersion()
	tests := []struct {
		name, yaml, wantErr string
	}{
		{"unknown field", reloadYAML(e.up.BaseURL(), []string{"m1"}, []string{hash}, "  bogus: 1\n"), "server.bogus"},
		{"missing env var", strings.Replace(reloadYAML(e.up.BaseURL(), []string{"m1"}, []string{hash}, ""), "OPENAI_API_KEY", "NOT_SET_ANYWHERE", 1), "NOT_SET_ANYWHERE"},
		{"bad reference", strings.Replace(reloadYAML(e.up.BaseURL(), []string{"m1"}, []string{hash}, ""), "provider: up", "provider: nope", 1), "nope"},
		{"file removed", "", "no such file"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.yaml == "" {
				if err := os.Remove(e.path); err != nil {
					t.Fatal(err)
				}
			} else {
				e.write(tt.yaml)
			}
			status, r := e.reload("")
			if status != http.StatusBadRequest || r.Result != "error" || !strings.Contains(r.Error, tt.wantErr) {
				t.Errorf("reload: %d %+v; want 400 with an error naming %q", status, r, tt.wantErr)
			}
			if s, _ := e.chat(key, "m1", false); s != http.StatusOK {
				t.Errorf("after the rejected reload: %d, want the old config serving", s)
			}
			if v := e.app.gw.ConfigVersion(); v != before {
				t.Errorf("config version %s, want %s", v, before)
			}
			if a := e.audits(); len(a) != i+1 || a[i]["result"] != "error" || a[i]["fingerprint"] != before {
				t.Errorf("audit lines %v", a)
			}
		})
	}
	_, metrics := e.do(http.MethodGet, e.run.admin+"/metrics", "", "")
	if !strings.Contains(metrics, `switchyard_config_reloads_total{result="error"} 4`) {
		t.Error(`metrics lack switchyard_config_reloads_total{result="error"} 4`)
	}
	if strings.Contains(e.logs.String(), openAIKey) || strings.Contains(e.logs.String(), key) {
		t.Error("the logs contain a secret")
	}
}

// TestReloadKeysAndRestartRequired: a reload swaps the API keys at once,
// reports the settings it did not apply, and an identical config is a
// no-op.
func TestReloadKeysAndRestartRequired(t *testing.T) {
	oldKey, oldHash := genKey(t)
	newKey, newHash := genKey(t)
	e := newReloadEnv(t, func(u string) string { return reloadYAML(u, []string{"m1"}, []string{oldHash}, "") })

	yaml := reloadYAML(e.up.BaseURL(), []string{"m1"}, []string{newHash}, "  idle_timeout: 33s\n  tls: {min_version: \"1.3\"}\n")
	yaml = strings.Replace(yaml, "level: info", "level: debug", 1) + "routing:\n  breaker: {consecutive_failures: 9}\n  request_timeout: 90s\n"
	e.write(yaml)
	status, r := e.reload("")
	want := []string{"logging.level", "routing.breaker.consecutive_failures", "server.idle_timeout", "server.tls.min_version"}
	if status != http.StatusOK || strings.Join(r.RestartRequired, ",") != strings.Join(want, ",") {
		t.Errorf("reload: %d %+v; want restart_required %v", status, r, want)
	}
	if s, _ := e.chat(oldKey, "m1", false); s != http.StatusUnauthorized {
		t.Errorf("the removed key after the reload: %d, want 401", s)
	}
	if s, _ := e.chat(newKey, "m1", false); s != http.StatusOK {
		t.Errorf("the added key after the reload: %d, want 200", s)
	}
	if strings.Contains(e.logs.String(), `"level":"DEBUG"`) {
		t.Error("logging.level was applied by a reload")
	}

	status, again := e.reload("")
	if status != http.StatusOK || !again.Unchanged || again.Fingerprint != r.Fingerprint || len(again.RestartRequired) != len(want) {
		t.Errorf("identical reload: %d %+v; want unchanged, still reporting %v", status, again, want)
	}
}

// TestReloadAdminToken: the reload endpoint needs the admin token; a
// reload rotates it.
func TestReloadAdminToken(t *testing.T) {
	_, hash := genKey(t)
	const tok1, tok2 = "adm-first-5d1c8a2b9e-0123456789abcd", "adm-second-7f3e6b0a4c-0123456789abc"
	e := newReloadEnv(t, func(u string) string {
		return reloadYAML(u, []string{"m1"}, []string{hash}, "  admin_token: "+tok1+"\n")
	})
	if status, body := e.do(http.MethodPost, e.run.admin+"/admin/v1/reload", "", ""); status != http.StatusUnauthorized || !strings.Contains(body, "invalid_admin_token") {
		t.Errorf("reload without the token: %d %s", status, body)
	}
	if status, _ := e.do(http.MethodGet, e.run.admin+"/metrics", "", ""); status != http.StatusOK {
		t.Errorf("/metrics without the token: %d", status)
	}
	if status, _ := e.do(http.MethodGet, e.run.admin+"/admin/v1/providers", "", ""); status != http.StatusUnauthorized {
		t.Errorf("/admin/v1/providers without the token: %d", status)
	}
	e.write(reloadYAML(e.up.BaseURL(), []string{"m1"}, []string{hash}, "  admin_token: "+tok2+"\n"))
	if status, r := e.reload(tok1); status != http.StatusOK {
		t.Fatalf("reload with the token: %d %+v", status, r)
	}
	if status, _ := e.do(http.MethodGet, e.run.admin+"/admin/v1/providers", tok1, ""); status != http.StatusUnauthorized {
		t.Errorf("the old token after rotation: %d, want 401", status)
	}
	if status, _ := e.do(http.MethodGet, e.run.admin+"/admin/v1/providers", tok2, ""); status != http.StatusOK {
		t.Errorf("the new token after rotation: %d, want 200", status)
	}
	if strings.Contains(e.logs.String(), tok1) || strings.Contains(e.logs.String(), tok2) {
		t.Error("the logs contain an admin token")
	}
}

// TestReloadSignalAndConcurrency: ReloadOnSignal audits with trigger
// signal, and concurrent reloads are serialized.
func TestReloadSignalAndConcurrency(t *testing.T) {
	_, hash := genKey(t)
	e := newReloadEnv(t, func(u string) string { return reloadYAML(u, []string{"m1"}, []string{hash}, "") })
	e.write(reloadYAML(e.up.BaseURL(), []string{"m1", "m2"}, []string{hash}, ""))
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(e.app.ReloadOnSignal)
	}
	wg.Wait()
	a := e.audits()
	if len(a) != 4 {
		t.Fatalf("%d audit lines, want 4", len(a))
	}
	for _, l := range a {
		if l["result"] != "ok" || l["trigger"] != "signal" || l["remote_ip"] != "" {
			t.Errorf("audit line %v", l)
		}
	}
	if s, _ := e.do(http.MethodGet, e.run.admin+"/metrics", "", ""); s != http.StatusOK {
		t.Fatal(s)
	}
	_, metrics := e.do(http.MethodGet, e.run.admin+"/metrics", "", "")
	if !strings.Contains(metrics, `switchyard_config_reloads_total{result="ok"} 4`) {
		t.Error(`metrics lack switchyard_config_reloads_total{result="ok"} 4`)
	}
}

// TestReloadKeepsExposedAdminGuarded: while the admin listener is bound to
// every interface, a reload cannot remove the admin token, even with a
// loopback admin_listen in the new file.
func TestReloadKeepsExposedAdminGuarded(t *testing.T) {
	restoreDefaultLogger(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "switchyard.yaml")
	base := "version: 1\nserver: {listen: '127.0.0.1:0', admin_listen: 'ADMIN', TOKEN}\nlogging: {format: json}\n"
	write := func(admin, token string) {
		yaml := strings.NewReplacer("ADMIN", admin, "TOKEN", token).Replace(base)
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(":0", "admin_token: adm-guard-3e9d1b7a5c-0123456789abcdef")
	snap, err := config.Load(path, config.LoadOptions{LookupEnv: fakeEnv})
	if err != nil {
		t.Fatal(err)
	}
	a, err := testNew(snap, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	a.reload.load = func() (*config.Snapshot, error) { return config.Load(path, config.LoadOptions{LookupEnv: fakeEnv}) }
	write("127.0.0.1:0", "shutdown_grace: 5s")
	if _, err := a.Reload(); err == nil || !strings.Contains(err.Error(), "server.admin_token") {
		t.Errorf("Reload removing the token of an exposed admin listener: %v; want an error", err)
	}
	if _, err := (&App{reload: reloader{now: time.Now, total: a.reload.total, lastSuccess: a.reload.lastSuccess}}).Reload(); err == nil {
		t.Error("Reload without a Loader succeeded")
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9090", true}, {"127.9.9.9:1", true}, {"[::1]:9090", true}, {"localhost:9090", true},
		{":9090", false}, {"0.0.0.0:9090", false}, {"[::]:9090", false}, {"10.0.0.1:9090", false}, {"admin:9090", false}, {"bad", false},
	}
	for _, tt := range tests {
		if got := isLoopbackAddr(tt.addr); got != tt.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}

// TestReloadRateLimits: a reload changes a tenant's request rate (lifting
// it, then setting it again) and rate_limits.auth_failures_per_minute for
// the requests that follow, without a restart, and reports neither as
// restart_required. (That a raised rate applies within one new interval
// is the limiters' contract test.)
func TestReloadRateLimits(t *testing.T) {
	key, hash := genKey(t)
	_, unknownHash := genKey(t)
	unknown, _ := genKey(t)
	withLimits := func(u, tenantLimit, gateway string) string {
		y := reloadYAML(u, []string{"m1"}, []string{hash, unknownHash}, "")
		y = strings.Replace(y, "  - id: test\n", "  - id: test\n    rate_limit: "+tenantLimit+"\n", 1)
		return y + gateway
	}
	e := newReloadEnv(t, func(u string) string { return withLimits(u, "{requests_per_minute: 1}", "") })

	// 1 a minute with the default burst of 1: two at once, then 429.
	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests} {
		if status, body := e.chat(key, "m1", false); status != want {
			t.Fatalf("request %d before the reload: %d %s, want %d", i, status, body, want)
		}
	}

	reload := func(tenantLimit, gateway string) {
		t.Helper()
		e.write(withLimits(e.up.BaseURL(), tenantLimit, gateway))
		status, r := e.reload("")
		if status != http.StatusOK {
			t.Fatalf("reload: %d %+v", status, r)
		}
		for _, p := range r.RestartRequired {
			if strings.Contains(p, "rate_limit") {
				t.Errorf("restart_required lists %s", p)
			}
		}
	}
	reload("{requests_per_minute: 0}", "")
	for i := range 5 {
		if status, body := e.chat(key, "m1", false); status != http.StatusOK {
			t.Fatalf("request %d after lifting the rate: %d %s", i, status, body)
		}
	}
	reload("{requests_per_minute: 1}", "rate_limits: {auth_failures_per_minute: 2}\n")
	if status, _ := e.chat(key, "m1", false); status != http.StatusTooManyRequests {
		t.Fatalf("after setting the rate again: %d, want 429", status)
	}

	// Two failed authentications from this address, then even a good
	// key is refused before it is checked.
	for i := range 2 {
		if status, _ := e.chat(unknown, "m1", false); status != http.StatusUnauthorized {
			t.Fatalf("failed authentication %d: %d, want 401", i, status)
		}
	}
	if status, body := e.chat(key, "m1", false); status != http.StatusTooManyRequests || !strings.Contains(body, "rate_limit_exceeded") {
		t.Errorf("after 2 failures under the reloaded limit: %d %s, want 429 rate_limit_exceeded", status, body)
	}
}

// TestReloadRejectedAfterStage: a reload rejected by a check that runs on
// the staged snapshot (here the semantic cache's embedding deployment;
// the shadow judge's is the same path) changes nothing the configuration in force uses: the
// health tracker keeps its providers' max_concurrency and its deployments,
// and routing still treats the provider as unlimited.
func TestReloadRejectedAfterStage(t *testing.T) {
	key, hash := genKey(t)
	e := newReloadEnv(t, func(u string) string { return reloadYAML(u, []string{"m1"}, []string{hash}, "") })
	before := e.app.gw.ConfigVersion()
	providers := func() string {
		t.Helper()
		status, body := e.do(http.MethodGet, e.run.admin+"/admin/v1/providers", "", "")
		var r struct {
			Providers json.RawMessage `json:"providers"`
		}
		if status != http.StatusOK || json.Unmarshal([]byte(body), &r) != nil {
			t.Fatalf("/admin/v1/providers: %d %s", status, body)
		}
		return string(r.Providers)
	}

	// The new file sets max_concurrency 1 on up, adds a model emb@up and
	// a latency hint on m1, and fails a staged check.
	staged := func(extra string) string {
		y := reloadYAML(e.up.BaseURL(), []string{"m1"}, []string{hash}, "")
		y = strings.Replace(y, `api_key: "${OPENAI_API_KEY}"}`, `api_key: "${OPENAI_API_KEY}", max_concurrency: 1}`, 1)
		y = strings.Replace(y, "upstream_model: m1,", "upstream_model: m1, latency_hint: {ttft_ms: 4321},", 1)
		y = strings.Replace(y, "tenants:\n", "  - id: emb\n    type: embedding\n    dimensions: 8\n    deployments:\n"+
			"      - {provider: up, upstream_model: emb, pricing: {input: \"1\"}}\ntenants:\n", 1)
		return y + extra
	}
	tests := []struct {
		name, extra, wantErr string
	}{
		{"embedding deployment not permitted",
			"policies:\n  - id: default\n    allow: [\"m1@up\"]\n    cache:\n      semantic: {enabled: true}\nsemantic_cache:\n  embedding_model: emb\n",
			"semantic_cache.embedding_model"},
		{"embedding deployment denied",
			"policies:\n  - id: default\n    deny: [\"emb@up\"]\n    cache:\n      semantic: {enabled: true}\nsemantic_cache:\n  embedding_model: emb\n",
			"semantic_cache.embedding_model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := providers()
			e.write(staged(tt.extra))
			status, r := e.reload("")
			if status != http.StatusBadRequest || !strings.Contains(r.Error, tt.wantErr) {
				t.Fatalf("reload: %d %+v; want 400 naming %s", status, r, tt.wantErr)
			}
			if v := e.app.gw.ConfigVersion(); v != before {
				t.Errorf("config version %s, want %s", v, before)
			}
			if got := providers(); got != want {
				t.Errorf("providers after the rejected reload:\n%s\nwant\n%s", got, want)
			}
			// With max_concurrency 1 applied, a second request while one
			// is in flight would be refused 503.
			release := make(chan struct{})
			e.up.Sequence(fakeupstream.Behavior{Hold: release})
			done := make(chan int, 1)
			go func() {
				s, _ := e.chat(key, "m1", false)
				done <- s
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := e.up.WaitReceived(ctx, 1); err != nil {
				t.Fatal(err)
			}
			if s, b := e.chat(key, "m1", false); s != http.StatusOK {
				t.Errorf("a second request while one is in flight: %d %s, want 200", s, b)
			}
			close(release)
			if s := <-done; s != http.StatusOK {
				t.Errorf("the held request: %d", s)
			}
			e.up.Reset()
		})
	}
}

// TestReloadBetweenAuthAndGateway: a reload committed after a request
// authenticated against the old keys, and before it reaches the gateway
// (the hook stands in for the rate limiter, where a request can wait), is
// served by the new snapshot only if the new configuration still has its
// key and tenant; otherwise it gets 401 invalid_api_key and no upstream
// call.
func TestReloadBetweenAuthAndGateway(t *testing.T) {
	keep, keepHash := genKey(t)
	revoke, revokeHash := genKey(t)
	original := func(u string) string { return reloadYAML(u, []string{"m1"}, []string{keepHash, revokeHash}, "") }
	e := newReloadEnv(t, original)
	tests := []struct {
		name, key string
		next      string
		want      int
	}{
		{"key revoked", revoke, reloadYAML(e.up.BaseURL(), []string{"m1"}, []string{keepHash}, ""), http.StatusUnauthorized},
		{"tenant removed", keep, strings.Replace(original(e.up.BaseURL()), "  - id: test\n", "  - id: other\n", 1), http.StatusUnauthorized},
		{"key kept", keep, reloadYAML(e.up.BaseURL(), []string{"m1"}, []string{keepHash}, ""), http.StatusOK},
	}
	var chat http.Handler
	for _, r := range openaiapi.Routes(e.app.gw, openaiapi.Options{}) {
		if r.Endpoint == gateway.EndpointChatCompletions {
			chat = r.Handler
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.write(original(e.up.BaseURL()))
			if _, err := e.app.Reload(); err != nil {
				t.Fatal(err)
			}
			upstream := len(e.up.Requests())
			hook := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				e.write(tt.next)
				if _, err := e.app.Reload(); err != nil {
					t.Errorf("reload: %v", err)
				}
				chat.ServeHTTP(w, r)
			})
			h := auth.Middleware(e.app.reload.keys, auth.Options{})(hook)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer "+tt.key)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status %d %s, want %d", rec.Code, rec.Body, tt.want)
			}
			calls := len(e.up.Requests()) - upstream
			if tt.want == http.StatusUnauthorized && (calls != 0 || !strings.Contains(rec.Body.String(), `"code":"invalid_api_key"`)) {
				t.Errorf("%d upstream calls, body %s; want none and invalid_api_key", calls, rec.Body)
			}
		})
	}
}

// TestAdminAuthFailures: the admin API is behind the failed-authentication
// guard: good tokens never use up rate_limits.auth_failures_per_minute,
// wrong ones do, and once the address is over it every admin request,
// even with the right token, gets 429 before the token is checked, with
// no admin authentication line logged for it. /metrics and the probes
// stay open.
func TestAdminAuthFailures(t *testing.T) {
	_, hash := genKey(t)
	const tok = "adm-guarded-1f2e3d4c5b6a-0123456789"
	e := newReloadEnv(t, func(u string) string {
		return reloadYAML(u, []string{"m1"}, []string{hash}, "  admin_token: "+tok+"\n") + "rate_limits: {auth_failures_per_minute: 3}\n"
	})
	providers := e.run.admin + "/admin/v1/providers"
	steps := []struct {
		name, token string
		want        int
	}{
		{"good token", tok, http.StatusOK},
		{"good token again", tok, http.StatusOK},
		{"good token a third time", tok, http.StatusOK},
		{"good token a fourth time", tok, http.StatusOK},
		{"wrong token 1", "wrong", http.StatusUnauthorized},
		{"no token", "", http.StatusUnauthorized},
		{"wrong token 3", "wrong", http.StatusUnauthorized},
		{"wrong token over the limit", "wrong", http.StatusTooManyRequests},
		{"good token over the limit", tok, http.StatusTooManyRequests},
	}
	for _, s := range steps {
		if status, body := e.do(http.MethodGet, providers, s.token, ""); status != s.want {
			t.Fatalf("%s: %d %s, want %d", s.name, status, body, s.want)
		}
	}
	for _, path := range []string{"/metrics", "/healthz", "/readyz"} {
		if status, _ := e.do(http.MethodGet, e.run.admin+path, "", ""); status != http.StatusOK {
			t.Errorf("%s while limited: %d, want 200", path, status)
		}
	}
	if n := strings.Count(e.logs.String(), "admin authentication failed"); n != 3 {
		t.Errorf("%d admin authentication lines, want 3 (none for the refused requests)", n)
	}
}
