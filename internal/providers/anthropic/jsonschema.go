package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// supportedFormats are the string formats structured outputs accept.
var supportedFormats = map[string]bool{
	"date-time": true, "time": true, "date": true, "duration": true, "email": true,
	"hostname": true, "uri": true, "ipv4": true, "ipv6": true, "uuid": true,
}

// schemaMember is one member of a JSON object, in document order.
type schemaMember struct {
	key string
	val json.RawMessage
}

// Structured outputs accept a subset of JSON Schema and reject a schema
// that uses anything else with a 400. The subset, from Anthropic's
// structured-outputs documentation ("JSON Schema limitations"):
//
//   - the types object, array, string, integer, number, boolean and null;
//   - enum of primitive values, const, anyOf, allOf, $ref, $defs and
//     definitions, default, title and description;
//   - required, and additionalProperties, which must be false;
//   - the string formats in supportedFormats, and pattern;
//   - minItems of 0 or 1.
//
// Numeric constraints (minimum, maximum, multipleOf…), string lengths,
// other array constraints and recursive schemas are not supported.
//
// structuredSchema reduces a client's schema to that subset the way
// Anthropic's SDK helpers do (transform_schema in the Python SDK): every
// keyword outside it is removed from its node and appended to the node's
// description as "{keyword: value, …}", so the model still sees it, and
// every object gets additionalProperties: false. oneOf becomes anyOf, and
// a node with $ref keeps only $ref and its definitions. Unlike the SDK
// helper, it keeps default and pattern, which the documentation lists as
// supported, keeps type beside enum, const and anyOf, applies the rules of
// each type in a type list such as ["string","null"], and never fails:
// what it cannot read is passed on for the upstream to judge. Members keep
// their order.
//
// A value that is not an object (such as the schema true) is returned as
// it is.
func structuredSchema(raw json.RawMessage) json.RawMessage {
	in, ok := parseMembers(raw)
	if !ok {
		return raw
	}
	get := func(key string) (json.RawMessage, bool) {
		for _, m := range in {
			if m.key == key {
				return m.val, true
			}
		}
		return nil, false
	}

	var out, extra []schemaMember
	keep := func(m schemaMember) { out = append(out, m) }

	if _, ok := get("$ref"); ok {
		for _, m := range in {
			switch m.key {
			case "$ref":
				keep(m)
			case "$defs", "definitions":
				keep(schemaMember{m.key, schemaMap(m.val)})
			}
		}
		return encodeMembers(out)
	}

	types := typeNames(get("type"))
	_, hasProperties := get("properties")
	isObject := types["object"] || (len(types) == 0 && hasProperties)
	_, hasAnyOf := get("anyOf")
	setAdditional := false

	for _, m := range in {
		switch {
		case m.key == "type", m.key == "title", m.key == "default":
			keep(m)
		case m.key == "description" && isString(m.val):
			keep(m)
		case m.key == "$defs", m.key == "definitions":
			keep(schemaMember{m.key, schemaMap(m.val)})
		case m.key == "enum" && primitiveArray(m.val), m.key == "const" && isPrimitive(m.val):
			keep(m)
		case (m.key == "anyOf" || m.key == "allOf") && isArray(m.val):
			keep(schemaMember{m.key, schemaList(m.val)})
		case m.key == "oneOf" && !hasAnyOf && isArray(m.val):
			keep(schemaMember{"anyOf", schemaList(m.val)})
		case isObject && m.key == "properties":
			keep(schemaMember{m.key, schemaMap(m.val)})
		case isObject && m.key == "required":
			keep(m)
		case isObject && m.key == "additionalProperties":
			if !bytes.Equal(bytes.TrimSpace(m.val), []byte("false")) {
				extra = append(extra, m)
			}
			keep(schemaMember{m.key, json.RawMessage("false")})
			setAdditional = true
		case types["string"] && m.key == "format" && supportedFormats[stringValue(m.val)]:
			keep(m)
		case types["string"] && m.key == "pattern":
			keep(m)
		case types["array"] && m.key == "items" && isObjectValue(m.val):
			keep(schemaMember{m.key, structuredSchema(m.val)})
		case types["array"] && m.key == "minItems" && (isLiteral(m.val, "0") || isLiteral(m.val, "1")):
			keep(m)
		default:
			extra = append(extra, m)
		}
	}
	if isObject && !setAdditional {
		keep(schemaMember{"additionalProperties", json.RawMessage("false")})
	}
	if len(extra) > 0 {
		out = withNote(out, extra)
	}
	return encodeMembers(out)
}

// withNote appends the removed keywords to the description, creating it
// if needed: "{minimum: 0, maxLength: 10}" after a blank line.
func withNote(out, extra []schemaMember) []schemaMember {
	var note strings.Builder
	note.WriteByte('{')
	for i, m := range extra {
		if i > 0 {
			note.WriteString(", ")
		}
		note.WriteString(m.key)
		note.WriteString(": ")
		var compact bytes.Buffer
		if json.Compact(&compact, m.val) == nil {
			note.Write(compact.Bytes())
		} else {
			note.Write(m.val)
		}
	}
	note.WriteByte('}')
	for i, m := range out {
		if m.key == "description" {
			var desc string
			_ = json.Unmarshal(m.val, &desc) // a string: checked when it was kept
			out[i].val = encodeJSONString(desc + "\n\n" + note.String())
			return out
		}
	}
	return append(out, schemaMember{"description", encodeJSONString(note.String())})
}

// schemaMap reduces each member of an object of schemas ($defs,
// properties). Any other value is returned as it is.
func schemaMap(raw json.RawMessage) json.RawMessage {
	in, ok := parseMembers(raw)
	if !ok {
		return raw
	}
	for i := range in {
		in[i].val = structuredSchema(in[i].val)
	}
	return encodeMembers(in)
}

// schemaList reduces each schema of an array (anyOf, allOf).
func schemaList(raw json.RawMessage) json.RawMessage {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return raw
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(structuredSchema(item))
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

// typeNames returns the names of a type value: a string or an array of
// strings.
func typeNames(raw json.RawMessage, ok bool) map[string]bool {
	names := map[string]bool{}
	if !ok {
		return names
	}
	var one string
	var many []string
	switch {
	case json.Unmarshal(raw, &one) == nil:
		names[one] = true
	case json.Unmarshal(raw, &many) == nil:
		for _, n := range many {
			names[n] = true
		}
	}
	return names
}

// parseMembers decodes a JSON object into its members in document order. A
// key that appears more than once keeps its first position and its last
// value. It reports false if raw is not one JSON object.
func parseMembers(raw json.RawMessage) ([]schemaMember, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var out []schemaMember
	at := map[string]int{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, _ := tok.(string) // the decoder only yields strings in key position
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		if i, dup := at[key]; dup {
			out[i].val = v
			continue
		}
		at[key] = len(out)
		out = append(out, schemaMember{key, v})
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return out, true
}

// encodeMembers encodes members as a JSON object, in order.
func encodeMembers(ms []schemaMember) json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(encodeJSONString(m.key))
		buf.WriteByte(':')
		buf.Write(m.val)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// encodeJSONString encodes s as a JSON string without escaping <, > and &.
func encodeJSONString(s string) json.RawMessage {
	b, _ := marshal(s) // a string cannot fail to encode
	return b
}

// firstByte returns the first non-space byte of raw, or 0.
func firstByte(raw json.RawMessage) byte {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return 0
	}
	return b[0]
}

func isString(raw json.RawMessage) bool      { return firstByte(raw) == '"' }
func isArray(raw json.RawMessage) bool       { return firstByte(raw) == '[' }
func isObjectValue(raw json.RawMessage) bool { return firstByte(raw) == '{' }

// isPrimitive reports whether raw is a string, number, boolean or null.
func isPrimitive(raw json.RawMessage) bool {
	switch firstByte(raw) {
	case '{', '[', 0:
		return false
	}
	return true
}

// primitiveArray reports whether raw is a non-empty array of primitives.
func primitiveArray(raw json.RawMessage) bool {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return false
	}
	for _, it := range items {
		if !isPrimitive(it) {
			return false
		}
	}
	return true
}

// stringValue returns raw decoded as a string, or "".
func stringValue(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// isLiteral reports whether raw is the JSON literal lit, ignoring
// surrounding space.
func isLiteral(raw json.RawMessage, lit string) bool {
	return string(bytes.TrimSpace(raw)) == lit
}
