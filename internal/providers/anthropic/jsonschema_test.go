package anthropic

import (
	"encoding/json"
	"testing"
)

// TestStructuredSchema is the golden test of the reduction of a client's
// json_schema to the structured-outputs subset. Outputs are compared byte
// for byte: member order is kept.
func TestStructuredSchema(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			name: "supported schema gains additionalProperties false",
			in:   `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
			want: `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}`,
		},
		{
			name: "additionalProperties false stays in place",
			in:   `{"type":"object","additionalProperties":false,"properties":{}}`,
			want: `{"type":"object","additionalProperties":false,"properties":{}}`,
		},
		{
			name: "additionalProperties true becomes false, noted",
			in:   `{"type":"object","additionalProperties":true}`,
			want: `{"type":"object","additionalProperties":false,"description":"{additionalProperties: true}"}`,
		},
		{
			name: "a map schema becomes false, noted",
			in:   `{"type":"object","description":"Scores.","additionalProperties":{"type":"number"}}`,
			want: `{"type":"object","description":"Scores.\n\n{additionalProperties: {\"type\":\"number\"}}","additionalProperties":false}`,
		},
		{
			name: "numeric constraints move to the description",
			in:   `{"type":"integer","description":"A number","minimum":1,"maximum":10}`,
			want: `{"type":"integer","description":"A number\n\n{minimum: 1, maximum: 10}"}`,
		},
		{
			name: "exclusive bounds and multipleOf",
			in:   `{"type":"number","exclusiveMinimum":0,"multipleOf":0.5}`,
			want: `{"type":"number","description":"{exclusiveMinimum: 0, multipleOf: 0.5}"}`,
		},
		{
			name: "string lengths are removed, pattern and a supported format kept",
			in:   `{"type":"string","minLength":1,"maxLength":10,"pattern":"^[a-z]+$","format":"email"}`,
			want: `{"type":"string","pattern":"^[a-z]+$","format":"email","description":"{minLength: 1, maxLength: 10}"}`,
		},
		{
			name: "an unsupported format moves to the description",
			in:   `{"type":"string","format":"uri-reference"}`,
			want: `{"type":"string","description":"{format: \"uri-reference\"}"}`,
		},
		{
			name: "minItems 0 and 1 are kept",
			in:   `{"type":"array","items":{"type":"string"},"minItems":1}`,
			want: `{"type":"array","items":{"type":"string"},"minItems":1}`,
		},
		{
			name: "other array constraints move to the description",
			in:   `{"type":"array","items":{"type":"string","maxLength":3},"minItems":2,"maxItems":5,"uniqueItems":true}`,
			want: `{"type":"array","items":{"type":"string","description":"{maxLength: 3}"},"description":"{minItems: 2, maxItems: 5, uniqueItems: true}"}`,
		},
		{
			name: "the reviewer's schema, nested",
			in: `{"type":"object","properties":{"age":{"type":"integer","minimum":0},
				"tags":{"type":"array","maxItems":3,"items":{"type":"string","maxLength":10}}}}`,
			want: `{"type":"object","properties":{"age":{"type":"integer","description":"{minimum: 0}"},` +
				`"tags":{"type":"array","items":{"type":"string","description":"{maxLength: 10}"},"description":"{maxItems: 3}"}},"additionalProperties":false}`,
		},
		{
			name: "enum, const, title and default are kept",
			in:   `{"type":"string","title":"Unit","enum":["c","f"],"default":"c"}`,
			want: `{"type":"string","title":"Unit","enum":["c","f"],"default":"c"}`,
		},
		{
			name: "an enum of objects is not supported",
			in:   `{"enum":[{"a":1}]}`,
			want: `{"description":"{enum: [{\"a\":1}]}"}`,
		},
		{
			name: "anyOf and allOf are reduced branch by branch, oneOf becomes anyOf",
			in:   `{"oneOf":[{"type":"object","properties":{}},{"type":"null"}],"allOf":[{"type":"integer","maximum":3}]}`,
			want: `{"anyOf":[{"type":"object","properties":{},"additionalProperties":false},{"type":"null"}],"allOf":[{"type":"integer","description":"{maximum: 3}"}]}`,
		},
		{
			name: "oneOf beside anyOf is not supported",
			in:   `{"anyOf":[{"type":"string"}],"oneOf":[{"type":"null"}]}`,
			want: `{"anyOf":[{"type":"string"}],"description":"{oneOf: [{\"type\":\"null\"}]}"}`,
		},
		{
			name: "a nullable type list keeps the rules of each type",
			in:   `{"type":["object","null"],"properties":{"s":{"type":["string","null"],"maxLength":2,"format":"date"}}}`,
			want: `{"type":["object","null"],"properties":{"s":{"type":["string","null"],"format":"date","description":"{maxLength: 2}"}},"additionalProperties":false}`,
		},
		{
			name: "properties without a type are an object",
			in:   `{"properties":{"a":{"type":"boolean"}}}`,
			want: `{"properties":{"a":{"type":"boolean"}},"additionalProperties":false}`,
		},
		{
			name: "$defs are reduced and a $ref node keeps only the reference",
			in: `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"p":{"$ref":"#/$defs/P","description":"x"}},` +
				`"$defs":{"P":{"type":"object","properties":{"n":{"type":"integer","minimum":1}}}}}`,
			want: `{"type":"object","properties":{"p":{"$ref":"#/$defs/P"}},` +
				`"$defs":{"P":{"type":"object","properties":{"n":{"type":"integer","description":"{minimum: 1}"}},"additionalProperties":false}},` +
				`"additionalProperties":false,"description":"{$schema: \"https://json-schema.org/draft/2020-12/schema\"}"}`,
		},
		{
			name: "a root $ref keeps its definitions",
			in:   `{"$ref":"#/definitions/A","definitions":{"A":{"type":"object"}},"title":"dropped"}`,
			want: `{"$ref":"#/definitions/A","definitions":{"A":{"type":"object","additionalProperties":false}}}`,
		},
		{
			name: "text keeps <, > and & unescaped",
			in:   `{"type":"integer","description":"a <b> & c","minimum":0}`,
			want: `{"type":"integer","description":"a <b> & c\n\n{minimum: 0}"}`,
		},
		{
			name: "duplicate keys keep their first position and last value",
			in:   `{"type":"string","type":"integer","maximum":1}`,
			want: `{"type":"integer","description":"{maximum: 1}"}`,
		},
		{name: "a boolean schema is passed on", in: `true`, want: `true`},
		{name: "a non-object is passed on", in: `[1]`, want: `[1]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := structuredSchema(json.RawMessage(tt.in))
			if string(got) != tt.want {
				t.Errorf("structuredSchema:\n got %s\nwant %s", got, tt.want)
			}
			if !json.Valid(got) {
				t.Errorf("output is not valid JSON: %s", got)
			}
		})
	}
}
