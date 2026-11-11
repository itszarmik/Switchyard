// Package fakeupstream is a scriptable fake of an OpenAI-wire upstream, built
// on httptest.Server, for the conformance suite, the load harness and any
// test that needs a provider to talk to.
//
// It serves POST /v1/chat/completions (streaming and non-streaming) and
// GET /v1/models. The canned answer follows the request the way OpenAI's
// does: tools produce tool calls (one per tool when parallel tool calls are
// allowed), response_format json_object or json_schema produces JSON, n
// produces n choices, stop sequences truncate the text, and a usage chunk is
// streamed only when stream_options.include_usage is true. The request's
// model is echoed, and every response carries an x-request-id header.
//
// Errors, delays and connection faults are scripted per test with
// [Server.Script]. Every request is recorded, including when its client went
// away, so tests can assert what reached the upstream and how quickly a
// cancellation propagated.
package fakeupstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxBodyBytes caps the request bodies the fake reads; larger ones get 413.
const maxBodyBytes = 64 << 20

// Fault is a connection-level failure the fake injects into a response.
type Fault int

const (
	// FaultNone answers normally.
	FaultNone Fault = iota
	// FaultDrop closes the TCP connection abruptly, without finishing the
	// response.
	FaultDrop
	// FaultHang stops responding until the client goes away or the server
	// is closed.
	FaultHang
)

// Behavior scripts how the fake answers chat completion requests. The zero
// value answers immediately and successfully.
type Behavior struct {
	// Status, when non-zero, makes every chat completion fail with this
	// HTTP status after TTFT, before anything else is checked except the
	// API key.
	Status int
	// Body is the response body for Status. When nil, the fake sends an
	// OpenAI error envelope that matches the status (see [StatusBody]).
	Body []byte
	// Header holds extra headers added to every chat completion response,
	// for example Retry-After on a 429.
	Header http.Header
	// TTFT delays the response headers of a non-streaming response, and the
	// first event of a stream (whose headers are sent at once).
	TTFT time.Duration
	// ChunkDelay is the pause between consecutive stream events.
	ChunkDelay time.Duration
	// Fault, when not FaultNone, is injected in place of a successful
	// answer: before any byte of a non-streaming response, or after
	// FaultAfterChunks events of a stream (0 means right after the headers).
	Fault Fault
	// FaultAfterChunks is how many stream events are sent before Fault.
	FaultAfterChunks int
	// ExtraFields are added to the top level of every completion and every
	// stream chunk, to model fields a client or gateway does not know. Each
	// value must be valid JSON.
	ExtraFields map[string]json.RawMessage
}

// Options configure a Server.
type Options struct {
	// Models lists the models the fake knows, for GET /v1/models and for
	// the 404 model_not_found answer. When empty, every model is accepted
	// and GET /v1/models returns an empty list.
	Models []string
	// APIKey, when set, must be presented as "Authorization: Bearer APIKey";
	// any other value gets OpenAI's 401 invalid_api_key.
	APIKey string
	// DisableRecording turns request recording off, for load tests.
	DisableRecording bool
}

// Request is a recorded request and what the fake did with it.
type Request struct {
	// Method, Path, Header and Body are what the fake received.
	Method string
	Path   string
	Header http.Header
	Body   []byte
	// ReceivedAt is when the handler started.
	ReceivedAt time.Time
	// RequestID is the x-request-id the fake sent back.
	RequestID string
	// Status is the status the fake sent, or 0 if it sent none.
	Status int
	// ResponseBody is every response byte the fake wrote.
	ResponseBody []byte
	// Chunks counts the stream events sent, excluding data: [DONE].
	Chunks int
	// ClientGoneAt is when the request's context was done while the
	// handler was still running: the client disconnected or canceled. It is
	// zero if the handler finished first.
	ClientGoneAt time.Time
	// DoneAt is when the handler returned; zero while it is running.
	DoneAt time.Time
}

// record is the mutable state behind a Request. Its req is guarded by
// Server.mu.
type record struct {
	req  Request
	done chan struct{} // closed when the handler returns
}

// Server is a running fake upstream. Its methods are safe for concurrent use.
type Server struct {
	opts      Options
	ts        *httptest.Server
	closing   chan struct{}
	closeOnce sync.Once
	seq       atomic.Uint64

	mu       sync.Mutex
	behavior Behavior
	records  []*record
	changed  chan struct{} // closed and replaced whenever a request is recorded
}

// New starts a fake upstream. Call Close when done with it.
func New(opts Options) *Server {
	s := &Server{
		opts:    opts,
		closing: make(chan struct{}),
		changed: make(chan struct{}),
	}
	s.opts.Models = append([]string(nil), opts.Models...)
	s.ts = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	return s
}

// URL is the server's root URL, such as http://127.0.0.1:1234.
func (s *Server) URL() string { return s.ts.URL }

// BaseURL is the OpenAI-style API root, URL() + "/v1": what a client or a
// gateway provider's base_url points at.
func (s *Server) BaseURL() string { return s.ts.URL + "/v1" }

// Close releases hanging and delayed handlers, then shuts the server down
// and waits for every handler to return.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.closing)
		s.ts.Close()
	})
}

// Script sets the behavior for every chat completion request received from
// now on. A request in flight keeps the behavior it started with.
func (s *Server) Script(b Behavior) {
	b.Header = b.Header.Clone()
	s.mu.Lock()
	s.behavior = b
	s.mu.Unlock()
}

// Reset restores the default behavior and forgets the recorded requests.
func (s *Server) Reset() {
	s.mu.Lock()
	s.behavior = Behavior{}
	s.records = nil
	s.mu.Unlock()
}

// Requests returns a snapshot of the recorded requests, oldest first.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.records))
	for i, rec := range s.records {
		out[i] = rec.snapshot()
	}
	return out
}

// WaitReceived waits until at least n requests have been recorded, whether
// or not they have been handled yet. It fails when ctx is done first.
func (s *Server) WaitReceived(ctx context.Context, n int) error {
	_, err := s.received(ctx, n)
	return err
}

// Await waits until at least n requests have been recorded and the first n
// have been completely handled, then returns those n. It fails when ctx is
// done first.
func (s *Server) Await(ctx context.Context, n int) ([]Request, error) {
	recs, err := s.received(ctx, n)
	if err != nil {
		return nil, err
	}
	for i, rec := range recs {
		select {
		case <-rec.done:
		case <-ctx.Done():
			return nil, fmt.Errorf("fakeupstream: waiting for request %d of %d to finish: %w", i+1, n, ctx.Err())
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(recs))
	for i, rec := range recs {
		out[i] = rec.snapshot()
	}
	return out, nil
}

// received waits until at least n requests have been recorded and returns
// the first n records.
func (s *Server) received(ctx context.Context, n int) ([]*record, error) {
	for {
		s.mu.Lock()
		if len(s.records) >= n {
			recs := append([]*record(nil), s.records[:n]...)
			s.mu.Unlock()
			return recs, nil
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, fmt.Errorf("fakeupstream: waiting for %d requests: %w", n, ctx.Err())
		}
	}
}

// snapshot copies the record's request. The caller holds Server.mu.
func (rec *record) snapshot() Request {
	r := rec.req
	r.ResponseBody = bytes.Clone(rec.req.ResponseBody)
	return r
}

// begin records a new request; it returns nil when recording is disabled.
func (s *Server) begin(r *http.Request, body []byte, requestID string) *record {
	if s.opts.DisableRecording {
		return nil
	}
	rec := &record{
		req: Request{
			Method:     r.Method,
			Path:       r.URL.Path,
			Header:     r.Header.Clone(),
			Body:       body,
			ReceivedAt: time.Now(),
			RequestID:  requestID,
		},
		done: make(chan struct{}),
	}
	s.mu.Lock()
	s.records = append(s.records, rec)
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
	return rec
}

// update applies f to rec under the lock; it does nothing for a nil rec.
func (s *Server) update(rec *record, f func(*Request)) {
	if rec == nil {
		return
	}
	s.mu.Lock()
	f(&rec.req)
	s.mu.Unlock()
}

func (s *Server) currentBehavior() Behavior {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.behavior
}

// serveHTTP records the request, watches for the client going away, and
// dispatches it.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := fmt.Sprintf("req_fake_%08d", s.seq.Add(1))
	w.Header().Set("X-Request-Id", requestID)

	body, readErr := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	rec := s.begin(r, body, requestID)
	if rec != nil {
		gone := make(chan struct{})
		stop := context.AfterFunc(r.Context(), func() {
			defer close(gone)
			s.update(rec, func(req *Request) { req.ClientGoneAt = time.Now() })
		})
		defer func() {
			// net/http cancels the context only after the handler returns,
			// so a context done before this point means the client left.
			// The context closes its Done channel before it starts its
			// AfterFunc callbacks, so the handler can see Done and return
			// before the callback starts; stop then succeeds and the
			// departure is recorded here instead.
			if !stop() {
				<-gone
			} else if r.Context().Err() != nil {
				s.update(rec, func(req *Request) { req.ClientGoneAt = time.Now() })
			}
			s.update(rec, func(req *Request) { req.DoneAt = time.Now() })
			close(rec.done)
		}()
	}

	out := &responder{s: s, w: w, rc: http.NewResponseController(w), rec: rec}
	switch {
	case readErr != nil:
		out.error(http.StatusBadRequest, apiError{message: "Could not read the request body.", typ: typeInvalidRequest})
	case len(body) > maxBodyBytes:
		out.error(http.StatusRequestEntityTooLarge, apiError{message: "Request body too large.", typ: typeInvalidRequest})
	case !s.authorized(r):
		out.error(http.StatusUnauthorized, s.authError(r))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		s.chat(r.Context(), out, body)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		s.models(out)
	default:
		out.error(http.StatusNotFound, apiError{
			message: fmt.Sprintf("Invalid URL (%s %s)", r.Method, r.URL.Path),
			typ:     typeInvalidRequest,
		})
	}
}

// authorized checks the bearer key when Options.APIKey is set.
func (s *Server) authorized(r *http.Request) bool {
	if s.opts.APIKey == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+s.opts.APIKey
}

// authError is OpenAI's 401. Like OpenAI, it echoes a masked form of the
// presented key, which a gateway must never pass on to its own client.
func (s *Server) authError(r *http.Request) apiError {
	auth := r.Header.Get("Authorization")
	key, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || key == "" {
		return apiError{
			message: "You didn't provide an API key. You need to provide your API key in an Authorization header using Bearer auth (i.e. Authorization: Bearer YOUR_KEY).",
			typ:     typeInvalidRequest,
		}
	}
	return apiError{
		message: fmt.Sprintf("Incorrect API key provided: %s. You can find your API key at https://platform.openai.com/account/api-keys.", maskKey(key)),
		typ:     typeInvalidRequest,
		code:    "invalid_api_key",
	}
}

// maskKey keeps the first and last four characters of key, as OpenAI does.
func maskKey(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + strings.Repeat("*", len(key)-8) + key[len(key)-4:]
}

// chat serves POST /v1/chat/completions.
func (s *Server) chat(ctx context.Context, out *responder, body []byte) {
	b := s.currentBehavior()
	for name, values := range b.Header {
		out.w.Header()[name] = append([]string(nil), values...)
	}

	if b.Status != 0 {
		if !s.sleep(ctx, b.TTFT) {
			return
		}
		out.scripted(b.Status, b.Body)
		return
	}

	req, aerr := parseChatRequest(body)
	if aerr != nil {
		out.error(aerr.status, *aerr)
		return
	}
	if len(s.opts.Models) > 0 && !slices.Contains(s.opts.Models, req.model) {
		out.error(http.StatusNotFound, apiError{
			message: fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", req.model),
			typ:     typeInvalidRequest,
			code:    "model_not_found",
		})
		return
	}

	p := newPlan(req, strings.TrimPrefix(out.w.Header().Get("X-Request-Id"), "req_fake_"))
	if req.stream {
		s.stream(ctx, out, b, p)
		return
	}
	if !s.sleep(ctx, b.TTFT) {
		return
	}
	if b.Fault != FaultNone {
		s.fault(ctx, out, b.Fault)
		return
	}
	out.w.Header().Set("Content-Type", "application/json")
	out.writeHeader(http.StatusOK)
	out.write(p.completion(b.ExtraFields))
}

// stream sends p as server-sent events.
func (s *Server) stream(ctx context.Context, out *responder, b Behavior, p *plan) {
	h := out.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	out.writeHeader(http.StatusOK)
	if out.flush() != nil {
		return
	}

	events := p.chunks(b.ExtraFields)
	for i, ev := range events {
		delay := b.ChunkDelay
		if i == 0 {
			delay = b.TTFT
		}
		if !s.sleep(ctx, delay) {
			return
		}
		if b.Fault != FaultNone && i == b.FaultAfterChunks {
			s.fault(ctx, out, b.Fault)
			return
		}
		if !out.event(ev) {
			return
		}
	}
	if b.Fault != FaultNone && b.FaultAfterChunks >= len(events) {
		s.fault(ctx, out, b.Fault)
		return
	}
	if out.write([]byte("data: [DONE]\n\n")) {
		_ = out.flush()
	}
}

// fault injects f.
func (s *Server) fault(ctx context.Context, out *responder, f Fault) {
	switch f {
	case FaultHang:
		select {
		case <-ctx.Done():
		case <-s.closing:
		}
	case FaultDrop:
		// Hijack flushes what was written, then hands over the connection.
		conn, _, err := out.rc.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	case FaultNone:
	}
}

// models serves GET /v1/models.
func (s *Server) models(out *responder) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	list := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: []model{}}
	for _, id := range s.opts.Models {
		list.Data = append(list.Data, model{ID: id, Object: "model", Created: Created, OwnedBy: "fakeupstream"})
	}
	b, err := json.Marshal(list)
	if err != nil {
		out.error(http.StatusInternalServerError, apiError{message: err.Error(), typ: typeServer})
		return
	}
	out.w.Header().Set("Content-Type", "application/json")
	out.writeHeader(http.StatusOK)
	out.write(b)
}

// sleep waits for d. It returns false if the client went away or the server
// is closing first.
func (s *Server) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-s.closing:
		return false
	}
}

// responder writes a response and mirrors it into the request's record.
type responder struct {
	s   *Server
	w   http.ResponseWriter
	rc  *http.ResponseController
	rec *record
}

func (o *responder) writeHeader(status int) {
	o.s.update(o.rec, func(r *Request) { r.Status = status })
	o.w.WriteHeader(status)
}

// write writes b and reports whether it succeeded.
func (o *responder) write(b []byte) bool {
	o.s.update(o.rec, func(r *Request) { r.ResponseBody = append(r.ResponseBody, b...) })
	_, err := o.w.Write(b)
	return err == nil
}

func (o *responder) flush() error {
	if err := o.rc.Flush(); err != nil {
		return fmt.Errorf("fakeupstream: flush: %w", err)
	}
	return nil
}

// event writes one SSE data event, flushes it, and reports whether both
// succeeded.
func (o *responder) event(data []byte) bool {
	frame := make([]byte, 0, len(data)+8)
	frame = append(frame, "data: "...)
	frame = append(frame, data...)
	frame = append(frame, "\n\n"...)
	if !o.write(frame) || o.flush() != nil {
		return false
	}
	o.s.update(o.rec, func(r *Request) { r.Chunks++ })
	return true
}

// error writes an OpenAI error envelope.
func (o *responder) error(status int, e apiError) {
	o.scripted(status, e.envelope())
}

// scripted writes status with body, or with the default envelope for status
// when body is nil.
func (o *responder) scripted(status int, body []byte) {
	if body == nil {
		body = StatusBody(status)
	}
	o.w.Header().Set("Content-Type", "application/json")
	o.writeHeader(status)
	o.write(body)
}
