package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

// object is a JSON object that keeps every member's raw bytes and the order
// in which the keys first appeared, so it can be re-encoded losslessly.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

// errNotObject reports a JSON value that is valid but not an object.
var errNotObject = errors.New("not a JSON object")

// parseObject parses data as exactly one JSON object. A key that appears
// more than once keeps its first position and its last value, matching
// encoding/json's own last-one-wins rule.
func parseObject(data []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errNotObject
	}
	o := &object{vals: make(map[string]json.RawMessage)}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			// The decoder only yields strings in key position.
			return nil, fmt.Errorf("unexpected object key %v", tok)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.set(key, v)
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("unexpected data after the top-level object")
		}
		return nil, err
	}
	return o, nil
}

// get returns the raw value of key, and whether the key is present.
func (o *object) get(key string) (json.RawMessage, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// set replaces the value of key, or appends key if it is new.
func (o *object) set(key string, v json.RawMessage) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// delete removes key, if present.
func (o *object) delete(key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	o.keys = slices.DeleteFunc(o.keys, func(k string) bool { return k == key })
}

// clone returns a copy that can be modified without affecting o. The raw
// values themselves are shared; they are never modified in place.
func (o *object) clone() *object {
	c := &object{keys: append([]string(nil), o.keys...), vals: make(map[string]json.RawMessage, len(o.vals))}
	for k, v := range o.vals {
		c.vals[k] = v
	}
	return c
}

// encode returns the object as JSON: keys in their original order, values
// byte for byte as they were parsed or set.
func (o *object) encode() []byte {
	size := 2
	for _, k := range o.keys {
		size += len(k) + len(o.vals[k]) + 4
	}
	buf := bytes.NewBuffer(make([]byte, 0, size))
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(encodeString(k))
		buf.WriteByte(':')
		buf.Write(o.vals[k])
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// encodeString returns s as a JSON string. Unlike json.Marshal it does not
// escape <, > and &, so ordinary text keeps its original spelling.
func encodeString(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoding a string cannot fail.
	_ = enc.Encode(s)
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
}

// jsonKind names the JSON type of a raw value the way OpenAI's validation
// messages do: object, array, string, boolean, null or number.
func jsonKind(raw json.RawMessage) string {
	b := bytes.TrimLeft(raw, " \t\r\n")
	if len(b) == 0 {
		return "nothing"
	}
	switch b[0] {
	case '{':
		return "an object"
	case '[':
		return "an array"
	case '"':
		return "a string"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	default:
		return "a number"
	}
}

// isNull reports whether raw is the JSON literal null.
func isNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// members decodes a JSON object into its members, matching keys exactly;
// a key that appears more than once keeps its last value, as in
// parseObject. It reports false if raw is not an object.
//
// Nested objects that routing reads (message content, content part types,
// response_format, stream_options) are decoded with members, never into a
// struct: encoding/json matches struct fields case-insensitively, so
// {"type":"image_url","TYPE":"text"} would read as text while the upstream,
// matching exactly, sees an image.
func members(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

// member decodes the member key of the object m into a T, as decodeField
// does.
func member[T any](m map[string]json.RawMessage, key string) (T, bool) {
	raw, ok := m[key]
	return decodeField[T](raw, ok)
}

// decodeField decodes a present, non-null raw value into a T. It reports
// false when the value is absent, null or of another type.
func decodeField[T any](raw json.RawMessage, present bool) (T, bool) {
	if !present || isNull(raw) {
		// v is declared below: its address escapes into json.Unmarshal,
		// so declaring it first would heap-allocate it on every call,
		// including for the absent fields the hot path checks.
		var zero T
		return zero, false
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		var zero T
		return zero, false
	}
	return v, true
}
