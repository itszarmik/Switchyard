package schema

import (
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
