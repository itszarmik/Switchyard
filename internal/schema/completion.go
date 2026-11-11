package schema

import (
	"encoding/json"
	"fmt"
)

// Legacy completion fields the gateway reads, validates or translates.
const (
	fieldPrompt = "prompt"
	fieldEcho   = "echo"
	fieldSuffix = "suffix"
	fieldBestOf = "best_of"
	fieldUser   = "user"
	fieldStop   = "stop"
)

// CodeUnsupportedParameter marks a parameter the gateway cannot honor on
// the deployment that serves the request, such as echo through the chat
// shim.
const CodeUnsupportedParameter = "unsupported_parameter"

// CompletionRequest is a legacy /v1/completions request body. Create it
// with DecodeCompletionRequest. Like ChatRequest, it keeps every member raw
// so the native path can forward it losslessly.
type CompletionRequest struct {
	obj     *object
	model   string
	stream  bool
	prompts []string
}

// DecodeCompletionRequest parses and validates a legacy completion request:
// model as for chat, prompt a string or an array of strings (token arrays
// are rejected), and stream a boolean if present. Every other field is left
// for the upstream, or the chat shim, to handle. A failure is always a
// *RequestError.
func DecodeCompletionRequest(data []byte) (*CompletionRequest, error) {
	obj, err := parseObject(data)
	if err != nil {
		return nil, invalidBody(err)
	}
	model, rerr := decodeModel(obj)
	if rerr != nil {
		return nil, rerr
	}
	r := &CompletionRequest{obj: obj, model: model}

	raw, ok := obj.get(fieldPrompt)
	if !ok || isNull(raw) {
		return nil, missing(fieldPrompt)
	}
	var one string
	var many []string
	switch {
	case json.Unmarshal(raw, &one) == nil:
		r.prompts = []string{one}
	case json.Unmarshal(raw, &many) == nil:
		if len(many) == 0 {
			return nil, &RequestError{
				Param: fieldPrompt, Code: CodeEmptyArray,
				Message: "Invalid 'prompt': empty array. Expected a string or an array of strings with minimum length 1.",
			}
		}
		r.prompts = many
	default:
		return nil, &RequestError{
			Param: fieldPrompt, Code: CodeInvalidType,
			Message: fmt.Sprintf("Invalid type for 'prompt': expected a string or an array of strings, but got %s instead. Token arrays are not supported.",
				describePrompt(raw)),
		}
	}

	if raw, ok := obj.get(fieldStream); ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &r.stream); err != nil {
			return nil, invalidType(fieldStream, "a boolean", raw)
		}
	}
	return r, nil
}

// describePrompt names the JSON type of an invalid prompt; an array that is
// not all strings is called a token array when it holds numbers.
func describePrompt(raw json.RawMessage) string {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		for _, v := range arr {
			if k := jsonKind(v); k != "a string" {
				return "an array containing " + k
			}
		}
	}
	return jsonKind(raw)
}

// Model returns the requested model, as the client sent it.
func (r *CompletionRequest) Model() string { return r.model }

// Stream reports whether the client asked for a streamed response.
func (r *CompletionRequest) Stream() bool { return r.stream }

// Prompts returns the prompt as a list: one entry for a string prompt.
func (r *CompletionRequest) Prompts() []string { return r.prompts }

// IncludeUsage reports whether the client set stream_options.include_usage
// to true.
func (r *CompletionRequest) IncludeUsage() bool {
	so, ok := decodeStreamOptions(r.obj.get(fieldStreamOptions))
	return ok && so.IncludeUsage
}

// MaxTokens returns max_tokens, and false if it is not an integer.
func (r *CompletionRequest) MaxTokens() (int64, bool) {
	return decodeField[int64](r.obj.get(fieldMaxTokens))
}

// Field returns the raw value of any top-level field, and whether it is
// present.
func (r *CompletionRequest) Field(name string) (json.RawMessage, bool) {
	return r.obj.get(name)
}

// RequiredCapabilities returns the capabilities a deployment needs to serve
// r natively: streaming, n > 1, seed, sampling, temperature_2, penalties,
// and logprobs when the legacy integer logprobs is set.
func (r *CompletionRequest) RequiredCapabilities() CapabilitySet {
	s := CapabilitySet{}
	add := func(c Capability, cond bool) {
		if cond {
			s[c] = struct{}{}
		}
	}
	_, ok := decodeField[int64](r.obj.get(fieldLogprobs))
	add(CapLogprobs, ok)
	add(CapStreaming, r.stream)
	addCommon(r.obj, add)
	return s
}

// ShimError returns why the chat shim cannot serve r, or nil if it can:
// echo or a non-empty suffix, logprobs, best_of above 1, or more than one
// prompt. Each has no Chat Completions equivalent.
func (r *CompletionRequest) ShimError() *RequestError {
	if isTrue(r.obj, fieldEcho) {
		return unsupported(fieldEcho, "echo is not supported for this model")
	}
	if s, ok := decodeField[string](r.obj.get(fieldSuffix)); ok && s != "" {
		return unsupported(fieldSuffix, "suffix is not supported for this model")
	}
	if _, ok := decodeField[int64](r.obj.get(fieldLogprobs)); ok {
		return unsupported(fieldLogprobs, "logprobs is not supported for this model")
	}
	if n, ok := intField(r.obj, fieldBestOf); ok && n > 1 {
		return unsupported(fieldBestOf, "best_of above 1 is not supported for this model")
	}
	if len(r.prompts) > 1 {
		return unsupported(fieldPrompt, "a prompt array with more than one entry is not supported for this model; send one prompt per request")
	}
	return nil
}

func unsupported(param, why string) *RequestError {
	return &RequestError{
		Param: param, Code: CodeUnsupportedParameter,
		Message: fmt.Sprintf("Unsupported parameter: '%s': %s, which is served through the chat completions shim.", param, why),
	}
}

// shimFields are the legacy fields the chat shim carries over unchanged.
var shimFields = []string{
	fieldMaxTokens, fieldTemperature, fieldTopP, fieldStop, fieldN, fieldStream,
	fieldStreamOptions, fieldUser, fieldSeed, fieldFrequencyPenalty, fieldPresencePenalty,
}

// ChatRequest translates r into the Chat Completions request the shim
// sends: one user message holding the prompt, plus max_tokens,
// temperature, top_p, stop, n, stream, stream_options, user, seed and the
// penalties as the client sent them. Other legacy fields are not carried
// over. Call ShimError first; ChatRequest uses only the first prompt.
func (r *CompletionRequest) ChatRequest() (*ChatRequest, error) {
	o := &object{vals: map[string]json.RawMessage{}}
	o.set(fieldModel, encodeString(r.model))
	msg := struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{Role: "user"}
	if len(r.prompts) > 0 {
		msg.Content = r.prompts[0]
	}
	m, err := json.Marshal([]any{msg})
	if err != nil {
		return nil, fmt.Errorf("schema: encode shim messages: %w", err)
	}
	o.set(fieldMessages, m)
	for _, f := range shimFields {
		if raw, ok := r.obj.get(f); ok {
			o.set(f, raw)
		}
	}
	return DecodeChatRequest(o.encode())
}

// Encode returns the body for a native upstream, as ChatRequest.EncodeFor
// does: lossless, with model replaced and, when injectUsage is set and the
// request streams, stream_options.include_usage set to true.
func (r *CompletionRequest) Encode(upstreamModel string, injectUsage bool) []byte {
	return encodeRequest(r.obj, upstreamModel, r.stream && injectUsage)
}

// Without returns a copy of r with the named top-level fields removed, and
// the names of those that were present. It never removes model, prompt or
// stream.
func (r *CompletionRequest) Without(fields ...string) (*CompletionRequest, []string) {
	o, removed := without(r.obj, fields, fieldModel, fieldPrompt, fieldStream)
	if len(removed) == 0 {
		return r, nil
	}
	c := *r
	c.obj = o
	return &c, removed
}
