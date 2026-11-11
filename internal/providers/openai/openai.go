// Package openai is the adapter for OpenAI and every OpenAI-compatible
// upstream (vLLM, Together, Groq, Gemini's OpenAI endpoint and others).
//
// The wire format is already the gateway's canonical one, so the adapter
// forwards the client's request body losslessly (see
// schema.ChatRequest.Encode) and returns upstream bodies and stream events
// byte for byte. Its job is the HTTP call itself: one pooled client per
// provider, the provider's timeouts, header hygiene, and turning every
// failure into a *providers.Error.
package openai

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
	"switchyard/internal/version"
)

// Limits on what the adapter reads from an upstream.
const (
	// maxResponseBytes caps a non-streaming chat or completions response
	// body.
	maxResponseBytes = 64 << 20
	// maxEmbeddingResponseBytes caps an embeddings response body. It is
	// larger because embeddings are big: 2,048 inputs at 3,072 dimensions
	// in float format are about 88 MB.
	maxEmbeddingResponseBytes = 256 << 20
	// maxErrorBodyBytes caps an error response body.
	maxErrorBodyBytes = 1 << 20
)

// Connection pool settings. Idle connections are cheap, and a pool smaller
// than the request concurrency makes every burst open new connections.
const (
	maxIdleConnsPerHost = 512
	idleConnTimeout     = 90 * time.Second
	tcpKeepAlive        = 30 * time.Second
)

// upstreamRequestIDHeader is the response header OpenAI-wire upstreams use
// for their own request id.
const upstreamRequestIDHeader = "X-Request-Id"

// maxRequestIDLen bounds the upstream request id that is returned to the
// client and logged.
const maxRequestIDLen = 128

// neverForwarded are request headers the adapter sets itself, or that
// describe the client's own connection or encoding. They are dropped even
// if forward_headers lists them (config validation already rejects the
// credential headers).
var neverForwarded = map[string]bool{
	"Authorization":       true,
	"Proxy-Authorization": true,
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

// Timeouts bound each upstream call. Zero disables a bound.
type timeouts struct {
	firstToken time.Duration
	idle       time.Duration
	total      time.Duration
}

// Adapter calls one OpenAI-wire provider. It is safe for concurrent use.
type Adapter struct {
	id          string
	endpoint    string // chat/completions
	completions string // completions
	embeddings  string // embeddings
	apiKey      string
	forward     []string
	timeouts    timeouts
	client      *http.Client
	dialer      *net.Dialer // bounds connection setup by the connect timeout
	userAgent   string
}

var (
	_ providers.Adapter   = (*Adapter)(nil)
	_ providers.Completer = (*Adapter)(nil)
	_ providers.Embedder  = (*Adapter)(nil)
)

// New returns the adapter for provider p, which must be validated config
// with type openai. It builds the provider's own HTTP client and transport:
// connect bounds dialing and the TLS handshake, idle connections are pooled
// and kept alive, and redirects are not followed.
func New(p config.Provider) (*Adapter, error) {
	endpoint, err := url.JoinPath(p.BaseURL, "chat/completions")
	completions, err2 := url.JoinPath(p.BaseURL, "completions")
	embeddings, err3 := url.JoinPath(p.BaseURL, "embeddings")
	if err = errors.Join(err, err2, err3); err != nil {
		// The URL is not echoed: config validation guarantees it has no
		// credentials, but it is still the operator's to print.
		return nil, fmt.Errorf("openai: provider %s: invalid base_url", p.ID)
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
		id:          p.ID,
		endpoint:    endpoint,
		completions: completions,
		embeddings:  embeddings,
		apiKey:      p.APIKey.Value(),
		forward:     forward,
		timeouts: timeouts{
			firstToken: p.Timeouts.FirstToken.Std(),
			idle:       p.Timeouts.Idle.Std(),
			total:      p.Timeouts.Total.Std(),
		},
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		dialer:    dialer,
		userAgent: "switchyard/" + version.Get().Version,
	}, nil
}

// CloseIdleConnections closes the provider's idle pooled connections.
func (a *Adapter) CloseIdleConnections() { a.client.CloseIdleConnections() }

// Chat implements providers.Adapter. first_token bounds the wait for the
// response headers, and total bounds the whole call including the body.
func (a *Adapter) Chat(ctx context.Context, req *providers.Request) (*providers.Response, error) {
	return a.post(ctx, a.endpoint, req.Chat.EncodeFor(req.UpstreamModel, !req.NoStreamUsage), req.Header, maxResponseBytes)
}

// ChatStream implements providers.Adapter. first_token bounds the wait for
// the first event, idle each gap between events, and total the whole call.
func (a *Adapter) ChatStream(ctx context.Context, req *providers.Request) (providers.Stream, error) {
	return a.stream(ctx, a.endpoint, req.Chat.EncodeFor(req.UpstreamModel, !req.NoStreamUsage), req.Header)
}

// Completion implements providers.Completer: POST {base_url}/completions,
// forwarded losslessly as Chat is.
func (a *Adapter) Completion(ctx context.Context, req *providers.CompletionRequest) (*providers.Response, error) {
	return a.post(ctx, a.completions, req.Completion.Encode(req.UpstreamModel, !req.NoStreamUsage), req.Header, maxResponseBytes)
}

// CompletionStream implements providers.Completer.
func (a *Adapter) CompletionStream(ctx context.Context, req *providers.CompletionRequest) (providers.Stream, error) {
	return a.stream(ctx, a.completions, req.Completion.Encode(req.UpstreamModel, !req.NoStreamUsage), req.Header)
}

// Embed implements providers.Embedder: POST {base_url}/embeddings,
// forwarded losslessly with the model replaced.
func (a *Adapter) Embed(ctx context.Context, req *providers.EmbedRequest) (*providers.Response, error) {
	return a.post(ctx, a.embeddings, req.Embedding.Encode(req.UpstreamModel), req.Header, maxEmbeddingResponseBytes)
}

// post sends a non-streaming request to endpoint and reads a response body
// of at most limit bytes. first_token bounds the wait for the response
// headers, and total bounds the whole call including the body.
func (a *Adapter) post(ctx context.Context, endpoint string, body []byte, header http.Header, limit int) (*providers.Response, error) {
	c := a.newCall(ctx)
	defer c.stop()

	resp, err := a.do(c, endpoint, body, header, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	ttft := time.Since(c.start)
	c.phase.Stop() // the first token has arrived; total still applies
	rid := a.requestID(resp.Header)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, a.statusError(resp, rid)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return nil, c.classify(err, rid)
	}
	if len(data) > limit {
		return nil, &providers.Error{
			Kind: providers.KindServerError, Status: resp.StatusCode, Provider: a.id, UpstreamRequestID: rid,
			Message: fmt.Sprintf("upstream response exceeds %d bytes", limit),
		}
	}
	out := &providers.Response{Status: resp.StatusCode, Body: data, UpstreamRequestID: rid, TTFT: ttft}
	var parsed struct {
		Usage *schema.Usage `json:"usage"`
	}
	if json.Unmarshal(data, &parsed) == nil {
		out.Usage = parsed.Usage
	}
	return out, nil
}

// stream sends a streaming request to endpoint and returns once the first
// event has arrived. first_token bounds the wait for the first event, idle
// each gap between events, and total the whole call.
func (a *Adapter) stream(ctx context.Context, endpoint string, body []byte, header http.Header) (providers.Stream, error) {
	c := a.newCall(ctx)
	resp, err := a.do(c, endpoint, body, header, true)
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
	s := newStream(a, c, resp.Body, rid)
	first, err := s.read()
	s.ttft = time.Since(c.start)
	switch {
	case errors.Is(err, io.EOF):
		// [DONE] before any chunk: an empty but well-formed stream.
		s.done = true
		_ = s.Close()
	case err != nil:
		_ = s.Close()
		return nil, err
	default:
		s.pending = first
	}
	return s, nil
}

// do sends body to endpoint and returns the response once its headers
// arrive.
func (a *Adapter) do(c *call, endpoint string, body []byte, header http.Header, stream bool) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(c.ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &providers.Error{Kind: providers.KindTransport, Provider: a.id, Message: "could not build the upstream request", Err: err}
	}
	h := httpReq.Header
	for _, name := range a.forward {
		for _, v := range header.Values(name) {
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
		h.Set("Authorization", "Bearer "+a.apiKey)
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, c.classify(err, "")
	}
	return resp, nil
}

// statusError turns a non-2xx response into an error. Only client errors
// keep the upstream body, and only when it is an OpenAI error envelope
// without the provider key (see clientErrorBody): a 401 or 403 body may
// echo parts of our credential, and 5xx bodies are the upstream's
// internals.
func (a *Adapter) statusError(resp *http.Response, rid string) *providers.Error {
	e := &providers.Error{Status: resp.StatusCode, Provider: a.id, UpstreamRequestID: rid}
	code := resp.StatusCode
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		e.Kind = providers.KindAuth
		e.Message = fmt.Sprintf("upstream rejected the provider credential (HTTP %d)", code)
		return e
	case code == http.StatusTooManyRequests:
		e.Kind = providers.KindRateLimited
		e.Message = "upstream rate limit reached (HTTP 429)"
		e.RetryAfter = retryAfter(resp.Header, time.Now())
	case code >= 400 && code <= 499:
		e.Kind = providers.KindClientError
		e.Message = fmt.Sprintf("upstream rejected the request (HTTP %d)", code)
	case code >= 500 && code <= 599:
		e.Kind = providers.KindServerError
		e.Message = fmt.Sprintf("upstream returned HTTP %d", code)
		return e
	default:
		e.Kind = providers.KindServerError
		e.Message = fmt.Sprintf("upstream returned unexpected HTTP %d", code)
		return e
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes+1))
	if err == nil && len(body) <= maxErrorBodyBytes {
		e.Body = a.clientErrorBody(body)
	}
	return e
}

// clientErrorBody returns the client-error body to forward, or nil when
// the gateway should send its generic envelope instead. The body must be a
// JSON object whose error member is an object, as in
// {"error":{"message":…}}, and no string in it, object keys included, may
// contain the provider key once decoded: a JSON escape such as \u002d
// hides the key from the raw bytes, but the client decodes it. What is
// forwarded is a canonical re-encoding, token by token in the upstream's
// order: strings are re-escaped (without escaping <, > and &) and numbers
// keep their literal text, so no escape the check did not see can reach
// the client.
func (a *Adapter) clientErrorBody(body []byte) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return nil
	}
	if e := bytes.TrimSpace(top["error"]); len(e) == 0 || e[0] != '{' {
		return nil
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Each open object or array counts the tokens written into it: in an
	// object, keys are the even ones and values the odd ones.
	var open []struct {
		object bool
		n      int
	}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return buf.Bytes()
		}
		if err != nil {
			return nil
		}
		if d, ok := tok.(json.Delim); ok && (d == '}' || d == ']') {
			buf.WriteRune(rune(d))
			open = open[:len(open)-1]
			continue
		}
		if n := len(open); n > 0 {
			f := &open[n-1]
			switch {
			case f.object && f.n%2 == 1:
				buf.WriteByte(':')
			case f.n > 0:
				buf.WriteByte(',')
			}
			f.n++
		} else if buf.Len() > 0 {
			return nil // a second top-level value
		}
		switch v := tok.(type) {
		case json.Delim:
			buf.WriteRune(rune(v))
			open = append(open, struct {
				object bool
				n      int
			}{object: v == '{'})
		case string:
			if a.containsKey([]byte(v)) || enc.Encode(v) != nil {
				return nil
			}
			buf.Truncate(buf.Len() - 1) // Encode's newline
		case json.Number:
			buf.WriteString(v.String())
		case bool:
			buf.WriteString(strconv.FormatBool(v))
		case nil:
			buf.WriteString("null")
		}
	}
}

// requestID returns the upstream's x-request-id, or "" unless it is 1–128
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

// retryAfter parses Retry-After (delay seconds or an HTTP date), falling
// back to OpenAI's retry-after-ms. It returns 0 if neither is usable.
func retryAfter(h http.Header, now time.Time) time.Duration {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 && secs < 1e9 {
			return time.Duration(secs * float64(time.Second))
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := t.Sub(now); d > 0 {
				return d
			}
			return 0
		}
	}
	if v := strings.TrimSpace(h.Get("Retry-After-Ms")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 && ms < 1e12 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	return 0
}
