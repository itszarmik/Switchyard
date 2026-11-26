package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// userHi is the Gemini form of the user message "hi".
const userHi = `{"role":"user","parts":[{"text":"hi"}]}`

// sigA and sigB stand for thought signatures Gemini returned.
const (
	sigA = "CiQBjz1rX2VxYW1wbGUtc2lnbmF0dXJlLWZvci1zdGVwLTE="
	sigB = "CiQBjz1rX2V4YW1wbGUtc2lnbmF0dXJlLWZvci1zdGVwLTI="
)

// TestTranslateRequest is the golden test of the request direction of the
// DESIGN §6.2 Gemini mapping table: each case is an OpenAI-shaped body and
// the generateContent body it must become. Every body sends store: false
// and never a sampling parameter.
func TestTranslateRequest(t *testing.T) {
	ours := func(id, sig string) string { return encodeToolCallID(testModel, id, sig) }
	other := encodeToolCallID("gemini-other", "fc_9", sigA)
	tampered := ours("fc_1", sigA)
	tampered = tampered[:len(tampered)-3] + "AAA"
	if tampered == ours("fc_1", sigA) {
		t.Fatal("the tampered id is unchanged")
	}
	tests := []struct {
		name   string
		body   string
		caps   []schema.Capability
		want   string
		maxOut int64
	}{
		{
			name: "minimal: contents and store false, nothing else",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			want: `{"contents":[` + userHi + `],"store":false}`,
		},
		{
			name:   "max_tokens becomes maxOutputTokens",
			body:   `{"model":"m","max_tokens":77,"messages":[{"role":"user","content":"hi"}]}`,
			want:   `{"contents":[` + userHi + `],"generationConfig":{"maxOutputTokens":77},"store":false}`,
			maxOut: 77,
		},
		{
			name:   "max_completion_tokens wins over max_tokens",
			body:   `{"model":"m","max_tokens":77,"max_completion_tokens":40000,"messages":[{"role":"user","content":"hi"}]}`,
			want:   `{"contents":[` + userHi + `],"generationConfig":{"maxOutputTokens":40000},"store":false}`,
			maxOut: 40000,
		},
		{
			name: "system and developer messages become systemInstruction parts, in order",
			body: `{"model":"m","messages":[
				{"role":"system","content":"You are terse."},
				{"role":"user","content":"hi"},
				{"role":"developer","content":[{"type":"text","text":"Answer in French."},{"type":"text","text":""}]},
				{"role":"system","content":""}]}`,
			want: `{"contents":[` + userHi + `],
				"systemInstruction":{"parts":[{"text":"You are terse."},{"text":"Answer in French."}]},"store":false}`,
		},
		{
			name: "user becomes user, assistant becomes model; consecutive messages are merged",
			body: `{"model":"m","messages":[
				{"role":"user","content":"one"},
				{"role":"user","content":[{"type":"text","text":"two"}]},
				{"role":"assistant","content":"a"},
				{"role":"assistant","content":[{"type":"text","text":"b"},{"type":"refusal","refusal":"no"}]},
				{"role":"user","content":""},
				{"role":"user","content":"three"}]}`,
			want: `{"contents":[
				{"role":"user","parts":[{"text":"one"},{"text":"two"}]},
				{"role":"model","parts":[{"text":"a"},{"text":"b"},{"text":"no"}]},
				{"role":"user","parts":[{"text":"three"}]}],"store":false}`,
		},
		{
			name: "stop as a string becomes stopSequences",
			body: `{"model":"m","stop":"END","messages":[{"role":"user","content":"hi"}]}`,
			want: `{"contents":[` + userHi + `],"generationConfig":{"stopSequences":["END"]},"store":false}`,
		},
		{
			name: "stop as an array becomes stopSequences, without empty strings",
			body: `{"model":"m","stop":["a","","b"],"messages":[{"role":"user","content":"hi"}]}`,
			want: `{"contents":[` + userHi + `],"generationConfig":{"stopSequences":["a","b"]},"store":false}`,
		},
		{
			name: "no-op and unmappable fields are never forwarded",
			body: `{"model":"m","n":1,"logprobs":false,"top_logprobs":2,"frequency_penalty":0,"presence_penalty":0,
				"user":"u-1","safety_identifier":"s","metadata":{"k":"v"},"store":true,"service_tier":"auto",
				"stream_options":{"include_usage":true},"modalities":["text"],"audio":{"voice":"x"},
				"prediction":{"type":"content","content":"x"},"web_search_options":{},"parallel_tool_calls":false,
				"temperature":null,"top_p":null,"seed":null,"x_unknown":{"a":1},
				"messages":[{"role":"user","content":"hi","name":"alice"}]}`,
			want: `{"contents":[` + userHi + `],"store":false}`,
		},
		{
			name: "tools become functionDeclarations with parametersJsonSchema as is; none without parameters",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[
				{"type":"function","function":{"name":"get_weather","description":"Looks it up.","strict":true,
					"parameters":{"type":"object","properties":{"location":{"type":"string","minLength":1}},"required":["location"],"additionalProperties":false}}},
				{"type":"function","function":{"name":"now","parameters":null}}]}`,
			want: `{"contents":[` + userHi + `],"tools":[{"functionDeclarations":[
				{"name":"get_weather","description":"Looks it up.",
					"parametersJsonSchema":{"type":"object","properties":{"location":{"type":"string","minLength":1}},"required":["location"],"additionalProperties":false}},
				{"name":"now"}]}],"store":false}`,
		},
		{
			name: "tool_choice auto becomes AUTO",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"auto","tools":[{"type":"function","function":{"name":"f"}}]}`,
			want: `{"contents":[` + userHi + `],"tools":[{"functionDeclarations":[{"name":"f"}]}],
				"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},"store":false}`,
		},
		{
			name: "tool_choice none becomes NONE",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"none","tools":[{"type":"function","function":{"name":"f"}}]}`,
			want: `{"contents":[` + userHi + `],"tools":[{"functionDeclarations":[{"name":"f"}]}],
				"toolConfig":{"functionCallingConfig":{"mode":"NONE"}},"store":false}`,
		},
		{
			name: "tool_choice required becomes ANY",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"required","tools":[{"type":"function","function":{"name":"f"}}]}`,
			want: `{"contents":[` + userHi + `],"tools":[{"functionDeclarations":[{"name":"f"}]}],
				"toolConfig":{"functionCallingConfig":{"mode":"ANY"}},"store":false}`,
		},
		{
			name: "a named tool_choice becomes ANY with allowedFunctionNames",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":"g"}},
				"tools":[{"type":"function","function":{"name":"f"}},{"type":"function","function":{"name":"g"}}]}`,
			want: `{"contents":[` + userHi + `],"tools":[{"functionDeclarations":[{"name":"f"},{"name":"g"}]}],
				"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["g"]}},"store":false}`,
		},
		{
			name: "tool_choice without tools is not sent",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"required","tools":[]}`,
			want: `{"contents":[` + userHi + `],"store":false}`,
		},
		{
			name: "foreign tool-call ids: the client's id, the skip value on the first call; tool results in one user content",
			body: `{"model":"m","messages":[
				{"role":"user","content":"weather?"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"toolu_01A","type":"function","function":{"name":"get_weather","arguments":"{\"location\": \"Paris\"}"}},
					{"id":"toolu_01B","type":"function","function":{"name":"get_time","arguments":""}}]},
				{"role":"tool","tool_call_id":"toolu_01A","content":"{\"temp\": 18}"},
				{"role":"tool","tool_call_id":"toolu_01B","content":[{"type":"text","text":"12:"},{"type":"text","text":"00"}]},
				{"role":"user","content":"thanks"}]}`,
			want: `{"contents":[
				{"role":"user","parts":[{"text":"weather?"}]},
				{"role":"model","parts":[
					{"functionCall":{"id":"toolu_01A","name":"get_weather","args":{"location":"Paris"}},"thoughtSignature":"` + SkipSignature + `"},
					{"functionCall":{"id":"toolu_01B","name":"get_time","args":{}}}]},
				{"role":"user","parts":[
					{"functionResponse":{"id":"toolu_01A","name":"get_weather","response":{"temp":18}}},
					{"functionResponse":{"id":"toolu_01B","name":"get_time","response":{"result":"12:00"}}}]},
				{"role":"user","parts":[{"text":"thanks"}]}],"store":false}`,
		},
		{
			name: "sequential steps: each first call gets back its own signature and Gemini id",
			body: `{"model":"m","messages":[
				{"role":"user","content":"plan a trip"},
				{"role":"assistant","tool_calls":[{"id":"` + ours("fc_1", sigA) + `","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"` + ours("fc_1", sigA) + `","content":"sunny"},
				{"role":"assistant","tool_calls":[{"id":"` + ours("fc_2", sigB) + `","type":"function","function":{"name":"book","arguments":"{\"day\":1}"}}]},
				{"role":"tool","tool_call_id":"` + ours("fc_2", sigB) + `","content":""}]}`,
			want: `{"contents":[
				{"role":"user","parts":[{"text":"plan a trip"}]},
				{"role":"model","parts":[{"functionCall":{"id":"fc_1","name":"get_weather","args":{}},"thoughtSignature":"` + sigA + `"}]},
				{"role":"user","parts":[{"functionResponse":{"id":"fc_1","name":"get_weather","response":{"result":"sunny"}}}]},
				{"role":"model","parts":[{"functionCall":{"id":"fc_2","name":"book","args":{"day":1}},"thoughtSignature":"` + sigB + `"}]},
				{"role":"user","parts":[{"functionResponse":{"id":"fc_2","name":"book","response":{"result":""}}}]}],"store":false}`,
		},
		{
			name: "parallel calls: the signature on the first call only, text before them kept",
			body: `{"model":"m","messages":[
				{"role":"user","content":"both"},
				{"role":"assistant","content":"Checking.","tool_calls":[
					{"id":"` + ours("fc_a", sigA) + `","type":"function","function":{"name":"f","arguments":"{}"}},
					{"id":"` + ours("fc_b", "") + `","type":"function","function":{"name":"g","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"` + ours("fc_b", "") + `","content":"2"},
				{"role":"tool","tool_call_id":"` + ours("fc_a", sigA) + `","content":"1"}]}`,
			want: `{"contents":[
				{"role":"user","parts":[{"text":"both"}]},
				{"role":"model","parts":[{"text":"Checking."},
					{"functionCall":{"id":"fc_a","name":"f","args":{}},"thoughtSignature":"` + sigA + `"},
					{"functionCall":{"id":"fc_b","name":"g","args":{}}}]},
				{"role":"user","parts":[
					{"functionResponse":{"id":"fc_b","name":"g","response":{"result":"2"}}},
					{"functionResponse":{"id":"fc_a","name":"f","response":{"result":"1"}}}]}],"store":false}`,
		},
		{
			name: "an id from another Gemini model: its Gemini id and the skip value; a tampered id: the client's id and the skip value",
			body: `{"model":"m","messages":[
				{"role":"user","content":"go"},
				{"role":"assistant","tool_calls":[{"id":"` + other + `","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"` + other + `","content":"x"},
				{"role":"assistant","tool_calls":[{"id":"` + tampered + `","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"` + tampered + `","content":"y"}]}`,
			want: `{"contents":[
				{"role":"user","parts":[{"text":"go"}]},
				{"role":"model","parts":[{"functionCall":{"id":"fc_9","name":"f","args":{}},"thoughtSignature":"` + SkipSignature + `"}]},
				{"role":"user","parts":[{"functionResponse":{"id":"fc_9","name":"f","response":{"result":"x"}}}]},
				{"role":"model","parts":[{"functionCall":{"id":"` + tampered + `","name":"f","args":{}},"thoughtSignature":"` + SkipSignature + `"}]},
				{"role":"user","parts":[{"functionResponse":{"id":"` + tampered + `","name":"f","response":{"result":"y"}}}]}],"store":false}`,
		},
		{
			name: "our id without a signature (Gemini returned the call unsigned) gets the skip value",
			body: `{"model":"m","messages":[{"role":"user","content":"go"},
				{"role":"assistant","tool_calls":[{"id":"` + ours("fc_1", "") + `","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"` + ours("fc_1", "") + `","content":"x"}]}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"go"}]},
				{"role":"model","parts":[{"functionCall":{"id":"fc_1","name":"f","args":{}},"thoughtSignature":"` + SkipSignature + `"}]},
				{"role":"user","parts":[{"functionResponse":{"id":"fc_1","name":"f","response":{"result":"x"}}}]}],"store":false}`,
		},
		{
			name: "an assistant text message merges into the tool calls after it, but two steps never merge",
			body: `{"model":"m","messages":[{"role":"user","content":"go"},
				{"role":"assistant","content":"Thinking aloud."},
				{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"assistant","tool_calls":[{"id":"c2","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"c2","content":"x"}]}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"go"}]},
				{"role":"model","parts":[{"text":"Thinking aloud."},
					{"functionCall":{"id":"c1","name":"f","args":{}},"thoughtSignature":"` + SkipSignature + `"}]},
				{"role":"model","parts":[{"functionCall":{"id":"c2","name":"f","args":{}},"thoughtSignature":"` + SkipSignature + `"}]},
				{"role":"user","parts":[{"functionResponse":{"id":"c2","name":"f","response":{"result":"x"}}}]}],"store":false}`,
		},
		{
			name: "image data URLs become inlineData",
			body: `{"model":"m","messages":[{"role":"user","content":[
				{"type":"text","text":"compare"},
				{"type":"image_url","image_url":{"url":"data:image/PNG;base64,iVBORw0KGgo=","detail":"high"}},
				{"type":"image_url","image_url":{"url":"data:image/webp;base64,UklGRg=="}}]}]}`,
			caps: []schema.Capability{schema.CapVision},
			want: `{"contents":[{"role":"user","parts":[{"text":"compare"},
				{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}},
				{"inlineData":{"mimeType":"image/webp","data":"UklGRg=="}}]}],"store":false}`,
		},
		{
			name: "json_object becomes responseMimeType application/json",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`,
			caps: []schema.Capability{schema.CapJSONMode},
			want: `{"contents":[` + userHi + `],"generationConfig":{"responseMimeType":"application/json"},"store":false}`,
		},
		{
			name: "json_schema becomes responseMimeType and responseJsonSchema, the schema as is",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema",
				"json_schema":{"name":"a","strict":true,"schema":{"type":"object","properties":{"a":{"type":"integer","minimum":0}},"required":["a"]}}}}`,
			caps: []schema.Capability{schema.CapJSONSchema},
			want: `{"contents":[` + userHi + `],"generationConfig":{"responseMimeType":"application/json",
				"responseJsonSchema":{"type":"object","properties":{"a":{"type":"integer","minimum":0}},"required":["a"]}},"store":false}`,
		},
		{
			name: "json_schema without a schema is json_object",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"x"}}}`,
			want: `{"contents":[` + userHi + `],"generationConfig":{"responseMimeType":"application/json"},"store":false}`,
		},
		{
			name: "response_format text changes nothing",
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"text"}}`,
			want: `{"contents":[` + userHi + `],"store":false}`,
		},
		{
			name: "text keeps <, > and & unescaped",
			body: `{"model":"m","messages":[{"role":"user","content":"a <b> & c"}]}`,
			want: `{"contents":[{"role":"user","parts":[{"text":"a <b> & c"}]}],"store":false}`,
		},
	}
	for effort, level := range map[string]string{
		"none": "LOW", "minimal": "LOW", "low": "LOW", "medium": "MEDIUM", "high": "HIGH", "xhigh": "HIGH",
	} {
		tests = append(tests, struct {
			name   string
			body   string
			caps   []schema.Capability
			want   string
			maxOut int64
		}{
			name: "reasoning_effort " + effort + " becomes thinkingLevel " + level,
			body: `{"model":"m","reasoning_effort":"` + effort + `","messages":[{"role":"user","content":"hi"}]}`,
			want: `{"contents":[` + userHi + `],"generationConfig":{"thinkingConfig":{"thinkingLevel":"` + level + `"}},"store":false}`,
		})
	}
	a := newAdapter(t, testProvider("http://upstream.test"))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, maxOut, err := a.translate(request(t, tt.body, tt.caps...))
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if diff := jsonDiff(t, []byte(tt.want), body); diff != "" {
				t.Errorf("generateContent body (-want +got):\n%s\ngot %s", diff, body)
			}
			if maxOut != tt.maxOut {
				t.Errorf("maxOutputTokens = %d, want %d", maxOut, tt.maxOut)
			}
			if strings.Contains(string(body), `\`+"u003c") {
				t.Errorf("body escapes HTML: %s", body)
			}
			for _, field := range []string{"temperature", "topP", "topK", "candidateCount", "seed", "Penalty", "logprobs", "thinkingBudget", "includeThoughts"} {
				if strings.Contains(string(body), `"`+field) {
					t.Errorf("body sends %s: %s", field, body)
				}
			}
		})
	}
}

// TestTranslateRequestErrors checks the requests the Gemini API cannot
// express: they fail as KindUnsupported with an OpenAI 400 envelope naming
// the parameter, so another deployment can serve them, and nothing is sent
// upstream.
func TestTranslateRequestErrors(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		caps  []schema.Capability
		param string
	}{
		{"n above 1", `{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}]}`, nil, "n"},
		{"logprobs", `{"model":"m","logprobs":true,"messages":[{"role":"user","content":"hi"}]}`, nil, "logprobs"},
		{"seed", `{"model":"m","seed":7,"messages":[{"role":"user","content":"hi"}]}`, nil, "seed"},
		{"temperature", `{"model":"m","temperature":0.2,"messages":[{"role":"user","content":"hi"}]}`, nil, "temperature"},
		{"top_p", `{"model":"m","top_p":0.9,"messages":[{"role":"user","content":"hi"}]}`, nil, "temperature"},
		{"temperature on a deployment that claims sampling", `{"model":"m","temperature":0.2,"messages":[{"role":"user","content":"hi"}]}`, []schema.Capability{schema.CapSampling}, "temperature"},
		{"penalties", `{"model":"m","presence_penalty":0.5,"messages":[{"role":"user","content":"hi"}]}`, nil, "frequency_penalty"},
		{"https image URL", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`, []schema.Capability{schema.CapVision}, "messages"},
		{"http image URL", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://example.com/a.png"}}]}]}`, []schema.Capability{schema.CapVision}, "messages"},
		{"unsupported image type", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/gif;base64,R0lG"}}]}]}`, []schema.Capability{schema.CapVision}, "messages"},
		{"data URL that is not base64", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png,AAAA"}}]}]}`, []schema.Capability{schema.CapVision}, "messages"},
		{"image on a deployment without vision", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`, nil, "messages"},
		{"image in a system message", `{"model":"m","messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},{"role":"user","content":"hi"}]}`, []schema.Capability{schema.CapVision}, "messages"},
		{"audio part", `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AA==","format":"wav"}}]}]}`, nil, "messages"},
		{"trailing assistant message (prefill)", `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"Sure, "}]}`, nil, "messages"},
		{"more than 5 stop sequences", `{"model":"m","stop":["a","b","c","d","e","f"],"messages":[{"role":"user","content":"hi"}]}`, nil, "stop"},
		{"stop of the wrong type", `{"model":"m","stop":7,"messages":[{"role":"user","content":"hi"}]}`, nil, "stop"},
		{"unknown reasoning_effort", `{"model":"m","reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`, nil, "reasoning_effort"},
		{"tool call arguments are not JSON", `{"model":"m","messages":[{"role":"user","content":"x"},{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{nope"}}]},{"role":"tool","tool_call_id":"c","content":"r"}]}`, nil, "messages"},
		{"tool call arguments are not an object", `{"model":"m","messages":[{"role":"user","content":"x"},{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"[1]"}}]},{"role":"tool","tool_call_id":"c","content":"r"}]}`, nil, "messages"},
		{"tool call without an id", `{"model":"m","messages":[{"role":"user","content":"x"},{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]}]}`, nil, "messages"},
		{"tool message without tool_call_id", `{"model":"m","messages":[{"role":"user","content":"x"},{"role":"tool","content":"x"}]}`, nil, "messages"},
		{"unknown role", `{"model":"m","messages":[{"role":"robot","content":"x"}]}`, nil, "messages"},
		{"function role", `{"model":"m","messages":[{"role":"function","name":"f","content":"x"}]}`, nil, "messages"},
		{"assistant function_call", `{"model":"m","messages":[{"role":"user","content":"x"},{"role":"assistant","function_call":{"name":"f","arguments":"{}"}}]}`, nil, "messages"},
		{"only a system message", `{"model":"m","messages":[{"role":"system","content":"x"}]}`, nil, "messages"},
		{"legacy functions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"functions":[{"name":"f"}]}`, nil, "functions"},
		{"legacy function_call", `{"model":"m","messages":[{"role":"user","content":"hi"}],"function_call":"auto"}`, nil, "function_call"},
		{"custom tool", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","custom":{"name":"f"}}]}`, nil, "tools"},
		{"allowed_tools tool_choice", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[]}}}`, nil, "tool_choice"},
		{"json_schema on a deployment without json_schema", `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}}}`, []schema.Capability{schema.CapJSONMode}, "response_format"},
		{"json_object on a deployment without json_mode", `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`, nil, "response_format"},
		{"unknown response_format", `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"yaml"}}`, nil, "response_format"},
	}
	a := newAdapter(t, testProvider("http://upstream.test"))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := a.translate(request(t, tt.body, tt.caps...))
			checkEnvelope(t, wantKind(t, err, providers.KindUnsupported), tt.param, "invalid_value")
		})
	}

	t.Run("a tool message that answers no tool call is a client error", func(t *testing.T) {
		_, _, err := a.translate(request(t, `{"model":"m","messages":[{"role":"user","content":"x"},
			{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c2","content":"r"}]}`))
		pe := wantKind(t, err, providers.KindClientError)
		checkEnvelope(t, pe, "messages", "invalid_value")
		if pe.Kind.FallsBack() {
			t.Error("the error falls back; every provider rejects such a request")
		}
	})
}

// checkEnvelope checks that pe is a 400 with an OpenAI envelope of type
// invalid_request_error naming param, with code.
func checkEnvelope(t *testing.T, pe *providers.Error, param, code string) {
	t.Helper()
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
	if env.Error.Type != "invalid_request_error" || env.Error.Param != param || env.Error.Code != code || env.Error.Message == "" {
		t.Errorf("envelope = %s, want type invalid_request_error, param %s, code %s", pe.Body, param, code)
	}
}
