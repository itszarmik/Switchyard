package openaiapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/api/openaiapi"
	"switchyard/internal/config"
	"switchyard/internal/gateway"
	"switchyard/internal/server"
)

// testTimeout bounds every wait in these tests. It is far longer than any
// operation should take, so it only fires when something is broken.
const testTimeout = 10 * time.Second

const (
	providerKey = "sk-provider-secret-0123456789abcdef"
	clientKey   = "sy_client_key_ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	maxBody     = 64 << 10
)

// logBuffer is a concurrency-safe log sink that can decode its JSON lines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// find decodes the JSON log lines whose msg is msg.
func (b *logBuffer) find(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(strings.NewReader(b.String()))
	for {
		var m map[string]any
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatalf("decode log: %v", err)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
}

// waitFor polls cond until it holds; it is for log lines written after the
// response, since the access-log line follows the last byte sent.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// received is one request the fake upstream got.
type received struct {
	body   string
	header http.Header
}

// upstream is a scriptable fake OpenAI-wire server.
type upstream struct {
	*httptest.Server
	handler func(u *upstream, w http.ResponseWriter, r *http.Request)

	mu       sync.Mutex
	requests []received
	// doneAt receives the time each request's context ended.
	doneAt chan time.Time

	release     chan struct{}
	releaseOnce sync.Once
}

func newUpstream(t *testing.T, h func(u *upstream, w http.ResponseWriter, r *http.Request)) *upstream {
	t.Helper()
	u := &upstream{handler: h, doneAt: make(chan time.Time, 16), release: make(chan struct{})}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, received{body: string(body), header: r.Header.Clone()})
		u.mu.Unlock()
		context.AfterFunc(r.Context(), func() { u.doneAt <- time.Now() })
		u.handler(u, w, r)
	}))
	t.Cleanup(u.Close)
	t.Cleanup(u.releaseAll)
	return u
}

func (u *upstream) releaseAll() { u.releaseOnce.Do(func() { close(u.release) }) }

// hang blocks until the request is canceled or the test ends.
func (u *upstream) hang(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-u.release:
	}
}

func (u *upstream) got() []received {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]received(nil), u.requests...)
}

// sse writes the raw SSE text of events, flushing after each.
func sse(w http.ResponseWriter, events ...string) {
	for _, ev := range events {
		_, _ = io.WriteString(w, ev)
		w.(http.Flusher).Flush()
	}
}

func sseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("x-request-id", "req-up-1")
	w.WriteHeader(http.StatusOK)
}

// env is a gateway server in front of a fake upstream.
type env struct {
	url  string
	addr string // host:port of url
	logs *logBuffer
	reg  *prometheus.Registry
	up   *upstream
}

func newEnv(t *testing.T, h func(u *upstream, w http.ResponseWriter, r *http.Request), mutate ...func(*config.Config)) *env {
	t.Helper()
	return newEnvWith(t, h, openaiapi.Options{}, mutate...)
}

// newEnvWith is newEnv with handler options.
func newEnvWith(t *testing.T, h func(u *upstream, w http.ResponseWriter, r *http.Request), opts openaiapi.Options, mutate ...func(*config.Config)) *env {
	t.Helper()
	up := newUpstream(t, h)
	cfg := &config.Config{
		Providers: []config.Provider{
			{
				ID: "fake", Type: "openai", BaseURL: up.URL + "/v1",
				APIKey:         config.NewSecret(providerKey),
				ForwardHeaders: []string{"X-Allowed", "OpenAI-Organization"},
				Timeouts: config.ProviderTimeouts{
					Connect:    config.Duration(time.Second),
					FirstToken: config.Duration(testTimeout),
					Idle:       config.Duration(testTimeout),
					Total:      config.Duration(testTimeout),
				},
			},
			{ID: "anth", Type: "anthropic", BaseURL: "https://api.anthropic.test"},
		},
		Models: []config.Model{
			{ID: "gpt-test", Deployments: []config.Deployment{{Provider: "fake", UpstreamModel: "upstream-gpt"}}},
			{ID: "claude-test", Deployments: []config.Deployment{{Provider: "anth", UpstreamModel: "claude"}}},
		},
	}
	for _, m := range mutate {
		m(cfg)
	}

	logs := &logBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reg := prometheus.NewRegistry()
	gw, err := gateway.New(gateway.Options{Config: cfg, Logger: logger, Registry: reg})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	srv, err := server.New(server.Options{
		Listen: "127.0.0.1:0", AdminListen: "127.0.0.1:0",
		MaxRequestBytes: maxBody, MaxHeaderBytes: 1 << 16,
		ReadHeaderTimeout: testTimeout, IdleTimeout: testTimeout, ShutdownGrace: testTimeout,
		Logger: logger, Registry: reg,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	for _, rt := range openaiapi.Routes(gw, opts) {
		srv.Handle(rt.Pattern, gw.Instrument(rt.Endpoint, rt.Handler))
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	// Runs before the shutdown above, so hanging upstream handlers return.
	t.Cleanup(up.releaseAll)
	return &env{url: "http://" + srv.PublicAddr(), addr: srv.PublicAddr(), logs: logs, reg: reg, up: up}
}

// post sends a chat completion request with the client key. The caller
// closes the response body.
func (e *env) post(ctx context.Context, t *testing.T, body string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clientKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

// result is a response whose body has been read and closed.
type result struct {
	StatusCode int
	Header     http.Header
}

// call sends body and returns the response and its whole body.
func (e *env) call(t *testing.T, body string) (result, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	resp := e.post(ctx, t, body, nil)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return result{StatusCode: resp.StatusCode, Header: resp.Header}, string(data)
}

// requestLines returns the access-log lines of chat completion requests.
func (e *env) requestLines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range e.logs.find(t, "request") {
		if rec["path"] == "/v1/chat/completions" {
			out = append(out, rec)
		}
	}
	return out
}

// requestLine waits for the last chat completion request's access-log
// line, which carries the completion fields, and returns it.
func (e *env) requestLine(t *testing.T) map[string]any {
	t.Helper()
	var recs []map[string]any
	waitFor(t, "the request log line", func() bool {
		recs = e.requestLines(t)
		return len(recs) > 0
	})
	return recs[len(recs)-1]
}

// metricValue returns a counter's value, or a histogram's sample count, for
// the series with the given label values (in label-name order).
func (e *env) metricValue(t *testing.T, name string, labels ...string) float64 {
	t.Helper()
	mfs, err := e.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			var vals []string
			for _, lp := range m.GetLabel() {
				vals = append(vals, lp.GetValue())
			}
			if fmt.Sprint(vals) != fmt.Sprint(labels) {
				continue
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
			if g := m.GetGauge(); g != nil {
				return g.GetValue()
			}
			return float64(m.GetHistogram().GetSampleCount())
		}
	}
	return 0
}

// errorEnvelope decodes an OpenAI error envelope.
type errorEnvelope struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    *string `json:"code"`
	} `json:"error"`
}

func decodeError(t *testing.T, body string) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode error envelope %q: %v", body, err)
	}
	return env
}

func deref(s *string) string {
	if s == nil {
		return "<null>"
	}
	return *s
}

// readEvents reads SSE data payloads from r until n have arrived.
func readEvents(t *testing.T, br *bufio.Reader, n int) []string {
	t.Helper()
	var out []string
	for len(out) < n {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read event %d: %v", len(out), err)
		}
		if data, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "data: "); ok {
			out = append(out, data)
		}
	}
	return out
}

// rawRequest is the head of a chat completion request, for tests that drive
// the connection by hand; the caller sends the body.
func rawRequest(contentLength int) string {
	return fmt.Sprintf("POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n"+
		"Authorization: Bearer %s\r\nContent-Length: %d\r\n\r\n", clientKey, contentLength)
}

// waitIdle waits until the public listener has no request in flight.
func (e *env) waitIdle(t *testing.T) {
	t.Helper()
	waitFor(t, "no request in flight", func() bool {
		return e.metricValue(t, "switchyard_http_requests_in_flight", "public") == 0
	})
}
