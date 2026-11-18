package cache

import (
	"encoding/json"
)

// cacheableFinish are the finish reasons of a complete answer. content_filter
// and anything else mean the answer is cut or withheld, so it is never
// cached.
var cacheableFinish = map[string]bool{"stop": true, "tool_calls": true, "length": true}

// Cacheable reports whether body, a 200 response to a chat request, may be
// cached: it is at most maxBytes long, it is a chat.completion object with
// at least one choice, and every choice has a message and a finish_reason of
// stop, tool_calls or length. The caller checks the status, and for a
// stream that it ended cleanly with [DONE] (see Aggregator).
func Cacheable(body []byte, maxBytes int) bool {
	if len(body) == 0 || len(body) > maxBytes {
		return false
	}
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message      json.RawMessage `json:"message"`
			FinishReason *string         `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Object != "chat.completion" || len(resp.Choices) == 0 {
		return false
	}
	for _, c := range resp.Choices {
		var msg map[string]json.RawMessage
		if c.FinishReason == nil || !cacheableFinish[*c.FinishReason] || json.Unmarshal(c.Message, &msg) != nil || msg == nil {
			return false
		}
	}
	return true
}
