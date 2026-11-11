package fakeanthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"switchyard/test/fakeupstream"
)

// topLevelFields are the request fields the Messages API accepts (API
// version 2023-06-01). Anything else is rejected, as the API does.
var topLevelFields = map[string]bool{
	"model": true, "max_tokens": true, "messages": true, "system": true, "metadata": true,
	"stop_sequences": true, "stream": true, "temperature": true, "top_p": true, "top_k": true,
	"tools": true, "tool_choice": true, "thinking": true, "output_config": true,
	"service_tier": true, "container": true, "inference_geo": true, "speed": true,
	"cache_control": true, "diagnostics": true,
}

// apiError is a request the fake rejects.
type apiError struct {
	status       int
	typ, message string
}

func invalid(format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, typ: typeInvalidRequest, message: fmt.Sprintf(format, args...)}
}

// messagesRequest is what the fake reads from a request.
type messagesRequest struct {
	model           string
	stream          bool
	stops           []string
	tools           []string // tool names, in order
	choice          string   // "", auto, any, tool or none
	forced          string   // with choice tool
	disableParallel bool
	nativeJSON      bool // output_config.format is set
	systemJSON      bool // the system prompt asks for JSON
	promptBytes     int
}

// requestBlock is a request content block, decoded for validation.
type requestBlock struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
}

// parseRequest decodes and validates body the way the Messages API does for
// the fields the fake knows.
func parseRequest(body []byte) (messagesRequest, *apiError) {
	var req messagesRequest
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return req, invalid("The request body is not valid JSON.")
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !topLevelFields[k] {
			return req, invalid("%s: Extra inputs are not permitted", k)
		}
	}
	if json.Unmarshal(raw["model"], &req.model) != nil || req.model == "" {
		return req, invalid("model: Field required")
	}
	var maxTokens *int64
	if json.Unmarshal(raw["max_tokens"], &maxTokens) != nil || maxTokens == nil {
		return req, invalid("max_tokens: Field required")
	}
	if *maxTokens < 0 {
		return req, invalid("max_tokens: Input should be greater than or equal to 0")
	}
	if v, ok := raw["stream"]; ok && json.Unmarshal(v, &req.stream) != nil {
		return req, invalid("stream: Input should be a valid boolean")
	}
	if v, ok := raw["temperature"]; ok {
		var t float64
		if json.Unmarshal(v, &t) != nil || t < 0 || t > 1 {
			return req, invalid("temperature: Input should be less than or equal to 1")
		}
	}
	if v, ok := raw["stop_sequences"]; ok && json.Unmarshal(v, &req.stops) != nil {
		return req, invalid("stop_sequences: Input should be a valid list")
	}

	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw["messages"], &messages) != nil {
		return req, invalid("messages: Field required")
	}
	if len(messages) == 0 {
		return req, invalid("messages: at least one message is required")
	}
	req.promptBytes = len(raw["messages"])
	for i, m := range messages {
		if m.Role != "user" && m.Role != "assistant" {
			return req, invalid("messages.%d.role: Input should be 'user' or 'assistant'", i)
		}
		if aerr := validateContent(m.Content, fmt.Sprintf("messages.%d.content", i)); aerr != nil {
			return req, aerr
		}
	}

	if v, ok := raw["system"]; ok {
		var s string
		var blocks []requestBlock
		switch {
		case json.Unmarshal(v, &s) == nil:
		case json.Unmarshal(v, &blocks) == nil:
			var parts []string
			for i, b := range blocks {
				if b.Type != "text" || b.Text == nil || *b.Text == "" {
					return req, invalid("system.%d: text content blocks must be non-empty", i)
				}
				parts = append(parts, *b.Text)
			}
			s = strings.Join(parts, "\n")
		default:
			return req, invalid("system: Input should be a valid string or list")
		}
		req.systemJSON = strings.Contains(s, "JSON")
	}

	if v, ok := raw["tools"]; ok {
		var tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if json.Unmarshal(v, &tools) != nil {
			return req, invalid("tools: Input should be a valid list")
		}
		for i, t := range tools {
			var schema struct {
				Type string `json:"type"`
			}
			if t.Name == "" || json.Unmarshal(t.InputSchema, &schema) != nil || schema.Type != "object" {
				return req, invalid("tools.%d: name and an input_schema of type object are required", i)
			}
			req.tools = append(req.tools, t.Name)
		}
	}
	if v, ok := raw["tool_choice"]; ok {
		var tc struct {
			Type                   string `json:"type"`
			Name                   string `json:"name"`
			DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
		}
		if json.Unmarshal(v, &tc) != nil {
			return req, invalid("tool_choice: Input should be an object")
		}
		switch tc.Type {
		case "auto", "any", "none":
		case "tool":
			if !slices.Contains(req.tools, tc.Name) {
				return req, invalid("tool_choice.name: tool %q is not defined in tools", tc.Name)
			}
			req.forced = tc.Name
		default:
			return req, invalid("tool_choice.type: Input should be 'auto', 'any', 'tool' or 'none'")
		}
		req.choice, req.disableParallel = tc.Type, tc.DisableParallelToolUse
	}
	if v, ok := raw["output_config"]; ok {
		var oc struct {
			Format *struct {
				Type   string          `json:"type"`
				Schema json.RawMessage `json:"schema"`
			} `json:"format"`
		}
		if json.Unmarshal(v, &oc) != nil {
			return req, invalid("output_config: Input should be an object")
		}
		if oc.Format != nil {
			if oc.Format.Type != "json_schema" || len(oc.Format.Schema) == 0 {
				return req, invalid("output_config.format: a json_schema format with a schema is required")
			}
			if msg := unsupportedSchema(oc.Format.Schema, "output_config.format.schema"); msg != "" {
				return req, invalid("%s", msg)
			}
			req.nativeJSON = true
		}
	}
	return req, nil
}

// unsupportedKeywords are JSON Schema keywords that structured outputs
// reject (Anthropic's "JSON Schema limitations").
var unsupportedKeywords = map[string]bool{
	"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true, "multipleOf": true,
	"minLength": true, "maxLength": true, "maxItems": true, "uniqueItems": true, "contains": true,
	"minProperties": true, "maxProperties": true,
}

// unsupportedSchema returns why a structured-output schema would be
// rejected, or "": an unsupported keyword, minItems other than 0 or 1, or
// an object whose additionalProperties is not false.
func unsupportedSchema(raw json.RawMessage, where string) string {
	var node map[string]json.RawMessage
	if json.Unmarshal(raw, &node) != nil {
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) == nil {
			for i, item := range list {
				if msg := unsupportedSchema(item, fmt.Sprintf("%s[%d]", where, i)); msg != "" {
					return msg
				}
			}
		}
		return ""
	}
	for _, k := range slices.Sorted(maps.Keys(node)) {
		v := node[k]
		switch {
		case unsupportedKeywords[k]:
			return fmt.Sprintf("%s: the keyword %s is not supported", where, k)
		case k == "minItems" && string(v) != "0" && string(v) != "1":
			return fmt.Sprintf("%s: minItems must be 0 or 1", where)
		case k == "properties" || k == "$defs" || k == "definitions":
			var m map[string]json.RawMessage
			_ = json.Unmarshal(v, &m)
			for _, name := range slices.Sorted(maps.Keys(m)) {
				if msg := unsupportedSchema(m[name], where+"."+k+"."+name); msg != "" {
					return msg
				}
			}
		case k == "items" || k == "anyOf" || k == "allOf":
			if msg := unsupportedSchema(v, where+"."+k); msg != "" {
				return msg
			}
		}
	}
	var typ string
	_ = json.Unmarshal(node["type"], &typ)
	if typ == "object" && string(bytes.TrimSpace(node["additionalProperties"])) != "false" {
		return where + ": additionalProperties must be set to false for objects"
	}
	return ""
}

// validateContent checks a message's content: a non-empty string or a
// non-empty list of well-formed blocks.
func validateContent(raw json.RawMessage, where string) *apiError {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return invalid("%s: text content must be non-empty", where)
		}
		return nil
	}
	var blocks []requestBlock
	if json.Unmarshal(raw, &blocks) != nil || len(blocks) == 0 {
		return invalid("%s: Input should be a non-empty string or list", where)
	}
	for i, b := range blocks {
		at := fmt.Sprintf("%s.%d", where, i)
		switch b.Type {
		case "text":
			if b.Text == nil || *b.Text == "" {
				return invalid("%s: text content blocks must be non-empty", at)
			}
		case "image":
			if b.Source == nil || (b.Source.Type == "base64" && (b.Source.Data == "" || b.Source.MediaType == "")) ||
				(b.Source.Type == "url" && b.Source.URL == "") || (b.Source.Type != "base64" && b.Source.Type != "url") {
				return invalid("%s.source: a base64 or url image source is required", at)
			}
		case "tool_use":
			if b.ID == "" || b.Name == "" || len(b.Input) == 0 || bytes.TrimSpace(b.Input)[0] != '{' {
				return invalid("%s.tool_use: id, name and an input object are required", at)
			}
		case "tool_result":
			if b.ToolUseID == "" {
				return invalid("%s.tool_result.tool_use_id: Field required", at)
			}
		case "thinking", "redacted_thinking", "document":
		default:
			return invalid("%s.type: unknown content block type", at)
		}
	}
	return nil
}

// plan is the canned answer to one request.
type plan struct {
	id           string
	model        string
	blocks       []blockPlan
	stopReason   string
	stopSequence *string
	usage        Usage
}

type blockPlan struct {
	toolUse bool
	text    string   // a text block's text
	id      string   // a tool_use block's id
	name    string   // and name
	input   string   // and input, as JSON
	pieces  []string // the text or input split into stream deltas
}

// newPlan builds the answer to req. seq makes the message ID unique.
func newPlan(req messagesRequest, seq string, b Behavior) *plan {
	p := &plan{id: "msg_fake" + seq, model: req.model, stopReason: "end_turn"}
	var calls []string
	switch {
	case req.forced != "":
		calls = []string{req.forced}
	case len(req.tools) == 0 || req.choice == "none":
	case req.disableParallel:
		calls = req.tools[:1]
	default:
		calls = req.tools
	}

	output := 0
	switch {
	case len(calls) > 0:
		p.stopReason = "tool_use"
		for i, name := range calls {
			bp := blockPlan{toolUse: true, id: fakeupstream.ToolCallID(0, i), name: name, input: fakeupstream.ToolArguments(i)}
			bp.pieces = splitBytes(bp.input, 6)
			p.blocks = append(p.blocks, bp)
			output += len(bp.pieces) + 1
		}
	case req.nativeJSON || req.systemJSON:
		p.blocks = []blockPlan{{text: fakeupstream.JSONText, pieces: splitBytes(fakeupstream.JSONText, 8)}}
		output += len(p.blocks[0].pieces)
	default:
		text, stop := applyStop(fakeupstream.Text, req.stops)
		if stop != "" {
			p.stopReason, p.stopSequence = "stop_sequence", &stop
		}
		p.blocks = []blockPlan{{text: text, pieces: splitWords(text)}}
		output += len(p.blocks[0].pieces)
	}
	if b.StopReason != "" {
		p.stopReason = b.StopReason
	}
	p.usage = Usage{InputTokens: max(1, req.promptBytes/4), OutputTokens: max(1, output)}
	if b.Usage != nil {
		p.usage = *b.Usage
	}
	return p
}

// applyStop cuts text at the earliest occurrence of any stop sequence and
// returns the sequence that matched, or "".
func applyStop(text string, stops []string) (string, string) {
	cut, which := len(text), ""
	for _, s := range stops {
		if i := strings.Index(text, s); s != "" && i >= 0 && i < cut {
			cut, which = i, s
		}
	}
	return text[:cut], which
}

// splitWords splits s after each space, so the pieces join back to s.
func splitWords(s string) []string {
	return slices.DeleteFunc(strings.SplitAfter(s, " "), func(p string) bool { return p == "" })
}

// splitBytes splits s into pieces of about n bytes, on rune boundaries.
func splitBytes(s string, n int) []string {
	var out []string
	for len(s) > 0 {
		end := min(n, len(s))
		for end < len(s) && !utf8.RuneStart(s[end]) {
			end++
		}
		out = append(out, s[:end])
		s = s[end:]
	}
	return out
}

// Wire types, in the API's field order.

type wireUsage struct {
	InputTokens              int  `json:"input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	OutputTokens             int  `json:"output_tokens"`
}

type wireBlock struct {
	Type  string          `json:"type"`
	Text  *string         `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type wireMessage struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Role         string      `json:"role"`
	Model        string      `json:"model"`
	Content      []wireBlock `json:"content"`
	StopReason   *string     `json:"stop_reason"`
	StopSequence *string     `json:"stop_sequence"`
	Usage        wireUsage   `json:"usage"`
}

func (p *plan) wireUsage(output int) wireUsage {
	u := wireUsage{InputTokens: p.usage.InputTokens, OutputTokens: output}
	cw, cr := p.usage.CacheCreationInputTokens, p.usage.CacheReadInputTokens
	u.CacheCreationInputTokens, u.CacheReadInputTokens = &cw, &cr
	return u
}

func (bp blockPlan) wire(full bool) wireBlock {
	if bp.toolUse {
		input := json.RawMessage(`{}`)
		if full {
			input = json.RawMessage(bp.input)
		}
		return wireBlock{Type: "tool_use", ID: bp.id, Name: bp.name, Input: input}
	}
	text := ""
	if full {
		text = bp.text
	}
	return wireBlock{Type: "text", Text: &text}
}

// message encodes p as a Message.
func (p *plan) message(extra map[string]json.RawMessage) []byte {
	m := wireMessage{
		ID: p.id, Type: "message", Role: "assistant", Model: p.model, Content: []wireBlock{},
		StopReason: &p.stopReason, StopSequence: p.stopSequence, Usage: p.wireUsage(p.usage.OutputTokens),
	}
	for _, bp := range p.blocks {
		m.Content = append(m.Content, bp.wire(true))
	}
	return withExtra(mustMarshal(m), extra)
}

// event is one named server-sent event.
type event struct {
	name string
	data []byte
}

// events encodes p as the documented event sequence: message_start, then
// each content block's start, deltas and stop (with a ping after the first
// start), then message_delta and message_stop.
func (p *plan) events(extra map[string]json.RawMessage) []event {
	var out []event
	add := func(name string, v any) {
		out = append(out, event{name: name, data: withExtra(mustMarshal(v), extra)})
	}
	start := wireMessage{ID: p.id, Type: "message", Role: "assistant", Model: p.model, Content: []wireBlock{}, Usage: p.wireUsage(1)}
	add("message_start", map[string]any{"type": "message_start", "message": start})
	for i, bp := range p.blocks {
		add("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": bp.wire(false)})
		if i == 0 {
			add("ping", map[string]string{"type": "ping"})
		}
		for _, piece := range bp.pieces {
			delta := map[string]string{"type": "text_delta", "text": piece}
			if bp.toolUse {
				delta = map[string]string{"type": "input_json_delta", "partial_json": piece}
			}
			add("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": delta})
		}
		add("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	add("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": p.stopReason, "stop_sequence": p.stopSequence},
		"usage": map[string]int{"output_tokens": p.usage.OutputTokens},
	})
	add("message_stop", map[string]string{"type": "message_stop"})
	return out
}

// mustMarshal encodes v without HTML escaping.
func mustMarshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// Only the fixed wire types above are marshaled.
		panic("fakeanthropic: encoding a response: " + err.Error())
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
}

// withExtra adds the extra fields, in key order, to the JSON object obj.
func withExtra(obj []byte, extra map[string]json.RawMessage) []byte {
	if len(extra) == 0 {
		return obj
	}
	var buf bytes.Buffer
	buf.Write(obj[:len(obj)-1])
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		buf.WriteByte(',')
		buf.Write(mustMarshal(k))
		buf.WriteByte(':')
		buf.Write(extra[k])
	}
	buf.WriteByte('}')
	return buf.Bytes()
}
