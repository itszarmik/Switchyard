package cache

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"switchyard/internal/schema"
)

// completions are chat.completion bodies covering the shapes Replay and
// the Aggregator must round-trip.
var completions = []struct {
	name string
	body string
}{
	{"plain content", `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"gpt-4o-2024-08-06",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"Paris is the capital of France.","refusal":null},"logprobs":null,"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`},
	{"fingerprint and service tier", `{"id":"c2","object":"chat.completion","created":1,"model":"m","service_tier":"default","system_fingerprint":"fp_1",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`},
	{"multiple choices", `{"id":"c3","object":"chat.completion","created":2,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"one"},"finish_reason":"stop"},` +
		`{"index":1,"message":{"role":"assistant","content":"two, longer"},"finish_reason":"length"},` +
		`{"index":2,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":9,"total_tokens":12}}`},
	{"parallel tool calls", `{"id":"c4","object":"chat.completion","created":3,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},` +
		`{"id":"call_b","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"CET\"}"}}]},"finish_reason":"tool_calls"}],` +
		`"usage":{"prompt_tokens":30,"completion_tokens":20,"total_tokens":50}}`},
	{"content and a tool call", `{"id":"c5","object":"chat.completion","created":4,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"Let me check.","tool_calls":[` +
		`{"id":"call_c","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`},
	{"multiple choices with tool calls", `{"id":"c6","object":"chat.completion","created":5,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"t0","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"},` +
		`{"index":1,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"t1","type":"function","function":{"name":"g","arguments":"{\"b\":2}"}},{"id":"t2","type":"function","function":{"name":"h","arguments":"{}"}}]},"finish_reason":"tool_calls"}],` +
		`"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`},
	{"refusal", `{"id":"c7","object":"chat.completion","created":6,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":"I can't help with that."},"finish_reason":"stop"}]}`},
	{"logprobs", `{"id":"c8","object":"chat.completion","created":7,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"logprobs":{"content":[{"token":"Hi","logprob":-0.1,"bytes":[72,105],"top_logprobs":[]}],"refusal":null},"finish_reason":"stop"}]}`},
	{"legacy function call", `{"id":"c9","object":"chat.completion","created":8,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{\"x\":1}"}},"finish_reason":"stop"}]}`},
	{"unicode and escapes", `{"id":"c10","object":"chat.completion","created":9,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"naïve \"quotes\" <tag> &   line\nnext"},"finish_reason":"stop"}]}`},
}

// normalized is what must survive a replay and re-aggregation.
type normalized struct {
	ID, Model         string
	Created           int64
	SystemFingerprint string
	Choices           []normChoice
	Usage             any
}

type normChoice struct {
	Index        int
	Role         string
	Content      *string
	Refusal      *string
	FunctionCall *functionBody
	ToolCalls    []toolCallBody
	Logprobs     any
	FinishReason string
}

// normalize decodes a chat.completion body into its comparable fields.
func normalize(t *testing.T, body []byte) normalized {
	t.Helper()
	var c struct {
		ID                string `json:"id"`
		Model             string `json:"model"`
		Created           int64  `json:"created"`
		SystemFingerprint string `json:"system_fingerprint"`
		Choices           []struct {
			Index   int `json:"index"`
			Message struct {
				Role         string         `json:"role"`
				Content      *string        `json:"content"`
				Refusal      *string        `json:"refusal"`
				FunctionCall *functionBody  `json:"function_call"`
				ToolCalls    []toolCallBody `json:"tool_calls"`
			} `json:"message"`
			Logprobs     any    `json:"logprobs"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage any `json:"usage"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	n := normalized{ID: c.ID, Model: c.Model, Created: c.Created, SystemFingerprint: c.SystemFingerprint, Usage: c.Usage}
	for _, ch := range c.Choices {
		n.Choices = append(n.Choices, normChoice{
			Index: ch.Index, Role: ch.Message.Role, Content: ch.Message.Content, Refusal: ch.Message.Refusal,
			FunctionCall: ch.Message.FunctionCall, ToolCalls: ch.Message.ToolCalls,
			Logprobs: ch.Logprobs, FinishReason: ch.FinishReason,
		})
	}
	return n
}

// aggregate feeds payloads to a new Aggregator and returns its body.
func aggregate(t *testing.T, payloads [][]byte) ([]byte, bool) {
	t.Helper()
	a := NewAggregator()
	for i, p := range payloads {
		c, err := schema.ParseChatChunk(p)
		if err != nil {
			t.Fatalf("chunk %d %s: %v", i, p, err)
		}
		if err := a.Add(c); err != nil {
			t.Fatalf("Add(chunk %d): %v", i, err)
		}
	}
	return a.Body()
}

func TestReplayAggregateRoundTrip(t *testing.T) {
	for _, tt := range completions {
		for _, includeUsage := range []bool{true, false} {
			name := tt.name
			if !includeUsage {
				name += " without usage"
			}
			t.Run(name, func(t *testing.T) {
				payloads, err := Replay([]byte(tt.body), includeUsage)
				if err != nil {
					t.Fatalf("Replay: %v", err)
				}
				for i, p := range payloads {
					var c struct {
						ID      string            `json:"id"`
						Object  string            `json:"object"`
						Choices []json.RawMessage `json:"choices"`
						Usage   json.RawMessage   `json:"usage"`
					}
					if err := json.Unmarshal(p, &c); err != nil || c.Object != "chat.completion.chunk" || c.ID == "" || c.Choices == nil {
						t.Errorf("payload %d is not a chunk: %s", i, p)
					}
					if len(c.Usage) > 0 && i != len(payloads)-1 {
						t.Errorf("payload %d carries usage before the end: %s", i, p)
					}
				}
				got, ok := aggregate(t, payloads)
				if !ok {
					t.Fatalf("Body reported an incomplete stream for:\n%s", payloads)
				}
				want := normalize(t, []byte(tt.body))
				if !includeUsage {
					want.Usage = nil
				}
				if diff := cmp.Diff(want, normalize(t, got)); diff != "" {
					t.Errorf("aggregated replay differs (-want +got):\n%s\naggregated: %s", diff, got)
				}
				if !Cacheable(got, 1<<20) {
					t.Errorf("the aggregated body is not cacheable: %s", got)
				}
			})
		}
	}
}

func TestReplayChunkOrder(t *testing.T) {
	body := completions[2].body // three choices
	payloads, err := Replay([]byte(body), true)
	if err != nil {
		t.Fatal(err)
	}
	type step struct {
		Index        int
		Role         string
		HasContent   bool
		FinishReason string
		Usage        bool
	}
	var got []step
	for _, p := range payloads {
		var c struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Role    string  `json:"role"`
					Content *string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(p, &c); err != nil {
			t.Fatal(err)
		}
		if len(c.Choices) == 0 {
			got = append(got, step{Index: -1, Usage: len(c.Usage) > 0})
			continue
		}
		ch := c.Choices[0]
		s := step{Index: ch.Index, Role: ch.Delta.Role, HasContent: ch.Delta.Content != nil}
		if ch.FinishReason != nil {
			s.FinishReason = *ch.FinishReason
		}
		got = append(got, s)
	}
	want := []step{
		{Index: 0, Role: "assistant"}, {Index: 0, HasContent: true}, {Index: 0, FinishReason: "stop"},
		{Index: 1, Role: "assistant"}, {Index: 1, HasContent: true}, {Index: 1, FinishReason: "length"},
		{Index: 2, Role: "assistant"}, {Index: 2, HasContent: true}, {Index: 2, FinishReason: "stop"},
		{Index: -1, Usage: true},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("chunk sequence (-want +got):\n%s", diff)
	}
}

func TestReplayErrors(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":   `{"id":`,
		"no choices": `{"id":"x","object":"chat.completion","choices":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Replay([]byte(body), true); err == nil {
				t.Error("Replay succeeded")
			}
		})
	}
}

func TestAggregatorUpstreamStream(t *testing.T) {
	// Fragments as an upstream sends them: interleaved choices, the tool
	// call's id and name only in its first fragment, arguments split, and a
	// usage chunk at the end with null usage before.
	chunks := []string{
		`{"id":"s1","object":"chat.completion.chunk","created":10,"model":"m","system_fingerprint":"fp","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null},{"index":1,"delta":{"role":"assistant","content":null},"finish_reason":null}],"usage":null}`,
		`{"id":"s1","object":"chat.completion.chunk","created":10,"model":"m","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}],"usage":null}`,
		`{"id":"s1","object":"chat.completion.chunk","created":10,"model":"m","choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}],"usage":null}`,
		`{"id":"s1","object":"chat.completion.chunk","created":10,"model":"m","choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\""}},{"index":1,"id":"call_2","type":"function","function":{"name":"g","arguments":"{}"}}]},"finish_reason":null}],"usage":null}`,
		`{"id":"s1","object":"chat.completion.chunk","created":10,"model":"m","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}],"usage":null}`,
		`{"id":"s1","object":"chat.completion.chunk","created":10,"model":"m","choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]},"finish_reason":null}],"usage":null}`,
		`{"id":"s1","object":"chat.completion.chunk","created":10,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`,
		`{"id":"s1","object":"chat.completion.chunk","created":10,"model":"m","choices":[{"index":1,"delta":{},"finish_reason":"tool_calls"}],"usage":null}`,
		`{"id":"s1","object":"chat.completion.chunk","created":10,"model":"m","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`,
	}
	payloads := make([][]byte, len(chunks))
	for i, c := range chunks {
		payloads[i] = []byte(c)
	}
	got, ok := aggregate(t, payloads)
	if !ok {
		t.Fatal("Body reported an incomplete stream")
	}
	want := normalize(t, []byte(`{"id":"s1","object":"chat.completion","created":10,"model":"m","system_fingerprint":"fp","choices":[`+
		`{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"},`+
		`{"index":1,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}},{"id":"call_2","type":"function","function":{"name":"g","arguments":"{}"}}]},"finish_reason":"tool_calls"}],`+
		`"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`))
	if diff := cmp.Diff(want, normalize(t, got)); diff != "" {
		t.Errorf("aggregate (-want +got):\n%s", diff)
	}
}

// TestAggregatorToolCallsWithoutContent: a choice that only calls tools
// aggregates to content null, as its non-streaming form has, even when its
// stream sent an empty content delta (as the Anthropic adapter's does); an
// empty answer without tool calls keeps its empty content.
func TestAggregatorToolCallsWithoutContent(t *testing.T) {
	const call = `{"id":"s","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}`
	const empty = `{"id":"s","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`
	finish := func(reason string) string {
		return `{"id":"s","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}]}`
	}
	tests := []struct {
		name    string
		chunks  []string
		message string
	}{
		{"empty content delta, then tool calls", []string{empty, call, finish("tool_calls")},
			`{"role":"assistant","content":null,"refusal":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}`},
		{"tool calls, then an empty content delta", []string{call, empty, finish("tool_calls")},
			`{"role":"assistant","content":null,"refusal":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}`},
		{"empty answer without tool calls", []string{empty, finish("stop")},
			`{"role":"assistant","content":"","refusal":null}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payloads := make([][]byte, len(tt.chunks))
			for i, c := range tt.chunks {
				payloads[i] = []byte(c)
			}
			body, ok := aggregate(t, payloads)
			if !ok {
				t.Fatal("Body reported an incomplete stream")
			}
			var got struct {
				Choices []struct {
					Message json.RawMessage `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(body, &got); err != nil || len(got.Choices) != 1 {
				t.Fatalf("body %s: %v", body, err)
			}
			if string(got.Choices[0].Message) != tt.message {
				t.Errorf("message %s, want %s", got.Choices[0].Message, tt.message)
			}
		})
	}
}

func TestAggregatorIncomplete(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
	}{
		{"no chunks", nil},
		{"only usage", []string{`{"id":"x","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`}},
		{"no finish reason", []string{`{"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`}},
		{"one choice unfinished", []string{
			`{"id":"x","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":"stop"}]}`,
			`{"id":"x","choices":[{"index":1,"delta":{"content":"b"},"finish_reason":null}]}`,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payloads := make([][]byte, len(tt.chunks))
			for i, c := range tt.chunks {
				payloads[i] = []byte(c)
			}
			if body, ok := aggregate(t, payloads); ok {
				t.Errorf("Body = %s, want an incomplete stream", body)
			}
		})
	}
}

func TestAggregatorRejects(t *testing.T) {
	tests := []struct {
		name  string
		chunk string
	}{
		{"error event", `{"error":{"message":"overloaded","type":"server_error"}}`},
		{"negative choice index", `{"id":"x","choices":[{"index":-1,"delta":{}}]}`},
		{"negative tool index", `{"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":-2}]}}]}`},
		{"wrong types", `{"id":"x","choices":[{"index":0,"delta":{"content":5}}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := schema.ParseChatChunk([]byte(tt.chunk))
			if err != nil {
				t.Fatal(err)
			}
			if err := NewAggregator().Add(c); err == nil {
				t.Error("Add accepted the chunk")
			}
		})
	}
}

// TestReplayOpenAISDK serves Replay's output over SSE to the openai-go
// SDK's streaming client and checks that its accumulator rebuilds the
// original message (P6 acceptance criterion 3, at the unit level).
func TestReplayOpenAISDK(t *testing.T) {
	for _, tt := range completions {
		if strings.Contains(tt.body, "function_call") {
			continue // the SDK accumulator does not merge the deprecated function_call
		}
		t.Run(tt.name, func(t *testing.T) {
			payloads, err := Replay([]byte(tt.body), true)
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, p := range payloads {
					_, _ = w.Write([]byte("data: " + string(p) + "\n\n"))
				}
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			}))
			t.Cleanup(srv.Close)
			client := openai.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stream := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
				Model:    "m",
				Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("q")},
			})
			var acc openai.ChatCompletionAccumulator
			for stream.Next() {
				if !acc.AddChunk(stream.Current()) {
					t.Fatalf("the SDK accumulator rejected a chunk: %s", stream.Current().RawJSON())
				}
			}
			if err := stream.Err(); err != nil {
				t.Fatalf("stream: %v", err)
			}
			_ = stream.Close()

			var want openai.ChatCompletion
			if err := json.Unmarshal([]byte(tt.body), &want); err != nil {
				t.Fatal(err)
			}
			if acc.ID != want.ID || acc.Model != want.Model || acc.Created != want.Created {
				t.Errorf("accumulated id %q model %q created %d, want %q %q %d",
					acc.ID, acc.Model, acc.Created, want.ID, want.Model, want.Created)
			}
			if len(acc.Choices) != len(want.Choices) {
				t.Fatalf("accumulated %d choices, want %d", len(acc.Choices), len(want.Choices))
			}
			for i, w := range want.Choices {
				g := acc.Choices[i]
				if g.Index != w.Index || g.FinishReason != w.FinishReason || g.Message.Content != w.Message.Content ||
					g.Message.Refusal != w.Message.Refusal || string(g.Message.Role) != string(w.Message.Role) {
					t.Errorf("choice %d: index %d finish %q role %q content %q refusal %q; want %d %q %q %q %q", i,
						g.Index, g.FinishReason, g.Message.Role, g.Message.Content, g.Message.Refusal,
						w.Index, w.FinishReason, w.Message.Role, w.Message.Content, w.Message.Refusal)
				}
				if len(g.Message.ToolCalls) != len(w.Message.ToolCalls) {
					t.Fatalf("choice %d: %d tool calls, want %d", i, len(g.Message.ToolCalls), len(w.Message.ToolCalls))
				}
				for j, wc := range w.Message.ToolCalls {
					gc := g.Message.ToolCalls[j]
					if gc.ID != wc.ID || gc.Function.Name != wc.Function.Name || gc.Function.Arguments != wc.Function.Arguments || gc.Type != wc.Type {
						t.Errorf("choice %d tool call %d = %s %s %s %s, want %s %s %s %s", i, j,
							gc.ID, gc.Type, gc.Function.Name, gc.Function.Arguments, wc.ID, wc.Type, wc.Function.Name, wc.Function.Arguments)
					}
				}
				if len(g.Logprobs.Content) != len(w.Logprobs.Content) {
					t.Errorf("choice %d: %d logprobs, want %d", i, len(g.Logprobs.Content), len(w.Logprobs.Content))
				}
			}
			if acc.Usage.PromptTokens != want.Usage.PromptTokens || acc.Usage.CompletionTokens != want.Usage.CompletionTokens ||
				acc.Usage.TotalTokens != want.Usage.TotalTokens {
				t.Errorf("accumulated usage %+v, want %+v", acc.Usage, want.Usage)
			}
		})
	}
}

func TestCacheable(t *testing.T) {
	ok := completions[0].body
	tests := []struct {
		name     string
		body     string
		maxBytes int
		want     bool
	}{
		{"stop", ok, 1 << 20, true},
		{"length", strings.Replace(ok, `"stop"`, `"length"`, 1), 1 << 20, true},
		{"tool_calls", completions[3].body, 1 << 20, true},
		{"every choice complete", completions[2].body, 1 << 20, true},
		{"exactly max bytes", ok, len(ok), true},
		{"over max bytes", ok, len(ok) - 1, false},
		{"content_filter", strings.Replace(ok, `"stop"`, `"content_filter"`, 1), 1 << 20, false},
		{"one choice filtered", strings.Replace(completions[2].body, `"length"`, `"content_filter"`, 1), 1 << 20, false},
		{"null finish reason", strings.Replace(ok, `"stop"`, `null`, 1), 1 << 20, false},
		{"no finish reason", `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"x"}}]}`, 1 << 20, false},
		{"no message", `{"object":"chat.completion","choices":[{"index":0,"finish_reason":"stop"}]}`, 1 << 20, false},
		{"no choices", `{"object":"chat.completion","choices":[]}`, 1 << 20, false},
		{"a chunk", strings.Replace(ok, `"chat.completion"`, `"chat.completion.chunk"`, 1), 1 << 20, false},
		{"an error body", `{"error":{"message":"bad","type":"invalid_request_error"}}`, 1 << 20, false},
		{"not JSON", `{"object":"chat.completion"`, 1 << 20, false},
		{"empty", ``, 1 << 20, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Cacheable([]byte(tt.body), tt.maxBytes); got != tt.want {
				t.Errorf("Cacheable = %v, want %v", got, tt.want)
			}
		})
	}
}
