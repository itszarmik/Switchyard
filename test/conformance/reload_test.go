package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeupstream"
)

// reloadAdminToken is the admin token of the reload gateway.
const reloadAdminToken = "adm-conformance-reload-9b2f4e7a1c"

// reloadConfig renders the reload gateway's config: the fake as provider
// up, serving models, and tenant t1 with the key of hash. extra is
// appended verbatim.
func reloadConfig(baseURL, hash string, models []string, extra string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 5s, admin_token: %q}
logging: {level: info, format: json}
providers:
  - {id: up, type: openai, base_url: %q, api_key: %q}
models:
`, reloadAdminToken, baseURL, providerKey)
	for _, m := range models {
		fmt.Fprintf(&b, "  - {id: %s, capabilities: [streaming, stream_usage], deployments: [{provider: up, upstream_model: %s, pricing: {input: \"1\", output: \"1\"}}]}\n", m, upstreamModel)
	}
	fmt.Fprintf(&b, "tenants:\n  - {id: t1, keys: [{id: k1, hash: %q}]}\n%s", hash, extra)
	return []byte(b.String())
}

// TestReloadThroughBinary drives hot reload through the gateway binary
// (PLAN P7: an invalid reload is rejected and the old config keeps
// serving; a valid reload takes effect without dropping in-flight
// streams), by POST /admin/v1/reload and by SIGHUP.
func TestReloadThroughBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the gateway; skipped with -short")
	}
	upstream := newUpstream(t)
	key, hash, err := harness.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	bin, err := harness.Build(ctx, dir, raceEnabled)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "switchyard.yaml")
	write := func(b []byte) {
		t.Helper()
		if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(reloadConfig(upstream.BaseURL(), hash, []string{"m1", "m2"}, ""))
	g, err := harness.Start(ctx, bin, cfgPath, harness.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := g.Stop(15 * time.Second); err != nil {
			t.Errorf("stopping the gateway: %v", err)
		}
		if strings.Contains(g.Stderr(), "WARNING: DATA RACE") {
			t.Errorf("the gateway reported a data race:\n%s", g.Stderr())
		}
		if t.Failed() {
			t.Logf("gateway logs:\n%s", strings.Join(g.Logs(), "\n"))
		}
	})

	c := &http.Client{Timeout: callTimeout}
	do := func(method, url, bearer, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(b)
	}
	chat := func(model string, stream bool) (int, string) {
		t.Helper()
		return do(http.MethodPost, g.PublicURL+"/v1/chat/completions", key,
			fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":%v}`, model, stream))
	}
	// waitLog waits for the n-th log line with msg, which the gateway
	// writes once a signaled reload is done.
	waitLog := func(msg string, n int) map[string]any {
		t.Helper()
		deadline := time.Now().Add(callTimeout)
		for {
			var found []map[string]any
			for _, l := range g.Logs() {
				var m map[string]any
				if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == msg {
					found = append(found, m)
				}
			}
			if len(found) >= n {
				return found[n-1]
			}
			if time.Now().After(deadline) {
				t.Fatalf("no %q log line #%d", msg, n)
			}
			select {
			case <-g.Exited():
				t.Fatalf("the gateway exited while waiting for %q", msg)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}

	t.Run("admin token", func(t *testing.T) {
		if s, b := do(http.MethodPost, g.AdminURL+"/admin/v1/reload", "", ""); s != http.StatusUnauthorized || !strings.Contains(b, "invalid_admin_token") {
			t.Errorf("reload without the token: %d %s", s, b)
		}
		if s, _ := do(http.MethodGet, g.AdminURL+"/metrics", "", ""); s != http.StatusOK {
			t.Errorf("/metrics without the token: %d", s)
		}
	})

	t.Run("a valid reload keeps the stream in flight", func(t *testing.T) {
		release := make(chan struct{})
		upstream.Script(fakeupstream.Behavior{Hold: release})
		type result struct {
			status int
			body   string
		}
		done := make(chan result, 1)
		go func() {
			s, b := chat("m2", true)
			done <- result{s, b}
		}()
		wctx, wcancel := context.WithTimeout(context.Background(), callTimeout)
		defer wcancel()
		if err := upstream.WaitReceived(wctx, 1); err != nil {
			t.Fatal(err)
		}

		write(reloadConfig(upstream.BaseURL(), hash, []string{"m1"}, ""))
		s, b := do(http.MethodPost, g.AdminURL+"/admin/v1/reload", reloadAdminToken, "")
		var r struct {
			Result          string
			Fingerprint     string
			RestartRequired []string `json:"restart_required"`
		}
		if err := json.Unmarshal([]byte(b), &r); err != nil || s != http.StatusOK || r.Result != "ok" || r.Fingerprint == "" || r.RestartRequired == nil {
			t.Fatalf("reload: %d %s", s, b)
		}
		upstream.Script(fakeupstream.Behavior{})
		if s, b := chat("m2", false); s != http.StatusNotFound {
			t.Errorf("the removed model after the reload: %d %s; want 404", s, b)
		}
		close(release)
		res := <-done
		if res.status != http.StatusOK || !strings.HasSuffix(strings.TrimSpace(res.body), "data: [DONE]") {
			t.Errorf("the stream in flight: %d %q; want 200 ending in [DONE]", res.status, res.body)
		}
		if _, b := do(http.MethodGet, g.PublicURL+"/readyz", "", ""); !strings.Contains(b, r.Fingerprint) {
			t.Errorf("/readyz: %s; want config_version %s", b, r.Fingerprint)
		}
	})

	t.Run("SIGHUP with an invalid config keeps the old one", func(t *testing.T) {
		write(append(reloadConfig(upstream.BaseURL(), hash, []string{"m1"}, ""), "bogus_section: 1\n"...))
		if err := g.Signal(syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		l := waitLog("config reload failed", 1)
		if e, _ := l["error"].(string); !strings.Contains(e, "bogus_section") {
			t.Errorf("reload failure line %v: want the unknown field named", l)
		}
		if s, b := chat("m1", false); s != http.StatusOK {
			t.Errorf("after the rejected reload: %d %s", s, b)
		}
	})

	t.Run("SIGHUP with a valid config applies it", func(t *testing.T) {
		write(reloadConfig(upstream.BaseURL(), hash, []string{"m1", "m2"}, ""))
		if err := g.Signal(syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		waitLog("config reloaded", 2)
		if s, b := chat("m2", false); s != http.StatusOK {
			t.Errorf("the restored model after the reload: %d %s", s, b)
		}
		select {
		case <-g.Exited():
			t.Fatal("SIGHUP stopped the gateway")
		default:
		}
	})

	t.Run("metrics, audit and secrets", func(t *testing.T) {
		_, m := do(http.MethodGet, g.AdminURL+"/metrics", "", "")
		for _, want := range []string{`switchyard_config_reloads_total{result="ok"} 2`, `switchyard_config_reloads_total{result="error"} 1`} {
			if !strings.Contains(m, want) {
				t.Errorf("metrics lack %s", want)
			}
		}
		var audits []string
		for _, l := range g.Logs() {
			var m map[string]any
			if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == "audit" {
				audits = append(audits, fmt.Sprintf("%v/%v/%v", m["action"], m["result"], m["trigger"]))
			}
		}
		if want := "config.reload/ok/admin config.reload/error/signal config.reload/ok/signal"; strings.Join(audits, " ") != want {
			t.Errorf("audit lines %v, want %s", audits, want)
		}
		all := strings.Join(g.Logs(), "\n") + g.Stderr()
		for _, secret := range []string{providerKey, key, reloadAdminToken} {
			if strings.Contains(all, secret) {
				t.Errorf("the gateway's output contains a secret (%.6s…)", secret)
			}
		}
	})
}
