package schema

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRequiredCapabilities(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []Capability
	}{
		{name: "plain", body: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, want: []Capability{}},
		{name: "tools", body: `{"model":"m","messages":[{}],"tools":[{"type":"function"}]}`, want: []Capability{CapTools}},
		{name: "empty tools", body: `{"model":"m","messages":[{}],"tools":[]}`, want: []Capability{}},
		{name: "legacy functions", body: `{"model":"m","messages":[{}],"functions":[{"name":"f"}]}`, want: []Capability{CapTools}},
		{
			name: "vision",
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"https://x"}}]}]}`,
			want: []Capability{CapVision},
		},
		{name: "text parts only", body: `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}]}`, want: []Capability{}},
		{name: "json mode", body: `{"model":"m","messages":[{}],"response_format":{"type":"json_object"}}`, want: []Capability{CapJSONMode}},
		{name: "json schema", body: `{"model":"m","messages":[{}],"response_format":{"type":"json_schema","json_schema":{}}}`, want: []Capability{CapJSONSchema}},
		{name: "text format", body: `{"model":"m","messages":[{}],"response_format":{"type":"text"}}`, want: []Capability{}},
		{name: "n of 1", body: `{"model":"m","messages":[{}],"n":1}`, want: []Capability{}},
		{name: "n of 2", body: `{"model":"m","messages":[{}],"n":2}`, want: []Capability{CapN}},
		{name: "logprobs", body: `{"model":"m","messages":[{}],"logprobs":true}`, want: []Capability{CapLogprobs}},
		{name: "logprobs false", body: `{"model":"m","messages":[{}],"logprobs":false}`, want: []Capability{}},
		{name: "seed", body: `{"model":"m","messages":[{}],"seed":0}`, want: []Capability{CapSeed}},
		{name: "null seed", body: `{"model":"m","messages":[{}],"seed":null}`, want: []Capability{}},
		{name: "streaming", body: `{"model":"m","messages":[{}],"stream":true}`, want: []Capability{CapStreaming}},
		{name: "temperature 1", body: `{"model":"m","messages":[{}],"temperature":1}`, want: []Capability{CapSampling}},
		{name: "temperature 1.5", body: `{"model":"m","messages":[{}],"temperature":1.5}`, want: []Capability{CapSampling, CapTemperature2}},
		{name: "null temperature", body: `{"model":"m","messages":[{}],"temperature":null}`, want: []Capability{}},
		{name: "top_p", body: `{"model":"m","messages":[{}],"top_p":0.5}`, want: []Capability{CapSampling}},
		{name: "zero penalties", body: `{"model":"m","messages":[{}],"frequency_penalty":0,"presence_penalty":0}`, want: []Capability{}},
		{name: "frequency penalty", body: `{"model":"m","messages":[{}],"frequency_penalty":0.5}`, want: []Capability{CapPenalties}},
		{name: "presence penalty", body: `{"model":"m","messages":[{}],"presence_penalty":-1}`, want: []Capability{CapPenalties}},
		{name: "parallel tool calls true", body: `{"model":"m","messages":[{}],"parallel_tool_calls":true}`, want: []Capability{CapParallelTools}},
		{name: "parallel tool calls false", body: `{"model":"m","messages":[{}],"parallel_tool_calls":false}`, want: []Capability{}},
		{name: "tool_choice required", body: `{"model":"m","messages":[{}],"tool_choice":"required"}`, want: []Capability{CapForcedTools}},
		{name: "tool_choice function", body: `{"model":"m","messages":[{}],"tool_choice":{"type":"function","function":{"name":"f"}}}`, want: []Capability{CapForcedTools}},
		{name: "tool_choice auto", body: `{"model":"m","messages":[{}],"tool_choice":"auto"}`, want: []Capability{}},
		{name: "tool_choice none", body: `{"model":"m","messages":[{}],"tool_choice":"none"}`, want: []Capability{}},
		{name: "stream_options never required", body: `{"model":"m","messages":[{}],"stream_options":{"include_usage":true}}`, want: []Capability{}},
		// encoding/json matches struct fields case-insensitively; the
		// upstream matches keys exactly, and so must routing.
		{
			name: "image part with a case-folded type duplicate",
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","TYPE":"text","image_url":{"url":"https://x/y.png"}}]}]}`,
			want: []Capability{CapVision},
		},
		{
			name: "text part with a case-folded image type",
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"text","TYPE":"image_url","text":"x"}]}]}`,
			want: []Capability{},
		},
		{
			name: "image content behind a case-folded content duplicate",
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x"}}],"CONTENT":"hi"}]}`,
			want: []Capability{CapVision},
		},
		{
			name: "json_schema with a case-folded type duplicate",
			body: `{"model":"m","messages":[{}],"response_format":{"type":"json_schema","Type":"text","json_schema":{"name":"x","schema":{}}}}`,
			want: []Capability{CapJSONSchema},
		},
		{
			name: "json_object with a case-folded type duplicate",
			body: `{"model":"m","messages":[{}],"response_format":{"type":"json_object","TYPE":"text"}}`,
			want: []Capability{CapJSONMode},
		},
		{
			name: "case-folded response_format type only",
			body: `{"model":"m","messages":[{}],"response_format":{"TYPE":"json_object"}}`,
			want: []Capability{},
		},
		{
			name: "everything",
			body: `{"model":"m","stream":true,"n":4,"seed":1,"logprobs":true,"tools":[{}],"response_format":{"type":"json_object"},
				"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
			want: []Capability{CapJSONMode, CapLogprobs, CapN, CapSeed, CapStreaming, CapTools, CapVision},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := DecodeChatRequest([]byte(tt.body))
			if err != nil {
				t.Fatalf("DecodeChatRequest: %v", err)
			}
			caps := r.RequiredCapabilities()
			if diff := cmp.Diff(tt.want, caps.Sorted()); diff != "" {
				t.Errorf("capabilities (-want +got):\n%s", diff)
			}
			for _, c := range tt.want {
				if !caps.Has(c) {
					t.Errorf("Has(%s) = false", c)
				}
			}
		})
	}
}

func TestWithoutAndEncodeFor(t *testing.T) {
	r, err := DecodeChatRequest([]byte(`{"model":"m","n":2,"messages":[{}],"temperature":1.5,"stream":true,"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	w, removed := r.Without("temperature", "top_p", "n", "model", "messages", "stream", "n")
	if diff := cmp.Diff([]string{"temperature", "n"}, removed); diff != "" {
		t.Errorf("removed (-want +got):\n%s", diff)
	}
	if got := string(w.EncodeFor("up", false)); got != `{"model":"up","messages":[{}],"stream":true,"x":1}` {
		t.Errorf("encoded %s", got)
	}
	if got := string(w.Encode("up")); got != `{"model":"up","messages":[{}],"stream":true,"x":1,"stream_options":{"include_usage":true}}` {
		t.Errorf("encoded %s", got)
	}
	if _, ok := r.Field("temperature"); !ok {
		t.Error("Without changed the original")
	}
	if same, removed := r.Without("top_p"); same != r || removed != nil {
		t.Error("Without copied a request it did not change")
	}
}

func TestCompletionRequest(t *testing.T) {
	r, err := DecodeCompletionRequest([]byte(`{"model":"m","prompt":"hi","temperature":1.2,"n":3,"logprobs":null,"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]Capability{CapN, CapSampling, CapStreaming, CapTemperature2}, r.RequiredCapabilities().Sorted()); diff != "" {
		t.Errorf("capabilities (-want +got):\n%s", diff)
	}
	if e := r.ShimError(); e != nil {
		t.Errorf("ShimError = %v", e)
	}
	c, err := r.ChatRequest()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(c.EncodeFor("up", false)); got != `{"model":"up","messages":[{"role":"user","content":"hi"}],"temperature":1.2,"n":3,"stream":true}` {
		t.Errorf("chat %s", got)
	}
	if got := string(r.Encode("up", true)); got != `{"model":"up","prompt":"hi","temperature":1.2,"n":3,"logprobs":null,"stream":true,"stream_options":{"include_usage":true}}` {
		t.Errorf("native %s", got)
	}
	for _, tc := range []struct{ body, code, param string }{
		{`{"model":"m"}`, CodeMissingParameter, "prompt"},
		{`{"model":"m","prompt":[1,2]}`, CodeInvalidType, "prompt"},
		{`{"model":"m","prompt":[[1]]}`, CodeInvalidType, "prompt"},
		{`{"model":"m","prompt":[]}`, CodeEmptyArray, "prompt"},
		{`{"prompt":"x"}`, CodeMissingParameter, "model"},
		{`{"model":"m","prompt":"x","stream":"yes"}`, CodeInvalidType, "stream"},
	} {
		_, err := DecodeCompletionRequest([]byte(tc.body))
		var re *RequestError
		if !errors.As(err, &re) || re.Code != tc.code || re.Param != tc.param {
			t.Errorf("%s: %v, want %s on %s", tc.body, err, tc.code, tc.param)
		}
	}
	for body, param := range map[string]string{
		`{"model":"m","prompt":"x","echo":true}`:  "echo",
		`{"model":"m","prompt":"x","suffix":"s"}`: "suffix",
		`{"model":"m","prompt":"x","logprobs":0}`: "logprobs",
		`{"model":"m","prompt":"x","best_of":2}`:  "best_of",
		`{"model":"m","prompt":["x","y"]}`:        "prompt",
	} {
		r, err := DecodeCompletionRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if e := r.ShimError(); e == nil || e.Param != param || e.Code != CodeUnsupportedParameter {
			t.Errorf("%s: ShimError = %+v, want %s", body, e, param)
		}
	}
	for _, body := range []string{
		`{"model":"m","prompt":"x","echo":false,"suffix":"","best_of":1}`,
		`{"model":"m","prompt":["x"]}`,
	} {
		r, _ := DecodeCompletionRequest([]byte(body))
		if e := r.ShimError(); e != nil {
			t.Errorf("%s: ShimError = %v", body, e)
		}
	}
}

func TestEmbeddingRequest(t *testing.T) {
	r, err := DecodeEmbeddingRequest([]byte(`{"model":"m","input":["abcdefgh","abcdefghabcdefgh"],"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Inputs() != 2 || r.LargestInputTokens(1) != 4 || r.LargestInputTokens(2) != 8 {
		t.Errorf("inputs %d, largest %d", r.Inputs(), r.LargestInputTokens(1))
	}
	if got := string(r.Encode("up")); got != `{"model":"up","input":["abcdefgh","abcdefghabcdefgh"],"x":1}` {
		t.Errorf("encoded %s", got)
	}
	r, err = DecodeEmbeddingRequest([]byte(`{"model":"m","input":[[1,2,3],[4]]}`))
	if err != nil || r.LargestInputTokens(5) != 3 {
		t.Errorf("token arrays: %v", err)
	}
	r, err = DecodeEmbeddingRequest([]byte(`{"model":"m","input":[1,2],"encoding_format":"base64","dimensions":256}`))
	if err != nil || r.LargestInputTokens(1) != 2 {
		t.Errorf("one token array: %v", err)
	}
}

func TestImageParts(t *testing.T) {
	img := `{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAAAAAA"}}`
	tests := []struct {
		name      string
		body      string
		wantN     int
		wantBytes int
	}{
		{name: "text only", body: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
		{name: "one data URL", body: `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"x"},` + img + `]}]}`,
			wantN: 1, wantBytes: len("data:image/png;base64,AAAAAAAA")},
		{name: "https URL counts as an image, not as inline bytes", body: `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
			wantN: 1},
		{name: "across messages", body: `{"model":"m","messages":[{"role":"user","content":[` + img + `,` + img + `]},{"role":"user","content":[` + img + `]}]}`,
			wantN: 3, wantBytes: 3 * len("data:image/png;base64,AAAAAAAA")},
		{name: "malformed image part", body: `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":"data:x"}]}]}`, wantN: 1},
		{name: "case-folded type is not an image", body: `{"model":"m","messages":[{"role":"user","content":[{"TYPE":"image_url","image_url":{"url":"data:x"}}]}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := DecodeChatRequest([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			n, b := r.ImageParts()
			if n != tt.wantN || b != tt.wantBytes {
				t.Errorf("ImageParts = %d, %d; want %d, %d", n, b, tt.wantN, tt.wantBytes)
			}
		})
	}
}
