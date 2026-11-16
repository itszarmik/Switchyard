package shadow

import (
	"encoding/json"
	"strings"

	"switchyard/internal/schema"
)

// DefaultMaxAnswerBytes is the longest answer a TextBuilder collects by
// default. A longer answer is not judged: a truncated answer would be
// judged unfairly.
const DefaultMaxAnswerBytes = 256 << 10

// ResponseText returns the answer text of a non-streaming chat.completion
// body, as the judge sees it: the first choice's content (a string, or the
// text of its text parts) or refusal, followed by one line per tool call,
// "[tool call] name(arguments)". It reports false when the body is not a
// chat completion with a choice.
func ResponseText(body []byte) (string, bool) {
	var r struct {
		Choices []struct {
			Index   int         `json:"index"`
			Message messageText `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &r) != nil || len(r.Choices) == 0 {
		return "", false
	}
	c := r.Choices[0]
	for _, ch := range r.Choices {
		if ch.Index == 0 {
			c = ch
			break
		}
	}
	m := c.Message
	var calls []toolCall
	for _, tc := range m.ToolCalls {
		calls = append(calls, toolCall{name: tc.Function.Name, args: tc.Function.Arguments})
	}
	return render(contentText(m.Content)+m.Refusal, calls), true
}

// TextBuilder collects the answer text of a stream, from its chunks, as
// ResponseText renders a non-streaming answer: choice 0's content and
// refusal deltas, and its tool calls assembled by index. The gateway feeds
// it the chunks it relays for a sampled request, then offers String as the
// served answer. The zero value collects up to DefaultMaxAnswerBytes. It is
// not safe for concurrent use.
type TextBuilder struct {
	// MaxBytes caps the collected text; 0 means DefaultMaxAnswerBytes.
	MaxBytes int

	text      strings.Builder
	calls     []toolCall
	size      int
	truncated bool
}

// Add collects the answer text of one stream chunk. Chunks of other
// choices, usage-only chunks and error events add nothing.
func (b *TextBuilder) Add(c *schema.ChatChunk) {
	if c == nil || c.IsError() || b.truncated {
		return
	}
	for _, raw := range c.Choices {
		var ch struct {
			Index int `json:"index"`
			Delta struct {
				Content   json.RawMessage `json:"content"`
				Refusal   string          `json:"refusal"`
				ToolCalls []struct {
					Index    int `json:"index"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		}
		if json.Unmarshal(raw, &ch) != nil || ch.Index != 0 {
			continue
		}
		d := ch.Delta
		b.write(contentText(d.Content) + d.Refusal)
		for _, tc := range d.ToolCalls {
			if tc.Index < 0 || tc.Index > maxToolCalls {
				continue
			}
			for len(b.calls) <= tc.Index {
				b.calls = append(b.calls, toolCall{})
			}
			if !b.grow(len(tc.Function.Name) + len(tc.Function.Arguments)) {
				return
			}
			b.calls[tc.Index].name += tc.Function.Name
			b.calls[tc.Index].args += tc.Function.Arguments
		}
	}
}

// maxToolCalls bounds the tool-call index a TextBuilder accepts.
const maxToolCalls = 128

// write appends s to the content, unless that exceeds the cap.
func (b *TextBuilder) write(s string) {
	if s != "" && b.grow(len(s)) {
		b.text.WriteString(s)
	}
}

// grow counts n more bytes and reports whether they fit the cap; once they
// do not, the builder is truncated and collects nothing more.
func (b *TextBuilder) grow(n int) bool {
	limit := b.MaxBytes
	if limit <= 0 {
		limit = DefaultMaxAnswerBytes
	}
	if b.size+n > limit {
		b.truncated = true
		return false
	}
	b.size += n
	return true
}

// Truncated reports whether the answer exceeded MaxBytes. A truncated
// answer must not be offered for judging.
func (b *TextBuilder) Truncated() bool { return b.truncated }

// String returns the collected answer text.
func (b *TextBuilder) String() string { return render(b.text.String(), b.calls) }

// toolCall is a tool call as the judge sees it.
type toolCall struct{ name, args string }

// render joins an answer's text and tool calls.
func render(text string, calls []toolCall) string {
	if len(calls) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(text)
	for _, c := range calls {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("[tool call] ")
		b.WriteString(c.name)
		b.WriteByte('(')
		b.WriteString(c.args)
		b.WriteByte(')')
	}
	return b.String()
}

// messageText is the part of a response message the judge sees.
type messageText struct {
	Content   json.RawMessage `json:"content"`
	Refusal   string          `json:"refusal"`
	ToolCalls []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// contentText returns the text of a content value: a string, or the
// concatenated text of an array's text parts. Anything else is "".
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" || p.Type == "output_text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
