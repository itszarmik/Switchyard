package fakeanthropic

import (
	"context"
	"encoding/json"

	"switchyard/test/fakeupstream"
)

// OpenAIUpstream adapts a Server to the method set of *fakeupstream.Server
// that the gateway conformance suite uses (BaseURL, Reset, Script with a
// fakeupstream.Behavior, Requests, WaitReceived, Await), so the suite can
// run against a gateway whose deployment is Anthropic-backed. A suite that
// takes an interface with these methods accepts both fakes.
//
// Script translates the OpenAI-wire behavior with [FromOpenAI]. Recorded
// requests are Anthropic requests: their Body is a Messages API body and
// the credential is in X-Api-Key, not Authorization.
type OpenAIUpstream struct {
	s *Server
}

// OpenAI returns the conformance view of s.
func (s *Server) OpenAI() *OpenAIUpstream { return &OpenAIUpstream{s: s} }

// Server returns the underlying fake.
func (u *OpenAIUpstream) Server() *Server { return u.s }

// BaseURL is the provider base_url for the gateway: the API root.
func (u *OpenAIUpstream) BaseURL() string { return u.s.BaseURL() }

// Close closes the server.
func (u *OpenAIUpstream) Close() { u.s.Close() }

// Reset restores the default behavior and forgets the recorded requests.
func (u *OpenAIUpstream) Reset() { u.s.Reset() }

// Script sets FromOpenAI(b) as the behavior.
func (u *OpenAIUpstream) Script(b fakeupstream.Behavior) { u.s.Script(FromOpenAI(b)) }

// Requests returns a snapshot of the recorded requests, oldest first.
func (u *OpenAIUpstream) Requests() []Request { return u.s.Requests() }

// WaitReceived waits until at least n requests have been recorded.
func (u *OpenAIUpstream) WaitReceived(ctx context.Context, n int) error {
	return u.s.WaitReceived(ctx, n)
}

// Await waits until the first n requests have been handled and returns
// them.
func (u *OpenAIUpstream) Await(ctx context.Context, n int) ([]Request, error) {
	return u.s.Await(ctx, n)
}

// FromOpenAI translates a fakeupstream behavior into the equivalent
// Anthropic one. Status, Header, TTFT, ChunkDelay, Fault,
// FaultAfterChunks and ExtraFields carry over unchanged; FaultAfterChunks
// then counts Anthropic events, which are not one to one with OpenAI
// chunks. An OpenAI error envelope in Body becomes an Anthropic error
// with the same message and the documented type for the status; its param
// and code have no Anthropic counterpart and are lost. Any other Body is
// kept as is.
func FromOpenAI(b fakeupstream.Behavior) Behavior {
	out := Behavior{
		Status: b.Status, Header: b.Header.Clone(), TTFT: b.TTFT, ChunkDelay: b.ChunkDelay,
		Fault: b.Fault, FaultAfterChunks: b.FaultAfterChunks, ExtraFields: b.ExtraFields,
		Body: b.Body,
	}
	if b.Body != nil {
		var env struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b.Body, &env) == nil && env.Error != nil {
			out.Body = errorBody(ErrorType(b.Status), env.Error.Message, "")
		}
	}
	return out
}
