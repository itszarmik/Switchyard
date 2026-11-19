package conformance

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeupstream"
)

// The secret values of TestNoSecretsLeak. Each is distinctive, so a match
// can only be a leak, and none has a well-known credential prefix, so the
// exact-value scrubbing is tested rather than the pattern matching (the
// gateway key, which must look like sy_…, is the exception). The admin
// token holds characters that URL and base64 encodings change.
const (
	leakProviderKey      = "provkey-Qx7Lr2Vt9Wm4Hs6Bn3Jd"
	leakWrongProviderKey = "wrongprov-Kd8Fz1Pq5Xc7Ty2Gu4"
	leakStatePassword    = "statepw-Hj6Rk3Mw9Zp1Vb7Nc5"
	leakCachePassword    = "cachepw-Lt4Yx8Qe2Sd6Fg1Ka9"
	leakDSNPassword      = "dsnpw-Wn5Cv3Bx7Zm1Lq9Pr2"
	leakAdminToken       = "adm1n+T0ken/Value=Rf8Gh2Jk-q4Wm7Xz3"
	leakTracingPassword  = "tracepw-Ub3Ni7Mo1Pq5Rs9Tv"
	// Values a reload rotates in, through ${file:…} references, since a
	// reload re-reads the files but not the environment.
	leakRotatedProviderKey = "rotprov-Ae4Bf8Cg2Dh6Ej1Fk5"
	leakRotatedBadKey      = "rotbad-Gl9Hm3In7Jo2Kp6Lq4"
	leakRotatedAdminToken  = "rot+Adm1n/Tok=Mr5Ns9Ot3+Pu8Qv2Rw"
	leakSignalAdminToken   = "sig+Adm1n/Tok=Pu8Qv2Rw6+Sx4Ty8Uz1Va"
)

// leakScan collects everything a gateway run exposes, labeled, and
// checks it for secrets at the end.
type leakScan struct {
	mu        sync.Mutex
	haystacks []haystack
}

type haystack struct{ label, text string }

func (s *leakScan) add(label, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.haystacks = append(s.haystacks, haystack{label, text})
}

// addResponse records a response's status line, headers and body.
func (s *leakScan) addResponse(label string, resp *http.Response, body []byte) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", resp.Status)
	keys := make([]string, 0, len(resp.Header))
	for k := range resp.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", k, strings.Join(resp.Header[k], ", "))
	}
	b.Write(body)
	s.add(label, b.String())
}

// secretForms returns value and the encodings it could leak in: standard
// and URL-safe base64, padded and not, and URL query and path escaping.
func secretForms(value string) map[string]string {
	forms := map[string]string{
		"raw":              value,
		"base64":           base64.StdEncoding.EncodeToString([]byte(value)),
		"base64 unpadded":  base64.RawStdEncoding.EncodeToString([]byte(value)),
		"base64url":        base64.URLEncoding.EncodeToString([]byte(value)),
		"base64url raw":    base64.RawURLEncoding.EncodeToString([]byte(value)),
		"url query escape": url.QueryEscape(value),
		"url path escape":  url.PathEscape(value),
	}
	return forms
}

// check fails t for every form of every secret found in a haystack.
func (s *leakScan) check(t *testing.T, secrets map[string]string) {
	t.Helper()
	findings, haystacks, total := s.find(secrets)
	if haystacks == 0 {
		t.Fatal("nothing was collected to scan")
	}
	for _, f := range findings {
		t.Error(f)
	}
	t.Logf("scanned %d haystacks, %d bytes, for %d secrets in %d forms each", haystacks, total, len(secrets), len(secretForms("x")))
}

// find returns a description of every form of every secret found in a
// haystack, and how many haystacks and bytes it scanned.
func (s *leakScan) find(secrets map[string]string) (findings []string, haystacks, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(secrets))
	for n := range secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, h := range s.haystacks {
		total += len(h.text)
		for _, name := range names {
			forms := secretForms(secrets[name])
			kinds := make([]string, 0, len(forms))
			for k := range forms {
				kinds = append(kinds, k)
			}
			sort.Strings(kinds)
			for _, form := range kinds {
				needle := forms[form]
				if i := strings.Index(h.text, needle); i >= 0 {
					lo, hi := max(0, i-80), min(len(h.text), i+len(needle)+80)
					findings = append(findings, fmt.Sprintf("secret %s (%s form) appears in %s: …%s…", name, form, h.label, h.text[lo:hi]))
				}
			}
		}
	}
	return findings, len(s.haystacks), total
}

// TestLeakScanFindsEveryForm: the scanner of TestNoSecretsLeak reports a
// secret planted in each form it looks for, and nothing in clean text.
func TestLeakScanFindsEveryForm(t *testing.T) {
	secrets := map[string]string{"admin token": leakAdminToken}
	for form, planted := range secretForms(leakAdminToken) {
		t.Run(form, func(t *testing.T) {
			s := &leakScan{}
			s.add("clean", "nothing to see "+strings.Repeat("x", 100))
			s.add("dirty", `{"msg":"oops","detail":"`+planted+`"}`)
			findings, _, _ := s.find(secrets)
			if len(findings) == 0 {
				t.Fatalf("the %s form %q was not found", form, planted)
			}
			for _, f := range findings {
				if !strings.Contains(f, "appears in dirty") {
					t.Errorf("finding outside the dirty haystack: %s", f)
				}
			}
		})
	}
}

// step is one request TestNoSecretsLeak drives. wantStatus 0 accepts any
// status: the step is there for what it logs.
type step struct {
	name, method, url, auth, body string
	wantStatus                    int
}

// waitReloads waits until the gateway has logged n successful reloads
// ("config reloaded"), or fails the test.
func waitReloads(t *testing.T, g *harness.Gateway, n int) {
	t.Helper()
	deadline := time.Now().Add(callTimeout)
	for {
		found := 0
		for _, l := range g.Logs() {
			if strings.Contains(l, `"msg":"config reloaded"`) {
				found++
			}
		}
		if found >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no successful reload #%d logged", n)
		}
		select {
		case <-g.Exited():
			t.Fatal("the gateway exited while reloading")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// closedAddr returns a loopback address that nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestNoSecretsLeak runs the gateway binary with distinctive secrets in
// every secret-bearing setting, drives the paths that log, fail or echo
// (startup at debug level, auth failures, upstream 401 and 500, invalid
// requests, streaming, the admin API with and without the token, a startup
// that fails on its database), and scans all of stdout and stderr, every
// response's headers and body, what the upstreams received besides their
// own Authorization header, and what the trace collector received, for
// each secret in its raw, base64 and URL-escaped forms (DESIGN §17).
//
// To cover a new path, add a step to steps; to cover a new secret, add it
// to secrets and to the config.
func TestNoSecretsLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the gateway binary")
	}
	scan := &leakScan{}

	// The upstreams: one that accepts the provider key, one that always
	// fails. A provider configured with the wrong key gets a 401 from the
	// first.
	good := fakeupstream.New(fakeupstream.Options{APIKey: leakProviderKey})
	t.Cleanup(good.Close)
	broken := fakeupstream.New(fakeupstream.Options{})
	t.Cleanup(broken.Close)
	broken.Script(fakeupstream.Behavior{Status: http.StatusInternalServerError})

	// The state Redis requires its password; the cache Redis is down.
	state := miniredis.RunT(t)
	state.RequireAuth(leakStatePassword)
	stateURL := "redis://:" + leakStatePassword + "@" + state.Addr() + "/0"
	cacheURL := "redis://:" + leakCachePassword + "@" + closedAddr(t) + "/0"

	// The trace collector records what it receives: the span names and
	// attributes are in the protobuf body as plain bytes. Its Basic
	// credentials are checked by the exporter's own tests.
	var exports atomic.Int64
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		scan.add("trace collector request to "+r.URL.Path, string(b))
		if r.URL.Path == "/v1/traces" && len(b) > 0 {
			exports.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)
	tracingEndpoint := strings.Replace(collector.URL, "http://", "http://otel:"+leakTracingPassword+"@", 1)

	key, hash, err := harness.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, _, err := harness.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dsn := "postgres://switchyard:" + leakDSNPassword + "@" + closedAddr(t) + "/switchyard?sslmode=disable"

	secrets := map[string]string{
		"provider api_key":       leakProviderKey,
		"wrong provider api_key": leakWrongProviderKey,
		"state Redis URL":        stateURL,
		"state Redis password":   leakStatePassword,
		"cache Redis URL":        cacheURL,
		"cache Redis password":   leakCachePassword,
		"DSN":                    dsn,
		"DSN password":           leakDSNPassword,
		"admin token":            leakAdminToken,
		"tracing endpoint":       tracingEndpoint,
		"tracing password":       leakTracingPassword,
		"gateway key":            key,
		"rotated provider key":   leakRotatedProviderKey,
		"rotated wrong key":      leakRotatedBadKey,
		"rotated admin token":    leakRotatedAdminToken,
		"SIGHUP admin token":     leakSignalAdminToken,
		"rejected gateway key":   wrongKey,
	}
	env := []string{
		"SECRET_PROVIDER_KEY=" + leakProviderKey,
		"SECRET_WRONG_PROVIDER_KEY=" + leakWrongProviderKey,
		"SECRET_STATE_URL=" + stateURL,
		"SECRET_CACHE_URL=" + cacheURL,
		"SECRET_ADMIN_TOKEN=" + leakAdminToken,
		"SECRET_TRACING_ENDPOINT=" + tracingEndpoint,
		"SECRET_DSN=" + dsn,
	}

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	bin, err := harness.Build(ctx, dir, raceEnabled)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("startup fails on an unreachable database", func(t *testing.T) {
		cfg := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", admin_token: "${SECRET_ADMIN_TOKEN}"}
logging: {level: debug, format: json}
storage:
  ledger: {driver: postgres, dsn: "${SECRET_DSN}"}
providers:
  - {id: good, type: openai, base_url: %q, api_key: "${SECRET_PROVIDER_KEY}"}
models:
  - id: m-ok
    deployments: [{provider: good, upstream_model: up-ok}]
`, good.BaseURL())
		path := filepath.Join(dir, "dsn.yaml")
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(runCtx, bin, "serve", "-c", path) // #nosec G204 -- the binary this test built
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		scan.add("stdout of the failed startup", stdout.String())
		scan.add("stderr of the failed startup", stderr.String())
		if err == nil {
			t.Fatal("serve succeeded with an unreachable Postgres")
		}
		if !strings.Contains(stderr.String()+stdout.String(), "storage.ledger") {
			t.Errorf("the failure does not name storage.ledger:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		}
		// check (below) also scans what each command printed.
		for _, args := range [][]string{{"config", "validate", "-c", path}, {"config", "print", "-c", path}} {
			cmd := exec.CommandContext(runCtx, bin, args...) // #nosec G204 -- the binary this test built
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
			out, _ := cmd.CombinedOutput()
			scan.add("output of switchyard "+strings.Join(args[:2], " "), string(out))
		}
	})

	// config renders the gateway's config with the given references (or
	// values) for the rotatable secrets; extra is appended.
	config := func(adminToken, brokenKey, badKey, extra string) []byte {
		return []byte(fmt.Sprintf(`version: 1
server:
  listen: "127.0.0.1:0"
  admin_listen: "127.0.0.1:0"
  shutdown_delay: 0s
  shutdown_grace: 5s
  admin_token: "%[4]s"
logging: {level: debug, format: json}
storage:
  mode: redis
  redis: {state_url: "${SECRET_STATE_URL}", cache_url: "${SECRET_CACHE_URL}"}
telemetry:
  tracing: {enabled: true, endpoint: "${SECRET_TRACING_ENDPOINT}", sample_ratio: 1}
providers:
  - {id: good, type: openai, base_url: %[1]q, api_key: "${SECRET_PROVIDER_KEY}"}
  - {id: badkey, type: openai, base_url: %[1]q, api_key: "%[6]s"}
  - {id: broken, type: openai, base_url: %[2]q, api_key: "%[5]s"}
models:
  - id: m-ok
    capabilities: [streaming, stream_usage]
    deployments: [{provider: good, upstream_model: up-ok, pricing: {input: "1", output: "2"}}]
  - id: m-401
    deployments: [{provider: badkey, upstream_model: up-401, pricing: {input: "1", output: "2"}}]
  - id: m-500
    capabilities: [streaming, stream_usage]
    deployments: [{provider: broken, upstream_model: up-500, pricing: {input: "1", output: "2"}}]
routing: {fallback: {retries_per_deployment: 0}}
policies:
  - id: default
    cache: {exact: {enabled: true}}
tenants:
  - id: t1
    keys: [{id: k1, hash: %[3]q}]
    budgets: [{scope: tenant, window: day, limit_usd: "100"}]
%[7]s`, good.BaseURL(), broken.BaseURL(), hash, adminToken, brokenKey, badKey, extra))
	}
	cfg := config("${SECRET_ADMIN_TOKEN}", "${SECRET_PROVIDER_KEY}", "${SECRET_WRONG_PROVIDER_KEY}", "")

	cfgPath := filepath.Join(dir, "switchyard.yaml")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := harness.Start(ctx, bin, cfgPath, harness.StartOptions{Env: env})
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		if err := g.Stop(30 * time.Second); err != nil {
			t.Errorf("stopping the gateway: %v", err)
		}
	}
	t.Cleanup(func() {
		stop()
		if t.Failed() {
			t.Logf("gateway stderr:\n%s", g.Stderr())
			t.Logf("gateway logs:\n%s", strings.Join(g.Logs(), "\n"))
		}
	})

	client := &http.Client{Timeout: callTimeout}
	// do sends one request, records the response for the scan and
	// returns its status and body.
	do := func(t *testing.T, label, method, url, auth, body string) (int, string) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rd)
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s: reading the response: %v", label, err)
		}
		scan.addResponse("response to "+label, resp, b)
		return resp.StatusCode, string(b)
	}
	chat := func(model string, stream bool) string {
		s := ""
		if stream {
			s = `,"stream":true`
		}
		return `{"model":"` + model + `","messages":[{"role":"user","content":"` + prompt + `"}]` + s + `}`
	}
	completions := g.PublicURL + "/v1/chat/completions"
	bearer := "Bearer " + key
	adminBearer := "Bearer " + leakAdminToken

	// steps are the paths driven, in order.
	steps := []step{
		{"no key", http.MethodPost, completions, "", chat("m-ok", false), http.StatusUnauthorized},
		{"wrong key", http.MethodPost, completions, "Bearer " + wrongKey, chat("m-ok", false), http.StatusUnauthorized},
		{"the admin token as an API key", http.MethodPost, completions, adminBearer, chat("m-ok", false), http.StatusUnauthorized},
		{"the provider key as an API key", http.MethodPost, completions, "Bearer " + leakProviderKey, chat("m-ok", false), http.StatusUnauthorized},
		{"basic auth with the key", http.MethodPost, completions, "Basic " + base64.StdEncoding.EncodeToString([]byte("u:"+key)), chat("m-ok", false), http.StatusUnauthorized},
		{"success", http.MethodPost, completions, bearer, chat("m-ok", false), http.StatusOK},
		{"success again (cache path)", http.MethodPost, completions, bearer, chat("m-ok", false), http.StatusOK},
		{"stream", http.MethodPost, completions, bearer, chat("m-ok", true), http.StatusOK},
		{"upstream 401", http.MethodPost, completions, bearer, chat("m-401", false), 0},
		{"upstream 500", http.MethodPost, completions, bearer, chat("m-500", false), 0},
		{"upstream 500 on a stream", http.MethodPost, completions, bearer, chat("m-500", true), 0},
		{"invalid JSON", http.MethodPost, completions, bearer, `{"model":`, http.StatusBadRequest},
		{"unknown model", http.MethodPost, completions, bearer, chat("no-such-model", false), http.StatusNotFound},
		{"usage report", http.MethodGet, g.PublicURL + "/switchyard/v1/usage", bearer, "", http.StatusOK},
		{"models", http.MethodGet, g.PublicURL + "/v1/models", bearer, "", http.StatusOK},
		{"readyz", http.MethodGet, g.PublicURL + "/readyz", "", "", 0},
		{"admin providers with the token", http.MethodGet, g.AdminURL + "/admin/v1/providers", adminBearer, "", http.StatusOK},
		{"admin usage with the token", http.MethodGet, g.AdminURL + "/admin/v1/usage?tenant=t1", adminBearer, "", http.StatusOK},
		{"admin cache purge with the token", http.MethodPost, g.AdminURL + "/admin/v1/cache/purge", adminBearer, `{"tenant":"t1"}`, 0},
		{"admin providers without the token", http.MethodGet, g.AdminURL + "/admin/v1/providers", "", "", http.StatusUnauthorized},
		{"admin providers with a wrong token", http.MethodGet, g.AdminURL + "/admin/v1/providers", "Bearer " + leakAdminToken + "x", "", http.StatusUnauthorized},
		{"admin cache purge without the token", http.MethodPost, g.AdminURL + "/admin/v1/cache/purge", "", `{"tenant":"t1"}`, http.StatusUnauthorized},
		{"reload without the token", http.MethodPost, g.AdminURL + "/admin/v1/reload", "", "", http.StatusUnauthorized},
		{"reload with the provider key as the token", http.MethodPost, g.AdminURL + "/admin/v1/reload", "Bearer " + leakProviderKey, "", http.StatusUnauthorized},
		{"metrics", http.MethodGet, g.AdminURL + "/metrics", "", "", http.StatusOK},
	}
	runSteps := func(steps []step) {
		t.Helper()
		for _, s := range steps {
			t.Run(s.name, func(t *testing.T) {
				status, body := do(t, s.name, s.method, s.url, s.auth, s.body)
				t.Logf("status %d", status)
				if s.wantStatus != 0 && status != s.wantStatus {
					t.Errorf("status %d, want %d: %.300s", status, s.wantStatus, body)
				}
			})
		}
	}
	runSteps(steps)

	// Reloads: a rejected one, then one that rotates a provider key, the
	// wrong provider key and the admin token to values read from files,
	// then a SIGHUP that rotates the admin token again. The new values
	// must be scrubbed as well as the old ones.
	secretFile := func(name, value string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		return "${file:" + path + "}"
	}
	writeConfig := func(b []byte) {
		if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rotatedAdmin := secretFile("admin2", leakRotatedAdminToken)
	rotatedProvider := secretFile("provider2", leakRotatedProviderKey)
	rotatedBad := secretFile("bad2", leakRotatedBadKey)
	reload := g.AdminURL + "/admin/v1/reload"

	writeConfig(config(rotatedAdmin, rotatedProvider, rotatedBad, "bogus_section: 1\n"))
	runSteps([]step{{"invalid reload", http.MethodPost, reload, adminBearer, "", http.StatusBadRequest}})

	writeConfig(config(rotatedAdmin, rotatedProvider, rotatedBad, ""))
	rotatedBearer := "Bearer " + leakRotatedAdminToken
	runSteps([]step{
		{"rotating reload", http.MethodPost, reload, adminBearer, "", http.StatusOK},
		{"the old admin token after the rotation", http.MethodGet, g.AdminURL + "/admin/v1/providers", adminBearer, "", http.StatusUnauthorized},
		{"the rotated admin token", http.MethodGet, g.AdminURL + "/admin/v1/providers", rotatedBearer, "", http.StatusOK},
		{"upstream 401 with the rotated wrong key", http.MethodPost, completions, bearer, chat("m-401", false), 0},
		{"upstream 500 with the rotated key", http.MethodPost, completions, bearer, chat("m-500", false), 0},
		{"upstream 500 on a stream with the rotated key", http.MethodPost, completions, bearer, chat("m-500", true), 0},
		{"success after the rotation", http.MethodPost, completions, bearer, chat("m-ok", true), http.StatusOK},
		{"the rotated admin token as an API key", http.MethodPost, completions, rotatedBearer, chat("m-ok", false), http.StatusUnauthorized},
	})

	writeConfig(config(secretFile("admin3", leakSignalAdminToken), rotatedProvider, rotatedBad, ""))
	if err := g.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitReloads(t, g, 2)
	runSteps([]step{
		{"the SIGHUP admin token", http.MethodGet, g.AdminURL + "/admin/v1/providers", "Bearer " + leakSignalAdminToken, "", http.StatusOK},
		{"the replaced admin token after SIGHUP", http.MethodGet, g.AdminURL + "/admin/v1/providers", rotatedBearer, "", http.StatusUnauthorized},
		{"metrics after the reloads", http.MethodGet, g.AdminURL + "/metrics", "", "", http.StatusOK},
	})

	// Shut down, so the shutdown logs are scanned too.
	stop()
	scan.add("gateway stdout", strings.Join(g.Logs(), "\n"))
	scan.add("gateway stderr", g.Stderr())
	for _, up := range []*fakeupstream.Server{good, broken} {
		for i, r := range up.Requests() {
			h := r.Header.Clone()
			// The provider key belongs in the upstream's own Authorization
			// header, and nowhere else.
			h.Del("Authorization")
			scan.add(fmt.Sprintf("upstream request %d to %s", i, up.BaseURL()), fmt.Sprintf("%s %s\n%v\n%s", r.Method, r.Path, h, r.Body))
			if got := r.Header.Get("Authorization"); strings.Contains(got, key) {
				t.Errorf("the client's gateway key reached the upstream: %q", got)
			}
		}
	}
	// The debug startup line dumps the effective config: the riskiest line.
	if logs := strings.Join(g.Logs(), "\n"); !strings.Contains(logs, `"msg":"effective config"`) {
		t.Error(`no "effective config" debug line: the startup path was not covered`)
	}
	// The spans were exported (at the latest by the shutdown's flush), so
	// their attributes were scanned too.
	if exports.Load() == 0 {
		t.Error("the trace collector received no spans")
	}
	if len(good.Requests()) == 0 || len(broken.Requests()) == 0 {
		t.Errorf("the upstreams saw %d and %d requests; want both reached", len(good.Requests()), len(broken.Requests()))
	}
	scan.check(t, secrets)
}
