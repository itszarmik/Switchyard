package fakeupstream_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"switchyard/test/fakeupstream"
)

// waitTimeout bounds every wait. It is generous so a slow, race-instrumented
// machine does not make the tests flaky.
const waitTimeout = 10 * time.Second

func newServer(t *testing.T, opts fakeupstream.Options) *fakeupstream.Server {
	t.Helper()
	s := fakeupstream.New(opts)
	t.Cleanup(s.Close)
	return s
}

// response is a fully read HTTP response.
type response struct {
	status int
	header http.Header
	body   []byte
}

func do(ctx context.Context, t *testing.T, method, url, auth, body string) (response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: b}, err
}

func post(t *testing.T, s *fakeupstream.Server, body string) response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	r, err := do(ctx, t, http.MethodPost, s.BaseURL()+"/chat/completions", "", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return r
}

// completion is the part of a chat.completion the tests check.
type completion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string  `json:"role"`
			Content   *string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// answer is a normalized choice, from a completion or a stream.
type answer struct {
	Content   *string
	ToolCalls []toolCall
	Finish    string
}

type toolCall struct{ ID, Name, Args string }

func strptr(s string) *string { return &s }

func (c completion) answers() []answer {
	var out []answer
	for i, ch := range c.Choices {
		if ch.Index != i {
			return nil
		}
		a := answer{Content: ch.Message.Content, Finish: ch.FinishReason}
		for _, tc := range ch.Message.ToolCalls {
			a.ToolCalls = append(a.ToolCalls, toolCall{tc.ID, tc.Function.Name, tc.Function.Arguments})
		}
		out = append(out, a)
	}
	return out
}

const weatherTools = `"tools":[
	{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}},
	{"type":"function","function":{"name":"get_time","parameters":{"type":"object"}}}]`

// answerCases are request variants and the answer the fake must give, both
// streaming and not.
var answerCases = []struct {
	name  string
	extra string // fields added to the request
	want  []answer
}{
	{
		name: "plain text",
		want: []answer{{Content: strptr(fakeupstream.Text), Finish: "stop"}},
	},
	{
		name:  "one tool",
		extra: `"tools":[{"type":"function","function":{"name":"get_weather"}}]`,
		want: []answer{{Finish: "tool_calls", ToolCalls: []toolCall{
			{fakeupstream.ToolCallID(0, 0), "get_weather", fakeupstream.ToolArguments(0)}}}},
	},
	{
		name:  "parallel tools",
		extra: weatherTools,
		want: []answer{{Finish: "tool_calls", ToolCalls: []toolCall{
			{fakeupstream.ToolCallID(0, 0), "get_weather", fakeupstream.ToolArguments(0)},
			{fakeupstream.ToolCallID(0, 1), "get_time", fakeupstream.ToolArguments(1)}}}},
	},
	{
		name:  "parallel tool calls disabled",
		extra: weatherTools + `,"parallel_tool_calls":false`,
		want: []answer{{Finish: "tool_calls", ToolCalls: []toolCall{
			{fakeupstream.ToolCallID(0, 0), "get_weather", fakeupstream.ToolArguments(0)}}}},
	},
	{
		name:  "tool_choice none",
		extra: weatherTools + `,"tool_choice":"none"`,
		want:  []answer{{Content: strptr(fakeupstream.Text), Finish: "stop"}},
	},
	{
		name:  "tool_choice names a function",
		extra: weatherTools + `,"tool_choice":{"type":"function","function":{"name":"get_time"}}`,
		want: []answer{{Finish: "stop", ToolCalls: []toolCall{
			{fakeupstream.ToolCallID(0, 0), "get_time", fakeupstream.ToolArguments(0)}}}},
	},
	{
		name:  "json mode",
		extra: `"response_format":{"type":"json_object"}`,
		want:  []answer{{Content: strptr(fakeupstream.JSONText), Finish: "stop"}},
	},
	{
		name:  "json schema",
		extra: `"response_format":{"type":"json_schema","json_schema":{"name":"a","schema":{"type":"object"}}}`,
		want:  []answer{{Content: strptr(fakeupstream.JSONText), Finish: "stop"}},
	},
	{
		name:  "n=3",
		extra: `"n":3`,
		want: []answer{
			{Content: strptr(fakeupstream.ChoiceText(0)), Finish: "stop"},
			{Content: strptr(fakeupstream.ChoiceText(1)), Finish: "stop"},
			{Content: strptr(fakeupstream.ChoiceText(2)), Finish: "stop"},
		},
	},
	{
		name:  "stop string",
		extra: `"stop":"lazy"`,
		want:  []answer{{Content: strptr("The quick brown fox jumps over the "), Finish: "stop"}},
	},
	{
		name:  "stop array, earliest wins",
		extra: `"stop":["dog","fox"]`,
		want:  []answer{{Content: strptr("The quick brown "), Finish: "stop"}},
	},
}

func requestBody(model string, stream bool, extra string) string {
	b := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]`
	if stream {
		b += `,"stream":true`
	}
	if extra != "" {
		b += "," + extra
	}
	return b + "}"
}

func TestNonStreamingAnswers(t *testing.T) {
	s := newServer(t, fakeupstream.Options{})
	for _, tc := range answerCases {
		t.Run(tc.name, func(t *testing.T) {
			r := post(t, s, requestBody("my-model", false, tc.extra))
			if r.status != http.StatusOK || r.header.Get("Content-Type") != "application/json" {
				t.Fatalf("status %d, Content-Type %q, body %s", r.status, r.header.Get("Content-Type"), r.body)
			}
			if !strings.HasPrefix(r.header.Get("X-Request-Id"), "req_fake_") {
				t.Errorf("x-request-id = %q", r.header.Get("X-Request-Id"))
			}
			var c completion
			if err := json.Unmarshal(r.body, &c); err != nil {
				t.Fatalf("body %s: %v", r.body, err)
			}
			if c.Model != "my-model" || c.Object != "chat.completion" || !strings.HasPrefix(c.ID, "chatcmpl-") {
				t.Errorf("model %q, object %q, id %q", c.Model, c.Object, c.ID)
			}
			if diff := cmp.Diff(tc.want, c.answers()); diff != "" {
				t.Errorf("answers (-want +got):\n%s", diff)
			}
			for _, ch := range c.Choices {
				if ch.Message.Role != "assistant" {
					t.Errorf("role %q", ch.Message.Role)
				}
			}
			u := c.Usage
			if u.PromptTokens <= 0 || u.CompletionTokens <= 0 || u.TotalTokens != u.PromptTokens+u.CompletionTokens {
				t.Errorf("usage %+v", u)
			}
		})
	}
}

// sseEvents splits an SSE body into its data payloads.
func sseEvents(t *testing.T, body []byte) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			t.Fatalf("unexpected SSE line %q", line)
		}
		out = append(out, data)
	}
	if !bytes.HasSuffix(body, []byte("\n\n")) {
		t.Errorf("stream does not end with a blank line: %q", body)
	}
	return out
}

// chunk is the part of a chat.completion.chunk the tests check.
type chunk struct {
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string  `json:"role"`
			Content   *string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

// accumulate folds stream chunks into answers, as an SDK does.
func accumulate(t *testing.T, chunks []chunk) []answer {
	t.Helper()
	var out []answer
	for _, c := range chunks {
		for _, ch := range c.Choices {
			for len(out) <= ch.Index {
				out = append(out, answer{})
			}
			a := &out[ch.Index]
			if ch.Delta.Content != nil {
				if a.Content == nil {
					a.Content = strptr("")
				}
				*a.Content += *ch.Delta.Content
			}
			for _, tc := range ch.Delta.ToolCalls {
				for len(a.ToolCalls) <= tc.Index {
					a.ToolCalls = append(a.ToolCalls, toolCall{})
				}
				call := &a.ToolCalls[tc.Index]
				if tc.ID != "" {
					call.ID = tc.ID
				}
				call.Name += tc.Function.Name
				call.Args += tc.Function.Arguments
			}
			if ch.FinishReason != nil {
				a.Finish = *ch.FinishReason
			}
		}
	}
	return out
}

func TestStreamingAnswers(t *testing.T) {
	s := newServer(t, fakeupstream.Options{})
	for _, tc := range answerCases {
		for _, includeUsage := range []bool{false, true} {
			name := tc.name
			extra := tc.extra
			if includeUsage {
				name += ", include_usage"
				if extra != "" {
					extra += ","
				}
				extra += `"stream_options":{"include_usage":true}`
			}
			t.Run(name, func(t *testing.T) {
				r := post(t, s, requestBody("my-model", true, extra))
				if r.status != http.StatusOK || !strings.HasPrefix(r.header.Get("Content-Type"), "text/event-stream") {
					t.Fatalf("status %d, Content-Type %q, body %s", r.status, r.header.Get("Content-Type"), r.body)
				}
				events := sseEvents(t, r.body)
				if len(events) < 3 || events[len(events)-1] != "[DONE]" {
					t.Fatalf("events %q, want at least two chunks then [DONE]", events)
				}
				var chunks []chunk
				for i, ev := range events[:len(events)-1] {
					var raw map[string]json.RawMessage
					var c chunk
					if json.Unmarshal([]byte(ev), &raw) != nil || json.Unmarshal([]byte(ev), &c) != nil {
						t.Fatalf("event %d is not a JSON object: %s", i, ev)
					}
					if c.Object != "chat.completion.chunk" || c.Model != "my-model" {
						t.Errorf("event %d: object %q, model %q", i, c.Object, c.Model)
					}
					_, hasUsage := raw["usage"]
					last := i == len(events)-2
					switch {
					case !includeUsage && hasUsage:
						t.Errorf("event %d has usage without include_usage: %s", i, ev)
					case includeUsage && !last && string(c.Usage) != "null":
						t.Errorf("event %d: usage %s, want null on every chunk but the last", i, c.Usage)
					case includeUsage && last:
						var u struct {
							Prompt     int `json:"prompt_tokens"`
							Completion int `json:"completion_tokens"`
							Total      int `json:"total_tokens"`
						}
						if err := json.Unmarshal(c.Usage, &u); err != nil || len(c.Choices) != 0 ||
							u.Prompt <= 0 || u.Completion <= 0 || u.Total != u.Prompt+u.Completion {
							t.Errorf("last event %s, want a usage chunk with empty choices", ev)
						}
					}
					chunks = append(chunks, c)
				}
				if diff := cmp.Diff(tc.want, accumulate(t, chunks)); diff != "" {
					t.Errorf("accumulated answers (-want +got):\n%s", diff)
				}
			})
		}
	}
}

func TestParallelToolCallsStreamAsArgumentDeltas(t *testing.T) {
	s := newServer(t, fakeupstream.Options{})
	r := post(t, s, requestBody("m", true, weatherTools))
	events := sseEvents(t, r.body)
	var indexes []int
	for _, ev := range events[:len(events)-1] {
		var c chunk
		if err := json.Unmarshal([]byte(ev), &c); err != nil {
			t.Fatal(err)
		}
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				if tc.ID != "" && tc.Function.Arguments != "" {
					t.Errorf("tool call header carries arguments: %s", ev)
				}
				indexes = append(indexes, tc.Index)
			}
		}
	}
	// Each call is a header then several argument deltas, call 0 first.
	first1 := -1
	for i, idx := range indexes {
		if idx == 1 && first1 < 0 {
			first1 = i
		}
		if first1 >= 0 && idx != 1 {
			t.Fatalf("tool call indexes %v are not sequential", indexes)
		}
	}
	if first1 < 3 || len(indexes)-first1 < 3 {
		t.Errorf("tool call indexes %v, want each call split into a header and several deltas", indexes)
	}
}

func TestStreamInterleavesChoices(t *testing.T) {
	s := newServer(t, fakeupstream.Options{})
	r := post(t, s, requestBody("m", true, `"n":2`))
	events := sseEvents(t, r.body)
	var order []int
	for _, ev := range events[:len(events)-1] {
		var c chunk
		if err := json.Unmarshal([]byte(ev), &c); err != nil {
			t.Fatal(err)
		}
		if len(c.Choices) != 1 {
			t.Fatalf("chunk with %d choices: %s", len(c.Choices), ev)
		}
		order = append(order, c.Choices[0].Index)
	}
	if len(order) < 4 || order[0] != 0 || order[1] != 1 || order[2] != 0 {
		t.Errorf("choice order %v, want round-robin", order)
	}
}

// envelope is an OpenAI error envelope; nil pointers are JSON null.
type envelope struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    *string `json:"code"`
	} `json:"error"`
}

func decodeEnvelope(t *testing.T, r response) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(r.body, &e); err != nil || e.Error.Message == "" || e.Error.Type == "" {
		t.Fatalf("body %s is not an error envelope (%v)", r.body, err)
	}
	if r.header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type %q", r.header.Get("Content-Type"))
	}
	return e
}

func deref(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

func TestScriptedErrors(t *testing.T) {
	tests := []struct {
		name            string
		behavior        fakeupstream.Behavior
		wantStatus      int
		wantType        string
		wantCode        string
		wantParam       string
		wantRetryAfter  string
		wantMessagePart string
	}{
		{"400 default", fakeupstream.Behavior{Status: 400}, 400, "invalid_request_error", "<null>", "<null>", "", ""},
		{"400 custom", fakeupstream.APIError(400, "invalid_request_error", "invalid_value", "temperature", "Invalid 'temperature'."),
			400, "invalid_request_error", "invalid_value", "temperature", "", "Invalid 'temperature'."},
		{"401", fakeupstream.Behavior{Status: 401}, 401, "invalid_request_error", "invalid_api_key", "<null>", "", "Incorrect API key"},
		{"429", fakeupstream.RateLimited("7"), 429, "requests", "rate_limit_exceeded", "<null>", "7", "Rate limit"},
		{"500", fakeupstream.Behavior{Status: 500}, 500, "server_error", "<null>", "<null>", "", ""},
		{"503", fakeupstream.Behavior{Status: 503}, 503, "server_error", "<null>", "<null>", "", "overloaded"},
		{"raw body", fakeupstream.Behavior{Status: 502, Body: []byte(`{"error":{"message":"m","type":"t","param":null,"code":"c"}}`)},
			502, "t", "c", "<null>", "", "m"},
	}
	s := newServer(t, fakeupstream.Options{})
	for _, tc := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "", true: ", stream"}[stream], func(t *testing.T) {
				s.Script(tc.behavior)
				t.Cleanup(s.Reset)
				r := post(t, s, requestBody("m", stream, ""))
				if r.status != tc.wantStatus {
					t.Fatalf("status %d, want %d", r.status, tc.wantStatus)
				}
				e := decodeEnvelope(t, r)
				if e.Error.Type != tc.wantType || deref(e.Error.Code) != tc.wantCode || deref(e.Error.Param) != tc.wantParam {
					t.Errorf("error %+v, want type %s, code %s, param %s", e.Error, tc.wantType, tc.wantCode, tc.wantParam)
				}
				if !strings.Contains(e.Error.Message, tc.wantMessagePart) {
					t.Errorf("message %q, want it to contain %q", e.Error.Message, tc.wantMessagePart)
				}
				if got := r.header.Get("Retry-After"); got != tc.wantRetryAfter {
					t.Errorf("Retry-After %q, want %q", got, tc.wantRetryAfter)
				}
				if r.header.Get("X-Request-Id") == "" {
					t.Error("no x-request-id")
				}
			})
		}
	}
}

func TestValidationErrors(t *testing.T) {
	s := newServer(t, fakeupstream.Options{Models: []string{"known"}})
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
		wantParam  string
	}{
		{"invalid JSON", `{"model":`, 400, "<null>", "<null>"},
		{"not an object", `[1]`, 400, "<null>", "<null>"},
		{"missing model", `{"messages":[{"role":"user","content":"hi"}]}`, 400, "<null>", "<null>"},
		{"model not a string", `{"model":1,"messages":[{"role":"user","content":"hi"}]}`, 400, "invalid_type", "model"},
		{"missing messages", `{"model":"known"}`, 400, "missing_required_parameter", "messages"},
		{"empty messages", `{"model":"known","messages":[]}`, 400, "empty_array", "messages"},
		{"stream not a boolean", `{"model":"known","messages":[{}],"stream":"yes"}`, 400, "invalid_type", "stream"},
		{"n out of range", `{"model":"known","messages":[{}],"n":0}`, 400, "invalid_value", "n"},
		{"unknown model", `{"model":"other","messages":[{}]}`, 404, "model_not_found", "<null>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := post(t, s, tc.body)
			if r.status != tc.wantStatus {
				t.Fatalf("status %d, want %d: %s", r.status, tc.wantStatus, r.body)
			}
			e := decodeEnvelope(t, r)
			if e.Error.Type != "invalid_request_error" || deref(e.Error.Code) != tc.wantCode || deref(e.Error.Param) != tc.wantParam {
				t.Errorf("error %+v, want code %s, param %s", e.Error, tc.wantCode, tc.wantParam)
			}
		})
	}
	t.Run("unknown model message", func(t *testing.T) {
		e := decodeEnvelope(t, post(t, s, `{"model":"other","messages":[{}]}`))
		if want := "The model `other` does not exist or you do not have access to it."; e.Error.Message != want {
			t.Errorf("message %q, want %q", e.Error.Message, want)
		}
	})
}

func TestAPIKey(t *testing.T) {
	const key = "sk-fake-0123456789abcdef"
	s := newServer(t, fakeupstream.Options{APIKey: key})
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	tests := []struct {
		name       string
		auth       string
		wantStatus int
		wantCode   string
	}{
		{"right key", key, 200, ""},
		{"wrong key", "sk-wrong-0123456789abcdef", 401, "invalid_api_key"},
		{"no key", "", 401, "<null>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := do(ctx, t, http.MethodPost, s.BaseURL()+"/chat/completions", tc.auth, requestBody("m", false, ""))
			if err != nil {
				t.Fatal(err)
			}
			if r.status != tc.wantStatus {
				t.Fatalf("status %d, want %d", r.status, tc.wantStatus)
			}
			if tc.wantStatus == 200 {
				return
			}
			e := decodeEnvelope(t, r)
			if deref(e.Error.Code) != tc.wantCode {
				t.Errorf("code %s, want %s", deref(e.Error.Code), tc.wantCode)
			}
			if tc.auth != "" && strings.Contains(e.Error.Message, tc.auth) {
				t.Errorf("message %q echoes the whole key", e.Error.Message)
			}
		})
	}
}

func TestModelsAndUnknownPaths(t *testing.T) {
	s := newServer(t, fakeupstream.Options{Models: []string{"a", "b"}})
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	r, err := do(ctx, t, http.MethodGet, s.BaseURL()+"/models", "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"object":"list","data":[` +
		`{"id":"a","object":"model","created":1767225600,"owned_by":"fakeupstream"},` +
		`{"id":"b","object":"model","created":1767225600,"owned_by":"fakeupstream"}]}`
	if r.status != 200 || string(r.body) != want {
		t.Errorf("GET /v1/models = %d %s, want 200 %s", r.status, r.body, want)
	}
	r, err = do(ctx, t, http.MethodPost, s.BaseURL()+"/embeddings", "", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if r.status != http.StatusNotFound {
		t.Errorf("POST /v1/embeddings = %d, want 404", r.status)
	}
	decodeEnvelope(t, r)
}

func TestRecording(t *testing.T) {
	s := newServer(t, fakeupstream.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	body := requestBody("m", false, `"x_unknown":{"big":12345678901234567890}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL()+"/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Custom", "v")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	reqs, err := s.Await(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	got := reqs[0]
	if got.Method != http.MethodPost || got.Path != "/v1/chat/completions" || string(got.Body) != body ||
		got.Header.Get("X-Custom") != "v" || got.Status != 200 || !bytes.Equal(got.ResponseBody, respBody) ||
		got.RequestID != resp.Header.Get("X-Request-Id") || got.ReceivedAt.IsZero() || got.DoneAt.IsZero() ||
		!got.ClientGoneAt.IsZero() {
		t.Errorf("recorded %+v", got)
	}
	if n := len(s.Requests()); n != 1 {
		t.Errorf("%d requests recorded, want 1", n)
	}
	s.Reset()
	if n := len(s.Requests()); n != 0 {
		t.Errorf("%d requests after Reset, want 0", n)
	}

	t.Run("disabled", func(t *testing.T) {
		s := newServer(t, fakeupstream.Options{DisableRecording: true})
		if r := post(t, s, requestBody("m", false, "")); r.status != 200 {
			t.Fatalf("status %d", r.status)
		}
		if n := len(s.Requests()); n != 0 {
			t.Errorf("%d requests recorded, want 0", n)
		}
	})
}

func TestAwaitTimesOut(t *testing.T) {
	s := newServer(t, fakeupstream.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Await(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Await = %v, want a deadline error", err)
	}
}

func TestExtraFields(t *testing.T) {
	s := newServer(t, fakeupstream.Options{})
	s.Script(fakeupstream.Behavior{ExtraFields: map[string]json.RawMessage{
		"x_b": json.RawMessage(`{"n":1}`), "x_a": json.RawMessage(`"v"`)}})
	r := post(t, s, requestBody("m", false, ""))
	if !bytes.HasSuffix(r.body, []byte(`,"x_a":"v","x_b":{"n":1}}`)) {
		t.Errorf("completion %s, want the extra fields at the end in key order", r.body)
	}
	r = post(t, s, requestBody("m", true, ""))
	events := sseEvents(t, r.body)
	for _, ev := range events[:len(events)-1] {
		if !strings.HasSuffix(ev, `,"x_a":"v","x_b":{"n":1}}`) {
			t.Errorf("chunk %s has no extra fields", ev)
		}
	}
}

// openStream starts a streaming request and returns its response body,
// which is closed when the test ends.
func openStream(ctx context.Context, t *testing.T, s *fakeupstream.Server) io.Reader {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL()+"/chat/completions",
		strings.NewReader(requestBody("m", true, "")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp.Body
}

// readEvents reads n SSE events from r.
func readEvents(t *testing.T, r *bufio.Reader, n int) {
	t.Helper()
	for i := 0; i < n; {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading event %d: %v", i, err)
		}
		if strings.HasPrefix(line, "data: ") {
			i++
		}
	}
}

func TestFaults(t *testing.T) {
	t.Run("drop before a non-streaming response", func(t *testing.T) {
		s := newServer(t, fakeupstream.Options{})
		s.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultDrop})
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		if r, err := do(ctx, t, http.MethodPost, s.BaseURL()+"/chat/completions", "", requestBody("m", false, "")); err == nil {
			t.Errorf("got %d %s, want a transport error", r.status, r.body)
		}
	})
	t.Run("drop after two chunks", func(t *testing.T) {
		s := newServer(t, fakeupstream.Options{})
		s.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultDrop, FaultAfterChunks: 2})
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		got, err := io.ReadAll(openStream(ctx, t, s))
		if err == nil {
			t.Fatalf("read the whole stream %q, want an error", got)
		}
		if n := strings.Count(string(got), "data: "); n != 2 || strings.Contains(string(got), "[DONE]") {
			t.Errorf("received %q before the drop, want exactly 2 events", got)
		}
		reqs, err := s.Await(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if reqs[0].Chunks != 2 {
			t.Errorf("recorded %d chunks, want 2", reqs[0].Chunks)
		}
	})
	t.Run("hang until the client cancels", func(t *testing.T) {
		s := newServer(t, fakeupstream.Options{})
		s.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultHang, FaultAfterChunks: 2})
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		reqCtx, cancelReq := context.WithCancel(ctx)
		body := openStream(reqCtx, t, s)
		readEvents(t, bufio.NewReader(body), 2)
		canceledAt := time.Now()
		cancelReq()
		reqs, err := s.Await(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		gone := reqs[0].ClientGoneAt
		if gone.IsZero() || gone.Before(canceledAt) {
			t.Fatalf("ClientGoneAt %v, want a time after the cancel at %v", gone, canceledAt)
		}
		// Generous: the cancel reaches the fake over loopback.
		if d := gone.Sub(canceledAt); d > time.Second {
			t.Errorf("fake saw the cancel %s after it happened", d)
		}
	})
	t.Run("Close releases a hanging handler", func(t *testing.T) {
		s := fakeupstream.New(fakeupstream.Options{})
		s.Script(fakeupstream.Behavior{Fault: fakeupstream.FaultHang})
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		errc := make(chan error, 1)
		go func() {
			_, err := do(ctx, t, http.MethodPost, s.BaseURL()+"/chat/completions", "", requestBody("m", false, ""))
			errc <- err
		}()
		if err := s.WaitReceived(ctx, 1); err != nil {
			t.Fatal(err)
		}
		closed := make(chan struct{})
		go func() { s.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(waitTimeout):
			t.Fatal("Close blocked on a hanging handler")
		}
		<-errc
	})
}

func TestDelays(t *testing.T) {
	const ttft, gap = 150 * time.Millisecond, 30 * time.Millisecond
	s := newServer(t, fakeupstream.Options{})
	s.Script(fakeupstream.Behavior{TTFT: ttft, ChunkDelay: gap})
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()

	start := time.Now()
	body := openStream(ctx, t, s)
	if d := time.Since(start); d >= ttft {
		t.Errorf("stream headers took %s, want them before the %s TTFT", d, ttft)
	}
	r := bufio.NewReader(body)
	readEvents(t, r, 1)
	if d := time.Since(start); d < ttft {
		t.Errorf("first event after %s, want at least the %s TTFT", d, ttft)
	}
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	reqs, err := s.Await(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if min := ttft + time.Duration(reqs[0].Chunks-1)*gap; time.Since(start) < min {
		t.Errorf("stream of %d chunks took %s, want at least %s", reqs[0].Chunks, time.Since(start), min)
	}

	start = time.Now()
	if r := post(t, s, requestBody("m", false, "")); r.status != 200 {
		t.Fatalf("status %d", r.status)
	}
	if d := time.Since(start); d < ttft {
		t.Errorf("non-streaming response after %s, want at least the %s TTFT", d, ttft)
	}
}

func TestSequenceAndHold(t *testing.T) {
	s := newServer(t, fakeupstream.Options{})
	s.Script(fakeupstream.Behavior{Status: 500})
	s.Sequence(fakeupstream.Behavior{Status: 503}, fakeupstream.RateLimited("1"), fakeupstream.Behavior{})
	for i, want := range []int{503, 429, 200, 500, 500} {
		if r := post(t, s, requestBody("m", i%2 == 0, "")); r.status != want {
			t.Errorf("request %d: status %d, want %d", i, r.status, want)
		}
	}
	s.Reset()
	s.Sequence(fakeupstream.Behavior{Status: 503})
	s.Reset() // drops the sequence
	if r := post(t, s, requestBody("m", false, "")); r.status != 200 {
		t.Errorf("after Reset: status %d", r.status)
	}

	release := make(chan struct{})
	s.Script(fakeupstream.Behavior{Hold: release})
	done := make(chan int, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		r, err := do(ctx, t, http.MethodPost, s.BaseURL()+"/chat/completions", "", requestBody("m", false, ""))
		if err != nil {
			done <- 0
			return
		}
		done <- r.status
	}()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := s.WaitReceived(ctx, 2); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("a held request was answered")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if status := <-done; status != 200 {
		t.Errorf("released request: status %d", status)
	}
}

func TestFinishReason(t *testing.T) {
	s := newServer(t, fakeupstream.Options{})
	s.Script(fakeupstream.Behavior{FinishReason: "content_filter"})
	for _, stream := range []bool{false, true} {
		r := post(t, s, requestBody("m", stream, ""))
		if r.status != 200 || !strings.Contains(string(r.body), `"finish_reason":"content_filter"`) ||
			strings.Contains(string(r.body), `"finish_reason":"stop"`) {
			t.Errorf("stream=%v: %d %s", stream, r.status, r.body)
		}
	}
}
