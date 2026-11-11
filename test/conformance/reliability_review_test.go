package conformance

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeupstream"
)

// Regression tests for the Phase 3 review findings, through the gateway
// binary: the timeouts that must not blame a deployment for its clients
// or for long generations, half-open probes that are streams, and the
// fallback past an adapter's local rejection.

// bulkUpstream is an OpenAI-wire upstream that answers a stream with
// chunks events of pad bytes of content each, delay apart, or fails every
// request with a 500 while failing is set.
type bulkUpstream struct {
	*httptest.Server
	failing  atomic.Bool
	chunks   int
	pad      int
	delay    time.Duration
	requests atomic.Int64
}

func newBulkUpstream(t *testing.T, chunks, pad int, delay time.Duration) *bulkUpstream {
	t.Helper()
	u := &bulkUpstream{chunks: chunks, pad: pad, delay: delay}
	u.Server = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.Close)
	return u
}

func (u *bulkUpstream) serve(w http.ResponseWriter, r *http.Request) {
	u.requests.Add(1)
	body, _ := io.ReadAll(r.Body)
	if u.failing.Load() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom","type":"server_error"}}`)
		return
	}
	if !strings.Contains(string(body), `"stream":true`) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fl := w.(http.Flusher)
	content := strings.Repeat("x", u.pad)
	for range u.chunks {
		if _, err := fmt.Fprintf(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", content); err != nil {
			return
		}
		fl.Flush()
		if u.delay > 0 {
			select {
			case <-time.After(u.delay):
			case <-r.Context().Done():
				return
			}
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	fl.Flush()
}

// singleEnv starts a gateway whose model "m" has a single deployment, on
// provider p at baseURL with the given timeouts (a YAML flow mapping).
// server and routing are YAML lines added to those sections, indented by
// two spaces.
func singleEnv(t *testing.T, baseURL, timeouts, server, routing string) *relEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs the gateway; skipped with -short")
	}
	key, hash, err := harness.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`version: 1
server:
  listen: "127.0.0.1:0"
  admin_listen: "127.0.0.1:0"
  shutdown_delay: 0s
  shutdown_grace: 5s
%s
logging: {level: info, format: json}
providers:
  - {id: p, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}", timeouts: %s}
models:
  - id: m
    capabilities: [streaming, stream_usage]
    deployments:
      - {provider: p, upstream_model: m-p, pricing: {input: "1", output: "1"}}
routing:
%s
tenants:
  - {id: %s, keys: [{id: %s, hash: %q}]}
`, server, baseURL, timeouts, routing, harness.TenantID, harness.KeyID, hash)
	g := harness.RunYAML(t, []byte(cfg), harness.RunOptions{Race: raceEnabled, Env: []string{"PROVIDER_KEY=" + providerKey}})
	return &relEnv{g: g, key: key}
}

// slowRead sends a streaming chat request over a raw connection with a
// small receive buffer, reads the status line, stops reading for pause
// (so the gateway's writes block), then reads the rest and returns it.
func slowRead(t *testing.T, e *relEnv, pause time.Duration) (status, rest string) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(e.g.PublicURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.(*net.TCPConn).SetReadBuffer(4096); err != nil {
		t.Fatal(err)
	}
	if err := c.SetDeadline(time.Now().Add(callTimeout)); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`
	fmt.Fprintf(c, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nConnection: close\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", e.key, len(body), body)
	br := bufio.NewReaderSize(c, 4096)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("status line: %v", err)
	}
	time.Sleep(pause)
	b, _ := io.ReadAll(br) // the connection may end without a clean close
	return strings.TrimSpace(line), string(b)
}

// TestReliabilitySlowReader checks that a client that reads its stream
// slowly is not the deployment's fault. The idle timeout measures only
// the wait for the upstream, so a stream whose writes block for longer
// than idle still completes; a client that stops reading for longer than
// server.write_timeout is cut off, as a client that went away. Neither
// opens the deployment's breaker for everyone else.
func TestReliabilitySlowReader(t *testing.T) {
	const pause = 3 * time.Second
	// 1000 events of 32 KiB, far more than the socket buffers hold.
	u := newBulkUpstream(t, 1000, 32<<10, 0)
	tests := []struct {
		name   string
		server string
		done   bool   // whether the stream completes
		chain  string // the attempt chain logged for it
	}{
		{name: "writes block longer than idle", done: true, chain: "m@p:ok"},
		// Cut off by write_timeout: the client's fault, so neutral.
		{name: "longer than write_timeout", server: "  write_timeout: 1s", done: false, chain: "m@p:canceled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := singleEnv(t, u.URL+"/v1", "{idle: 1s}", tt.server,
				"  breaker: {consecutive_failures: 1}\n  fallback: {retries_per_deployment: 0}")
			status, rest := slowRead(t, e, pause)
			if !strings.Contains(status, " 200 ") {
				t.Fatalf("status line %q", status)
			}
			if done := strings.Contains(rest, "data: [DONE]"); done != tt.done {
				tail := rest[max(0, len(rest)-200):]
				t.Errorf("stream completed = %v, want %v; tail %q", done, tt.done, tail)
			}
			if strings.Contains(rest, "idle timeout") {
				t.Errorf("the stream hit the idle timeout while the client was slow")
			}
			if chain := e.waitRequestLines(t, 1)[0]["attempt_chain"]; chain != tt.chain {
				t.Errorf("attempt_chain %v, want %s", chain, tt.chain)
			}
			if got := e.breakerState(t, "m@p"); got != "closed" {
				t.Errorf("breaker %q after a slow reader, want closed", got)
			}
			if r := e.call(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, nil); r.status != http.StatusOK {
				t.Errorf("another client: status %d, body %s", r.status, r.body)
			}
		})
	}
}

// TestReliabilityLongNonStream checks that first_token does not bound a
// non-streaming request, whose response arrives only after the whole
// generation, and that a non-streaming request that exceeds the
// provider's total timeout is not retried on the same deployment.
func TestReliabilityLongNonStream(t *testing.T) {
	long := `{"model":"m","messages":[{"role":"user","content":"write a long essay"}],"max_tokens":8000}`
	t.Run("longer than first_token", func(t *testing.T) {
		f := fakeupstream.New(fakeupstream.Options{APIKey: providerKey})
		t.Cleanup(f.Close)
		e := singleEnv(t, f.BaseURL(), "{first_token: 1s}", "",
			"  fallback: {retries_per_deployment: 1, backoff_base: 5ms, backoff_max: 10ms}\n  breaker: {consecutive_failures: 2}")
		f.Script(fakeupstream.Behavior{TTFT: 1500 * time.Millisecond})
		for i := range 2 {
			r := e.call(t, long, nil)
			if r.status != http.StatusOK || r.header.Get("X-Switchyard-Attempts") != "1" {
				t.Fatalf("request %d: status %d, attempts %q, body %s", i, r.status, r.header.Get("X-Switchyard-Attempts"), r.body)
			}
		}
		if n := len(f.Requests()); n != 2 {
			t.Errorf("%d upstream requests for 2 client requests", n)
		}
		if got := e.breakerState(t, "m@p"); got != "closed" {
			t.Errorf("breaker %q, want closed", got)
		}
	})
	t.Run("longer than total", func(t *testing.T) {
		f := fakeupstream.New(fakeupstream.Options{APIKey: providerKey})
		t.Cleanup(f.Close)
		e := singleEnv(t, f.BaseURL(), "{first_token: 100ms, total: 500ms}", "",
			"  fallback: {retries_per_deployment: 1, backoff_base: 5ms, backoff_max: 10ms}")
		f.Script(fakeupstream.Behavior{TTFT: 5 * time.Second})
		r := e.call(t, long, nil)
		if r.status != http.StatusGatewayTimeout || r.header.Get("X-Switchyard-Attempts") != "1" {
			t.Fatalf("status %d, attempts %q, body %s", r.status, r.header.Get("X-Switchyard-Attempts"), r.body)
		}
		if n := len(f.Requests()); n != 1 {
			t.Errorf("%d upstream requests, want 1: a long generation is not repeated", n)
		}
	})
}

// TestReliabilityStreamingProbe checks that a half-open probe that is a
// long stream closes the breaker when its first event arrives, so other
// requests are served while it streams.
func TestReliabilityStreamingProbe(t *testing.T) {
	// 20 events 200 ms apart: a 4 s stream.
	u := newBulkUpstream(t, 20, 10, 200*time.Millisecond)
	e := singleEnv(t, u.URL+"/v1", "{first_token: 5s}", "",
		"  breaker: {consecutive_failures: 1, open_for: 1s, half_open_probes: 1}\n  fallback: {retries_per_deployment: 0}")
	plain := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	u.failing.Store(true)
	if r := e.call(t, plain, nil); r.status != http.StatusBadGateway {
		t.Fatalf("failing request: status %d", r.status)
	}
	u.failing.Store(false)
	waitUntil(t, "the breaker to go half-open", func() bool { return e.breakerState(t, "m@p") == "half_open" })

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.g.PublicURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if first, err := br.ReadString('\n'); resp.StatusCode != http.StatusOK || err != nil || !strings.HasPrefix(first, "data: ") {
		t.Fatalf("probe stream: status %d, first line %q, %v", resp.StatusCode, first, err)
	}
	// The probe is still streaming.
	if got := e.breakerState(t, "m@p"); got != "closed" {
		t.Errorf("breaker %q once the probe's first event arrived, want closed", got)
	}
	if r := e.call(t, plain, nil); r.status != http.StatusOK {
		t.Errorf("request while the probe streams: status %d, body %s", r.status, r.body)
	}
	rest, err := io.ReadAll(br)
	if err != nil || !strings.Contains(string(rest), "data: [DONE]") {
		t.Errorf("probe stream did not complete: %v", err)
	}
}

// TestReliabilityUnsupportedFallsBack checks that a request the Anthropic
// adapter cannot translate falls back to the next deployment rather than
// stopping with the adapter's 400, and that the 400 is the answer when no
// other deployment may serve the request.
func TestReliabilityUnsupportedFallsBack(t *testing.T) {
	e := startReliability(t, relOptions{routing: quickRouting(0)})
	// A file content part: OpenAI-wire upstreams take it, the Messages API
	// translation cannot express it.
	body := func(model string) string {
		return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"file","file":{"file_id":"file-abc"}}]}]}`, model)
	}
	e.primary.Script(fakeupstream.Behavior{Status: http.StatusServiceUnavailable})
	r := e.call(t, body(relModel), nil)
	if r.status != http.StatusOK || r.header.Get("X-Switchyard-Provider") != "backup" ||
		r.header.Get("X-Switchyard-Attempts") != "3" || r.header.Get("X-Switchyard-Route-Reason") != "fallback:server_error" {
		t.Fatalf("status %d, headers %v, body %s", r.status, r.header, r.body)
	}
	if e.counts() != "primary=1 claude=0 backup=1" {
		t.Errorf("upstream calls %s", e.counts())
	}
	lines := e.waitRequestLines(t, 1)
	if chain := lines[len(lines)-1]["attempt_chain"]; chain != "m@primary:server_error,m@claude:unsupported,m@backup:ok" {
		t.Errorf("attempt_chain %v", chain)
	}
	// Pinned to the Anthropic deployment, nothing else may serve it.
	r = e.call(t, body(relModel+"@claude"), nil)
	if r.status != http.StatusBadRequest || r.err.Type != "invalid_request_error" || !strings.Contains(r.err.Message, "file") {
		t.Errorf("pinned: status %d, error %+v", r.status, r.err)
	}
	if n := len(e.claude.Requests()); n != 0 {
		t.Errorf("the Anthropic upstream got %d requests", n)
	}
}

// TestReliabilityMidStreamFailuresOpenBreaker checks that upstream
// failures after the commit point still count toward the breaker: three
// streams that the primary drops mid-way open its breaker, and traffic
// shifts to the next deployment. Each client gets the error event, after
// one upstream call.
func TestReliabilityMidStreamFailuresOpenBreaker(t *testing.T) {
	e := startReliability(t, relOptions{routing: quickRouting(1, "breaker: {consecutive_failures: 3}")})
	e.primary.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultDrop, FaultAfterChunks: 2})
	for i := range 3 {
		r := e.call(t, relBody(true), nil)
		if r.status != http.StatusOK || r.header.Get("X-Switchyard-Provider") != "primary" || len(r.events) == 0 ||
			!strings.Contains(r.events[len(r.events)-1], `"error"`) {
			t.Fatalf("stream %d: status %d, provider %q, events %v", i, r.status, r.header.Get("X-Switchyard-Provider"), r.events)
		}
	}
	if got := e.counts(); got != "primary=3 claude=0 backup=0" {
		t.Errorf("upstream requests %s, want one per stream", got)
	}
	if got := e.breakerState(t, "m@primary"); got != "open" {
		t.Fatalf("breaker %q after 3 mid-stream drops, want open", got)
	}
	r := e.call(t, relBody(true), nil)
	if r.status != http.StatusOK || r.header.Get("X-Switchyard-Provider") != "claude" || r.events[len(r.events)-1] != "[DONE]" {
		t.Errorf("with the breaker open: status %d, provider %q", r.status, r.header.Get("X-Switchyard-Provider"))
	}
}
