//go:build record

package fakeanthropic

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRecordFixtures records real Messages API responses into
// test/fixtures/anthropic/recorded, for comparison with the handcrafted
// fixtures. It calls the live API, which costs money, so it is behind the
// record build tag and needs ANTHROPIC_API_KEY; without the key it skips.
// Run it with `make record-fixtures`.
//
//	ANTHROPIC_API_KEY   required
//	ANTHROPIC_BASE_URL  default https://api.anthropic.com
//	RECORD_MODEL        default claude-opus-5-5
//
// Only response bodies are written, never request headers, and a body that
// contains the key is not written at all.
func TestRecordFixtures(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY is not set; not recording fixtures")
	}
	base := strings.TrimSuffix(envOr("ANTHROPIC_BASE_URL", "https://api.anthropic.com"), "/")
	model := envOr("RECORD_MODEL", "claude-opus-5-5")
	dir := filepath.Join("..", "fixtures", "anthropic", "recorded")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	const weatherTool = `{"name":"get_weather","description":"Gets the current weather. Call it once per city the user asks about.",
		"input_schema":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}}`
	const timeTool = `{"name":"get_time","description":"Gets the local time. Call it once per city the user asks about.",
		"input_schema":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}}`
	cases := []struct {
		name, body string
	}{
		{"text", `{"model":"` + model + `","max_tokens":2048,"messages":[{"role":"user","content":"Say hello in five words."}]}`},
		{"tool_use", `{"model":"` + model + `","max_tokens":2048,"tools":[` + weatherTool + `],
			"messages":[{"role":"user","content":"What is the weather in Paris? Use the tool."}]}`},
		{"parallel_tool_use", `{"model":"` + model + `","max_tokens":2048,"tools":[` + weatherTool + `,` + timeTool + `],
			"messages":[{"role":"user","content":"What are the weather and the local time in Paris? Call both tools at once."}]}`},
		{"structured_output", `{"model":"` + model + `","max_tokens":2048,
			"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}}},
			"messages":[{"role":"user","content":"What is the capital of France?"}]}`},
		{"error_not_found", `{"model":"claude-nonexistent-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	for _, c := range cases {
		for _, stream := range []bool{false, true} {
			name, ext, body := c.name, ".json", c.body
			if stream {
				ext = ".sse"
				body = strings.TrimSuffix(strings.TrimSpace(body), "}") + `,"stream":true}`
			}
			t.Run(name+ext, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("X-Api-Key", key)
				req.Header.Set("Anthropic-Version", "2023-06-01")
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("request failed: %v", err)
				}
				defer resp.Body.Close()
				out, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(out, []byte(key)) {
					t.Fatal("the response contains the API key; not writing it")
				}
				path := filepath.Join(dir, name+ext)
				if err := os.WriteFile(path, out, 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("HTTP %d, request-id %s: wrote %s", resp.StatusCode, resp.Header.Get("Request-Id"), path)
			})
		}
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
