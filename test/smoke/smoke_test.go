//go:build smoke

package smoke

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"switchyard/test/conformance/harness"
)

// callTimeout bounds each live call; reasoning models can be slow.
const callTimeout = 2 * time.Minute

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// TestSmoke runs one chat call and one stream call through the gateway
// against a real provider.
func TestSmoke(t *testing.T) {
	providerKey := os.Getenv("OPENAI_API_KEY")
	if providerKey == "" {
		t.Skip("OPENAI_API_KEY is not set; the live smoke test is opt-in")
	}
	model := envOr("SMOKE_MODEL", "gpt-5-mini")
	baseURL := envOr("SMOKE_BASE_URL", "https://api.openai.com/v1")

	key, hash, err := harness.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	// The provider key reaches the gateway through its environment, never
	// through the config file on disk.
	env := []string{"OPENAI_API_KEY=" + providerKey}
	for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	g := harness.Run(t, harness.Config{
		ProviderBaseURL: baseURL,
		ProviderAPIKey:  "${OPENAI_API_KEY}",
		Models:          []harness.Model{{ID: model, UpstreamModel: model}},
		KeyHash:         hash,
		Timeouts:        harness.Timeouts{FirstToken: 90 * time.Second, Total: callTimeout},
	}, harness.RunOptions{Env: env})

	client := openai.NewClient(
		option.WithBaseURL(g.PublicURL+"/v1"),
		option.WithAPIKey(key),
		option.WithMaxRetries(0),
	)
	params := openai.ChatCompletionNewParams{
		Model:    model,
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Reply with the single word: pong")},
	}

	t.Run("chat", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		var resp *http.Response
		c, err := client.Chat.Completions.New(ctx, params, option.WithResponseInto(&resp))
		if err != nil {
			t.Fatalf("chat: %v", describe(err))
		}
		if len(c.Choices) == 0 || strings.TrimSpace(c.Choices[0].Message.Content) == "" {
			t.Fatalf("empty answer: %s", c.RawJSON())
		}
		if c.Usage.TotalTokens <= 0 {
			t.Errorf("usage %s", c.Usage.RawJSON())
		}
		if got := resp.Header.Get("X-Switchyard-Model"); got != model {
			t.Errorf("X-Switchyard-Model = %q, want %q", got, model)
		}
		if resp.Header.Get("X-Switchyard-Upstream-Request-Id") == "" {
			t.Error("no X-Switchyard-Upstream-Request-Id")
		}
		t.Logf("chat answered %q (%d tokens)", c.Choices[0].Message.Content, c.Usage.TotalTokens)
	})

	t.Run("stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		p := params
		p.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
		stream := client.Chat.Completions.NewStreaming(ctx, p)
		defer stream.Close()
		var acc openai.ChatCompletionAccumulator
		chunks := 0
		for stream.Next() {
			chunks++
			acc.AddChunk(stream.Current())
		}
		if err := stream.Err(); err != nil {
			t.Fatalf("stream: %v", describe(err))
		}
		if len(acc.Choices) == 0 || strings.TrimSpace(acc.Choices[0].Message.Content) == "" {
			t.Fatalf("empty answer after %d chunks", chunks)
		}
		if acc.Usage.TotalTokens <= 0 {
			t.Error("no usage, although the stream asked for it")
		}
		t.Logf("stream answered %q in %d chunks (%d tokens)", acc.Choices[0].Message.Content, chunks, acc.Usage.TotalTokens)
	})

	for i, line := range g.Logs() {
		if strings.Contains(line, providerKey) || strings.Contains(line, key) {
			t.Errorf("log line %d contains an API key", i)
		}
	}
}

// describe adds an API error's details, which its Error method omits.
func describe(err error) string {
	var aerr *openai.Error
	if errors.As(err, &aerr) {
		return aerr.Error() + ": " + aerr.RawJSON()
	}
	return err.Error()
}
