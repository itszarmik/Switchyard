package fakeanthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"switchyard/test/fakeupstream"
)

// upstream is the method set the conformance suite needs from a fake.
// Both fakes must satisfy it.
type upstream interface {
	BaseURL() string
	Reset()
	Script(fakeupstream.Behavior)
	Requests() []fakeupstream.Request
	WaitReceived(ctx context.Context, n int) error
	Await(ctx context.Context, n int) ([]fakeupstream.Request, error)
}

var (
	_ upstream = (*fakeupstream.Server)(nil)
	_ upstream = (*OpenAIUpstream)(nil)
)

const (
	testKey     = "sk-ant-fake-key"
	testTimeout = 10 * time.Second
)

func newServer(t *testing.T) *Server {
	t.Helper()
	s := New(Options{APIKey: testKey, Models: []string{"claude-test"}})
	t.Cleanup(s.Close)
	return s
}

// post sends body to /v1/messages with the right headers, unless edit
// changes them, and returns the status and the response body.
func post(t *testing.T, s *Server, body string, edit func(http.Header)) (int, http.Header, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL()+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", testKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	if edit != nil {
		edit(req.Header)
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
	return resp.StatusCode, resp.Header, b
}

func body(extra string) string {
	return `{"model":"claude-test","max_tokens":100,"messages":[{"role":"user","content":"hi"}]` + extra + `}`
}

type message struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func decodeMessage(t *testing.T, b []byte) message {
	t.Helper()
	var m message
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("message %s: %v", b, err)
	}
	return m
}

func TestCannedMessages(t *testing.T) {
	tools := `,"tools":[{"name":"get_weather","input_schema":{"type":"object"}},{"name":"get_time","input_schema":{"type":"object"}}]`
	tests := []struct {
		name   string
		extra  string
		stop   string
		blocks []string // "text:<text>" or "tool:<id> <name> <input>"
	}{
		{name: "text", stop: "end_turn", blocks: []string{"text:" + fakeupstream.Text}},
		{name: "stop sequence", extra: `,"stop_sequences":["fox"]`, stop: "stop_sequence", blocks: []string{"text:The quick brown "}},
		{name: "parallel tool use", extra: tools, stop: "tool_use", blocks: []string{
			"tool:call_fake_0_0 get_weather " + fakeupstream.ToolArguments(0),
			"tool:call_fake_0_1 get_time " + fakeupstream.ToolArguments(1),
		}},
		{name: "parallel disabled", extra: tools + `,"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`, stop: "tool_use",
			blocks: []string{"tool:call_fake_0_0 get_weather " + fakeupstream.ToolArguments(0)}},
		{name: "tool_choice none", extra: tools + `,"tool_choice":{"type":"none"}`, stop: "end_turn", blocks: []string{"text:" + fakeupstream.Text}},
		{name: "forced tool", extra: tools + `,"tool_choice":{"type":"tool","name":"get_time"}`, stop: "tool_use",
			blocks: []string{"tool:call_fake_0_0 get_time " + fakeupstream.ToolArguments(0)}},
		{name: "native structured output", extra: `,"output_config":{"format":{"type":"json_schema","schema":{"type":"object","additionalProperties":false}}}`,
			stop: "end_turn", blocks: []string{"text:" + fakeupstream.JSONText}},
		{name: "system prompt asks for JSON", extra: `,"system":[{"type":"text","text":"Reply in JSON."}]`,
			stop: "end_turn", blocks: []string{"text:" + fakeupstream.JSONText}},
	}
	s := newServer(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, h, b := post(t, s, body(tt.extra), nil)
			if status != http.StatusOK || !strings.HasPrefix(h.Get("Request-Id"), "req_fake_") {
				t.Fatalf("status %d, request-id %q: %s", status, h.Get("Request-Id"), b)
			}
			m := decodeMessage(t, b)
			var got []string
			for _, c := range m.Content {
				if c.Type == "text" {
					got = append(got, "text:"+c.Text)
				} else {
					got = append(got, "tool:"+c.ID+" "+c.Name+" "+string(c.Input))
				}
			}
			if strings.Join(got, "\n") != strings.Join(tt.blocks, "\n") || m.StopReason != tt.stop {
				t.Errorf("content %q, stop %q; want %q, %q", got, m.StopReason, tt.blocks, tt.stop)
			}
			if m.Type != "message" || m.Model != "claude-test" || !strings.HasPrefix(m.ID, "msg_fake") ||
				m.Usage.InputTokens <= 0 || m.Usage.OutputTokens <= 0 {
				t.Errorf("message %s", b)
			}
		})
	}
}

// sseEvents parses an event stream into event names and data payloads.
func sseEvents(t *testing.T, b []byte) (names []string, data []string) {
	t.Helper()
	for _, frame := range strings.Split(strings.TrimSpace(string(b)), "\n\n") {
		lines := strings.Split(frame, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("malformed event %q", frame)
		}
		name, payload := strings.TrimPrefix(lines[0], "event: "), strings.TrimPrefix(lines[1], "data: ")
		var v struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(payload), &v); err != nil || v.Type != name {
			t.Fatalf("event %s has data %s", name, payload)
		}
		names, data = append(names, name), append(data, payload)
	}
	return names, data
}

func TestStreamEventSequence(t *testing.T) {
	s := newServer(t)
	status, h, b := post(t, s, body(`,"stream":true,"tools":[{"name":"f","input_schema":{"type":"object"}}]`), nil)
	if status != http.StatusOK || !strings.HasPrefix(h.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content type %q", status, h.Get("Content-Type"))
	}
	names, data := sseEvents(t, b)
	pieces := len(splitBytes(fakeupstream.ToolArguments(0), 6))
	want := []string{"message_start", "content_block_start", "ping"}
	for range pieces {
		want = append(want, "content_block_delta")
	}
	want = append(want, "content_block_stop", "message_delta", "message_stop")
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("events %v, want %v", names, want)
	}
	var args strings.Builder
	for _, d := range data {
		var ev struct {
			Delta struct {
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
		}
		_ = json.Unmarshal([]byte(d), &ev)
		args.WriteString(ev.Delta.PartialJSON)
	}
	if args.String() != fakeupstream.ToolArguments(0) {
		t.Errorf("partial_json joins to %q", args.String())
	}
	if !strings.Contains(data[len(data)-2], `"stop_reason":"tool_use"`) {
		t.Errorf("message_delta %s", data[len(data)-2])
	}
	if reqs := s.Requests(); len(reqs) != 1 || reqs[0].Chunks != len(want) {
		t.Errorf("recorded %+v", reqs)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		edit   func(http.Header)
		status int
		typ    string
	}{
		{"wrong key", body(""), func(h http.Header) { h.Set("X-Api-Key", "nope") }, 401, "authentication_error"},
		{"Authorization is not the key", body(""), func(h http.Header) { h.Del("X-Api-Key"); h.Set("Authorization", "Bearer "+testKey) }, 401, "authentication_error"},
		{"no anthropic-version", body(""), func(h http.Header) { h.Del("Anthropic-Version") }, 400, "invalid_request_error"},
		{"unknown field", body(`,"n":2`), nil, 400, "invalid_request_error"},
		{"temperature above 1", body(`,"temperature":1.5`), nil, 400, "invalid_request_error"},
		{"no max_tokens", `{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`, nil, 400, "invalid_request_error"},
		{"empty messages", `{"model":"claude-test","max_tokens":1,"messages":[]}`, nil, 400, "invalid_request_error"},
		{"system role", `{"model":"claude-test","max_tokens":1,"messages":[{"role":"system","content":"x"}]}`, nil, 400, "invalid_request_error"},
		{"empty text block", `{"model":"claude-test","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":""}]}]}`, nil, 400, "invalid_request_error"},
		{"tool_use input not an object", `{"model":"claude-test","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":"x"}]}]}`, nil, 400, "invalid_request_error"},
		{"forced tool not defined", body(`,"tool_choice":{"type":"tool","name":"f"}`), nil, 400, "invalid_request_error"},
		{"structured output with a numeric constraint", body(`,"output_config":{"format":{"type":"json_schema","schema":{"type":"object","additionalProperties":false,"properties":{"a":{"type":"integer","minimum":0}}}}}`), nil, 400, "invalid_request_error"},
		{"structured output with minItems 2", body(`,"output_config":{"format":{"type":"json_schema","schema":{"type":"array","minItems":2,"items":{"type":"string"}}}}`), nil, 400, "invalid_request_error"},
		{"structured output object without additionalProperties false", body(`,"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"object","additionalProperties":true}},"additionalProperties":false}}}`), nil, 400, "invalid_request_error"},
		{"unknown model", `{"model":"claude-x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, nil, 404, "not_found_error"},
	}
	s := newServer(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, _, b := post(t, s, tt.body, tt.edit)
			var env struct {
				Type  string                         `json:"type"`
				Error struct{ Type, Message string } `json:"error"`
			}
			if err := json.Unmarshal(b, &env); err != nil {
				t.Fatal(err)
			}
			if status != tt.status || env.Type != "error" || env.Error.Type != tt.typ || env.Error.Message == "" {
				t.Errorf("status %d, body %s; want %d %s", status, b, tt.status, tt.typ)
			}
		})
	}
}

func TestScriptedErrors(t *testing.T) {
	s := newServer(t)
	for status, typ := range map[int]string{400: "invalid_request_error", 401: "authentication_error", 402: "billing_error",
		403: "permission_error", 404: "not_found_error", 413: "request_too_large", 429: "rate_limit_error",
		500: "api_error", 504: "timeout_error", 529: "overloaded_error"} {
		s.Script(Behavior{Status: status})
		got, _, b := post(t, s, body(""), nil)
		if got != status || !bytes.Contains(b, []byte(`"type":"`+typ+`"`)) {
			t.Errorf("scripted %d: got %d %s", status, got, b)
		}
	}

	s.Script(RateLimited("7"))
	status, h, _ := post(t, s, body(""), nil)
	if status != 429 || h.Get("Retry-After") != "7" {
		t.Errorf("rate limited: %d, Retry-After %q", status, h.Get("Retry-After"))
	}

	// The conformance view translates an OpenAI envelope.
	s.OpenAI().Script(fakeupstream.APIError(400, "invalid_request_error", "invalid_value", "temperature", "Invalid 'temperature'."))
	status, _, b := post(t, s, body(""), nil)
	if status != 400 || string(b) != `{"type":"error","error":{"type":"invalid_request_error","message":"Invalid 'temperature'."}}` {
		t.Errorf("translated: %d %s", status, b)
	}
	s.OpenAI().Script(fakeupstream.Behavior{Status: 503})
	if status, _, b := post(t, s, body(""), nil); status != 503 || !bytes.Contains(b, []byte(`"api_error"`)) {
		t.Errorf("translated 503: %d %s", status, b)
	}
}

func TestErrorEventAndReplay(t *testing.T) {
	s := newServer(t)
	s.Script(Behavior{ErrorEvent: "overloaded_error", ErrorAfterEvents: 2})
	_, _, b := post(t, s, body(`,"stream":true`), nil)
	names, data := sseEvents(t, b)
	if strings.Join(names, " ") != "message_start content_block_start error" || !strings.Contains(data[2], "overloaded_error") {
		t.Errorf("events %v: %v", names, data)
	}

	replay := []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	s.Script(Behavior{Replay: replay})
	if _, h, b := post(t, s, body(`,"stream":true`), nil); !bytes.Equal(b, replay) || !strings.HasPrefix(h.Get("Content-Type"), "text/event-stream") {
		t.Errorf("replay: %q", b)
	}
	// A replay still validates the request.
	if status, _, _ := post(t, s, body(`,"bogus":1`), nil); status != 400 {
		t.Errorf("invalid request with a replay: %d", status)
	}
}

func TestOverridesAndExtraFields(t *testing.T) {
	s := newServer(t)
	s.Script(Behavior{
		StopReason:  "refusal",
		Usage:       &Usage{InputTokens: 5, OutputTokens: 6, CacheReadInputTokens: 7, CacheCreationInputTokens: 8},
		ExtraFields: map[string]json.RawMessage{"x_extra": json.RawMessage(`{"a":1}`)},
	})
	_, _, b := post(t, s, body(""), nil)
	for _, want := range []string{`"stop_reason":"refusal"`, `"input_tokens":5`, `"cache_read_input_tokens":7`,
		`"cache_creation_input_tokens":8`, `"output_tokens":6`, `"x_extra":{"a":1}`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("message %s lacks %s", b, want)
		}
	}
	_, _, b = post(t, s, body(`,"stream":true`), nil)
	_, data := sseEvents(t, b)
	for i, d := range data {
		if !strings.Contains(d, `"x_extra":{"a":1}`) {
			t.Errorf("event %d lacks the extra field: %s", i, d)
		}
	}
}

func TestClientGoneIsRecorded(t *testing.T) {
	s := newServer(t)
	s.Script(Behavior{Fault: FaultHang})
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL()+"/v1/messages", strings.NewReader(body("")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", testKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	wctx, wcancel := context.WithTimeout(context.Background(), testTimeout)
	defer wcancel()
	if err := s.OpenAI().WaitReceived(wctx, 1); err != nil {
		t.Fatal(err)
	}
	canceled := time.Now()
	cancel()
	<-done
	reqs, err := s.OpenAI().Await(wctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if gone := reqs[0].ClientGoneAt; gone.IsZero() || gone.Before(canceled) {
		t.Errorf("ClientGoneAt = %v, canceled at %v", gone, canceled)
	}
	if reqs[0].Header.Get("X-Api-Key") != testKey || reqs[0].Path != "/v1/messages" {
		t.Errorf("recorded %+v", reqs[0])
	}
	s.OpenAI().Reset()
	if len(s.Requests()) != 0 {
		t.Error("Reset kept the records")
	}
}

func TestSequenceAndHold(t *testing.T) {
	s := newServer(t)
	s.OpenAI().Sequence(fakeupstream.Behavior{Status: 529}, fakeupstream.Behavior{})
	s.Script(Behavior{Status: 500})
	for i, want := range []int{529, 200, 500} {
		if got, _, b := post(t, s, body(""), nil); got != want {
			t.Errorf("request %d: status %d, want %d: %s", i, got, want, b)
		}
	}
	s.Reset()

	release := make(chan struct{})
	s.Script(Behavior{Hold: release})
	done := make(chan int, 1)
	go func() {
		status, _, _ := post(t, s, body(""), nil)
		done <- status
	}()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := s.WaitReceived(ctx, 1); err != nil {
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
