package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"switchyard/internal/schema"
)

// completionIDPrefix starts every chat.completion and chunk id, followed by
// the Anthropic message id. OpenAI clients expect the prefix.
const completionIDPrefix = "chatcmpl-"

// Wire types of the Messages API response: only what the adapter reads.
type (
	anthropicMessage struct {
		ID         string          `json:"id"`
		Type       string          `json:"type"`
		Model      string          `json:"model"`
		Content    []responseBlock `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      anthropicUsage  `json:"usage"`
	}
	responseBlock struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	// anthropicUsage is a Message's usage, or a message_delta's cumulative
	// usage. Every count is a pointer, because a delta may leave counts out
	// or send null for them.
	anthropicUsage struct {
		InputTokens              *int64 `json:"input_tokens"`
		OutputTokens             *int64 `json:"output_tokens"`
		CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
		OutputTokensDetails      *struct {
			ThinkingTokens *int64 `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	}
)

// OpenAI-shaped output types, in OpenAI's field order.
type (
	completion struct {
		ID      string        `json:"id"`
		Object  string        `json:"object"`
		Created int64         `json:"created"`
		Model   string        `json:"model"`
		Choices []choice      `json:"choices"`
		Usage   *schema.Usage `json:"usage"`
	}
	choice struct {
		Index        int             `json:"index"`
		Message      choiceMessage   `json:"message"`
		Logprobs     json.RawMessage `json:"logprobs"`
		FinishReason string          `json:"finish_reason"`
	}
	choiceMessage struct {
		Role      string     `json:"role"`
		Content   *string    `json:"content"`
		Refusal   *string    `json:"refusal"`
		ToolCalls []toolCall `json:"tool_calls,omitempty"`
	}
	toolCall struct {
		ID       string       `json:"id"`
		Type     string       `json:"type"`
		Function functionCall `json:"function"`
	}
	functionCall struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	}
)

// jsonNull is the JSON null literal.
var jsonNull = json.RawMessage("null")

// translateMessage turns a Messages API response body into a
// chat.completion body and its normalized usage. Text blocks are
// concatenated into the content; tool_use blocks become tool_calls, with
// the input object as the arguments string; thinking and every other block
// type are dropped.
func translateMessage(body []byte, now time.Time) ([]byte, *schema.Usage, error) {
	var m anthropicMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, fmt.Errorf("decode message: %w", err)
	}
	if m.Type != "message" {
		return nil, nil, errors.New("decode message: type is not message")
	}
	var text strings.Builder
	var calls []toolCall
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			calls = append(calls, toolCall{ID: b.ID, Type: "function", Function: functionCall{Name: b.Name, Arguments: compactObject(b.Input)}})
		}
	}

	msg := choiceMessage{Role: "assistant", ToolCalls: calls}
	if text.Len() > 0 || len(calls) == 0 {
		s := text.String()
		msg.Content = &s
	}
	usage := m.Usage.normalize()
	out := completion{
		ID: completionIDPrefix + m.ID, Object: "chat.completion", Created: now.Unix(), Model: m.Model,
		Choices: []choice{{
			Index: 0, Message: msg, Logprobs: jsonNull,
			FinishReason: finishReason(m.StopReason),
		}},
		Usage: usage,
	}
	b, err := marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return b, usage, nil
}

// finishReason maps a stop_reason onto OpenAI's finish_reason. Unknown and
// missing reasons are stop.
func finishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default: // end_turn, stop_sequence, pause_turn
		return "stop"
	}
}

// compactObject returns a tool input as a compact JSON string, or "{}" when
// it is absent or not valid JSON.
func compactObject(raw json.RawMessage) string {
	var buf bytes.Buffer
	if len(raw) == 0 || json.Compact(&buf, raw) != nil {
		return "{}"
	}
	return buf.String()
}

// merge copies the counts that u2 reports onto u. Message delta counts are
// cumulative, so a later value replaces an earlier one.
func (u *anthropicUsage) merge(u2 anthropicUsage) {
	if u2.InputTokens != nil {
		u.InputTokens = u2.InputTokens
	}
	if u2.OutputTokens != nil {
		u.OutputTokens = u2.OutputTokens
	}
	if u2.CacheCreationInputTokens != nil {
		u.CacheCreationInputTokens = u2.CacheCreationInputTokens
	}
	if u2.CacheReadInputTokens != nil {
		u.CacheReadInputTokens = u2.CacheReadInputTokens
	}
	if u2.OutputTokensDetails != nil && u2.OutputTokensDetails.ThinkingTokens != nil {
		u.OutputTokensDetails = u2.OutputTokensDetails
	}
}

// normalize maps Anthropic usage onto OpenAI's (DESIGN §6.2, §12.1):
// prompt_tokens is input + cache read + cache creation, because Anthropic's
// input_tokens excludes cached tokens; cached_tokens is the cache read and
// cache_write_tokens the cache creation, so that the uncached input is
// prompt_tokens minus both; completion_tokens is the output, and
// reasoning_tokens the thinking tokens when reported.
func (u anthropicUsage) normalize() *schema.Usage {
	val := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	read := val(u.CacheReadInputTokens)
	out := &schema.Usage{
		PromptTokens:        val(u.InputTokens) + read + val(u.CacheCreationInputTokens),
		CompletionTokens:    val(u.OutputTokens),
		PromptTokensDetails: &schema.PromptTokensDetails{CachedTokens: read, CacheWriteTokens: val(u.CacheCreationInputTokens)},
	}
	out.TotalTokens = out.PromptTokens + out.CompletionTokens
	if d := u.OutputTokensDetails; d != nil && d.ThinkingTokens != nil {
		out.CompletionTokensDetails = &schema.CompletionTokensDetails{ReasoningTokens: *d.ThinkingTokens}
	}
	return out
}
