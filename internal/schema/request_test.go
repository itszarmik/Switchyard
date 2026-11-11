package schema

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestDecodeChatRequestErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want RequestError
	}{
		{name: "not JSON", body: `{"model":`, want: RequestError{Code: CodeInvalidJSON}},
		{name: "empty body", body: ``, want: RequestError{Code: CodeInvalidJSON}},
		{name: "array", body: `[]`, want: RequestError{Code: CodeInvalidJSON}},
		{name: "string", body: `"x"`, want: RequestError{Code: CodeInvalidJSON}},
		{name: "trailing data", body: `{"model":"m","messages":[{}]} {}`, want: RequestError{Code: CodeInvalidJSON}},
		{name: "trailing garbage", body: `{"model":"m","messages":[{}]} x`, want: RequestError{Code: CodeInvalidJSON}},
		{name: "missing model", body: `{"messages":[{}]}`, want: RequestError{Param: "model", Code: CodeMissingParameter}},
		{name: "null model", body: `{"model":null,"messages":[{}]}`, want: RequestError{Param: "model", Code: CodeMissingParameter}},
		{name: "empty model", body: `{"model":"","messages":[{}]}`, want: RequestError{Param: "model", Code: CodeMissingParameter}},
		{name: "numeric model", body: `{"model":4,"messages":[{}]}`, want: RequestError{Param: "model", Code: CodeInvalidType}},
		{name: "model too long", body: `{"model":"` + strings.Repeat("m", 257) + `","messages":[{}]}`, want: RequestError{Param: "model", Code: CodeInvalidValue}},
		{name: "model too long in bytes", body: `{"model":"` + strings.Repeat("é", 129) + `","messages":[{}]}`, want: RequestError{Param: "model", Code: CodeInvalidValue}},
		{name: "missing messages", body: `{"model":"m"}`, want: RequestError{Param: "messages", Code: CodeMissingParameter}},
		{name: "null messages", body: `{"model":"m","messages":null}`, want: RequestError{Param: "messages", Code: CodeMissingParameter}},
		{name: "empty messages", body: `{"model":"m","messages":[]}`, want: RequestError{Param: "messages", Code: CodeEmptyArray}},
		{name: "object messages", body: `{"model":"m","messages":{}}`, want: RequestError{Param: "messages", Code: CodeInvalidType}},
		{name: "string stream", body: `{"model":"m","messages":[{}],"stream":"yes"}`, want: RequestError{Param: "stream", Code: CodeInvalidType}},
		{name: "numeric stream", body: `{"model":"m","messages":[{}],"stream":1}`, want: RequestError{Param: "stream", Code: CodeInvalidType}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeChatRequest([]byte(tt.body))
			var re *RequestError
			if !errors.As(err, &re) {
				t.Fatalf("error = %v, want a *RequestError", err)
			}
			if re.Param != tt.want.Param || re.Code != tt.want.Code {
				t.Errorf("got param %q code %q, want param %q code %q", re.Param, re.Code, tt.want.Param, tt.want.Code)
			}
			if re.Message == "" {
				t.Error("empty message")
			}
		})
	}
}

func TestDecodeChatRequestMessages(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "missing", body: `{"messages":[{}]}`, want: "Missing required parameter: 'model'."},
		{name: "type", body: `{"model":"m","messages":"hi"}`, want: "Invalid type for 'messages': expected an array, but got a string instead."},
		{name: "stream type", body: `{"model":"m","messages":[{}],"stream":{}}`, want: "Invalid type for 'stream': expected a boolean, but got an object instead."},
		{name: "model too long", body: `{"model":"` + strings.Repeat("m", 300) + `","messages":[{}]}`, want: "Invalid 'model': string too long. Expected a string with maximum length 256 bytes, but got a string with length 300 instead."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeChatRequest([]byte(tt.body))
			if err == nil || err.Error() != tt.want {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestChatRequestAccessors(t *testing.T) {
	body := `{
		"model": "gpt-x",
		"stream": true,
		"stream_options": {"include_usage": true},
		"n": 3,
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "user", "content": [{"type": "text", "text": "hi"}]},
			"not an object",
			{"role": 7}
		],
		"tools": [{"type": "function", "function": {"name": "f"}}],
		"tool_choice": "auto",
		"response_format": {"type": "json_schema", "json_schema": {}},
		"logprobs": true,
		"seed": 42,
		"temperature": 0.5,
		"max_tokens": 10,
		"max_completion_tokens": 20,
		"x_unknown": {"keep": [1, 2]}
	}`
	r, err := DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeChatRequest: %v", err)
	}
	if r.Model() != "gpt-x" || !r.Stream() || !r.IncludeUsage() {
		t.Errorf("model/stream/include_usage = %q %v %v", r.Model(), r.Stream(), r.IncludeUsage())
	}
	if n, ok := r.N(); !ok || n != 3 {
		t.Errorf("N = %d %v", n, ok)
	}
	roles := []string{}
	for _, m := range r.Messages() {
		roles = append(roles, m.Role)
	}
	if diff := cmp.Diff([]string{"system", "user", "", ""}, roles); diff != "" {
		t.Errorf("roles (-want +got):\n%s", diff)
	}
	if got := string(r.Messages()[1].Content); got != `[{"type": "text", "text": "hi"}]` {
		t.Errorf("content kept raw = %s", got)
	}
	if len(r.Tools()) != 1 || string(r.ToolChoice()) != `"auto"` {
		t.Errorf("tools/tool_choice = %d %s", len(r.Tools()), r.ToolChoice())
	}
	if rf, ok := r.ResponseFormat(); !ok || rf.Type != "json_schema" {
		t.Errorf("response_format = %+v %v", rf, ok)
	}
	if !r.Logprobs() {
		t.Error("logprobs = false")
	}
	if s, ok := r.Seed(); !ok || s != 42 {
		t.Errorf("seed = %d %v", s, ok)
	}
	if v, ok := r.Temperature(); !ok || v != 0.5 {
		t.Errorf("temperature = %v %v", v, ok)
	}
	if v, ok := r.MaxTokens(); !ok || v != 20 {
		t.Errorf("max tokens = %d %v, want max_completion_tokens 20", v, ok)
	}
	if v, ok := r.Field("x_unknown"); !ok || string(v) != `{"keep": [1, 2]}` {
		t.Errorf("unknown field = %s %v", v, ok)
	}
}

func TestChatRequestAccessorDefaults(t *testing.T) {
	r, err := DecodeChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}],"stream":null,
		"n":1.5,"seed":"s","temperature":null,"max_tokens":7,"stream_options":"bad","logprobs":"yes"}`))
	if err != nil {
		t.Fatalf("DecodeChatRequest: %v", err)
	}
	if r.Stream() || r.IncludeUsage() {
		t.Error("stream null must mean false")
	}
	if _, ok := r.N(); ok {
		t.Error("non-integer n reported")
	}
	if _, ok := r.Seed(); ok {
		t.Error("string seed reported")
	}
	if _, ok := r.Temperature(); ok {
		t.Error("null temperature reported")
	}
	if v, ok := r.MaxTokens(); !ok || v != 7 {
		t.Errorf("max tokens = %d %v, want legacy max_tokens 7", v, ok)
	}
	if r.Logprobs() || r.Tools() != nil || r.ToolChoice() != nil {
		t.Error("absent or invalid fields reported")
	}
	if _, ok := r.ResponseFormat(); ok {
		t.Error("absent response_format reported")
	}
}

// TestEncodeGolden checks lossless forwarding: the encoded body is the
// client's body, members in their original order and values byte for byte,
// with only the model replaced and, for streams, include_usage merged in.
func TestEncodeGolden(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "non-stream keeps every field",
			body: `{"temperature": 0.25, "model": "gpt-x", "messages": [{"role":"user","content":"<b>hi</b> & é"}],
				"x_vendor": {"nested": [1, 2.50, "s"], "big": 12345678901234567890}, "stream": false, "stream_options": {"include_usage": false}}`,
			want: `{"temperature":0.25,"model":"upstream-x","messages":[{"role":"user","content":"<b>hi</b> & é"}],` +
				`"x_vendor":{"nested": [1, 2.50, "s"], "big": 12345678901234567890},"stream":false,"stream_options":{"include_usage": false}}`,
		},
		{
			name: "stream without stream_options",
			body: `{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}],"unknown":null}`,
			want: `{"model":"upstream-x","stream":true,"messages":[{"role":"user","content":"hi"}],"unknown":null,"stream_options":{"include_usage":true}}`,
		},
		{
			name: "stream merges stream_options",
			body: `{"model":"gpt-x","stream":true,"stream_options":{"include_obfuscation":false,"include_usage":false,"x":[1]},"messages":[{}]}`,
			want: `{"model":"upstream-x","stream":true,"stream_options":{"include_obfuscation":false,"include_usage":true,"x":[1]},"messages":[{}]}`,
		},
		{
			name: "stream replaces a non-object stream_options",
			body: `{"model":"gpt-x","stream":true,"stream_options":null,"messages":[{}]}`,
			want: `{"model":"upstream-x","stream":true,"stream_options":{"include_usage":true},"messages":[{}]}`,
		},
		{
			name: "duplicate keys keep the first position and the last value",
			body: `{"model":"a","messages":[{}],"model":"b","x":1}`,
			want: `{"model":"upstream-x","messages":[{}],"x":1}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := DecodeChatRequest([]byte(tt.body))
			if err != nil {
				t.Fatalf("DecodeChatRequest: %v", err)
			}
			got := r.Encode("upstream-x")
			if string(got) != tt.want {
				t.Errorf("Encode:\n got %s\nwant %s", got, tt.want)
			}
			if !json.Valid(got) {
				t.Error("encoded body is not valid JSON")
			}
			// Encode does not modify the request.
			if again := r.Encode("upstream-x"); string(again) != string(got) {
				t.Errorf("second Encode differs: %s", again)
			}
			if r.Model() == "upstream-x" {
				t.Error("Encode changed the request's model")
			}
		})
	}
}

func TestEncodeModelEscaping(t *testing.T) {
	r, err := DecodeChatRequest([]byte(`{"model":"m","messages":[{}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(r.Encode(`a<b>&"c"`)), `{"model":"a<b>&\"c\"","messages":[{}]}`; got != want {
		t.Errorf("Encode = %s, want %s", got, want)
	}
}

// TestEncodeSemanticRoundTrip checks, for a request with many shapes of
// value, that the encoded body decodes to the same value as the original
// apart from the model.
func TestEncodeSemanticRoundTrip(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:x"}}]}],
		"a":[],"b":{},"c":"😀","d":-0.0e+1,"e":true,"f":null,"g":"\\\"/"}`
	r, err := DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var want, got map[string]any
	if err := json.Unmarshal([]byte(body), &want); err != nil {
		t.Fatal(err)
	}
	want["model"] = "up"
	if err := json.Unmarshal(r.Encode("up"), &got); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("round trip (-want +got):\n%s", diff)
	}
}

func FuzzDecodeChatRequest(f *testing.F) {
	for _, s := range []string{
		`{"model":"m","messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","messages":[{}],"stream":true,"stream_options":{"a":1}}`,
		`{"model":"m","messages":[{}],"model":"n"}`,
		`[]`, `{`, `{"model":1}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := DecodeChatRequest(data)
		if err != nil {
			var re *RequestError
			if !errors.As(err, &re) {
				t.Fatalf("error %v is not a *RequestError", err)
			}
			return
		}
		out := r.Encode("up")
		var want, got map[string]any
		if err := json.Unmarshal(data, &want); err != nil {
			// Valid JSON that map[string]any cannot hold, such as 1e400,
			// is forwarded as is but cannot be compared this way.
			return
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("Encode produced invalid JSON %q: %v", out, err)
		}
		want["model"] = "up"
		if r.Stream() {
			so, _ := want["stream_options"].(map[string]any)
			if so == nil {
				so = map[string]any{}
			}
			so["include_usage"] = true
			want["stream_options"] = so
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("Encode changed more than model and stream_options (-want +got):\n%s", diff)
		}
		_ = r.RequiredCapabilities()
	})
}

func TestDecodeChatRequestModelAtLimit(t *testing.T) {
	model := strings.Repeat("m", MaxModelLength)
	r, err := DecodeChatRequest([]byte(`{"model":"` + model + `","messages":[{}]}`))
	if err != nil {
		t.Fatalf("a %d-byte model was rejected: %v", MaxModelLength, err)
	}
	if r.Model() != model {
		t.Errorf("model = %q", r.Model())
	}
}
