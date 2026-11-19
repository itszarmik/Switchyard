package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// chatAdapter is a scripted chat-only adapter that records what it was
// sent: it is neither a Completer nor an Embedder.
type chatAdapter struct {
	name   string
	body   string   // the non-stream answer; default a one-choice completion
	chunks []string // the stream; default two content chunks and a usage chunk
	usage  *schema.Usage
	calls  []call
}

// call is one recorded upstream call.
type call struct {
	kind  string // chat, chat-stream, completion, completion-stream, embed
	model string
	body  string // what an OpenAI-wire adapter would send
	req   *providers.Request
}

const (
	defaultChat = `{"id":"chatcmpl-abc","object":"chat.completion","created":7,"model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
)

var defaultChunks = []string{
	`{"id":"chatcmpl-abc","object":"chat.completion.chunk","created":7,"model":"up","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
	`{"id":"chatcmpl-abc","object":"chat.completion.chunk","created":7,"model":"up","choices":[{"index":0,"delta":{"content":"hel"}}]}`,
	`{"id":"chatcmpl-abc","object":"chat.completion.chunk","created":7,"model":"up","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
	`{"id":"chatcmpl-abc","object":"chat.completion.chunk","created":7,"model":"up","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
}

func (a *chatAdapter) Chat(_ context.Context, r *providers.Request) (*providers.Response, error) {
	a.calls = append(a.calls, call{kind: "chat", model: r.UpstreamModel, body: string(r.Chat.EncodeFor(r.UpstreamModel, !r.NoStreamUsage)), req: r})
	body := a.body
	if body == "" {
		body = defaultChat
	}
	resp := &providers.Response{Status: 200, Body: []byte(body), UpstreamRequestID: "rid", TTFT: time.Millisecond, Usage: a.usage}
	if a.usage == nil {
		var u struct{ Usage *schema.Usage }
		_ = json.Unmarshal([]byte(body), &u)
		resp.Usage = u.Usage
	}
	return resp, nil
}

func (a *chatAdapter) ChatStream(_ context.Context, r *providers.Request) (providers.Stream, error) {
	a.calls = append(a.calls, call{kind: "chat-stream", model: r.UpstreamModel, body: string(r.Chat.EncodeFor(r.UpstreamModel, !r.NoStreamUsage)), req: r})
	chunks := a.chunks
	if chunks == nil {
		chunks = defaultChunks
	}
	return &scriptStream{chunks: chunks}, nil
}

// fullAdapter also serves legacy completions natively and embeddings.
type fullAdapter struct{ chatAdapter }

func (a *fullAdapter) Completion(_ context.Context, r *providers.CompletionRequest) (*providers.Response, error) {
	a.calls = append(a.calls, call{kind: "completion", model: r.UpstreamModel, body: string(r.Completion.Encode(r.UpstreamModel, !r.NoStreamUsage))})
	return &providers.Response{Status: 200, Body: []byte(`{"object":"text_completion","native":true}`)}, nil
}

func (a *fullAdapter) CompletionStream(_ context.Context, r *providers.CompletionRequest) (providers.Stream, error) {
	a.calls = append(a.calls, call{kind: "completion-stream", model: r.UpstreamModel, body: string(r.Completion.Encode(r.UpstreamModel, !r.NoStreamUsage))})
	return &scriptStream{chunks: []string{`{"object":"text_completion","choices":[{"text":"n"}]}`}}, nil
}

func (a *fullAdapter) Embed(_ context.Context, r *providers.EmbedRequest) (*providers.Response, error) {
	a.calls = append(a.calls, call{kind: "embed", model: r.UpstreamModel, body: string(r.Embedding.Encode(r.UpstreamModel))})
	return &providers.Response{Status: 200, Body: []byte(`{"object":"list","usage":{"prompt_tokens":9,"total_tokens":9}}`),
		Usage: &schema.Usage{PromptTokens: 9, TotalTokens: 9}}, nil
}

// scriptStream replays chunks, keeping the last usage it saw.
type scriptStream struct {
	chunks []string
	i      int
	usage  *schema.Usage
}

func (s *scriptStream) Next() (*schema.ChatChunk, error) {
	if s.i >= len(s.chunks) {
		return nil, io.EOF
	}
	c, err := schema.ParseChatChunk([]byte(s.chunks[s.i]))
	if err != nil {
		return nil, err
	}
	s.i++
	if c.Usage != nil {
		s.usage = c.Usage
	}
	return c, nil
}
func (s *scriptStream) Usage() *schema.Usage      { return s.usage }
func (s *scriptStream) UpstreamRequestID() string { return "rid-s" }
func (s *scriptStream) TTFT() time.Duration       { return time.Millisecond }
func (s *scriptStream) Close() error              { return nil }

// routingConfig is a catalog with a model on two providers of different
// prices and capabilities, a chat-only model, an embedding model on two
// providers and aliases.
func routingConfig(drop bool) *config.Config {
	withPrice := func(d config.Deployment, in, out string) config.Deployment {
		p := price(in, out)
		d.Pricing = p
		return d
	}
	embedPrice := func(in string) config.Pricing {
		p, err := config.ParsePrice(in)
		if err != nil {
			panic(err)
		}
		return config.Pricing{Input: p}
	}
	return &config.Config{
		Providers: []config.Provider{
			{ID: "cheap", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"},
			{ID: "rich", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"},
			{ID: "claude", Type: "anthropic", BaseURL: "http://127.0.0.1:1"},
		},
		Models: []config.Model{
			{
				ID: "duo", ContextWindow: 100000, MaxOutputTokens: 4000,
				Capabilities: []string{"streaming", "stream_usage", "tools", "sampling"},
				Deployments: []config.Deployment{
					// Config order puts the pricier one first.
					withPrice(config.Deployment{Provider: "rich", UpstreamModel: "duo-rich",
						Capabilities: []string{"streaming", "stream_usage", "tools", "sampling", "n", "seed", "temperature_2", "vision"}}, "5", "10"),
					withPrice(config.Deployment{Provider: "cheap", UpstreamModel: "duo-cheap"}, "1", "2"),
				},
			},
			{
				ID: "sonnet", Capabilities: []string{"streaming", "stream_usage", "tools", "json_schema"},
				Deployments: []config.Deployment{withPrice(config.Deployment{Provider: "claude", UpstreamModel: "claude-x"}, "3", "15")},
			},
			{
				ID: "plain", Capabilities: []string{"streaming"},
				Deployments: []config.Deployment{withPrice(config.Deployment{Provider: "cheap", UpstreamModel: "plain-up"}, "1", "1")},
			},
			{
				ID: "legacy", Capabilities: []string{"streaming", "stream_usage", "completions"},
				Deployments: []config.Deployment{withPrice(config.Deployment{Provider: "rich", UpstreamModel: "legacy-up"}, "1", "1")},
			},
			{
				ID: "embed", Type: "embedding",
				Deployments: []config.Deployment{
					{Provider: "claude", UpstreamModel: "never", Pricing: embedPrice("0.001")},
					{Provider: "rich", UpstreamModel: "embed-rich", Pricing: embedPrice("0.5")},
					{Provider: "cheap", UpstreamModel: "embed-cheap", Pricing: embedPrice("0.1")},
				},
			},
		},
		Aliases: map[string]string{"best": "duo@rich", "fast": "duo"},
		Routing: config.Routing{DropUnsupportedParams: drop},
	}
}

// routingEnv is a gateway over routingConfig with scripted adapters.
type routingEnv struct {
	g      *Gateway
	cheap  *fullAdapter
	rich   *fullAdapter
	claude *chatAdapter
	logs   *logBuffer
}

func newRoutingEnv(t *testing.T, drop bool) *routingEnv {
	t.Helper()
	logs := &logBuffer{}
	g, err := New(Options{Config: routingConfig(drop), Logger: slog.New(slog.NewJSONHandler(logs, nil)), Registry: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	e := &routingEnv{g: g, cheap: &fullAdapter{chatAdapter{name: "cheap"}}, rich: &fullAdapter{chatAdapter{name: "rich"}}, claude: &chatAdapter{name: "claude"}, logs: logs}
	g.current().adapters["cheap"], g.current().adapters["rich"], g.current().adapters["claude"] = e.cheap, e.rich, e.claude
	return e
}

// exchangeResult is what a recorded exchange saw.
type exchangeResult struct {
	info   Info
	status int
	err    *Error
	body   string
	events []string
	ann    map[string]any
}

// recExchange records one exchange.
type recExchange struct {
	body []byte
	res  exchangeResult
}

func (x *recExchange) Body() ([]byte, *Error) { return x.body, nil }
func (x *recExchange) Header() http.Header    { return http.Header{} }
func (x *recExchange) Respond(info Info, status int, body []byte) {
	x.res.info, x.res.status, x.res.body = info, status, string(body)
}
func (x *recExchange) RespondError(info Info, e *Error) {
	x.res.info, x.res.status, x.res.err = info, e.Status, e
}
func (x *recExchange) StartStream(info Info) error { x.res.info, x.res.status = info, 200; return nil }
func (x *recExchange) Comment(string) error        { return nil }
func (x *recExchange) Send(data []byte) error {
	x.res.events = append(x.res.events, string(data))
	return nil
}
func (x *recExchange) Done() error { x.res.events = append(x.res.events, "[DONE]"); return nil }

// do serves body on the endpoint and returns what the client saw.
func (e *routingEnv) do(endpoint, body string) exchangeResult {
	ctx, ann := annotated(e.logs)
	x := &recExchange{body: []byte(body)}
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

func TestRouting(t *testing.T) {
	msgs := `"messages":[{"role":"user","content":"hi"}]`
	tests := []struct {
		name         string
		drop         bool
		body         string
		wantStatus   int
		wantCode     string
		wantMessage  []string // substrings of the error message
		wantProvider string
		wantUpstream string
		wantAlias    string
		wantDropped  []string
		wantExcluded int
		absent       []string // fields that must not reach the upstream
	}{
		{name: "cheapest of two", body: `{"model":"duo",` + msgs + `}`, wantStatus: 200, wantProvider: "cheap", wantUpstream: "duo-cheap"},
		{name: "model@provider pins the other", body: `{"model":"duo@rich",` + msgs + `}`, wantStatus: 200, wantProvider: "rich", wantUpstream: "duo-rich"},
		{name: "alias to a pinned deployment", body: `{"model":"best",` + msgs + `}`, wantStatus: 200, wantProvider: "rich", wantUpstream: "duo-rich", wantAlias: "best"},
		{name: "alias to a model", body: `{"model":"fast",` + msgs + `}`, wantStatus: 200, wantProvider: "cheap", wantAlias: "fast"},
		{
			name: "capable deployment chosen over the cheaper one", body: `{"model":"duo","n":2,` + msgs + `}`,
			wantStatus: 200, wantProvider: "rich", wantUpstream: "duo-rich", wantExcluded: 1,
		},
		{
			name: "image on a text-only deployment", body: `{"model":"duo","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:x"}}]}]}`,
			wantStatus: 200, wantProvider: "rich", wantExcluded: 1,
		},
		{
			name: "image hidden behind a case-folded type", body: `{"model":"duo","messages":[{"role":"user","content":[{"type":"image_url","TYPE":"text","image_url":{"url":"https://x/y.png"}}]}]}`,
			wantStatus: 200, wantProvider: "rich", wantExcluded: 1,
		},
		{
			name: "json_schema hidden behind a case-folded type", body: `{"model":"duo","response_format":{"type":"json_schema","Type":"text","json_schema":{"name":"x","schema":{}}},` + msgs + `}`,
			wantStatus: 400, wantCode: "no_eligible_model", wantMessage: []string{"lacks capability json_schema"}, wantExcluded: 2,
		},
		{
			name: "image on a text-only model", body: `{"model":"plain","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:x"}}]}]}`,
			wantStatus: 400, wantCode: "no_eligible_model", wantMessage: []string{"plain@cheap (lacks capability vision)"}, wantExcluded: 1,
		},
		{
			name: "n=2 on an Anthropic-only model", body: `{"model":"sonnet","n":2,` + msgs + `}`,
			wantStatus: 400, wantCode: "no_eligible_model", wantMessage: []string{"sonnet@claude (lacks capability n)"}, wantExcluded: 1,
		},
		{
			name: "temperature 1.5 goes to the deployment that accepts it", body: `{"model":"duo","temperature":1.5,` + msgs + `}`,
			wantStatus: 200, wantProvider: "rich", wantExcluded: 1,
		},
		{
			name: "temperature 1.5 on a model without sampling is dropped", body: `{"model":"sonnet","temperature":1.5,"top_p":0.5,` + msgs + `}`,
			wantStatus: 200, wantProvider: "claude", wantDropped: []string{"temperature", "top_p"}, absent: []string{"temperature", "top_p"},
		},
		{
			name: "logprobs and penalties on an Anthropic-only model", body: `{"model":"sonnet","logprobs":true,"top_logprobs":2,"frequency_penalty":0.5,` + msgs + `}`,
			wantStatus: 400, wantCode: "no_eligible_model", wantMessage: []string{"sonnet@claude (lacks capabilities logprobs, penalties)"}, wantExcluded: 1,
		},
		{
			name: "logprobs and penalties are dropped when allowed", drop: true, body: `{"model":"sonnet","logprobs":true,"top_logprobs":2,"frequency_penalty":0.5,"presence_penalty":0,"parallel_tool_calls":true,` + msgs + `}`,
			wantStatus: 200, wantProvider: "claude",
			wantDropped: []string{"logprobs", "top_logprobs", "parallel_tool_calls", "frequency_penalty", "presence_penalty"},
			absent:      []string{"logprobs", "top_logprobs", "frequency_penalty", "presence_penalty", "parallel_tool_calls"},
		},
		{
			name: "drop_unsupported_params drops n", drop: true, body: `{"model":"sonnet","n":2,"seed":4,` + msgs + `}`,
			wantStatus: 200, wantProvider: "claude", wantDropped: []string{"n", "seed"}, absent: []string{"n", "seed"},
		},
		{
			name: "drop prefers the cheapest, dropping what it lacks", drop: true, body: `{"model":"duo","n":2,` + msgs + `}`,
			wantStatus: 200, wantProvider: "cheap", wantDropped: []string{"n"}, absent: []string{"n"},
		},
		{
			name: "hard requirements are never dropped", drop: true, body: `{"model":"plain","tools":[{"type":"function","function":{"name":"f"}}],` + msgs + `}`,
			wantStatus: 400, wantCode: "no_eligible_model", wantMessage: []string{"lacks capability tools"}, wantExcluded: 1,
		},
		{
			name: "forced tool on a model without forced_tools", body: `{"model":"sonnet","tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"required",` + msgs + `}`,
			wantStatus: 400, wantCode: "no_eligible_model", wantMessage: []string{"lacks capability forced_tools"}, wantExcluded: 1,
		},
		{
			name: "context window", body: `{"model":"duo","max_tokens":200000,` + msgs + `}`,
			wantStatus: 400, wantCode: "no_eligible_model", wantMessage: []string{"duo@rich (context 100000 < ", "duo@cheap (context 100000 < "}, wantExcluded: 2,
		},
		{
			name: "max_tokens that overflows the estimate", body: `{"model":"duo","max_tokens":9223372036854775807,` + msgs + `}`,
			wantStatus: 400, wantCode: "no_eligible_model", wantMessage: []string{"duo@rich (context 100000 < ", "duo@cheap (context 100000 < "}, wantExcluded: 2,
		},
		{name: "virtual model", body: `{"model":"auto:cheap",` + msgs + `}`, wantStatus: 400, wantCode: "model_not_supported", wantMessage: []string{"this policy has no baseline_model; auto routing is disabled"}},
		{name: "embedding model on chat", body: `{"model":"embed",` + msgs + `}`, wantStatus: 400, wantCode: "model_not_supported", wantMessage: []string{"embedding model"}},
		{name: "unknown model", body: `{"model":"nope",` + msgs + `}`, wantStatus: 404, wantCode: "model_not_found"},
		{name: "unknown deployment", body: `{"model":"duo@claude",` + msgs + `}`, wantStatus: 404, wantCode: "model_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newRoutingEnv(t, tt.drop)
			r := e.do(EndpointChatCompletions, tt.body)
			if r.status != tt.wantStatus {
				t.Fatalf("status %d (%+v), want %d", r.status, r.err, tt.wantStatus)
			}
			if r.err != nil {
				if r.err.Code != tt.wantCode {
					t.Errorf("code %q, want %q", r.err.Code, tt.wantCode)
				}
				if r.err.Type != "invalid_request_error" {
					t.Errorf("type %q", r.err.Type)
				}
				for _, m := range tt.wantMessage {
					if !strings.Contains(r.err.Message, m) {
						t.Errorf("message %q, want it to contain %q", r.err.Message, m)
					}
				}
			}
			if r.info.Provider != tt.wantProvider || r.info.Alias != tt.wantAlias {
				t.Errorf("info %+v, want provider %q, alias %q", r.info, tt.wantProvider, tt.wantAlias)
			}
			if diff := cmp.Diff(tt.wantDropped, r.info.DroppedParams); diff != "" {
				t.Errorf("dropped (-want +got):\n%s", diff)
			}
			if got, ok := r.ann["dropped_params"]; ok != (len(tt.wantDropped) > 0) || ok && got != strings.Join(tt.wantDropped, ",") {
				t.Errorf("dropped_params annotation %q", got)
			}
			if got, ok := r.ann["alias"]; ok != (tt.wantAlias != "") || ok && got != tt.wantAlias {
				t.Errorf("alias annotation %q", got)
			}
			if got := r.ann["excluded"]; got != int64(tt.wantExcluded) {
				t.Errorf("excluded annotation %v, want %d", got, tt.wantExcluded)
			}
			if tt.wantStatus != 200 {
				for _, a := range []*chatAdapter{&e.cheap.chatAdapter, &e.rich.chatAdapter, e.claude} {
					if len(a.calls) != 0 {
						t.Errorf("%s was called: %+v", a.name, a.calls)
					}
				}
				return
			}
			var got []call
			for _, a := range []*chatAdapter{&e.cheap.chatAdapter, &e.rich.chatAdapter, e.claude} {
				got = append(got, a.calls...)
			}
			if len(got) != 1 {
				t.Fatalf("%d upstream calls, want 1", len(got))
			}
			if tt.wantUpstream != "" && got[0].model != tt.wantUpstream {
				t.Errorf("upstream model %q, want %q", got[0].model, tt.wantUpstream)
			}
			if r.info.Model != "duo" && r.info.Model != "sonnet" && r.info.Model != "plain" {
				t.Errorf("info model %q", r.info.Model)
			}
			for _, f := range tt.absent {
				if _, ok := got[0].req.Chat.Field(f); ok {
					t.Errorf("the adapter saw %s", f)
				}
				if strings.Contains(got[0].body, `"`+f+`"`) {
					t.Errorf("the encoded body has %s: %s", f, got[0].body)
				}
			}
			if d := got[0].req; d.MaxOutputTokens == 0 && r.info.Model == "duo" || d.Capabilities == nil {
				t.Errorf("request lacks the deployment info: max %d, caps %v", d.MaxOutputTokens, d.Capabilities)
			}
		})
	}
}

func TestStreamUsageInjection(t *testing.T) {
	e := newRoutingEnv(t, false)
	// plain lacks stream_usage: stream_options is forwarded as sent.
	e.do(EndpointChatCompletions, `{"model":"plain","stream":true,"messages":[{"role":"user","content":"x"}]}`)
	e.do(EndpointChatCompletions, `{"model":"plain","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"x"}]}`)
	// duo has it: include_usage is injected.
	e.do(EndpointChatCompletions, `{"model":"duo","stream":true,"messages":[{"role":"user","content":"x"}]}`)
	calls := e.cheap.calls
	if len(calls) != 3 {
		t.Fatalf("%d calls", len(calls))
	}
	if strings.Contains(calls[0].body, "stream_options") {
		t.Errorf("stream_options injected for a deployment without stream_usage: %s", calls[0].body)
	}
	if !strings.Contains(calls[1].body, `"stream_options":{"include_usage":true}`) {
		t.Errorf("the client's stream_options were not forwarded: %s", calls[1].body)
	}
	if !strings.Contains(calls[2].body, `"stream_options":{"include_usage":true}`) {
		t.Errorf("include_usage not injected: %s", calls[2].body)
	}
}

func TestUsageUnknownIsOmitted(t *testing.T) {
	e := newRoutingEnv(t, false)
	e.cheap.chunks = defaultChunks[:3] // no usage chunk
	r := e.do(EndpointChatCompletions, `{"model":"plain","stream":true,"messages":[{"role":"user","content":"x"}]}`)
	for _, k := range []string{"prompt_tokens", "completion_tokens"} {
		if v, ok := r.ann[k]; ok {
			t.Errorf("%s = %v for a stream without usage, want it omitted", k, v)
		}
	}
	// A request that never reached the upstream spent nothing.
	r = e.do(EndpointChatCompletions, `{"model":"nope","messages":[{"role":"user","content":"x"}]}`)
	if r.ann["prompt_tokens"] != int64(0) {
		t.Errorf("prompt_tokens = %v for a rejected request, want 0", r.ann["prompt_tokens"])
	}
	// With usage, the tokens are logged.
	e.cheap.chunks = nil
	r = e.do(EndpointChatCompletions, `{"model":"duo","stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if r.ann["prompt_tokens"] != int64(3) || r.ann["completion_tokens"] != int64(2) {
		t.Errorf("tokens %v/%v, want 3/2", r.ann["prompt_tokens"], r.ann["completion_tokens"])
	}
	if r.ann["deployment"] != "duo@cheap" || r.ann["excluded"] != int64(0) {
		t.Errorf("annotations %v", r.ann)
	}
	for _, k := range []string{"alias", "dropped_params"} {
		if v, ok := r.ann[k]; ok {
			t.Errorf("%s = %v, want it omitted when unset", k, v)
		}
	}
}

func TestCompletionsShim(t *testing.T) {
	t.Run("non-stream", func(t *testing.T) {
		e := newRoutingEnv(t, false)
		r := e.do(EndpointCompletions, `{"model":"duo","prompt":"Say hi","max_tokens":5,"temperature":0.2,"stop":["x"],"user":"u","logit_bias":{}}`)
		if r.status != 200 || r.info.Shim != "chat" || r.info.Provider != "cheap" {
			t.Fatalf("status %d, info %+v, err %+v", r.status, r.info, r.err)
		}
		want := `{"id":"cmpl-abc","object":"text_completion","created":7,"model":"up","choices":[{"text":"hello","index":0,"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
		if r.body != want {
			t.Errorf("body\n got %s\nwant %s", r.body, want)
		}
		c := e.cheap.calls
		if len(c) != 1 || c[0].kind != "chat" {
			t.Fatalf("calls %+v", c)
		}
	})
	t.Run("stream", func(t *testing.T) {
		e := newRoutingEnv(t, false)
		r := e.do(EndpointCompletions, `{"model":"duo","prompt":["Say hi"],"stream":true,"stream_options":{"include_usage":true}}`)
		if r.status != 200 || r.info.Shim != "chat" {
			t.Fatalf("status %d, info %+v, err %+v", r.status, r.info, r.err)
		}
		want := []string{
			`{"id":"cmpl-abc","object":"text_completion","created":7,"model":"up","choices":[{"text":"hel","index":0,"logprobs":null,"finish_reason":null}]}`,
			`{"id":"cmpl-abc","object":"text_completion","created":7,"model":"up","choices":[{"text":"lo","index":0,"logprobs":null,"finish_reason":"stop"}]}`,
			`{"id":"cmpl-abc","object":"text_completion","created":7,"model":"up","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
			"[DONE]",
		}
		if diff := cmp.Diff(want, r.events); diff != "" {
			t.Errorf("events (-want +got):\n%s", diff)
		}
		var sent struct {
			Messages []struct{ Role, Content string }
		}
		if err := json.Unmarshal([]byte(e.cheap.calls[0].body), &sent); err != nil || len(sent.Messages) != 1 ||
			sent.Messages[0].Role != "user" || sent.Messages[0].Content != "Say hi" {
			t.Errorf("chat body %s", e.cheap.calls[0].body)
		}
	})
	t.Run("field mapping", func(t *testing.T) {
		e := newRoutingEnv(t, false)
		e.do(EndpointCompletions, `{"model":"duo@rich","prompt":"p","max_tokens":5,"temperature":0.2,"top_p":0.9,"stop":"x","n":2,"user":"u","seed":3,"logit_bias":{"1":2},"echo":false}`)
		var got map[string]json.RawMessage
		if err := json.Unmarshal([]byte(e.rich.calls[0].body), &got); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"model", "messages", "max_tokens", "temperature", "top_p", "stop", "n", "user", "seed"} {
			if _, ok := got[k]; !ok {
				t.Errorf("chat request lacks %s: %s", k, e.rich.calls[0].body)
			}
		}
		for _, k := range []string{"prompt", "logit_bias", "echo"} {
			if _, ok := got[k]; ok {
				t.Errorf("chat request has %s", k)
			}
		}
	})
	for _, tc := range []struct{ name, body, param string }{
		{"echo", `{"model":"duo","prompt":"p","echo":true}`, "echo"},
		{"suffix", `{"model":"duo","prompt":"p","suffix":"s"}`, "suffix"},
		{"logprobs", `{"model":"duo","prompt":"p","logprobs":2}`, "logprobs"},
		{"best_of", `{"model":"duo","prompt":"p","best_of":3}`, "best_of"},
		{"several prompts", `{"model":"duo","prompt":["a","b"]}`, "prompt"},
	} {
		t.Run("shim rejects "+tc.name, func(t *testing.T) {
			e := newRoutingEnv(t, false)
			r := e.do(EndpointCompletions, tc.body)
			if r.status != 400 || r.err.Code != "unsupported_parameter" || r.err.Param != tc.param || r.err.Type != "invalid_request_error" {
				t.Errorf("status %d, error %+v", r.status, r.err)
			}
			if len(e.cheap.calls)+len(e.rich.calls) != 0 {
				t.Error("the upstream was called")
			}
		})
	}
	t.Run("token arrays are rejected", func(t *testing.T) {
		e := newRoutingEnv(t, false)
		r := e.do(EndpointCompletions, `{"model":"legacy","prompt":[1,2,3]}`)
		if r.status != 400 || r.err.Param != "prompt" || r.err.Code != "invalid_type" {
			t.Errorf("status %d, error %+v", r.status, r.err)
		}
	})
	t.Run("native deployment", func(t *testing.T) {
		e := newRoutingEnv(t, false)
		r := e.do(EndpointCompletions, `{"model":"legacy","prompt":["a","b"],"echo":true,"x_extra":12345678901234567890}`)
		if r.status != 200 || r.info.Shim != "" || r.body != `{"object":"text_completion","native":true}` {
			t.Fatalf("status %d, info %+v, body %s, err %+v", r.status, r.info, r.body, r.err)
		}
		if c := e.rich.calls; len(c) != 1 || c[0].kind != "completion" ||
			c[0].body != `{"model":"legacy-up","prompt":["a","b"],"echo":true,"x_extra":12345678901234567890}` {
			t.Errorf("calls %+v", c)
		}
		r = e.do(EndpointCompletions, `{"model":"legacy","prompt":"a","stream":true}`)
		if r.status != 200 || len(r.events) != 2 {
			t.Errorf("stream: status %d, events %q", r.status, r.events)
		}
		if c := e.rich.calls[1]; c.kind != "completion-stream" || !strings.Contains(c.body, `"stream_options":{"include_usage":true}`) {
			t.Errorf("stream call %+v", c)
		}
	})
	t.Run("embedding model", func(t *testing.T) {
		e := newRoutingEnv(t, false)
		r := e.do(EndpointCompletions, `{"model":"embed","prompt":"a"}`)
		if r.status != 400 || r.err.Code != "model_not_supported" {
			t.Errorf("status %d, error %+v", r.status, r.err)
		}
	})
	t.Run("capabilities apply through the shim", func(t *testing.T) {
		e := newRoutingEnv(t, false)
		r := e.do(EndpointCompletions, `{"model":"sonnet","prompt":"a","n":2}`)
		if r.status != 400 || r.err.Code != "no_eligible_model" {
			t.Errorf("status %d, error %+v", r.status, r.err)
		}
		e = newRoutingEnv(t, true)
		r = e.do(EndpointCompletions, `{"model":"sonnet","prompt":"a","n":2,"temperature":0.5}`)
		if r.status != 200 || r.info.Shim != "chat" || !cmp.Equal(r.info.DroppedParams, []string{"n", "temperature"}) {
			t.Errorf("status %d, info %+v, error %+v", r.status, r.info, r.err)
		}
		if _, ok := e.claude.calls[0].req.Chat.Field("n"); ok {
			t.Error("n reached the adapter")
		}
	})
}

func TestEmbeddings(t *testing.T) {
	e := newRoutingEnv(t, false)
	r := e.do(EndpointEmbeddings, `{"model":"embed","input":["a","b"],"encoding_format":"float","dimensions":8,"user":"u"}`)
	if r.status != 200 || r.info.Provider != "cheap" || r.info.Model != "embed" {
		t.Fatalf("status %d, info %+v, err %+v", r.status, r.info, r.err)
	}
	if c := e.cheap.calls; len(c) != 1 || c[0].kind != "embed" ||
		c[0].body != `{"model":"embed-cheap","input":["a","b"],"encoding_format":"float","dimensions":8,"user":"u"}` {
		t.Errorf("calls %+v", c)
	}
	// The anthropic deployment is the cheapest but cannot embed.
	if r.ann["excluded"] != int64(1) || r.ann["prompt_tokens"] != int64(9) {
		t.Errorf("annotations %v", r.ann)
	}
	if len(e.claude.calls) != 0 {
		t.Error("the anthropic adapter was called")
	}

	r = e.do(EndpointEmbeddings, `{"model":"embed@rich","input":[[1,2],[3]]}`)
	if r.status != 200 || r.info.Provider != "rich" || e.rich.calls[0].model != "embed-rich" {
		t.Errorf("pinned: status %d, info %+v", r.status, r.info)
	}
	r = e.do(EndpointEmbeddings, `{"model":"embed@claude","input":"a"}`)
	if r.status != 400 || r.err.Code != "no_eligible_model" || !strings.Contains(r.err.Message, "provider does not serve embeddings") {
		t.Errorf("anthropic pinned: status %d, err %+v", r.status, r.err)
	}
	for _, tc := range []struct{ body, code, param string }{
		{`{"model":"duo","input":"a"}`, "model_not_supported", "model"},
		{`{"model":"embed"}`, "missing_required_parameter", "input"},
		{`{"model":"embed","input":[]}`, "empty_array", "input"},
		{`{"model":"embed","input":[1,"a"]}`, "invalid_type", "input"},
		{`{"model":"embed","input":"a","encoding_format":"int8"}`, "invalid_value", "encoding_format"},
		{`{"model":"embed","input":"a","dimensions":0}`, "invalid_value", "dimensions"},
		{`{"model":"embed","input":"a","dimensions":"8"}`, "invalid_type", "dimensions"},
		{`{"model":"embed","input":"a","user":1}`, "invalid_type", "user"},
	} {
		r := e.do(EndpointEmbeddings, tc.body)
		if r.status != 400 || r.err.Code != tc.code || r.err.Param != tc.param {
			t.Errorf("%s: status %d, err %+v, want %s on %s", tc.body, r.status, r.err, tc.code, tc.param)
		}
	}
}

func TestListModelsAndAliases(t *testing.T) {
	e := newRoutingEnv(t, false)
	var ids []string
	for _, m := range e.g.ListModels(context.Background()) {
		ids = append(ids, m.ID)
	}
	if diff := cmp.Diff([]string{"duo", "sonnet", "plain", "legacy", "embed", "best", "fast"}, ids); diff != "" {
		t.Errorf("models (-want +got):\n%s", diff)
	}
}

func TestEstimates(t *testing.T) {
	m := &catalog.Model{TokenRatio: 1}
	m2 := &catalog.Model{TokenRatio: 1.5}
	in := []struct {
		name              string
		n, images, inline int
		model             *catalog.Model
		want              int64
	}{
		{name: "text", n: 4000, model: m, want: 1000},
		{name: "token ratio", n: 4000, model: m2, want: 1500},
		{name: "inline image bytes are not text", n: 4_000_400, images: 1, inline: 4_000_000, model: m, want: 100 + imageTokens},
		{name: "an image by URL", n: 400, images: 1, model: m2, want: 150 + imageTokens},
		{name: "token ratio does not scale images", n: 0, images: 4, model: m2, want: 4 * imageTokens},
		{name: "inline larger than the body", n: 10, images: 1, inline: 20, model: m, want: imageTokens},
	}
	for _, tt := range in {
		t.Run("in/"+tt.name, func(t *testing.T) {
			rec := &record{}
			rec.inBytes, rec.inImages = inputSize(tt.n, tt.images, tt.inline)
			if got := rec.estIn(tt.model); got != tt.want {
				t.Errorf("estIn = %d, want %d", got, tt.want)
			}
		})
	}
	out := []struct {
		name    string
		max     int64
		ok      bool
		outputs int64
		want    int64
	}{
		{name: "unset", want: defaultEstOut},
		{name: "zero", max: 0, ok: true, want: defaultEstOut},
		{name: "set", max: 1234, ok: true, want: 1234},
		{name: "clamped", max: math.MaxInt64, ok: true, want: maxEstOut},
		{name: "n", max: 10, ok: true, outputs: 100, want: 1000},
		{name: "n unset max", outputs: 3, want: 3 * defaultEstOut},
		{name: "n clamped", max: maxEstOut / 2, ok: true, outputs: 3, want: maxEstOut},
		{name: "n huge", max: 2, ok: true, outputs: math.MaxInt64, want: maxEstOut},
	}
	for _, tt := range out {
		t.Run("out/"+tt.name, func(t *testing.T) {
			if got := estimateOut(tt.max, tt.ok, tt.outputs); got != tt.want {
				t.Errorf("estimateOut = %d, want %d", got, tt.want)
			}
		})
	}
}
