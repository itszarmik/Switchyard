package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIsAdminAPI(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
	}{
		{"POST /admin/v1/reload", true},
		{"GET /admin/v1/providers", true},
		{"/admin/v1/", true},
		{"GET example.com/admin/v1/usage", true},
		{"GET /metrics", false},
		{"GET /healthz", false},
		{"GET /admin/v2/x", false},
		{"GET /adminx/v1/x", false},
	}
	for _, tt := range tests {
		if got := isAdminAPI(tt.pattern); got != tt.want {
			t.Errorf("isAdminAPI(%q) = %v, want %v", tt.pattern, got, tt.want)
		}
	}
}

// TestAdminToken: with a token set, /admin/v1/* needs it, /metrics stays
// open, and SetAdminToken rotates or removes it.
func TestAdminToken(t *testing.T) {
	const token = "adm-0f3c9e1d7b5a4c2e8d6f"
	s, logs := newTestServer(t, func(o *Options) { o.AdminToken = token })
	s.HandleAdmin("POST /admin/v1/thing", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	startServer(t, s)
	c := newClient(t)
	admin := "http://" + s.AdminAddr()

	call := func(path string, auth ...string) response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, admin+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range auth {
			req.Header.Add("Authorization", a)
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
		return response{status: resp.StatusCode, header: resp.Header, body: string(b)}
	}

	tests := []struct {
		name   string
		auth   []string
		status int
	}{
		{"no header", nil, http.StatusUnauthorized},
		{"wrong token", []string{"Bearer wrong-token-value"}, http.StatusUnauthorized},
		{"token prefix", []string{"Bearer " + token[:10]}, http.StatusUnauthorized},
		{"basic scheme", []string{"Basic " + token}, http.StatusUnauthorized},
		{"two headers", []string{"Bearer " + token, "Bearer " + token}, http.StatusUnauthorized},
		{"right token", []string{"Bearer " + token}, http.StatusNoContent},
		{"scheme in lower case", []string{"bearer  " + token}, http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := call("/admin/v1/thing", tt.auth...)
			if r.status != tt.status {
				t.Fatalf("status %d, want %d (%s)", r.status, tt.status, r.body)
			}
			if tt.status != http.StatusUnauthorized {
				return
			}
			var env struct {
				Error struct{ Type, Code string }
			}
			if err := json.Unmarshal([]byte(r.body), &env); err != nil || env.Error.Code != "invalid_admin_token" || env.Error.Type != "invalid_request_error" {
				t.Errorf("body %s: want an invalid_admin_token error", r.body)
			}
			if r.header.Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("WWW-Authenticate = %q", r.header.Get("WWW-Authenticate"))
			}
			if strings.Contains(r.body, "wrong-token") || strings.Contains(r.body, token[:10]) {
				t.Errorf("the 401 echoes the presented token: %s", r.body)
			}
		})
	}

	if r := do(t, c, http.MethodGet, admin+"/metrics", nil); r.status != http.StatusOK {
		t.Errorf("/metrics without a token: %d, want 200", r.status)
	}

	s.SetAdminToken("adm-rotated-1a2b3c4d5e6f")
	if r := call("/admin/v1/thing", "Bearer "+token); r.status != http.StatusUnauthorized {
		t.Errorf("the old token after rotation: %d, want 401", r.status)
	}
	if r := call("/admin/v1/thing", "Bearer adm-rotated-1a2b3c4d5e6f"); r.status != http.StatusNoContent {
		t.Errorf("the new token after rotation: %d, want 204", r.status)
	}
	s.SetAdminToken("")
	if r := call("/admin/v1/thing"); r.status != http.StatusNoContent {
		t.Errorf("no token configured: %d, want 204", r.status)
	}
	if strings.Contains(logs.String(), token) {
		t.Error("the logs contain the admin token")
	}
}

// TestSetConfigVersion: /readyz reports the version set after a reload.
func TestSetConfigVersion(t *testing.T) {
	s, _ := newTestServer(t, nil)
	startServer(t, s)
	s.SetConfigVersion("cfg-new")
	r := do(t, newClient(t), http.MethodGet, "http://"+s.PublicAddr()+"/readyz", nil)
	if !strings.Contains(r.body, `"config_version":"cfg-new"`) || s.ConfigVersion() != "cfg-new" {
		t.Errorf("/readyz after SetConfigVersion: %s", r.body)
	}
}

func TestIsAuditLine(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{`{"time":"2026-11-19T08:00:00.123456789Z","level":"INFO","msg":"audit","action":"config.reload"}` + "\n", true},
		{`{"time":"2026-11-19T08:00:00.123456789Z","level":"WARN","msg":"audit","action":"cache.purge"}` + "\n", true},
		{`time=2026-11-19T08:00:00.123Z level=INFO msg=audit action=config.reload` + "\n", true},
		{`{"time":"2026-11-19T08:00:00.123456789Z","level":"INFO","msg":"request","path":"/admin/v1/reload"}` + "\n", false},
		{`{"time":"2026-11-19T08:00:00.123456789Z","level":"INFO","msg":"audited"}` + "\n", false},
		{`time=2026-11-19T08:00:00.123Z level=INFO msg=auditor` + "\n", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsAuditLine([]byte(tt.line)); got != tt.want {
			t.Errorf("IsAuditLine(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}
