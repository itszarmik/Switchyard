package schema

import (
	"encoding/json"
	"slices"
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
	// CapJSONSchema: response_format is json_schema.
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
	n, ok := r.N()
	add(CapN, ok && n > 1)
	add(CapLogprobs, r.Logprobs())
	_, ok = r.Seed()
	add(CapSeed, ok)
	add(CapStreaming, r.Stream())
	return s
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
	for _, m := range r.messages {
		parts, ok := decodeField[[]struct {
			Type string `json:"type"`
		}](m.Content, m.Content != nil)
		if !ok {
			continue
		}
		for _, p := range parts {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}
