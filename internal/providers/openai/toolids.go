package openai

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"

	"switchyard/internal/schema"
)

// MaxToolCallIDLen is the longest tool-call id OpenAI accepts in a
// request: a longer assistant tool_calls[].id or tool_call_id gets a 400
// (code string_above_max_length). Other providers' ids can be longer: the
// gemini adapter's carry Gemini's thought signatures and run to
// kilobytes. Such a history reaching an OpenAI-wire deployment (through
// auto routing, substitution or fallback) would fail on every later turn.
const MaxToolCallIDLen = 40

// ShortToolCallID is the id a tool-call id longer than MaxToolCallIDLen is
// sent as: "call_" and the first 32 characters of the unpadded base64url
// SHA-256 of the id, 37 characters in all. It is deterministic, so an
// assistant tool call and the tool message answering it get the same id
// in every request of the conversation. Nothing maps it back: the client
// gets the upstream's own ids for new calls, and keeps the original ids in
// its history.
func ShortToolCallID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "call_" + base64.RawURLEncoding.EncodeToString(sum[:])[:32]
}

// withShortToolCallIDs returns c with every tool-call id longer than
// MaxToolCallIDLen replaced by ShortToolCallID, in assistant tool_calls
// and in tool messages. A request without such an id is returned as is,
// so it is still forwarded byte for byte; in one with such ids, only the
// messages that hold them are re-encoded (their members in key order).
func withShortToolCallIDs(c *schema.ChatRequest) *schema.ChatRequest {
	raw, ok := c.Field("messages")
	if !ok || !bytes.Contains(raw, []byte("tool_call")) {
		return c // no tool_calls or tool_call_id member: nothing to shorten
	}
	var msgs []json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return c
	}
	changed := false
	for i, m := range msgs {
		if out, ok := shortenMessage(m); ok {
			msgs[i], changed = out, true
		}
	}
	if !changed {
		return c
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, m := range msgs {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(m)
	}
	buf.WriteByte(']')
	return c.WithMessages(buf.Bytes())
}

// shortenMessage shortens the long tool-call ids of one message. It
// reports false, with nil, when the message has none.
func shortenMessage(raw json.RawMessage) (json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	changed := false
	if id, ok := longID(m["tool_call_id"]); ok {
		m["tool_call_id"], changed = encodeJSON(ShortToolCallID(id)), true
	}
	var calls []map[string]json.RawMessage
	if tc, ok := m["tool_calls"]; ok && json.Unmarshal(tc, &calls) == nil {
		callsChanged := false
		for _, call := range calls {
			if id, ok := longID(call["id"]); ok {
				call["id"], callsChanged = encodeJSON(ShortToolCallID(id)), true
			}
		}
		if callsChanged {
			m["tool_calls"], changed = encodeJSON(calls), true
		}
	}
	if !changed {
		return nil, false
	}
	return encodeJSON(m), true
}

// longID returns the id a raw value holds when it is a string longer than
// MaxToolCallIDLen.
func longID(raw json.RawMessage) (string, bool) {
	var id string
	if len(raw) == 0 || json.Unmarshal(raw, &id) != nil || len(id) <= MaxToolCallIDLen {
		return "", false
	}
	return id, true
}

// encodeJSON encodes v without HTML escaping, so that the raw values it
// holds keep their spelling. Only strings, raw JSON and maps of them are
// encoded, which cannot fail.
func encodeJSON(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
}
