package schema

import (
	"encoding/json"
	"fmt"
)

// Usage is the token accounting of a completion.
type Usage struct {
	PromptTokens            int64                    `json:"prompt_tokens"`
	CompletionTokens        int64                    `json:"completion_tokens"`
	TotalTokens             int64                    `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// PromptTokensDetails breaks down the prompt tokens.
type PromptTokensDetails struct {
	// CachedTokens were served from the provider's prompt cache.
	CachedTokens int64 `json:"cached_tokens"`
}

// CompletionTokensDetails breaks down the completion tokens.
type CompletionTokensDetails struct {
	// ReasoningTokens were spent on hidden reasoning.
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// ChatResponse is a non-streamed chat.completion object. Only the fields
// the gateway reads are decoded; Raw holds the original bytes, which are
// what the client receives.
type ChatResponse struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []json.RawMessage `json:"choices"`
	Usage   *Usage            `json:"usage"`
	// Raw is the body exactly as the upstream sent it.
	Raw []byte `json:"-"`
}

// ParseChatResponse decodes a chat.completion body and keeps data as Raw.
func ParseChatResponse(data []byte) (*ChatResponse, error) {
	var r ChatResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("schema: decode chat completion: %w", err)
	}
	r.Raw = data
	return &r, nil
}

// ChatChunk is one chat.completion.chunk event of a streamed completion.
// Only the fields the gateway reads are decoded; Raw holds the event's data
// payload exactly as the upstream sent it.
type ChatChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []json.RawMessage `json:"choices"`
	// Usage is set on the final usage chunk, and null on the others.
	Usage *Usage `json:"usage"`
	// Error is set when the upstream reports a failure mid-stream, as
	// {"error":{…}} in place of a chunk.
	Error json.RawMessage `json:"error"`
	// Raw is the data payload exactly as the upstream sent it.
	Raw []byte `json:"-"`
}

// ParseChatChunk decodes a chat.completion.chunk payload and keeps data as
// Raw.
func ParseChatChunk(data []byte) (*ChatChunk, error) {
	var c ChatChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("schema: decode chat completion chunk: %w", err)
	}
	c.Raw = data
	return &c, nil
}

// IsError reports whether c is an error event rather than a chunk.
func (c *ChatChunk) IsError() bool {
	return len(c.Error) > 0 && !isNull(c.Error)
}

// IsUsageOnly reports whether c is the usage chunk that ends a stream when
// stream_options.include_usage is set: no choices, and a non-null usage.
func (c *ChatChunk) IsUsageOnly() bool {
	return len(c.Choices) == 0 && c.Usage != nil
}

// RawWithoutUsage returns Raw with the usage member set to null, and every
// other member kept byte for byte and in order. It is for forwarding a
// chunk that carries usage to a client that did not ask for it. If Raw is
// not a JSON object, it is returned as is.
func (c *ChatChunk) RawWithoutUsage() []byte {
	obj, err := parseObject(c.Raw)
	if err != nil {
		return c.Raw
	}
	obj.set("usage", json.RawMessage("null"))
	return obj.encode()
}
