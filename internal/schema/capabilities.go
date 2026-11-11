package schema

import (
	"encoding/json"
	"slices"
	"strings"
)

// Capability is a feature a deployment must support to serve a request.
type Capability string

// The capabilities RequiredCapabilities can report.
const (
	// CapTools: the request defines tools (or legacy functions).
	CapTools Capability = "tools"
	// CapVision: a message has an image_url content part.
	CapVision Capability = "vision"
	// CapJSONMode: response_format is json_object.
	CapJSONMode Capability = "json_mode"
	// CapJSONSchema: response_format is json_schema. A deployment with it
	// supports native structured output; it is never emulated.
	CapJSONSchema Capability = "json_schema"
	// CapN: more than one choice is requested.
	CapN Capability = "n"
	// CapLogprobs: logprobs is true.
	CapLogprobs Capability = "logprobs"
	// CapSeed: a seed is set.
	CapSeed Capability = "seed"
	// CapStreaming: the response is streamed.
	CapStreaming Capability = "streaming"
)

// CapabilitySet is a set of capabilities.
type CapabilitySet map[Capability]struct{}

// Has reports whether c is in the set.
func (s CapabilitySet) Has(c Capability) bool {
	_, ok := s[c]
	return ok
}

// Sorted returns the capabilities in lexical order.
func (s CapabilitySet) Sorted() []Capability {
	out := make([]Capability, 0, len(s))
	for c := range s {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// RequiredCapabilities returns the capabilities a deployment needs to serve
// r. Values of the wrong type are ignored, since the upstream rejects them.
//
// Besides the features a request uses (tools, images, response formats,
// n > 1, logprobs, seed, streaming), it reports: sampling when temperature
// or top_p is set; temperature_2 when temperature is above 1; penalties
// for a non-zero frequency_penalty or presence_penalty; parallel_tools when
// parallel_tool_calls is explicitly true; and forced_tools when tool_choice
// is "required" or an object naming a function. stream_usage is never
// required: it only decides whether the gateway asks for usage.
func (r *ChatRequest) RequiredCapabilities() CapabilitySet {
	s := CapabilitySet{}
	add := func(c Capability, cond bool) {
		if cond {
			s[c] = struct{}{}
		}
	}
	add(CapTools, len(r.Tools()) > 0 || r.hasFunctions())
	add(CapVision, r.hasImage())
	if rf, ok := r.ResponseFormat(); ok {
		add(CapJSONMode, rf.Type == "json_object")
		add(CapJSONSchema, rf.Type == "json_schema")
	}
	add(CapLogprobs, r.Logprobs())
	add(CapParallelTools, isTrue(r.obj, fieldParallelToolCalls))
	add(CapForcedTools, forcesTool(r.ToolChoice()))
	add(CapStreaming, r.Stream())
	addCommon(r.obj, add)
	return s
}

// addCommon reports the capabilities that chat and legacy completion
// requests share: n > 1, seed, sampling, temperature_2 and penalties.
func addCommon(o *object, add func(Capability, bool)) {
	n, ok := intField(o, fieldN)
	add(CapN, ok && n > 1)
	_, ok = decodeField[int64](o.get(fieldSeed))
	add(CapSeed, ok)
	temp, hasTemp := decodeField[float64](o.get(fieldTemperature))
	_, hasTopP := decodeField[float64](o.get(fieldTopP))
	add(CapSampling, hasTemp || hasTopP)
	add(CapTemperature2, hasTemp && temp > 1)
	fp, _ := decodeField[float64](o.get(fieldFrequencyPenalty))
	pp, _ := decodeField[float64](o.get(fieldPresencePenalty))
	add(CapPenalties, fp != 0 || pp != 0)
}

// isTrue reports whether field is the JSON literal true.
func isTrue(o *object, field string) bool {
	v, _ := decodeField[bool](o.get(field))
	return v
}

// forcesTool reports whether a tool_choice value forces a tool call:
// "required", or an object such as {"type":"function","function":{…}}.
func forcesTool(raw json.RawMessage) bool {
	if raw == nil || isNull(raw) {
		return false
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str == "required"
	}
	return jsonKind(raw) == "an object"
}

// hasFunctions reports whether the deprecated functions array is non-empty.
func (r *ChatRequest) hasFunctions() bool {
	raw, ok := r.obj.get(fieldFunctions)
	fns, _ := decodeField[[]json.RawMessage](raw, ok)
	return len(fns) > 0
}

// hasImage reports whether any message has a content part of type
// image_url.
func (r *ChatRequest) hasImage() bool {
	n, _ := r.ImageParts()
	return n > 0
}

// ImageParts returns the number of content parts of type image_url across
// the messages, and the total length in bytes of their data: URLs, the
// inline image data that is part of the body. The type member is matched
// exactly (see members).
func (r *ChatRequest) ImageParts() (count, inlineBytes int) {
	for _, m := range r.messages {
		if jsonKind(m.Content) != "an array" {
			continue // a string or null: no parts, and no decoding error to build
		}
		parts, ok := decodeField[[]json.RawMessage](m.Content, m.Content != nil)
		if !ok {
			continue
		}
		for _, raw := range parts {
			p, ok := members(raw)
			if !ok {
				continue
			}
			if typ, _ := member[string](p, "type"); typ != "image_url" {
				continue
			}
			count++
			img, _ := members(p["image_url"])
			if url, _ := member[string](img, "url"); strings.HasPrefix(url, "data:") {
				inlineBytes += len(url)
			}
		}
	}
	return count, inlineBytes
}
