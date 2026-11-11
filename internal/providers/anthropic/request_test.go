package anthropic

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// userHi is the Anthropic form of the user message "hi".
const userHi = `{"role":"user","content":[{"type":"text","text":"hi"}]}`

// TestTranslateRequest is the golden test of the request direction of the
// DESIGN §6.2 mapping table: each case is an OpenAI-shaped body and the
// Messages API body it must become.
func TestTranslateRequest(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		caps      []schema.Capability
		maxOutput int64
		stream    bool
		want      string
	}{
		{
			name: "minimal: max_tokens defaults to 4096",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `]}`,
		},
		{
			name:      "max_tokens defaults to the catalog's max_output_tokens",
			body:      `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			maxOutput: 128000,
			want:      `{"model":"claude-test","max_tokens":128000,"messages":[` + userHi + `]}`,
		},
		{
			name:      "max_tokens from the request wins",
			body:      `{"model":"m","max_tokens":77,"messages":[{"role":"user","content":"hi"}]}`,
			maxOutput: 128000,
			want:      `{"model":"claude-test","max_tokens":77,"messages":[` + userHi + `]}`,
		},
		{
			name: "max_completion_tokens wins over max_tokens",
			body: `{"model":"m","max_tokens":77,"max_completion_tokens":88,"messages":[{"role":"user","content":"hi"}]}`,
			want: `{"model":"claude-test","max_tokens":88,"messages":[` + userHi + `]}`,
		},
		{
			name: "system and developer messages are concatenated in order into system",
			body: `{"model":"m","messages":[
				{"role":"system","content":"You are terse."},
				{"role":"user","content":"hi"},
				{"role":"developer","content":[{"type":"text","text":"Answer in French."},{"type":"text","text":""}]},
				{"role":"system","content":""}]}`,
			want: `{"model":"claude-test","max_tokens":4096,
				"system":[{"type":"text","text":"You are terse."},{"type":"text","text":"Answer in French."}],
				"messages":[` + userHi + `]}`,
		},
		{
			name: "consecutive messages with the same role are merged",
			body: `{"model":"m","messages":[
				{"role":"user","content":"one"},
				{"role":"user","content":[{"type":"text","text":"two"},{"type":"text","text":"three"}]},
				{"role":"assistant","content":"a"},
				{"role":"system","content":"between"},
				{"role":"assistant","content":[{"type":"text","text":"b"},{"type":"refusal","refusal":"no"}]},
				{"role":"user","content":""},
				{"role":"user","content":"four"}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"system":[{"type":"text","text":"between"}],"messages":[
				{"role":"user","content":[{"type":"text","text":"one"},{"type":"text","text":"two"},{"type":"text","text":"three"}]},
				{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"text","text":"b"},{"type":"text","text":"no"}]},
				{"role":"user","content":[{"type":"text","text":"four"}]}]}`,
		},
		{
			name: "stop as a string becomes stop_sequences",
			body: `{"model":"m","stop":"END","messages":[{"role":"user","content":"hi"}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `],"stop_sequences":["END"]}`,
		},
		{
			name: "stop as an array becomes stop_sequences, without empty strings",
			body: `{"model":"m","stop":["a","","b"],"messages":[{"role":"user","content":"hi"}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `],"stop_sequences":["a","b"]}`,
		},
		{
			name: "temperature and top_p are copied exactly",
			body: `{"model":"m","temperature":0.70,"top_p":1e-1,"messages":[{"role":"user","content":"hi"}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `],"temperature":0.70,"top_p":1e-1}`,
		},
		{
			name: "null temperature and top_p are not sent",
			body: `{"model":"m","temperature":null,"top_p":null,"stop":null,"user":null,"messages":[{"role":"user","content":"hi"}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `]}`,
		},
		{
			name: "user becomes metadata.user_id",
			body: `{"model":"m","user":"user-123","safety_identifier":"other","messages":[{"role":"user","content":"hi"}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `],"metadata":{"user_id":"user-123"}}`,
		},
		{
			name: "safety_identifier becomes metadata.user_id without user",
			body: `{"model":"m","safety_identifier":"hash-9","messages":[{"role":"user","content":"hi"}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `],"metadata":{"user_id":"hash-9"}}`,
		},
		{
			name: "unmappable and unknown fields are never forwarded",
			body: `{"model":"m","n":1,"logprobs":false,"top_logprobs":2,"seed":7,"frequency_penalty":0.5,
				"presence_penalty":0.1,"logit_bias":{"1":2},"metadata":{"k":"v"},"store":true,"service_tier":"auto",
				"reasoning_effort":"high","stream_options":{"include_usage":true},"x_unknown":{"a":1},
				"messages":[{"role":"user","content":"hi","name":"alice"}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `]}`,
		},
		{
			name:   "stream sets stream",
			body:   `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			stream: true,
			want:   `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `],"stream":true}`,
		},
		{
			name: "tools: function to name, description, input_schema; strict kept only when true",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[
				{"type":"function","function":{"name":"get_weather","description":"Looks it up.","strict":true,
					"parameters":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"],"additionalProperties":false}}},
				{"type":"function","function":{"name":"now","strict":false}}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `],"tools":[
				{"name":"get_weather","description":"Looks it up.","strict":true,
					"input_schema":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"],"additionalProperties":false}},
				{"name":"now","input_schema":{"type":"object","properties":{}}}],
				"tool_choice":{"type":"auto"}}`,
		},
		{
			name: "tool_choice none",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"none","parallel_tool_calls":false,
				"tools":[{"type":"function","function":{"name":"f"}}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `],
				"tools":[{"name":"f","input_schema":{"type":"object","properties":{}}}],"tool_choice":{"type":"none"}}`,
		},
		{
			name: "tool_choice required becomes any",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"required",
				"tools":[{"type":"function","function":{"name":"f"}}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `],
				"tools":[{"name":"f","input_schema":{"type":"object","properties":{}}}],"tool_choice":{"type":"any"}}`,
		},
		{
			name: "tool_choice function becomes tool",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":"f"}},
				"tools":[{"type":"function","function":{"name":"f"}}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `],
				"tools":[{"name":"f","input_schema":{"type":"object","properties":{}}}],"tool_choice":{"type":"tool","name":"f"}}`,
		},
		{
			name: "parallel_tool_calls false becomes disable_parallel_tool_use",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"parallel_tool_calls":false,"tool_choice":"auto",
				"tools":[{"type":"function","function":{"name":"f"}}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `],
				"tools":[{"name":"f","input_schema":{"type":"object","properties":{}}}],
				"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`,
		},
		{
			name: "parallel_tool_calls true changes nothing",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"parallel_tool_calls":true,
				"tools":[{"type":"function","function":{"name":"f"}}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `],
				"tools":[{"name":"f","input_schema":{"type":"object","properties":{}}}],"tool_choice":{"type":"auto"}}`,
		},
		{
			name: "tool_choice without tools is not sent",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"required","tools":[]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `]}`,
		},
		{
			name: "assistant tool_calls become tool_use; tool messages become tool_result in one user message",
			body: `{"model":"m","messages":[
				{"role":"user","content":"weather?"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"location\": \"Paris\"}"}},
					{"id":"call_2","type":"function","function":{"name":"get_time","arguments":""}}]},
				{"role":"tool","tool_call_id":"call_1","content":"18C"},
				{"role":"tool","tool_call_id":"call_2","content":[{"type":"text","text":"12:00"}]},
				{"role":"tool","tool_call_id":"call_3","content":""},
				{"role":"user","content":"thanks"}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[
				{"role":"user","content":[{"type":"text","text":"weather?"}]},
				{"role":"assistant","content":[
					{"type":"tool_use","id":"call_1","name":"get_weather","input":{"location":"Paris"}},
					{"type":"tool_use","id":"call_2","name":"get_time","input":{}}]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"call_1","content":"18C"},
					{"type":"tool_result","tool_use_id":"call_2","content":[{"type":"text","text":"12:00"}]},
					{"type":"tool_result","tool_use_id":"call_3"},
					{"type":"text","text":"thanks"}]}]}`,
		},
		{
			name: "assistant text before tool calls",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"},
				{"role":"assistant","content":"Checking.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `,
				{"role":"assistant","content":[{"type":"text","text":"Checking."},{"type":"tool_use","id":"call_1","name":"f","input":{}}]}]}`,
		},
		{
			name: "image_url parts: data URLs become base64, https URLs become url sources",
			body: `{"model":"m","messages":[{"role":"user","content":[
				{"type":"text","text":"compare"},
				{"type":"image_url","image_url":{"url":"data:image/PNG;base64,iVBORw0KGgo=","detail":"high"}},
				{"type":"image_url","image_url":{"url":"https://example.com/cat.jpg"}}]}]}`,
			caps: []schema.Capability{schema.CapVision},
			want: `{"model":"claude-test","max_tokens":4096,"messages":[{"role":"user","content":[
				{"type":"text","text":"compare"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}},
				{"type":"image","source":{"type":"url","url":"https://example.com/cat.jpg"}}]}]}`,
		},
		{
			name: "json_schema uses native structured output when the deployment declares json_schema",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema",
				"json_schema":{"name":"answer","strict":true,"schema":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}}}}`,
			caps: []schema.Capability{schema.CapJSONSchema},
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `],
				"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}}}}`,
		},
		{
			name: "json_schema is reduced to the structured-outputs subset, strict or not",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema",
				"json_schema":{"name":"answer","strict":false,"schema":{"type":"object","properties":{"age":{"type":"integer","minimum":0}}}}}}`,
			caps: []schema.Capability{schema.CapJSONSchema},
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `],
				"output_config":{"format":{"type":"json_schema","schema":{"type":"object",
					"properties":{"age":{"type":"integer","description":"{minimum: 0}"}},"additionalProperties":false}}}}`,
		},
		{
			name: "tool-call ids outside Anthropic's pattern are mapped the same way in both places",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"},
				{"role":"assistant","tool_calls":[{"id":"functions.now:0","type":"function","function":{"name":"now","arguments":"{}"}},
					{"id":"call_ok-1","type":"function","function":{"name":"now","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"functions.now:0","content":"12:00"},
				{"role":"tool","tool_call_id":"call_ok-1","content":"12:01"}],
				"tools":[{"type":"function","function":{"name":"now"}}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `,
				{"role":"assistant","content":[{"type":"tool_use","id":"` + toolUseID("functions.now:0") + `","name":"now","input":{}},
					{"type":"tool_use","id":"call_ok-1","name":"now","input":{}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + toolUseID("functions.now:0") + `","content":"12:00"},
					{"type":"tool_result","tool_use_id":"call_ok-1","content":"12:01"}]}],
				"tools":[{"name":"now","input_schema":{"type":"object","properties":{}}}],"tool_choice":{"type":"auto"}}`,
		},
		{
			name: "json_object asks for JSON in the system prompt",
			body: `{"model":"m","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`,
			caps: []schema.Capability{schema.CapJSONMode},
			want: `{"model":"claude-test","max_tokens":4096,
				"system":[{"type":"text","text":"Be brief."},{"type":"text","text":"` + JSONObjectInstruction + `"}],"messages":[` + userHi + `]}`,
		},
		{
			name: "json_schema without a schema is treated as json_object",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"x"}}}`,
			want: `{"model":"claude-test","max_tokens":4096,"system":[{"type":"text","text":"` + JSONObjectInstruction + `"}],"messages":[` + userHi + `]}`,
		},
		{
			name: "response_format text changes nothing",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"text"}}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[` + userHi + `]}`,
		},
		{
			name: "text keeps <, > and & unescaped",
			body: `{"model":"m","messages":[{"role":"user","content":"a <b> & c"}]}`,
			want: `{"model":"claude-test","max_tokens":4096,"messages":[{"role":"user","content":[{"type":"text","text":"a <b> & c"}]}]}`,
		},
	}
	a := newAdapter(t, testProvider("http://upstream.test"))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := request(t, tt.body, tt.caps...)
			req.MaxOutputTokens = tt.maxOutput
			body, err := a.translate(req, tt.stream)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if diff := jsonDiff(t, []byte(tt.want), body); diff != "" {
				t.Errorf("Messages body (-want +got):\n%s\ngot %s", diff, body)
			}
			if strings.Contains(string(body), `\`+"u003c") {
				t.Errorf("body escapes HTML: %s", body)
			}
		})
	}
}

// TestTranslateRequestErrors checks the requests the Messages API cannot
// express: they fail as a 400 client error with an OpenAI envelope, and
// nothing is sent upstream.
func TestTranslateRequestErrors(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		caps  []schema.Capability
		param string
	}{
		{"tool call arguments are not JSON", `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{nope"}}]}]}`, nil, "messages"},
		{"tool call arguments are not an object", `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"[1]"}}]}]}`, nil, "messages"},
		{"tool call without an id", `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]}]}`, nil, "messages"},
		{"tool message without tool_call_id", `{"model":"m","messages":[{"role":"tool","content":"x"}]}`, nil, "messages"},
		{"audio part", `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AA==","format":"wav"}}]}]}`, nil, "messages"},
		{"image in a system message", `{"model":"m","messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"https://x.test/a.png"}}]},{"role":"user","content":"hi"}]}`, []schema.Capability{schema.CapVision}, "messages"},
		{"http image URL", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://example.com/a.png"}}]}]}`, []schema.Capability{schema.CapVision}, "messages"},
		{"unsupported image type", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/bmp;base64,AAAA"}}]}]}`, []schema.Capability{schema.CapVision}, "messages"},
		{"data URL that is not base64", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png,AAAA"}}]}]}`, []schema.Capability{schema.CapVision}, "messages"},
		{"image on a deployment without vision", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`, nil, "messages"},
		{"image behind a case-folded type on a deployment without vision", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","TYPE":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`, nil, "messages"},
		{"unknown role", `{"model":"m","messages":[{"role":"robot","content":"x"}]}`, nil, "messages"},
		{"function role", `{"model":"m","messages":[{"role":"function","name":"f","content":"x"}]}`, nil, "messages"},
		{"assistant function_call", `{"model":"m","messages":[{"role":"assistant","function_call":{"name":"f","arguments":"{}"}}]}`, nil, "messages"},
		{"only a system message", `{"model":"m","messages":[{"role":"system","content":"x"}]}`, nil, "messages"},
		{"legacy functions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"functions":[{"name":"f"}]}`, nil, "functions"},
		{"legacy function_call", `{"model":"m","messages":[{"role":"user","content":"hi"}],"function_call":"auto"}`, nil, "function_call"},
		{"custom tool", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","custom":{"name":"f"}}]}`, nil, "tools"},
		{"allowed_tools tool_choice", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[]}}}`, nil, "tool_choice"},
		{"stop of the wrong type", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":7}`, nil, "stop"},
		{"json_schema on a deployment without json_schema", `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}}}`, []schema.Capability{schema.CapForcedTools}, "response_format"},
		{"json_object on a deployment without json_mode", `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`, nil, "response_format"},
		{"json_object behind a case-folded type on a deployment without json_mode", `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"text","TYPE":"json_object"}}`, nil, "response_format"},
		{"unknown response_format", `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"yaml"}}`, nil, "response_format"},
	}
	a := newAdapter(t, testProvider("http://upstream.test"))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := a.translate(request(t, tt.body, tt.caps...), false)
			pe := wantKind(t, err, providers.KindClientError)
			if pe.Status != 400 {
				t.Errorf("status = %d, want 400", pe.Status)
			}
			var env struct {
				Error struct {
					Message, Type, Param, Code string
				} `json:"error"`
			}
			if err := json.Unmarshal(pe.Body, &env); err != nil {
				t.Fatalf("body %s: %v", pe.Body, err)
			}
			if env.Error.Type != "invalid_request_error" || env.Error.Param != tt.param || env.Error.Code != "invalid_value" || env.Error.Message == "" {
				t.Errorf("envelope = %s, want type invalid_request_error, param %s, code invalid_value", pe.Body, tt.param)
			}
		})
	}
}

func TestToolUseID(t *testing.T) {
	pattern := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	tests := []struct{ id, want string }{
		{"call_abc-123", "call_abc-123"},
		{"toolu_01A", "toolu_01A"},
		{"functions.now:0", "functions_now_0_"},
		{"a b", "a_b_"},
		{"é", "__"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got := toolUseID(tt.id)
			if !strings.HasPrefix(got, tt.want) || (got != tt.want && len(got) != len(tt.want)+8) {
				t.Errorf("toolUseID(%q) = %q, want %q (plus an 8-character hash if mapped)", tt.id, got, tt.want)
			}
			if !pattern.MatchString(got) {
				t.Errorf("toolUseID(%q) = %q does not match %s", tt.id, got, pattern)
			}
			if again := toolUseID(tt.id); again != got {
				t.Errorf("not deterministic: %q then %q", got, again)
			}
		})
	}
	if a, b := toolUseID("a.b"), toolUseID("a:b"); a == b {
		t.Errorf("distinct ids a.b and a:b both map to %q", a)
	}
}
