package judge

import (
	"strings"
	"testing"

	"switchyard/internal/schema"
)

func TestAnswerText(t *testing.T) {
	tests := []struct {
		name, body, want string
		err              bool
	}{
		{name: "text", body: `{"choices":[{"message":{"role":"assistant","content":"Paris."},"finish_reason":"stop"}]}`, want: "Paris."},
		{name: "tool calls", body: `{"choices":[{"message":{"content":null,"tool_calls":[{"type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}}]}}]}`,
			want: `[tool call] get_weather({"city":"Oslo"})`},
		{name: "refusal", body: `{"choices":[{"message":{"content":null,"refusal":"I can't help with that."}}]}`, want: "[refusal] I can't help with that."},
		{name: "cut off", body: `{"choices":[{"message":{"content":"The first"},"finish_reason":"length"}]}`, want: "The first\n[the answer was cut off at its output token limit]"},
		{name: "parts", body: `{"choices":[{"message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}]}`, want: "a\nb"},
		{name: "empty", body: `{"choices":[{"message":{"content":""}}]}`, want: ""},
		{name: "no choices", body: `{"choices":[]}`, err: true},
		{name: "not json", body: `secret`, err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AnswerText([]byte(tt.body))
			if (err != nil) != tt.err || got != tt.want {
				t.Errorf("AnswerText = %q, %v; want %q, error %v", got, err, tt.want, tt.err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Errorf("error echoes the body: %v", err)
			}
		})
	}
}

func TestRenderRequest(t *testing.T) {
	req, err := schema.DecodeChatRequest([]byte(`{"model":"m",
		"messages":[
			{"role":"system","content":"Be terse."},
			{"role":"user","content":[{"type":"text","text":"What is in this picture?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
			{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"a cat"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"response_format":{"type":"json_object"}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := RenderRequest(req)
	for _, want := range []string{"[system]\nBe terse.", "[user]\nWhat is in this picture?\n[image]", "[tool call] lookup({})",
		"[tool result for c1]\na cat", `[tools available]` + "\n" + `{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}`,
		`[response_format] {"type":"json_object"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("rendering lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "base64") {
		t.Errorf("an image's data reached the judge prompt:\n%s", got)
	}
}
