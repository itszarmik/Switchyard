package fakegemini

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"switchyard/test/fakeupstream"
)

// MaxNonStreamingOutput is the largest maxOutputTokens the fake accepts on
// generateContent, as Google does for Gemini 4 Carbon; streams may ask for
// more.
const MaxNonStreamingOutput = 32768

// skipSignatures are the values Google documents for skipping the thought
// signature check.
var skipSignatures = map[string]bool{
	"skip_thought_signature_validator":     true,
	"context_engineering_is_the_way_to_go": true,
}

// topLevelFields are the only request fields the gateway may send. The real
// API accepts more (safetySettings, cachedContent, serviceTier, …), which
// the adapter must never send, so the fake rejects them loudly.
var topLevelFields = map[string]bool{
	"contents": true, "systemInstruction": true, "tools": true, "toolConfig": true,
	"generationConfig": true, "store": true,
}

// generationFields are the generationConfig fields the adapter maps.
// Sampling (temperature, topP, topK), candidateCount, seed, penalties,
// logprobs and anything else fail loudly.
var generationFields = map[string]bool{
	"stopSequences": true, "maxOutputTokens": true, "responseMimeType": true,
	"responseJsonSchema": true, "thinkingConfig": true,
}

// thinkingLevels are the levels current models accept: minimal returns an
// error on Gemini 3.8 Flash and Gemini 4.
var thinkingLevels = map[string]bool{"LOW": true, "MEDIUM": true, "HIGH": true, "XHIGH": true}

// apiError is a request the fake rejects.
type apiError struct {
	status  int
	message string
}

func invalid(format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, message: fmt.Sprintf(format, args...)}
}

// generateRequest is what the fake reads from a request.
type generateRequest struct {
	model       string
	stream      bool
	stops       []string
	tools       []string // declared function names, in order
	mode        string   // "", AUTO, ANY or NONE
	allowed     []string
	json        bool // responseMimeType application/json
	promptBytes int
}

// wirePart is a request part, decoded for validation.
type wirePart struct {
	Text       *string `json:"text"`
	InlineData *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData"`
	FunctionCall *struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall"`
	FunctionResponse *struct {
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
	} `json:"functionResponse"`
	ThoughtSignature *string `json:"thoughtSignature"`
}

type wireContent struct {
	Role  string     `json:"role"`
	Parts []wirePart `json:"parts"`
}

// strictDecode decodes raw into v, rejecting unknown fields.
func strictDecode(raw json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

func isObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil
}

// parseRequest decodes and validates body the way the Gemini API does for
// the fields the fake knows, and more strictly for what the gateway must
// never send.
func parseRequest(body []byte, model string, stream bool) (generateRequest, *apiError) {
	req := generateRequest{model: model, stream: stream}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return req, invalid("Invalid JSON payload received.")
	}
	for _, k := range slices.Sorted(maps.Keys(raw)) {
		if !topLevelFields[k] {
			return req, invalid("fakegemini: the gateway must not send the field %q", k)
		}
	}
	if string(bytes.TrimSpace(raw["store"])) != "false" {
		return req, invalid(`fakegemini: every request must send "store": false`)
	}

	var contents []wireContent
	if err := strictDecode(raw["contents"], &contents); err != nil {
		return req, invalid("Invalid value at 'contents': %v", err)
	}
	if len(contents) == 0 {
		return req, invalid("contents is not specified")
	}
	req.promptBytes = len(raw["contents"])
	if aerr := validateContents(contents, model); aerr != nil {
		return req, aerr
	}

	if v, ok := raw["systemInstruction"]; ok {
		var si wireContent
		if err := strictDecode(v, &si); err != nil || len(si.Parts) == 0 || si.Role != "" {
			return req, invalid("Invalid value at 'system_instruction': a content of text parts, without a role, is required")
		}
		for i, p := range si.Parts {
			if p.Text == nil || *p.Text == "" || p.InlineData != nil || p.FunctionCall != nil || p.FunctionResponse != nil {
				return req, invalid("system_instruction.parts[%d]: only non-empty text is supported", i)
			}
		}
	}

	if v, ok := raw["tools"]; ok {
		var tools []struct {
			FunctionDeclarations []struct {
				Name                 string          `json:"name"`
				Description          string          `json:"description"`
				ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
			} `json:"functionDeclarations"`
		}
		if err := strictDecode(v, &tools); err != nil {
			return req, invalid("Invalid value at 'tools': %v", err)
		}
		for i, t := range tools {
			for j, d := range t.FunctionDeclarations {
				if d.Name == "" {
					return req, invalid("tools[%d].function_declarations[%d].name: Name cannot be empty.", i, j)
				}
				if d.ParametersJSONSchema != nil && !isObject(d.ParametersJSONSchema) {
					return req, invalid("tools[%d].function_declarations[%d].parameters_json_schema must be an object", i, j)
				}
				req.tools = append(req.tools, d.Name)
			}
		}
	}
	if v, ok := raw["toolConfig"]; ok {
		var tc struct {
			FunctionCallingConfig struct {
				Mode                 string   `json:"mode"`
				AllowedFunctionNames []string `json:"allowedFunctionNames"`
			} `json:"functionCallingConfig"`
		}
		if err := strictDecode(v, &tc); err != nil {
			return req, invalid("Invalid value at 'tool_config': %v", err)
		}
		fc := tc.FunctionCallingConfig
		switch fc.Mode {
		case "AUTO", "NONE", "ANY":
		default:
			return req, invalid("Invalid value at 'tool_config.function_calling_config.mode' (%q)", fc.Mode)
		}
		if len(fc.AllowedFunctionNames) > 0 && fc.Mode != "ANY" {
			return req, invalid("allowed_function_names is only supported with mode ANY")
		}
		for _, n := range fc.AllowedFunctionNames {
			if !slices.Contains(req.tools, n) {
				return req, invalid("allowed_function_names: %q is not declared", n)
			}
		}
		req.mode, req.allowed = fc.Mode, fc.AllowedFunctionNames
	}

	if v, ok := raw["generationConfig"]; ok {
		if aerr := req.parseGenerationConfig(v); aerr != nil {
			return req, aerr
		}
	}
	return req, nil
}

func (req *generateRequest) parseGenerationConfig(v json.RawMessage) *apiError {
	var gc map[string]json.RawMessage
	if err := json.Unmarshal(v, &gc); err != nil || gc == nil {
		return invalid("Invalid value at 'generation_config'")
	}
	for _, k := range slices.Sorted(maps.Keys(gc)) {
		if !generationFields[k] {
			return invalid("fakegemini: the gateway must not send generationConfig.%s", k)
		}
	}
	if s, ok := gc["stopSequences"]; ok {
		if json.Unmarshal(s, &req.stops) != nil || len(req.stops) > 5 {
			return invalid("stop_sequences: at most 5 strings are allowed")
		}
	}
	if m, ok := gc["maxOutputTokens"]; ok {
		var n int64
		if json.Unmarshal(m, &n) != nil || n <= 0 {
			return invalid("max_output_tokens must be a positive integer")
		}
		if !req.stream && n > MaxNonStreamingOutput {
			return invalid("max_output_tokens (%d) exceeds the limit of %d for non-streaming requests; use streaming", n, MaxNonStreamingOutput)
		}
	}
	var mime string
	if m, ok := gc["responseMimeType"]; ok {
		if json.Unmarshal(m, &mime) != nil || (mime != "text/plain" && mime != "application/json") {
			return invalid("response_mime_type: unsupported value")
		}
		req.json = mime == "application/json"
	}
	if s, ok := gc["responseJsonSchema"]; ok && (mime != "application/json" || !isObject(s)) {
		return invalid("response_json_schema needs response_mime_type application/json and an object schema")
	}
	if t, ok := gc["thinkingConfig"]; ok {
		var tc struct {
			ThinkingLevel string `json:"thinkingLevel"`
		}
		if err := strictDecode(t, &tc); err != nil {
			return invalid("fakegemini: thinkingConfig may hold only thinkingLevel: %v", err)
		}
		if !thinkingLevels[tc.ThinkingLevel] {
			return invalid("thinking_level %q is not supported for this model", tc.ThinkingLevel)
		}
	}
	return nil
}

// validateContents checks the conversation: roles, parts, function
// responses that answer exactly the calls before them, no prefilled model
// turn, and thought signatures (see checkSignatures).
func validateContents(contents []wireContent, model string) *apiError {
	for i, c := range contents {
		if c.Role != "user" && c.Role != "model" {
			return invalid("contents[%d].role must be user or model", i)
		}
		if len(c.Parts) == 0 {
			return invalid("contents[%d].parts must not be empty", i)
		}
		responses := 0
		for j, p := range c.Parts {
			at := fmt.Sprintf("contents[%d].parts[%d]", i, j)
			n := 0
			for _, set := range []bool{p.Text != nil, p.InlineData != nil, p.FunctionCall != nil, p.FunctionResponse != nil} {
				if set {
					n++
				}
			}
			if n != 1 {
				return invalid("%s must have exactly one of text, inline_data, function_call and function_response", at)
			}
			if p.ThoughtSignature != nil && p.FunctionCall == nil {
				return invalid("fakegemini: %s: the gateway sends thought signatures only on function calls", at)
			}
			switch {
			case p.Text != nil && *p.Text == "":
				return invalid("%s.text must not be empty", at)
			case p.InlineData != nil:
				if _, err := base64.StdEncoding.DecodeString(p.InlineData.Data); err != nil || p.InlineData.MimeType == "" || c.Role != "user" {
					return invalid("%s.inline_data needs a mime_type and base64 data, in a user content", at)
				}
			case p.FunctionCall != nil:
				if c.Role != "model" || p.FunctionCall.Name == "" || !isObject(p.FunctionCall.Args) {
					return invalid("%s.function_call needs a name and an args object, in a model content", at)
				}
			case p.FunctionResponse != nil:
				fr := p.FunctionResponse
				if c.Role != "user" || fr.ID == "" || fr.Name == "" || !isObject(fr.Response) {
					return invalid("%s.function_response needs an id, a name and a response object, in a user content", at)
				}
				responses++
			}
		}
		if responses > 0 {
			if responses != len(c.Parts) {
				return invalid("fakegemini: contents[%d] mixes function responses with other parts", i)
			}
			if aerr := checkResponses(contents, i); aerr != nil {
				return aerr
			}
		}
	}
	if contents[len(contents)-1].Role == "model" {
		return invalid("Please ensure that the last content is a user turn: prefilled model turns are not supported.")
	}
	return checkSignatures(contents, model)
}

// checkResponses checks that contents[i], a content of function
// responses, answers exactly the function calls of the model content just
// before it, by id and name. Google rejects interleaved calls and
// responses (FC1, FR1, FC2, FR2).
func checkResponses(contents []wireContent, i int) *apiError {
	if i == 0 || contents[i-1].Role != "model" {
		return invalid("contents[%d]: function responses must follow the model turn with the function calls", i)
	}
	calls := map[string]string{}
	for _, p := range contents[i-1].Parts {
		if p.FunctionCall != nil {
			calls[p.FunctionCall.ID] = p.FunctionCall.Name
		}
	}
	if len(calls) != len(contents[i].Parts) {
		return invalid("contents[%d]: the number of function responses (%d) does not match the number of function calls (%d)", i, len(contents[i].Parts), len(calls))
	}
	for j, p := range contents[i].Parts {
		if name, ok := calls[p.FunctionResponse.ID]; !ok || name != p.FunctionResponse.Name {
			return invalid("contents[%d].parts[%d]: function response %q answers no function call of the previous turn", i, j, p.FunctionResponse.ID)
		}
	}
	return nil
}

// checkSignatures validates thought signatures as Google documents: in
// the current turn (every model content after the last user content with
// text or images), the first function call of each step must carry a
// signature. A signature must be one the fake issued for that call and
// model (see Signature), or a documented skip value; the fake checks that
// in every turn, and fails loudly on a signature on any call but the
// first of its step.
func checkSignatures(contents []wireContent, model string) *apiError {
	current := 0
	for i, c := range contents {
		if c.Role != "user" {
			continue
		}
		for _, p := range c.Parts {
			if p.Text != nil || p.InlineData != nil {
				current = i + 1
				break
			}
		}
	}
	for i, c := range contents {
		if c.Role != "model" {
			continue
		}
		first := true
		for j, p := range c.Parts {
			if p.FunctionCall == nil {
				continue
			}
			sig := ""
			if p.ThoughtSignature != nil {
				sig = *p.ThoughtSignature
			}
			switch {
			case !first && sig != "":
				return invalid("fakegemini: contents[%d].parts[%d]: a thought signature on a function call that is not the first of its step", i, j)
			case first && sig == "" && i >= current:
				return invalid("Function call is missing a thought_signature in functionCall parts. This is required for tools to work correctly, and missing thought_signature may lead to degraded model performance. Additional data, function call `%s` , position %d.", p.FunctionCall.Name, j+1)
			case sig != "" && !skipSignatures[sig] && sig != Signature(model, p.FunctionCall.ID) && sig != Signature(model, ""):
				return invalid("Corrupted thought signature.")
			}
			first = false
		}
	}
	return nil
}

// plan is the canned answer to one request.
type plan struct {
	id     string
	model  string
	calls  []callPlan
	text   string
	pieces []string
	finish string
	block  string
	usage  Usage
	noIDs  bool
}

type callPlan struct {
	id, name, args string
}

// newPlan builds the answer to req; id is its responseId.
func newPlan(req generateRequest, id string, b Behavior) *plan {
	p := &plan{id: id, model: req.model, finish: "STOP", block: b.BlockReason, noIDs: b.NoCallIDs}
	var calls []string
	switch {
	case len(req.tools) == 0 || req.mode == "NONE":
	case len(req.allowed) > 0:
		calls = req.allowed
	default:
		calls = req.tools
	}
	output := 0
	switch {
	case len(calls) > 0:
		for i, name := range calls {
			p.calls = append(p.calls, callPlan{id: fakeupstream.ToolCallID(0, i), name: name, args: fakeupstream.ToolArguments(i)})
		}
		output = 10 * len(calls)
	case req.json:
		p.text = fakeupstream.JSONText
		p.pieces = splitBytes(p.text, 8)
		output = len(p.pieces)
	default:
		p.text = applyStop(fakeupstream.Text, req.stops)
		p.pieces = splitWords(p.text)
		output = len(p.pieces)
	}
	if b.FinishReason != "" {
		p.finish = b.FinishReason
	}
	p.usage = Usage{Prompt: max(1, req.promptBytes/4), Candidates: max(1, output)}
	if b.Usage != nil {
		p.usage = *b.Usage
	}
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

// Wire types of the response, in the API's field order.
type (
	outPart struct {
		Text             *string  `json:"text,omitempty"`
		FunctionCall     *outCall `json:"functionCall,omitempty"`
		ThoughtSignature string   `json:"thoughtSignature,omitempty"`
	}
	outCall struct {
		ID   string          `json:"id,omitempty"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	outContent struct {
		Parts []outPart `json:"parts"`
		Role  string    `json:"role"`
	}
	outCandidate struct {
		Content      *outContent `json:"content,omitempty"`
		FinishReason string      `json:"finishReason,omitempty"`
		Index        int         `json:"index"`
	}
	outUsage struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount,omitempty"`
		TotalTokenCount         int `json:"totalTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
		ToolUsePromptTokenCount int `json:"toolUsePromptTokenCount,omitempty"`
		ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
	}
	outResponse struct {
		Candidates     []outCandidate `json:"candidates,omitempty"`
		PromptFeedback *struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback,omitempty"`
		UsageMetadata outUsage `json:"usageMetadata"`
		ModelVersion  string   `json:"modelVersion"`
		ResponseID    string   `json:"responseId"`
	}
)

// wireUsage is the usage after candidates output tokens.
func (p *plan) wireUsage(candidates int) outUsage {
	u := p.usage
	return outUsage{
		PromptTokenCount: u.Prompt, CandidatesTokenCount: candidates, CachedContentTokenCount: u.Cached,
		ToolUsePromptTokenCount: u.ToolUsePrompt, ThoughtsTokenCount: u.Thoughts,
		TotalTokenCount: u.Prompt + candidates + u.ToolUsePrompt + u.Thoughts,
	}
}

func (p *plan) callParts() []outPart {
	var out []outPart
	for i, c := range p.calls {
		op := outPart{FunctionCall: &outCall{ID: c.id, Name: c.name, Args: json.RawMessage(c.args)}}
		if p.noIDs {
			op.FunctionCall.ID = ""
		}
		if i == 0 {
			// A call without an id gets a signature bound to the model
			// only: its id is the client's to make up.
			op.ThoughtSignature = Signature(p.model, op.FunctionCall.ID)
		}
		out = append(out, op)
	}
	return out
}

// signaturePart is the empty text part with a signature that ends a
// thinking model's answer without function calls.
func (p *plan) signaturePart() outPart {
	empty := ""
	return outPart{Text: &empty, ThoughtSignature: TextSignature(p.model)}
}

func (p *plan) envelope(cands []outCandidate, candidates int) outResponse {
	r := outResponse{Candidates: cands, UsageMetadata: p.wireUsage(candidates), ModelVersion: p.model, ResponseID: p.id}
	if p.block != "" {
		r.Candidates = nil
		r.PromptFeedback = &struct {
			BlockReason string `json:"blockReason"`
		}{p.block}
		r.UsageMetadata = outUsage{PromptTokenCount: p.usage.Prompt, TotalTokenCount: p.usage.Prompt}
	}
	return r
}

// response encodes p as one GenerateContentResponse.
func (p *plan) response(extra map[string]json.RawMessage) []byte {
	var parts []outPart
	if len(p.calls) > 0 {
		parts = p.callParts()
	} else {
		text := p.text
		if text != "" {
			parts = append(parts, outPart{Text: &text})
		}
		parts = append(parts, p.signaturePart())
	}
	cand := outCandidate{Content: &outContent{Parts: parts, Role: "model"}, FinishReason: p.finish}
	return withExtra(mustMarshal(p.envelope([]outCandidate{cand}, p.usage.Candidates)), extra)
}

// events encodes p as stream events: one per text piece, the finish
// reason and the signature-only part on the last, or one event with every
// function call; usage is cumulative.
func (p *plan) events(extra map[string]json.RawMessage) [][]byte {
	add := func(out [][]byte, r outResponse) [][]byte { return append(out, withExtra(mustMarshal(r), extra)) }
	if p.block != "" || len(p.calls) > 0 {
		cand := outCandidate{Content: &outContent{Parts: p.callParts(), Role: "model"}, FinishReason: p.finish}
		return add(nil, p.envelope([]outCandidate{cand}, p.usage.Candidates))
	}
	var out [][]byte
	n := len(p.pieces)
	for i, piece := range p.pieces {
		text := piece
		cand := outCandidate{Content: &outContent{Parts: []outPart{{Text: &text}}, Role: "model"}}
		candidates := max(1, p.usage.Candidates*(i+1)/n)
		if i == n-1 {
			cand.Content.Parts = append(cand.Content.Parts, p.signaturePart())
			cand.FinishReason = p.finish
			candidates = p.usage.Candidates
		}
		out = add(out, p.envelope([]outCandidate{cand}, candidates))
	}
	if n == 0 {
		cand := outCandidate{Content: &outContent{Parts: []outPart{p.signaturePart()}, Role: "model"}, FinishReason: p.finish}
		out = add(out, p.envelope([]outCandidate{cand}, p.usage.Candidates))
	}
	return out
}

// mustMarshal encodes v without HTML escaping.
func mustMarshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// Only the fixed wire types above are marshaled.
		panic("fakegemini: encoding a response: " + err.Error())
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
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		buf.WriteByte(',')
		buf.Write(mustMarshal(k))
		buf.WriteByte(':')
		buf.Write(extra[k])
	}
	buf.WriteByte('}')
	return buf.Bytes()
}
