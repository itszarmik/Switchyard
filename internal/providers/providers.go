// Package providers defines the contract between the gateway and the
// per-wire-protocol upstream adapters: the Adapter interface, the request
// and response values they exchange, and *Error, the normalized failure
// every adapter returns.
//
// Adapters live in subpackages (providers/openai). Each adapter value
// serves exactly one configured provider, with its own HTTP client.
package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"switchyard/internal/schema"
)

// Adapter calls one upstream provider.
type Adapter interface {
	// Chat sends a non-streaming chat completion. A 2xx answer is returned
	// as a *Response; anything else, including a non-2xx answer, as an
	// *Error.
	Chat(ctx context.Context, req *Request) (*Response, error)
	// ChatStream sends a streaming chat completion. It returns once the
	// first event has arrived, so a failure before that point is still an
	// ordinary *Error the caller can answer with an HTTP status. The caller
	// must Close the stream.
	ChatStream(ctx context.Context, req *Request) (Stream, error)
}

// Request is one upstream call.
type Request struct {
	// Chat is the client's request.
	Chat *schema.ChatRequest
	// UpstreamModel replaces the request's model field.
	UpstreamModel string
	// Header holds the client's request headers. Adapters copy only those
	// the provider's forward_headers allow, and never Authorization.
	Header http.Header
}

// Response is a successful non-streaming upstream answer.
type Response struct {
	// Status is the upstream's 2xx status code.
	Status int
	// Body is the upstream body, byte for byte.
	Body []byte
	// UpstreamRequestID is the upstream's x-request-id header.
	UpstreamRequestID string
	// Usage is parsed from Body, or nil if the body carries none.
	Usage *schema.Usage
	// TTFT is the time from sending the request until the response headers
	// arrived.
	TTFT time.Duration
}

// Stream is a streaming upstream answer. It is not safe for concurrent use.
type Stream interface {
	// Next returns the next chunk. It returns io.EOF after the upstream's
	// [DONE] event, and an *Error on any failure.
	Next() (*schema.ChatChunk, error)
	// Usage returns the last usage the stream reported, or nil if none has
	// arrived (yet).
	Usage() *schema.Usage
	// UpstreamRequestID is the upstream's x-request-id header.
	UpstreamRequestID() string
	// TTFT is the time from sending the request until the first event.
	TTFT() time.Duration
	// Close releases the stream; if it has not ended, the upstream request
	// is canceled. It is safe to call more than once.
	Close() error
}

// Kind classifies an upstream failure. The values double as the outcome
// label of the upstream request metrics.
type Kind string

// The failure kinds.
const (
	// KindClientError is a 4xx other than 401, 403 and 429: the request
	// itself was rejected.
	KindClientError Kind = "client_error"
	// KindRateLimited is a 429.
	KindRateLimited Kind = "rate_limited"
	// KindAuth is a 401 or 403: the upstream rejected our credential.
	KindAuth Kind = "auth"
	// KindServerError is a 5xx, an unexpected status, an error event in a
	// stream, or a malformed stream.
	KindServerError Kind = "server_error"
	// KindTimeout is a first-token, idle or total timeout.
	KindTimeout Kind = "timeout"
	// KindTransport is a connection failure: DNS, connect, TLS, reset, or
	// a body cut short.
	KindTransport Kind = "transport"
	// KindCanceled means the caller's context ended, usually because the
	// client went away.
	KindCanceled Kind = "canceled"
)

// OutcomeOK is the metrics outcome of a successful upstream call.
const OutcomeOK = "ok"

// Outcome returns the metrics outcome for the result of an upstream call:
// OutcomeOK for nil, the Kind of an *Error, and KindTransport otherwise.
func Outcome(err error) string {
	if err == nil {
		return OutcomeOK
	}
	var e *Error
	if errors.As(err, &e) {
		return string(e.Kind)
	}
	return string(KindTransport)
}

// Error is an upstream failure. Its fields never hold the provider
// credential.
type Error struct {
	Kind Kind
	// Status is the upstream HTTP status, or 0 if no response arrived.
	Status int
	// RetryAfter is the upstream's Retry-After, or 0 if it sent none.
	RetryAfter time.Duration
	// Provider is the provider id.
	Provider string
	// UpstreamRequestID is the upstream's x-request-id, if a response
	// arrived.
	UpstreamRequestID string
	// Message describes the failure. It is safe to log and to show the
	// client: it never contains upstream bodies or credentials.
	Message string
	// Body is the upstream's error body, set only for KindClientError and
	// KindRateLimited and only when it is an OpenAI error envelope. It is
	// never set for KindAuth, whose body may echo parts of our credential.
	Body []byte
	// Err is the underlying cause, if any. It may contain URLs (never
	// credentials) and is meant for logs, not clients.
	Err error
}

// Error returns "provider <id>: <kind>: <message>".
func (e *Error) Error() string {
	return fmt.Sprintf("provider %s: %s: %s", e.Provider, e.Kind, e.Message)
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error { return e.Err }
