package gemini

import (
	"encoding/json"
	"errors"
	"io"
	"time"

	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// stream implements providers.Stream over a streamGenerateContent?alt=sse
// body, translating its GenerateContentResponse events into
// chat.completion.chunk events:
//
//   - the first event: a role chunk;
//   - text parts: content deltas, as they arrive;
//   - functionCall parts: collected, and sent when the finish reason
//     arrives, one tool_calls[i] delta per call with the encoded id, the
//     name and the whole arguments. Gemini sends each call whole, but a
//     call split over events (the same functionCall.id again) is merged
//     rather than sent twice;
//   - the event with the finishReason (or a blocked prompt's
//     promptFeedback): the finish_reason chunk, once;
//   - the end of the body: the usage-only chunk if the client asked for
//     usage, then the end of the stream. usageMetadata is cumulative, so
//     the last one counts.
//
// An event with a top-level error fails the stream with the kind its code
// maps to, and so does a finish reason that is not an answer (see
// finishReason); a body that ends before a finish reason is a server
// error. Every chunk has the same id (from the responseId) and created.
type stream struct {
	a    *Adapter
	c    *call
	body io.ReadCloser
	r    *eventReader
	ttft time.Duration

	includeUsage bool
	created      int64

	acc      *accumulator
	id       string
	model    string
	started  bool // the role chunk was queued
	finished bool // the finish chunk was queued
	norm     *schema.Usage

	pending []*schema.ChatChunk // chunks translated but not yet returned
	done    bool                // the body ended after a finish reason
	err     error               // sticky failure
	closed  bool
}

var _ providers.Stream = (*stream)(nil)

func newStream(a *Adapter, c *call, body io.ReadCloser, model string, includeUsage bool) *stream {
	return &stream{
		a: a, c: c, body: body, r: newEventReader(body),
		includeUsage: includeUsage, created: a.now().Unix(), acc: newAccumulator(a.id, model),
	}
}

// Next implements providers.Stream. The idle timeout runs only while Next
// waits for the upstream, not between calls.
func (s *stream) Next() (*schema.ChatChunk, error) {
	if len(s.pending) == 0 && !s.done && s.err == nil {
		s.c.resume()
		defer s.c.pause()
	}
	for {
		if len(s.pending) > 0 {
			c := s.pending[0]
			s.pending = s.pending[1:]
			return c, nil
		}
		if s.done {
			return nil, io.EOF
		}
		if s.err != nil {
			return nil, s.err
		}
		if err := s.step(); err != nil {
			s.err = err
		}
	}
}

// step reads one event and queues the chunks it translates to. It returns
// an error when the stream fails.
func (s *stream) step() error {
	gr, err := s.r.next()
	if errors.Is(err, io.EOF) {
		if !s.acc.finished {
			return s.serverError("upstream ended the stream without a finish reason")
		}
		s.done = true
		if s.includeUsage {
			s.emitUsage()
		}
		return nil
	}
	if err != nil {
		return s.a.streamReadError(s.c, err, s.acc.responseID())
	}
	s.c.eventArrived()
	if gr.Error != nil {
		return s.a.eventError(gr.Error, s.acc.responseID())
	}
	texts, err := s.acc.apply(gr)
	// apply took the event's usage even when its finish reason fails the
	// stream: that usage is what Google billed, so Usage reports it.
	s.norm = s.acc.usage.normalize()
	if err != nil {
		return err
	}
	if !s.started {
		s.started = true
		s.id, s.model = s.acc.completionID(), s.acc.modelName()
		empty := ""
		s.emit(chunkDelta{Role: "assistant", Content: &empty}, nil)
	}
	for _, t := range texts {
		text := t
		s.emit(chunkDelta{Content: &text}, nil)
	}
	if s.acc.finished && !s.finished {
		s.finished = true
		for i, tc := range s.acc.toolCalls() {
			fn := tc.Function
			s.emit(chunkDelta{ToolCalls: []toolCallDelta{{Index: i, ID: tc.ID, Type: "function", Function: &fn}}}, nil)
		}
		reason := s.acc.finish()
		s.emit(chunkDelta{}, &reason)
	}
	return nil
}

// Chunk wire types, in OpenAI's field order.
type (
	chunk struct {
		ID      string          `json:"id"`
		Object  string          `json:"object"`
		Created int64           `json:"created"`
		Model   string          `json:"model"`
		Choices []chunkChoice   `json:"choices"`
		Usage   json.RawMessage `json:"usage,omitempty"`
	}
	chunkChoice struct {
		Index        int             `json:"index"`
		Delta        chunkDelta      `json:"delta"`
		Logprobs     json.RawMessage `json:"logprobs"`
		FinishReason *string         `json:"finish_reason"`
	}
	chunkDelta struct {
		Role      string          `json:"role,omitempty"`
		Content   *string         `json:"content,omitempty"`
		ToolCalls []toolCallDelta `json:"tool_calls,omitempty"`
	}
	toolCallDelta struct {
		Index    int           `json:"index"`
		ID       string        `json:"id,omitempty"`
		Type     string        `json:"type,omitempty"`
		Function *functionArgs `json:"function,omitempty"`
	}
)

// emit queues a chunk with one choice. When the client asked for usage,
// it carries "usage":null, as OpenAI's chunks do.
func (s *stream) emit(d chunkDelta, finish *string) {
	c := chunk{
		ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
		Choices: []chunkChoice{{Index: 0, Delta: d, Logprobs: jsonNull, FinishReason: finish}},
	}
	if s.includeUsage {
		c.Usage = jsonNull
	}
	s.queue(c)
}

// emitUsage queues the usage-only chunk that ends the stream. A stream
// that reported no usage gets none.
func (s *stream) emitUsage() {
	if s.norm == nil {
		return
	}
	u, _ := json.Marshal(s.norm) // plain integers; cannot fail
	s.queue(chunk{
		ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
		Choices: []chunkChoice{}, Usage: u,
	})
}

func (s *stream) queue(c chunk) {
	raw, err := marshal(c)
	if err != nil {
		// Only the fixed wire types above are marshaled.
		s.err = s.serverError("could not encode a chunk")
		return
	}
	parsed, err := schema.ParseChatChunk(raw)
	if err != nil {
		s.err = s.serverError("could not encode a chunk")
		return
	}
	s.pending = append(s.pending, parsed)
}

func (s *stream) serverError(msg string) *providers.Error {
	return &providers.Error{
		Kind: providers.KindServerError, Provider: s.a.id, UpstreamRequestID: s.a.safeID(s.acc.responseID()), Message: msg,
	}
}

// Usage implements providers.Stream. It is the normalized usage of the
// last event that reported one.
func (s *stream) Usage() *schema.Usage { return s.norm }

// UpstreamRequestID implements providers.Stream: the responseId of the
// first event (see Adapter.safeID).
func (s *stream) UpstreamRequestID() string { return s.a.safeID(s.acc.responseID()) }

// TTFT implements providers.Stream.
func (s *stream) TTFT() time.Duration { return s.ttft }

// Close implements providers.Stream. A stream read to its end has
// consumed the whole body, so its connection goes back to the pool;
// otherwise canceling the call's context before closing the body makes the
// transport drop the connection.
func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.c.stop()
	_ = s.body.Close()
	return nil
}
