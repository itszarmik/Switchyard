package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"

	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
	"switchyard/test/fakeanthropic"
)

const streamBody = `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`

const streamBodyUsage = `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`

// chunks builds the expected chunks of one stream: each delta (a JSON
// object, or "finish:<reason>" for the finish chunk), then the usage chunk
// when usage is not "".
func chunks(id, model string, includeUsage bool, usage string, deltas ...string) string {
	usageField := ""
	if includeUsage {
		usageField = `,"usage":null`
	}
	var out []string
	for _, d := range deltas {
		finish := "null"
		if reason, ok := strings.CutPrefix(d, "finish:"); ok {
			d, finish = "{}", `"`+reason+`"`
		}
		out = append(out, `{"id":"`+id+`","object":"chat.completion.chunk","created":1794398400,"model":"`+model+
			`","choices":[{"index":0,"delta":`+d+`,"logprobs":null,"finish_reason":`+finish+`}]`+usageField+`}`)
	}
	if includeUsage {
		out = append(out, `{"id":"`+id+`","object":"chat.completion.chunk","created":1794398400,"model":"`+model+
			`","choices":[],"usage":`+usage+`}`)
	}
	return "[" + strings.Join(out, ",") + "]"
}

const roleDelta = `{"role":"assistant","content":""}`

// TestStreamGolden is the golden test of the stream translation: each
// handcrafted event stream, replayed by the fake, must become these
// chat.completion.chunk events, with and without the usage chunk, and the
// official SDK's accumulator must rebuild the answer from them.
func TestStreamGolden(t *testing.T) {
	tests := []struct {
		fixture string
		body    func(includeUsage bool) string
		caps    []schema.Capability
		id      string
		model   string
		usage   string
		deltas  []string
		// The SDK accumulator's view.
		content   string
		toolCalls []string // "id name arguments"
		finish    string
	}{
		{
			fixture: "stream_text.sse",
			id:      "chatcmpl-msg_1nZdL29xx5MUA1yADyHTEsnR8uuvGzszyY", model: "claude-opus-5-5",
			usage:   `{"prompt_tokens":25,"completion_tokens":15,"total_tokens":40,"prompt_tokens_details":{"cached_tokens":0}}`,
			deltas:  []string{roleDelta, `{"content":"Hello"}`, `{"content":"!"}`, "finish:stop"},
			content: "Hello!", finish: "stop",
		},
		{
			fixture: "stream_tool_use.sse",
			id:      "chatcmpl-msg_014p7gG3wDgGV9EUtLvnow3U", model: "claude-opus-5",
			usage: `{"prompt_tokens":472,"completion_tokens":89,"total_tokens":561,"prompt_tokens_details":{"cached_tokens":0}}`,
			deltas: []string{
				roleDelta, `{"content":"Okay"}`, `{"content":", let's check:"}`,
				`{"tool_calls":[{"index":0,"id":"toolu_01T1x1fJ34qAmk2tNTrN7Up6","type":"function","function":{"name":"get_weather","arguments":""}}]}`,
				`{"tool_calls":[{"index":0,"function":{"arguments":"{\"location\":"}}]}`,
				`{"tool_calls":[{"index":0,"function":{"arguments":" \"San"}}]}`,
				`{"tool_calls":[{"index":0,"function":{"arguments":" Francisc"}}]}`,
				`{"tool_calls":[{"index":0,"function":{"arguments":"o,"}}]}`,
				`{"tool_calls":[{"index":0,"function":{"arguments":" CA\"}"}}]}`,
				"finish:tool_calls",
			},
			content:   "Okay, let's check:",
			toolCalls: []string{`toolu_01T1x1fJ34qAmk2tNTrN7Up6 get_weather {"location": "San Francisco, CA"}`},
			finish:    "tool_calls",
		},
		{
			fixture: "stream_parallel_tool_use.sse",
			id:      "chatcmpl-msg_01ParallelStream", model: "claude-opus-5-5",
			usage: `{"prompt_tokens":510,"completion_tokens":120,"total_tokens":630,"prompt_tokens_details":{"cached_tokens":0}}`,
			deltas: []string{
				roleDelta,
				`{"tool_calls":[{"index":0,"id":"toolu_01Weather","type":"function","function":{"name":"get_weather","arguments":""}}]}`,
				`{"tool_calls":[{"index":0,"function":{"arguments":"{\"location\": \"Paris, France\"}"}}]}`,
				`{"tool_calls":[{"index":1,"id":"toolu_02Time","type":"function","function":{"name":"get_time","arguments":""}}]}`,
				`{"tool_calls":[{"index":1,"function":{"arguments":"{\"timezone\":"}}]}`,
				`{"tool_calls":[{"index":1,"function":{"arguments":" \"Europe/Paris\"}"}}]}`,
				"finish:tool_calls",
			},
			toolCalls: []string{
				`toolu_01Weather get_weather {"location": "Paris, France"}`,
				`toolu_02Time get_time {"timezone": "Europe/Paris"}`,
			},
			finish: "tool_calls",
		},
		{
			fixture: "stream_thinking.sse",
			id:      "chatcmpl-msg_01ThinkingStream", model: "claude-opus-5-5",
			usage: `{"prompt_tokens":40,"completion_tokens":310,"total_tokens":350,"prompt_tokens_details":{"cached_tokens":0},
				"completion_tokens_details":{"reasoning_tokens":280}}`,
			deltas:  []string{roleDelta, `{"content":"The GCD is 21."}`, "finish:stop"},
			content: "The GCD is 21.", finish: "stop",
		},
		{
			fixture: "stream_cache_usage.sse",
			id:      "chatcmpl-msg_01CacheStream", model: "claude-opus-5-5",
			usage:   `{"prompt_tokens":6197,"completion_tokens":503,"total_tokens":6700,"prompt_tokens_details":{"cached_tokens":2051}}`,
			deltas:  []string{roleDelta, `{"content":"Hi!"}`, "finish:length"},
			content: "Hi!", finish: "length",
		},
	}
	f := newFake(t)
	a := newAdapter(t, testProvider(f.BaseURL()))
	for _, tt := range tests {
		for _, includeUsage := range []bool{false, true} {
			name := tt.fixture + map[bool]string{false: "/no usage", true: "/include_usage"}[includeUsage]
			t.Run(name, func(t *testing.T) {
				f.Reset()
				f.Script(fakeanthropic.Behavior{Replay: fixture(t, tt.fixture)})
				body := streamBody
				if includeUsage {
					body = streamBodyUsage
				}
				if tt.body != nil {
					body = tt.body(includeUsage)
				}
				s, err := a.ChatStream(ctxWithTimeout(t), request(t, body, tt.caps...))
				if err != nil {
					t.Fatalf("ChatStream: %v", err)
				}
				defer func() { _ = s.Close() }()
				got, err := collect(t, s)
				if !errors.Is(err, io.EOF) {
					t.Fatalf("stream ended with %v, want io.EOF", err)
				}
				want := chunks(tt.id, tt.model, includeUsage, tt.usage, tt.deltas...)
				gotJSON := "[" + strings.Join(got, ",") + "]"
				if diff := jsonDiff(t, []byte(want), []byte(gotJSON)); diff != "" {
					t.Errorf("chunks (-want +got):\n%s\ngot:\n%s", diff, strings.Join(got, "\n"))
				}
				var wantUsage schema.Usage
				if err := json.Unmarshal([]byte(tt.usage), &wantUsage); err != nil {
					t.Fatal(err)
				}
				if u := s.Usage(); u == nil || *u.PromptTokensDetails != *wantUsage.PromptTokensDetails ||
					u.PromptTokens != wantUsage.PromptTokens || u.CompletionTokens != wantUsage.CompletionTokens {
					t.Errorf("Usage() = %+v, want %s", u, tt.usage)
				}
				checkAccumulated(t, got, tt.content, tt.toolCalls, tt.finish)
			})
		}
	}
}

// checkAccumulated feeds the chunks to the openai-go SDK's accumulator and
// checks the answer it rebuilds.
func checkAccumulated(t *testing.T, raw []string, content string, toolCalls []string, finish string) {
	t.Helper()
	var acc openai.ChatCompletionAccumulator
	for i, r := range raw {
		var c openai.ChatCompletionChunk
		if err := json.Unmarshal([]byte(r), &c); err != nil {
			t.Fatalf("chunk %d %s: %v", i, r, err)
		}
		if !acc.AddChunk(c) {
			t.Fatalf("the SDK accumulator rejected chunk %d: %s", i, r)
		}
	}
	if len(acc.Choices) != 1 {
		t.Fatalf("accumulated %d choices, want 1", len(acc.Choices))
	}
	ch := acc.Choices[0]
	var calls []string
	for _, tc := range ch.Message.ToolCalls {
		var args any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			t.Errorf("tool call %s arguments %q are not JSON", tc.ID, tc.Function.Arguments)
		}
		calls = append(calls, tc.ID+" "+tc.Function.Name+" "+tc.Function.Arguments)
	}
	if ch.Message.Content != content || ch.FinishReason != finish || strings.Join(calls, "\n") != strings.Join(toolCalls, "\n") {
		t.Errorf("accumulated content %q, finish %q, tool calls %q; want %q, %q, %q",
			ch.Message.Content, ch.FinishReason, calls, content, finish, toolCalls)
	}
}

// TestStreamErrorEvent checks the documented mid-stream error: the chunks
// before it are delivered, then the stream fails with a server error that
// names the error type, and the failure is sticky.
func TestStreamErrorEvent(t *testing.T) {
	f := newFake(t)
	f.Script(fakeanthropic.Behavior{Replay: fixture(t, "stream_error_overloaded.sse")})
	a := newAdapter(t, testProvider(f.BaseURL()))
	s, err := a.ChatStream(ctxWithTimeout(t), request(t, streamBody))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer func() { _ = s.Close() }()
	got, err := collect(t, s)
	if len(got) != 2 {
		t.Errorf("%d chunks before the error, want 2: %v", len(got), got)
	}
	pe := wantKind(t, err, providers.KindServerError)
	if !strings.Contains(pe.Message, "error event (overloaded_error)") {
		t.Errorf("message = %q", pe.Message)
	}
	if _, again := s.Next(); !errors.Is(again, err) {
		t.Errorf("error is not sticky: %v then %v", err, again)
	}
}

const (
	evStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"
	evBlock = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	evText  = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	evPing  = "event: ping\ndata: {\"type\": \"ping\"}\n\n"
	evDelta = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n"
	evStop  = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	evError = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"boom\"}}\n\n"
)

// every writes ev every 10ms until the request ends, then runs after
// (which may be nil) once d has passed. With d 0 it never stops.
func every(ev string, d time.Duration, after []string) func(*upstream, http.ResponseWriter, *http.Request) {
	return func(_ *upstream, w http.ResponseWriter, r *http.Request) {
		var deadline <-chan time.Time
		if d > 0 {
			deadline = time.After(d)
		}
		for {
			if _, err := io.WriteString(w, ev); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-time.After(10 * time.Millisecond):
			case <-deadline:
				for _, e := range after {
					_, _ = io.WriteString(w, e)
				}
				w.(http.Flusher).Flush()
				return
			case <-r.Context().Done():
				return
			}
		}
	}
}

func TestStreamFailures(t *testing.T) {
	const short = 100 * time.Millisecond
	hang := func(u *upstream, _ http.ResponseWriter, r *http.Request) { u.hang(r) }
	tests := []struct {
		name string
		h    func(*upstream, http.ResponseWriter, *http.Request)
		set  func(*config.ProviderTimeouts)
		// before is the number of chunks read before the failure; -1 means
		// ChatStream itself fails, -2 any number.
		before  int
		kind    providers.Kind
		message string
	}{
		{
			name:   "first_token: headers but no event",
			h:      sseHandler(nil, hang),
			set:    func(p *config.ProviderTimeouts) { p.FirstToken = config.Duration(short) },
			before: -1, kind: providers.KindTimeout, message: "first_token timeout",
		},
		{
			name:   "first_token: no headers",
			h:      hang,
			set:    func(p *config.ProviderTimeouts) { p.FirstToken = config.Duration(short) },
			before: -1, kind: providers.KindTimeout, message: "first_token timeout",
		},
		{
			name:   "first_token: pings are not a first token",
			h:      sseHandler(nil, every(evPing, 0, nil)),
			set:    func(p *config.ProviderTimeouts) { p.FirstToken = config.Duration(short) },
			before: -1, kind: providers.KindTimeout, message: "first_token timeout",
		},
		{
			name:   "idle after message_start",
			h:      sseHandler([]string{evStart, evBlock}, hang),
			set:    func(p *config.ProviderTimeouts) { p.Idle = config.Duration(short) },
			before: 1, kind: providers.KindTimeout, message: "idle timeout",
		},
		{
			name:   "idle after a delta",
			h:      sseHandler([]string{evStart, evBlock, evText}, hang),
			set:    func(p *config.ProviderTimeouts) { p.Idle = config.Duration(short) },
			before: 2, kind: providers.KindTimeout, message: "idle timeout",
		},
		{
			name: "total over a stream kept alive by pings",
			h:    sseHandler([]string{evStart}, every(evPing, 0, nil)),
			set: func(p *config.ProviderTimeouts) {
				p.Total = config.Duration(300 * time.Millisecond)
				p.Idle = config.Duration(time.Second)
			},
			before: 1, kind: providers.KindTimeout, message: "total timeout",
		},
		{
			name:   "end without message_stop",
			h:      sseHandler([]string{evStart, evBlock, evText, evDelta}, nil),
			before: 3, kind: providers.KindServerError, message: "without message_stop",
		},
		{
			name:   "error event first",
			h:      sseHandler([]string{evError}, nil),
			before: -1, kind: providers.KindServerError, message: "error event (api_error)",
		},
		{
			name:   "error event later",
			h:      sseHandler([]string{evStart, evBlock, evText, evError}, nil),
			before: 2, kind: providers.KindServerError, message: "error event (api_error)",
		},
		{
			name:   "content before message_start",
			h:      sseHandler([]string{evBlock}, nil),
			before: -1, kind: providers.KindServerError, message: "before message_start",
		},
		{
			name:   "event that is not JSON",
			h:      sseHandler([]string{evStart, "event: content_block_delta\ndata: {nope\n\n"}, nil),
			before: 1, kind: providers.KindServerError, message: "not JSON",
		},
		{
			name: "connection dropped",
			h: sseHandler([]string{evStart}, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
				conn, _, err := http.NewResponseController(w).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}),
			before: 1, kind: providers.KindTransport, message: "connection",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newUpstream(t, tt.h)
			p := testProvider(u.URL)
			if tt.set != nil {
				tt.set(&p.Timeouts)
			}
			a := newAdapter(t, p)
			s, err := a.ChatStream(ctxWithTimeout(t), request(t, streamBody))
			if tt.before == -1 {
				if s != nil {
					t.Fatal("ChatStream returned a stream")
				}
			} else {
				if err != nil {
					t.Fatalf("ChatStream: %v", err)
				}
				var got []string
				got, err = collect(t, s)
				if tt.before >= 0 && len(got) != tt.before {
					t.Errorf("read %d chunks before the failure, want %d", len(got), tt.before)
				}
				if _, again := s.Next(); !errors.Is(again, err) {
					t.Errorf("error is not sticky: %v then %v", err, again)
				}
				_ = s.Close()
			}
			pe := wantKind(t, err, tt.kind)
			if !strings.Contains(pe.Message, tt.message) {
				t.Errorf("message = %q, want it to mention %q", pe.Message, tt.message)
			}
		})
	}
}

// TestStreamPingsKeepAlive checks that, once the stream has started,
// pings re-arm the idle timeout: Anthropic sends them while the model
// works without output, for example while it thinks.
func TestStreamPingsKeepAlive(t *testing.T) {
	const idle = 100 * time.Millisecond
	u := newUpstream(t, sseHandler([]string{evStart, evBlock}, every(evPing, 4*idle, []string{evText, evDelta, evStop})))
	p := testProvider(u.URL)
	p.Timeouts.Idle = config.Duration(idle)
	a := newAdapter(t, p)
	s, err := a.ChatStream(ctxWithTimeout(t), request(t, streamBody))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer func() { _ = s.Close() }()
	got, err := collect(t, s)
	if !errors.Is(err, io.EOF) || len(got) != 3 {
		t.Fatalf("stream: %d chunks, then %v; want 3 chunks, then io.EOF", len(got), err)
	}
}

func TestStreamCloseCancelsUpstream(t *testing.T) {
	u := newUpstream(t, sseHandler([]string{evStart}, func(u *upstream, _ http.ResponseWriter, r *http.Request) {
		u.hang(r)
	}))
	a := newAdapter(t, testProvider(u.URL))
	s, err := a.ChatStream(ctxWithTimeout(t), request(t, streamBody))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if _, err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	_ = s.Close()
	waitCanceled(t, u)
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestStreamReusesConnection checks that a stream read to its message_stop
// gives its connection back to the pool, including when the upstream ends
// the response a moment later, and that an upstream that keeps the body
// open only delays Close by the drain bound.
func TestStreamReusesConnection(t *testing.T) {
	tests := []struct {
		name      string
		after     time.Duration // pause between message_stop and the end of the body
		streams   int
		wantConns int64 // at most
	}{
		{name: "body ends with message_stop", streams: 20, wantConns: 1},
		{name: "body ends 2ms after message_stop", after: 2 * time.Millisecond, streams: 20, wantConns: 1},
		{name: "body stays open after message_stop", after: 5 * drainTimeout, streams: 2, wantConns: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var conns atomic.Int64
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				for _, ev := range []string{evStart, evBlock, evText, evDelta, evStop} {
					_, _ = io.WriteString(w, ev)
					w.(http.Flusher).Flush()
				}
				if tt.after > 0 {
					select {
					case <-time.After(tt.after):
					case <-r.Context().Done():
					}
				}
			}))
			srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
				if s == http.StateNew {
					conns.Add(1)
				}
			}
			srv.Start()
			defer srv.Close()
			a := newAdapter(t, testProvider(srv.URL))

			for i := range tt.streams {
				s, err := a.ChatStream(ctxWithTimeout(t), request(t, streamBody))
				if err != nil {
					t.Fatalf("stream %d: %v", i, err)
				}
				if _, err := collect(t, s); !errors.Is(err, io.EOF) {
					t.Fatalf("stream %d ended with %v, want io.EOF", i, err)
				}
				start := time.Now()
				_ = s.Close()
				if d := time.Since(start); d > drainTimeout+time.Second {
					t.Errorf("stream %d: Close took %s, want at most about %s", i, d, drainTimeout)
				}
			}
			if n := conns.Load(); n > tt.wantConns {
				t.Errorf("%d streams used %d upstream connections, want at most %d", tt.streams, n, tt.wantConns)
			}
		})
	}
}

// TestStreamFromFake runs the fake's canned streams end to end: text with
// a stop sequence, parallel and single tool use, and structured output,
// each checked through the SDK accumulator, with the usage chunk last.
func TestStreamFromFake(t *testing.T) {
	tools := `"tools":[{"type":"function","function":{"name":"get_weather"}},{"type":"function","function":{"name":"get_time"}}]`
	tests := []struct {
		name      string
		body      string
		caps      []schema.Capability
		content   string
		toolCalls []string
		finish    string
	}{
		{name: "text", body: `{"model":"m","messages":[{"role":"user","content":"hi"}]`, content: "The quick brown fox jumps over the lazy dog.", finish: "stop"},
		{name: "stop sequence", body: `{"model":"m","stop":"lazy","messages":[{"role":"user","content":"hi"}]`, content: "The quick brown fox jumps over the ", finish: "stop"},
		{
			name: "parallel tool use", body: `{"model":"m","messages":[{"role":"user","content":"hi"}],` + tools,
			toolCalls: []string{
				`call_fake_0_0 get_weather {"location":"Paris, France","unit":"celsius"}`,
				`call_fake_0_1 get_time {"location":"Tokyo, Japan","unit":"celsius"}`,
			},
			finish: "tool_calls",
		},
		{
			name: "single tool use", body: `{"model":"m","parallel_tool_calls":false,"messages":[{"role":"user","content":"hi"}],` + tools,
			toolCalls: []string{`call_fake_0_0 get_weather {"location":"Paris, France","unit":"celsius"}`},
			finish:    "tool_calls",
		},
		{
			name: "json_schema native", caps: []schema.Capability{schema.CapJSONSchema},
			body:    `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"a","schema":{"type":"object","additionalProperties":false}}}`,
			content: `{"answer":"Paris","confidence":0.98}`, finish: "stop",
		},
		{
			// The fake rejects the schema as the API does unless the
			// adapter reduces it to the supported subset.
			name: "json_schema native with unsupported keywords", caps: []schema.Capability{schema.CapJSONSchema},
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"a","strict":true,
				"schema":{"type":"object","properties":{"n":{"type":"integer","minimum":0,"maximum":9}},"required":["n"]}}}`,
			content: `{"answer":"Paris","confidence":0.98}`, finish: "stop",
		},
		{
			name: "json_object", caps: []schema.Capability{schema.CapJSONMode},
			body:    `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}`,
			content: `{"answer":"Paris","confidence":0.98}`, finish: "stop",
		},
	}
	f := newFake(t)
	a := newAdapter(t, testProvider(f.BaseURL()))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.Reset()
			s, err := a.ChatStream(ctxWithTimeout(t), request(t, tt.body+`,"stream":true,"stream_options":{"include_usage":true}}`, tt.caps...))
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			defer func() { _ = s.Close() }()
			got, err := collect(t, s)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("stream ended with %v", err)
			}
			checkAccumulated(t, got, tt.content, tt.toolCalls, tt.finish)
			last, err := schema.ParseChatChunk([]byte(got[len(got)-1]))
			if err != nil || !last.IsUsageOnly() || last.Usage.PromptTokens <= 0 || last.Usage.CompletionTokens <= 0 {
				t.Errorf("last chunk %s is not a usage chunk with positive counts", got[len(got)-1])
			}
			ids := map[string]bool{}
			for _, raw := range got {
				var c struct {
					ID      string `json:"id"`
					Created int64  `json:"created"`
				}
				_ = json.Unmarshal([]byte(raw), &c)
				ids[fmt.Sprintf("%s/%d", c.ID, c.Created)] = true
			}
			if len(ids) != 1 {
				t.Errorf("chunks have %d distinct id/created pairs, want 1", len(ids))
			}
		})
	}
}

// TestStreamIsIncremental checks that chunks are returned as the upstream
// sends their events, not buffered, and that the stream reports the
// upstream request id and a time to first token that covers the fake's
// TTFT.
func TestStreamIsIncremental(t *testing.T) {
	const gap, ttft = 40 * time.Millisecond, 60 * time.Millisecond
	f := newFake(t)
	f.Script(fakeanthropic.Behavior{TTFT: ttft, ChunkDelay: gap})
	a := newAdapter(t, testProvider(f.BaseURL()))
	s, err := a.ChatStream(ctxWithTimeout(t), request(t, streamBody))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer func() { _ = s.Close() }()
	if !strings.HasPrefix(s.UpstreamRequestID(), "req_fake_") || s.TTFT() < ttft {
		t.Errorf("request id %q, TTFT %s (want at least %s)", s.UpstreamRequestID(), s.TTFT(), ttft)
	}
	var times []time.Time
	for {
		_, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		times = append(times, time.Now())
	}
	if len(times) < 6 {
		t.Fatalf("%d chunks, want at least 6", len(times))
	}
	// Buffering would deliver them all at once; allow half the spread for
	// scheduling noise.
	if spread, least := times[len(times)-1].Sub(times[0]), time.Duration(len(times)-1)*gap/2; spread < least {
		t.Errorf("%d chunks arrived within %s, want them spread over at least %s", len(times), spread, least)
	}
}

// sseEvents frames Messages API event payloads as SSE, each with its
// type as the event name.
func sseEvents(payloads ...string) []string {
	out := make([]string, len(payloads))
	for i, p := range payloads {
		var e struct{ Type string }
		_ = json.Unmarshal([]byte(p), &e)
		out[i] = "event: " + e.Type + "\ndata: " + p + "\n\n"
	}
	return out
}

// TestStreamEmptyToolInput checks that a tool call whose input is empty
// streams the arguments "{}", as the same message does non-streamed, and
// that a tool call with arguments gets nothing extra.
func TestStreamEmptyToolInput(t *testing.T) {
	const (
		start = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1}}}`
		end   = `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":3}}`
		stop  = `{"type":"message_stop"}`
	)
	toolStart := func(i int, id string) string {
		return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":"now","input":{}}}`, i, id)
	}
	blockStop := func(i int) string { return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i) }
	argsDelta := func(i int, s string) string {
		b, _ := json.Marshal(s)
		return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, i, b)
	}
	tests := []struct {
		name   string
		events []string
		calls  []string
	}{
		{
			name:   "an empty partial_json",
			events: sseEvents(start, toolStart(0, "toolu_1"), argsDelta(0, ""), blockStop(0), end, stop),
			calls:  []string{"toolu_1 now {}"},
		},
		{
			name:   "no delta at all",
			events: sseEvents(start, toolStart(0, "toolu_1"), blockStop(0), end, stop),
			calls:  []string{"toolu_1 now {}"},
		},
		{
			name: "an empty call beside one with arguments",
			events: sseEvents(start, toolStart(0, "toolu_1"), argsDelta(0, `{"tz":`), argsDelta(0, `"UTC"}`), blockStop(0),
				toolStart(1, "toolu_2"), blockStop(1), end, stop),
			calls: []string{`toolu_1 now {"tz":"UTC"}`, "toolu_2 now {}"},
		},
		{
			name:   "a block without content_block_stop before the next",
			events: sseEvents(start, toolStart(0, "toolu_1"), toolStart(1, "toolu_2"), blockStop(1), end, stop),
			calls:  []string{"toolu_1 now {}", "toolu_2 now {}"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newUpstream(t, sseHandler(tt.events, nil))
			a := newAdapter(t, testProvider(u.URL))
			s, err := a.ChatStream(ctxWithTimeout(t), request(t, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"now"}}]}`))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			got, err := collect(t, s)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("stream ended with %v", err)
			}
			checkAccumulated(t, got, "", tt.calls, "tool_calls")
		})
	}
	msg := `{"id":"msg_1","type":"message","model":"claude-test","content":[{"type":"tool_use","id":"toolu_1","name":"now","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
	out, _, err := translateMessage([]byte(msg), fixedNow)
	if err != nil || !strings.Contains(string(out), `"arguments":"{}"`) {
		t.Errorf("non-streamed empty tool input: %s, %v; want arguments {}", out, err)
	}
}

// TestStreamBlockState checks that the stream keeps only the current
// content block: memory stays flat however many blocks arrive, a delta for
// any other block is dropped, and more than maxBlocks blocks fail the
// stream.
func TestStreamBlockState(t *testing.T) {
	thinking := func(i int) string {
		return fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n"+
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i, i)
	}
	blocks := func(n int) func(*upstream, http.ResponseWriter, *http.Request) {
		return func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, evStart)
			for i := range n {
				_, _ = io.WriteString(w, thinking(i))
			}
			_, _ = io.WriteString(w, evDelta+evStop)
		}
	}

	t.Run("memory stays flat", func(t *testing.T) {
		u := newUpstream(t, blocks(maxBlocks))
		a := newAdapter(t, testProvider(u.URL))
		ps, err := a.ChatStream(ctxWithTimeout(t), request(t, streamBody))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ps.Close() }()
		s := ps.(*stream)
		heap := func() uint64 {
			var m runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&m)
			return m.HeapAlloc
		}
		// Step through the events: two per block.
		steps := func(n int) {
			for range n {
				if err := s.step(); err != nil {
					t.Fatalf("step: %v", err)
				}
			}
		}
		steps(2 * 1000)
		before := heap()
		steps(2 * (maxBlocks - 1000))
		after := heap()
		// A map entry per block costs tens of bytes, so 9,000 more
		// blocks would add hundreds of KiB.
		if after > before && after-before > 128<<10 {
			t.Errorf("live heap grew by %d bytes over %d blocks", after-before, maxBlocks-1000)
		}
		if s.blocks != maxBlocks || s.block != nil {
			t.Errorf("blocks = %d, current = %+v; want %d and none", s.blocks, s.block, maxBlocks)
		}
		got, err := collect(t, ps)
		if !errors.Is(err, io.EOF) || len(got) != 2 {
			t.Errorf("after the blocks: chunks %q, err %v; want the role and finish chunks and EOF", got, err)
		}
	})

	t.Run("more than maxBlocks fail the stream", func(t *testing.T) {
		u := newUpstream(t, blocks(maxBlocks+1))
		a := newAdapter(t, testProvider(u.URL))
		s, err := a.ChatStream(ctxWithTimeout(t), request(t, streamBody))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		_, err = collect(t, s)
		if pe := wantKind(t, err, providers.KindServerError); !strings.Contains(pe.Message, "content blocks") {
			t.Errorf("error = %v", pe)
		}
	})

	t.Run("deltas of other blocks are dropped", func(t *testing.T) {
		events := sseEvents(
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"late"}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":7,"delta":{"type":"text_delta","text":"stray"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"b"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		)
		u := newUpstream(t, sseHandler(events, nil))
		a := newAdapter(t, testProvider(u.URL))
		s, err := a.ChatStream(ctxWithTimeout(t), request(t, streamBody))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		got, err := collect(t, s)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream ended with %v", err)
		}
		checkAccumulated(t, got, "ab", nil, "stop")
	})
}
