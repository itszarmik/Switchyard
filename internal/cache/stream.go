package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"switchyard/internal/schema"
)

// Object types of a chat completion and of its stream chunks.
const (
	objectCompletion = "chat.completion"
	objectChunk      = "chat.completion.chunk"
)

// completionIn is the part of a chat.completion body that Replay reads.
// Top-level members are kept raw so chunks carry them verbatim.
type completionIn struct {
	ID                json.RawMessage `json:"id"`
	Created           json.RawMessage `json:"created"`
	Model             json.RawMessage `json:"model"`
	ServiceTier       json.RawMessage `json:"service_tier"`
	SystemFingerprint json.RawMessage `json:"system_fingerprint"`
	Choices           []struct {
		Index   *int `json:"index"`
		Message struct {
			Role         string          `json:"role"`
			Content      json.RawMessage `json:"content"`
			Refusal      json.RawMessage `json:"refusal"`
			FunctionCall json.RawMessage `json:"function_call"`
			ToolCalls    []struct {
				ID       json.RawMessage `json:"id"`
				Type     json.RawMessage `json:"type"`
				Function json.RawMessage `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		Logprobs     json.RawMessage `json:"logprobs"`
		FinishReason json.RawMessage `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

// chunkOut is one replayed chat.completion.chunk. Raw members left empty
// are omitted; Logprobs and FinishReason are null when empty.
type chunkOut struct {
	ID                json.RawMessage `json:"id"`
	Object            string          `json:"object"`
	Created           json.RawMessage `json:"created"`
	Model             json.RawMessage `json:"model"`
	ServiceTier       json.RawMessage `json:"service_tier,omitempty"`
	SystemFingerprint json.RawMessage `json:"system_fingerprint,omitempty"`
	Choices           []choiceOut     `json:"choices"`
	Usage             json.RawMessage `json:"usage,omitempty"`
}

type choiceOut struct {
	Index        int             `json:"index"`
	Delta        deltaOut        `json:"delta"`
	Logprobs     json.RawMessage `json:"logprobs"`
	FinishReason json.RawMessage `json:"finish_reason"`
}

type deltaOut struct {
	Role         string          `json:"role,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"`
	Refusal      json.RawMessage `json:"refusal,omitempty"`
	FunctionCall json.RawMessage `json:"function_call,omitempty"`
	ToolCalls    []toolCallOut   `json:"tool_calls,omitempty"`
}

type toolCallOut struct {
	Index    int             `json:"index"`
	ID       json.RawMessage `json:"id,omitempty"`
	Type     json.RawMessage `json:"type,omitempty"`
	Function json.RawMessage `json:"function,omitempty"`
}

// present reports whether raw is a value other than null.
func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// Replay turns a cached chat.completion body into the data payloads of a
// minimal valid stream (DESIGN §7), the bytes after "data: ". For each
// choice in turn it yields a role chunk; a chunk with the content, refusal,
// tool calls (each with its index, id, type, function name and arguments),
// legacy function_call and logprobs that are present, if any is; and a
// finish chunk. When includeUsage is set and the body has usage, a usage
// chunk with no choices follows. The caller writes the final [DONE].
//
// Every chunk keeps the body's id, created and model, and its
// service_tier and system_fingerprint when present.
func Replay(body []byte, includeUsage bool) ([][]byte, error) {
	var c completionIn
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, fmt.Errorf("cache: replay: decode completion: %w", err)
	}
	if len(c.Choices) == 0 {
		return nil, errors.New("cache: replay: the completion has no choices")
	}
	base := chunkOut{
		ID: c.ID, Object: objectChunk, Created: c.Created, Model: c.Model,
		ServiceTier: c.ServiceTier, SystemFingerprint: c.SystemFingerprint,
	}
	if !present(base.ServiceTier) {
		base.ServiceTier = nil
	}
	if !present(base.SystemFingerprint) {
		base.SystemFingerprint = nil
	}
	var out [][]byte
	emit := func(choices []choiceOut, usage json.RawMessage) error {
		ch := base
		ch.Choices, ch.Usage = choices, usage
		b, err := json.Marshal(ch)
		if err != nil {
			return fmt.Errorf("cache: replay: encode chunk: %w", err)
		}
		out = append(out, b)
		return nil
	}
	for i, choice := range c.Choices {
		idx := i
		if choice.Index != nil {
			idx = *choice.Index
		}
		msg := &choice.Message
		role := msg.Role
		if role == "" {
			role = "assistant"
		}
		if err := emit([]choiceOut{{Index: idx, Delta: deltaOut{Role: role}}}, nil); err != nil {
			return nil, err
		}

		d := deltaOut{}
		if present(msg.Content) {
			d.Content = msg.Content
		}
		if present(msg.Refusal) {
			d.Refusal = msg.Refusal
		}
		if present(msg.FunctionCall) {
			d.FunctionCall = msg.FunctionCall
		}
		for j, tc := range msg.ToolCalls {
			d.ToolCalls = append(d.ToolCalls, toolCallOut{Index: j, ID: tc.ID, Type: tc.Type, Function: tc.Function})
		}
		var logprobs json.RawMessage
		if present(choice.Logprobs) {
			logprobs = choice.Logprobs
		}
		if d.Content != nil || d.Refusal != nil || d.FunctionCall != nil || d.ToolCalls != nil || logprobs != nil {
			if err := emit([]choiceOut{{Index: idx, Delta: d, Logprobs: logprobs}}, nil); err != nil {
				return nil, err
			}
		}

		if err := emit([]choiceOut{{Index: idx, FinishReason: choice.FinishReason}}, nil); err != nil {
			return nil, err
		}
	}
	if includeUsage && present(c.Usage) {
		if err := emit([]choiceOut{}, c.Usage); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// chunkIn is the part of a chat.completion.chunk that the Aggregator reads.
type chunkIn struct {
	ID                string          `json:"id"`
	Created           int64           `json:"created"`
	Model             string          `json:"model"`
	ServiceTier       json.RawMessage `json:"service_tier"`
	SystemFingerprint json.RawMessage `json:"system_fingerprint"`
	Choices           []struct {
		Index int `json:"index"`
		Delta struct {
			Role         string  `json:"role"`
			Content      *string `json:"content"`
			Refusal      *string `json:"refusal"`
			FunctionCall *struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function_call"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Logprobs *struct {
			Content []json.RawMessage `json:"content"`
			Refusal []json.RawMessage `json:"refusal"`
		} `json:"logprobs"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

// Aggregator folds the chunks of a streamed chat completion back into a
// chat.completion body, for caching a streamed miss. The zero value is not
// usable; create one with NewAggregator. It is not safe for concurrent
// use.
type Aggregator struct {
	id, model         string
	created           int64
	serviceTier       json.RawMessage
	systemFingerprint json.RawMessage
	choices           map[int]*aggChoice
	usage             json.RawMessage
}

// aggChoice is the state of one choice.
type aggChoice struct {
	role                string
	content, refusal    strings.Builder
	hasContent, hasRef  bool
	functionCall        *aggFunction
	toolCalls           map[int]*aggToolCall
	hasLogprobs         bool
	lpContent, lpRefuse []json.RawMessage
	finish              string
}

// aggFunction is a function call being assembled from fragments.
type aggFunction struct {
	name, arguments strings.Builder
}

// aggToolCall is a tool call being assembled from fragments.
type aggToolCall struct {
	id, typ string
	fn      aggFunction
}

// NewAggregator returns an empty Aggregator.
func NewAggregator() *Aggregator {
	return &Aggregator{choices: make(map[int]*aggChoice)}
}

// Add folds one chunk in: content and refusal text is concatenated per
// choice index, tool-call deltas are merged by index (the id and type from
// the first fragment that has them, name and argument fragments
// concatenated, as the openai-go accumulator does), and the role, the
// finish_reason and the last non-null usage are kept. The id, created,
// model, service_tier and system_fingerprint come from the first chunk
// that has them. An error event, or a payload that is not a chunk, is an
// error: such a stream must not be cached.
func (a *Aggregator) Add(chunk *schema.ChatChunk) error {
	if chunk.IsError() {
		return errors.New("cache: aggregate: the stream reported an error")
	}
	var c chunkIn
	if err := json.Unmarshal(chunk.Raw, &c); err != nil {
		return fmt.Errorf("cache: aggregate: decode chunk: %w", err)
	}
	if a.id == "" {
		a.id = c.ID
	}
	if a.model == "" {
		a.model = c.Model
	}
	if a.created == 0 {
		a.created = c.Created
	}
	if a.serviceTier == nil && present(c.ServiceTier) {
		a.serviceTier = c.ServiceTier
	}
	if a.systemFingerprint == nil && present(c.SystemFingerprint) {
		a.systemFingerprint = c.SystemFingerprint
	}
	if present(c.Usage) {
		a.usage = c.Usage
	}
	for i := range c.Choices {
		in := &c.Choices[i]
		if in.Index < 0 {
			return fmt.Errorf("cache: aggregate: negative choice index %d", in.Index)
		}
		ch := a.choices[in.Index]
		if ch == nil {
			ch = &aggChoice{toolCalls: make(map[int]*aggToolCall)}
			a.choices[in.Index] = ch
		}
		d := &in.Delta
		if d.Role != "" {
			ch.role = d.Role
		}
		if d.Content != nil {
			ch.content.WriteString(*d.Content)
			ch.hasContent = true
		}
		if d.Refusal != nil {
			ch.refusal.WriteString(*d.Refusal)
			ch.hasRef = true
		}
		if f := d.FunctionCall; f != nil {
			if ch.functionCall == nil {
				ch.functionCall = &aggFunction{}
			}
			ch.functionCall.name.WriteString(f.Name)
			ch.functionCall.arguments.WriteString(f.Arguments)
		}
		for j := range d.ToolCalls {
			tc := &d.ToolCalls[j]
			if tc.Index < 0 {
				return fmt.Errorf("cache: aggregate: negative tool call index %d", tc.Index)
			}
			t := ch.toolCalls[tc.Index]
			if t == nil {
				t = &aggToolCall{}
				ch.toolCalls[tc.Index] = t
			}
			if t.id == "" {
				t.id = tc.ID
			}
			if t.typ == "" {
				t.typ = tc.Type
			}
			t.fn.name.WriteString(tc.Function.Name)
			t.fn.arguments.WriteString(tc.Function.Arguments)
		}
		if lp := in.Logprobs; lp != nil {
			ch.hasLogprobs = true
			ch.lpContent = append(ch.lpContent, lp.Content...)
			ch.lpRefuse = append(ch.lpRefuse, lp.Refusal...)
		}
		if in.FinishReason != nil && *in.FinishReason != "" {
			ch.finish = *in.FinishReason
		}
	}
	return nil
}

// completionOut is the chat.completion body the Aggregator builds.
type completionOut struct {
	ID                string          `json:"id"`
	Object            string          `json:"object"`
	Created           int64           `json:"created"`
	Model             string          `json:"model"`
	ServiceTier       json.RawMessage `json:"service_tier,omitempty"`
	SystemFingerprint json.RawMessage `json:"system_fingerprint,omitempty"`
	Choices           []choiceBody    `json:"choices"`
	Usage             json.RawMessage `json:"usage,omitempty"`
}

type choiceBody struct {
	Index        int          `json:"index"`
	Message      messageBody  `json:"message"`
	Logprobs     *logprobsOut `json:"logprobs"`
	FinishReason string       `json:"finish_reason"`
}

type messageBody struct {
	Role         string         `json:"role"`
	Content      *string        `json:"content"`
	Refusal      *string        `json:"refusal"`
	FunctionCall *functionBody  `json:"function_call,omitempty"`
	ToolCalls    []toolCallBody `json:"tool_calls,omitempty"`
}

type functionBody struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolCallBody struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionBody `json:"function"`
}

type logprobsOut struct {
	Content []json.RawMessage `json:"content"`
	Refusal []json.RawMessage `json:"refusal"`
}

// Body returns the aggregated chat.completion, its choices in index order
// and each choice's tool calls in index order. It reports false when the
// stream is not complete: it had no choice, or a choice has no
// finish_reason.
func (a *Aggregator) Body() ([]byte, bool) {
	if len(a.choices) == 0 {
		return nil, false
	}
	out := completionOut{
		ID: a.id, Object: objectCompletion, Created: a.created, Model: a.model,
		ServiceTier: a.serviceTier, SystemFingerprint: a.systemFingerprint, Usage: a.usage,
	}
	for _, idx := range slices.Sorted(maps.Keys(a.choices)) {
		ch := a.choices[idx]
		if ch.finish == "" {
			return nil, false
		}
		msg := messageBody{Role: ch.role}
		if msg.Role == "" {
			msg.Role = "assistant"
		}
		// An answer that only calls tools has null content, as the
		// non-streaming form has, though its stream (Anthropic's, say)
		// may have sent an empty content delta.
		calls := ch.functionCall != nil || len(ch.toolCalls) > 0
		if ch.hasContent && (ch.content.Len() > 0 || !calls) {
			s := ch.content.String()
			msg.Content = &s
		}
		if ch.hasRef {
			s := ch.refusal.String()
			msg.Refusal = &s
		}
		if f := ch.functionCall; f != nil {
			msg.FunctionCall = &functionBody{Name: f.name.String(), Arguments: f.arguments.String()}
		}
		for _, ti := range slices.Sorted(maps.Keys(ch.toolCalls)) {
			t := ch.toolCalls[ti]
			typ := t.typ
			if typ == "" {
				typ = "function"
			}
			msg.ToolCalls = append(msg.ToolCalls, toolCallBody{
				ID: t.id, Type: typ,
				Function: functionBody{Name: t.fn.name.String(), Arguments: t.fn.arguments.String()},
			})
		}
		cb := choiceBody{Index: idx, Message: msg, FinishReason: ch.finish}
		if ch.hasLogprobs {
			cb.Logprobs = &logprobsOut{Content: ch.lpContent, Refusal: ch.lpRefuse}
		}
		out.Choices = append(out.Choices, cb)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return b, true
}
