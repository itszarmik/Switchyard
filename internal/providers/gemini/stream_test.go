package gemini

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"

	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

const streamBodyUsage = `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`

// chunks builds the expected chunks of one stream: each delta (a JSON
// object, or "finish:<reason>" for the finish chunk), then the usage chunk
// when includeUsage is set.
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

func text(s string) string { return `{"content":` + string(mustJSONString(s)) + `}` }

func mustJSONString(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

func toolDelta(index int, id, name, args string) string {
	return `{"tool_calls":[{"index":` + string(rune('0'+index)) + `,"id":"` + id + `","type":"function","function":{"name":"` + name +
		`","arguments":` + string(mustJSONString(args)) + `}}]}`
}

// usageJSON is a normalized usage object.
func usageJSON(prompt, completion, cached, reasoning int) string {
	b, _ := json.Marshal(map[string]any{
		"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion,
		"prompt_tokens_details":     map[string]int{"cached_tokens": cached},
		"completion_tokens_details": map[string]int{"reasoning_tokens": reasoning},
	})
	return string(b)
}

// TestStreamGolden is the golden test of the stream translation: each
// handcrafted event stream must become these chat.completion.chunk
// events, with and without the usage chunk, and the official SDK's
// accumulator must rebuild the answer from them.
func TestStreamGolden(t *testing.T) {
	tests := []struct {
		fixture, id, model, usage string
		deltas                    func(raw []byte) []string
		content                   string
		calls                     int
		finish                    string
	}{
		{
			// CRLF framing and no blank line after the last event; the
			// signature-only text part adds nothing.
			fixture: "stream_text.sse", id: "chatcmpl-gf9KaW7rT8-ihMIP9_aZ-Ag", model: "gemini-3.8-flash",
			usage: usageJSON(8, 34, 0, 25),
			deltas: func([]byte) []string {
				out := []string{roleDelta}
				for _, w := range []string{"The ", "quick ", "brown ", "fox ", "jumps ", "over ", "the ", "lazy ", "dog."} {
					out = append(out, text(w))
				}
				return append(out, "finish:stop")
			},
			content: "The quick brown fox jumps over the lazy dog.", finish: "stop",
		},
		{
			fixture: "stream_tool_call.sse", id: "chatcmpl-hf9KaX8sU8-ihMIPx_aZ-Ag", model: "gemini-3.8-flash",
			usage: usageJSON(61, 60, 0, 42),
			deltas: func(raw []byte) []string {
				return []string{roleDelta,
					toolDelta(0, withIDs(t, "{{id:fc_7p2k9x}}", raw), "get_weather", `{"location":"Paris","unit":"celsius"}`),
					"finish:tool_calls"}
			},
			calls: 1, finish: "tool_calls",
		},
		{
			fixture: "stream_parallel_tool_calls.sse", id: "chatcmpl-if9KaY9tV8-ihMIPz_aZ-Ag", model: "gemini-3.8-flash",
			usage: usageJSON(96, 93, 0, 57),
			deltas: func(raw []byte) []string {
				return []string{roleDelta, text("Checking both."),
					toolDelta(0, withIDs(t, "{{id:fc_a1}}", raw), "get_weather", `{"location":"Paris"}`),
					toolDelta(1, withIDs(t, "{{id:fc_a2}}", raw), "get_time", `{"location":"Paris"}`),
					"finish:tool_calls"}
			},
			content: "Checking both.", calls: 2, finish: "tool_calls",
		},
		{
			// usageMetadata is cumulative: the last event's counts.
			fixture: "stream_cache_thinking_usage.sse", id: "chatcmpl-jf9KaZ0uW8-ihMIPj_aZ-Ag", model: "gemini-4-carbon",
			usage: usageJSON(12100, 350, 8192, 300),
			deltas: func([]byte) []string {
				return []string{roleDelta, text("The summary "), text("is ready."), "finish:stop"}
			},
			content: "The summary is ready.", finish: "stop",
		},
		{
			fixture: "stream_blocked_prompt.sse", id: "chatcmpl-lf9KaR2wY8-ihMIPd_aZ-Ag", model: "gemini-3.8-flash",
			usage:  usageJSON(12, 0, 0, 0),
			deltas: func([]byte) []string { return []string{roleDelta, "finish:content_filter"} },
			finish: "content_filter",
		},
	}
	for _, tt := range tests {
		for _, includeUsage := range []bool{false, true} {
			name := tt.fixture
			if includeUsage {
				name += "/include_usage"
			}
			t.Run(name, func(t *testing.T) {
				raw := fixture(t, tt.fixture)
				u := newUpstream(t, sseHandler([]string{string(raw)}, nil))
				a := newAdapter(t, testProvider(u.URL))
				body := streamBody
				if includeUsage {
					body = streamBodyUsage
				}
				s, err := a.ChatStream(ctxWithTimeout(t), request(t, body))
				if err != nil {
					t.Fatalf("ChatStream: %v", err)
				}
				defer func() { _ = s.Close() }()
				got, err := collect(t, s)
				if !errors.Is(err, io.EOF) {
					t.Fatalf("stream ended with %v, want io.EOF", err)
				}
				want := chunks(tt.id, tt.model, includeUsage, tt.usage, tt.deltas(raw)...)
				if diff := jsonDiff(t, []byte(want), []byte("["+strings.Join(got, ",")+"]")); diff != "" {
					t.Errorf("chunks (-want +got):\n%s", diff)
				}
				if diff := jsonDiff(t, []byte(tt.usage), mustJSON(t, s.Usage())); diff != "" {
					t.Errorf("Usage() (-want +got):\n%s", diff)
				}
				if rid := strings.TrimPrefix(tt.id, completionIDPrefix); s.UpstreamRequestID() != rid {
					t.Errorf("UpstreamRequestID = %q, want %q", s.UpstreamRequestID(), rid)
				}

				var acc openai.ChatCompletionAccumulator
				for i, c := range got {
					var ch openai.ChatCompletionChunk
					if err := json.Unmarshal([]byte(c), &ch); err != nil {
						t.Fatal(err)
					}
					if !acc.AddChunk(ch) {
						t.Fatalf("the SDK accumulator rejected chunk %d: %s", i, c)
					}
				}
				msg := acc.Choices[0].Message
				if msg.Content != tt.content || len(msg.ToolCalls) != tt.calls || acc.Choices[0].FinishReason != tt.finish {
					t.Errorf("accumulated content %q, %d tool calls, finish %q", msg.Content, len(msg.ToolCalls), acc.Choices[0].FinishReason)
				}
				for _, tc := range msg.ToolCalls {
					if !idPattern.MatchString(tc.ID) || !json.Valid([]byte(tc.Function.Arguments)) {
						t.Errorf("tool call %+v", tc)
					}
				}
			})
		}
	}
}

// TestStreamErrorEvent checks an error event: after the first chunk it
// ends the stream with an error of the kind its code maps to; as the first
// event it is ChatStream's error, so it is retried or falls back like the
// same HTTP status.
func TestStreamErrorEvent(t *testing.T) {
	t.Run("mid-stream", func(t *testing.T) {
		u := newUpstream(t, sseHandler([]string{string(fixture(t, "stream_error.sse"))}, nil))
		a := newAdapter(t, testProvider(u.URL))
		s, err := a.ChatStream(ctxWithTimeout(t), request(t, streamBody))
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		defer func() { _ = s.Close() }()
		got, err := collect(t, s)
		if len(got) != 2 {
			t.Errorf("%d chunks before the error, want the role and one content chunk", len(got))
		}
		pe := wantKind(t, err, providers.KindServerError)
		if pe.Status != 503 || !strings.Contains(pe.Message, "UNAVAILABLE") || strings.Contains(pe.Message, "overloaded") {
			t.Errorf("error %+v: want status 503 and the status name, not the message", pe)
		}
	})
	tests := []struct {
		name       string
		event      string
		kind       providers.Kind
		retryAfter time.Duration
	}{
		{"429 with RetryInfo", string(fixture(t, "error_429_resource_exhausted.json")), providers.KindRateLimited, 7 * time.Second},
		{"400", string(fixture(t, "error_400_invalid_argument.json")), providers.KindClientError, 0},
		{"500", string(fixture(t, "error_500_internal.json")), providers.KindServerError, 0},
		{"504", string(fixture(t, "error_504_deadline_exceeded.json")), providers.KindTimeout, 0},
		{"no code", `{"error":{"message":"x"}}`, providers.KindServerError, 0},
	}
	for _, tt := range tests {
		t.Run("first event: "+tt.name, func(t *testing.T) {
			u := newUpstream(t, sseHandler([]string{data(strings.TrimSpace(tt.event))}, nil))
			_, err := newAdapter(t, testProvider(u.URL)).ChatStream(ctxWithTimeout(t), request(t, streamBody))
			pe := wantKind(t, err, tt.kind)
			if pe.RetryAfter != tt.retryAfter {
				t.Errorf("RetryAfter = %s, want %s", pe.RetryAfter, tt.retryAfter)
			}
		})
	}
}

// TestStreamFailures checks the malformed streams and the finish reasons
// that are not an answer.
func TestStreamFailures(t *testing.T) {
	const ev1 = `{"candidates":[{"content":{"parts":[{"text":"Hi"}]}}],"responseId":"r1"}`
	tests := []struct {
		name   string
		events []string
		open   bool // ChatStream succeeds and the error comes from Next
		kind   providers.Kind
	}{
		{"the body ends without a finish reason", []string{data(ev1)}, true, providers.KindServerError},
		{"an empty body", nil, false, providers.KindServerError},
		{"an event that is not JSON", []string{data(ev1), data("nope")}, true, providers.KindServerError},
		{"MALFORMED_FUNCTION_CALL after content", []string{data(ev1),
			data(`{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}`)}, true, providers.KindUnsupported},
		{"MISSING_THOUGHT_SIGNATURE first", []string{data(`{"candidates":[{"finishReason":"MISSING_THOUGHT_SIGNATURE"}]}`)}, false, providers.KindClientError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newUpstream(t, sseHandler(tt.events, nil))
			s, err := newAdapter(t, testProvider(u.URL)).ChatStream(ctxWithTimeout(t), request(t, streamBody))
			if tt.open {
				if err != nil {
					t.Fatalf("ChatStream: %v", err)
				}
				defer func() { _ = s.Close() }()
				_, err = collect(t, s)
			}
			pe := wantKind(t, err, tt.kind)
			// Only a finish reason that is not an answer was billed.
			if billed := strings.Contains(tt.name, "MALFORMED") || strings.Contains(tt.name, "MISSING"); pe.Billed != billed {
				t.Errorf("Billed = %v, want %v", pe.Billed, billed)
			}
		})
	}
}

// TestStreamCallShapes checks function calls that arrive in odd shapes: a
// call split over two events with the same id is one call with the merged
// arguments; calls without ids are distinct calls; text after a call in a
// later event is still content; and an event after the finish only
// updates the usage.
func TestStreamCallShapes(t *testing.T) {
	events := []string{
		data(`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"fc_1","name":"get_weather","args":{"location":"Paris"}},"thoughtSignature":"c2lnMQ=="}]}}],"responseId":"r"}`),
		data(`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"fc_1","name":"get_weather","args":{"unit":"celsius"}}},{"text":"Also: "}]}}],"responseId":"r"}`),
		data(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"now","args":{}}},{"functionCall":{"name":"now","args":{}}}]},"finishReason":"STOP"}],"responseId":"r"}`),
		data(`{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":7},"responseId":"r"}`),
	}
	u := newUpstream(t, sseHandler(events, nil))
	s, err := newAdapter(t, testProvider(u.URL)).ChatStream(ctxWithTimeout(t), request(t, streamBodyUsage))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	got, err := collect(t, s)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("stream: %v", err)
	}
	var acc openai.ChatCompletionAccumulator
	for _, c := range got {
		var ch openai.ChatCompletionChunk
		_ = json.Unmarshal([]byte(c), &ch)
		acc.AddChunk(ch)
	}
	calls := acc.Choices[0].Message.ToolCalls
	if len(calls) != 3 || calls[0].Function.Arguments != `{"location":"Paris","unit":"celsius"}` || calls[1].ID == calls[2].ID {
		t.Fatalf("calls %+v: want the split call merged, then two distinct calls", calls)
	}
	if d := decodeToolCallID(calls[0].ID, testModel); d.geminiID != "fc_1" || d.sig != "c2lnMQ==" {
		t.Errorf("first call decodes to %+v", d)
	}
	if acc.Choices[0].Message.Content != "Also: " || acc.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("content %q, finish %q", acc.Choices[0].Message.Content, acc.Choices[0].FinishReason)
	}
	if u := s.Usage(); u == nil || u.PromptTokens != 5 || u.CompletionTokens != 7 {
		t.Errorf("usage %+v, want the event after the finish's", u)
	}
}

// TestStreamTimeouts checks first_token, which bounds the wait for the
// first event, and idle, which bounds a gap between events.
func TestStreamTimeouts(t *testing.T) {
	const short = 50 * time.Millisecond
	t.Run("first_token", func(t *testing.T) {
		u := newUpstream(t, sseHandler(nil, func(u *upstream, _ http.ResponseWriter, r *http.Request) { u.hang(r) }))
		p := testProvider(u.URL)
		p.Timeouts.FirstToken = config.Duration(short)
		_, err := newAdapter(t, p).ChatStream(ctxWithTimeout(t), request(t, streamBody))
		if pe := wantKind(t, err, providers.KindTimeout); pe.Timeout != providers.TimeoutFirstToken {
			t.Errorf("timeout %q, want first_token", pe.Timeout)
		}
		waitCanceled(t, u)
	})
	t.Run("idle", func(t *testing.T) {
		u := newUpstream(t, sseHandler([]string{data(`{"candidates":[{"content":{"parts":[{"text":"Hi"}]}}]}`)},
			func(u *upstream, _ http.ResponseWriter, r *http.Request) { u.hang(r) }))
		p := testProvider(u.URL)
		p.Timeouts.Idle = config.Duration(short)
		s, err := newAdapter(t, p).ChatStream(ctxWithTimeout(t), request(t, streamBody))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		_, err = collect(t, s)
		if pe := wantKind(t, err, providers.KindTimeout); pe.Timeout != providers.TimeoutIdle {
			t.Errorf("timeout %q, want idle", pe.Timeout)
		}
	})
}

func TestStreamCloseCancelsUpstream(t *testing.T) {
	u := newUpstream(t, sseHandler([]string{data(`{"candidates":[{"content":{"parts":[{"text":"Hi"}]}}]}`)},
		func(u *upstream, _ http.ResponseWriter, r *http.Request) { u.hang(r) }))
	s, err := newAdapter(t, testProvider(u.URL)).ChatStream(ctxWithTimeout(t), request(t, streamBody))
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

// TestStreamBilledFailureUsage checks that a stream ending in a finish
// reason that fails it reports the final event's usage, which Google
// billed, as its Usage and on the error, not the previous event's.
func TestStreamBilledFailureUsage(t *testing.T) {
	u := newUpstream(t, sseHandler([]string{
		data(`{"candidates":[{"content":{"parts":[{"text":"Let me check."}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":3,"thoughtsTokenCount":40},"responseId":"r"}`),
		data(`{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":50,"thoughtsTokenCount":600},"responseId":"r"}`),
	}, nil))
	s, err := newAdapter(t, testProvider(u.URL)).ChatStream(ctxWithTimeout(t), request(t, streamBody))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_, err = collect(t, s)
	pe := wantKind(t, err, providers.KindUnsupported)
	for name, got := range map[string]*schema.Usage{"Usage()": s.Usage(), "BilledUsage": pe.BilledUsage} {
		if got == nil || got.PromptTokens != 100 || got.CompletionTokens != 650 {
			t.Errorf("%s = %+v, want the final event's 100 prompt and 650 completion tokens", name, got)
		}
	}
}
