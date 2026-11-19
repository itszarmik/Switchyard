package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/readyz":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/moved":
			http.Redirect(w, r, "/healthz", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String() + "/healthz"
	_ = ln.Close()

	tests := []struct {
		name       string
		args       []string
		want       int
		wantStderr string
	}{
		{"healthy", []string{"--url", srv.URL + "/healthz"}, exitOK, ""},
		{"not ready", []string{"--url", srv.URL + "/readyz"}, exitError, "status 503"},
		{"redirect is not followed", []string{"--url", srv.URL + "/moved"}, exitError, "status 302"},
		{"nothing listening", []string{"--url", closed, "--timeout", "1s"}, exitError, "connection refused"},
		{"not a URL", []string{"--url", "127.0.0.1:8080"}, exitError, "http or https URL"},
		{"bad timeout", []string{"--url", srv.URL, "--timeout", "0s"}, exitError, "positive"},
		{"positional argument", []string{"extra"}, exitError, "unexpected argument"},
		{"unknown flag", []string{"--bogus"}, exitError, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			got := run(append([]string{"healthcheck"}, tt.args...), &stdout, &stderr, func(string) (string, bool) { return "", false })
			if got != tt.want {
				t.Fatalf("exit %d, want %d; stderr: %s", got, tt.want, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout %q, want nothing", stdout.String())
			}
			if tt.want == exitOK && stderr.Len() != 0 {
				t.Errorf("stderr %q on success, want nothing", stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}
