package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"switchyard/internal/providers"
	"switchyard/internal/schema"
	"switchyard/internal/sse"
)

// Bounds on draining what follows message_stop, so the connection can be
// reused.
const (
	drainTimeout  = 250 * time.Millisecond
	maxDrainBytes = 4 << 10
)

// maxBlocks bounds the content blocks of one stream. A real message has a
// handful; an upstream that sends more is broken, and the stream fails
// rather than keep translating it.
const maxBlocks = 10_000

// blockKind is what a content block of the stream maps to.
type blockKind int

const (
	blockIgnored  blockKind = iota // thinking, server tools and unknown types
	blockText                      // text deltas become content deltas
	blockToolCall                  // input_json_delta becomes arguments deltas
)

// streamBlock is the state of the content block being streamed. Blocks
// arrive one after another (start, deltas, stop), so only the current one
// is kept.
type streamBlock struct {
	index int // the Anthropic index
	kind  blockKind
	tool  int  // the OpenAI tool_calls index, for blockToolCall
	args  bool // an arguments delta was emitted, for blockToolCall
}

// stream implements providers.Stream over a Messages API event stream,
// translating it into chat.completion.chunk events:
//
//   - message_start: a role chunk;
//   - text deltas: content deltas;
//   - a tool_use block start: a tool_calls[i] chunk with the id, the name
//     and empty arguments, then one arguments chunk per non-empty
//     input_json_delta, at the same index; a tool block that ends without
//     any gets one arguments chunk of "{}", as a non-streamed response has;
//   - message_delta: the finish_reason chunk, once, and the usage;
//   - message_stop: the usage-only chunk if the client asked for usage,
//     then the end of the stream.
//
// ping and unknown events produce nothing; an error event, or more than
// maxBlocks content blocks, fails the stream. Every chunk has the same id
// (from the message id) and created.
type stream struct {
	a    *Adapter
	c    *call
	body io.ReadCloser
	r    *sse.Reader
	rid  string
	ttft time.Duration

	includeUsage bool
	created      int64

	started  bool // message_start was read
	id       string
	model    string
	block    *streamBlock // the current content block, or nil
	blocks   int          // content blocks started so far
	tools    int          // tool calls started so far
	finished bool
	usage    anthropicUsage
	norm     *schema.Usage

	pending []*schema.ChatChunk // chunks translated but not yet returned
	done    bool                // message_stop was read
	err     error               // sticky failure
	closed  bool
}

var _ providers.Stream = (*stream)(nil)

func newStream(a *Adapter, c *call, body io.ReadCloser, rid string, includeUsage bool) *stream {
	return &stream{
		a: a, c: c, body: body, r: sse.NewReader(body, sse.ReaderOptions{}), rid: rid,
		includeUsage: includeUsage, created: a.now().Unix(),
	}
}

// Next implements providers.Stream.
func (s *stream) Next() (*schema.ChatChunk, error) {
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

// Wire types of the stream events: only what the adapter reads.
type (
	streamEvent struct {
		Type         string            `json:"type"`
		Message      *anthropicMessage `json:"message"`
		Index        int               `json:"index"`
		ContentBlock *responseBlock    `json:"content_block"`
		Delta        *eventDelta       `json:"delta"`
		Usage        *anthropicUsage   `json:"usage"`
	}
	eventDelta struct {
		Type        string  `json:"type"`
		Text        string  `json:"text"`
		PartialJSON string  `json:"partial_json"`
		StopReason  *string `json:"stop_reason"`
	}
)

// step reads one event and queues the chunks it translates to. It returns
// an error when the stream fails.
func (s *stream) step() error {
	ev, err := s.r.Next()
	if err != nil {
		return s.readError(err)
	}
	var e streamEvent
	if jerr := json.Unmarshal(ev.Data, &e); jerr != nil {
		return s.serverError("upstream sent an event that is not JSON")
	}
	typ := ev.Type
	if typ == "" || typ == "message" {
		typ = e.Type
	}
	if typ == "ping" {
		s.c.keepAlive()
		return nil
	}
	s.c.eventArrived()
	switch typ {
	case "error":
		// The data is {"type":"error","error":{"type":…,"message":…}}.
		kind := "unknown"
		if env := s.a.parseErrorEnvelope(ev.Data); env != nil {
			kind = env.Error.Type
		}
		return s.serverError(fmt.Sprintf("upstream sent an error event (%s)", kind))
	case "message_start":
		if s.started || e.Message == nil {
			return s.serverError("upstream sent an unexpected message_start")
		}
		s.started = true
		s.id, s.model = completionIDPrefix+e.Message.ID, e.Message.Model
		s.usage.merge(e.Message.Usage)
		s.norm = s.usage.normalize()
		empty := ""
		s.emit(chunkDelta{Role: "assistant", Content: &empty}, nil)
		return nil
	case "message_stop":
		if !s.started {
			return s.serverError("upstream sent message_stop before message_start")
		}
		s.done = true
		if s.includeUsage {
			s.emitUsage()
		}
		return nil
	case "content_block_start", "content_block_delta", "content_block_stop", "message_delta":
		if !s.started {
			return s.serverError(fmt.Sprintf("upstream sent %s before message_start", typ))
		}
	default:
		return nil // new event types may be added; they carry nothing we map
	}

	switch typ {
	case "content_block_start":
		if s.blocks++; s.blocks > maxBlocks {
			return s.serverError(fmt.Sprintf("upstream sent more than %d content blocks", maxBlocks))
		}
		s.blockStart(e)
	case "content_block_delta":
		s.blockDelta(e)
	case "content_block_stop":
		s.blockStop(e)
	case "message_delta":
		if e.Usage != nil {
			s.usage.merge(*e.Usage)
			s.norm = s.usage.normalize()
		}
		if e.Delta != nil && e.Delta.StopReason != nil && !s.finished {
			s.finished = true
			reason := finishReason(*e.Delta.StopReason)
			s.emit(chunkDelta{}, &reason)
		}
	}
	return nil
}

// blockStart records a new content block, first ending the current one if
// its content_block_stop never came. A tool_use block emits the tool
// call's header chunk; a text block with initial text emits it.
func (s *stream) blockStart(e streamEvent) {
	if s.block != nil {
		s.blockStop(streamEvent{Index: s.block.index})
	}
	b := e.ContentBlock
	sb := &streamBlock{index: e.Index, kind: blockIgnored}
	s.block = sb
	if b == nil {
		return
	}
	switch b.Type {
	case "text":
		sb.kind = blockText
		if b.Text != "" {
			text := b.Text
			s.emit(chunkDelta{Content: &text}, nil)
		}
	case "tool_use":
		sb.kind, sb.tool = blockToolCall, s.tools
		s.tools++
		s.emit(chunkDelta{ToolCalls: []toolCallDelta{{
			Index: sb.tool, ID: b.ID, Type: "function", Function: &functionCall{Name: b.Name, Arguments: ""},
		}}}, nil)
	}
}

// blockDelta translates a text_delta or input_json_delta of the current
// block. Other delta types (thinking, signatures, citations) are dropped,
// and so are deltas of any block but the current one.
func (s *stream) blockDelta(e streamEvent) {
	sb, d := s.block, e.Delta
	if sb == nil || sb.index != e.Index || d == nil {
		return
	}
	switch {
	case d.Type == "text_delta" && sb.kind == blockText && d.Text != "":
		text := d.Text
		s.emit(chunkDelta{Content: &text}, nil)
	case d.Type == "input_json_delta" && sb.kind == blockToolCall && d.PartialJSON != "":
		sb.args = true
		s.emitArguments(sb.tool, d.PartialJSON)
	}
}

// blockStop ends the current block. A tool call that got no arguments
// delta gets "{}", the arguments a non-streamed response gives a tool call
// with empty input.
func (s *stream) blockStop(e streamEvent) {
	sb := s.block
	if sb == nil || sb.index != e.Index {
		return
	}
	s.block = nil
	if sb.kind == blockToolCall && !sb.args {
		s.emitArguments(sb.tool, "{}")
	}
}

// emitArguments queues an arguments delta for tool call index.
func (s *stream) emitArguments(index int, args string) {
	s.emit(chunkDelta{ToolCalls: []toolCallDelta{{
		Index: index, Function: &functionCall{Arguments: args},
	}}}, nil)
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
		Function *functionCall `json:"function,omitempty"`
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

// emitUsage queues the usage-only chunk that ends the stream.
func (s *stream) emitUsage() {
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

func (s *stream) readError(err error) error {
	switch {
	case s.c.ctx.Err() != nil:
		return s.c.classify(err, s.rid)
	case errors.Is(err, io.EOF):
		return s.serverError("upstream ended the stream without message_stop")
	case errors.Is(err, sse.ErrEventTooLarge):
		return s.serverError(fmt.Sprintf("upstream sent an event larger than %d bytes", sse.DefaultMaxEventSize))
	default:
		return s.c.classify(err, s.rid)
	}
}

func (s *stream) serverError(msg string) *providers.Error {
	return &providers.Error{Kind: providers.KindServerError, Provider: s.a.id, UpstreamRequestID: s.rid, Message: msg}
}

// Usage implements providers.Stream. It is the normalized usage reported
// so far: message_start's, updated by every message_delta.
func (s *stream) Usage() *schema.Usage { return s.norm }

// UpstreamRequestID implements providers.Stream.
func (s *stream) UpstreamRequestID() string { return s.rid }

// TTFT implements providers.Stream.
func (s *stream) TTFT() time.Duration { return s.ttft }

// Close implements providers.Stream.
//
// After message_stop only the end of the response should remain, so Close
// reads it, up to maxDrainBytes and for at most drainTimeout, to give the
// connection back to the pool. Otherwise, or if the drain does not reach
// the end in time, canceling the call's context before closing the body
// makes the transport drop the connection.
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
