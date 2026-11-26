package gemini

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// fixtureSignature returns the thoughtSignature of the functionCall part
// with Gemini id id in a fixture (a response, or SSE events).
func fixtureSignature(t *testing.T, raw []byte, id string) string {
	t.Helper()
	re := regexp.MustCompile(`"functionCall":\{"id":"` + regexp.QuoteMeta(id) + `"[^}]*\}\},"thoughtSignature":"([^"]+)"`)
	m := re.FindSubmatch(raw)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// idRef matches {{id:GEMINI_ID}} placeholders in golden bodies.
var idRef = regexp.MustCompile(`\{\{id:([^}]+)\}\}`)

// withIDs replaces each {{id:X}} in want with the tool-call id the adapter
// returns for Gemini id X of testModel, with the signature fixture raw
// gave that call.
func withIDs(t *testing.T, want string, raw []byte) string {
	t.Helper()
	return idRef.ReplaceAllStringFunc(want, func(ref string) string {
		id := idRef.FindStringSubmatch(ref)[1]
		return encodeToolCallID(testModel, id, fixtureSignature(t, raw, id))
	})
}

// TestChatResponseGolden is the golden test of the response direction of
// the DESIGN §6.2 Gemini mapping table: each handcrafted
// GenerateContentResponse and the chat.completion it must become, usage
// included.
func TestChatResponseGolden(t *testing.T) {
	const head = `{"object":"chat.completion","created":1794398400,`
	tests := []struct {
		fixture string
		want    string
	}{
		{"generate_text.json", head + `"id":"chatcmpl-Xf9KaeTgN4-ihMIP2YjD-Ag","model":"gemini-3.8-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"Hello! How can I help you today?","refusal":null},"logprobs":null,"finish_reason":"stop"}],
			"usage":{"prompt_tokens":8,"completion_tokens":40,"total_tokens":48,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":31}}}`},
		{"generate_tool_call.json", head + `"id":"chatcmpl-Yf9KaZ2kA_-ihMIP8a7Q-Ag","model":"gemini-3.8-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":null,"tool_calls":[
				{"id":"{{id:fc_7p2k9x}}","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Paris\",\"unit\":\"celsius\"}"}}]},
				"logprobs":null,"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":61,"completion_tokens":60,"total_tokens":121,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":42}}}`},
		{"generate_parallel_tool_calls.json", head + `"id":"chatcmpl-Zf9KaY3hF8-ihMIPv5u4-Ag","model":"gemini-3.8-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"Checking both.","refusal":null,"tool_calls":[
				{"id":"{{id:fc_a1}}","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Paris\"}"}},
				{"id":"{{id:fc_a2}}","type":"function","function":{"name":"get_time","arguments":"{\"location\":\"Paris\"}"}}]},
				"logprobs":null,"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":96,"completion_tokens":93,"total_tokens":189,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":57}}}`},
		// prompt_tokens = 12,000 (cached included) + 100 tool-use prompt;
		// completion_tokens = 50 + 300 thinking.
		{"generate_cache_thinking_usage.json", head + `"id":"chatcmpl-af9KaQ7bM8-ihMIPgdKZ-Ag","model":"gemini-4-carbon",
			"choices":[{"index":0,"message":{"role":"assistant","content":"The summary is ready.","refusal":null},"logprobs":null,"finish_reason":"stop"}],
			"usage":{"prompt_tokens":12100,"completion_tokens":350,"total_tokens":12450,"prompt_tokens_details":{"cached_tokens":8192},"completion_tokens_details":{"reasoning_tokens":300}}}`},
		{"generate_blocked_prompt.json", head + `"id":"chatcmpl-bf9KaR4mL8-ihMIPm9qZ-Ag","model":"gemini-3.8-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"","refusal":null},"logprobs":null,"finish_reason":"content_filter"}],
			"usage":{"prompt_tokens":12,"completion_tokens":0,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`},
		{"generate_safety.json", head + `"id":"chatcmpl-cf9KaS5nP8-ihMIPw8aZ-Ag","model":"gemini-3.8-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"I can","refusal":null},"logprobs":null,"finish_reason":"content_filter"}],
			"usage":{"prompt_tokens":14,"completion_tokens":2,"total_tokens":16,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`},
		{"generate_max_tokens.json", head + `"id":"chatcmpl-df9KaT6oQ8-ihMIPs4mZ-Ag","model":"gemini-3.8-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"The quick brown","refusal":null},"logprobs":null,"finish_reason":"length"}],
			"usage":{"prompt_tokens":10,"completion_tokens":64,"total_tokens":74,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":61}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			raw := fixture(t, tt.fixture)
			u := newUpstream(t, jsonHandler(200, string(raw)))
			a := newAdapter(t, testProvider(u.URL))
			resp, err := a.Chat(ctxWithTimeout(t), request(t, plainBody))
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if diff := jsonDiff(t, []byte(withIDs(t, tt.want, raw)), resp.Body); diff != "" {
				t.Errorf("chat.completion (-want +got):\n%s\ngot %s", diff, resp.Body)
			}
			parsed, err := schema.ParseChatResponse(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if diff := jsonDiff(t, mustJSON(t, parsed.Usage), mustJSON(t, resp.Usage)); diff != "" {
				t.Errorf("Response.Usage differs from the body's (-body +Usage):\n%s", diff)
			}
			if want := strings.TrimPrefix(parsed.ID, completionIDPrefix); resp.UpstreamRequestID != want {
				t.Errorf("UpstreamRequestID = %q, want the responseId %q", resp.UpstreamRequestID, want)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestChatFinishFailures checks the finish reasons that are not an
// answer: a missing thought signature is a client error, as Google's 400
// for it; a malformed function call falls back to another deployment but
// is not retried and does not count toward the breaker.
func TestChatFinishFailures(t *testing.T) {
	t.Run("MISSING_THOUGHT_SIGNATURE", func(t *testing.T) {
		u := newUpstream(t, jsonHandler(200, string(fixture(t, "generate_missing_thought_signature.json"))))
		_, err := newAdapter(t, testProvider(u.URL)).Chat(ctxWithTimeout(t), request(t, plainBody))
		pe := wantKind(t, err, providers.KindClientError)
		checkEnvelope(t, pe, "messages", "missing_thought_signature")
		if !pe.Billed || pe.BilledUsage == nil || pe.BilledUsage.PromptTokens != 40 {
			t.Errorf("Billed %v, BilledUsage %+v: a 200 Google billed", pe.Billed, pe.BilledUsage)
		}
	})
	t.Run("MALFORMED_FUNCTION_CALL", func(t *testing.T) {
		u := newUpstream(t, jsonHandler(200, string(fixture(t, "generate_malformed_function_call.json"))))
		_, err := newAdapter(t, testProvider(u.URL)).Chat(ctxWithTimeout(t), request(t, plainBody))
		pe := wantKind(t, err, providers.KindUnsupported)
		if !strings.Contains(pe.Message, "MALFORMED_FUNCTION_CALL") || strings.Contains(pe.Message, "get_weather") {
			t.Errorf("message = %q: want the reason, never the model's output", pe.Message)
		}
		if providers.Retryable(pe, false) || pe.Kind.CountsTowardBreaker() || !pe.Kind.FallsBack() || pe.Status != 502 {
			t.Errorf("error %+v: want a 502 that falls back, without a retry or a breaker count", pe)
		}
		if strings.Contains(string(pe.Body), "get_weather") || !strings.Contains(string(pe.Body), `"code":"malformed_function_call"`) {
			t.Errorf("body %s", pe.Body)
		}
		// Google billed the answer: the error says so, with its usage.
		if u := pe.BilledUsage; !pe.Billed || u == nil || u.PromptTokens != 40 || u.CompletionTokens != 12 {
			t.Errorf("Billed %v, BilledUsage %+v: want the answer's usage (40 prompt, 12 completion)", pe.Billed, u)
		}
	})
}

// TestFinishReason covers every finishReason Google documents.
func TestFinishReason(t *testing.T) {
	tests := []struct {
		reason   string
		calls    bool
		want     string
		failKind providers.Kind
	}{
		{"STOP", false, "stop", ""},
		{"STOP", true, "tool_calls", ""},
		{"MAX_TOKENS", false, "length", ""},
		{"MAX_TOKENS", true, "length", ""},
		{"CONTINUATION", false, "length", ""},
		{"SAFETY", false, "content_filter", ""},
		{"RECITATION", false, "content_filter", ""},
		{"BLOCKLIST", false, "content_filter", ""},
		{"PROHIBITED_CONTENT", false, "content_filter", ""},
		{"SPII", false, "content_filter", ""},
		{"ESCALATION", false, "content_filter", ""},
		{"IMAGE_SAFETY", false, "content_filter", ""},
		{"IMAGE_PROHIBITED_CONTENT", false, "content_filter", ""},
		{"IMAGE_OTHER", false, "content_filter", ""},
		{"IMAGE_RECITATION", false, "content_filter", ""},
		{"LANGUAGE", false, "stop", ""},
		{"OTHER", false, "stop", ""},
		{"NO_IMAGE", false, "stop", ""},
		{"UNEXPECTED_TOOL_CALL", false, "stop", ""},
		{"TOO_MANY_TOOL_CALLS", true, "tool_calls", ""},
		{"PUP_LIMITED_DISABLED", false, "stop", ""},
		{"FINISH_REASON_UNSPECIFIED", false, "stop", ""},
		{"SOMETHING_NEW", false, "stop", ""},
		{"MALFORMED_FUNCTION_CALL", false, "", providers.KindUnsupported},
		{"MALFORMED_RESPONSE", false, "", providers.KindUnsupported},
		{"MISSING_THOUGHT_SIGNATURE", false, "", providers.KindClientError},
	}
	for _, tt := range tests {
		got, fail := finishReason(tt.reason, tt.calls)
		var kind providers.Kind
		if fail != nil {
			kind = fail.kind
		}
		if got != tt.want || kind != tt.failKind {
			t.Errorf("finishReason(%s, calls %v) = %q, failure %q; want %q, %q", tt.reason, tt.calls, got, kind, tt.want, tt.failKind)
		}
	}
}

// TestChatResponseShapes covers the response details the fixtures do not:
// thought parts are skipped, a call without an id gets a unique one, a
// response without a finish reason ends as STOP, one without candidates or
// usage, and a body that is not JSON.
func TestChatResponseShapes(t *testing.T) {
	t.Run("thought parts are not content", func(t *testing.T) {
		u := newUpstream(t, jsonHandler(200, `{"candidates":[{"content":{"parts":[
			{"text":"Let me think.","thought":true},{"text":"Answer."}]},"finishReason":"STOP"}],"responseId":"r"}`))
		resp, err := newAdapter(t, testProvider(u.URL)).Chat(ctxWithTimeout(t), request(t, plainBody))
		if err != nil || !strings.Contains(string(resp.Body), `"content":"Answer."`) {
			t.Fatalf("Chat: %v, %s", err, resp.Body)
		}
		if resp.Usage != nil || !strings.Contains(string(resp.Body), `"usage":null`) || !strings.Contains(string(resp.Body), `"model":"`+testModel+`"`) {
			t.Errorf("without usageMetadata or modelVersion: %s", resp.Body)
		}
	})
	t.Run("calls without ids get unique ones", func(t *testing.T) {
		u := newUpstream(t, jsonHandler(200, `{"candidates":[{"content":{"parts":[
			{"functionCall":{"name":"f","args":{}}},{"functionCall":{"name":"f","args":{"a":1}}}]}}]}`))
		resp, err := newAdapter(t, testProvider(u.URL)).Chat(ctxWithTimeout(t), request(t, plainBody))
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			ID      string `json:"id"`
			Choices []struct {
				Message struct {
					ToolCalls []struct{ ID string } `json:"tool_calls"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(resp.Body, &c); err != nil {
			t.Fatal(err)
		}
		calls := c.Choices[0].Message.ToolCalls
		if len(calls) != 2 || calls[0].ID == calls[1].ID || c.Choices[0].FinishReason != "tool_calls" {
			t.Fatalf("calls %+v, finish %q: want two distinct ids and tool_calls", calls, c.Choices[0].FinishReason)
		}
		for _, call := range calls {
			if d := decodeToolCallID(call.ID, testModel); !d.ours || !strings.HasPrefix(d.geminiID, "call_") {
				t.Errorf("id %q decodes to %+v", call.ID, d)
			}
		}
		if !strings.HasPrefix(c.ID, completionIDPrefix) || len(c.ID) <= len(completionIDPrefix) || resp.UpstreamRequestID != "" {
			t.Errorf("id %q, upstream request id %q: want a made-up completion id and no upstream id", c.ID, resp.UpstreamRequestID)
		}
	})
	for name, body := range map[string]string{
		"no candidates and no block reason": `{"usageMetadata":{"promptTokenCount":3}}`,
		"not JSON":                          `nope`,
	} {
		t.Run(name, func(t *testing.T) {
			u := newUpstream(t, jsonHandler(200, body))
			_, err := newAdapter(t, testProvider(u.URL)).Chat(ctxWithTimeout(t), request(t, plainBody))
			_ = wantKind(t, err, providers.KindServerError)
		})
	}
}

// TestMergeArgs covers the merge of a function call split over events.
func TestMergeArgs(t *testing.T) {
	tests := []struct{ prev, next, want string }{
		{"", `{"a":1}`, `{"a":1}`},
		{"", `null`, `{}`},
		{"", ``, `{}`},
		{`{"a":1}`, `{"b":2}`, `{"a":1,"b":2}`},
		{`{"a":1}`, `{"a":3}`, `{"a":3}`},
		{`{"a":1}`, `{}`, `{"a":1}`},
		{`{"a":1}`, `[1]`, `{"a":1}`},
		{"", `{ "a" : [1, 2] }`, `{"a":[1,2]}`},
	}
	for _, tt := range tests {
		var prev json.RawMessage
		if tt.prev != "" {
			prev = json.RawMessage(tt.prev)
		}
		if got := string(mergeArgs(prev, json.RawMessage(tt.next))); got != tt.want {
			t.Errorf("mergeArgs(%s, %s) = %s, want %s", tt.prev, tt.next, got, tt.want)
		}
	}
}
