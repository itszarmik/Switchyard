//go:build smoke

package smoke

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"switchyard/test/conformance/harness"
)

// TestSmokeGemini runs the gateway against Google's native Gemini API
// (provider type gemini): one chat call, one stream with usage, and a
// two-turn tool call whose second turn sends the first turn's tool-call
// ids back. Gemini 3 and 4 reject that second turn with a 400 unless the
// thought signatures carried in those ids come back on the function
// calls, so it passing proves the round trip.
//
//	GEMINI_API_KEY          required; the test skips without it
//	SMOKE_GEMINI_MODEL      default gemini-3.8-flash
//	SMOKE_GEMINI_BASE_URL   default https://generativelanguage.googleapis.com/v1beta
func TestSmokeGemini(t *testing.T) {
	providerKey := os.Getenv("GEMINI_API_KEY")
	if providerKey == "" {
		t.Skip("GEMINI_API_KEY is not set; the live smoke test is opt-in")
	}
	model := envOr("SMOKE_GEMINI_MODEL", "gemini-3.8-flash")
	baseURL := envOr("SMOKE_GEMINI_BASE_URL", "https://generativelanguage.googleapis.com/v1beta")

	key, hash, err := harness.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	// The provider key reaches the gateway through its environment, never
	// through the config file on disk.
	env := []string{"GEMINI_API_KEY=" + providerKey}
	for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	g := harness.Run(t, harness.Config{
		ProviderBaseURL: baseURL,
		ProviderAPIKey:  "${GEMINI_API_KEY}",
		ProviderType:    "gemini",
		Models: []harness.Model{{ID: model, UpstreamModel: model, Capabilities: []string{
			"streaming", "stream_usage", "tools", "parallel_tools", "forced_tools", "vision", "json_mode", "json_schema", "prompt_caching",
		}}},
		KeyHash:  hash,
		Timeouts: harness.Timeouts{FirstToken: 2 * time.Minute, Total: callTimeout},
	}, harness.RunOptions{Env: env})

	client := openai.NewClient(
		option.WithBaseURL(g.PublicURL+"/v1"),
		option.WithAPIKey(key),
		option.WithMaxRetries(0),
	)
	ask := openai.UserMessage("Reply with the single word: pong")

	t.Run("chat", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		c, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model: model, Messages: []openai.ChatCompletionMessageParamUnion{ask},
		})
		if err != nil {
			t.Fatalf("chat: %v", describe(err))
		}
		if len(c.Choices) == 0 || strings.TrimSpace(c.Choices[0].Message.Content) == "" || c.Usage.TotalTokens <= 0 {
			t.Fatalf("answer %s", c.RawJSON())
		}
		t.Logf("chat answered %q (%d tokens, %d reasoning)", c.Choices[0].Message.Content, c.Usage.TotalTokens,
			c.Usage.CompletionTokensDetails.ReasoningTokens)
	})

	t.Run("stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		stream := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
			Model: model, Messages: []openai.ChatCompletionMessageParamUnion{ask},
			StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
		})
		defer stream.Close()
		var acc openai.ChatCompletionAccumulator
		for stream.Next() {
			acc.AddChunk(stream.Current())
		}
		if err := stream.Err(); err != nil {
			t.Fatalf("stream: %v", describe(err))
		}
		if len(acc.Choices) == 0 || strings.TrimSpace(acc.Choices[0].Message.Content) == "" || acc.Usage.TotalTokens <= 0 {
			t.Fatal("empty answer or no usage")
		}
	})

	t.Run("two-turn tool call", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*callTimeout)
		defer cancel()
		weather := openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        "get_weather",
			Description: openai.String("Gets the current weather in a city."),
			Parameters: shared.FunctionParameters{
				"type": "object", "properties": map[string]any{"location": map[string]any{"type": "string"}},
				"required": []string{"location"},
			},
		})
		p := openai.ChatCompletionNewParams{
			Model:    model,
			Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("What is the weather in Paris? Use the tool.")},
			Tools:    []openai.ChatCompletionToolUnionParam{weather},
		}
		first, err := client.Chat.Completions.New(ctx, p)
		if err != nil {
			t.Fatalf("turn 1: %v", describe(err))
		}
		msg := first.Choices[0].Message
		if len(msg.ToolCalls) == 0 {
			t.Fatalf("turn 1 made no tool call: %s", first.RawJSON())
		}
		t.Logf("turn 1: %d tool calls, first id %d bytes", len(msg.ToolCalls), len(msg.ToolCalls[0].ID))
		p.Messages = append(p.Messages, msg.ToParam())
		for _, tc := range msg.ToolCalls {
			p.Messages = append(p.Messages, openai.ToolMessage(`{"temperature_c": 18, "sky": "clear"}`, tc.ID))
		}
		second, err := client.Chat.Completions.New(ctx, p)
		if err != nil {
			t.Fatalf("turn 2 (thought signatures did not round-trip?): %v", describe(err))
		}
		if c := second.Choices[0]; strings.TrimSpace(c.Message.Content) == "" && len(c.Message.ToolCalls) == 0 {
			t.Fatalf("turn 2 answered nothing: %s", second.RawJSON())
		}
	})

	for i, line := range g.Logs() {
		if strings.Contains(line, providerKey) || strings.Contains(line, key) {
			t.Errorf("log line %d contains an API key", i)
		}
	}
}
