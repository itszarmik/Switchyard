package fakeupstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"
)

// Canned content. Tests compare what a client received with these.
const (
	// Text is the answer of choice 0 to a plain request.
	Text = "The quick brown fox jumps over the lazy dog."
	// JSONText is the answer to a request with response_format json_object
	// or json_schema.
	JSONText = `{"answer":"Paris","confidence":0.98}`
	// Created is the created timestamp of every completion, chunk and model.
	Created int64 = 1767225600
	// SystemFingerprint is the system_fingerprint of every completion.
	SystemFingerprint = "fp_fakeupstream"
)

// ChoiceText is the plain-text answer of choice i, before stop sequences
// are applied. Choice 0 answers Text.
func ChoiceText(i int) string {
	if i == 0 {
		return Text
	}
	return fmt.Sprintf("Choice %d: %s", i, Text)
}

// toolCities feeds ToolArguments.
var toolCities = [...]string{"Paris, France", "Tokyo, Japan", "Lima, Peru", "Oslo, Norway"}

// ToolArguments is the JSON arguments string of the i-th tool call of a
// choice.
func ToolArguments(i int) string {
	return fmt.Sprintf(`{"location":%q,"unit":"celsius"}`, toolCities[i%len(toolCities)])
}

// ToolCallID is the ID of the i-th tool call of choice c.
func ToolCallID(c, i int) string {
	return fmt.Sprintf("call_fake_%d_%d", c, i)
}

// chatRequest is what the fake reads from a chat completion request.
type chatRequest struct {
	model        string
	stream       bool
	includeUsage bool
	n            int
	stop         []string
	tools        []string // function names, in order
	toolChoice   string   // "", "auto", "none", "required", or "function"
	forcedTool   string   // with toolChoice "function"
	parallel     bool
	jsonMode     bool
	promptBytes  int
}

// MaxToolCallIDLen is the longest tool-call id OpenAI accepts in a
// request's messages.
const MaxToolCallIDLen = 40

// checkToolCallIDs rejects an assistant tool_calls[].id or a tool message's
// tool_call_id longer than MaxToolCallIDLen, as OpenAI does.
func checkToolCallIDs(messages []json.RawMessage) *apiError {
	tooLong := func(param string, n int) *apiError {
		return &apiError{status: http.StatusBadRequest, typ: typeInvalidRequest, param: param, code: "string_above_max_length",
			message: fmt.Sprintf("Invalid '%s': string too long. Expected a string with maximum length %d, but got a string with length %d instead.", param, MaxToolCallIDLen, n)}
	}
	for i, raw := range messages {
		var m struct {
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if n := len(m.ToolCallID); n > MaxToolCallIDLen {
			return tooLong(fmt.Sprintf("messages[%d].tool_call_id", i), n)
		}
		for j, tc := range m.ToolCalls {
			if n := len(tc.ID); n > MaxToolCallIDLen {
				return tooLong(fmt.Sprintf("messages[%d].tool_calls[%d].id", i, j), n)
			}
		}
	}
	return nil
}

// parseChatRequest decodes and validates body the way OpenAI does for the
// fields the fake uses.
func parseChatRequest(body []byte) (chatRequest, *apiError) {
	bad := func(param, code, format string, args ...any) *apiError {
		return &apiError{status: http.StatusBadRequest, message: fmt.Sprintf(format, args...),
			typ: typeInvalidRequest, param: param, code: code}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return chatRequest{}, bad("", "", "We could not parse the JSON body of your request. (HINT: This likely means you aren't using your HTTP library correctly. The OpenAI API expects a JSON payload, but what was sent was not valid JSON.)")
	}
	present := func(k string) bool { v, ok := raw[k]; return ok && string(v) != "null" }

	req := chatRequest{n: 1, parallel: true}
	if !present("model") {
		return req, bad("", "", "you must provide a model parameter")
	}
	if json.Unmarshal(raw["model"], &req.model) != nil {
		return req, bad("model", "invalid_type", "Invalid type for 'model': expected a string.")
	}

	if !present("messages") {
		return req, bad("messages", "missing_required_parameter", "Missing required parameter: 'messages'.")
	}
	var messages []json.RawMessage
	if json.Unmarshal(raw["messages"], &messages) != nil {
		return req, bad("messages", "invalid_type", "Invalid type for 'messages': expected an array of objects.")
	}
	if len(messages) == 0 {
		return req, bad("messages", "empty_array", "Invalid 'messages': empty array. Expected an array with minimum length 1, but got an empty array instead.")
	}
	req.promptBytes = len(raw["messages"])
	if aerr := checkToolCallIDs(messages); aerr != nil {
		return req, aerr
	}

	if present("stream") && json.Unmarshal(raw["stream"], &req.stream) != nil {
		return req, bad("stream", "invalid_type", "Invalid type for 'stream': expected a boolean.")
	}
	if present("stream_options") {
		var so struct {
			IncludeUsage *bool `json:"include_usage"`
		}
		if json.Unmarshal(raw["stream_options"], &so) != nil {
			return req, bad("stream_options", "invalid_type", "Invalid type for 'stream_options': expected an object.")
		}
		req.includeUsage = so.IncludeUsage != nil && *so.IncludeUsage
	}
	if present("n") {
		if json.Unmarshal(raw["n"], &req.n) != nil {
			return req, bad("n", "invalid_type", "Invalid type for 'n': expected an integer.")
		}
		if req.n < 1 || req.n > 128 {
			return req, bad("n", "invalid_value", "Invalid 'n': expected a value between 1 and 128, but got %d instead.", req.n)
		}
	}
	if present("stop") {
		var one string
		if json.Unmarshal(raw["stop"], &one) == nil {
			req.stop = []string{one}
		} else if json.Unmarshal(raw["stop"], &req.stop) != nil {
			return req, bad("stop", "invalid_type", "Invalid type for 'stop': expected a string or an array of strings.")
		}
	}
	if present("tools") {
		var tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(raw["tools"], &tools) != nil {
			return req, bad("tools", "invalid_type", "Invalid type for 'tools': expected an array of objects.")
		}
		for _, t := range tools {
			req.tools = append(req.tools, t.Function.Name)
		}
	}
	if present("tool_choice") {
		var forced struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		switch {
		case json.Unmarshal(raw["tool_choice"], &req.toolChoice) == nil:
		case json.Unmarshal(raw["tool_choice"], &forced) == nil:
			req.toolChoice, req.forcedTool = "function", forced.Function.Name
		default:
			return req, bad("tool_choice", "invalid_type", "Invalid type for 'tool_choice'.")
		}
	}
	if present("parallel_tool_calls") && json.Unmarshal(raw["parallel_tool_calls"], &req.parallel) != nil {
		return req, bad("parallel_tool_calls", "invalid_type", "Invalid type for 'parallel_tool_calls': expected a boolean.")
	}
	if present("response_format") {
		var rf struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw["response_format"], &rf) != nil {
			return req, bad("response_format", "invalid_type", "Invalid type for 'response_format': expected an object.")
		}
		req.jsonMode = rf.Type == "json_object" || rf.Type == "json_schema"
	}
	return req, nil
}

// plan is the canned answer to one request.
type plan struct {
	id           string
	model        string
	includeUsage bool
	choices      []choicePlan
	usage        usage
}

type choicePlan struct {
	content   *string  // nil for a tool-call answer
	pieces    []string // content split into stream deltas
	toolCalls []toolCallPlan
	finish    string
}

type toolCallPlan struct {
	id, name, args string
	pieces         []string // args split into stream deltas
}

// newPlan builds the answer to req. seq makes the completion ID unique.
func newPlan(req chatRequest, seq string) *plan {
	p := &plan{id: "chatcmpl-fake" + seq, model: req.model, includeUsage: req.includeUsage}
	var calls []string
	switch {
	case req.toolChoice == "function":
		calls = []string{req.forcedTool}
	case len(req.tools) == 0 || req.toolChoice == "none":
	case req.parallel:
		calls = req.tools
	default:
		calls = req.tools[:1]
	}

	completionTokens := 0
	for c := range req.n {
		var cp choicePlan
		switch {
		case len(calls) > 0:
			cp.finish = "tool_calls"
			if req.toolChoice == "function" {
				cp.finish = "stop" // as OpenAI does when tool_choice names a function
			}
			for i, name := range calls {
				tc := toolCallPlan{id: ToolCallID(c, i), name: name, args: ToolArguments(i)}
				tc.pieces = splitBytes(tc.args, 6)
				cp.toolCalls = append(cp.toolCalls, tc)
				completionTokens += len(tc.pieces) + 1
			}
		case req.jsonMode:
			text := JSONText
			cp.content, cp.pieces, cp.finish = &text, splitBytes(text, 8), "stop"
		default:
			text := applyStop(ChoiceText(c), req.stop)
			cp.content, cp.pieces, cp.finish = &text, splitWords(text), "stop"
		}
		completionTokens += len(cp.pieces)
		p.choices = append(p.choices, cp)
	}
	p.usage = usage{PromptTokens: max(1, req.promptBytes/4), CompletionTokens: completionTokens}
	p.usage.TotalTokens = p.usage.PromptTokens + p.usage.CompletionTokens
	return p
}

// applyStop cuts text at the earliest occurrence of any stop sequence.
func applyStop(text string, stops []string) string {
	cut := len(text)
	for _, s := range stops {
		if i := strings.Index(text, s); s != "" && i >= 0 && i < cut {
			cut = i
		}
	}
	return text[:cut]
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

// Wire types, in OpenAI's field order.

type usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		AudioTokens      int `json:"audio_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens          int `json:"reasoning_tokens"`
		AudioTokens              int `json:"audio_tokens"`
		AcceptedPredictionTokens int `json:"accepted_prediction_tokens"`
		RejectedPredictionTokens int `json:"rejected_prediction_tokens"`
	} `json:"completion_tokens_details"`
}

type functionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type message struct {
	Role        string     `json:"role"`
	Content     *string    `json:"content"`
	Refusal     *string    `json:"refusal"`
	ToolCalls   []toolCall `json:"tool_calls,omitempty"`
	Annotations []any      `json:"annotations"`
}

type choice struct {
	Index        int       `json:"index"`
	Message      message   `json:"message"`
	Logprobs     *struct{} `json:"logprobs"`
	FinishReason string    `json:"finish_reason"`
}

type completion struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	Choices           []choice `json:"choices"`
	Usage             usage    `json:"usage"`
	ServiceTier       string   `json:"service_tier"`
	SystemFingerprint string   `json:"system_fingerprint"`
}

type toolCallDelta struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function functionCall `json:"function"`
}

type delta struct {
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	ToolCalls []toolCallDelta `json:"tool_calls,omitempty"`
	Refusal   json.RawMessage `json:"refusal,omitempty"`
}

type chunkChoice struct {
	Index        int       `json:"index"`
	Delta        delta     `json:"delta"`
	Logprobs     *struct{} `json:"logprobs"`
	FinishReason *string   `json:"finish_reason"`
}

type chunk struct {
	ID                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
	ServiceTier       string        `json:"service_tier"`
	SystemFingerprint string        `json:"system_fingerprint"`
	Choices           []chunkChoice `json:"choices"`
	Usage             any           `json:"usage,omitempty"`
}

// nullJSON is the JSON null literal.
const nullJSON = "null"

// completion encodes p as a chat.completion object.
func (p *plan) completion(extra map[string]json.RawMessage) []byte {
	out := completion{
		ID: p.id, Object: "chat.completion", Created: Created, Model: p.model,
		Usage: p.usage, ServiceTier: "default", SystemFingerprint: SystemFingerprint,
	}
	for i, cp := range p.choices {
		m := message{Role: "assistant", Content: cp.content, Annotations: []any{}}
		for _, tc := range cp.toolCalls {
			m.ToolCalls = append(m.ToolCalls, toolCall{ID: tc.id, Type: "function",
				Function: functionCall{Name: tc.name, Arguments: tc.args}})
		}
		out.Choices = append(out.Choices, choice{Index: i, Message: m, FinishReason: cp.finish})
	}
	return withExtra(mustMarshal(out), extra)
}

// chunks encodes p as the data payloads of a chat.completion.chunk stream:
// each choice's deltas, interleaved round-robin across choices, then the
// usage chunk when the client asked for it.
func (p *plan) chunks(extra map[string]json.RawMessage) [][]byte {
	perChoice := make([][]chunkChoice, len(p.choices))
	for i, cp := range p.choices {
		perChoice[i] = cp.deltas(i)
	}
	var out [][]byte
	for step := 0; ; step++ {
		emitted := false
		for i := range perChoice {
			if step < len(perChoice[i]) {
				out = append(out, p.chunk([]chunkChoice{perChoice[i][step]}, nil, extra))
				emitted = true
			}
		}
		if !emitted {
			break
		}
	}
	if p.includeUsage {
		out = append(out, p.chunk([]chunkChoice{}, p.usage, extra))
	}
	return out
}

// chunk encodes one chunk. A nil usage is sent as "usage":null when the
// client asked for usage, and omitted otherwise, as OpenAI does.
func (p *plan) chunk(choices []chunkChoice, u any, extra map[string]json.RawMessage) []byte {
	c := chunk{
		ID: p.id, Object: "chat.completion.chunk", Created: Created, Model: p.model,
		ServiceTier: "default", SystemFingerprint: SystemFingerprint, Choices: choices,
	}
	switch {
	case u != nil:
		c.Usage = u
	case p.includeUsage:
		c.Usage = json.RawMessage(nullJSON)
	}
	return withExtra(mustMarshal(c), extra)
}

// deltas returns choice index's stream deltas, the finish delta included.
func (cp choicePlan) deltas(index int) []chunkChoice {
	var out []chunkChoice
	add := func(d delta, finish *string) {
		out = append(out, chunkChoice{Index: index, Delta: d, FinishReason: finish})
	}
	if cp.content != nil {
		add(delta{Role: "assistant", Content: mustMarshal(""), Refusal: json.RawMessage(nullJSON)}, nil)
		for _, piece := range cp.pieces {
			add(delta{Content: mustMarshal(piece)}, nil)
		}
	}
	for i, tc := range cp.toolCalls {
		header := toolCallDelta{Index: i, ID: tc.id, Type: "function", Function: functionCall{Name: tc.name}}
		if i == 0 {
			add(delta{Role: "assistant", Content: json.RawMessage(nullJSON), ToolCalls: []toolCallDelta{header}, Refusal: json.RawMessage(nullJSON)}, nil)
		} else {
			add(delta{ToolCalls: []toolCallDelta{header}}, nil)
		}
		for _, piece := range tc.pieces {
			add(delta{ToolCalls: []toolCallDelta{{Index: i, Function: functionCall{Arguments: piece}}}}, nil)
		}
	}
	finish := cp.finish
	add(delta{}, &finish)
	return out
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Only the fixed wire types above are marshaled.
		panic("fakeupstream: encoding a response: " + err.Error())
	}
	return b
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
