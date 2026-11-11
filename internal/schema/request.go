// Package schema holds Switchyard's canonical, OpenAI-shaped Chat Completions
// types.
//
// A ChatRequest keeps the client's top-level JSON object member by member,
// raw, so it can be forwarded losslessly: every field, unknown ones
// included, reaches the upstream exactly as the client sent it, except for
// the edits Encode makes. Typed accessors read the few fields the gateway
// itself needs. Responses and stream chunks are parsed only for what the
// gateway uses (usage, mostly); their original bytes are kept for
// forwarding.
package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
)

// Request fields the gateway reads or edits.
const (
	fieldModel               = "model"
	fieldStream              = "stream"
	fieldStreamOptions       = "stream_options"
	fieldN                   = "n"
	fieldMessages            = "messages"
	fieldTools               = "tools"
	fieldFunctions           = "functions"
	fieldToolChoice          = "tool_choice"
	fieldResponseFormat      = "response_format"
	fieldLogprobs            = "logprobs"
	fieldSeed                = "seed"
	fieldTemperature         = "temperature"
	fieldMaxTokens           = "max_tokens"
	fieldMaxCompletionTokens = "max_completion_tokens"
	fieldTopP                = "top_p"
	fieldFrequencyPenalty    = "frequency_penalty"
	fieldPresencePenalty     = "presence_penalty"
	fieldParallelToolCalls   = "parallel_tool_calls"
)

// Error codes of RequestError, following OpenAI's validation errors.
const (
	// CodeInvalidJSON marks a body that is not one JSON object.
	CodeInvalidJSON = "invalid_json"
	// CodeMissingParameter marks a required field that is absent or null.
	CodeMissingParameter = "missing_required_parameter"
	// CodeInvalidType marks a field of the wrong JSON type.
	CodeInvalidType = "invalid_type"
	// CodeEmptyArray marks a required array that is empty.
	CodeEmptyArray = "empty_array"
	// CodeInvalidValue marks a field whose value is out of range.
	CodeInvalidValue = "invalid_value"
)

// MaxModelLength bounds the model field, in bytes. The model string is
// logged and echoed in errors, so a client must not be able to make it
// arbitrarily large.
const MaxModelLength = 256

// RequestError describes why a request body was rejected. It maps onto the
// OpenAI error envelope with type invalid_request_error and status 400.
type RequestError struct {
	// Param is the offending top-level field, or "" when the body as a whole
	// is invalid.
	Param string
	// Code is one of the Code* constants.
	Code string
	// Message is safe to return to the client; it never quotes the body.
	Message string
}

// Error returns the message.
func (e *RequestError) Error() string { return e.Message }

// ChatRequest is a Chat Completions request body. Create it with
// DecodeChatRequest. It is not safe for concurrent modification, but every
// method only reads, so concurrent use is fine.
type ChatRequest struct {
	obj      *object
	model    string
	stream   bool
	messages []Message
}

// Message is one entry of the messages array. Only the role is decoded;
// the content is kept raw.
type Message struct {
	// Role is the message's role, or "" if it is missing or not a string.
	Role string
	// Content is the raw content value: a string, an array of parts, or
	// null. It is nil when the field is absent.
	Content json.RawMessage
}

// StreamOptions is the decoded stream_options object.
type StreamOptions struct {
	// IncludeUsage asks for a final chunk that carries the usage.
	IncludeUsage bool `json:"include_usage"`
}

// ResponseFormat is the decoded response_format object.
type ResponseFormat struct {
	// Type is text, json_object or json_schema.
	Type string `json:"type"`
}

// DecodeChatRequest parses and validates a Chat Completions request body.
// It checks only what the gateway depends on: the body is one JSON object,
// model is a non-empty string of at most MaxModelLength bytes, messages is
// a non-empty array, and stream,
// if present and not null, is a boolean. Every other field is left for the
// upstream to validate. A failure is always a *RequestError.
func DecodeChatRequest(data []byte) (*ChatRequest, error) {
	obj, err := parseObject(data)
	if err != nil {
		return nil, invalidBody(err)
	}
	model, rerr := decodeModel(obj)
	if rerr != nil {
		return nil, rerr
	}
	r := &ChatRequest{obj: obj, model: model}

	raw, ok := obj.get(fieldMessages)
	if !ok || isNull(raw) {
		return nil, missing(fieldMessages)
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, invalidType(fieldMessages, "an array", raw)
	}
	if len(msgs) == 0 {
		return nil, &RequestError{
			Param:   fieldMessages,
			Code:    CodeEmptyArray,
			Message: "Invalid 'messages': empty array. Expected an array with minimum length 1, but got an empty array instead.",
		}
	}
	r.messages = make([]Message, len(msgs))
	for i, m := range msgs {
		r.messages[i] = decodeMessage(m)
	}

	if raw, ok := obj.get(fieldStream); ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &r.stream); err != nil {
			return nil, invalidType(fieldStream, "a boolean", raw)
		}
	}
	return r, nil
}

// invalidBody is the error for a body that is not one JSON object.
func invalidBody(err error) *RequestError {
	msg := "We could not parse the JSON body of your request. The API expects a JSON object in the request body."
	if errors.Is(err, errNotObject) {
		msg = "The request body must be a JSON object."
	}
	return &RequestError{Code: CodeInvalidJSON, Message: msg}
}

// decodeModel reads and validates the model field: a non-empty string of
// at most MaxModelLength bytes.
func decodeModel(obj *object) (string, *RequestError) {
	raw, ok := obj.get(fieldModel)
	if !ok || isNull(raw) {
		return "", missing(fieldModel)
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		return "", invalidType(fieldModel, "a string", raw)
	}
	if model == "" {
		return "", missing(fieldModel)
	}
	if len(model) > MaxModelLength {
		// The message gives the length, never the value.
		return "", &RequestError{
			Param: fieldModel,
			Code:  CodeInvalidValue,
			Message: fmt.Sprintf("Invalid 'model': string too long. Expected a string with maximum length %d bytes, but got a string with length %d instead.",
				MaxModelLength, len(model)),
		}
	}
	return model, nil
}

func missing(param string) *RequestError {
	return &RequestError{
		Param:   param,
		Code:    CodeMissingParameter,
		Message: fmt.Sprintf("Missing required parameter: '%s'.", param),
	}
}

func invalidType(param, want string, got json.RawMessage) *RequestError {
	return &RequestError{
		Param:   param,
		Code:    CodeInvalidType,
		Message: fmt.Sprintf("Invalid type for '%s': expected %s, but got %s instead.", param, want, jsonKind(got)),
	}
}

// decodeMessage reads the role and the raw content of one message. A
// message that is not an object, or whose role is not a string, gets an
// empty role; the upstream rejects such messages itself.
func decodeMessage(raw json.RawMessage) Message {
	m, ok := members(raw)
	if !ok {
		return Message{}
	}
	role, _ := member[string](m, "role")
	return Message{Role: role, Content: m["content"]}
}

// Model returns the requested model, as the client sent it.
func (r *ChatRequest) Model() string { return r.model }

// Stream reports whether the client asked for a streamed response.
func (r *ChatRequest) Stream() bool { return r.stream }

// StreamOptions returns the decoded stream_options, and false if the field
// is absent, null or not an object of the expected shape.
func (r *ChatRequest) StreamOptions() (StreamOptions, bool) {
	return decodeStreamOptions(r.obj.get(fieldStreamOptions))
}

// decodeStreamOptions decodes a present stream_options value, matching
// member names exactly. It reports false if the value is absent, null, not
// an object, or has an include_usage that is neither a boolean nor null.
func decodeStreamOptions(raw json.RawMessage, present bool) (StreamOptions, bool) {
	m, ok := members(raw)
	if !present || !ok {
		return StreamOptions{}, false
	}
	v, ok := m["include_usage"]
	if !ok || isNull(v) {
		return StreamOptions{}, true
	}
	inc, ok := decodeField[bool](v, true)
	return StreamOptions{IncludeUsage: inc}, ok
}

// IncludeUsage reports whether the client itself set
// stream_options.include_usage to true.
func (r *ChatRequest) IncludeUsage() bool {
	so, ok := r.StreamOptions()
	return ok && so.IncludeUsage
}

// N returns the number of choices requested, and false if n is absent,
// null or not an integer.
func (r *ChatRequest) N() (int, bool) { return intField(r.obj, fieldN) }

// intField returns an integer field that fits an int32, and false if it is
// absent, null or not an integer.
func intField(o *object, name string) (int, bool) {
	f, ok := decodeField[float64](o.get(name))
	if !ok || f != math.Trunc(f) || f < math.MinInt32 || f > math.MaxInt32 {
		return 0, false
	}
	return int(f), true
}

// Messages returns the messages, with roles decoded and content raw. The
// slice must not be modified.
func (r *ChatRequest) Messages() []Message { return r.messages }

// Tools returns the raw entries of the tools array, or nil if it is absent,
// null or not an array.
func (r *ChatRequest) Tools() []json.RawMessage {
	raw, ok := r.obj.get(fieldTools)
	tools, _ := decodeField[[]json.RawMessage](raw, ok)
	return tools
}

// ToolChoice returns the raw tool_choice value, or nil if it is absent.
func (r *ChatRequest) ToolChoice() json.RawMessage {
	raw, _ := r.obj.get(fieldToolChoice)
	return raw
}

// ResponseFormat returns the decoded response_format, and false if it is
// absent, null or not an object of the expected shape.
// Member names are matched exactly.
func (r *ChatRequest) ResponseFormat() (ResponseFormat, bool) {
	raw, ok := r.obj.get(fieldResponseFormat)
	m, isObj := members(raw)
	if !ok || !isObj {
		return ResponseFormat{}, false
	}
	t, ok := m["type"]
	if !ok || isNull(t) {
		return ResponseFormat{}, true
	}
	typ, ok := decodeField[string](t, true)
	return ResponseFormat{Type: typ}, ok
}

// Logprobs reports whether logprobs is true.
func (r *ChatRequest) Logprobs() bool {
	raw, ok := r.obj.get(fieldLogprobs)
	v, _ := decodeField[bool](raw, ok)
	return v
}

// Seed returns the seed, and false if it is absent, null or not an integer.
func (r *ChatRequest) Seed() (int64, bool) {
	raw, ok := r.obj.get(fieldSeed)
	return decodeField[int64](raw, ok)
}

// Temperature returns the sampling temperature, and false if it is absent,
// null or not a number.
func (r *ChatRequest) Temperature() (float64, bool) {
	raw, ok := r.obj.get(fieldTemperature)
	return decodeField[float64](raw, ok)
}

// MaxTokens returns the output token cap: max_completion_tokens if it is
// set, otherwise the legacy max_tokens. It reports false if neither is an
// integer.
func (r *ChatRequest) MaxTokens() (int64, bool) {
	raw, ok := r.obj.get(fieldMaxCompletionTokens)
	if v, ok := decodeField[int64](raw, ok); ok {
		return v, true
	}
	raw, ok = r.obj.get(fieldMaxTokens)
	return decodeField[int64](raw, ok)
}

// Field returns the raw value of any top-level field, and whether it is
// present.
func (r *ChatRequest) Field(name string) (json.RawMessage, bool) {
	return r.obj.get(name)
}

// Encode returns the request body to send upstream: the client's object
// with every member intact and in its original order, except that model is
// replaced by upstreamModel and, for a streamed request,
// stream_options.include_usage is set to true. Other stream_options members
// are kept; a stream_options value that is not an object is replaced.
func (r *ChatRequest) Encode(upstreamModel string) []byte {
	return r.EncodeFor(upstreamModel, true)
}

// EncodeFor is Encode, with injectUsage deciding whether a streamed
// request gets stream_options.include_usage. Without it, stream_options is
// forwarded exactly as the client sent it (or not at all), for upstreams
// that reject the option.
func (r *ChatRequest) EncodeFor(upstreamModel string, injectUsage bool) []byte {
	return encodeRequest(r.obj, upstreamModel, r.stream && injectUsage)
}

// encodeRequest returns o with model replaced and, when injectUsage is set,
// stream_options.include_usage set to true.
func encodeRequest(obj *object, upstreamModel string, injectUsage bool) []byte {
	o := obj.clone()
	o.set(fieldModel, encodeString(upstreamModel))
	if injectUsage {
		opts := &object{vals: map[string]json.RawMessage{}}
		if raw, ok := o.get(fieldStreamOptions); ok {
			if parsed, err := parseObject(raw); err == nil {
				opts = parsed
			}
		}
		opts.set("include_usage", json.RawMessage("true"))
		o.set(fieldStreamOptions, opts.encode())
	}
	return o.encode()
}

// Without returns a copy of r with the named top-level fields removed, and
// the names of those that were present, in the order given. It never
// removes model, messages or stream. r itself is unchanged.
func (r *ChatRequest) Without(fields ...string) (*ChatRequest, []string) {
	o, removed := without(r.obj, fields, fieldModel, fieldMessages, fieldStream)
	if len(removed) == 0 {
		return r, nil
	}
	c := *r
	c.obj = o
	return &c, removed
}

// without returns a copy of obj without fields, except those in keep, and
// the fields that were present. When none was, it returns obj itself.
func without(obj *object, fields []string, keep ...string) (*object, []string) {
	var removed []string
	var o *object
	for _, f := range fields {
		if slices.Contains(keep, f) || slices.Contains(removed, f) {
			continue
		}
		if _, ok := obj.get(f); !ok {
			continue
		}
		if o == nil {
			o = obj.clone()
		}
		o.delete(f)
		removed = append(removed, f)
	}
	if o == nil {
		return obj, nil
	}
	return o, removed
}
