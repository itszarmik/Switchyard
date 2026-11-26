// Package fakegemini is a scriptable fake of Google's native Gemini API
// (generateContent and streamGenerateContent?alt=sse), built on
// httptest.Server, for the gemini adapter's tests and for running the
// gateway's conformance suite against a Gemini-backed deployment.
//
// It serves POST /v1beta/models/{model}:generateContent and
// :streamGenerateContent (which must ask for alt=sse). It checks the
// x-goog-api-key header, and fails loudly on what the gateway must never
// send: the key in the URL, a request without "store": false, sampling or
// other generationConfig fields the adapter does not map, thinkingBudget
// or the minimal thinking level, and any top-level field but contents,
// systemInstruction, tools, toolConfig, generationConfig and store. Like
// the real API it rejects a non-streaming maxOutputTokens above 32,768 (the
// Gemini 4 Carbon cap, applied to every model here), a conversation that
// ends with a model turn, function responses that do not answer exactly
// the calls of the model turn before them, and, in the current turn, a
// step whose first function call lacks its thought signature or carries
// one the fake did not issue for that call and model.
//
// The canned answer follows the request: tools produce one function call
// per declared function (Gemini calls in parallel), or the one
// allowedFunctionNames names, each with fakeupstream's ids and arguments
// and a thought signature on the first; JSON output produces
// fakeupstream.JSONText; stop sequences truncate fakeupstream.Text. A
// stream sends one event per word, the finish reason on the last, then a
// signature-only text part as Gemini does; function calls come whole in one
// event. Usage is cumulative across events.
//
// Errors, delays, connection faults, mid-stream error events, finish
// reasons, blocked prompts and usage are scripted with [Server.Script] or
// [Server.Sequence]; [Behavior.Replay] sends a fixture instead of the
// canned answer. Every request is recorded with fakeupstream's Request
// type. [Server.OpenAI] adapts the server to the method set of
// *fakeupstream.Server, for the conformance suite.
package fakegemini

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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

// APIPrefix is the API version path the fake serves; BaseURL includes it.
const APIPrefix = "/v1beta"

// Request is a recorded request and what the fake did with it. It is
// fakeupstream's type, so code that inspects requests works with every fake.
type Request = fakeupstream.Request

// Fault is a connection-level failure the fake injects into a response.
type Fault = fakeupstream.Fault

// The faults, as in fakeupstream.
const (
	FaultNone = fakeupstream.FaultNone
	FaultDrop = fakeupstream.FaultDrop
	FaultHang = fakeupstream.FaultHang
)

// Usage overrides the token counts of the canned answer, as usageMetadata
// reports them: Prompt includes Cached, and Candidates excludes Thoughts.
type Usage struct {
	Prompt, Cached, Candidates, ToolUsePrompt, Thoughts int
}

// Behavior scripts how the fake answers. The zero value answers
// immediately and successfully. The fields shared with fakeupstream mean
// the same.
type Behavior struct {
	// Status, when non-zero, makes every request fail with this HTTP status
	// after TTFT, before anything else is checked except the key.
	Status int
	// Body is the response body for Status. When nil, the fake sends a
	// Google error that matches the status (see [StatusBody]).
	Body []byte
	// Header holds extra headers added to every response.
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
	// Hold, when set, makes the request wait, before anything is sent,
	// until Hold is closed (or the client goes away, or the server closes).
	Hold <-chan struct{}
	// ExtraFields are added to the top level of every response and stream
	// event. Each value must be valid JSON.
	ExtraFields map[string]json.RawMessage

	// FinishReason, when set, replaces the canned finishReason, for
	// example SAFETY, MAX_TOKENS or MISSING_THOUGHT_SIGNATURE.
	FinishReason string
	// BlockReason, when set, answers with promptFeedback.blockReason and
	// no candidates, as Google does for a blocked prompt.
	BlockReason string
	// Usage, when set, replaces the canned token counts.
	Usage *Usage
	// NoCallIDs leaves functionCall.id out, as older models do.
	NoCallIDs bool
	// ErrorEvent, when set, makes a stream send an event with this Google
	// error body (see [ErrorBody]) after ErrorAfterEvents events, then
	// end.
	ErrorEvent []byte
	// ErrorAfterEvents is how many events precede ErrorEvent.
	ErrorAfterEvents int
	// Replay, when set, is sent with status 200 in place of the canned
	// answer, after the request is validated: a GenerateContentResponse
	// for generateContent, an SSE body for streamGenerateContent.
	Replay []byte
}

// Options configure a Server.
type Options struct {
	// Models lists the models the fake knows; any other gets a 404
	// NOT_FOUND. When empty, every model is accepted.
	Models []string
	// APIKey, when set, must be presented in x-goog-api-key; any other
	// value gets Google's 400 with ErrorInfo reason API_KEY_INVALID.
	APIKey string
	// DisableRecording turns request recording off, for load tests.
	DisableRecording bool
}

// Server is a running fake Gemini API. Its methods are safe for
// concurrent use.
type Server struct {
	opts      Options
	ts        *httptest.Server
	closing   chan struct{}
	closeOnce sync.Once
	seq       atomic.Uint64

	mu       sync.Mutex
	behavior Behavior
	queue    []Behavior // per-request behaviors, see Sequence
	records  []*record
	changed  chan struct{} // closed and replaced whenever a request is recorded
}

// New starts a fake. Call Close when done with it.
func New(opts Options) *Server {
	s := &Server{opts: opts, closing: make(chan struct{}), changed: make(chan struct{})}
	s.opts.Models = append([]string(nil), opts.Models...)
	s.ts = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	return s
}

// URL is the server's root URL, such as http://127.0.0.1:1234.
func (s *Server) URL() string { return s.ts.URL }

// BaseURL is what a gateway provider's base_url points at: the API
// version root, URL() + "/v1beta".
func (s *Server) BaseURL() string { return s.ts.URL + APIPrefix }

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

// Sequence scripts the next requests one by one: the first request
// received from now on gets bs[0], the second bs[1], and so on; once they
// are used up, requests get the behavior set by Script.
func (s *Server) Sequence(bs ...Behavior) {
	q := make([]Behavior, len(bs))
	for i, b := range bs {
		b.Header = b.Header.Clone()
		q[i] = b
	}
	s.mu.Lock()
	s.queue = q
	s.mu.Unlock()
}

// Reset restores the default behavior, drops any sequence and forgets the
// recorded requests.
func (s *Server) Reset() {
	s.mu.Lock()
	s.queue = nil
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
// have been completely handled, then returns those n.
func (s *Server) Await(ctx context.Context, n int) ([]Request, error) {
	recs, err := s.received(ctx, n)
	if err != nil {
		return nil, err
	}
	for i, rec := range recs {
		select {
		case <-rec.done:
		case <-ctx.Done():
			return nil, fmt.Errorf("fakegemini: waiting for request %d of %d to finish: %w", i+1, n, ctx.Err())
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
			return nil, fmt.Errorf("fakegemini: waiting for %d requests: %w", n, ctx.Err())
		}
	}
}

// record is the mutable state behind a Request. Its req is guarded by
// Server.mu.
type record struct {
	req  Request
	done chan struct{} // closed when the handler returns
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

// nextBehavior returns the behavior of a new request: the next one of
// the sequence, if any is left, else the scripted one.
func (s *Server) nextBehavior() Behavior {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) > 0 {
		b := s.queue[0]
		s.queue = s.queue[1:]
		return b
	}
	return s.behavior
}

// hold waits for b.Hold, if set. It returns false if the client went away
// or the server is closing first.
func (s *Server) hold(ctx context.Context, b Behavior) bool {
	if b.Hold == nil {
		return true
	}
	select {
	case <-b.Hold:
		return true
	case <-ctx.Done():
		return false
	case <-s.closing:
		return false
	}
}

// serveHTTP records the request, watches for the client going away, and
// dispatches it.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := fmt.Sprintf("resp_fake_%08d", s.seq.Add(1))

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
	model, method, routed := strings.Cut(strings.TrimPrefix(r.URL.Path, APIPrefix+"/models/"), ":")
	routed = routed && strings.HasPrefix(r.URL.Path, APIPrefix+"/models/") && model != ""
	query := r.URL.Query()
	switch {
	case readErr != nil:
		out.error(http.StatusBadRequest, "Could not read the request body.")
	case len(body) > maxBodyBytes:
		out.error(http.StatusRequestEntityTooLarge, "Request payload size exceeds the limit.")
	case query.Has("key"):
		// Not an error the real API gives: the gateway must never put the
		// key in a URL.
		out.error(http.StatusBadRequest, "fakegemini: the API key was sent in the URL")
	case s.opts.APIKey != "" && r.Header.Get("X-Goog-Api-Key") != s.opts.APIKey:
		out.error(http.StatusBadRequest, "API key not valid. Please pass a valid API key.", errorInfo("API_KEY_INVALID"))
	case r.Method != http.MethodPost || !routed || (method != "generateContent" && method != "streamGenerateContent"):
		out.error(http.StatusNotFound, fmt.Sprintf("Not Found: %s %s", r.Method, r.URL.Path))
	case method == "streamGenerateContent" && query.Get("alt") != "sse":
		out.error(http.StatusBadRequest, "fakegemini: streamGenerateContent without alt=sse")
	default:
		s.generate(r.Context(), out, model, method == "streamGenerateContent", body)
	}
}

// generate serves generateContent and streamGenerateContent.
func (s *Server) generate(ctx context.Context, out *responder, model string, stream bool, body []byte) {
	b := s.nextBehavior()
	for name, values := range b.Header {
		out.w.Header()[name] = append([]string(nil), values...)
	}
	if !s.hold(ctx, b) {
		return
	}
	if b.Status != 0 {
		if !s.sleep(ctx, b.TTFT) {
			return
		}
		out.scripted(b.Status, b.Body)
		return
	}

	req, aerr := parseRequest(body, model, stream)
	if aerr != nil {
		out.error(aerr.status, aerr.message)
		return
	}
	if len(s.opts.Models) > 0 && !slices.Contains(s.opts.Models, model) {
		out.error(http.StatusNotFound, fmt.Sprintf("models/%s is not found for API version v1beta, or is not supported for generateContent.", model))
		return
	}
	if b.Replay != nil {
		ct := "application/json; charset=UTF-8"
		if stream {
			ct = "text/event-stream"
		}
		out.w.Header().Set("Content-Type", ct)
		out.writeHeader(http.StatusOK)
		out.write(b.Replay)
		return
	}

	p := newPlan(req, out.requestID, b)
	if stream {
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
	out.w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	out.writeHeader(http.StatusOK)
	out.write(p.response(b.ExtraFields))
}

// stream sends p as server-sent events.
func (s *Server) stream(ctx context.Context, out *responder, b Behavior, p *plan) {
	h := out.w.Header()
	h.Set("Content-Type", "text/event-stream")
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
		if b.ErrorEvent != nil && i == b.ErrorAfterEvents {
			out.event(b.ErrorEvent)
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

// Signature is the thought signature the fake issues for the function
// call id of model, and the only one it accepts back for it: base64 of a
// hash of both, so a signature that was altered, moved to another call or
// sent to another model is rejected, as Google rejects one it did not
// issue.
func Signature(model, callID string) string {
	sum := sha256.Sum256([]byte("fakegemini\x00" + model + "\x00" + callID))
	return base64.StdEncoding.EncodeToString(bytes.Repeat(sum[:], 4))
}

// TextSignature is the signature the fake puts on the empty last text
// part of an answer without function calls. It is never checked.
func TextSignature(model string) string { return Signature(model, "text") }

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
		return fmt.Errorf("fakegemini: flush: %w", err)
	}
	return nil
}

// event writes one SSE event, "data: <json>" and a blank line, flushes
// it, and reports whether both succeeded.
func (o *responder) event(data []byte) bool {
	frame := make([]byte, 0, len(data)+8)
	frame = append(frame, "data: "...)
	frame = append(frame, data...)
	frame = append(frame, "\r\n\r\n"...)
	if !o.write(frame) || o.flush() != nil {
		return false
	}
	o.s.update(o.rec, func(r *Request) { r.Chunks++ })
	return true
}

// error writes a Google error.
func (o *responder) error(status int, message string, details ...any) {
	o.scripted(status, ErrorBody(status, message, details...))
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
