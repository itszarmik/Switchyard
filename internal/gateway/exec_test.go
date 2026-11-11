package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/config"
	"switchyard/internal/health"
	"switchyard/internal/providers"
	"switchyard/internal/reqctx"
	"switchyard/internal/schema"
)

// step scripts one upstream call. The zero value succeeds at once.
type step struct {
	// err fails the call before anything arrives.
	err error
	// hang waits for the call's context to end, then fails as the
	// adapters do: timeout on a deadline, canceled otherwise.
	hang bool
	// hold, when set, is waited for (or the context) before answering.
	hold <-chan struct{}
	// delay is waited before answering.
	delay time.Duration
	// body is the non-stream answer; default defaultChat.
	body string
	// chunks is the stream; default defaultChunks.
	chunks []string
	// end is what the stream returns after its chunks; nil means io.EOF.
	end error
}

// scripted is a concurrency-safe adapter of every interface (chat,
// completions, embeddings). Call n gets steps[n], and the last step
// repeats.
type scripted struct {
	name string

	mu       sync.Mutex
	steps    []step
	calls    int
	kinds    []string
	inFlight int
	peak     int
}

func (a *scripted) script(steps ...step) {
	a.mu.Lock()
	a.steps = steps
	a.mu.Unlock()
}

func (a *scripted) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// begin records a call and returns its step, after waiting as scripted.
func (a *scripted) begin(ctx context.Context, kind string) (step, error) {
	a.mu.Lock()
	n := a.calls
	a.calls++
	a.kinds = append(a.kinds, kind)
	var s step
	if len(a.steps) > 0 {
		s = a.steps[min(n, len(a.steps)-1)]
	}
	a.inFlight++
	a.peak = max(a.peak, a.inFlight)
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.inFlight--
		a.mu.Unlock()
	}()
	ctxErr := func() error {
		kind := providers.KindCanceled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			kind = providers.KindTimeout
		}
		return &providers.Error{Kind: kind, Provider: a.name, Message: "context done"}
	}
	if s.hang {
		<-ctx.Done()
		return s, ctxErr()
	}
	if s.hold != nil {
		select {
		case <-s.hold:
		case <-ctx.Done():
			return s, ctxErr()
		}
	}
	if s.delay > 0 {
		t := time.NewTimer(s.delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return s, ctxErr()
		}
	}
	return s, s.err
}

func (a *scripted) response(ctx context.Context, kind, def string) (*providers.Response, error) {
	s, err := a.begin(ctx, kind)
	if err != nil {
		return nil, err
	}
	body := s.body
	if body == "" {
		body = def
	}
	var u struct{ Usage *schema.Usage }
	_ = json.Unmarshal([]byte(body), &u)
	return &providers.Response{Status: 200, Body: []byte(body), UpstreamRequestID: a.name + "-rid", TTFT: time.Millisecond, Usage: u.Usage}, nil
}

func (a *scripted) stream(ctx context.Context, kind string) (providers.Stream, error) {
	s, err := a.begin(ctx, kind)
	if err != nil {
		return nil, err
	}
	chunks := s.chunks
	if chunks == nil {
		chunks = defaultChunks
	}
	return &endStream{scriptStream: scriptStream{chunks: chunks}, end: s.end, rid: a.name + "-rid"}, nil
}

func (a *scripted) Chat(ctx context.Context, _ *providers.Request) (*providers.Response, error) {
	return a.response(ctx, "chat", defaultChat)
}

func (a *scripted) ChatStream(ctx context.Context, _ *providers.Request) (providers.Stream, error) {
	return a.stream(ctx, "chat-stream")
}

func (a *scripted) Completion(ctx context.Context, _ *providers.CompletionRequest) (*providers.Response, error) {
	return a.response(ctx, "completion", `{"object":"text_completion","native":true}`)
}

func (a *scripted) CompletionStream(ctx context.Context, _ *providers.CompletionRequest) (providers.Stream, error) {
	return a.stream(ctx, "completion-stream")
}

func (a *scripted) Embed(ctx context.Context, _ *providers.EmbedRequest) (*providers.Response, error) {
	return a.response(ctx, "embed", `{"object":"list","usage":{"prompt_tokens":2,"total_tokens":2}}`)
}

// endStream is a scriptStream that ends with end instead of io.EOF.
type endStream struct {
	scriptStream
	end error
	rid string
}

func (s *endStream) Next() (*schema.ChatChunk, error) {
	c, err := s.scriptStream.Next()
	if errors.Is(err, io.EOF) && s.end != nil {
		return nil, s.end
	}
	return c, err
}

func (s *endStream) UpstreamRequestID() string { return s.rid }

// Upstream failures the tests script.
var (
	err503        = &providers.Error{Kind: providers.KindServerError, Status: 503, Message: "upstream returned HTTP 503"}
	errTransport  = &providers.Error{Kind: providers.KindTransport, Message: "connection to the upstream failed"}
	errAuth       = &providers.Error{Kind: providers.KindAuth, Status: 401, Message: "upstream rejected the provider credential"}
	errNotFound   = &providers.Error{Kind: providers.KindNotFound, Status: 404, Body: []byte(`{"error":{"message":"no model","type":"invalid_request_error"}}`)}
	errContextLen = &providers.Error{Kind: providers.KindContextLength, Status: 400, Body: []byte(`{"error":{"message":"too long","type":"invalid_request_error","code":"context_length_exceeded"}}`)}
	err400        = &providers.Error{Kind: providers.KindClientError, Status: 400, Body: []byte(`{"error":{"message":"bad","type":"invalid_request_error"}}`)}
	err422        = &providers.Error{Kind: providers.KindClientError, Status: 422, Body: []byte(`{"error":{"message":"unprocessable","type":"invalid_request_error"}}`)}
	errFilter     = &providers.Error{Kind: providers.KindContentFilter, Status: 400, Body: []byte(`{"error":{"message":"rejected","type":"invalid_request_error","code":"content_policy_violation"}}`)}
	errCanceled   = &providers.Error{Kind: providers.KindCanceled, Message: "canceled"}
)

func rateLimited(retryAfter time.Duration) *providers.Error {
	return &providers.Error{Kind: providers.KindRateLimited, Status: 429, RetryAfter: retryAfter,
		Body: []byte(`{"error":{"message":"slow down","type":"requests","code":"rate_limit_exceeded"}}`)}
}

// execEnv is a gateway over one model, "m", on providers a (cheapest),
// b and c (priciest), all scripted.
type execEnv struct {
	g       *Gateway
	reg     *prometheus.Registry
	logs    *logBuffer
	a, b, c *scripted
}

// fastRouting is the routing the tests start from: one retry, quick
// backoff, a breaker that does not get in the way.
func fastRouting() config.Routing {
	r := config.DefaultRouting()
	r.Fallback.BackoffBase = config.Duration(time.Millisecond)
	r.Fallback.BackoffMax = config.Duration(2 * time.Millisecond)
	r.Breaker.ConsecutiveFailures = 1000
	r.Breaker.MinRequests = 1000
	return r
}

func newExecEnv(t *testing.T, routing config.Routing, edit func(*config.Config)) *execEnv {
	t.Helper()
	return newExecEnvClock(t, routing, edit, nil)
}

// newExecEnvClock is newExecEnv with the health tracker's clock.
func newExecEnvClock(t *testing.T, routing config.Routing, edit func(*config.Config), now func() time.Time) *execEnv {
	t.Helper()
	cfg := &config.Config{
		Providers: []config.Provider{
			{ID: "a", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"},
			{ID: "b", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"},
			{ID: "c", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"},
		},
		Models: []config.Model{
			{ID: "m", Capabilities: []string{"streaming", "stream_usage", "completions"}, Deployments: []config.Deployment{
				{Provider: "c", UpstreamModel: "m-c", Pricing: price("3", "3")},
				{Provider: "a", UpstreamModel: "m-a", Pricing: price("1", "1")},
				{Provider: "b", UpstreamModel: "m-b", Pricing: price("2", "2"), Capabilities: []string{"streaming", "stream_usage"}},
			}},
			{ID: "e", Type: "embedding", Deployments: []config.Deployment{
				{Provider: "a", UpstreamModel: "e-a", Pricing: price("1", "0")},
				{Provider: "b", UpstreamModel: "e-b", Pricing: price("2", "0")},
			}},
		},
		Routing: routing,
	}
	if edit != nil {
		edit(cfg)
	}
	logs := &logBuffer{}
	reg := prometheus.NewRegistry()
	g, err := New(Options{Config: cfg, Logger: slog.New(slog.NewJSONHandler(logs, nil)), Registry: reg, Clock: now})
	if err != nil {
		t.Fatal(err)
	}
	e := &execEnv{g: g, reg: reg, logs: logs, a: &scripted{name: "a"}, b: &scripted{name: "b"}, c: &scripted{name: "c"}}
	g.adapters["a"], g.adapters["b"], g.adapters["c"] = e.a, e.b, e.c
	return e
}

// hdrExchange is a recExchange with request headers, that also records
// keep-alive comments among the events.
type hdrExchange struct {
	recExchange
	header http.Header
}

func (x *hdrExchange) Header() http.Header {
	if x.header == nil {
		return http.Header{}
	}
	return x.header
}

func (x *hdrExchange) Comment(text string) error {
	x.res.events = append(x.res.events, ": "+text)
	return nil
}

// do serves body on endpoint with header and returns what the client saw.
func (e *execEnv) do(endpoint, body string, header http.Header) exchangeResult {
	ann := &reqctx.Annotations{}
	ctx := reqctx.WithAnnotations(reqctx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(e.logs, nil))), ann)
	x := &hdrExchange{recExchange: recExchange{body: []byte(body)}, header: header}
	switch endpoint {
	case EndpointCompletions:
		e.g.Completions(ctx, x)
	case EndpointEmbeddings:
		e.g.Embeddings(ctx, x)
	default:
		e.g.ChatCompletions(ctx, x)
	}
	x.res.ann = annotations(ann)
	return x.res
}

func (e *execEnv) chat(stream bool) exchangeResult {
	return e.do(EndpointChatCompletions, fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":%v}`, stream), nil)
}

func (e *execEnv) calls() string {
	return fmt.Sprintf("a=%d b=%d c=%d", e.a.count(), e.b.count(), e.c.count())
}

func TestFallbackOnServerError(t *testing.T) {
	for _, tc := range []struct {
		retries      int
		wantCalls    string
		wantAttempts int
		wantChain    string
	}{
		{1, "a=2 b=1 c=0", 3, "m@a:server_error,m@a:server_error,m@b:ok"},
		{0, "a=1 b=1 c=0", 2, "m@a:server_error,m@b:ok"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("retries=%d/stream=%v", tc.retries, stream), func(t *testing.T) {
				r := fastRouting()
				r.Fallback.RetriesPerDeployment = tc.retries
				e := newExecEnv(t, r, nil)
				e.a.script(step{err: err503})
				res := e.chat(stream)
				if res.status != 200 || res.err != nil {
					t.Fatalf("status %d, error %+v", res.status, res.err)
				}
				if got := e.calls(); got != tc.wantCalls {
					t.Errorf("calls %s, want %s", got, tc.wantCalls)
				}
				if res.info.Provider != "b" || res.info.Attempts != tc.wantAttempts || res.info.RouteReason != "fallback:server_error" ||
					res.info.UpstreamRequestID != "b-rid" {
					t.Errorf("info %+v", res.info)
				}
				if stream && (len(res.events) != 4 || res.events[3] != "[DONE]") {
					t.Errorf("events %v, want a clean stream", res.events)
				}
				ann := res.ann
				if ann["attempts"] != int64(tc.wantAttempts) || ann["attempt_chain"] != tc.wantChain ||
					ann["fallback_reason"] != "server_error" || ann["deployment"] != "m@b" || ann["error_code"] != "" {
					t.Errorf("annotations %v", ann)
				}
				if got := metricValue(t, e.reg, "switchyard_fallbacks_total", "m@a", "server_error", "m@b"); got != 1 {
					t.Errorf("fallbacks_total = %v", got)
				}
				if got := metricValue(t, e.reg, "switchyard_retries_total", "m@a", "server_error"); got != float64(tc.retries) {
					t.Errorf("retries_total = %v, want %d", got, tc.retries)
				}
				if got := metricValue(t, e.reg, "switchyard_upstream_requests_total", "m", "server_error", "a"); got != float64(tc.retries+1) {
					t.Errorf("upstream_requests_total{a, server_error} = %v", got)
				}
			})
		}
	}
}

func TestCheapestAndPinnedRouteReasons(t *testing.T) {
	e := newExecEnv(t, fastRouting(), nil)
	res := e.chat(false)
	if res.info.Provider != "a" || res.info.Attempts != 1 || res.info.RouteReason != "cheapest_eligible" {
		t.Errorf("info %+v", res.info)
	}
	if res.ann["attempt_chain"] != "m@a:ok" {
		t.Errorf("annotations %v", res.ann)
	}
	if _, ok := res.ann["fallback_reason"]; ok {
		t.Errorf("fallback_reason logged without a fallback: %v", res.ann)
	}
	// A pinned deployment is a plan of one: no fallback.
	e.c.script(step{err: err503})
	res = e.do(EndpointChatCompletions, `{"model":"m@c","messages":[{"role":"user","content":"hi"}]}`, nil)
	if res.status != 502 || res.info.RouteReason != "pinned" || res.info.Attempts != 2 || e.calls() != "a=1 b=0 c=2" {
		t.Errorf("pinned: status %d, info %+v, calls %s", res.status, res.info, e.calls())
	}
}

func TestNoRetryNoFallback(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{"400", err400, 400, string(err400.Body)},
		{"422", err422, 422, string(err422.Body)},
		{"content_policy_violation", errFilter, 400, string(errFilter.Body)},
		{"canceled", errCanceled, StatusClientClosedRequest, ""},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tt.name, stream), func(t *testing.T) {
				e := newExecEnv(t, fastRouting(), nil)
				e.a.script(step{err: tt.err})
				res := e.chat(stream)
				if res.status != tt.wantStatus {
					t.Fatalf("status %d, want %d", res.status, tt.wantStatus)
				}
				if tt.wantBody != "" && string(res.err.Body) != tt.wantBody {
					t.Errorf("body %s, want the upstream's", res.err.Body)
				}
				if got := e.calls(); got != "a=1 b=0 c=0" {
					t.Errorf("calls %s, want exactly one", got)
				}
				if res.info.Attempts != 1 {
					t.Errorf("attempts %d", res.info.Attempts)
				}
			})
		}
	}
}

func TestFallbackWithoutRetry(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		kind string
	}{
		{"auth", errAuth, "auth"},
		{"not found", errNotFound, "not_found"},
		{"context length", errContextLen, "context_length"},
		{"429 without Retry-After", rateLimited(0), "rate_limited"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newExecEnv(t, fastRouting(), nil)
			e.a.script(step{err: tt.err})
			res := e.chat(false)
			if res.status != 200 || e.calls() != "a=1 b=1 c=0" || res.info.RouteReason != "fallback:"+tt.kind {
				t.Errorf("status %d, calls %s, info %+v", res.status, e.calls(), res.info)
			}
		})
	}
	t.Run("auth is logged at ERROR", func(t *testing.T) {
		e := newExecEnv(t, fastRouting(), nil)
		e.a.script(step{err: errAuth})
		e.chat(false)
		lines := e.logs.find(t, "upstream rejected the provider credential")
		if len(lines) != 1 || lines[0]["level"] != "ERROR" || lines[0]["deployment"] != "m@a" {
			t.Errorf("log lines %v", lines)
		}
	})
}

func TestBestFailureIsAnswered(t *testing.T) {
	tests := []struct {
		name       string
		a, b, c    error
		wantStatus int
		wantCode   string
	}{
		{"every failure a 5xx", err503, err503, errTransport, 502, "upstream_error"},
		{"context length wins", err503, errContextLen, errTransport, 400, ""},
		{"all rate limited", rateLimited(0), rateLimited(0), rateLimited(0), 429, ""},
		{"rate limits and a 5xx", rateLimited(0), err503, rateLimited(0), 502, "upstream_error"},
		{"timeouts", &providers.Error{Kind: providers.KindTimeout, Message: "first token"}, errTransport, errTransport, 504, "timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := fastRouting()
			r.Fallback.RetriesPerDeployment = 0
			e := newExecEnv(t, r, nil)
			e.a.script(step{err: tt.a})
			e.b.script(step{err: tt.b})
			e.c.script(step{err: tt.c})
			res := e.chat(false)
			if res.status != tt.wantStatus || res.err == nil || res.err.Code != tt.wantCode {
				t.Errorf("status %d, error %+v; want %d %q", res.status, res.err, tt.wantStatus, tt.wantCode)
			}
			if e.calls() != "a=1 b=1 c=1" || res.info.Attempts != 3 {
				t.Errorf("calls %s, attempts %d", e.calls(), res.info.Attempts)
			}
		})
	}
}

func TestRefusalIsNotRetried(t *testing.T) {
	refusal := `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":"no"},"finish_reason":"content_filter"}]}`
	refusalChunks := []string{
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","refusal":"no"}}]}`,
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`,
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			e := newExecEnv(t, fastRouting(), nil)
			e.a.script(step{body: refusal, chunks: refusalChunks})
			res := e.chat(stream)
			if res.status != 200 || e.calls() != "a=1 b=0 c=0" || res.info.Attempts != 1 {
				t.Fatalf("status %d, calls %s, attempts %d", res.status, e.calls(), res.info.Attempts)
			}
			if stream && !strings.Contains(strings.Join(res.events, " "), "content_filter") {
				t.Errorf("events %v", res.events)
			}
			if !stream && res.body != refusal {
				t.Errorf("body %s", res.body)
			}
		})
	}
}

func TestCommitPoint(t *testing.T) {
	t.Run("a mid-stream failure is an error event, never retried", func(t *testing.T) {
		e := newExecEnv(t, fastRouting(), nil)
		e.a.script(step{chunks: defaultChunks[:2], end: errTransport})
		res := e.chat(true)
		if e.calls() != "a=1 b=0 c=0" || res.info.Attempts != 1 {
			t.Fatalf("calls %s, attempts %d", e.calls(), res.info.Attempts)
		}
		if len(res.events) != 3 || !strings.Contains(res.events[2], `"error"`) {
			t.Errorf("events %v, want two chunks and an error event", res.events)
		}
		if res.ann["error_code"] != codeUpstreamError || res.ann["attempt_chain"] != "m@a:transport" {
			t.Errorf("annotations %v", res.ann)
		}
	})
	t.Run("a failure before the first event falls back cleanly", func(t *testing.T) {
		e := newExecEnv(t, fastRouting(), nil)
		e.a.script(step{err: &providers.Error{Kind: providers.KindTimeout, Message: "first token"}})
		res := e.chat(true)
		if res.status != 200 || e.calls() != "a=2 b=1 c=0" || res.info.Provider != "b" || res.info.RouteReason != "fallback:timeout" {
			t.Fatalf("status %d, calls %s, info %+v", res.status, e.calls(), res.info)
		}
		if len(res.events) != 4 || res.events[3] != "[DONE]" {
			t.Errorf("events %v", res.events)
		}
	})
}

func TestRetryAfter(t *testing.T) {
	t.Run("retried after Retry-After", func(t *testing.T) {
		e := newExecEnv(t, fastRouting(), nil)
		e.a.script(step{err: rateLimited(150 * time.Millisecond)}, step{})
		start := time.Now()
		res := e.chat(false)
		elapsed := time.Since(start)
		if res.status != 200 || e.calls() != "a=2 b=0 c=0" || res.info.RouteReason != "cheapest_eligible" {
			t.Fatalf("status %d, calls %s, info %+v", res.status, e.calls(), res.info)
		}
		if elapsed < 150*time.Millisecond {
			t.Errorf("retried after %v, before Retry-After", elapsed)
		}
		if got := metricValue(t, e.reg, "switchyard_rate_limit_cooldowns_total", "m@a"); got != 1 {
			t.Errorf("rate_limit_cooldowns_total = %v", got)
		}
		// The overhead excludes the wait.
		if o, _ := res.ann["overhead_ms"].(float64); o >= 100 {
			t.Errorf("overhead_ms %v includes the Retry-After wait", o)
		}
	})
	t.Run("a Retry-After past the deadline falls back at once", func(t *testing.T) {
		e := newExecEnv(t, fastRouting(), nil)
		e.a.script(step{err: rateLimited(time.Second)}, step{})
		start := time.Now()
		res := e.do(EndpointChatCompletions, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			http.Header{HeaderTimeoutMs: {"500"}})
		if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
			t.Errorf("took %v, want the next candidate at once", elapsed)
		}
		if res.status != 200 || e.calls() != "a=1 b=1 c=0" || res.info.RouteReason != "fallback:rate_limited" {
			t.Errorf("status %d, calls %s, info %+v", res.status, e.calls(), res.info)
		}
	})
	t.Run("the cooldown skips the deployment for the next request", func(t *testing.T) {
		e := newExecEnv(t, fastRouting(), nil)
		e.a.script(step{err: rateLimited(10 * time.Second)})
		e.do(EndpointChatCompletions, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, http.Header{HeaderTimeoutMs: {"500"}})
		res := e.chat(false)
		if res.status != 200 || e.calls() != "a=1 b=2 c=0" || res.info.RouteReason != "cheapest_eligible" {
			t.Errorf("status %d, calls %s, info %+v", res.status, e.calls(), res.info)
		}
		if res.ann["excluded"] != int64(1) {
			t.Errorf("annotations %v", res.ann)
		}
	})
}

func TestNoHealthyUpstream(t *testing.T) {
	r := fastRouting()
	r.Fallback.RetriesPerDeployment = 0
	e := newExecEnv(t, r, nil)
	for _, a := range []*scripted{e.a, e.b, e.c} {
		a.script(step{err: rateLimited(10 * time.Second)})
	}
	res := e.chat(false)
	if res.status != 429 || res.err.RetryAfter != 10*time.Second || e.calls() != "a=1 b=1 c=1" {
		t.Fatalf("all rate limited: status %d, error %+v, calls %s", res.status, res.err, e.calls())
	}
	res = e.chat(false)
	if res.status != 503 || res.err.Code != codeNoHealthy || res.info.Attempts != 0 || e.calls() != "a=1 b=1 c=1" {
		t.Fatalf("status %d, error %+v, calls %s", res.status, res.err, e.calls())
	}
	if !strings.Contains(res.err.Message, "m@a (rate-limit cooldown 10s)") {
		t.Errorf("message %q does not name the reasons", res.err.Message)
	}
	// An ineligible request keeps its 400.
	res = e.do(EndpointChatCompletions, `{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if res.status != 400 || res.err.Code != codeNoEligible {
		t.Errorf("ineligible: status %d, error %+v", res.status, res.err)
	}
}

// clock is a manual clock for the health tracker.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestBreakerShiftsTraffic(t *testing.T) {
	r := fastRouting()
	r.Fallback.RetriesPerDeployment = 0
	r.Breaker.ConsecutiveFailures = 2
	r.Breaker.OpenFor = config.Duration(30 * time.Second)
	clk := &clock{now: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	e := newExecEnvClock(t, r, nil, clk.Now)
	reg := e.reg

	e.a.script(step{err: err503})
	for range 2 {
		if res := e.chat(false); res.info.Provider != "b" {
			t.Fatalf("served by %q", res.info.Provider)
		}
	}
	// The breaker is open: a is no longer tried.
	res := e.chat(false)
	if res.info.Provider != "b" || res.info.Attempts != 1 || res.info.RouteReason != "cheapest_eligible" || e.calls() != "a=2 b=3 c=0" {
		t.Fatalf("with the breaker open: info %+v, calls %s", res.info, e.calls())
	}
	if got := metricValue(t, reg, "switchyard_circuit_state", "m@a"); got != 2 {
		t.Errorf("circuit_state = %v, want 2", got)
	}
	// After open_for a probe goes through and, succeeding, closes it.
	e.a.script(step{})
	clk.advance(30 * time.Second)
	if got := metricValue(t, reg, "switchyard_circuit_state", "m@a"); got != 1 {
		t.Errorf("circuit_state after open_for = %v, want 1", got)
	}
	if res := e.chat(false); res.info.Provider != "a" {
		t.Fatalf("probe served by %q", res.info.Provider)
	}
	if got := metricValue(t, reg, "switchyard_circuit_state", "m@a"); got != 0 {
		t.Errorf("circuit_state after the probe = %v, want 0", got)
	}
	for _, to := range []string{"open", "half_open", "closed"} {
		if got := metricValue(t, reg, "switchyard_circuit_transitions_total", "m@a", to); got != 1 {
			t.Errorf("circuit_transitions_total{to=%s} = %v", to, got)
		}
	}
}

func TestDeadline(t *testing.T) {
	const deadline = 150 * time.Millisecond
	tests := []struct {
		name    string
		a, b, c step
		stream  bool
	}{
		{name: "hung upstreams", a: step{hang: true}, b: step{hang: true}, c: step{hang: true}},
		{name: "hung stream", a: step{hang: true}, b: step{hang: true}, c: step{hang: true}, stream: true},
		{name: "slow failures and backoff", a: step{err: err503, delay: 40 * time.Millisecond},
			b: step{err: err503, delay: 40 * time.Millisecond}, c: step{hang: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := fastRouting()
			r.Fallback.BackoffBase = config.Duration(60 * time.Millisecond)
			r.Fallback.BackoffMax = config.Duration(60 * time.Millisecond)
			e := newExecEnv(t, r, nil)
			e.a.script(tt.a)
			e.b.script(tt.b)
			e.c.script(tt.c)
			start := time.Now()
			res := e.do(EndpointChatCompletions, fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":%v}`, tt.stream),
				http.Header{HeaderTimeoutMs: {fmt.Sprint(deadline.Milliseconds())}})
			elapsed := time.Since(start)
			if elapsed > deadline+50*time.Millisecond {
				t.Errorf("took %v, more than 50ms past the %v deadline", elapsed, deadline)
			}
			if res.status != 504 || res.err.Code != codeTimeout {
				t.Errorf("status %d, error %+v", res.status, res.err)
			}
			if res.ann["deadline_ms"] != int64(150) {
				t.Errorf("annotations %v", res.ann)
			}
		})
	}
	t.Run("a deadline timeout does not count toward the breaker", func(t *testing.T) {
		r := fastRouting()
		r.Breaker.ConsecutiveFailures = 1
		r.Fallback.MaxDeployments = 1
		e := newExecEnv(t, r, nil)
		e.a.script(step{hang: true}, step{})
		e.do(EndpointChatCompletions, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, http.Header{HeaderTimeoutMs: {"20"}})
		if res := e.chat(false); res.status != 200 || res.info.Provider != "a" {
			t.Errorf("status %d, info %+v: the breaker opened", res.status, res.info)
		}
	})
}

func TestDeadlineHeader(t *testing.T) {
	r := fastRouting()
	r.RequestTimeout = config.Duration(2 * time.Second)
	e := newExecEnv(t, r, nil)
	for _, tc := range []struct {
		header     string
		wantStatus int
		wantMs     any
	}{
		{"", 200, int64(2000)},
		{"500", 200, int64(500)},
		{" 750 ", 200, int64(750)},
		{"60000", 200, int64(2000)}, // can never raise it
		{"0", 400, nil},
		{"-5", 400, nil},
		{"1.5", 400, nil},
		{"soon", 400, nil},
	} {
		t.Run(fmt.Sprintf("%q", tc.header), func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set(HeaderTimeoutMs, tc.header)
			}
			res := e.do(EndpointChatCompletions, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, h)
			if res.status != tc.wantStatus {
				t.Fatalf("status %d, want %d", res.status, tc.wantStatus)
			}
			if tc.wantStatus == 400 && (res.err.Code != codeInvalidTimeout || res.err.Param != HeaderTimeoutMs) {
				t.Errorf("error %+v", res.err)
			}
			if res.ann["deadline_ms"] != tc.wantMs {
				t.Errorf("deadline_ms %v, want %v", res.ann["deadline_ms"], tc.wantMs)
			}
		})
	}
}

func TestRetryBudget(t *testing.T) {
	r := fastRouting()
	r.Fallback.MaxDeployments = 1
	r.Fallback.RetriesPerDeployment = 5
	r.Fallback.RetryBudgetRatio = 0.2
	r.Fallback.RetryBudgetMin = 3
	e := newExecEnv(t, r, nil)
	e.a.script(step{err: errTransport})
	const requests = 40
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := e.chat(false); res.status != 502 {
				t.Errorf("status %d", res.status)
			}
		}()
	}
	wg.Wait()
	limit := requests + int(0.2*requests) + 3
	if got := e.a.count(); got > limit {
		t.Errorf("%d upstream attempts for %d requests, more than the budget's %d", got, requests, limit)
	}
	if got := metricValue(t, e.reg, "switchyard_retry_budget_exhausted_total", "m@a"); got == 0 {
		t.Error("retry_budget_exhausted_total is 0")
	}
	t.Logf("%d requests made %d upstream attempts (limit %d)", requests, e.a.count(), limit)
}

func TestSpillover(t *testing.T) {
	e := newExecEnv(t, fastRouting(), func(c *config.Config) {
		c.Providers[0].MaxConcurrency = 2 // a
		c.Providers[1].MaxConcurrency = 1 // b
		c.Providers[2].MaxConcurrency = 1 // c
	})
	release := make(chan struct{})
	e.a.script(step{hold: release})
	e.b.script(step{hold: release})
	e.c.script(step{hold: release})

	results := make(chan exchangeResult, 4)
	start := func() { go func() { results <- e.chat(false) }() }
	waitCalls := func(a *scripted, n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for a.count() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%s got %d calls, want %d", a.name, a.count(), n)
			}
			time.Sleep(time.Millisecond)
		}
	}
	start()
	start()
	waitCalls(e.a, 2)
	start() // a is saturated: spills over to b, the next cheapest
	waitCalls(e.b, 1)
	start() // then to c
	waitCalls(e.c, 1)

	// Everything is saturated now.
	res := e.chat(false)
	if res.status != 503 || res.err.Code != codeNoHealthy || !strings.Contains(res.err.Message, "m@a (provider at max_concurrency 2)") {
		t.Fatalf("status %d, error %+v", res.status, res.err)
	}
	if got := metricValue(t, e.reg, "switchyard_provider_in_flight", "a"); got != 2 {
		t.Errorf("provider_in_flight{a} = %v", got)
	}
	close(release)
	served := map[string]int{}
	for range 4 {
		r := <-results
		served[r.info.Provider]++
	}
	if served["a"] != 2 || served["b"] != 1 || served["c"] != 1 {
		t.Errorf("served %v", served)
	}
	for _, p := range []string{"a", "b", "c"} {
		if got := metricValue(t, e.reg, "switchyard_provider_in_flight", p); got != 0 {
			t.Errorf("provider_in_flight{%s} = %v after the requests", p, got)
		}
	}
	if got := metricValue(t, e.reg, "switchyard_provider_saturated_total", "a"); got < 2 {
		t.Errorf("provider_saturated_total{a} = %v", got)
	}
}

func TestKeepalive(t *testing.T) {
	r := fastRouting()
	r.StreamKeepalive = config.Duration(20 * time.Millisecond)
	t.Run("comments until the first event", func(t *testing.T) {
		e := newExecEnv(t, r, nil)
		e.a.script(step{delay: 90 * time.Millisecond})
		res := e.chat(true)
		if res.status != 200 || len(res.events) < 3 || res.events[0] != ": keep-alive" {
			t.Fatalf("status %d, events %v", res.status, res.events)
		}
		if last := res.events[len(res.events)-1]; last != "[DONE]" {
			t.Errorf("events %v", res.events)
		}
		n := 0
		for _, ev := range res.events {
			if ev == ": keep-alive" {
				n++
			}
		}
		if n < 2 {
			t.Errorf("%d keep-alives in 90ms at a 20ms interval", n)
		}
	})
	t.Run("a failure after a keep-alive is an error event", func(t *testing.T) {
		e := newExecEnv(t, r, nil)
		e.a.script(step{delay: 50 * time.Millisecond, err: err503})
		res := e.chat(true)
		if res.status != 200 || e.calls() != "a=1 b=0 c=0" {
			t.Fatalf("status %d, calls %s", res.status, e.calls())
		}
		if last := res.events[len(res.events)-1]; !strings.Contains(last, `"error"`) {
			t.Errorf("events %v", res.events)
		}
	})
	t.Run("a quick first event sends none", func(t *testing.T) {
		e := newExecEnv(t, r, nil)
		e.a.script(step{err: err503}, step{})
		res := e.chat(true)
		if res.status != 200 || res.events[0] == ": keep-alive" || res.info.Attempts != 2 {
			t.Errorf("status %d, events %v, info %+v", res.status, res.events, res.info)
		}
	})
	t.Run("non-streaming requests are unaffected", func(t *testing.T) {
		e := newExecEnv(t, r, nil)
		e.a.script(step{delay: 50 * time.Millisecond})
		if res := e.chat(false); res.status != 200 || res.body == "" {
			t.Errorf("status %d", res.status)
		}
	})
}

func TestCompletionsAndEmbeddingsFallBack(t *testing.T) {
	t.Run("native completion falls back to the shim", func(t *testing.T) {
		r := fastRouting()
		r.Fallback.RetriesPerDeployment = 0
		e := newExecEnv(t, r, nil)
		e.a.script(step{err: err503})
		res := e.do(EndpointCompletions, `{"model":"m","prompt":"hi"}`, nil)
		if res.status != 200 || res.info.Provider != "b" || res.info.Shim != ShimChat || res.info.Attempts != 2 {
			t.Fatalf("status %d, info %+v", res.status, res.info)
		}
		if e.a.kinds[0] != "completion" || e.b.kinds[0] != "chat" {
			t.Errorf("calls a %v, b %v", e.a.kinds, e.b.kinds)
		}
	})
	t.Run("a shim stream falls back to a native one", func(t *testing.T) {
		r := fastRouting()
		r.Fallback.RetriesPerDeployment = 0
		// Pinning b first: a pricier native deployment backs it up.
		e := newExecEnv(t, r, func(c *config.Config) {
			c.Models[0].Deployments[2].Pricing = price("0.5", "0.5")
		})
		e.b.script(step{err: err503})
		res := e.do(EndpointCompletions, `{"model":"m","prompt":"hi","stream":true}`, nil)
		if res.status != 200 || res.info.Provider != "a" || res.info.Shim != "" {
			t.Fatalf("status %d, info %+v", res.status, res.info)
		}
		if e.b.kinds[0] != "chat-stream" || e.a.kinds[0] != "completion-stream" {
			t.Errorf("calls a %v, b %v", e.a.kinds, e.b.kinds)
		}
	})
	t.Run("shim-only parameters plan native deployments only", func(t *testing.T) {
		e := newExecEnv(t, fastRouting(), nil)
		e.a.script(step{err: err503})
		res := e.do(EndpointCompletions, `{"model":"m","prompt":"hi","echo":true}`, nil)
		// a and c are native; b is not.
		if res.status != 200 || res.info.Provider != "c" || e.calls() != "a=2 b=0 c=1" {
			t.Errorf("status %d, info %+v, calls %s", res.status, res.info, e.calls())
		}
	})
	t.Run("embeddings", func(t *testing.T) {
		e := newExecEnv(t, fastRouting(), nil)
		e.a.script(step{err: errTransport})
		res := e.do(EndpointEmbeddings, `{"model":"e","input":"x"}`, nil)
		if res.status != 200 || res.info.Provider != "b" || res.info.RouteReason != "fallback:transport" || e.calls() != "a=2 b=1 c=0" {
			t.Errorf("status %d, info %+v, calls %s", res.status, res.info, e.calls())
		}
	})
}

func TestPlanPrefersProviderDiversity(t *testing.T) {
	r := fastRouting()
	r.Fallback.MaxDeployments = 2
	e := newExecEnv(t, r, nil)
	res, err := e.g.catalog.Resolve("m")
	if err != nil {
		t.Fatal(err)
	}
	var p plan
	if perr := e.g.buildPlan(&p, res, nil, 10, 10, nil); perr != nil {
		t.Fatal(perr)
	}
	if len(p.entries) != 2 || p.entries[0].d.ID != "m@a" || p.entries[1].d.ID != "m@b" {
		t.Errorf("plan %+v", p.entries)
	}
}

func TestBackoff(t *testing.T) {
	base, maxWait := 250*time.Millisecond, 2*time.Second
	for n, want := range []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second} {
		if got := backoffCap(base, maxWait, n); got != want {
			t.Errorf("backoffCap(%d) = %v, want %v", n, got, want)
		}
	}
	for range 1000 {
		if d := fullJitter(10 * time.Millisecond); d < 0 || d > 10*time.Millisecond {
			t.Fatalf("fullJitter = %v", d)
		}
	}
	if fullJitter(0) != 0 {
		t.Error("fullJitter(0) != 0")
	}
}

func TestProvidersHandler(t *testing.T) {
	r := fastRouting()
	r.Fallback.RetriesPerDeployment = 0
	e := newExecEnv(t, r, func(c *config.Config) { c.Providers[0].MaxConcurrency = 7 })
	e.a.script(step{err: rateLimited(3 * time.Second)})
	e.chat(false)
	w := &responseRecorder{header: http.Header{}}
	e.g.ProvidersHandler().ServeHTTP(w, &http.Request{})
	var rep struct {
		GeneratedAt time.Time `json:"generated_at"`
		Providers   []struct {
			ID             string `json:"id"`
			InFlight       int64  `json:"in_flight"`
			MaxConcurrency int    `json:"max_concurrency"`
			Deployments    []struct {
				ID            string     `json:"id"`
				CooldownUntil *time.Time `json:"cooldown_until"`
				Breaker       struct {
					State string `json:"state"`
				} `json:"breaker"`
				Recent struct {
					Requests int64 `json:"requests"`
				} `json:"recent"`
				RetryBudget struct {
					Requests int64 `json:"requests"`
				} `json:"retry_budget"`
				Latency struct {
					TTFTSamples int64 `json:"ttft_samples"`
				} `json:"latency"`
			} `json:"deployments"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(w.body.String()), &rep); err != nil {
		t.Fatalf("%v: %s", err, w.body.String())
	}
	if w.header.Get("Content-Type") != "application/json" || len(rep.Providers) != 3 || rep.GeneratedAt.IsZero() {
		t.Fatalf("report %s", w.body.String())
	}
	a := rep.Providers[0]
	if a.ID != "a" || a.MaxConcurrency != 7 || a.InFlight != 0 || len(a.Deployments) != 2 {
		t.Fatalf("provider a %+v", a)
	}
	var ma = a.Deployments[1]
	if ma.ID != "m@a" || ma.CooldownUntil == nil || ma.Breaker.State != "closed" || ma.Recent.Requests != 0 ||
		ma.RetryBudget.Requests != 1 || ma.Latency.TTFTSamples != 0 {
		t.Errorf("m@a %+v", ma)
	}
	mb := rep.Providers[1].Deployments[1]
	if mb.ID != "m@b" || mb.Recent.Requests != 1 || mb.Latency.TTFTSamples != 1 || mb.CooldownUntil != nil {
		t.Errorf("m@a %+v", ma)
	}
}

// responseRecorder is a minimal http.ResponseWriter.
type responseRecorder struct {
	header http.Header
	status int
	body   strings.Builder
}

func (w *responseRecorder) Header() http.Header         { return w.header }
func (w *responseRecorder) WriteHeader(s int)           { w.status = s }
func (w *responseRecorder) Write(b []byte) (int, error) { return w.body.Write(b) }

// panicky panics where its adapter would answer, standing in for any panic
// inside an attempt (adapter, translation, converter).
type panicky struct{ *scripted }

func (p panicky) Chat(context.Context, *providers.Request) (*providers.Response, error) {
	panic("boom")
}

func (p panicky) ChatStream(context.Context, *providers.Request) (providers.Stream, error) {
	panic("boom")
}

// chatRecovered serves a chat request whose attempt panics and returns the
// recovered panic value, as the server's recovery middleware would see it.
func chatRecovered(e *execEnv, stream bool) (v any) {
	defer func() { v = recover() }()
	e.chat(stream)
	return nil
}

// TestPanicReleasesSlotAndProbe checks that a panic inside an attempt
// still releases the provider's concurrency slot and ends the breaker
// admission, so that neither leaks, and that it reaches the handler's
// goroutine, keep-alives or not.
func TestPanicReleasesSlotAndProbe(t *testing.T) {
	t.Run("concurrency slot", func(t *testing.T) {
		for _, keepalive := range []time.Duration{0, time.Hour} {
			t.Run(fmt.Sprintf("keepalive=%v", keepalive), func(t *testing.T) {
				r := fastRouting()
				r.StreamKeepalive = config.Duration(keepalive)
				e := newExecEnv(t, r, func(c *config.Config) { c.Providers[1].MaxConcurrency = 1 }) // a
				e.g.adapters["a"] = panicky{e.a}
				for _, stream := range []bool{false, true} {
					if v := chatRecovered(e, stream); v != "boom" {
						t.Fatalf("stream=%v: recovered %v, want the panic", stream, v)
					}
					if n := e.g.provs["a"].InFlight(); n != 0 {
						t.Fatalf("stream=%v: %d requests in flight after the panic", stream, n)
					}
				}
				e.g.adapters["a"] = e.a
				if res := e.chat(false); res.status != 200 || res.info.Provider != "a" {
					t.Errorf("after the panic: status %d, provider %q", res.status, res.info.Provider)
				}
			})
		}
	})
	t.Run("half-open probe", func(t *testing.T) {
		r := fastRouting()
		r.Fallback.RetriesPerDeployment = 0
		r.Breaker.ConsecutiveFailures = 1
		r.Breaker.OpenFor = config.Duration(time.Second)
		r.Breaker.HalfOpenProbes = 1
		clk := &clock{now: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
		e := newExecEnvClock(t, r, nil, clk.Now)
		e.a.script(step{err: err503})
		e.chat(false) // opens a's breaker
		clk.advance(2 * time.Second)
		e.g.adapters["a"] = panicky{e.a}
		if v := chatRecovered(e, false); v != "boom" {
			t.Fatalf("recovered %v, want the panic", v)
		}
		if r := e.g.deps["m@a"].Skip(); r != "" {
			t.Fatalf("after the probe panicked: Skip = %q, want the probe slot free", r)
		}
		e.g.adapters["a"] = e.a
		e.a.script(step{})
		if res := e.chat(false); res.info.Provider != "a" || e.g.deps["m@a"].State() != health.StateClosed {
			t.Errorf("next probe served by %q, breaker %s", res.info.Provider, e.g.deps["m@a"].State())
		}
	})
}

// TestCancelDuringBackoff checks that a client that goes away while the
// executor waits before a retry spends no retry from the budget and
// counts no retry.
func TestCancelDuringBackoff(t *testing.T) {
	r := fastRouting()
	r.Fallback.MaxDeployments = 1
	r.Fallback.BackoffBase = config.Duration(time.Minute)
	r.Fallback.BackoffMax = config.Duration(time.Minute)
	e := newExecEnv(t, r, nil)
	e.g.jitter = func(d time.Duration) time.Duration { return d }
	e.a.script(step{err: err503})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for e.a.count() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	x := &hdrExchange{recExchange: recExchange{body: []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)}}
	e.g.ChatCompletions(ctx, x)
	if x.res.status != StatusClientClosedRequest || e.calls() != "a=1 b=0 c=0" {
		t.Fatalf("status %d, calls %s", x.res.status, e.calls())
	}
	if got := metricValue(t, e.reg, "switchyard_retries_total", "m@a", "server_error"); got > 0 {
		t.Errorf("retries_total = %v, want 0", got)
	}
	for _, p := range e.g.health.Snapshot().Providers {
		for _, d := range p.Deployments {
			if d.ID == "m@a" && d.RetryBudget.Used != 0 {
				t.Errorf("retry budget used %d, want 0", d.RetryBudget.Used)
			}
		}
	}
}

// TestFailureHeaders checks that when every attempt failed, the provider
// and upstream request id headers belong to the failure being answered,
// not to the last attempt.
func TestFailureHeaders(t *testing.T) {
	r := fastRouting()
	r.Fallback.RetriesPerDeployment = 0
	withRID := func(err *providers.Error, rid string) *providers.Error {
		c := *err
		c.UpstreamRequestID = rid
		return &c
	}
	tests := []struct {
		name          string
		a, b, c       *providers.Error
		status        int
		provider, rid string
	}{
		{name: "best failure first", a: withRID(errContextLen, "a-rid"), b: withRID(err503, "b-rid"), c: withRID(err503, "c-rid"),
			status: 400, provider: "a", rid: "a-rid"},
		{name: "best failure in the middle", a: withRID(err503, "a-rid"), b: withRID(errNotFound, "b-rid"), c: withRID(errTransport, "c-rid"),
			status: 404, provider: "b", rid: "b-rid"},
		{name: "every failure rate-limited", a: withRID(rateLimited(0), "a-rid"), b: withRID(rateLimited(0), "b-rid"), c: withRID(rateLimited(0), "c-rid"),
			status: 429, provider: "c", rid: "c-rid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newExecEnv(t, r, nil)
			e.a.script(step{err: tt.a})
			e.b.script(step{err: tt.b})
			e.c.script(step{err: tt.c})
			res := e.chat(false)
			if res.status != tt.status || res.info.Provider != tt.provider || res.info.UpstreamRequestID != tt.rid {
				t.Errorf("status %d, provider %q, upstream request id %q; want %d, %q, %q",
					res.status, res.info.Provider, res.info.UpstreamRequestID, tt.status, tt.provider, tt.rid)
			}
			if res.info.Attempts != 3 || res.ann["upstream_request_id"] != tt.rid {
				t.Errorf("attempts %d, logged upstream_request_id %v", res.info.Attempts, res.ann["upstream_request_id"])
			}
		})
	}
}

// heldAdapter streams one chunk, then holds its stream until release is
// closed; waiting is closed once the gateway asks for the second chunk.
type heldAdapter struct {
	*scripted
	waiting, release chan struct{}
}

func (h *heldAdapter) ChatStream(ctx context.Context, _ *providers.Request) (providers.Stream, error) {
	if _, err := h.begin(ctx, "chat-stream"); err != nil {
		return nil, err
	}
	return &heldStream{scriptStream: scriptStream{chunks: defaultChunks}, h: h}, nil
}

type heldStream struct {
	scriptStream
	h *heldAdapter
}

func (s *heldStream) Next() (*schema.ChatChunk, error) {
	if s.i == 1 {
		close(s.h.waiting)
		<-s.h.release
	}
	return s.scriptStream.Next()
}

// TestStreamHealthAtCommitPoint checks that a half-open probe ends at its
// commit point, when the upstream answers: a probe that is a long stream
// closes the breaker at its first event, not when the stream ends. After
// the commit point, upstream failures still count toward the breaker,
// while the request's total timeout and the client going away do not.
func TestStreamHealthAtCommitPoint(t *testing.T) {
	r := fastRouting()
	r.Fallback.RetriesPerDeployment = 0
	r.Breaker.ConsecutiveFailures = 1
	r.Breaker.OpenFor = config.Duration(time.Second)
	r.Breaker.HalfOpenProbes = 1
	t.Run("a probe closes the breaker at its first event", func(t *testing.T) {
		clk := &clock{now: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
		e := newExecEnvClock(t, r, nil, clk.Now)
		e.a.script(step{err: err503})
		e.chat(false) // opens a's breaker
		clk.advance(2 * time.Second)
		h := &heldAdapter{scripted: e.a, waiting: make(chan struct{}), release: make(chan struct{})}
		e.a.script(step{})
		e.g.adapters["a"] = h
		done := make(chan exchangeResult, 1)
		go func() { done <- e.chat(true) }()
		<-h.waiting
		d := e.g.deps["m@a"]
		if st, skip := d.State(), d.Skip(); st != health.StateClosed || skip != "" {
			t.Errorf("while the probe streams: breaker %s, Skip %q; want closed", st, skip)
		}
		close(h.release)
		if res := <-done; res.status != 200 || res.events[len(res.events)-1] != "[DONE]" {
			t.Errorf("probe stream: status %d, events %v", res.status, res.events)
		}
	})
	t.Run("a non-streaming probe closes the breaker before the answer is written", func(t *testing.T) {
		clk := &clock{now: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
		e := newExecEnvClock(t, r, nil, clk.Now)
		e.a.script(step{err: err503})
		e.chat(false) // opens a's breaker
		clk.advance(2 * time.Second)
		e.a.script(step{})
		var during health.State = -1
		x := &respondHook{hdrExchange: hdrExchange{recExchange: recExchange{body: []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)}},
			hook: func() { during = e.g.deps["m@a"].State() }}
		e.g.ChatCompletions(context.Background(), x)
		if x.res.status != 200 || x.res.info.Provider != "a" || during != health.StateClosed {
			t.Errorf("status %d, provider %q, breaker while answering %s; want closed", x.res.status, x.res.info.Provider, during)
		}
	})
	t.Run("after the commit point", func(t *testing.T) {
		r := fastRouting()
		r.Fallback.RetriesPerDeployment = 0
		r.Breaker.ConsecutiveFailures = 3
		tests := []struct {
			name   string
			end    error
			client error // what writing to the client returns
			opens  bool
		}{
			{name: "connection dropped", end: errTransport, opens: true},
			{name: "idle timeout", end: &providers.Error{Kind: providers.KindTimeout, Timeout: providers.TimeoutIdle, Message: "idle timeout"}, opens: true},
			{name: "error event", end: &providers.Error{Kind: providers.KindServerError, Message: "upstream sent an error event"}, opens: true},
			{name: "total timeout", end: &providers.Error{Kind: providers.KindTimeout, Timeout: providers.TimeoutTotal, Message: "total timeout"}},
			{name: "request canceled", end: errCanceled},
			{name: "client stopped reading", client: errors.New("write: i/o timeout")},
			{name: "completed"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				e := newExecEnv(t, r, nil)
				e.a.script(step{chunks: defaultChunks[:2], end: tt.end})
				for range 3 {
					x := &sendFails{hdrExchange: hdrExchange{recExchange: recExchange{
						body: []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)}}, err: tt.client}
					e.g.ChatCompletions(context.Background(), x)
				}
				want := health.StateClosed
				if tt.opens {
					want = health.StateOpen
				}
				if st := e.g.deps["m@a"].State(); st != want || e.a.count() != 3 {
					t.Errorf("breaker %s after 3 streams, calls %s; want %s", st, e.calls(), want)
				}
			})
		}
	})
}

// sendFails is an hdrExchange whose stream events fail with err, if set.
type sendFails struct {
	hdrExchange
	err error
}

func (x *sendFails) Send(data []byte) error {
	if x.err != nil {
		return x.err
	}
	return x.hdrExchange.Send(data)
}

// TestNonStreamTotalTimeout checks that a non-streaming call that hit its
// provider's total timeout falls back without a retry, which would repeat
// a long, billed generation, and counts toward the breaker; a stream's
// total timeout before its first event is retried as other timeouts are.
func TestNonStreamTotalTimeout(t *testing.T) {
	total := &providers.Error{Kind: providers.KindTimeout, Timeout: providers.TimeoutTotal, Message: "total timeout"}
	r := fastRouting()
	r.Breaker.ConsecutiveFailures = 1
	t.Run("non-stream", func(t *testing.T) {
		e := newExecEnv(t, r, nil)
		e.a.script(step{err: total})
		res := e.chat(false)
		if res.status != 200 || e.calls() != "a=1 b=1 c=0" || res.info.RouteReason != "fallback:timeout" {
			t.Fatalf("status %d, calls %s, info %+v", res.status, e.calls(), res.info)
		}
		if st := e.g.deps["m@a"].State(); st != health.StateOpen {
			t.Errorf("breaker %s, want open: the timeout counts", st)
		}
	})
	t.Run("stream", func(t *testing.T) {
		e := newExecEnv(t, fastRouting(), nil)
		e.a.script(step{err: total}, step{})
		if res := e.chat(true); res.status != 200 || e.calls() != "a=2 b=0 c=0" {
			t.Errorf("status %d, calls %s", res.status, e.calls())
		}
	})
}

// TestUnsupportedFallsBack checks the kind of an adapter's local
// rejection: it falls back without a retry or a breaker failure, and is
// the answer only when every attempt was rejected so.
func TestUnsupportedFallsBack(t *testing.T) {
	unsupported := &providers.Error{Kind: providers.KindUnsupported, Status: 400,
		Body: []byte(`{"error":{"message":"cannot translate","type":"invalid_request_error","param":"messages","code":"invalid_value"}}`)}
	r := fastRouting()
	r.Breaker.ConsecutiveFailures = 1
	tests := []struct {
		name    string
		a, b, c step
		status  int
		calls   string
		route   string
	}{
		{name: "the next deployment serves", a: step{err: unsupported}, status: 200, calls: "a=1 b=1 c=0", route: "fallback:unsupported"},
		{name: "after a transient failure", a: step{err: err503}, b: step{err: unsupported}, status: 200, calls: "a=1 b=1 c=1", route: "fallback:server_error"},
		{name: "every deployment rejects it", a: step{err: unsupported}, b: step{err: unsupported}, c: step{err: unsupported},
			status: 400, calls: "a=1 b=1 c=1", route: "fallback:unsupported"},
		{name: "another failure is the answer", a: step{err: err503}, b: step{err: unsupported}, c: step{err: unsupported},
			status: 502, calls: "a=1 b=1 c=1", route: "fallback:server_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newExecEnv(t, r, nil)
			e.a.script(tt.a)
			e.b.script(tt.b)
			e.c.script(tt.c)
			res := e.chat(false)
			if res.status != tt.status || e.calls() != tt.calls || res.info.RouteReason != tt.route {
				t.Fatalf("status %d, calls %s, route %q; want %d, %s, %q", res.status, e.calls(), res.info.RouteReason, tt.status, tt.calls, tt.route)
			}
			if tt.status == 400 && (res.err == nil || string(res.err.Body) != string(unsupported.Body)) {
				t.Errorf("error %+v, want the adapter's body", res.err)
			}
			for _, id := range []string{"m@b", "m@c"} {
				if st := e.g.deps[id].State(); st != health.StateClosed {
					t.Errorf("%s breaker %s, want closed", id, st)
				}
			}
		})
	}
}

// respondHook is an hdrExchange that calls hook as it answers.
type respondHook struct {
	hdrExchange
	hook func()
}

func (x *respondHook) Respond(info Info, status int, body []byte) {
	x.hook()
	x.hdrExchange.Respond(info, status, body)
}
