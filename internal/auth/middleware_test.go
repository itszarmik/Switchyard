package auth

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/config"
	"switchyard/internal/reqctx"
)

// wantUnauthorized is the exact 401 body.
const wantUnauthorized = `{"error":{"message":"Incorrect API key provided.","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}` + "\n"

// harness serves requests through Middleware with a request logger that
// writes JSON to logs at DEBUG. The logger is a plain slog handler, not the
// redacting one, so a key that reached the log would show up verbatim.
type harness struct {
	handler http.Handler
	logs    *bytes.Buffer
	// reached records the Principal of each request that got past the
	// middleware.
	reached []Principal
}

func newHarness(t *testing.T, s *Store) *harness {
	t.Helper()
	h := &harness{logs: &bytes.Buffer{}}
	logger := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		if !ok {
			t.Error("the handler ran without a Principal in its context")
		}
		h.reached = append(h.reached, p)
		reqctx.Logger(r.Context()).InfoContext(r.Context(), "handled")
		w.WriteHeader(http.StatusNoContent)
	})
	mw := Middleware(s)(next)
	// Stand in for the server's request-ID middleware.
	h.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := reqctx.WithRequestID(r.Context(), "req-1")
		ctx = reqctx.WithLogger(ctx, logger.With("request_id", "req-1"))
		mw.ServeHTTP(w, r.WithContext(ctx))
	})
	return h
}

// serve sends a request with the given Authorization header values.
func (h *harness) serve(auth ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	for _, a := range auth {
		req.Header.Add("Authorization", a)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// logLines parses the captured JSON log lines.
func (h *harness) logLines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(h.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestMiddlewareAccepts(t *testing.T) {
	key, hash := mustGenerate(t)
	s := NewStore([]config.Tenant{{ID: "acme", Keys: []config.APIKey{{ID: "acme-backend", Hash: hash, Tags: []string{"prod"}}}}})
	want := Principal{TenantID: "acme", KeyID: "acme-backend", Tags: []string{"prod"}}

	tests := []struct {
		name   string
		header string
	}{
		{"canonical", "Bearer " + key},
		{"lower-case scheme", "bearer " + key},
		{"upper-case scheme", "BEARER " + key},
		{"several spaces", "Bearer   " + key},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, s)
			rec := h.serve(tt.header)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusNoContent, rec.Body)
			}
			if len(h.reached) != 1 || !cmp.Equal(h.reached[0], want) {
				t.Errorf("handler saw %+v, want one request as %+v", h.reached, want)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "" {
				t.Errorf("WWW-Authenticate = %q on success", got)
			}

			// The handler's log line carries the request ID, tenant and key
			// id, and never the key.
			lines := h.logLines(t)
			if len(lines) != 1 {
				t.Fatalf("got %d log lines, want 1:\n%s", len(lines), h.logs)
			}
			l := lines[0]
			if l["msg"] != "handled" || l["request_id"] != "req-1" || l["tenant"] != "acme" || l["key_id"] != "acme-backend" {
				t.Errorf("log line = %v, want msg handled with request_id, tenant and key_id", l)
			}
			if strings.Contains(h.logs.String(), key) {
				t.Errorf("the logs contain the key:\n%s", h.logs)
			}
		})
	}
}

func TestMiddlewareRejects(t *testing.T) {
	key, hash := mustGenerate(t)
	unknown, _ := mustGenerate(t)
	s := NewStore([]config.Tenant{{ID: "acme", Keys: []config.APIKey{{ID: "acme-backend", Hash: hash}}}})

	tests := []struct {
		name       string
		headers    []string
		wantReason string
	}{
		{"no header", nil, reasonMissing},
		{"empty header", []string{""}, reasonMalformed},
		{"unknown key", []string{"Bearer " + unknown}, reasonUnknown},
		{"the hash instead of the key", []string{"Bearer " + hash}, reasonUnknown},
		{"key without a scheme", []string{key}, reasonMalformed},
		{"scheme only", []string{"Bearer"}, reasonMalformed},
		{"scheme and space", []string{"Bearer "}, reasonMalformed},
		{"basic auth", []string{"Basic " + key}, reasonMalformed},
		{"other scheme", []string{"Token " + key}, reasonMalformed},
		{"scheme without a space", []string{"Bearer" + key}, reasonMalformed},
		{"tab separator", []string{"Bearer\t" + key}, reasonMalformed},
		{"two tokens", []string{"Bearer " + key + " " + key}, reasonMalformed},
		{"token with a tab", []string{"Bearer " + key + "\tx"}, reasonMalformed},
		{"two headers", []string{"Bearer " + key, "Bearer " + key}, reasonMalformed},
		{"key with a suffix", []string{"Bearer " + key + "x"}, reasonUnknown},
		{"wrong prefix", []string{"Bearer sk-" + strings.TrimPrefix(key, "sy_")}, reasonUnknown},
		{"very long key", []string{"Bearer sy_" + strings.Repeat("a", 4096)}, reasonUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, s)
			rec := h.serve(tt.headers...)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := rec.Body.String(); got != wantUnauthorized {
				t.Errorf("body = %q, want %q", got, wantUnauthorized)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", got)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			if len(h.reached) != 0 {
				t.Errorf("the handler ran for a rejected request")
			}

			lines := h.logLines(t)
			if len(lines) != 1 {
				t.Fatalf("got %d log lines, want 1:\n%s", len(lines), h.logs)
			}
			l := lines[0]
			if l["level"] != "DEBUG" || l["msg"] != "authentication failed" || l["reason"] != tt.wantReason || l["request_id"] != "req-1" {
				t.Errorf("log line = %v, want a DEBUG authentication failed line with reason %s", l, tt.wantReason)
			}
			if _, ok := l["tenant"]; ok {
				t.Errorf("a rejected request was logged with a tenant: %v", l)
			}

			// Neither the presented key, the configured key nor its hash
			// appears anywhere in the response or the logs.
			secrets := []string{key, unknown, hash, strings.TrimPrefix(hash, "sha256:"), strings.TrimPrefix(key, "sy_")}
			var resp strings.Builder
			for k, v := range rec.Header() {
				resp.WriteString(k + ": " + strings.Join(v, ",") + "\n")
			}
			resp.WriteString(rec.Body.String())
			for _, sec := range secrets {
				if strings.Contains(resp.String(), sec) {
					t.Errorf("the response contains a secret:\n%s", resp.String())
				}
				if strings.Contains(h.logs.String(), sec) {
					t.Errorf("the logs contain a secret:\n%s", h.logs)
				}
			}
		})
	}
}

func TestMiddlewareWithNoKeys(t *testing.T) {
	key, _ := mustGenerate(t)
	for name, s := range map[string]*Store{"empty store": NewStore(nil), "nil store": nil} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, s)
			if rec := h.serve("Bearer " + key); rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// The tenant and key_id attributes are added to the request logger, not
// set globally: a later request with another key logs its own principal.
func TestMiddlewareLoggerIsPerRequest(t *testing.T) {
	k1, h1 := mustGenerate(t)
	k2, h2 := mustGenerate(t)
	s := NewStore([]config.Tenant{
		{ID: "a", Keys: []config.APIKey{{ID: "ka", Hash: h1}}},
		{ID: "b", Keys: []config.APIKey{{ID: "kb", Hash: h2}}},
	})
	h := newHarness(t, s)
	h.serve("Bearer " + k1)
	h.serve("Bearer " + k2)
	lines := h.logLines(t)
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2:\n%s", len(lines), h.logs)
	}
	if lines[0]["tenant"] != "a" || lines[0]["key_id"] != "ka" || lines[1]["tenant"] != "b" || lines[1]["key_id"] != "kb" {
		t.Errorf("log lines = %v", lines)
	}
	if strings.Count(h.logs.String(), `"tenant"`) != 2 {
		t.Errorf("each line should have exactly one tenant attribute:\n%s", h.logs)
	}
}

// A rejected client that stalls its request body gets the 401 at once, and
// the connection is closed rather than held while net/http waits for the
// rest of the body.
func TestMiddlewareRejectsStalledBody(t *testing.T) {
	srv := httptest.NewServer(Middleware(NewStore(nil))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler ran for a rejected request")
	})))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Far less than the 256 KiB net/http would wait for; only 1 byte comes.
	if _, err := io.WriteString(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\n"+
		"Authorization: Bearer wrong\r\nContent-Length: 100000\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("no response while the body is stalled: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read the 401 body: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized || !resp.Close {
		t.Errorf("status %d, close %v; want 401 and Connection: close", resp.StatusCode, resp.Close)
	}
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("after the 401, read = %v; want io.EOF (connection closed)", err)
	}
}

// A rejected request without a body keeps its connection.
func TestMiddlewareRejectKeepsBodylessConnection(t *testing.T) {
	rec := httptest.NewRecorder()
	Middleware(NewStore(nil))(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("Connection") != "" {
		t.Errorf("status %d, Connection %q; want 401 without Connection: close", rec.Code, rec.Header().Get("Connection"))
	}
}

// The tenant and key_id go on the request's access-log line too, which the
// server logs from outside the middleware.
func TestMiddlewareAnnotatesPrincipal(t *testing.T) {
	key, hash := mustGenerate(t)
	s := NewStore([]config.Tenant{{ID: "acme", Keys: []config.APIKey{{ID: "acme-backend", Hash: hash}}}})
	for _, tt := range []struct {
		name string
		key  string
		want string
	}{
		{name: "accepted", key: key, want: "tenant=acme key_id=acme-backend"},
		{name: "rejected", key: "sy_unknown", want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ann := &reqctx.Annotations{}
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			req.Header.Set("Authorization", "Bearer "+tt.key)
			req = req.WithContext(reqctx.WithAnnotations(req.Context(), ann))
			Middleware(s)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), req)
			var got []string
			for _, a := range ann.Attrs() {
				got = append(got, a.String())
			}
			if strings.Join(got, " ") != tt.want {
				t.Errorf("annotations = %q, want %q", strings.Join(got, " "), tt.want)
			}
		})
	}
}
