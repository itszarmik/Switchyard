package schema

import (
	"testing"
)

func TestParseChatResponse(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","created":1,"model":"up","choices":[{"index":0}],
		"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8,"prompt_tokens_details":{"cached_tokens":2},
		"completion_tokens_details":{"reasoning_tokens":1}},"x_extra":true}`)
	r, err := ParseChatResponse(body)
	if err != nil {
		t.Fatalf("ParseChatResponse: %v", err)
	}
	if r.ID != "c1" || r.Model != "up" || len(r.Choices) != 1 || string(r.Raw) != string(body) {
		t.Errorf("parsed %+v", r)
	}
	u := r.Usage
	if u == nil || u.PromptTokens != 3 || u.CompletionTokens != 5 || u.TotalTokens != 8 ||
		u.PromptTokensDetails.CachedTokens != 2 || u.CompletionTokensDetails.ReasoningTokens != 1 {
		t.Errorf("usage = %+v", u)
	}
	if _, err := ParseChatResponse([]byte(`{"usage":"x"}`)); err == nil {
		t.Error("ParseChatResponse accepted a bad usage")
	}
}

func TestParseChatChunk(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		usageOnly bool
		wantUsage bool
		isError   bool
	}{
		{name: "content chunk", data: `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}],"usage":null}`},
		{name: "usage chunk", data: `{"id":"c","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, usageOnly: true, wantUsage: true},
		{name: "usage on a content chunk", data: `{"choices":[{"index":0}],"usage":{"prompt_tokens":1}}`, wantUsage: true},
		{name: "no choices and no usage", data: `{"choices":[]}`},
		{name: "error event", data: `{"error":{"message":"boom","type":"server_error"}}`, isError: true},
		{name: "null error", data: `{"choices":[],"error":null}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseChatChunk([]byte(tt.data))
			if err != nil {
				t.Fatalf("ParseChatChunk: %v", err)
			}
			if c.IsUsageOnly() != tt.usageOnly {
				t.Errorf("IsUsageOnly = %v, want %v", c.IsUsageOnly(), tt.usageOnly)
			}
			if c.IsError() != tt.isError {
				t.Errorf("IsError = %v, want %v", c.IsError(), tt.isError)
			}
			if (c.Usage != nil) != tt.wantUsage {
				t.Errorf("usage = %+v", c.Usage)
			}
			if string(c.Raw) != tt.data {
				t.Error("Raw differs from the input")
			}
		})
	}
	if _, err := ParseChatChunk([]byte(`not json`)); err == nil {
		t.Error("ParseChatChunk accepted invalid JSON")
	}
}

func TestChatChunkRawWithoutUsage(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			name: "usage on a chunk with choices",
			in:   `{"id":"c", "choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10},"x":[1, 2.50]}`,
			want: `{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null,"x":[1, 2.50]}`,
		},
		{
			name: "no usage field",
			in:   `{"id":"c","choices":[]}`,
			want: `{"id":"c","choices":[],"usage":null}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseChatChunk([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if got := string(c.RawWithoutUsage()); got != tt.want {
				t.Errorf("RawWithoutUsage = %s, want %s", got, tt.want)
			}
			if string(c.Raw) != tt.in {
				t.Errorf("Raw was modified: %s", c.Raw)
			}
		})
	}
}
