package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/schema"
	"switchyard/test/fakeanthropic"
)

// plainBody is a request the fake accepts and answers with a replay.
const plainBody = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

// newFake starts a fake Messages API that requires the provider key.
func newFake(t *testing.T) *fakeanthropic.Server {
	t.Helper()
	f := fakeanthropic.New(fakeanthropic.Options{APIKey: providerKey})
	t.Cleanup(f.Close)
	return f
}

// TestChatResponseGolden is the golden test of the response direction:
// each handcrafted Message fixture, replayed by the fake, must become this
// chat.completion and this normalized usage.
func TestChatResponseGolden(t *testing.T) {
	tests := []struct {
		fixture string
		body    string
		caps    []schema.Capability
		want    string
	}{
		{
			fixture: "message_text.json",
			want: `{"id":"chatcmpl-msg_01XFDUDYJgAACzvnptvVoYEL","object":"chat.completion","created":1794398400,"model":"claude-opus-5-5",
				"choices":[{"index":0,"message":{"role":"assistant","content":"Hello! How can I help?","refusal":null},"logprobs":null,"finish_reason":"stop"}],
				"usage":{"prompt_tokens":12,"completion_tokens":9,"total_tokens":21,"prompt_tokens_details":{"cached_tokens":0}}}`,
		},
		{
			fixture: "message_tool_use.json",
			want: `{"id":"chatcmpl-msg_01Aq9w938a90dw8q","object":"chat.completion","created":1794398400,"model":"claude-opus-5-5",
				"choices":[{"index":0,"message":{"role":"assistant","content":"I'll check the weather.","refusal":null,
					"tool_calls":[{"id":"toolu_01A09q90qw90lq917835lq9","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"San Francisco, CA\",\"unit\":\"celsius\"}"}}]},
					"logprobs":null,"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":472,"completion_tokens":89,"total_tokens":561,"prompt_tokens_details":{"cached_tokens":0}}}`,
		},
		{
			fixture: "message_parallel_tool_use.json",
			want: `{"id":"chatcmpl-msg_01ParallelToolUse","object":"chat.completion","created":1794398400,"model":"claude-opus-5-5",
				"choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":null,"tool_calls":[
					{"id":"toolu_01Weather","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Paris, France\"}"}},
					{"id":"toolu_02Time","type":"function","function":{"name":"get_time","arguments":"{\"timezone\":\"Europe/Paris\"}"}}]},
					"logprobs":null,"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":510,"completion_tokens":120,"total_tokens":630,"prompt_tokens_details":{"cached_tokens":0}}}`,
		},
		{
			fixture: "message_thinking.json",
			want: `{"id":"chatcmpl-msg_01Thinking","object":"chat.completion","created":1794398400,"model":"claude-opus-5-5",
				"choices":[{"index":0,"message":{"role":"assistant","content":"The GCD of 1071 and 462 is 21.","refusal":null},"logprobs":null,"finish_reason":"stop"}],
				"usage":{"prompt_tokens":40,"completion_tokens":310,"total_tokens":350,"prompt_tokens_details":{"cached_tokens":0},
					"completion_tokens_details":{"reasoning_tokens":280}}}`,
		},
		{
			// The full documented response: unknown fields (container,
			// citations, diagnostics) are ignored, and cache reads and
			// writes count as prompt tokens.
			fixture: "message_cache_usage.json",
			want: `{"id":"chatcmpl-msg_013Zva2CMHLNnXjNJJKqJ2EF","object":"chat.completion","created":1794398400,"model":"claude-opus-5-5",
				"choices":[{"index":0,"message":{"role":"assistant","content":"Hi! My name is Claude.","refusal":null},"logprobs":null,"finish_reason":"stop"}],
				"usage":{"prompt_tokens":6197,"completion_tokens":503,"total_tokens":6700,"prompt_tokens_details":{"cached_tokens":2051,"cache_write_tokens":2051},
					"completion_tokens_details":{"reasoning_tokens":0}}}`,
		},
		{
			fixture: "message_refusal.json",
			want: `{"id":"chatcmpl-msg_01Refusal","object":"chat.completion","created":1794398400,"model":"claude-opus-5-5",
				"choices":[{"index":0,"message":{"role":"assistant","content":"I can't help with that.","refusal":null},"logprobs":null,"finish_reason":"content_filter"}],
				"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28,"prompt_tokens_details":{"cached_tokens":0}}}`,
		},
	}
	f := newFake(t)
	a := newAdapter(t, testProvider(f.BaseURL()))
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			f.Reset()
			f.Script(fakeanthropic.Behavior{Replay: fixture(t, tt.fixture)})
			body := tt.body
			if body == "" {
				body = plainBody
			}
			resp, err := a.Chat(ctxWithTimeout(t), request(t, body, tt.caps...))
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if diff := jsonDiff(t, []byte(tt.want), resp.Body); diff != "" {
				t.Errorf("chat.completion (-want +got):\n%s\ngot %s", diff, resp.Body)
			}
			parsed, err := schema.ParseChatResponse(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(parsed.Usage, resp.Usage); diff != "" {
				t.Errorf("Response.Usage differs from the body's (-body +Usage):\n%s", diff)
			}
			if resp.Status != 200 || resp.UpstreamRequestID == "" || resp.TTFT <= 0 {
				t.Errorf("response = status %d, rid %q, ttft %s", resp.Status, resp.UpstreamRequestID, resp.TTFT)
			}
		})
	}
}

// TestFinishReason covers every stop_reason, through a whole message.
func TestFinishReason(t *testing.T) {
	tests := []struct {
		stopReason string
		want       string
	}{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"max_tokens", "length"},
		{"model_context_window_exceeded", "length"},
		{"tool_use", "tool_calls"},
		{"refusal", "content_filter"},
		{"pause_turn", "stop"},
		{"some_future_reason", "stop"},
		{"", "stop"},
	}
	for _, tt := range tests {
		t.Run(tt.stopReason, func(t *testing.T) {
			content := `[{"type":"text","text":"x"}]`
			msg := `{"id":"msg_1","type":"message","model":"claude-test","content":` + content +
				`,"stop_reason":"` + tt.stopReason + `","usage":{"input_tokens":1,"output_tokens":1}}`
			out, _, err := translateMessage([]byte(msg), fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			var c struct {
				Choices []struct {
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(out, &c); err != nil || len(c.Choices) != 1 {
				t.Fatalf("completion %s: %v", out, err)
			}
			if got := c.Choices[0].FinishReason; got != tt.want {
				t.Errorf("finish_reason = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestChatMalformedResponse checks that a 200 that is not a Message is a
// server error.
func TestChatMalformedResponse(t *testing.T) {
	for _, body := range []string{`not json`, `{"type":"error","error":{"type":"api_error"}}`, `[]`} {
		t.Run(body, func(t *testing.T) {
			f := newFake(t)
			f.Script(fakeanthropic.Behavior{Replay: []byte(body)})
			a := newAdapter(t, testProvider(f.BaseURL()))
			_, err := a.Chat(ctxWithTimeout(t), request(t, plainBody))
			pe := wantKind(t, err, "server_error")
			if strings.Contains(pe.Message, body) {
				t.Errorf("message quotes the body: %q", pe.Message)
			}
		})
	}
}
