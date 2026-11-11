package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeanthropic"
	"switchyard/test/fakeupstream"
)

// The P3 acceptance cases, through the gateway binary: retries, fallback,
// circuit breakers, deadlines, retry budgets, spillover and keep-alives.
// The model "m" is served by three scripted fakes, cheapest first:
// primary (OpenAI wire), claude (Messages API) and backup (OpenAI wire).

// relModel is the model every reliability gateway serves.
const relModel = "m"

// relEnv is a gateway in front of the three fakes.
type relEnv struct {
	g       *harness.Gateway
	key     string
	primary *fakeupstream.Server
	claude  *fakeanthropic.Server
	backup  *fakeupstream.Server
}

// relOptions shape a reliability gateway.
type relOptions struct {
	// routing is the YAML of the routing section's body, indented by four
	// spaces.
	routing string
	// maxConcurrency sets a provider's max_concurrency, by id.
	maxConcurrency map[string]int
}

func startReliability(t *testing.T, opts relOptions) *relEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs the gateway; skipped with -short")
	}
	e := &relEnv{
		primary: fakeupstream.New(fakeupstream.Options{APIKey: providerKey}),
		claude:  fakeanthropic.New(fakeanthropic.Options{APIKey: providerKey}),
		backup:  fakeupstream.New(fakeupstream.Options{APIKey: providerKey}),
	}
	t.Cleanup(e.primary.Close)
	t.Cleanup(e.claude.Close)
	t.Cleanup(e.backup.Close)
	key, hash, err := harness.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	e.key = key
	conc := func(id string) int { return opts.maxConcurrency[id] }
	cfg := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 5s}
logging: {level: info, format: json}
providers:
  - {id: primary, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}", max_concurrency: %d, timeouts: {first_token: 5s}}
  - {id: claude, type: anthropic, base_url: %q, api_key: "${PROVIDER_KEY}", max_concurrency: %d, timeouts: {first_token: 5s}}
  - {id: backup, type: openai, base_url: %q, api_key: "${PROVIDER_KEY}", max_concurrency: %d, timeouts: {first_token: 5s}}
models:
  - id: %s
    capabilities: [streaming, stream_usage]
    deployments:
      - {provider: backup, upstream_model: m-backup, pricing: {input: "3", output: "3"}}
      - {provider: primary, upstream_model: m-primary, pricing: {input: "1", output: "1"}}
      - {provider: claude, upstream_model: m-claude, pricing: {input: "2", output: "2"}}
routing:
%s
tenants:
  - {id: %s, keys: [{id: %s, hash: %q}]}
`, e.primary.BaseURL(), conc("primary"), e.claude.BaseURL(), conc("claude"), e.backup.BaseURL(), conc("backup"),
		relModel, opts.routing, harness.TenantID, harness.KeyID, hash)
	e.g = harness.RunYAML(t, []byte(cfg), harness.RunOptions{Race: raceEnabled, Env: []string{"PROVIDER_KEY=" + providerKey}})
	return e
}

// quickRouting is a routing body with quick backoff and a breaker and
// cooldown that stay out of the way; extra lines override its sections.
func quickRouting(retries int, extra ...string) string {
	sections := map[string]string{
		"fallback":            fmt.Sprintf("{retries_per_deployment: %d, backoff_base: 5ms, backoff_max: 10ms}", retries),
		"breaker":             "{consecutive_failures: 1000000, min_requests: 1000000}",
		"rate_limit_cooldown": "{default: 1ns, max: 1ns}",
	}
	var out []string
	for _, x := range extra {
		k, v, _ := strings.Cut(x, ":")
		sections[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	for _, k := range []string{"request_timeout", "fallback", "breaker", "rate_limit_cooldown", "stream_keepalive"} {
		if v, ok := sections[k]; ok {
			out = append(out, "  "+k+": "+v)
		}
	}
	return strings.Join(out, "\n")
}

// counts returns how many requests each fake has received.
func (e *relEnv) counts() string {
	return fmt.Sprintf("primary=%d claude=%d backup=%d", len(e.primary.Requests()), len(e.claude.Requests()), len(e.backup.Requests()))
}

// relBody is a chat request for the model.
func relBody(stream bool) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}],"stream":%v}`, relModel, stream)
}

// call posts body to the chat endpoint with header and returns the reply.
func (e *relEnv) call(t *testing.T, body string, header http.Header) reply {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return e.callCtx(ctx, t, body, header)
}

func (e *relEnv) callCtx(ctx context.Context, t *testing.T, body string, header http.Header) reply {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.g.PublicURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := reply{status: resp.StatusCode, header: resp.Header}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	r.body = string(b)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(strings.NewReader(r.body))
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				r.events = append(r.events, data)
			}
		}
		return r
	}
	if r.status >= 400 {
		var env struct{ Error json.RawMessage }
		if json.Unmarshal(b, &env) == nil {
			_ = json.Unmarshal(env.Error, &r.err)
		}
	}
	return r
}

// relParams is an SDK request for the model.
func relParams() openai.ChatCompletionNewParams {
	p := params()
	p.Model = relModel
	return p
}

// sdk returns an openai-go client for the gateway, without SDK retries.
func (e *relEnv) sdk() *openai.Client {
	c := openai.NewClient(option.WithBaseURL(e.g.PublicURL+"/v1"), option.WithAPIKey(e.key), option.WithMaxRetries(0))
	return &c
}

// requestLines returns the gateway's access-log lines for chat requests.
func (e *relEnv) requestLines() []map[string]any {
	var out []map[string]any
	for _, line := range e.g.Logs() {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "request" && m["path"] == "/v1/chat/completions" {
			out = append(out, m)
		}
	}
	return out
}

// waitRequestLines waits until there are n access-log lines; the line is
// written once the handler returns, just after the client's response.
func (e *relEnv) waitRequestLines(t *testing.T, n int) []map[string]any {
	t.Helper()
	var lines []map[string]any
	waitUntil(t, fmt.Sprintf("%d request lines", n), func() bool {
		lines = e.requestLines()
		return len(lines) >= n
	})
	return lines
}

// adminGet fetches path from the admin listener.
func (e *relEnv) adminGet(t *testing.T, path string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.g.AdminURL+path, nil)
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
	}
	return string(b)
}

// metric returns the value of the series named by a /metrics line prefix
// such as `switchyard_circuit_state{deployment="m@primary"}`, or -1.
func (e *relEnv) metric(t *testing.T, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(e.adminGet(t, "/metrics"), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				t.Fatalf("metric line %q", line)
			}
			return f
		}
	}
	return -1
}

// breakerState returns the breaker state of deployment from the admin
// report.
func (e *relEnv) breakerState(t *testing.T, deployment string) string {
	t.Helper()
	var rep struct {
		Providers []struct {
			Deployments []struct {
				ID      string
				Breaker struct{ State string }
			}
		}
	}
	if err := json.Unmarshal([]byte(e.adminGet(t, "/admin/v1/providers")), &rep); err != nil {
		t.Fatal(err)
	}
	for _, p := range rep.Providers {
		for _, d := range p.Deployments {
			if d.ID == deployment {
				return d.Breaker.State
			}
		}
	}
	return ""
}

// waitUntil polls cond until it holds, failing after callTimeout.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(callTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReliabilityFallbackOn503(t *testing.T) {
	for _, tc := range []struct {
		retries      int
		wantAttempts string
		wantChain    string
		wantCounts   string
	}{
		{1, "3", "m@primary:server_error,m@primary:server_error,m@claude:ok", "primary=2 claude=1 backup=0"},
		{0, "2", "m@primary:server_error,m@claude:ok", "primary=1 claude=1 backup=0"},
	} {
		t.Run(fmt.Sprintf("retries=%d", tc.retries), func(t *testing.T) {
			e := startReliability(t, relOptions{routing: quickRouting(tc.retries)})
			e.primary.Script(fakeupstream.Behavior{Status: http.StatusServiceUnavailable})
			for i, stream := range []bool{false, true} {
				e.primary.Reset()
				e.primary.Script(fakeupstream.Behavior{Status: http.StatusServiceUnavailable})
				e.claude.Reset()
				r := e.call(t, relBody(stream), nil)
				if r.status != http.StatusOK {
					t.Fatalf("stream=%v: status %d, body %s", stream, r.status, r.body)
				}
				h := r.header
				if h.Get("X-Switchyard-Attempts") != tc.wantAttempts || h.Get("X-Switchyard-Route-Reason") != "fallback:server_error" ||
					h.Get("X-Switchyard-Provider") != "claude" || !strings.HasPrefix(h.Get("X-Switchyard-Upstream-Request-Id"), "req_fake_") {
					t.Errorf("stream=%v: headers %v", stream, h)
				}
				if got := e.counts(); got != tc.wantCounts {
					t.Errorf("stream=%v: upstream requests %s, want %s", stream, got, tc.wantCounts)
				}
				if stream && r.events[len(r.events)-1] != "[DONE]" {
					t.Errorf("events %v", r.events)
				}
				line := e.waitRequestLines(t, i+1)[i]
				if fmt.Sprint(line["attempts"]) != tc.wantAttempts || line["attempt_chain"] != tc.wantChain || line["fallback_reason"] != "server_error" ||
					line["deployment"] != "m@claude" || line["status"] != float64(200) {
					t.Errorf("request line %v", line)
				}
				t.Logf("stream=%v: attempts %s, chain %v", stream, h.Get("X-Switchyard-Attempts"), line["attempt_chain"])
			}
			// The SDK sees an ordinary success.
			e.primary.Reset()
			e.primary.Script(fakeupstream.Behavior{Status: http.StatusServiceUnavailable})
			ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
			defer cancel()
			c, err := e.sdk().Chat.Completions.New(ctx, relParams())
			if err != nil || len(c.Choices) != 1 || c.Choices[0].Message.Content == "" {
				t.Fatalf("SDK: %v, %+v", err, c)
			}
			metrics := e.adminGet(t, "/metrics")
			for _, want := range []string{
				`switchyard_fallbacks_total{from="m@primary",kind="server_error",to="m@claude"} 3`,
				`switchyard_upstream_requests_total{model="m",outcome="server_error",provider="primary"}`,
			} {
				if !strings.Contains(metrics, want) {
					t.Errorf("/metrics has no %s", want)
				}
			}
			if tc.retries > 0 && !strings.Contains(metrics, `switchyard_retries_total{deployment="m@primary",kind="server_error"} 3`) {
				t.Error("/metrics has no retries_total for m@primary")
			}
		})
	}
}

func TestReliabilityRetryAfter(t *testing.T) {
	t.Run("retried after Retry-After when the deadline allows", func(t *testing.T) {
		e := startReliability(t, relOptions{routing: quickRouting(1)})
		e.primary.Sequence(fakeupstream.RateLimited("1"))
		start := time.Now()
		r := e.call(t, relBody(false), nil)
		elapsed := time.Since(start)
		if r.status != 200 || r.header.Get("X-Switchyard-Provider") != "primary" || r.header.Get("X-Switchyard-Attempts") != "2" {
			t.Fatalf("status %d, headers %v", r.status, r.header)
		}
		reqs := e.primary.Requests()
		if len(reqs) != 2 || e.counts() != "primary=2 claude=0 backup=0" {
			t.Fatalf("upstream requests %s", e.counts())
		}
		if gap := reqs[1].ReceivedAt.Sub(reqs[0].ReceivedAt); gap < time.Second {
			t.Errorf("retried after %v, before Retry-After: 1", gap)
		}
		t.Logf("served after %v; the retry came %v after the 429", elapsed, reqs[1].ReceivedAt.Sub(reqs[0].ReceivedAt))
	})
	t.Run("the next candidate at once when the deadline does not allow", func(t *testing.T) {
		e := startReliability(t, relOptions{routing: quickRouting(1)})
		e.primary.Sequence(fakeupstream.RateLimited("1"))
		start := time.Now()
		r := e.call(t, relBody(false), http.Header{"X-Switchyard-Timeout-Ms": {"900"}})
		elapsed := time.Since(start)
		if r.status != 200 || r.header.Get("X-Switchyard-Provider") != "claude" ||
			r.header.Get("X-Switchyard-Route-Reason") != "fallback:rate_limited" || e.counts() != "primary=1 claude=1 backup=0" {
			t.Fatalf("status %d, headers %v, upstream requests %s", r.status, r.header, e.counts())
		}
		if gap := e.claude.Requests()[0].ReceivedAt.Sub(e.primary.Requests()[0].ReceivedAt); gap > 300*time.Millisecond {
			t.Errorf("the next candidate came %v after the 429", gap)
		}
		t.Logf("served by the next candidate after %v", elapsed)
	})
}

func TestReliabilityNoRetryOrFallback(t *testing.T) {
	e := startReliability(t, relOptions{routing: quickRouting(1)})
	cases := []struct {
		name     string
		behavior fakeupstream.Behavior
		status   int
		code     string
	}{
		{"400", fakeupstream.APIError(400, "invalid_request_error", "invalid_value", "temperature", "Invalid 'temperature'."), 400, "invalid_value"},
		{"422", fakeupstream.APIError(422, "invalid_request_error", "unprocessable", "", "Unprocessable."), 422, "unprocessable"},
		{"content_policy_violation", fakeupstream.APIError(400, "invalid_request_error", "content_policy_violation", "", "Rejected by the safety system."), 400, "content_policy_violation"},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				e.primary.Reset()
				e.claude.Reset()
				e.backup.Reset()
				e.primary.Script(tc.behavior)
				ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
				defer cancel()
				var err error
				if stream {
					s := e.sdk().Chat.Completions.NewStreaming(ctx, relParams())
					for s.Next() {
					}
					err = s.Err()
				} else {
					_, err = e.sdk().Chat.Completions.New(ctx, relParams())
				}
				var aerr *openai.Error
				if !errors.As(err, &aerr) || aerr.StatusCode != tc.status || aerr.Code != tc.code {
					t.Fatalf("error %v, want %d %s", err, tc.status, tc.code)
				}
				if got := e.counts(); got != "primary=1 claude=0 backup=0" {
					t.Errorf("upstream requests %s, want exactly one", got)
				}
				if aerr.Response.Header.Get("X-Switchyard-Attempts") != "1" {
					t.Errorf("X-Switchyard-Attempts %q", aerr.Response.Header.Get("X-Switchyard-Attempts"))
				}
			})
		}
	}
	t.Run("canceled", func(t *testing.T) {
		e.primary.Reset()
		e.claude.Reset()
		e.backup.Reset()
		e.primary.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultHang})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.g.PublicURL+"/v1/chat/completions", strings.NewReader(relBody(false)))
			req.Header.Set("Authorization", "Bearer "+e.key)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}()
		wctx, wcancel := context.WithTimeout(context.Background(), callTimeout)
		defer wcancel()
		if err := e.primary.WaitReceived(wctx, 1); err != nil {
			t.Fatal(err)
		}
		cancel()
		<-done
		reqs, err := e.primary.Await(wctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if reqs[0].ClientGoneAt.IsZero() {
			t.Error("the upstream request was not canceled")
		}
		// The gateway has finished with the request once its line is
		// logged; nothing else may have been tried.
		waitUntil(t, "the canceled request's line", func() bool {
			for _, l := range e.requestLines() {
				if l["error_code"] == "canceled" {
					return true
				}
			}
			return false
		})
		if got := e.counts(); got != "primary=1 claude=0 backup=0" {
			t.Errorf("upstream requests %s, want exactly one", got)
		}
	})
}

func TestReliabilityRefusalsStayPut(t *testing.T) {
	e := startReliability(t, relOptions{routing: quickRouting(1)})
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("OpenAI content_filter/stream=%v", stream), func(t *testing.T) {
			e.primary.Reset()
			e.claude.Reset()
			e.backup.Reset()
			e.primary.Script(fakeupstream.Behavior{FinishReason: "content_filter"})
			finish := sdkFinish(t, e, stream)
			if finish != "content_filter" || e.counts() != "primary=1 claude=0 backup=0" {
				t.Errorf("finish_reason %q, upstream requests %s", finish, e.counts())
			}
		})
		t.Run(fmt.Sprintf("Anthropic refusal/stream=%v", stream), func(t *testing.T) {
			e.primary.Reset()
			e.claude.Reset()
			e.backup.Reset()
			// The primary fails, so the request reaches claude, which
			// refuses: the refusal is the answer, never shopped around.
			e.primary.Script(fakeupstream.Behavior{Status: http.StatusBadGateway})
			e.claude.Script(fakeanthropic.Behavior{StopReason: "refusal"})
			finish := sdkFinish(t, e, stream)
			if finish != "content_filter" || e.counts() != "primary=2 claude=1 backup=0" {
				t.Errorf("finish_reason %q, upstream requests %s", finish, e.counts())
			}
		})
	}
}

// sdkFinish makes one SDK call and returns its finish reason.
func sdkFinish(t *testing.T, e *relEnv, stream bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	if !stream {
		c, err := e.sdk().Chat.Completions.New(ctx, relParams())
		if err != nil || len(c.Choices) == 0 {
			t.Fatalf("SDK: %v", err)
		}
		return c.Choices[0].FinishReason
	}
	s := e.sdk().Chat.Completions.NewStreaming(ctx, relParams())
	finish := ""
	for s.Next() {
		for _, ch := range s.Current().Choices {
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("SDK stream: %v", err)
	}
	return finish
}

func TestReliabilityCommitPoint(t *testing.T) {
	e := startReliability(t, relOptions{routing: quickRouting(1)})
	t.Run("a mid-stream drop is an error the SDK raises, after one upstream call", func(t *testing.T) {
		e.primary.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultDrop, FaultAfterChunks: 2})
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		s := e.sdk().Chat.Completions.NewStreaming(ctx, relParams())
		chunks := 0
		for s.Next() {
			chunks++
		}
		checkStreamErrorEvent(t, s.Err())
		if chunks != 2 {
			t.Errorf("%d chunks before the error, want 2", chunks)
		}
		if got := e.counts(); got != "primary=1 claude=0 backup=0" {
			t.Errorf("upstream requests %s, want exactly one", got)
		}
	})
	t.Run("a failure before the first event falls back to a clean stream", func(t *testing.T) {
		e.primary.Reset()
		e.claude.Reset()
		e.primary.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultDrop})
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		s := e.sdk().Chat.Completions.NewStreaming(ctx, relParams())
		var acc openai.ChatCompletionAccumulator
		for s.Next() {
			acc.AddChunk(s.Current())
		}
		if err := s.Err(); err != nil {
			t.Fatalf("stream error %v", err)
		}
		if len(acc.Choices) != 1 || acc.Choices[0].Message.Content != fakeupstream.Text {
			t.Errorf("accumulated %+v", acc.Choices)
		}
		if got := e.counts(); got != "primary=2 claude=1 backup=0" {
			t.Errorf("upstream requests %s", got)
		}
	})
}

func TestReliabilityBreaker(t *testing.T) {
	e := startReliability(t, relOptions{routing: quickRouting(0,
		"breaker: {consecutive_failures: 3, open_for: 1s, half_open_probes: 1}")})
	e.primary.Script(fakeupstream.Behavior{Status: http.StatusInternalServerError})
	for i := range 3 {
		if r := e.call(t, relBody(false), nil); r.status != 200 || r.header.Get("X-Switchyard-Provider") != "claude" {
			t.Fatalf("request %d: status %d, provider %q", i, r.status, r.header.Get("X-Switchyard-Provider"))
		}
	}
	if got := e.breakerState(t, "m@primary"); got != "open" {
		t.Fatalf("breaker %q after 3 failures, want open", got)
	}
	if got := e.metric(t, `switchyard_circuit_state{deployment="m@primary"}`); got != 2 {
		t.Errorf("circuit_state = %v, want 2", got)
	}
	// Traffic shifts: the primary is skipped, not tried.
	before := len(e.primary.Requests())
	for range 3 {
		r := e.call(t, relBody(false), nil)
		if r.status != 200 || r.header.Get("X-Switchyard-Provider") != "claude" || r.header.Get("X-Switchyard-Attempts") != "1" ||
			r.header.Get("X-Switchyard-Route-Reason") != "cheapest_eligible" {
			t.Fatalf("with the breaker open: status %d, headers %v", r.status, r.header)
		}
	}
	if n := len(e.primary.Requests()); n != before {
		t.Errorf("the primary got %d requests while its breaker was open", n-before)
	}
	// The primary recovers; after open_for a probe restores it.
	e.primary.Script(fakeupstream.Behavior{})
	waitUntil(t, "the breaker to go half-open", func() bool { return e.breakerState(t, "m@primary") == "half_open" })
	r := e.call(t, relBody(false), nil)
	if r.status != 200 || r.header.Get("X-Switchyard-Provider") != "primary" {
		t.Fatalf("probe: status %d, provider %q", r.status, r.header.Get("X-Switchyard-Provider"))
	}
	if got := e.breakerState(t, "m@primary"); got != "closed" {
		t.Errorf("breaker %q after a successful probe, want closed", got)
	}
	if got := e.metric(t, `switchyard_circuit_state{deployment="m@primary"}`); got != 0 {
		t.Errorf("circuit_state = %v, want 0", got)
	}
	for _, to := range []string{"open", "half_open", "closed"} {
		if got := e.metric(t, `switchyard_circuit_transitions_total{deployment="m@primary",to="`+to+`"}`); got != 1 {
			t.Errorf("circuit_transitions_total{to=%s} = %v", to, got)
		}
	}
}

func TestReliabilityDeadline(t *testing.T) {
	// The gateway's own maximum is 1.5 s; a header can lower it.
	e := startReliability(t, relOptions{routing: quickRouting(1, "request_timeout: 1500ms",
		"fallback: {retries_per_deployment: 2, backoff_base: 150ms, backoff_max: 150ms}")})
	const slack = 50 * time.Millisecond
	tests := []struct {
		name     string
		header   string
		deadline time.Duration
		stream   bool
		setup    func()
	}{
		{name: "hung upstreams", header: "300", deadline: 300 * time.Millisecond, setup: func() {
			e.primary.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultHang})
			e.claude.Script(fakeanthropic.Behavior{Fault: fakeanthropic.FaultHang})
			e.backup.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultHang})
		}},
		{name: "hung stream", header: "300", deadline: 300 * time.Millisecond, stream: true, setup: func() {
			e.primary.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultHang})
			e.claude.Script(fakeanthropic.Behavior{Fault: fakeanthropic.FaultHang})
			e.backup.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultHang})
		}},
		{name: "retries and backoff", header: "400", deadline: 400 * time.Millisecond, setup: func() {
			e.primary.Script(fakeupstream.Behavior{Status: 503, TTFT: 60 * time.Millisecond})
			e.claude.Script(fakeanthropic.Behavior{Status: 529, TTFT: 60 * time.Millisecond})
			// The last candidate hangs, so the deadline, not the plan
			// running out, ends the request.
			e.backup.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultHang})
		}},
		{name: "a header cannot raise the deadline", header: "60000", deadline: 1500 * time.Millisecond, setup: func() {
			e.primary.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultHang})
			e.claude.Script(fakeanthropic.Behavior{Fault: fakeanthropic.FaultHang})
			e.backup.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultHang})
		}},
		{name: "without a header", deadline: 1500 * time.Millisecond, stream: true, setup: func() {
			e.primary.Script(fakeupstream.Behavior{Status: 500})
			e.claude.Script(fakeanthropic.Behavior{Status: 500})
			e.backup.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultHang})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.primary.Reset()
			e.claude.Reset()
			e.backup.Reset()
			tt.setup()
			h := http.Header{}
			if tt.header != "" {
				h.Set("X-Switchyard-Timeout-Ms", tt.header)
			}
			start := time.Now()
			r := e.call(t, relBody(tt.stream), h)
			elapsed := time.Since(start)
			if r.status != http.StatusGatewayTimeout || str(r.err.Code) != "timeout" {
				t.Errorf("status %d, body %s", r.status, r.body)
			}
			if elapsed > tt.deadline+slack {
				t.Errorf("took %v, more than %v past the %v deadline", elapsed, slack, tt.deadline)
			}
			if elapsed < tt.deadline-slack {
				t.Errorf("took %v, well before the %v deadline", elapsed, tt.deadline)
			}
			t.Logf("deadline %v: answered after %v (%s)", tt.deadline, elapsed, e.counts())
		})
	}
}

func TestReliabilityRetryBudget(t *testing.T) {
	const (
		clients  = 10
		perEach  = 3
		ratio    = 0.2
		minRetry = 5
		retries  = 5
	)
	e := startReliability(t, relOptions{routing: quickRouting(retries,
		fmt.Sprintf("fallback: {retries_per_deployment: %d, backoff_base: 1ms, backoff_max: 2ms, retry_budget_ratio: %v, retry_budget_min: %d}", retries, ratio, minRetry))})
	// A full outage: every deployment fails.
	e.primary.Script(fakeupstream.Behavior{Status: 503})
	e.claude.Script(fakeanthropic.Behavior{Status: 529})
	e.backup.Script(fakeupstream.Behavior{Status: 500})
	var wg sync.WaitGroup
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perEach {
				if r := e.call(t, relBody(false), nil); r.status != http.StatusBadGateway {
					t.Errorf("status %d, body %s", r.status, r.body)
				}
			}
		}()
	}
	wg.Wait()
	requests := clients * perEach
	// Every request makes one first attempt on each deployment; retries
	// may add at most ratio × requests + min to each.
	limit := int(float64(requests)*(1+ratio)) + minRetry
	total := 0
	for name, n := range map[string]int{
		"primary": len(e.primary.Requests()), "claude": len(e.claude.Requests()), "backup": len(e.backup.Requests()),
	} {
		total += n
		if n > limit {
			t.Errorf("%s got %d upstream attempts for %d requests, more than the budget's %d", name, n, requests, limit)
		}
		if n < requests {
			t.Errorf("%s got %d upstream attempts, fewer than the %d requests", name, n, requests)
		}
	}
	unbounded := 3 * requests * (1 + retries)
	t.Logf("%d requests made %d upstream attempts in all (%s); at most %d per deployment allowed, %d without a budget",
		requests, total, e.counts(), limit, unbounded)
	if got := e.metric(t, `switchyard_retry_budget_exhausted_total{deployment="m@primary"}`); got <= 0 {
		t.Errorf("retry_budget_exhausted_total{m@primary} = %v", got)
	}
}

func TestReliabilitySpillover(t *testing.T) {
	e := startReliability(t, relOptions{
		routing:        quickRouting(1),
		maxConcurrency: map[string]int{"primary": 2, "claude": 1, "backup": 1},
	})
	release := make(chan struct{})
	e.primary.Script(fakeupstream.Behavior{Hold: release})
	e.claude.Script(fakeanthropic.Behavior{Hold: release})
	e.backup.Script(fakeupstream.Behavior{Hold: release})

	type result struct {
		status   int
		provider string
	}
	results := make(chan result, 4)
	start := func() {
		go func() {
			r := e.call(t, relBody(false), nil)
			results <- result{r.status, r.header.Get("X-Switchyard-Provider")}
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	start()
	start()
	if err := e.primary.WaitReceived(ctx, 2); err != nil {
		t.Fatal(err)
	}
	// The self-hosted primary is at max_concurrency: the next request
	// spills over to the next cheapest provider, then the one after.
	start()
	if err := e.claude.WaitReceived(ctx, 1); err != nil {
		t.Fatal(err)
	}
	start()
	if err := e.backup.WaitReceived(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := e.metric(t, `switchyard_provider_in_flight{provider="primary"}`); got != 2 {
		t.Errorf("provider_in_flight{primary} = %v, want 2", got)
	}
	// Every provider is saturated.
	r := e.call(t, relBody(false), nil)
	if r.status != http.StatusServiceUnavailable || str(r.err.Code) != "no_healthy_upstream" ||
		!strings.Contains(r.err.Message, "m@primary (provider at max_concurrency 2)") {
		t.Fatalf("all saturated: status %d, body %s", r.status, r.body)
	}
	t.Logf("all saturated: %s", r.err.Message)
	close(release)
	served := map[string]int{}
	for range 4 {
		res := <-results
		if res.status != 200 {
			t.Errorf("status %d", res.status)
		}
		served[res.provider]++
	}
	if served["primary"] != 2 || served["claude"] != 1 || served["backup"] != 1 {
		t.Errorf("served %v", served)
	}
	if got := e.counts(); got != "primary=2 claude=1 backup=1" {
		t.Errorf("upstream requests %s", got)
	}
	for _, p := range []string{"primary", "claude", "backup"} {
		waitUntil(t, "in-flight "+p+" to return to 0", func() bool {
			return e.metric(t, `switchyard_provider_in_flight{provider="`+p+`"}`) == 0
		})
	}
	if got := e.metric(t, `switchyard_provider_saturated_total{provider="primary"}`); got < 2 {
		t.Errorf("provider_saturated_total{primary} = %v", got)
	}
}

func TestReliabilityAdminAndMetrics(t *testing.T) {
	e := startReliability(t, relOptions{
		routing:        quickRouting(0, "rate_limit_cooldown: {default: 1s, max: 30s}"),
		maxConcurrency: map[string]int{"primary": 8},
	})
	e.primary.Script(fakeupstream.RateLimited("2"))
	if r := e.call(t, relBody(false), nil); r.status != 200 || r.header.Get("X-Switchyard-Provider") != "claude" {
		t.Fatalf("status %d, headers %v", r.status, r.header)
	}
	body := e.adminGet(t, "/admin/v1/providers")
	if strings.Contains(body, providerKey) || strings.Contains(body, e.key) {
		t.Fatalf("the provider report contains a key: %s", body)
	}
	var rep struct {
		GeneratedAt string `json:"generated_at"`
		Providers   []struct {
			ID             string `json:"id"`
			InFlight       int64  `json:"in_flight"`
			MaxConcurrency int    `json:"max_concurrency"`
			Deployments    []struct {
				ID      string `json:"id"`
				Breaker struct {
					State          string  `json:"state"`
					NextTransition *string `json:"next_transition"`
				} `json:"breaker"`
				CooldownUntil *string `json:"cooldown_until"`
				Recent        struct {
					Requests, Failures int64
				} `json:"recent"`
				RetryBudget struct {
					Requests, Used, Allowed int64
				} `json:"retry_budget"`
				Latency struct {
					TTFTMs      float64 `json:"ttft_ms"`
					TTFTSamples int64   `json:"ttft_samples"`
					TPSSamples  int64   `json:"tokens_per_sec_samples"`
				} `json:"latency"`
			} `json:"deployments"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for i, p := range rep.Providers {
		byID[p.ID] = i
	}
	if len(rep.Providers) != 3 || rep.GeneratedAt == "" {
		t.Fatalf("report %s", body)
	}
	primary := rep.Providers[byID["primary"]]
	if primary.MaxConcurrency != 8 || primary.InFlight != 0 || len(primary.Deployments) != 1 {
		t.Errorf("primary %+v", primary)
	}
	if d := primary.Deployments[0]; d.ID != "m@primary" || d.CooldownUntil == nil || d.Breaker.State != "closed" ||
		d.RetryBudget.Requests != 1 || d.RetryBudget.Allowed < 1 {
		t.Errorf("m@primary %+v", d)
	}
	if d := rep.Providers[byID["claude"]].Deployments[0]; d.Recent.Requests != 1 || d.Latency.TTFTSamples != 1 || d.Latency.TTFTMs <= 0 {
		t.Errorf("m@claude %+v", d)
	}
	metrics := e.adminGet(t, "/metrics")
	for _, want := range []string{
		`switchyard_rate_limit_cooldowns_total{deployment="m@primary"} 1`,
		`switchyard_fallbacks_total{from="m@primary",kind="rate_limited",to="m@claude"} 1`,
		`switchyard_circuit_state{deployment="m@backup"} 0`,
		`switchyard_provider_in_flight{provider="backup"} 0`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics has no %s", want)
		}
	}
	if strings.Contains(metrics, providerKey) {
		t.Error("/metrics contains the provider key")
	}
}

// keepAliveLine matches an SSE keep-alive comment.
var keepAliveLine = regexp.MustCompile(`(?m)^: keep-alive$`)

func TestReliabilityKeepalive(t *testing.T) {
	e := startReliability(t, relOptions{routing: quickRouting(1, "stream_keepalive: 50ms")})
	e.primary.Script(fakeupstream.Behavior{TTFT: 300 * time.Millisecond})
	t.Run("comments are sent until the first event", func(t *testing.T) {
		r := e.call(t, relBody(true), nil)
		n := len(keepAliveLine.FindAllString(r.body, -1))
		if r.status != 200 || n < 3 || !strings.HasPrefix(r.body, ": keep-alive\n") {
			t.Fatalf("status %d, %d keep-alives, body %q", r.status, n, r.body)
		}
		if last := r.events[len(r.events)-1]; last != "[DONE]" {
			t.Errorf("events %v", r.events)
		}
		t.Logf("%d keep-alives before the first event", n)
	})
	t.Run("the SDK ignores them", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		s := e.sdk().Chat.Completions.NewStreaming(ctx, relParams())
		var acc openai.ChatCompletionAccumulator
		for s.Next() {
			acc.AddChunk(s.Current())
		}
		if err := s.Err(); err != nil {
			t.Fatal(err)
		}
		if len(acc.Choices) != 1 || acc.Choices[0].Message.Content != fakeupstream.Text {
			t.Errorf("accumulated %+v", acc.Choices)
		}
	})
	t.Run("after a keep-alive a failure is an error event, without fallback", func(t *testing.T) {
		e.primary.Reset()
		e.claude.Reset()
		e.primary.Script(fakeupstream.Behavior{Status: 503, TTFT: 200 * time.Millisecond})
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		s := e.sdk().Chat.Completions.NewStreaming(ctx, relParams())
		for s.Next() {
		}
		checkStreamErrorEvent(t, s.Err())
		if got := e.counts(); got != "primary=1 claude=0 backup=0" {
			t.Errorf("upstream requests %s", got)
		}
	})
}

// checkStreamErrorEvent checks that err is the SDK raising the gateway's
// error event.
func checkStreamErrorEvent(t *testing.T, err error) {
	t.Helper()
	var serr *ssestream.StreamError
	if !errors.As(err, &serr) {
		t.Fatalf("stream error %v, want an error event the SDK raises", err)
	}
	var ev struct {
		Error struct{ Message, Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal(serr.Event.Data, &ev); err != nil || ev.Error.Type != "server_error" || ev.Error.Code != "upstream_error" {
		t.Errorf("error event %s", serr.Event.Data)
	}
}
