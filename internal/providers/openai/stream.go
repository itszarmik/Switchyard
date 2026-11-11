package openai

import (
	"errors"
	"fmt"
	"io"
	"time"

	"switchyard/internal/providers"
	"switchyard/internal/schema"
	"switchyard/internal/sse"
)

// Bounds on draining what follows [DONE], so the connection can be reused.
const (
	drainTimeout  = 250 * time.Millisecond
	maxDrainBytes = 4 << 10
)

// stream implements providers.Stream over an upstream SSE body.
type stream struct {
	a    *Adapter
	c    *call
	body io.ReadCloser
	r    *sse.Reader
	rid  string
	ttft time.Duration

	pending *schema.ChatChunk // the first chunk, read before ChatStream returned
	usage   *schema.Usage
	done    bool  // [DONE] was read
	err     error // sticky failure
	closed  bool
}

var _ providers.Stream = (*stream)(nil)

func newStream(a *Adapter, c *call, body io.ReadCloser, rid string) *stream {
	return &stream{a: a, c: c, body: body, r: sse.NewReader(body, sse.ReaderOptions{}), rid: rid}
}

// Next implements providers.Stream.
func (s *stream) Next() (*schema.ChatChunk, error) {
	if p := s.pending; p != nil {
		s.pending = nil
		return p, nil
	}
	if s.done {
		return nil, io.EOF
	}
	if s.err != nil {
		return nil, s.err
	}
	chunk, err := s.read()
	if errors.Is(err, io.EOF) {
		s.done = true
	} else if err != nil {
		s.err = err
	}
	return chunk, err
}

// read returns the next chunk, io.EOF after [DONE], or a *providers.Error.
// Events of a type other than message are skipped, except error events.
// A payload that is not a valid chunk is still returned, with only Raw set,
// so that it is forwarded as the upstream sent it.
//
// Only the events read returns count for the first_token and idle
// timeouts: an upstream that sends nothing but skipped events (such as
// pings) still times out.
func (s *stream) read() (*schema.ChatChunk, error) {
	for {
		ev, err := s.r.Next()
		if err != nil {
			return nil, s.readError(err)
		}
		switch ev.Type {
		case "", "message":
			s.c.eventArrived()
		case "error":
			s.c.eventArrived()
			return nil, s.serverError("upstream sent an error event")
		default:
			continue
		}
		if sse.IsDone(ev.Data) {
			return nil, io.EOF
		}
		chunk, perr := schema.ParseChatChunk(ev.Data)
		if perr != nil {
			chunk = &schema.ChatChunk{Raw: ev.Data}
		}
		if chunk.IsError() {
			return nil, s.serverError("upstream reported an error mid-stream")
		}
		if chunk.Usage != nil {
			s.usage = chunk.Usage
		}
		return chunk, nil
	}
}

func (s *stream) readError(err error) error {
	switch {
	case s.c.ctx.Err() != nil:
		return s.c.classify(err, s.rid)
	case errors.Is(err, io.EOF):
		return s.serverError("upstream ended the stream without [DONE]")
	case errors.Is(err, sse.ErrEventTooLarge):
		return s.serverError(fmt.Sprintf("upstream sent an event larger than %d bytes", sse.DefaultMaxEventSize))
	default:
		return s.c.classify(err, s.rid)
	}
}

func (s *stream) serverError(msg string) *providers.Error {
	return &providers.Error{
		Kind: providers.KindServerError, Status: 0, Provider: s.a.id, UpstreamRequestID: s.rid, Message: msg,
	}
}

// Usage implements providers.Stream.
func (s *stream) Usage() *schema.Usage { return s.usage }

// UpstreamRequestID implements providers.Stream.
func (s *stream) UpstreamRequestID() string { return s.rid }

// TTFT implements providers.Stream.
func (s *stream) TTFT() time.Duration { return s.ttft }

// Close implements providers.Stream.
//
// After [DONE], only the end of the response should remain, so Close reads
// it, up to maxDrainBytes and for at most drainTimeout, to give the
// connection back to the pool. Otherwise, or if the drain does not reach
// the end in time, canceling the call's context before closing the body
// makes the transport drop the connection rather than read a stream nobody
// wants.
func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.done {
		t := time.AfterFunc(drainTimeout, func() { s.c.cancel(errCallDone) })
		_, _ = io.Copy(io.Discard, io.LimitReader(s.body, maxDrainBytes))
		t.Stop()
	}
	s.c.stop()
	_ = s.body.Close()
	return nil
}
