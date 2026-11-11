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
	// MaxOutputTokens is the catalog model's output limit, or 0 if unknown.
	// Adapters whose API requires max_tokens use it as the default.
	MaxOutputTokens int64
	// Capabilities is what the chosen deployment supports, as declared in
	// the catalog. Adapters check it before using a feature (for example
	// native structured output) and reject a request that needs one the
	// deployment lacks.
	Capabilities schema.CapabilitySet
	// NoStreamUsage is set when the deployment lacks capability
	// stream_usage. An OpenAI-wire adapter then forwards stream_options as
	// the client sent it instead of setting include_usage, so the stream
	// may carry no usage.
	NoStreamUsage bool
}

// Completer is implemented by adapters that can serve the legacy
// /v1/completions endpoint natively. Deployments of other adapters are
// served through the gateway's chat shim.
type Completer interface {
	// Completion sends a non-streaming legacy completion, with the same
	// contract as Adapter.Chat.
	Completion(ctx context.Context, req *CompletionRequest) (*Response, error)
	// CompletionStream sends a streaming legacy completion, with the same
	// contract as Adapter.ChatStream. Its chunks are text_completion
	// objects.
	CompletionStream(ctx context.Context, req *CompletionRequest) (Stream, error)
}

// CompletionRequest is one native legacy completion call.
type CompletionRequest struct {
	// Completion is the client's request.
	Completion *schema.CompletionRequest
	// UpstreamModel replaces the request's model field.
	UpstreamModel string
	// Header holds the client's request headers, as in Request.
	Header http.Header
	// NoStreamUsage is as in Request.
	NoStreamUsage bool
}

// Embedder is implemented by adapters whose provider serves /v1/embeddings.
// A deployment whose adapter is not an Embedder cannot serve embeddings.
type Embedder interface {
	// Embed sends an embeddings request, with the same contract as
	// Adapter.Chat. Response.Usage carries the prompt tokens.
	Embed(ctx context.Context, req *EmbedRequest) (*Response, error)
}

// EmbedRequest is one embeddings call.
type EmbedRequest struct {
	// Embedding is the client's request.
	Embedding *schema.EmbeddingRequest
	// UpstreamModel replaces the request's model field.
	UpstreamModel string
	// Header holds the client's request headers, as in Request.
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
