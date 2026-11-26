//go:build record

package fakegemini

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

// TestRecordFixtures records real Gemini API responses into
// test/fixtures/gemini/recorded, for comparison with the handcrafted
// fixtures. It calls the live API, which costs money, so it is behind the
// record build tag and needs GEMINI_API_KEY; without the key it skips.
// Run it with `make record-fixtures`.
//
//	GEMINI_API_KEY   required
//	GEMINI_BASE_URL  default https://generativelanguage.googleapis.com/v1beta
//	RECORD_MODEL     default gemini-3.8-flash
//
// The key is sent only in the x-goog-api-key header, never in a URL. Only
// response bodies are written, never request headers, and a body that
// contains the key is not written at all. The live check that thought
// signatures round-trip is test/smoke's.
func TestRecordFixtures(t *testing.T) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("GEMINI_API_KEY is not set; not recording fixtures")
	}
	base := strings.TrimSuffix(envOr("GEMINI_BASE_URL", "https://generativelanguage.googleapis.com/v1beta"), "/")
	model := envOr("RECORD_MODEL", "gemini-3.8-flash")
	dir := filepath.Join("..", "fixtures", "gemini", "recorded")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	const weather = `{"name":"get_weather","description":"Gets the current weather. Call it once per city the user asks about.",
		"parametersJsonSchema":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}}`
	const clock = `{"name":"get_time","description":"Gets the local time. Call it once per city the user asks about.",
		"parametersJsonSchema":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}}`
	user := func(text string) string { return `{"role":"user","parts":[{"text":"` + text + `"}]}` }
	cases := []struct{ name, model, body string }{
		{"text", model, `{"store":false,"contents":[` + user("Say hello in five words.") + `]}`},
		{"tool_call", model, `{"store":false,"tools":[{"functionDeclarations":[` + weather + `]}],
			"contents":[` + user("What is the weather in Paris? Use the tool.") + `]}`},
		{"parallel_tool_calls", model, `{"store":false,"tools":[{"functionDeclarations":[` + weather + `,` + clock + `]}],
			"contents":[` + user("What are the weather and the local time in Paris? Call both tools at once.") + `]}`},
		{"json_schema", model, `{"store":false,"generationConfig":{"responseMimeType":"application/json",
			"responseJsonSchema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}},
			"contents":[` + user("What is the capital of France?") + `]}`},
		{"json_object", model, `{"store":false,"generationConfig":{"responseMimeType":"application/json"},
			"contents":[` + user("Give the capital of France as a JSON object.") + `]}`},
		{"thinking_low", model, `{"store":false,"generationConfig":{"thinkingConfig":{"thinkingLevel":"LOW"}},
			"contents":[` + user("What is 17 times 23?") + `]}`},
		{"error_not_found", "gemini-nonexistent-model", `{"store":false,"contents":[` + user("hi") + `]}`},
	}
	client := &http.Client{Timeout: 15 * time.Minute}
	for _, c := range cases {
		for _, stream := range []bool{false, true} {
			name, ext, method := c.name, ".json", ":generateContent"
			if stream {
				ext, method = ".sse", ":streamGenerateContent?alt=sse"
			}
			t.Run(name+ext, func(t *testing.T) {
				out, status := post(t, client, base+"/models/"+c.model+method, key, c.body)
				write(t, filepath.Join(dir, name+ext), out, key)
				t.Logf("HTTP %d: wrote %s", status, name+ext)
			})
		}
	}
}

// post sends body with the key in its header and returns the response
// body and status.
func post(t *testing.T, client *http.Client, url, key, body string) ([]byte, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Goog-Api-Key", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// The error may quote the URL, which never holds the key.
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return out, resp.StatusCode
}

// write saves a response body, unless it contains the key.
func write(t *testing.T, path string, body []byte, key string) {
	t.Helper()
	if bytes.Contains(body, []byte(key)) {
		t.Fatal("the response contains the API key; not writing it")
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
