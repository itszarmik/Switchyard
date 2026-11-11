package conformance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeupstream"
)

// Provider timeouts of the gateway under test. first_token is short so the
// timeout case is quick; idle and total leave room for slow machines.
const (
	connectTimeout    = 2 * time.Second
	firstTokenTimeout = time.Second
	idleTimeout       = 5 * time.Second
	totalTimeout      = 30 * time.Second
)

// maxCancelLatency is the acceptance bound for a client disconnect to
// cancel the upstream request.
const maxCancelLatency = 100 * time.Millisecond

// newUpstream starts a fake that knows both model names and requires the
// provider key.
func newUpstream(t *testing.T) *fakeupstream.Server {
	t.Helper()
	u := fakeupstream.New(fakeupstream.Options{
		Models: []string{catalogModel, upstreamModel},
		APIKey: providerKey,
	})
	t.Cleanup(u.Close)
	return u
}

// startGateway builds and starts the gateway in front of upstream and
// returns it with a client key it accepts.
func startGateway(t *testing.T, upstream *fakeupstream.Server) (*harness.Gateway, string) {
	t.Helper()
	key, hash, err := harness.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	g := harness.Run(t, harness.Config{
		ProviderBaseURL: upstream.BaseURL(),
		ProviderAPIKey:  providerKey,
		ForwardHeaders:  []string{forwardedHeader},
		Timeouts: harness.Timeouts{
			Connect: connectTimeout, FirstToken: firstTokenTimeout, Idle: idleTimeout, Total: totalTimeout,
		},
		Models:  []harness.Model{{ID: catalogModel, UpstreamModel: upstreamModel}},
		KeyHash: hash,
	}, harness.RunOptions{Race: raceEnabled})
	return g, key
}

func TestConformanceDirect(t *testing.T) {
	upstream := newUpstream(t)
	runSuite(t, upstream.BaseURL(), providerKey, upstream)
}

// TestConformanceThroughGateway runs the suite through the gateway binary,
// then checks the gateway's logs and metrics.
func TestConformanceThroughGateway(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the gateway; skipped with -short")
	}
	upstream := newUpstream(t)
	g, key := startGateway(t, upstream)
	runSuite(t, g.PublicURL+"/v1", key, upstream)

	t.Run("logs", func(t *testing.T) { checkLogs(t, g, key) })
	t.Run("metrics", func(t *testing.T) { checkMetrics(t, g) })
}

// checkLogs checks the security invariant (no key and no content in the
// logs) and the one access-log line per request, which carries the
// completion fields.
func checkLogs(t *testing.T, g *harness.Gateway, clientKey string) {
	lines := g.Logs()
	for i, line := range lines {
		for name, secret := range map[string]string{
			"the client key": clientKey, "the provider key": providerKey, "the wrong key": wrongKey,
			"the prompt": prompt, "the response text": fakeupstream.Text,
		} {
			if strings.Contains(line, secret) {
				t.Errorf("log line %d contains %s: %s", i, name, line)
			}
		}
	}

	fields := []string{"requested_model", "deployment", "stream", "status", "ttft_ms", "latency_ms",
		"overhead_ms", "prompt_tokens", "completion_tokens", "upstream_request_id", "error_code",
		"request_id", "tenant", "key_id"}
	completions, rejected, streamsWithUsage := 0, 0, 0
	for _, line := range lines {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m["msg"] == "completion" {
			t.Errorf("a separate completion line was logged: %s", line)
		}
		if m["msg"] != "request" || m["path"] != "/v1/chat/completions" {
			continue
		}
		if m["status"] == float64(http.StatusUnauthorized) {
			// Rejected by authentication, before the gateway.
			rejected++
			if _, ok := m["tenant"]; ok {
				t.Errorf("a rejected request was logged with a tenant: %s", line)
			}
			continue
		}
		completions++
		for _, f := range fields {
			if _, ok := m[f]; !ok {
				t.Errorf("request line has no %s: %s", f, line)
			}
		}
		if m["level"] != "INFO" || m["tenant"] != harness.TenantID || m["key_id"] != harness.KeyID {
			t.Errorf("request line level %v, tenant %v, key_id %v: %s", m["level"], m["tenant"], m["key_id"], line)
		}
		if rm := m["requested_model"]; rm == catalogModel && m["deployment"] != catalogModel+"@"+harness.ProviderID {
			t.Errorf("request line deployment %v, want %s@%s", m["deployment"], catalogModel, harness.ProviderID)
		}
		// The gateway always asks the upstream for usage, so every stream
		// that completed carries it, whether or not the client asked.
		if m["stream"] == true && m["status"] == float64(http.StatusOK) && m["error_code"] == "" {
			pt, _ := m["prompt_tokens"].(float64)
			ct, _ := m["completion_tokens"].(float64)
			if pt <= 0 || ct <= 0 {
				t.Errorf("completed stream logged prompt_tokens %v, completion_tokens %v, want both positive: %s",
					m["prompt_tokens"], m["completion_tokens"], line)
			}
			streamsWithUsage++
		}
	}
	if completions == 0 {
		t.Errorf("no chat completion request line among %d lines", len(lines))
	}
	if rejected == 0 {
		t.Error("no request line for the requests authentication rejected")
	}
	if streamsWithUsage == 0 {
		t.Error("no completed stream among the request lines")
	} else {
		t.Logf("%d chat completion request lines (and %d rejected), %d of them completed streams, all with usage",
			completions, rejected, streamsWithUsage)
	}
}

// checkMetrics checks that the P1 metrics are exported on the admin
// listener.
func checkMetrics(t *testing.T, g *harness.Gateway) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.AdminURL+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, name := range []string{
		"switchyard_requests_total", "switchyard_request_duration_seconds",
		"switchyard_overhead_duration_seconds", "switchyard_upstream_requests_total",
		"switchyard_upstream_ttft_seconds", "switchyard_upstream_duration_seconds",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics has no %s", name)
		}
	}
	for _, outcome := range []string{"ok", "client_error", "rate_limited", "auth", "server_error", "timeout", "transport"} {
		if !strings.Contains(body, `outcome="`+outcome+`"`) {
			t.Errorf("/metrics has no upstream_requests_total with outcome %q after the suite", outcome)
		}
	}
	if strings.Contains(body, providerKey) {
		t.Error("/metrics contains the provider key")
	}
}

func TestStreamDisconnectDirect(t *testing.T) {
	upstream := newUpstream(t)
	checkDisconnect(t, upstream.BaseURL(), providerKey, upstream)
}

func TestStreamDisconnectThroughGateway(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the gateway; skipped with -short")
	}
	upstream := newUpstream(t)
	g, key := startGateway(t, upstream)
	checkDisconnect(t, g.PublicURL+"/v1", key, upstream)
}

// checkDisconnect streams from baseURL, cancels after two chunks, and checks
// that the upstream request was canceled within maxCancelLatency.
func checkDisconnect(t *testing.T, baseURL, apiKey string, upstream *fakeupstream.Server) {
	upstream.Reset()
	// The upstream is still generating: it sends three chunks, then nothing
	// until its client goes away.
	upstream.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultHang, FaultAfterChunks: 3})

	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey(apiKey),
		option.WithMaxRetries(0),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	stream := client.Chat.Completions.NewStreaming(streamCtx, params())
	defer stream.Close()
	for i := range 2 {
		if !stream.Next() {
			t.Fatalf("stream ended after %d chunks: %v", i, stream.Err())
		}
	}
	canceledAt := time.Now()
	cancelStream()

	reqs, err := upstream.Await(ctx, 1)
	if err != nil {
		t.Fatalf("the upstream request did not finish after the client canceled: %v", err)
	}
	gone := reqs[0].ClientGoneAt
	if gone.IsZero() {
		t.Fatal("the upstream request was not canceled")
	}
	if latency := gone.Sub(canceledAt); latency > maxCancelLatency {
		t.Errorf("the upstream saw the cancellation %s after the client canceled, want at most %s", latency, maxCancelLatency)
	} else {
		t.Logf("cancellation reached the upstream after %s", latency)
	}
}
