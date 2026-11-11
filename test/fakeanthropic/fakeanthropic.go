// Package fakeanthropic is a scriptable fake of Anthropic's Messages API,
// built on httptest.Server, for the anthropic adapter's tests and for
// running the gateway's conformance suite against an Anthropic-backed
// deployment.
//
// It serves POST /v1/messages, streaming and non-streaming. It checks the
// x-api-key and anthropic-version headers and validates the request the
// way the real API does for the fields it knows, including rejecting
// unknown top-level fields and a temperature above 1. The canned answer
// follows the request: tools produce tool_use blocks (one per tool unless
// tool_choice.disable_parallel_tool_use is set), native structured output
// (output_config.format, whose schema must keep to the supported subset)
// produces JSON, and stop sequences truncate the text. The canned content, tool call IDs
// and tool arguments are fakeupstream's (Text, JSONText, ToolCallID,
// ToolArguments), so a conversation through either fake gives the same
// answers. Streams follow the documented event sequence, a ping included.
//
// Errors, delays, connection faults, mid-stream error events, stop reasons
// and usage are scripted per test with [Server.Script]; [Behavior.Replay]
// sends a recorded or handcrafted fixture instead of the canned answer.
// Every request is recorded, including when its client went away, using
// fakeupstream's Request type. [Server.OpenAI] adapts the server to the
// method set of *fakeupstream.Server, for the conformance suite.
package fakeanthropic

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

	"switchyard/test/fakeupstream"
)

// maxBodyBytes caps the request bodies the fake reads; larger ones get 413.
const maxBodyBytes = 64 << 20

// Request is a recorded request and what the fake did with it. It is
// fakeupstream's type, so code that inspects requests works with both fakes.
type Request = fakeupstream.Request

// Fault is a connection-level failure the fake injects into a response.
type Fault = fakeupstream.Fault

// The faults, as in fakeupstream.
const (
	// FaultNone answers normally.
	FaultNone = fakeupstream.FaultNone
	// FaultDrop closes the TCP connection abruptly.
	FaultDrop = fakeupstream.FaultDrop
	// FaultHang stops responding until the client goes away or the server
	// is closed.
	FaultHang = fakeupstream.FaultHang
)

// Usage overrides the token counts of the canned answer.
type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

// Behavior scripts how the fake answers. The zero value answers
// immediately and successfully. The fields shared with fakeupstream mean
// the same.
type Behavior struct {
	// Status, when non-zero, makes every request fail with this HTTP status
	// after TTFT, before anything else is checked except the headers.
	Status int
	// Body is the response body for Status. When nil, the fake sends an
	// Anthropic error that matches the status (see [StatusBody]).
	Body []byte
	// Header holds extra headers added to every response, for example
	// Retry-After on a 429.
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
	// FaultAfterChunks is how many stream events, pings included, are sent
	// before Fault.
	FaultAfterChunks int
	// ExtraFields are added to the top level of every message and every
	// stream event, to model fields a client does not know. Each value
	// must be valid JSON.
	ExtraFields map[string]json.RawMessage

	// StopReason, when set, replaces the canned stop_reason, for example
	// refusal, max_tokens or pause_turn.
	StopReason string
	// Usage, when set, replaces the canned token counts.
	Usage *Usage
	// ErrorEvent, when set, makes a stream send an error event of this
	// type (such as overloaded_error) after ErrorAfterEvents events, and
	// then end without message_stop.
	ErrorEvent string
	// ErrorAfterEvents is how many events, pings included, precede
	// ErrorEvent.
	ErrorAfterEvents int
	// Replay, when set, is sent with status 200 in place of the canned
	// answer, after the request is validated: a Message JSON body for a
	// non-streaming request, an SSE stream for a streaming one. Delays and
	// faults do not apply to it.
	Replay []byte
}

// Options configure a Server.
type Options struct {
	// Models lists the models the fake knows; any other gets a 404
	// not_found_error. When empty, every model is accepted.
	Models []string
	// APIKey, when set, must be presented in x-api-key; any other value
	// gets a 401 authentication_error.
	APIKey string
	// DisableRecording turns request recording off, for load tests.
	DisableRecording bool
}

// record is the mutable state behind a Request. Its req is guarded by
// Server.mu.
type record struct {
	req  Request
	done chan struct{} // closed when the handler returns
}

// Server is a running fake Messages API. Its methods are safe for
// concurrent use.
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

// New starts a fake. Call Close when done with it.
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

// BaseURL is what a gateway provider's base_url points at. For Anthropic
// that is the API root, URL() itself: requests go to BaseURL() +
// "/v1/messages".
func (s *Server) BaseURL() string { return s.ts.URL }

// Close releases hanging and delayed handlers, then shuts the server down
// and waits for every handler to return.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.closing)
		s.ts.Close()
	})
}

// Script sets the behavior for every request received from now on. A
// request in flight keeps the behavior it started with.
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
			return nil, fmt.Errorf("fakeanthropic: waiting for request %d of %d to finish: %w", i+1, n, ctx.Err())
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
			return nil, fmt.Errorf("fakeanthropic: waiting for %d requests: %w", n, ctx.Err())
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
	w.Header().Set("Request-Id", requestID)

	body, readErr := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	rec := s.begin(r, body, requestID)
	if rec != nil {
		gone := make(chan struct{})
		stop := context.AfterFunc(r.Context(), func() {
			defer close(gone)
			s.update(rec, func(req *Request) { req.ClientGoneAt = time.Now() })
		})
		defer func() {
			// As in fakeupstream: a context done before this point means
			// the client left, whether or not the AfterFunc ran.
			if !stop() {
				<-gone
			} else if r.Context().Err() != nil {
				s.update(rec, func(req *Request) { req.ClientGoneAt = time.Now() })
			}
			s.update(rec, func(req *Request) { req.DoneAt = time.Now() })
			close(rec.done)
		}()
	}

	out := &responder{s: s, w: w, rc: http.NewResponseController(w), rec: rec, requestID: requestID}
	switch {
	case readErr != nil:
		out.error(http.StatusBadRequest, typeInvalidRequest, "Could not read the request body.")
	case len(body) > maxBodyBytes:
		out.error(http.StatusRequestEntityTooLarge, typeRequestTooLarge, "Request exceeds the maximum allowed number of bytes.")
	case s.opts.APIKey != "" && r.Header.Get("X-Api-Key") != s.opts.APIKey:
		out.error(http.StatusUnauthorized, typeAuthentication, "invalid x-api-key")
	case r.Header.Get("Anthropic-Version") == "":
		out.error(http.StatusBadRequest, typeInvalidRequest, "anthropic-version: header is required")
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		s.messages(r.Context(), out, body)
	default:
		out.error(http.StatusNotFound, typeNotFound, fmt.Sprintf("Not Found: %s %s", r.Method, r.URL.Path))
	}
}

// messages serves POST /v1/messages.
func (s *Server) messages(ctx context.Context, out *responder, body []byte) {
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

	req, aerr := parseRequest(body)
	if aerr != nil {
		out.error(aerr.status, aerr.typ, aerr.message)
		return
	}
	if len(s.opts.Models) > 0 && !slices.Contains(s.opts.Models, req.model) {
		out.error(http.StatusNotFound, typeNotFound, "model: "+req.model)
		return
	}
	if b.Replay != nil {
		ct := "application/json"
		if req.stream {
			ct = "text/event-stream; charset=utf-8"
		}
		out.w.Header().Set("Content-Type", ct)
		out.writeHeader(http.StatusOK)
		out.write(b.Replay)
		return
	}

	p := newPlan(req, strings.TrimPrefix(out.requestID, "req_fake_"), b)
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
	out.write(p.message(b.ExtraFields))
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

	events := p.events(b.ExtraFields)
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
		if b.ErrorEvent != "" && i == b.ErrorAfterEvents {
			out.event(errorEvent(b.ErrorEvent))
			return
		}
		if !out.event(ev) {
			return
		}
	}
	if b.Fault != FaultNone && b.FaultAfterChunks >= len(events) {
		s.fault(ctx, out, b.Fault)
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
	s         *Server
	w         http.ResponseWriter
	rc        *http.ResponseController
	rec       *record
	requestID string
}

func (o *responder) writeHeader(status int) {
	o.s.update(o.rec, func(r *Request) { r.Status = status })
	o.w.WriteHeader(status)
}

// write writes b and reports whether it succeeded.
func (o *responder) write(b []byte) bool {
	o.s.update(o.rec, func(r *Request) { r.ResponseBody = append(r.ResponseBody, b...) })
	_, err := o.w.Write(b) // #nosec G705 -- a test fake answering JSON and SSE, never HTML
	return err == nil
}

func (o *responder) flush() error {
	if err := o.rc.Flush(); err != nil {
		return fmt.Errorf("fakeanthropic: flush: %w", err)
	}
	return nil
}

// event writes one named SSE event, flushes it, and reports whether both
// succeeded.
func (o *responder) event(ev event) bool {
	frame := make([]byte, 0, len(ev.data)+len(ev.name)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, ev.name...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, ev.data...)
	frame = append(frame, "\n\n"...)
	if !o.write(frame) || o.flush() != nil {
		return false
	}
	o.s.update(o.rec, func(r *Request) { r.Chunks++ })
	return true
}

// error writes an Anthropic error.
func (o *responder) error(status int, typ, message string) {
	o.scripted(status, errorBody(typ, message, o.requestID))
}

// scripted writes status with body, or with the default error for status
// when body is nil.
func (o *responder) scripted(status int, body []byte) {
	if body == nil {
		body = StatusBody(status)
	}
	o.w.Header().Set("Content-Type", "application/json")
	o.writeHeader(status)
	o.write(body)
}
