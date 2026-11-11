package anthropic

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// DefaultMaxTokens is the max_tokens sent when neither the request nor the
// catalog gives one. The Messages API requires the field.
const DefaultMaxTokens = 4096

// JSONObjectInstruction is appended to the system prompt for
// response_format json_object. The Messages API has no free-form JSON
// mode, and a forced tool is rejected by current models (Claude Opus 5.5,
// Claude Sonnet 5.5 and the Fable 5.1+ family return 400 for a tool_choice
// of type tool or any), so the adapter asks for JSON in the system prompt.
const JSONObjectInstruction = "Respond with a single valid JSON object and nothing else: no text before or after it, and no Markdown code fence."

// emptyObjectSchema is the input_schema of a function without parameters.
var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// imageMediaTypes are the base64 image types the Messages API accepts.
var imageMediaTypes = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true,
}

// Wire types of the Messages API request, in the API's field order. Only
// the fields the adapter maps exist, so nothing else can be forwarded.
type (
	messagesRequest struct {
		Model         string          `json:"model"`
		MaxTokens     int64           `json:"max_tokens"`
		System        []block         `json:"system,omitempty"`
		Messages      []message       `json:"messages"`
		StopSequences []string        `json:"stop_sequences,omitempty"`
		Temperature   json.RawMessage `json:"temperature,omitempty"`
		TopP          json.RawMessage `json:"top_p,omitempty"`
		Metadata      *metadata       `json:"metadata,omitempty"`
		Tools         []tool          `json:"tools,omitempty"`
		ToolChoice    *toolChoice     `json:"tool_choice,omitempty"`
		OutputConfig  *outputConfig   `json:"output_config,omitempty"`
		Stream        bool            `json:"stream,omitempty"`
	}
	message struct {
		Role    string  `json:"role"`
		Content []block `json:"content"`
	}
	// block is a request content block: text, image, tool_use or
	// tool_result. Only the fields of its type are set.
	block struct {
		Type      string          `json:"type"`
		Text      string          `json:"text,omitempty"`
		Source    *imageSource    `json:"source,omitempty"`
		ID        string          `json:"id,omitempty"`
		Name      string          `json:"name,omitempty"`
		Input     json.RawMessage `json:"input,omitempty"`
		ToolUseID string          `json:"tool_use_id,omitempty"`
		// Content is a tool_result's content: a string or text blocks.
		Content any `json:"content,omitempty"`
	}
	imageSource struct {
		Type      string `json:"type"` // base64 or url
		MediaType string `json:"media_type,omitempty"`
		Data      string `json:"data,omitempty"`
		URL       string `json:"url,omitempty"`
	}
	metadata struct {
		UserID string `json:"user_id"`
	}
	tool struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		InputSchema json.RawMessage `json:"input_schema"`
		Strict      *bool           `json:"strict,omitempty"`
	}
	toolChoice struct {
		Type                   string `json:"type"` // auto, any, tool or none
		Name                   string `json:"name,omitempty"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
	}
	outputConfig struct {
		Format *outputFormat `json:"format,omitempty"`
	}
	outputFormat struct {
		Type   string          `json:"type"` // json_schema
		Schema json.RawMessage `json:"schema"`
	}
)

// The client's OpenAI-shaped fields that the translation reads.
type (
	openAIMessage struct {
		Role         string           `json:"role"`
		Content      json.RawMessage  `json:"content"`
		ToolCalls    []openAIToolCall `json:"tool_calls"`
		ToolCallID   string           `json:"tool_call_id"`
		FunctionCall json.RawMessage  `json:"function_call"`
	}
	openAIToolCall struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	openAIPart struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Refusal  string `json:"refusal"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	openAITool struct {
		Type     string `json:"type"`
		Function *struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      *bool           `json:"strict"`
		} `json:"function"`
	}
	openAIResponseFormat struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
)

// translate builds the Messages request body for req (DESIGN §6.2). It
// returns a KindUnsupported *providers.Error for a request the Messages API
// cannot express, or that needs a capability the deployment lacks; nothing
// is sent upstream then.
func (a *Adapter) translate(req *providers.Request, stream bool) ([]byte, error) {
	c := req.Chat
	out := messagesRequest{Model: req.UpstreamModel, Stream: stream}

	if _, ok := present(c, "functions"); ok {
		return nil, a.unsupported("functions", "the deprecated functions parameter is not supported; use tools")
	}
	if _, ok := present(c, "function_call"); ok {
		return nil, a.unsupported("function_call", "the deprecated function_call parameter is not supported; use tool_choice")
	}

	if err := a.translateMessages(req, &out); err != nil {
		return nil, err
	}

	switch n, ok := c.MaxTokens(); {
	case ok:
		out.MaxTokens = n
	case req.MaxOutputTokens > 0:
		out.MaxTokens = req.MaxOutputTokens
	default:
		out.MaxTokens = DefaultMaxTokens
	}

	if raw, ok := present(c, "stop"); ok {
		var one string
		var many []string
		switch {
		case json.Unmarshal(raw, &one) == nil:
			many = []string{one}
		case json.Unmarshal(raw, &many) == nil:
		default:
			return nil, a.unsupported("stop", "stop must be a string or an array of strings")
		}
		for _, s := range many {
			if s != "" {
				out.StopSequences = append(out.StopSequences, s)
			}
		}
	}
	if raw, ok := present(c, "temperature"); ok {
		out.Temperature = raw
	}
	if raw, ok := present(c, "top_p"); ok {
		out.TopP = raw
	}
	for _, field := range []string{"user", "safety_identifier"} {
		raw, ok := present(c, field)
		var id string
		if ok && json.Unmarshal(raw, &id) == nil && id != "" {
			out.Metadata = &metadata{UserID: id}
			break
		}
	}

	if err := a.translateTools(c, &out); err != nil {
		return nil, err
	}
	if err := a.translateResponseFormat(req, &out); err != nil {
		return nil, err
	}

	body, err := marshal(out)
	if err != nil {
		return nil, &providers.Error{
			Kind: providers.KindClientError, Provider: a.id, Message: "could not encode the upstream request", Err: err,
		}
	}
	return body, nil
}

// present returns the raw value of a top-level field that is present and
// not null.
func present(c *schema.ChatRequest, name string) (json.RawMessage, bool) {
	raw, ok := c.Field(name)
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	return raw, true
}

// translateMessages fills out.System and out.Messages. System and developer
// messages become system text blocks, in order. Tool messages become
// tool_result blocks in a user message. Consecutive messages with the same
// role are merged into one, so tool results and the user text after them
// share a message, tool results first.
func (a *Adapter) translateMessages(req *providers.Request, out *messagesRequest) error {
	raw, _ := req.Chat.Field("messages")
	var msgs []openAIMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return a.unsupported("messages", "messages must be an array of message objects")
	}
	for i, m := range msgs {
		where := fmt.Sprintf("messages[%d]", i)
		var role string
		var blocks []block
		switch m.Role {
		case "system", "developer":
			texts, err := a.textBlocks(m.Content, where)
			if err != nil {
				return err
			}
			out.System = append(out.System, texts...)
			continue
		case "user":
			role = "user"
			var err error
			if blocks, err = a.userBlocks(m.Content, where, req.Capabilities); err != nil {
				return err
			}
		case "assistant":
			role = "assistant"
			var err error
			if blocks, err = a.assistantBlocks(m, where); err != nil {
				return err
			}
		case "tool":
			role = "user"
			if m.ToolCallID == "" {
				return a.unsupported("messages", "%s.tool_call_id is required", where)
			}
			result := block{Type: "tool_result", ToolUseID: toolUseID(m.ToolCallID)}
			var s string
			if json.Unmarshal(m.Content, &s) == nil {
				if s != "" {
					result.Content = s
				}
			} else {
				texts, err := a.textBlocks(m.Content, where)
				if err != nil {
					return err
				}
				if len(texts) > 0 {
					result.Content = texts
				}
			}
			blocks = []block{result}
		case "function":
			return a.unsupported("messages", "%s: the deprecated function role is not supported; use tool messages", where)
		default:
			return a.unsupported("messages", "%s.role must be system, developer, user, assistant or tool", where)
		}
		if len(blocks) == 0 {
			continue // an empty message adds nothing
		}
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
			continue
		}
		out.Messages = append(out.Messages, message{Role: role, Content: blocks})
	}
	if len(out.Messages) == 0 {
		return a.unsupported("messages", "at least one user or assistant message with content is required")
	}
	return nil
}

// parseContent decodes a message's content: absent or null (nil, nil), a
// string (one text part) or an array of parts.
func (a *Adapter) parseContent(raw json.RawMessage, where string) ([]openAIPart, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []openAIPart{{Type: "text", Text: s}}, nil
	}
	var parts []openAIPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, a.unsupported("messages", "%s.content must be a string or an array of content parts", where)
	}
	return parts, nil
}

// textBlocks maps content that may only hold text (system, developer and
// tool messages) to text blocks. Empty text is dropped: the Messages API
// rejects empty text blocks.
func (a *Adapter) textBlocks(raw json.RawMessage, where string) ([]block, error) {
	parts, err := a.parseContent(raw, where)
	if err != nil {
		return nil, err
	}
	var out []block
	for j, p := range parts {
		if p.Type != "text" {
			return nil, a.unsupported("messages", "%s.content[%d]: only text parts are supported here", where, j)
		}
		if p.Text != "" {
			out = append(out, block{Type: "text", Text: p.Text})
		}
	}
	return out, nil
}

// userBlocks maps a user message's content: text parts to text blocks and
// image_url parts to image blocks. An image part on a deployment without
// the vision capability is a client error. Routing never picks such a
// deployment for a request it sees images in, but it reads member names
// exactly while this decoding, like encoding/json, does not, so the check
// keeps a part routing saw as text from reaching the upstream as an image.
func (a *Adapter) userBlocks(raw json.RawMessage, where string, caps schema.CapabilitySet) ([]block, error) {
	parts, err := a.parseContent(raw, where)
	if err != nil {
		return nil, err
	}
	var out []block
	for j, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, block{Type: "text", Text: p.Text})
			}
		case "image_url":
			if !caps.Has(schema.CapVision) {
				return nil, a.unsupported("messages", "%s.content[%d]: this deployment does not accept images (capability vision)", where, j)
			}
			if p.ImageURL == nil {
				return nil, a.unsupported("messages", "%s.content[%d].image_url is required", where, j)
			}
			src, err := a.imageSource(p.ImageURL.URL, fmt.Sprintf("%s.content[%d]", where, j))
			if err != nil {
				return nil, err
			}
			out = append(out, block{Type: "image", Source: src})
		default:
			return nil, a.unsupported("messages", "%s.content[%d]: content parts of type %q are not supported", where, j, safeName(p.Type))
		}
	}
	return out, nil
}

// imageSource maps an image_url: a data URL with base64 data becomes a
// base64 source, an https URL a url source.
func (a *Adapter) imageSource(u, where string) (*imageSource, error) {
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		meta, data, ok := strings.Cut(rest, ",")
		mediaType, isBase64 := strings.CutSuffix(meta, ";base64")
		mediaType = strings.ToLower(mediaType)
		if !ok || !isBase64 || data == "" {
			return nil, a.unsupported("messages", "%s: an image data URL must be base64-encoded", where)
		}
		if !imageMediaTypes[mediaType] {
			return nil, a.unsupported("messages", "%s: image type %q is not supported; use image/jpeg, image/png, image/gif or image/webp", where, safeName(mediaType))
		}
		return &imageSource{Type: "base64", MediaType: mediaType, Data: data}, nil
	}
	if len(u) > len("https://") && strings.EqualFold(u[:len("https://")], "https://") {
		return &imageSource{Type: "url", URL: u}, nil
	}
	return nil, a.unsupported("messages", "%s: an image URL must be an https URL or a base64 data URL", where)
}

// assistantBlocks maps an assistant message: its text (and refusal) parts
// to text blocks, then its tool calls to tool_use blocks whose input is the
// parsed arguments object.
func (a *Adapter) assistantBlocks(m openAIMessage, where string) ([]block, error) {
	if len(m.FunctionCall) > 0 && !bytes.Equal(bytes.TrimSpace(m.FunctionCall), []byte("null")) {
		return nil, a.unsupported("messages", "%s: the deprecated function_call is not supported; use tool_calls", where)
	}
	parts, err := a.parseContent(m.Content, where)
	if err != nil {
		return nil, err
	}
	var out []block
	for j, p := range parts {
		text := p.Text
		switch p.Type {
		case "text":
		case "refusal":
			text = p.Refusal
		default:
			return nil, a.unsupported("messages", "%s.content[%d]: content parts of type %q are not supported", where, j, safeName(p.Type))
		}
		if text != "" {
			out = append(out, block{Type: "text", Text: text})
		}
	}
	for j, tc := range m.ToolCalls {
		at := fmt.Sprintf("%s.tool_calls[%d]", where, j)
		if tc.Type != "" && tc.Type != "function" {
			return nil, a.unsupported("messages", "%s: tool calls of type %q are not supported", at, safeName(tc.Type))
		}
		if tc.ID == "" || tc.Function.Name == "" {
			return nil, a.unsupported("messages", "%s: id and function.name are required", at)
		}
		input, ok := argumentsObject(tc.Function.Arguments)
		if !ok {
			return nil, a.unsupported("messages", "%s.function.arguments must be a JSON object", at)
		}
		out = append(out, block{Type: "tool_use", ID: toolUseID(tc.ID), Name: tc.Function.Name, Input: input})
	}
	return out, nil
}

// toolUseID maps an OpenAI tool-call id onto Anthropic's id pattern,
// ^[a-zA-Z0-9_-]+$. A valid id is kept. Any other has each invalid
// character replaced by _ and a short hash of the original appended, so
// that distinct ids stay distinct; the mapping is deterministic, so an
// assistant tool call and the tool message answering it get the same id.
// Response ids need no mapping back: the client gets Anthropic's own,
// which are already valid.
func toolUseID(id string) string {
	valid := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
	}
	if !strings.ContainsFunc(id, func(r rune) bool { return !valid(r) }) {
		return id
	}
	mapped := strings.Map(func(r rune) rune {
		if valid(r) {
			return r
		}
		return '_'
	}, id)
	sum := sha256.Sum256([]byte(id))
	return mapped + "_" + hex.EncodeToString(sum[:4])
}

// argumentsObject parses a tool call's arguments string into a compact
// JSON object. An empty string is an empty object.
func argumentsObject(args string) (json.RawMessage, bool) {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return json.RawMessage(`{}`), true
	}
	if trimmed[0] != '{' || !json.Valid([]byte(trimmed)) {
		return nil, false
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(trimmed)); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// translateTools maps tools, tool_choice and parallel_tool_calls.
// tool_choice auto, none and required become auto, none and any, and a
// named function becomes a tool choice. parallel_tool_calls: false sets
// disable_parallel_tool_use. Without tools, tool_choice is not sent.
func (a *Adapter) translateTools(c *schema.ChatRequest, out *messagesRequest) error {
	for j, raw := range c.Tools() {
		var t openAITool
		if err := json.Unmarshal(raw, &t); err != nil || t.Function == nil {
			return a.unsupported("tools", "tools[%d] must be a function tool", j)
		}
		if t.Type != "function" {
			return a.unsupported("tools", "tools[%d]: tools of type %q are not supported", j, safeName(t.Type))
		}
		if t.Function.Name == "" {
			return a.unsupported("tools", "tools[%d].function.name is required", j)
		}
		schemaRaw := t.Function.Parameters
		if len(schemaRaw) == 0 || bytes.Equal(bytes.TrimSpace(schemaRaw), []byte("null")) {
			schemaRaw = emptyObjectSchema
		}
		var strict *bool
		if t.Function.Strict != nil && *t.Function.Strict {
			strict = t.Function.Strict
		}
		out.Tools = append(out.Tools, tool{
			Name: t.Function.Name, Description: t.Function.Description, InputSchema: schemaRaw, Strict: strict,
		})
	}
	if len(out.Tools) == 0 {
		return nil
	}

	choice := &toolChoice{Type: "auto"}
	if raw, ok := present(c, "tool_choice"); ok {
		var s string
		var named struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		switch {
		case json.Unmarshal(raw, &s) == nil:
			switch s {
			case "auto":
			case "none":
				choice.Type = "none"
			case "required":
				choice.Type = "any"
			default:
				return a.unsupported("tool_choice", "tool_choice must be auto, none, required or a function")
			}
		case json.Unmarshal(raw, &named) == nil && named.Type == "function" && named.Function.Name != "":
			choice = &toolChoice{Type: "tool", Name: named.Function.Name}
		default:
			return a.unsupported("tool_choice", "tool_choice must be auto, none, required or a function")
		}
	}
	if raw, ok := present(c, "parallel_tool_calls"); ok && string(bytes.TrimSpace(raw)) == "false" && choice.Type != "none" {
		choice.DisableParallelToolUse = true
	}
	out.ToolChoice = choice
	return nil
}

// translateResponseFormat maps response_format. json_schema uses native
// structured output (output_config.format), with the schema reduced to the
// subset the Messages API accepts (see structuredSchema); a deployment
// without the json_schema capability gets a client error, since routing
// should never have chosen it. json_object, and json_schema without a
// schema, append JSONObjectInstruction to the system prompt.
func (a *Adapter) translateResponseFormat(req *providers.Request, out *messagesRequest) error {
	raw, ok := present(req.Chat, "response_format")
	if !ok {
		return nil
	}
	var rf openAIResponseFormat
	if err := json.Unmarshal(raw, &rf); err != nil {
		return a.unsupported("response_format", "response_format must be an object")
	}
	switch rf.Type {
	case "", "text":
		return nil
	case "json_object":
		// Checked for the same reason as vision in userBlocks.
		if !req.Capabilities.Has(schema.CapJSONMode) {
			return a.unsupported("response_format", "response_format json_object needs the capability json_mode, and this deployment does not declare it")
		}
	case "json_schema":
		if rf.JSONSchema == nil || len(rf.JSONSchema.Schema) == 0 ||
			bytes.Equal(bytes.TrimSpace(rf.JSONSchema.Schema), []byte("null")) {
			break // no schema: any JSON object will do
		}
		if !req.Capabilities.Has(schema.CapJSONSchema) {
			return a.unsupported("response_format",
				"response_format json_schema needs native structured output (capability json_schema), and this deployment does not declare it")
		}
		out.OutputConfig = &outputConfig{Format: &outputFormat{Type: "json_schema", Schema: structuredSchema(rf.JSONSchema.Schema)}}
		return nil
	default:
		return a.unsupported("response_format", "response_format type %q is not supported", safeName(rf.Type))
	}
	out.System = append(out.System, block{Type: "text", Text: JSONObjectInstruction})
	return nil
}

// maxNameLen bounds a client-supplied name quoted in an error message.
const maxNameLen = 40

// safeName returns s for quoting in an error message: at most maxNameLen
// bytes of printable ASCII, or "…" in place of anything else.
func safeName(s string) string {
	if len(s) > maxNameLen {
		return "…"
	}
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return "…"
		}
	}
	return s
}
