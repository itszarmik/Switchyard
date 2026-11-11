package gateway

import (
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

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/apierror"
	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/providers/anthropic"
	"switchyard/internal/providers/openai"
	"switchyard/internal/reqctx"
	"switchyard/internal/schema"
)

// allCapabilities is every capability in the vocabulary.
var allCapabilities = func() []string {
	var out []string
	for _, c := range schema.Vocabulary() {
		out = append(out, string(c))
	}
	return out
}()

// price returns a deployment price; it panics on a bad literal.
func price(in, out string) config.Pricing {
	p := config.Pricing{}
	var err error
	if p.Input, err = config.ParsePrice(in); err != nil {
		panic(err)
	}
	if p.Output, err = config.ParsePrice(out); err != nil {
		panic(err)
	}
	return p
}

// testConfig is a gateway config for the P1 pipeline tests: one attempt
// per request, on the cheapest deployment only.
func testConfig() *config.Config {
	return &config.Config{
		Routing: config.Routing{Fallback: config.Fallback{MaxDeployments: 1}},
		Providers: []config.Provider{
			{ID: "fake", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"},
			{ID: "anth", Type: "anthropic", BaseURL: "https://api.anthropic.test"},
		},
		Models: []config.Model{
			{ID: "gpt-test", Capabilities: allCapabilities, Deployments: []config.Deployment{
				{Provider: "fake", UpstreamModel: "upstream-gpt", Pricing: price("1", "2")},
				{Provider: "anth", UpstreamModel: "pricier-second", Pricing: price("3", "4")},
			}},
			{ID: "claude-test", Capabilities: []string{"streaming", "tools"}, Deployments: []config.Deployment{
				{Provider: "anth", UpstreamModel: "claude", Pricing: price("1", "2")},
			}},
		},
	}
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// find decodes the JSON log lines whose msg is msg.
func (b *logBuffer) find(t *testing.T, msg string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(b.buf.Bytes()))
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

func newTestGateway(t *testing.T) (*Gateway, *logBuffer, *prometheus.Registry) {
	t.Helper()
	logs := &logBuffer{}
	reg := prometheus.NewRegistry()
	g, err := New(Options{
		Config:   testConfig(),
		Logger:   slog.New(slog.NewJSONHandler(logs, nil)),
		Registry: reg,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g, logs, reg
}

func TestNew(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{name: "no config", opts: Options{Logger: logger, Registry: prometheus.NewRegistry()}, want: "missing Config"},
		{name: "no logger", opts: Options{Config: testConfig(), Registry: prometheus.NewRegistry()}, want: "missing Logger"},
		{name: "no registry", opts: Options{Config: testConfig(), Logger: logger}, want: "missing Registry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.opts); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("New error = %v, want %q", err, tt.want)
			}
		})
	}

	t.Run("registers its metrics once", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		if _, err := New(Options{Config: testConfig(), Logger: logger, Registry: reg}); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Options{Config: testConfig(), Logger: logger, Registry: reg}); err == nil {
			t.Error("second New on the same registry succeeded")
		}
	})

	t.Run("one adapter per provider, of its type", func(t *testing.T) {
		g, _, _ := newTestGateway(t)
		if _, ok := g.adapters["fake"].(*openai.Adapter); !ok {
			t.Errorf("fake adapter = %T", g.adapters["fake"])
		}
		if _, ok := g.adapters["anth"].(*anthropic.Adapter); !ok {
			t.Errorf("anth adapter = %T", g.adapters["anth"])
		}
	})
}

func TestListModels(t *testing.T) {
	before := time.Now().Unix()
	g, _, reg := newTestGateway(t)
	after := time.Now().Unix()
	got := g.ListModels()
	if len(got) != 2 || got[0].ID != "gpt-test" || got[1].ID != "claude-test" {
		t.Fatalf("models = %+v", got)
	}
	for _, m := range got {
		if m.Created < before || m.Created > after {
			t.Errorf("created = %d, want the load time in [%d, %d]", m.Created, before, after)
		}
	}
	got[0].ID = "mutated"
	if g.ListModels()[0].ID != "gpt-test" {
		t.Error("ListModels returned its internal slice")
	}
	// Instrument counts the request, not ListModels.
	if v := metricValue(t, reg, "switchyard_requests_total", EndpointModels, "2xx"); v != 0 {
		t.Errorf("requests_total{/v1/models,2xx} = %v, want 0", v)
	}
}

// metricValue returns a counter's or gauge's value, or a histogram's
// sample count,
// for the series with the given label values (in label-name order), or 0
// if there is no such series.
func metricValue(t *testing.T, reg *prometheus.Registry, name string, labels ...string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
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

func TestUpstreamError(t *testing.T) {
	envelope := []byte(`{"error":{"message":"bad","type":"invalid_request_error","param":null,"code":"x"}}`)
	tests := []struct {
		name string
		in   error
		want Error
	}{
		{
			name: "4xx with envelope",
			in:   &providers.Error{Kind: providers.KindClientError, Status: 422, Body: envelope},
			want: Error{Status: 422, Body: envelope},
		},
		{
			name: "4xx without envelope",
			in:   &providers.Error{Kind: providers.KindClientError, Status: 400},
			want: Error{Status: 400, Type: apierror.TypeInvalidRequest, Code: "upstream_error", Message: "The upstream provider rejected the request with HTTP 400."},
		},
		{
			name: "429 with envelope keeps Retry-After",
			in:   &providers.Error{Kind: providers.KindRateLimited, Status: 429, Body: envelope, RetryAfter: 3 * time.Second},
			want: Error{Status: 429, Body: envelope, RetryAfter: 3 * time.Second},
		},
		{
			name: "429 without envelope",
			in:   &providers.Error{Kind: providers.KindRateLimited, Status: 429, RetryAfter: time.Second},
			want: Error{Status: 429, Type: "requests", Code: "rate_limit_exceeded", RetryAfter: time.Second,
				Message: "The upstream provider's rate limit was reached. Please retry later."},
		},
		{
			name: "401 is a generic 502",
			in:   &providers.Error{Kind: providers.KindAuth, Status: 401, Message: "upstream rejected the provider credential (HTTP 401)"},
			want: Error{Status: 502, Type: apierror.TypeServer, Code: "upstream_error", Message: "The upstream provider could not authenticate the gateway."},
		},
		{
			name: "403 is a generic 502",
			in:   &providers.Error{Kind: providers.KindAuth, Status: 403},
			want: Error{Status: 502, Type: apierror.TypeServer, Code: "upstream_error", Message: "The upstream provider could not authenticate the gateway."},
		},
		{
			name: "5xx names the status",
			in:   &providers.Error{Kind: providers.KindServerError, Status: 503},
			want: Error{Status: 502, Type: apierror.TypeServer, Code: "upstream_error", Message: "The upstream provider returned HTTP 503."},
		},
		{
			name: "malformed stream",
			in:   &providers.Error{Kind: providers.KindServerError, Message: "upstream sent an error event"},
			want: Error{Status: 502, Type: apierror.TypeServer, Code: "upstream_error", Message: "The upstream provider failed: upstream sent an error event."},
		},
		{
			name: "connection error",
			in:   &providers.Error{Kind: providers.KindTransport, Err: errors.New("dial tcp: refused")},
			want: Error{Status: 502, Type: apierror.TypeServer, Code: "upstream_error", Message: "The upstream provider could not be reached."},
		},
		{
			name: "non-provider error",
			in:   errors.New("boom"),
			want: Error{Status: 502, Type: apierror.TypeServer, Code: "upstream_error", Message: "The upstream provider could not be reached."},
		},
		{
			name: "timeout",
			in:   &providers.Error{Kind: providers.KindTimeout, Message: "upstream sent no first token within 1s (first_token timeout)"},
			want: Error{Status: 504, Type: apierror.TypeServer, Code: "timeout",
				Message: "The upstream provider did not respond in time: upstream sent no first token within 1s (first_token timeout)."},
		},
		{
			name: "canceled",
			in:   &providers.Error{Kind: providers.KindCanceled},
			want: Error{Status: 499, Type: apierror.TypeInvalidRequest, Code: "canceled", Message: "The request was canceled by the client."},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(&tt.want, upstreamError(tt.in)); diff != "" {
				t.Errorf("upstreamError (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStreamErrorEvent(t *testing.T) {
	got := string(streamErrorEvent(&providers.Error{Kind: providers.KindTimeout, Message: `idle "x"`}))
	want := `{"error":{"message":"The upstream provider failed mid-stream: idle \"x\".","type":"server_error","code":"upstream_error"}}`
	if got != want {
		t.Errorf("event = %s, want %s", got, want)
	}
}

// fakeExchange records the calls the pipeline makes, in order.
type fakeExchange struct {
	body    []byte
	bodyErr *Error
	header  http.Header
	log     *[]string
	failAt  string // a call that returns an error
	info    Info
	// slow is how long every write to the client takes.
	slow time.Duration
}

func (f *fakeExchange) add(s string) {
	*f.log = append(*f.log, s)
	if f.slow > 0 {
		time.Sleep(f.slow)
	}
}

func (f *fakeExchange) Body() ([]byte, *Error) { return f.body, f.bodyErr }
func (f *fakeExchange) Header() http.Header    { return f.header }
func (f *fakeExchange) Respond(info Info, status int, body []byte) {
	f.info = info
	f.add(fmt.Sprintf("respond %d %s", status, body))
}
func (f *fakeExchange) RespondError(info Info, e *Error) {
	f.info = info
	f.add(fmt.Sprintf("error %d %s", e.Status, e.Code))
}
func (f *fakeExchange) StartStream(info Info) error {
	f.info = info
	f.add("start")
	return f.fail("start")
}
func (f *fakeExchange) Comment(text string) error {
	f.add(": " + text)
	return f.fail(": " + text)
}
func (f *fakeExchange) Send(data []byte) error {
	f.add("send " + string(data))
	return f.fail("send " + string(data))
}
func (f *fakeExchange) Done() error { f.add("done"); return f.fail("done") }

func (f *fakeExchange) fail(call string) error {
	if call == f.failAt {
		return errors.New("client gone")
	}
	return nil
}

// fakeAdapter scripts the upstream. It appends "upstream: …" entries to the
// same log as the exchange, so tests can check the order of events.
type fakeAdapter struct {
	log    *[]string
	chunks []string
	usage  *schema.Usage
	end    error // what Next returns after the chunks; nil means io.EOF
	err    error // returned by Chat and ChatStream
}

func (a *fakeAdapter) Chat(_ context.Context, _ *providers.Request) (*providers.Response, error) {
	*a.log = append(*a.log, "upstream: response")
	if a.err != nil {
		return nil, a.err
	}
	return &providers.Response{Status: 200, Body: []byte(`{"ok":true}`), UpstreamRequestID: "rid", Usage: a.usage, TTFT: time.Millisecond}, nil
}

func (a *fakeAdapter) ChatStream(_ context.Context, _ *providers.Request) (providers.Stream, error) {
	*a.log = append(*a.log, "upstream: first event")
	if a.err != nil {
		return nil, a.err
	}
	return &fakeStream{a: a}, nil
}

type fakeStream struct {
	a      *fakeAdapter
	i      int
	closed bool
}

func (s *fakeStream) Next() (*schema.ChatChunk, error) {
	if s.i < len(s.a.chunks) {
		c, err := schema.ParseChatChunk([]byte(s.a.chunks[s.i]))
		if err != nil {
			panic(err)
		}
		s.i++
		return c, nil
	}
	if s.a.end != nil {
		return nil, s.a.end
	}
	return nil, io.EOF
}
func (s *fakeStream) Usage() *schema.Usage      { return s.a.usage }
func (s *fakeStream) UpstreamRequestID() string { return "rid-s" }
func (s *fakeStream) TTFT() time.Duration       { return time.Millisecond }
func (s *fakeStream) Close() error {
	s.closed = true
	*s.a.log = append(*s.a.log, "upstream: closed")
	return nil
}

func TestChatCompletionsPipeline(t *testing.T) {
	const (
		content = `{"choices":[{"index":0,"delta":{"content":"hi"}}],"usage":null}`
		usage   = `{"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`
		// Some upstreams put the usage on the last chunk with choices.
		finalWithUsage = `{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10},"x":1}`
		finalNoUsage   = `{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null,"x":1}`
	)
	streamReq := `{"model":"gpt-test","messages":[{"role":"user","content":"x"}],"stream":true}`
	usageReq := `{"model":"gpt-test","messages":[{"role":"user","content":"x"}],"stream":true,"stream_options":{"include_usage":true}}`
	tests := []struct {
		name     string
		body     string
		bodyErr  *Error
		adapter  fakeAdapter
		failAt   string
		want     []string
		wantCode string
	}{
		{
			name: "non-stream",
			body: `{"model":"gpt-test","messages":[{}]}`,
			want: []string{"upstream: response", `respond 200 {"ok":true}`},
		},
		{
			name:     "non-stream upstream error",
			body:     `{"model":"gpt-test","messages":[{}]}`,
			adapter:  fakeAdapter{err: &providers.Error{Kind: providers.KindServerError, Status: 500}},
			want:     []string{"upstream: response", "error 502 upstream_error"},
			wantCode: "upstream_error",
		},
		{
			name:     "body error",
			bodyErr:  &Error{Status: 413, Code: "request_too_large"},
			want:     []string{"error 413 request_too_large"},
			wantCode: "request_too_large",
		},
		{
			name:     "invalid request",
			body:     `{"model":"gpt-test"}`,
			want:     []string{"error 400 missing_required_parameter"},
			wantCode: "missing_required_parameter",
		},
		{
			name:     "unknown model",
			body:     `{"model":"nope","messages":[{}]}`,
			want:     []string{"error 404 model_not_found"},
			wantCode: "model_not_found",
		},
		{
			name:     "virtual model",
			body:     `{"model":"auto","messages":[{}]}`,
			want:     []string{"error 400 model_not_supported"},
			wantCode: "model_not_supported",
		},
		{
			name:     "no eligible deployment",
			body:     `{"model":"claude-test","messages":[{}],"n":2}`,
			want:     []string{"error 400 no_eligible_model"},
			wantCode: "no_eligible_model",
		},
		{
			name:    "stream commits after the first upstream event and strips usage",
			body:    streamReq,
			adapter: fakeAdapter{chunks: []string{content, content, usage}},
			want: []string{
				"upstream: first event", "start", "send " + content, "send " + content, "done", "upstream: closed",
			},
		},
		{
			name:    "stream strips usage from a chunk with choices",
			body:    streamReq,
			adapter: fakeAdapter{chunks: []string{content, finalWithUsage}},
			want: []string{
				"upstream: first event", "start", "send " + content, "send " + finalNoUsage, "done", "upstream: closed",
			},
		},
		{
			name:    "stream keeps usage on a chunk with choices when the client asked",
			body:    usageReq,
			adapter: fakeAdapter{chunks: []string{content, finalWithUsage}},
			want: []string{
				"upstream: first event", "start", "send " + content, "send " + finalWithUsage, "done", "upstream: closed",
			},
		},
		{
			name:    "stream forwards usage when the client asked",
			body:    usageReq,
			adapter: fakeAdapter{chunks: []string{content, usage}},
			want: []string{
				"upstream: first event", "start", "send " + content, "send " + usage, "done", "upstream: closed",
			},
		},
		{
			name:     "stream failing before the first event never commits",
			body:     streamReq,
			adapter:  fakeAdapter{err: &providers.Error{Kind: providers.KindTimeout, Message: "t"}},
			want:     []string{"upstream: first event", "error 504 timeout"},
			wantCode: "timeout",
		},
		{
			name:    "stream failing after the commit sends an error event without [DONE]",
			body:    streamReq,
			adapter: fakeAdapter{chunks: []string{content}, end: &providers.Error{Kind: providers.KindTransport, Message: "connection to the upstream failed"}},
			want: []string{
				"upstream: first event", "start", "send " + content,
				`send {"error":{"message":"The upstream provider failed mid-stream: connection to the upstream failed.","type":"server_error","code":"upstream_error"}}`,
				"upstream: closed",
			},
			wantCode: "upstream_error",
		},
		{
			name:     "client gone mid-stream closes the upstream",
			body:     streamReq,
			adapter:  fakeAdapter{chunks: []string{content, content}},
			failAt:   "send " + content,
			want:     []string{"upstream: first event", "start", "send " + content, "upstream: closed"},
			wantCode: "client_disconnected",
		},
		{
			name:     "upstream canceled mid-stream writes nothing more",
			body:     streamReq,
			adapter:  fakeAdapter{chunks: []string{content}, end: &providers.Error{Kind: providers.KindCanceled}},
			want:     []string{"upstream: first event", "start", "send " + content, "upstream: closed"},
			wantCode: "client_disconnected",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, logs, _ := newTestGateway(t)
			var log []string
			ad := tt.adapter
			ad.log = &log
			g.adapters["fake"] = &ad
			ex := &fakeExchange{body: []byte(tt.body), bodyErr: tt.bodyErr, header: http.Header{}, log: &log, failAt: tt.failAt}
			ctx, ann := annotated(logs)

			g.ChatCompletions(ctx, ex)

			if diff := cmp.Diff(tt.want, log); diff != "" {
				t.Errorf("calls (-want +got):\n%s", diff)
			}
			if got := annotations(ann)["error_code"]; got != tt.wantCode {
				t.Errorf("error_code annotation = %v, want %q", got, tt.wantCode)
			}
			if n := len(logs.find(t, "completion")); n != 0 {
				t.Errorf("the gateway logged %d completion lines; it should only annotate", n)
			}
		})
	}
}

// annotated returns a request context whose logger writes to logs, with a
// collector for the access-log annotations, as the server sets them up.
func annotated(logs *logBuffer) (context.Context, *reqctx.Annotations) {
	ann := &reqctx.Annotations{}
	ctx := reqctx.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(logs, nil)))
	return reqctx.WithAnnotations(ctx, ann), ann
}

// annotations returns the annotated attributes by key.
func annotations(a *reqctx.Annotations) map[string]any {
	m := map[string]any{}
	for _, attr := range a.Attrs() {
		m[attr.Key] = attr.Value.Any()
	}
	return m
}

func TestCompletionAnnotations(t *testing.T) {
	g, logs, reg := newTestGateway(t)
	var log []string
	g.adapters["fake"] = &fakeAdapter{
		log: &log, chunks: []string{`{"choices":[{"index":0}]}`},
		usage: &schema.Usage{PromptTokens: 11, CompletionTokens: 22},
	}
	ctx, ann := annotated(logs)
	h := g.Instrument(EndpointChatCompletions, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		g.ChatCompletions(r.Context(), &fakeExchange{
			body: []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"SECRET PROMPT"}],"stream":true}`),
			log:  &log, header: http.Header{},
		})
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodPost, EndpointChatCompletions, nil))

	r := annotations(ann)
	for k, want := range map[string]any{
		"requested_model": "gpt-test", "deployment": "gpt-test@fake", "stream": true,
		"prompt_tokens": int64(11), "completion_tokens": int64(22), "upstream_request_id": "rid-s", "error_code": "",
	} {
		if r[k] != want {
			t.Errorf("%s = %v (%T), want %v", k, r[k], r[k], want)
		}
	}
	for _, k := range []string{"ttft_ms", "latency_ms", "overhead_ms"} {
		if v, ok := r[k].(float64); !ok || v < 0 {
			t.Errorf("%s = %v", k, r[k])
		}
	}
	if _, ok := r["status"]; ok {
		t.Error("status is annotated; the access line already has it")
	}
	if strings.Contains(fmt.Sprint(r), "SECRET PROMPT") {
		t.Error("the annotations contain prompt content")
	}

	checks := []struct {
		name   string
		labels []string
		want   float64
	}{
		{"switchyard_requests_total", []string{EndpointChatCompletions, "2xx"}, 1},
		{"switchyard_request_duration_seconds", []string{EndpointChatCompletions, "true"}, 1},
		{"switchyard_overhead_duration_seconds", []string{EndpointChatCompletions}, 1},
		{"switchyard_upstream_requests_total", []string{"gpt-test", "ok", "fake"}, 1},
		{"switchyard_upstream_ttft_seconds", []string{"gpt-test", "fake"}, 1},
		{"switchyard_upstream_duration_seconds", []string{"gpt-test", "fake"}, 1},
	}
	for _, c := range checks {
		if v := metricValue(t, reg, c.name, c.labels...); v != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, v, c.want)
		}
	}
}

// TestInstrument checks that every request through Instrument is counted,
// including those its handler rejects without reaching the gateway.
func TestInstrument(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   string
		handler    http.HandlerFunc
		wantClass  string
		wantStream string
	}{
		{
			name: "rejected before the gateway", endpoint: EndpointChatCompletions,
			handler:   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
			wantClass: "4xx", wantStream: "false",
		},
		{
			name: "implicit 200", endpoint: EndpointModels,
			handler:   func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) },
			wantClass: "2xx", wantStream: "false",
		},
		{
			name: "nothing written", endpoint: EndpointModels,
			handler:   func(http.ResponseWriter, *http.Request) {},
			wantClass: "2xx", wantStream: "false",
		},
		{
			name: "stream request", endpoint: EndpointChatCompletions,
			handler: func(w http.ResponseWriter, r *http.Request) {
				markStream(r.Context(), true)
				w.WriteHeader(http.StatusBadGateway)
			},
			wantClass: "5xx", wantStream: "true",
		},
		{
			name: "response controller reaches the writer below", endpoint: EndpointChatCompletions,
			handler: func(w http.ResponseWriter, r *http.Request) {
				markStream(r.Context(), true)
				w.WriteHeader(http.StatusOK)
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Errorf("Flush through Instrument: %v", err)
				}
			},
			wantClass: "2xx", wantStream: "true",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, _, reg := newTestGateway(t)
			rec := httptest.NewRecorder()
			g.Instrument(tt.endpoint, tt.handler).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tt.endpoint, nil))
			if v := metricValue(t, reg, "switchyard_requests_total", tt.endpoint, tt.wantClass); v != 1 {
				t.Errorf("requests_total{%s,%s} = %v, want 1", tt.endpoint, tt.wantClass, v)
			}
			if v := metricValue(t, reg, "switchyard_request_duration_seconds", tt.endpoint, tt.wantStream); v != 1 {
				t.Errorf("request_duration_seconds{%s,%s} count = %v, want 1", tt.endpoint, tt.wantStream, v)
			}
		})
	}
}

// TestOverheadExcludesClientWrites checks that time spent writing to a slow
// client is not counted as gateway overhead.
func TestOverheadExcludesClientWrites(t *testing.T) {
	const slow = 50 * time.Millisecond
	tests := []struct {
		name   string
		body   string
		chunks []string
		writes int // client writes: each takes slow
	}{
		{name: "non-stream", body: `{"model":"gpt-test","messages":[{}]}`, writes: 1},
		{
			name:   "stream",
			body:   `{"model":"gpt-test","messages":[{}],"stream":true}`,
			chunks: []string{`{"choices":[{"index":0}]}`, `{"choices":[{"index":0}]}`},
			writes: 4, // start, two chunks, [DONE]
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, logs, _ := newTestGateway(t)
			var log []string
			g.adapters["fake"] = &fakeAdapter{log: &log, chunks: tt.chunks}
			ctx, ann := annotated(logs)
			g.ChatCompletions(ctx, &fakeExchange{body: []byte(tt.body), log: &log, header: http.Header{}, slow: slow})

			r := annotations(ann)
			latency, _ := r["latency_ms"].(float64)
			overhead, _ := r["overhead_ms"].(float64)
			if min := ms(time.Duration(tt.writes) * slow); latency < min {
				t.Fatalf("latency_ms = %v, want at least %v", latency, min)
			}
			// The gateway's own work takes microseconds; the margin is
			// for slow machines.
			if overhead >= ms(slow) {
				t.Errorf("overhead_ms = %v with latency_ms %v: the client writes were counted", overhead, latency)
			}
		})
	}
}

func TestStatusClass(t *testing.T) {
	for in, want := range map[int]string{200: "2xx", 404: "4xx", 499: "4xx", 502: "5xx", 0: "other", 999: "other"} {
		if got := statusClass(in); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", in, got, want)
		}
	}
}
