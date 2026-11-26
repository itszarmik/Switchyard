package fakegemini

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"switchyard/test/fakeupstream"
)

// OpenAIUpstream adapts a Server to the method set of *fakeupstream.Server
// that the gateway conformance suite uses (BaseURL, Reset, Script with a
// fakeupstream.Behavior, Requests, WaitReceived, Await), so the suite can
// run against a gateway whose deployment is Gemini-backed.
//
// Script translates the OpenAI-wire behavior with [FromOpenAI]. Recorded
// requests are Gemini requests: the model is in the path, the body is a
// GenerateContentRequest and the credential is in x-goog-api-key.
type OpenAIUpstream struct {
	s *Server
}

// OpenAI returns the conformance view of s.
func (s *Server) OpenAI() *OpenAIUpstream { return &OpenAIUpstream{s: s} }

// Server returns the underlying fake.
func (u *OpenAIUpstream) Server() *Server { return u.s }

// BaseURL is the provider base_url for the gateway: the API version root.
func (u *OpenAIUpstream) BaseURL() string { return u.s.BaseURL() }

// Close closes the server.
func (u *OpenAIUpstream) Close() { u.s.Close() }

// Reset restores the default behavior and forgets the recorded requests.
func (u *OpenAIUpstream) Reset() { u.s.Reset() }

// Script sets FromOpenAI(b) as the behavior.
func (u *OpenAIUpstream) Script(b fakeupstream.Behavior) { u.s.Script(FromOpenAI(b)) }

// Sequence scripts the next requests one by one, each translated with
// FromOpenAI; see Server.Sequence.
func (u *OpenAIUpstream) Sequence(bs ...fakeupstream.Behavior) {
	out := make([]Behavior, len(bs))
	for i, b := range bs {
		out[i] = FromOpenAI(b)
	}
	u.s.Sequence(out...)
}

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

// FromOpenAI translates a fakeupstream behavior into the equivalent Gemini
// one. Status, TTFT, ChunkDelay, Fault, FaultAfterChunks, ExtraFields and
// Hold carry over unchanged; FaultAfterChunks then counts Gemini events,
// one per word of text. An OpenAI error envelope in Body becomes a Google
// error with the same message and the documented status name; its param
// and code have no counterpart and are lost. A 429's Retry-After header
// becomes a RetryInfo detail, which is how Google sends the delay. The
// finish reasons content_filter and length become SAFETY and MAX_TOKENS,
// and usage maps prompt, cached and completion tokens onto
// promptTokenCount, cachedContentTokenCount and candidatesTokenCount.
func FromOpenAI(b fakeupstream.Behavior) Behavior {
	out := Behavior{
		Status: b.Status, Header: b.Header.Clone(), TTFT: b.TTFT, ChunkDelay: b.ChunkDelay,
		Fault: b.Fault, FaultAfterChunks: b.FaultAfterChunks, ExtraFields: b.ExtraFields,
		Body: b.Body, Hold: b.Hold,
	}
	var details []any
	if b.Status == http.StatusTooManyRequests && out.Header != nil {
		if secs, err := strconv.ParseFloat(out.Header.Get("Retry-After"), 64); err == nil && secs > 0 {
			details = append(details, RetryInfo(time.Duration(secs*float64(time.Second))))
			out.Header.Del("Retry-After")
			if out.Body == nil {
				out.Body = ErrorBody(b.Status, "You exceeded your current quota, please check your plan and billing details.", details...)
			}
		}
	}
	if b.Body != nil {
		var env struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b.Body, &env) == nil && env.Error != nil {
			out.Body = ErrorBody(b.Status, env.Error.Message, details...)
		}
	}
	switch b.FinishReason {
	case "content_filter":
		out.FinishReason = "SAFETY"
	case "length":
		out.FinishReason = "MAX_TOKENS"
	}
	if u := b.Usage; u != nil {
		out.Usage = &Usage{Prompt: u.PromptTokens, Cached: u.CachedTokens, Candidates: u.CompletionTokens}
	}
	return out
}
