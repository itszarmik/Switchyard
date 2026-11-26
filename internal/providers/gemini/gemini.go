// Package gemini is the adapter for Google's native Gemini API
// (POST {base_url}/models/{model}:generateContent, and
// :streamGenerateContent?alt=sse for streams), with base_url the API
// version root, such as https://generativelanguage.googleapis.com/v1beta.
//
// The gateway speaks the OpenAI Chat Completions wire format, so the
// adapter translates in both directions, following DESIGN §6.2: the
// client's OpenAI-shaped request becomes a GenerateContentRequest
// (request.go), a GenerateContentResponse becomes a chat.completion
// (response.go), and the SSE stream of GenerateContentResponse objects
// becomes chat.completion.chunk events (stream.go). Fields with no Gemini
// counterpart are dropped, never forwarded; parameters that ask for a
// feature Gemini cannot honor (n > 1, logprobs, seed, sampling,
// penalties) are refused, so the request falls back to another
// deployment.
//
// Gemini 3 and 4 return thought signatures that must come back on the
// next turn's function calls. The adapter keeps no state: it carries each
// signature inside the tool-call id it returns to the client (toolid.go)
// and puts it back on the matching functionCall part when the id returns.
//
// The HTTP side matches the anthropic adapter: one pooled client per
// provider, the provider's connect, first_token, idle and total timeouts,
// the commit point at the first stream event, header hygiene, and every
// failure as a *providers.Error that never contains the provider key. The
// key is sent only in the x-goog-api-key header, never in a URL.
package gemini

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

// apiKeyHeader carries the provider key. The key is never put in the key
// query parameter, which would put it in URLs and so in logs.
const apiKeyHeader = "X-Goog-Api-Key" //nolint:gosec // G101: a header name, not a credential

// maxRequestIDLen bounds the upstream response id that is returned to the
// client and logged.
const maxRequestIDLen = 128

// MaxNonStreamingOutput is the most output tokens a non-streaming
// generateContent call may ask for: Google rejects a larger
// maxOutputTokens on non-streaming REST calls to Gemini 4 Carbon (and
// Argon) with a 400. A non-streaming request above it is served by
// streaming internally and aggregating the stream (see Adapter.Chat).
const MaxNonStreamingOutput = 32768

// neverForwarded are request headers the adapter sets itself, or that
// describe the client's own connection or encoding. They are dropped even
// if forward_headers lists them.
var neverForwarded = map[string]bool{
	"Authorization":       true,
	"Proxy-Authorization": true,
	"X-Api-Key":           true,
	apiKeyHeader:          true,
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

// Adapter calls one Gemini provider. It is safe for concurrent use.
type Adapter struct {
	id        string
	models    string // base_url + "/models"
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
// with type gemini. Requests go to base_url + "/models/{model}:…", so
// base_url is the API version root, such as
// https://generativelanguage.googleapis.com/v1beta. The transport is built
// as in the anthropic adapter: connect bounds dialing and the TLS
// handshake, idle connections are pooled and kept alive, and redirects are
// not followed. With o.PropagateTrace the transport sends the W3C
// traceparent of the call's span.
func New(p config.Provider, o providers.Options) (*Adapter, error) {
	models, err := url.JoinPath(p.BaseURL, "models")
	if err != nil {
		// The URL is not echoed; it is the operator's to print.
		return nil, fmt.Errorf("gemini: provider %s: invalid base_url", p.ID)
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
		id:      p.ID,
		models:  models,
		apiKey:  p.APIKey.Value(),
		forward: forward,
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

// endpoint returns the URL of model's generateContent method, or of
// streamGenerateContent with SSE framing. A "models/" prefix on the
// upstream model, the resource name Google's own responses use, is
// accepted.
func (a *Adapter) endpoint(model string, stream bool) string {
	u := a.models + "/" + url.PathEscape(strings.TrimPrefix(model, "models/"))
	if stream {
		return u + ":streamGenerateContent?alt=sse"
	}
	return u + ":generateContent"
}

// Chat implements providers.Adapter. The response body is a chat.completion
// translated from the upstream GenerateContentResponse. total bounds the
// whole call including the body. first_token does not apply: the response
// headers arrive only after the whole generation.
//
// A request whose output limit is above MaxNonStreamingOutput is sent to
// streamGenerateContent instead, which allows the model's full output
// limit, and the stream is aggregated into the same chat.completion (see
// chatLong). The limit is the client's max_tokens (or
// max_completion_tokens), else the catalog's max_output_tokens: without a
// client limit, Google caps a non-streaming Gemini 4 Carbon call at 32,768
// tokens, thinking included, while a stream gets the model's default.
func (a *Adapter) Chat(ctx context.Context, req *providers.Request) (*providers.Response, error) {
	body, maxOut, err := a.translate(req)
	if err != nil {
		return nil, err
	}
	limit := maxOut
	if limit == 0 {
		limit = req.MaxOutputTokens
	}
	if limit > MaxNonStreamingOutput {
		return a.chatLong(ctx, req, body)
	}
	c := a.newCall(ctx, false)
	defer c.stop()

	resp, err := a.do(c, req, body, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	ttft := time.Since(c.start)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, a.statusError(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, c.classify(err, "")
	}
	if len(raw) > maxResponseBytes {
		return nil, &providers.Error{
			Kind: providers.KindServerError, Status: resp.StatusCode, Provider: a.id,
			Message: fmt.Sprintf("upstream response exceeds %d bytes", maxResponseBytes),
		}
	}
	var gr generateResponse
	if err := json.Unmarshal(raw, &gr); err != nil {
		return nil, &providers.Error{
			Kind: providers.KindServerError, Status: resp.StatusCode, Provider: a.id,
			Message: "upstream sent a response that is not a GenerateContentResponse", Err: err,
		}
	}
	acc := newAccumulator(a.id, req.UpstreamModel)
	if _, err := acc.apply(&gr); err != nil {
		return nil, err
	}
	if err := acc.end(); err != nil {
		return nil, err
	}
	return a.respond(acc, resp.StatusCode, ttft)
}

// chatLong serves a non-streaming request through streamGenerateContent
// and aggregates the stream. The timeouts are a non-streaming call's:
// only total (and the caller's deadline) bounds it, since the client
// waits for the whole answer either way. TTFT is the time to the first
// stream event, the first sign of generation, rather than to the
// response headers, which a stream sends at once.
func (a *Adapter) chatLong(ctx context.Context, req *providers.Request, body []byte) (*providers.Response, error) {
	c := a.newCall(ctx, false)
	defer c.stop()
	resp, err := a.do(c, req, body, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, a.statusError(resp)
	}
	r := newEventReader(resp.Body)
	acc := newAccumulator(a.id, req.UpstreamModel)
	var ttft time.Duration
	for {
		gr, err := r.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, a.streamReadError(c, err, "")
		}
		if acc.events == 0 {
			ttft = time.Since(c.start)
		}
		if gr.Error != nil {
			return nil, a.eventError(gr.Error, acc.responseID())
		}
		if _, err := acc.apply(gr); err != nil {
			return nil, err
		}
	}
	if !acc.finished {
		return nil, &providers.Error{
			Kind: providers.KindServerError, Provider: a.id, UpstreamRequestID: acc.responseID(),
			Message: "upstream ended the stream without a finish reason",
		}
	}
	return a.respond(acc, resp.StatusCode, ttft)
}

// respond builds the chat.completion of a finished accumulator.
func (a *Adapter) respond(acc *accumulator, status int, ttft time.Duration) (*providers.Response, error) {
	out, usage, err := acc.completion(a.now())
	if err != nil {
		return nil, &providers.Error{
			Kind: providers.KindServerError, Status: status, Provider: a.id, UpstreamRequestID: acc.responseID(),
			Message: "could not encode the translated response", Err: err,
		}
	}
	return &providers.Response{
		Status: status, Body: out, UpstreamRequestID: a.safeID(acc.responseID()), Usage: usage, TTFT: ttft,
	}, nil
}

// ChatStream implements providers.Adapter. It returns once the first
// chunk, the role chunk of the first event, is ready. first_token bounds
// the wait for the first event, idle each gap between events, and total
// the whole call.
func (a *Adapter) ChatStream(ctx context.Context, req *providers.Request) (providers.Stream, error) {
	body, _, err := a.translate(req)
	if err != nil {
		return nil, err
	}
	c := a.newCall(ctx, true)
	resp, err := a.do(c, req, body, true)
	if err != nil {
		c.stop()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer c.stop()
		defer func() { _ = resp.Body.Close() }()
		return nil, a.statusError(resp)
	}
	s := newStream(a, c, resp.Body, req.UpstreamModel, req.Chat.IncludeUsage())
	first, err := s.Next()
	s.ttft = time.Since(c.start)
	switch {
	case errors.Is(err, io.EOF):
		// The first event always queues a role chunk, so this cannot
		// happen, but an empty well-formed stream is not an error.
		_ = s.Close()
	case err != nil:
		_ = s.Close()
		return nil, err
	default:
		s.pending = append([]*schema.ChatChunk{first}, s.pending...)
	}
	return s, nil
}

// do sends body to the model's generateContent method (streaming or not)
// and returns the response once its headers arrive.
func (a *Adapter) do(c *call, req *providers.Request, body []byte, stream bool) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(c.ctx, http.MethodPost, a.endpoint(req.UpstreamModel, stream), bytes.NewReader(body))
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
	if a.apiKey != "" {
		h.Set(apiKeyHeader, a.apiKey)
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, c.classify(err, "")
	}
	return resp, nil
}

// Google error statuses (google.rpc.Code names) and ErrorInfo reasons the
// adapter tells apart.
const (
	statusInvalidArgument    = "INVALID_ARGUMENT"
	statusFailedPrecondition = "FAILED_PRECONDITION"
	reasonAPIKeyInvalid      = "API_KEY_INVALID" //nolint:gosec // G101: an error reason, not a credential
)

// inputTooLong are lower-cased fragments of the message of Google's 400
// for a prompt over the model's input limit, such as "The input token
// count (1200000) exceeds the maximum number of tokens allowed (1048576)."
// Google documents no dedicated error for it, so the message is matched.
var inputTooLong = []string{"input token count", "exceeds the maximum number of tokens"}

// statusError turns a non-2xx response into an error.
//
//   - 401, 402 and 403 are the gateway's own credential or account problem
//     (KindAuth), and so are a 400 whose ErrorInfo reason is
//     API_KEY_INVALID (Google's answer to a bad key) and a 400
//     FAILED_PRECONDITION (the free tier is not available in the region
//     and the project has no billing). Their body is never kept: it may
//     echo parts of the key, and the client has no use for it.
//   - 429 is KindRateLimited, with the delay of a google.rpc.RetryInfo
//     detail, else Retry-After; 404 is KindNotFound; 408 and 504
//     (DEADLINE_EXCEEDED) are KindTimeout; 413, and a 400 whose message
//     says the input is over the token limit, are KindContextLength; other
//     4xx are KindClientError. The client-visible kinds keep the Google
//     error, re-shaped into an OpenAI envelope, unless it contains the
//     provider key.
//   - 5xx and anything else are KindServerError, without the body.
//
// The message names the status and, when the body has one, the Google
// status name (such as RESOURCE_EXHAUSTED); it never quotes the body.
func (a *Adapter) statusError(resp *http.Response) *providers.Error {
	code := resp.StatusCode
	e := &providers.Error{Status: code, Provider: a.id}
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
	a.classifyError(e, code, env, retryAfter(resp.Header, time.Now()))
	return e
}

// classifyError fills e's kind, message, retry delay and body for an
// upstream error with HTTP status code and the parsed Google error body
// env (nil if there was none, or it was unsafe). headerDelay is the
// Retry-After header's delay, used when the body has no RetryInfo.
func (a *Adapter) classifyError(e *providers.Error, code int, env *errorEnvelope, headerDelay time.Duration) {
	suffix := ""
	if env != nil && env.Error.Status != "" {
		suffix = " (" + env.Error.Status + ")"
	}
	switch {
	case code == http.StatusUnauthorized || code == http.StatusPaymentRequired || code == http.StatusForbidden ||
		(code == http.StatusBadRequest && env != nil &&
			(env.reason() == reasonAPIKeyInvalid || env.Error.Status == statusFailedPrecondition)):
		e.Kind = providers.KindAuth
		e.Message = fmt.Sprintf("upstream rejected the provider credential or account (HTTP %d)%s", code, suffix)
		return
	case code == http.StatusTooManyRequests:
		e.Kind = providers.KindRateLimited
		e.Message = "upstream rate limit reached (HTTP 429)" + suffix
		e.RetryAfter = headerDelay
		if env != nil {
			if d := env.retryDelay(); d > 0 {
				e.RetryAfter = d
			}
		}
	case code == http.StatusNotFound:
		e.Kind = providers.KindNotFound
		e.Message = "upstream does not know the model (HTTP 404)" + suffix
	case code == http.StatusRequestTimeout || code == http.StatusGatewayTimeout:
		e.Kind = providers.KindTimeout
		e.Message = fmt.Sprintf("upstream timed out (HTTP %d)%s", code, suffix)
		return
	case code == http.StatusRequestEntityTooLarge || (code == http.StatusBadRequest && env != nil && env.inputTooLong()):
		e.Kind = providers.KindContextLength
		e.Message = fmt.Sprintf("request exceeds the model's context window (HTTP %d)%s", code, suffix)
	case code >= 400 && code <= 499:
		e.Kind = providers.KindClientError
		e.Message = fmt.Sprintf("upstream rejected the request (HTTP %d)%s", code, suffix)
	case code >= 500 && code <= 599:
		e.Kind = providers.KindServerError
		e.Message = fmt.Sprintf("upstream returned HTTP %d%s", code, suffix)
		return
	default:
		e.Kind = providers.KindServerError
		e.Message = fmt.Sprintf("upstream returned unexpected HTTP %d", code)
		return
	}
	if env != nil {
		e.Body = env.openAI(code)
	}
}

// eventError turns the error object of a stream event into an error of
// the kind its code maps to (see classifyError), so that an error before
// the first chunk is retried or falls back like the same HTTP status. Its
// Status is the error's code, which the gateway answers with when the
// kind passes through.
func (a *Adapter) eventError(ge *googleError, rid string) *providers.Error {
	env := a.parseErrorEnvelope(mustMarshal(errorEnvelope{Error: *ge}))
	code := ge.Code
	if code < 400 || code > 599 {
		code = http.StatusInternalServerError
	}
	e := &providers.Error{Status: code, Provider: a.id, UpstreamRequestID: a.safeID(rid)}
	a.classifyError(e, code, env, 0)
	e.Message = "upstream sent an error event: " + e.Message
	return e
}

// streamReadError classifies a failure to read a stream.
func (a *Adapter) streamReadError(c *call, err error, rid string) *providers.Error {
	switch {
	case c.ctx.Err() != nil:
		return c.classify(err, a.safeID(rid))
	case errors.Is(err, errEventTooLarge):
		return &providers.Error{
			Kind: providers.KindServerError, Provider: a.id, UpstreamRequestID: a.safeID(rid),
			Message: "upstream sent an event that is too large",
		}
	case errors.Is(err, errNotJSON):
		return &providers.Error{
			Kind: providers.KindServerError, Provider: a.id, UpstreamRequestID: a.safeID(rid),
			Message: "upstream sent an event that is not a GenerateContentResponse",
		}
	default:
		return c.classify(err, a.safeID(rid))
	}
}

// safeID returns the upstream's responseId, or "" unless it is 1–128
// bytes of printable ASCII without the provider key: it goes to the client
// in a header and into the logs. Google sends no request-id header, so the
// response id, which identifies the call to Google, stands in for one.
func (a *Adapter) safeID(rid string) string {
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
// upstream, because it cannot be expressed in the Gemini API (or needs a
// capability the deployment lacks). Its kind is KindUnsupported, so the
// request falls back to a deployment that may serve it; if none does, it
// is answered like an upstream 400, with an OpenAI envelope naming param.
func (a *Adapter) unsupported(param, format string, args ...any) *providers.Error {
	msg := fmt.Sprintf(format, args...)
	return &providers.Error{
		Kind: providers.KindUnsupported, Status: http.StatusBadRequest, Provider: a.id,
		Message: "request cannot be sent to a Gemini deployment: " + msg,
		Body:    openAIEnvelope(msg, "invalid_request_error", param, "invalid_value"),
	}
}

// invalid is a request the adapter rejects as malformed for every
// provider, such as a tool message that answers no tool call: a 400 client
// error that does not fall back.
func (a *Adapter) invalid(param, format string, args ...any) *providers.Error {
	msg := fmt.Sprintf(format, args...)
	return &providers.Error{
		Kind: providers.KindClientError, Status: http.StatusBadRequest, Provider: a.id,
		Message: "invalid request: " + msg,
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
	return mustMarshal(openAIErrorBody{Error: openAIError{
		Message: message, Type: typ, Param: nullable(param), Code: nullable(code),
	}})
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

// errorEnvelope is Google's error body, also the error of a stream event:
// {"error":{"code":429,"message":…,"status":"RESOURCE_EXHAUSTED","details":[…]}}.
type errorEnvelope struct {
	Error googleError `json:"error"`
}

type googleError struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Status  string        `json:"status"`
	Details []errorDetail `json:"details,omitempty"`
}

// errorDetail is one google.rpc detail: ErrorInfo (reason) or RetryInfo
// (retryDelay, a protobuf Duration such as "7s"); others are ignored.
type errorDetail struct {
	Type       string `json:"@type"`
	Reason     string `json:"reason,omitempty"`
	RetryDelay string `json:"retryDelay,omitempty"`
}

// maxErrorStatusLen bounds the status name copied into messages and codes.
const maxErrorStatusLen = 64

// parseErrorEnvelope decodes a Google error body. It returns nil unless
// the body has an error object whose status is empty or a short
// UPPER_SNAKE_CASE word, so that it is safe to put in log messages, and
// nil if the provider key appears in the raw body or in any decoded
// string: a JSON escape such as - hides the key from the raw bytes but
// not from the decoded text.
func (a *Adapter) parseErrorEnvelope(raw []byte) *errorEnvelope {
	var env errorEnvelope
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &probe) != nil || len(probe.Error) == 0 || json.Unmarshal(raw, &env) != nil {
		return nil
	}
	if len(env.Error.Status) > maxErrorStatusLen {
		return nil
	}
	for _, r := range env.Error.Status {
		if (r < 'A' || r > 'Z') && r != '_' {
			return nil
		}
	}
	if a.containsKey(raw) || a.containsKey([]byte(env.Error.Message)) {
		return nil
	}
	for _, d := range env.Error.Details {
		if a.containsKey([]byte(d.Type)) || a.containsKey([]byte(d.Reason)) || a.containsKey([]byte(d.RetryDelay)) {
			return nil
		}
	}
	return &env
}

// reason returns the reason of the first ErrorInfo detail, or "".
func (e *errorEnvelope) reason() string {
	for _, d := range e.Error.Details {
		if strings.HasSuffix(d.Type, "google.rpc.ErrorInfo") {
			return d.Reason
		}
	}
	return ""
}

// retryDelay returns the delay of the first RetryInfo detail, or 0.
func (e *errorEnvelope) retryDelay() time.Duration {
	for _, d := range e.Error.Details {
		if !strings.HasSuffix(d.Type, "google.rpc.RetryInfo") {
			continue
		}
		if secs, err := strconv.ParseFloat(strings.TrimSuffix(d.RetryDelay, "s"), 64); err == nil &&
			strings.HasSuffix(d.RetryDelay, "s") && secs > 0 && secs < 1e9 {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return 0
}

// inputTooLong reports whether the message says the prompt is over the
// model's input token limit.
func (e *errorEnvelope) inputTooLong() bool {
	msg := strings.ToLower(e.Error.Message)
	for _, f := range inputTooLong {
		if strings.Contains(msg, f) {
			return true
		}
	}
	return false
}

// openAI re-shapes the error into an OpenAI envelope for status: a 429
// gets OpenAI's rate-limit type and code; any other status gets type
// invalid_request_error, with the lower-cased Google status as the code
// unless it is INVALID_ARGUMENT, the generic one.
func (e *errorEnvelope) openAI(status int) []byte {
	if status == http.StatusTooManyRequests {
		return openAIEnvelope(e.Error.Message, "requests", "", "rate_limit_exceeded")
	}
	code := strings.ToLower(e.Error.Status)
	if e.Error.Status == statusInvalidArgument {
		code = ""
	}
	return openAIEnvelope(e.Error.Message, "invalid_request_error", "", code)
}

// retryAfter parses Retry-After (delay seconds or an HTTP date). It returns
// 0 if the header is absent or unusable. Google documents RetryInfo in the
// body rather than the header, which classifyError prefers.
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
		return nil, fmt.Errorf("gemini: encode JSON: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// mustMarshal is marshal for values made only of strings, numbers and
// fixed wire types, whose encoding cannot fail.
func mustMarshal(v any) []byte {
	b, err := marshal(v)
	if err != nil {
		panic(err.Error())
	}
	return b
}
