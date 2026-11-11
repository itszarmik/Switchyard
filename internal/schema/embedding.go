package schema

import (
	"encoding/json"
	"fmt"
)

// Embedding request fields the gateway validates.
const (
	fieldInput          = "input"
	fieldEncodingFormat = "encoding_format"
	fieldDimensions     = "dimensions"
)

// EmbeddingRequest is a /v1/embeddings request body. Create it with
// DecodeEmbeddingRequest. It keeps every member raw so it can be forwarded
// losslessly.
type EmbeddingRequest struct {
	obj   *object
	model string
	// inputSizes is the size of each input: bytes for text, the token
	// count for a token array.
	inputSizes []inputSize
}

type inputSize struct {
	n      int
	tokens bool
}

// DecodeEmbeddingRequest parses and validates an embeddings request: model
// as for chat; input a string, an array of strings, an array of token
// integers or an array of token arrays (token arrays are passed through);
// encoding_format float or base64; dimensions a positive integer; and user
// a string. Every other field is left for the upstream. A failure is always
// a *RequestError.
func DecodeEmbeddingRequest(data []byte) (*EmbeddingRequest, error) {
	obj, err := parseObject(data)
	if err != nil {
		return nil, invalidBody(err)
	}
	model, rerr := decodeModel(obj)
	if rerr != nil {
		return nil, rerr
	}
	r := &EmbeddingRequest{obj: obj, model: model}

	raw, ok := obj.get(fieldInput)
	if !ok || isNull(raw) {
		return nil, missing(fieldInput)
	}
	sizes, ok := decodeInput(raw)
	if !ok {
		return nil, &RequestError{
			Param: fieldInput, Code: CodeInvalidType,
			Message: fmt.Sprintf("Invalid type for 'input': expected a string, an array of strings, an array of integers or an array of integer arrays, but got %s instead.", describePrompt(raw)),
		}
	}
	if len(sizes) == 0 {
		return nil, &RequestError{
			Param: fieldInput, Code: CodeEmptyArray,
			Message: "Invalid 'input': empty array. Expected an array with minimum length 1, but got an empty array instead.",
		}
	}
	r.inputSizes = sizes

	if raw, ok := obj.get(fieldEncodingFormat); ok && !isNull(raw) {
		var f string
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, invalidType(fieldEncodingFormat, "a string", raw)
		}
		if f != "float" && f != "base64" {
			return nil, &RequestError{
				Param: fieldEncodingFormat, Code: CodeInvalidValue,
				Message: "Invalid 'encoding_format': expected one of float or base64.",
			}
		}
	}
	if raw, ok := obj.get(fieldDimensions); ok && !isNull(raw) {
		d, ok := intField(obj, fieldDimensions)
		if !ok {
			return nil, invalidType(fieldDimensions, "an integer", raw)
		}
		if d < 1 {
			return nil, &RequestError{
				Param: fieldDimensions, Code: CodeInvalidValue,
				Message: fmt.Sprintf("Invalid 'dimensions': integer below minimum value. Expected a value >= 1, but got %d instead.", d),
			}
		}
	}
	if raw, ok := obj.get(fieldUser); ok && !isNull(raw) {
		var u string
		if err := json.Unmarshal(raw, &u); err != nil {
			return nil, invalidType(fieldUser, "a string", raw)
		}
	}
	return r, nil
}

// decodeInput returns the size of each input, and false if raw is not a
// valid input value.
func decodeInput(raw json.RawMessage) ([]inputSize, bool) {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []inputSize{{n: len(one)}}, true
	}
	var texts []string
	if json.Unmarshal(raw, &texts) == nil {
		out := make([]inputSize, len(texts))
		for i, t := range texts {
			out[i] = inputSize{n: len(t)}
		}
		return out, true
	}
	var tokens []int64
	if json.Unmarshal(raw, &tokens) == nil {
		if len(tokens) == 0 {
			return nil, true
		}
		return []inputSize{{n: len(tokens), tokens: true}}, true
	}
	var batches [][]int64
	if json.Unmarshal(raw, &batches) == nil {
		out := make([]inputSize, len(batches))
		for i, b := range batches {
			out[i] = inputSize{n: len(b), tokens: true}
		}
		return out, true
	}
	return nil, false
}

// Model returns the requested model, as the client sent it.
func (r *EmbeddingRequest) Model() string { return r.model }

// Inputs returns the number of inputs.
func (r *EmbeddingRequest) Inputs() int { return len(r.inputSizes) }

// LargestInputTokens estimates the token count of the largest input: the
// length of a token array, or bytes/4 × ratio for text, and at least 1.
func (r *EmbeddingRequest) LargestInputTokens(ratio float64) int64 {
	var most int64 = 1
	for _, s := range r.inputSizes {
		n := int64(s.n)
		if !s.tokens {
			n = int64(float64(s.n) / 4 * ratio)
		}
		most = max(most, n)
	}
	return most
}

// Field returns the raw value of any top-level field, and whether it is
// present.
func (r *EmbeddingRequest) Field(name string) (json.RawMessage, bool) {
	return r.obj.get(name)
}

// Encode returns the body to send upstream: the client's object, lossless
// and in order, with model replaced by upstreamModel.
func (r *EmbeddingRequest) Encode(upstreamModel string) []byte {
	return encodeRequest(r.obj, upstreamModel, false)
}
