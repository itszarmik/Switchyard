// Package anthropic is the adapter for Anthropic's Messages API
// (POST {base_url}/v1/messages).
//
// The gateway speaks the OpenAI Chat Completions wire format, so the
// adapter translates in both directions, following DESIGN §6.2: the
// client's OpenAI-shaped request becomes a Messages request (request.go),
// a Message becomes a chat.completion (response.go), and the Messages
// event stream becomes chat.completion.chunk events (stream.go). Fields
// with no Anthropic counterpart are dropped, never forwarded: the gateway
// has already filtered deployments on the capabilities a request needs.
//
// The HTTP side matches the openai adapter: one pooled client per
// provider, the provider's connect, first_token, idle and total timeouts,
// the commit point at the first stream event, the keep-alive drain after
// message_stop, header hygiene, and every failure as a *providers.Error
// that never contains the provider key.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
	"switchyard/internal/tracing"
	"switchyard/internal/version"
)

// APIVersion is the anthropic-version header the adapter sends.
const APIVersion = "2023-06-01"

// Limits on what the adapter reads from an upstream.
const (
	// maxResponseBytes caps a non-streaming response body.
	maxResponseBytes = 64 << 20
	// maxErrorBodyBytes caps an error response body.
	maxErrorBodyBytes = 1 << 20
)

// Connection pool settings, as in the openai adapter.
const (
	maxIdleConnsPerHost = 512
	idleConnTimeout     = 90 * time.Second
	tcpKeepAlive        = 30 * time.Second
)

// upstreamRequestIDHeader is the response header Anthropic uses for its own
// request id.
const upstreamRequestIDHeader = "Request-Id"

// maxRequestIDLen bounds the upstream request id that is returned to the
// client and logged.
const maxRequestIDLen = 128

// neverForwarded are request headers the adapter sets itself, or that
// describe the client's own connection or encoding. They are dropped even
// if forward_headers lists them.
var neverForwarded = map[string]bool{
	"Authorization":       true,
	"Proxy-Authorization": true,
	"X-Api-Key":           true,
	"Anthropic-Version":   true,
	"Cookie":              true,
	"Host":                true,
	"Content-Length":      true,
	"Content-Type":        true,
	"Accept":              true,
	"Accept-Encoding":     true,
	"User-Agent":          true,
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Connection":    true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// timeouts bound each upstream call. Zero disables a bound.
type timeouts struct {
	firstToken time.Duration
	idle       time.Duration
	total      time.Duration
}

// Adapter calls one Anthropic provider. It is safe for concurrent use.
type Adapter struct {
	id        string
	endpoint  string
	apiKey    string
	forward   []string
	timeouts  timeouts
	client    *http.Client
	dialer    *net.Dialer // bounds connection setup by the connect timeout
	userAgent string
	// now is the clock behind the created field of responses and chunks.
	now func() time.Time
}

var _ providers.Adapter = (*Adapter)(nil)

// New returns the adapter for provider p, which must be validated config
// with type anthropic. Requests go to base_url + "/v1/messages", so
// base_url is the API root, such as https://api.anthropic.com. The
// transport is built as in the openai adapter: connect bounds dialing and
// the TLS handshake, idle connections are pooled and kept alive, and
// redirects are not followed. With o.PropagateTrace the transport sends the
// W3C traceparent of the call's span.
func New(p config.Provider, o providers.Options) (*Adapter, error) {
	endpoint, err := url.JoinPath(p.BaseURL, "v1/messages")
	if err != nil {
		// The URL is not echoed; it is the operator's to print.
		return nil, fmt.Errorf("anthropic: provider %s: invalid base_url", p.ID)
	}
	connect := p.Timeouts.Connect.Std()
	dialer := &net.Dialer{Timeout: connect, KeepAlive: tcpKeepAlive}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   connect,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		IdleConnTimeout:       idleConnTimeout,
		ExpectContinueTimeout: time.Second,
	}

	var forward []string
	for _, h := range p.ForwardHeaders {
		name := textproto.CanonicalMIMEHeaderKey(h)
		if !neverForwarded[name] {
			forward = append(forward, name)
		}
	}

	return &Adapter{
		id:       p.ID,
		endpoint: endpoint,
		apiKey:   p.APIKey.Value(),
		forward:  forward,
		timeouts: timeouts{
			firstToken: p.Timeouts.FirstToken.Std(),
			idle:       p.Timeouts.Idle.Std(),
			total:      p.Timeouts.Total.Std(),
		},
		client: &http.Client{
			// tracing.Transport returns transport itself unless the trace
			// context is propagated.
			Transport: tracing.Transport(transport, o.PropagateTrace),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		dialer:    dialer,
		userAgent: "switchyard/" + version.Get().Version,
		now:       time.Now,
	}, nil
}

// CloseIdleConnections closes the provider's idle pooled connections.
func (a *Adapter) CloseIdleConnections() { a.client.CloseIdleConnections() }

// Chat implements providers.Adapter. The response body is a chat.completion
// translated from the upstream Message. total bounds the whole call
// including the body. first_token does not apply: the response headers
// arrive only after the whole generation.
func (a *Adapter) Chat(ctx context.Context, req *providers.Request) (*providers.Response, error) {
	body, err := a.translate(req, false)
	if err != nil {
		return nil, err
	}
	c := a.newCall(ctx, false)
	defer c.stop()

	resp, err := a.do(c, req, body, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	ttft := time.Since(c.start)
	rid := a.requestID(resp.Header)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, a.statusError(resp, rid)
	}
	msg, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, c.classify(err, rid)
	}
	if len(msg) > maxResponseBytes {
		return nil, &providers.Error{
			Kind: providers.KindServerError, Status: resp.StatusCode, Provider: a.id, UpstreamRequestID: rid,
			Message: fmt.Sprintf("upstream response exceeds %d bytes", maxResponseBytes),
		}
	}
	out, usage, err := translateMessage(msg, a.now())
	if err != nil {
		return nil, &providers.Error{
			Kind: providers.KindServerError, Status: resp.StatusCode, Provider: a.id, UpstreamRequestID: rid,
			Message: "upstream sent a response that is not a Messages API message", Err: err,
		}
	}
	return &providers.Response{Status: resp.StatusCode, Body: out, UpstreamRequestID: rid, Usage: usage, TTFT: ttft}, nil
}

// ChatStream implements providers.Adapter. It returns once the first
// chunk, the role chunk of message_start, is ready. first_token bounds the
// wait for the first event, idle each gap between events, and total the
// whole call.
func (a *Adapter) ChatStream(ctx context.Context, req *providers.Request) (providers.Stream, error) {
	body, err := a.translate(req, true)
	if err != nil {
		return nil, err
	}
	c := a.newCall(ctx, true)
	resp, err := a.do(c, req, body, true)
	if err != nil {
		c.stop()
		return nil, err
	}
	rid := a.requestID(resp.Header)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer c.stop()
		defer func() { _ = resp.Body.Close() }()
		return nil, a.statusError(resp, rid)
	}
	s := newStream(a, c, resp.Body, rid, req.Chat.IncludeUsage())
	first, err := s.Next()
	s.ttft = time.Since(c.start)
	switch {
	case errors.Is(err, io.EOF):
		// message_stop without any chunk cannot happen after
		// message_start, but an empty well-formed stream is not an error.
		_ = s.Close()
	case err != nil:
		_ = s.Close()
		return nil, err
	default:
		s.pending = append([]*schema.ChatChunk{first}, s.pending...)
	}
	return s, nil
}

// do sends body and returns the response once its headers arrive.
func (a *Adapter) do(c *call, req *providers.Request, body []byte, stream bool) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(c.ctx, http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &providers.Error{Kind: providers.KindTransport, Provider: a.id, Message: "could not build the upstream request", Err: err}
	}
	h := httpReq.Header
	for _, name := range a.forward {
		for _, v := range req.Header.Values(name) {
			h.Add(name, v)
		}
	}
	h.Set("Content-Type", "application/json")
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	h.Set("User-Agent", a.userAgent)
	h.Set("Anthropic-Version", APIVersion)
	if a.apiKey != "" {
		h.Set("X-Api-Key", a.apiKey)
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, c.classify(err, "")
	}
	return resp, nil
}

// promptTooLong is the lower-cased start of the message of Anthropic's 400
// for a request that exceeds the context window, such as "prompt is too
// long: 210000 tokens > 200000 maximum".
const promptTooLong = "prompt is too long"

// statusError turns a non-2xx response into an error.
//
//   - 401, 402 and 403 are the gateway's own credential or account
//     problem (KindAuth). Their body is never read: it may echo parts of
//     the key, and the client has no use for it.
//   - 429 is KindRateLimited with Retry-After; 404 is KindNotFound; 413,
//     and a 400 whose message says the prompt is too long, are
//     KindContextLength; other 4xx are KindClientError. All of them keep
//     the Anthropic error, re-shaped into an OpenAI envelope, unless it
//     contains the provider key. (Anthropic refusals are not errors: they
//     arrive as a 200 with stop_reason refusal.)
//   - 5xx, 529 overloaded_error included, and anything else are
//     KindServerError, without the body.
//
// The message names the status and, when the body has one, the Anthropic
// error type (such as overloaded_error); it never quotes the body.
func (a *Adapter) statusError(resp *http.Response, rid string) *providers.Error {
	e := &providers.Error{Status: resp.StatusCode, Provider: a.id, UpstreamRequestID: rid}
	code := resp.StatusCode
	if code == http.StatusUnauthorized || code == http.StatusPaymentRequired || code == http.StatusForbidden {
		e.Kind = providers.KindAuth
		e.Message = fmt.Sprintf("upstream rejected the provider credential or account (HTTP %d)", code)
		return e
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes+1))
	var env *errorEnvelope
	if err == nil && len(raw) <= maxErrorBodyBytes {
		env = a.parseErrorEnvelope(raw)
	}
	suffix := ""
	if env != nil {
		suffix = " (" + env.Error.Type + ")"
	}
	switch {
	case code == http.StatusTooManyRequests:
		e.Kind = providers.KindRateLimited
		e.Message = "upstream rate limit reached (HTTP 429)" + suffix
		e.RetryAfter = retryAfter(resp.Header, time.Now())
	case code == http.StatusNotFound:
		e.Kind = providers.KindNotFound
		e.Message = "upstream does not know the model (HTTP 404)" + suffix
	case code == http.StatusRequestEntityTooLarge ||
		(code == http.StatusBadRequest && env != nil && strings.Contains(strings.ToLower(env.Error.Message), promptTooLong)):
		e.Kind = providers.KindContextLength
		e.Message = fmt.Sprintf("request exceeds the model's context window (HTTP %d)%s", code, suffix)
	case code >= 400 && code <= 499:
		e.Kind = providers.KindClientError
		e.Message = fmt.Sprintf("upstream rejected the request (HTTP %d)%s", code, suffix)
	case code >= 500 && code <= 599:
		e.Kind = providers.KindServerError
		e.Message = fmt.Sprintf("upstream returned HTTP %d%s", code, suffix)
		return e
	default:
		e.Kind = providers.KindServerError
		e.Message = fmt.Sprintf("upstream returned unexpected HTTP %d", code)
		return e
	}
	if env != nil {
		e.Body = env.openAI(code)
	}
	return e
}

// requestID returns the upstream's request-id, or "" unless it is 1–128
// bytes of printable ASCII without the provider key: it goes to the client
// in a header and into the logs.
func (a *Adapter) requestID(h http.Header) string {
	rid := h.Get(upstreamRequestIDHeader)
	if len(rid) > maxRequestIDLen || a.containsKey([]byte(rid)) {
		return ""
	}
	for i := 0; i < len(rid); i++ {
		if rid[i] < ' ' || rid[i] > '~' {
			return ""
		}
	}
	return rid
}

// containsKey reports whether b contains the provider's API key.
func (a *Adapter) containsKey(b []byte) bool {
	return a.apiKey != "" && bytes.Contains(b, []byte(a.apiKey))
}

// unsupported is a request the adapter rejects before calling the
// upstream, because it cannot be expressed in the Messages API (or needs a
// capability the deployment lacks). Its kind is KindUnsupported, so the
// request falls back to a deployment that may serve it; if none does, it
// is answered like an upstream 400, with an OpenAI envelope naming param.
func (a *Adapter) unsupported(param, format string, args ...any) *providers.Error {
	msg := fmt.Sprintf(format, args...)
	return &providers.Error{
		Kind: providers.KindUnsupported, Status: http.StatusBadRequest, Provider: a.id,
		Message: "request cannot be sent to an Anthropic deployment: " + msg,
		Body:    openAIEnvelope(msg, "invalid_request_error", param, "invalid_value"),
	}
}

// openAIEnvelope encodes an OpenAI error envelope. An empty param or code
// is sent as null.
func openAIEnvelope(message, typ, param, code string) []byte {
	nullable := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	b, _ := marshal(openAIErrorBody{Error: openAIError{ // only strings; cannot fail
		Message: message, Type: typ, Param: nullable(param), Code: nullable(code),
	}})
	return b
}

// openAIErrorBody is OpenAI's error envelope.
type openAIErrorBody struct {
	Error openAIError `json:"error"`
}

type openAIError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// errorEnvelope is Anthropic's error body:
// {"type":"error","error":{"type":…,"message":…},"request_id":…}.
type errorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// maxErrorTypeLen bounds the error type copied into messages and codes.
const maxErrorTypeLen = 64

// parseErrorEnvelope decodes an Anthropic error body. It returns nil unless
// the body has an error object whose type is a short snake_case word, so
// that the type is safe to put in log messages, and nil if the provider key
// appears in the raw body or in any decoded field: a JSON escape such as
// \u002d hides the key from the raw bytes but not from the decoded text.
func (a *Adapter) parseErrorEnvelope(raw []byte) *errorEnvelope {
	var env errorEnvelope
	if json.Unmarshal(raw, &env) != nil || env.Error.Type == "" || len(env.Error.Type) > maxErrorTypeLen {
		return nil
	}
	if a.containsKey(raw) || a.containsKey([]byte(env.Type)) ||
		a.containsKey([]byte(env.Error.Type)) || a.containsKey([]byte(env.Error.Message)) {
		return nil
	}
	for _, r := range env.Error.Type {
		if (r < 'a' || r > 'z') && r != '_' {
			return nil
		}
	}
	return &env
}

// openAI re-shapes the error into an OpenAI envelope for status: a 429
// gets OpenAI's rate-limit type and code; any other status gets type
// invalid_request_error, with the Anthropic error type as the code unless
// it is invalid_request_error itself.
func (e *errorEnvelope) openAI(status int) []byte {
	if status == http.StatusTooManyRequests {
		return openAIEnvelope(e.Error.Message, "requests", "", "rate_limit_exceeded")
	}
	code := e.Error.Type
	if code == "invalid_request_error" {
		code = ""
	}
	return openAIEnvelope(e.Error.Message, "invalid_request_error", "", code)
}

// retryAfter parses Retry-After (delay seconds or an HTTP date). It returns
// 0 if the header is absent or unusable.
func retryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 && secs < 1e9 {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// marshal encodes v as JSON without escaping <, > and &, so that text
// keeps its original spelling.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("anthropic: encode JSON: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}
