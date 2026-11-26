package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// imageMediaTypes are the inline image types the Gemini API accepts.
var imageMediaTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/webp": true, "image/heic": true, "image/heif": true,
}

// maxStopSequences is the most stop sequences generateContent accepts.
const maxStopSequences = 5

// thinkingLevels maps reasoning_effort onto thinkingConfig.thinkingLevel,
// clamped to the levels every current Gemini model accepts: Gemini 3.8
// Flash and Gemini 4 cannot turn thinking off and reject MINIMAL, so none
// and minimal ask for LOW; XHIGH exists only on Gemini 4 Neon and
// Hydrogen, and Google rejects it on the others with a 400 that does not
// fall back, so xhigh asks for HIGH. Under auto routing the client does
// not choose the model, so the same request must work on every Gemini
// deployment. The names are the enum's, as Google's Go SDK sends them.
var thinkingLevels = map[string]string{
	"none": "LOW", "minimal": "LOW", "low": "LOW", "medium": "MEDIUM", "high": "HIGH", "xhigh": "HIGH",
}

// refusedCapabilities are the request features the native API cannot
// honor: candidateCount is unsupported on Gemini 3 and later, Gemini 3.6+
// and 4 ignore temperature, top_p and top_k, and logprobs, seed and the
// penalties are not documented for current models. The catalog does not
// list them for Gemini, so the gateway already excludes the deployment or
// drops the parameters; a request that still has them (a deployment whose
// capabilities say otherwise) is refused rather than silently changed.
var refusedCapabilities = []schema.Capability{
	schema.CapN, schema.CapLogprobs, schema.CapSeed, schema.CapSampling, schema.CapTemperature2, schema.CapPenalties,
}

// Wire types of the generateContent request. Only the fields the adapter
// maps exist, so nothing else can be forwarded.
type (
	generateRequest struct {
		Contents          []content         `json:"contents"`
		SystemInstruction *content          `json:"systemInstruction,omitempty"`
		Tools             []tool            `json:"tools,omitempty"`
		ToolConfig        *toolConfig       `json:"toolConfig,omitempty"`
		GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
		// Store is always false: paid projects created from 2026-10-26
		// store requests unless told not to.
		Store bool `json:"store"`
	}
	content struct {
		Role  string `json:"role,omitempty"` // user or model
		Parts []part `json:"parts"`
	}
	// part is one Part union member: text, inlineData, functionCall or
	// functionResponse, with a functionCall's thoughtSignature beside it.
	part struct {
		Text             string            `json:"text,omitempty"`
		InlineData       *blob             `json:"inlineData,omitempty"`
		FunctionCall     *functionCall     `json:"functionCall,omitempty"`
		FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
		ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	}
	blob struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	}
	functionCall struct {
		ID   string          `json:"id,omitempty"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	functionResponse struct {
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
	}
	tool struct {
		FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
	}
	functionDeclaration struct {
		Name                 string          `json:"name"`
		Description          string          `json:"description,omitempty"`
		ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
	}
	toolConfig struct {
		FunctionCallingConfig functionCallingConfig `json:"functionCallingConfig"`
	}
	functionCallingConfig struct {
		Mode                 string   `json:"mode"` // AUTO, ANY or NONE
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	}
	generationConfig struct {
		StopSequences      []string        `json:"stopSequences,omitempty"`
		MaxOutputTokens    int64           `json:"maxOutputTokens,omitempty"`
		ResponseMimeType   string          `json:"responseMimeType,omitempty"`
		ResponseJSONSchema json.RawMessage `json:"responseJsonSchema,omitempty"`
		ThinkingConfig     *thinkingConfig `json:"thinkingConfig,omitempty"`
	}
	thinkingConfig struct {
		ThinkingLevel string `json:"thinkingLevel"`
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
		} `json:"function"`
	}
	openAIResponseFormat struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
)

// translate builds the generateContent body for req (DESIGN §6.2) and
// returns it with the maxOutputTokens it sets (0 for none). It returns a
// KindUnsupported *providers.Error for a request the Gemini API cannot
// express, or that needs a capability the deployment lacks, and a
// KindClientError one for a request no provider would accept; nothing is
// sent upstream then. The body is the same for generateContent and
// streamGenerateContent, which take the stream choice from the URL.
func (a *Adapter) translate(req *providers.Request) ([]byte, int64, error) {
	c := req.Chat
	var out generateRequest

	if _, ok := present(c, "functions"); ok {
		return nil, 0, a.unsupported("functions", "the deprecated functions parameter is not supported; use tools")
	}
	if _, ok := present(c, "function_call"); ok {
		return nil, 0, a.unsupported("function_call", "the deprecated function_call parameter is not supported; use tool_choice")
	}
	required := c.RequiredCapabilities()
	for _, capability := range refusedCapabilities {
		if required.Has(capability) {
			return nil, 0, a.unsupported(paramOf(capability),
				"the Gemini API cannot honor %s (capability %s); remove it or use another deployment", paramOf(capability), capability)
		}
	}

	if err := a.translateMessages(req, &out); err != nil {
		return nil, 0, err
	}

	gc := &generationConfig{}
	if n, ok := c.MaxTokens(); ok && n > 0 {
		// maxOutputTokens covers thinking and the answer, like
		// max_completion_tokens on OpenAI's reasoning models. Without one
		// the model's default applies.
		gc.MaxOutputTokens = n
	}
	if raw, ok := present(c, "stop"); ok {
		var one string
		var many []string
		switch {
		case json.Unmarshal(raw, &one) == nil:
			many = []string{one}
		case json.Unmarshal(raw, &many) == nil:
		default:
			return nil, 0, a.unsupported("stop", "stop must be a string or an array of strings")
		}
		for _, s := range many {
			if s != "" {
				gc.StopSequences = append(gc.StopSequences, s)
			}
		}
		if len(gc.StopSequences) > maxStopSequences {
			return nil, 0, a.unsupported("stop", "at most %d stop sequences are supported", maxStopSequences)
		}
	}
	if raw, ok := present(c, "reasoning_effort"); ok {
		var effort string
		level, known := "", false
		if json.Unmarshal(raw, &effort) == nil {
			level, known = thinkingLevels[effort]
		}
		if !known {
			return nil, 0, a.unsupported("reasoning_effort", "reasoning_effort must be none, minimal, low, medium, high or xhigh")
		}
		gc.ThinkingConfig = &thinkingConfig{ThinkingLevel: level}
	}
	if err := a.translateResponseFormat(req, gc); err != nil {
		return nil, 0, err
	}
	if gc.MaxOutputTokens != 0 || gc.StopSequences != nil || gc.ResponseMimeType != "" || gc.ThinkingConfig != nil {
		out.GenerationConfig = gc
	}

	if err := a.translateTools(c, &out); err != nil {
		return nil, 0, err
	}

	body, err := marshal(out)
	if err != nil {
		return nil, 0, &providers.Error{
			Kind: providers.KindClientError, Provider: a.id, Message: "could not encode the upstream request", Err: err,
		}
	}
	return body, gc.MaxOutputTokens, nil
}

// paramOf names the request parameter behind a refused capability.
func paramOf(c schema.Capability) string {
	switch c {
	case schema.CapN:
		return "n"
	case schema.CapLogprobs:
		return "logprobs"
	case schema.CapSeed:
		return "seed"
	case schema.CapPenalties:
		return "frequency_penalty"
	default: // sampling, temperature_2
		return "temperature"
	}
}

// present returns the raw value of a top-level field that is present and
// not null.
func present(c *schema.ChatRequest, name string) (json.RawMessage, bool) {
	raw, ok := c.Field(name)
	if !ok || isNull(raw) {
		return nil, false
	}
	return raw, true
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// translateMessages fills out.SystemInstruction and out.Contents.
//
//   - System and developer messages become systemInstruction text parts,
//     in order.
//   - User messages become user contents; consecutive ones are merged,
//     but never into a content of function responses: Google tells the
//     current turn by the last user content with text or images.
//   - Assistant messages become model contents: text parts, then one
//     functionCall part per tool call. An assistant message is merged into
//     the one before it only if that one has no function calls, so each
//     model content with calls is exactly one step of the model, and its
//     first functionCall part is the first call of that step.
//   - Consecutive tool messages become one user content of
//     functionResponse parts, in order, so the responses of parallel calls
//     are never interleaved with the calls.
//
// Every tool-call id is decoded (see decodeToolCallID): the function call
// and its response get the Gemini id (the client's own id when it is not
// one this adapter made), and the first call of each step its thought
// signature for this model, or else SkipSignature.
func (a *Adapter) translateMessages(req *providers.Request, out *generateRequest) error {
	raw, _ := req.Chat.Field("messages")
	var msgs []openAIMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return a.unsupported("messages", "messages must be an array of message objects")
	}
	model := req.UpstreamModel
	names := make(map[string]string) // tool-call id -> function name
	var system []part
	// lastTool is set while the last content holds functionResponse parts.
	lastTool := false
	for i, m := range msgs {
		where := fmt.Sprintf("messages[%d]", i)
		switch m.Role {
		case "system", "developer":
			texts, err := a.textParts(m.Content, where)
			if err != nil {
				return err
			}
			system = append(system, texts...)
		case "user":
			parts, err := a.userParts(m.Content, where, req.Capabilities)
			if err != nil {
				return err
			}
			if len(parts) == 0 {
				continue
			}
			if n := len(out.Contents); n > 0 && out.Contents[n-1].Role == "user" && !lastTool {
				out.Contents[n-1].Parts = append(out.Contents[n-1].Parts, parts...)
			} else {
				out.Contents = append(out.Contents, content{Role: "user", Parts: parts})
			}
			lastTool = false
		case "assistant":
			parts, err := a.modelParts(m, where, model, names)
			if err != nil {
				return err
			}
			if len(parts) == 0 {
				continue
			}
			if n := len(out.Contents); n > 0 && out.Contents[n-1].Role == "model" && !hasCalls(out.Contents[n-1].Parts) {
				out.Contents[n-1].Parts = append(out.Contents[n-1].Parts, parts...)
			} else {
				out.Contents = append(out.Contents, content{Role: "model", Parts: parts})
			}
			lastTool = false
		case "tool":
			fr, err := a.toolResponse(m, where, model, names)
			if err != nil {
				return err
			}
			p := part{FunctionResponse: fr}
			if n := len(out.Contents); n > 0 && lastTool {
				out.Contents[n-1].Parts = append(out.Contents[n-1].Parts, p)
			} else {
				out.Contents = append(out.Contents, content{Role: "user", Parts: []part{p}})
			}
			lastTool = true
		case "function":
			return a.unsupported("messages", "%s: the deprecated function role is not supported; use tool messages", where)
		default:
			return a.unsupported("messages", "%s.role must be system, developer, user, assistant or tool", where)
		}
	}
	if len(system) > 0 {
		out.SystemInstruction = &content{Parts: system}
	}
	if len(out.Contents) == 0 {
		return a.unsupported("messages", "at least one user or assistant message with content is required")
	}
	if out.Contents[len(out.Contents)-1].Role == "model" {
		// Google documents removing prefilled model turns: the request must
		// end with the user (or with tool results).
		return a.unsupported("messages", "the last message must not be an assistant message: Gemini does not continue a prefilled answer")
	}
	return nil
}

// hasCalls reports whether parts include a function call.
func hasCalls(parts []part) bool {
	for _, p := range parts {
		if p.FunctionCall != nil {
			return true
		}
	}
	return false
}

// parseContent decodes a message's content: absent or null (nil, nil), a
// string (one text part) or an array of parts.
func (a *Adapter) parseContent(raw json.RawMessage, where string) ([]openAIPart, error) {
	if isNull(raw) {
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

// textParts maps content that may only hold text (system and developer
// messages) to text parts. Empty text is dropped.
func (a *Adapter) textParts(raw json.RawMessage, where string) ([]part, error) {
	parts, err := a.parseContent(raw, where)
	if err != nil {
		return nil, err
	}
	var out []part
	for j, p := range parts {
		if p.Type != "text" {
			return nil, a.unsupported("messages", "%s.content[%d]: only text parts are supported here", where, j)
		}
		if p.Text != "" {
			out = append(out, part{Text: p.Text})
		}
	}
	return out, nil
}

// userParts maps a user message's content: text parts to text parts and
// image_url parts to inlineData parts. An image part on a deployment
// without the vision capability is refused, for the reason the anthropic
// adapter gives (routing matches member names exactly, decoding does not).
func (a *Adapter) userParts(raw json.RawMessage, where string, caps schema.CapabilitySet) ([]part, error) {
	parts, err := a.parseContent(raw, where)
	if err != nil {
		return nil, err
	}
	var out []part
	for j, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, part{Text: p.Text})
			}
		case "image_url":
			if !caps.Has(schema.CapVision) {
				return nil, a.unsupported("messages", "%s.content[%d]: this deployment does not accept images (capability vision)", where, j)
			}
			if p.ImageURL == nil {
				return nil, a.unsupported("messages", "%s.content[%d].image_url is required", where, j)
			}
			img, err := a.inlineImage(p.ImageURL.URL, fmt.Sprintf("%s.content[%d]", where, j))
			if err != nil {
				return nil, err
			}
			out = append(out, part{InlineData: img})
		default:
			return nil, a.unsupported("messages", "%s.content[%d]: content parts of type %q are not supported", where, j, safeName(p.Type))
		}
	}
	return out, nil
}

// inlineImage maps an image_url that is a base64 data URL to inline data.
// The adapter never fetches URLs, so an https image is refused, and the
// request can go to a deployment that takes image URLs.
func (a *Adapter) inlineImage(u, where string) (*blob, error) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return nil, a.unsupported("messages", "%s: Gemini deployments take images only as base64 data URLs; the gateway does not fetch image URLs", where)
	}
	meta, data, ok := strings.Cut(rest, ",")
	mediaType, isBase64 := strings.CutSuffix(meta, ";base64")
	mediaType = strings.ToLower(mediaType)
	if !ok || !isBase64 || data == "" {
		return nil, a.unsupported("messages", "%s: an image data URL must be base64-encoded", where)
	}
	if !imageMediaTypes[mediaType] {
		return nil, a.unsupported("messages", "%s: image type %q is not supported; use image/png, image/jpeg, image/webp, image/heic or image/heif", where, safeName(mediaType))
	}
	return &blob{MimeType: mediaType, Data: data}, nil
}

// modelParts maps an assistant message: its text (and refusal) parts to
// text parts, then its tool calls to functionCall parts whose args are the
// parsed arguments object. The first call gets its thought signature, or
// SkipSignature when it has none for this model (an id from another
// provider or model, or from a call Gemini returned unsigned); the others
// never get one. It records each call's function name
// in names, for the tool messages that answer it.
func (a *Adapter) modelParts(m openAIMessage, where, model string, names map[string]string) ([]part, error) {
	if !isNull(m.FunctionCall) {
		return nil, a.unsupported("messages", "%s: the deprecated function_call is not supported; use tool_calls", where)
	}
	parts, err := a.parseContent(m.Content, where)
	if err != nil {
		return nil, err
	}
	var out []part
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
			out = append(out, part{Text: text})
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
		args, ok := argumentsObject(tc.Function.Arguments)
		if !ok {
			return nil, a.unsupported("messages", "%s.function.arguments must be a JSON object", at)
		}
		names[tc.ID] = tc.Function.Name
		d := decodeToolCallID(tc.ID, model)
		p := part{FunctionCall: &functionCall{ID: d.geminiID, Name: tc.Function.Name, Args: args}}
		if j == 0 {
			// The first call of a step carries its own signature when the
			// adapter made its id, for this model, with one; any other
			// gets the skip value, since a current-turn step without a
			// signature is a 400 that does not fall back.
			p.ThoughtSignature = d.sig
			if p.ThoughtSignature == "" {
				p.ThoughtSignature = SkipSignature
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// toolResponse maps a tool message to a functionResponse. Its name is the
// function of the assistant tool call it answers, which must come earlier
// in the conversation: Gemini requires the name, and an answer to no call
// is invalid for every provider. Content that is a JSON object is the
// response as is; any other text (text parts are concatenated) becomes
// {"result": text}, since the response must be an object.
func (a *Adapter) toolResponse(m openAIMessage, where, model string, names map[string]string) (*functionResponse, error) {
	if m.ToolCallID == "" {
		return nil, a.unsupported("messages", "%s.tool_call_id is required", where)
	}
	name, ok := names[m.ToolCallID]
	if !ok {
		return nil, a.invalid("messages", "%s.tool_call_id does not match a tool call of an earlier assistant message", where)
	}
	var text string
	if json.Unmarshal(m.Content, &text) != nil {
		texts, err := a.textParts(m.Content, where)
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		for _, p := range texts {
			b.WriteString(p.Text)
		}
		text = b.String()
	}
	resp, ok := argumentsObject(text)
	if !ok || strings.TrimSpace(text) == "" {
		resp = mustMarshal(struct {
			Result string `json:"result"`
		}{text})
	}
	return &functionResponse{ID: decodeToolCallID(m.ToolCallID, model).geminiID, Name: name, Response: resp}, nil
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

// translateTools maps tools and tool_choice. Each function becomes a
// functionDeclaration whose parametersJsonSchema is the client's JSON
// Schema as is; a function without parameters gets none, which Google
// documents as optional. tool_choice auto, none and required become modes
// AUTO, NONE and ANY, and a named function ANY with allowedFunctionNames.
// Without tool_choice no toolConfig is sent, so Google's default applies
// (AUTO, or VALIDATED with structured output). parallel_tool_calls has no
// Gemini counterpart: true is the API's behavior, and false is not
// honored (the capability machinery treats it as no requirement, as for
// any OpenAI-compatible upstream that ignores it).
func (a *Adapter) translateTools(c *schema.ChatRequest, out *generateRequest) error {
	var decls []functionDeclaration
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
		d := functionDeclaration{Name: t.Function.Name, Description: t.Function.Description}
		if !isNull(t.Function.Parameters) {
			d.ParametersJSONSchema = t.Function.Parameters
		}
		decls = append(decls, d)
	}
	if len(decls) == 0 {
		return nil
	}
	out.Tools = []tool{{FunctionDeclarations: decls}}

	raw, ok := present(c, "tool_choice")
	if !ok {
		return nil
	}
	var s string
	var named struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	cfg := functionCallingConfig{}
	switch {
	case json.Unmarshal(raw, &s) == nil:
		switch s {
		case "auto":
			cfg.Mode = "AUTO"
		case "none":
			cfg.Mode = "NONE"
		case "required":
			cfg.Mode = "ANY"
		default:
			return a.unsupported("tool_choice", "tool_choice must be auto, none, required or a function")
		}
	case json.Unmarshal(raw, &named) == nil && named.Type == "function" && named.Function.Name != "":
		cfg = functionCallingConfig{Mode: "ANY", AllowedFunctionNames: []string{named.Function.Name}}
	default:
		return a.unsupported("tool_choice", "tool_choice must be auto, none, required or a function")
	}
	out.ToolConfig = &toolConfig{FunctionCallingConfig: cfg}
	return nil
}

// translateResponseFormat maps response_format: json_object to
// responseMimeType application/json, and json_schema to that plus
// responseJsonSchema, the client's JSON Schema as is (Google ignores the
// keywords it does not support). The adapter uses these fields, which
// Google's SDKs send, rather than the newer responseFormat.text. A
// deployment without the json_mode or json_schema capability is refused,
// for the reason the anthropic adapter gives; json_schema without a schema
// is json_object.
func (a *Adapter) translateResponseFormat(req *providers.Request, gc *generationConfig) error {
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
		if !req.Capabilities.Has(schema.CapJSONMode) {
			return a.unsupported("response_format", "response_format json_object needs the capability json_mode, and this deployment does not declare it")
		}
	case "json_schema":
		if rf.JSONSchema == nil || isNull(rf.JSONSchema.Schema) {
			break // no schema: any JSON will do
		}
		if !req.Capabilities.Has(schema.CapJSONSchema) {
			return a.unsupported("response_format",
				"response_format json_schema needs native structured output (capability json_schema), and this deployment does not declare it")
		}
		gc.ResponseJSONSchema = rf.JSONSchema.Schema
	default:
		return a.unsupported("response_format", "response_format type %q is not supported", safeName(rf.Type))
	}
	gc.ResponseMimeType = "application/json"
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
