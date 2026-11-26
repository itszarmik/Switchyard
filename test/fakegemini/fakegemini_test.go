package fakegemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"switchyard/test/fakeupstream"
)

// upstream is the method set the conformance suite needs from a fake.
type upstream interface {
	BaseURL() string
	Reset()
	Script(fakeupstream.Behavior)
	Requests() []fakeupstream.Request
	WaitReceived(ctx context.Context, n int) error
	Await(ctx context.Context, n int) ([]fakeupstream.Request, error)
}

var _ upstream = (*OpenAIUpstream)(nil)

const (
	testKey     = "AIzaSyFakeKeyForTheFakeItself0000000000"
	testModel   = "gemini-test"
	testTimeout = 10 * time.Second
)

func newServer(t *testing.T) *Server {
	t.Helper()
	s := New(Options{APIKey: testKey, Models: []string{testModel}})
	t.Cleanup(s.Close)
	return s
}

// post sends body to model's method with the key header, unless edit
// changes the request, and returns the status and body.
func post(t *testing.T, s *Server, method, body string, edit func(*http.Request)) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL()+"/models/"+testModel+":"+method, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Goog-Api-Key", testKey)
	req.Header.Set("Content-Type", "application/json")
	if edit != nil {
		edit(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

const (
	userHi = `{"role":"user","parts":[{"text":"hi"}]}`
	tools  = `"tools":[{"functionDeclarations":[{"name":"f"},{"name":"g"}]}]`
)

func TestCannedAnswers(t *testing.T) {
	s := newServer(t)
	tests := []struct {
		name, body, want string
	}{
		{"text", `{"store":false,"contents":[` + userHi + `]}`, fakeupstream.Text},
		{"stop sequence", `{"store":false,"contents":[` + userHi + `],"generationConfig":{"stopSequences":["fox"]}}`, "The quick brown "},
		{"json", `{"store":false,"contents":[` + userHi + `],"generationConfig":{"responseMimeType":"application/json"}}`, `{\"answer\":\"Paris\"`},
		{"parallel calls with a signature on the first", `{"store":false,"contents":[` + userHi + `],` + tools + `}`,
			`"thoughtSignature":"` + Signature(testModel, fakeupstream.ToolCallID(0, 0)) + `"`},
		{"a forced call", `{"store":false,"contents":[` + userHi + `],` + tools + `,"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["g"]}}}`,
			`"name":"g"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := post(t, s, "generateContent", tt.body, nil)
			if status != 200 || !strings.Contains(string(body), tt.want) {
				t.Errorf("HTTP %d %s, want %s", status, body, tt.want)
			}
		})
	}
	status, body := post(t, s, "streamGenerateContent?alt=sse", `{"store":false,"contents":[`+userHi+`]}`, nil)
	if status != 200 || strings.Count(string(body), "data: ") != 9 || !strings.Contains(string(body), `"finishReason":"STOP"`) ||
		strings.Contains(string(body), "[DONE]") {
		t.Errorf("stream HTTP %d:\n%s", status, body)
	}
}

// TestValidation checks the rejections the adapter's tests rely on: the
// documented Google rules, and the fake's loud checks of what the gateway
// must never send.
func TestValidation(t *testing.T) {
	sig := Signature(testModel, "c1")
	step := func(fc string) string {
		return `{"role":"model","parts":[` + fc + `]},{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"f","response":{}}}]}`
	}
	tests := []struct {
		name, method, body, want string
		edit                     func(*http.Request)
	}{
		{"wrong key", "generateContent", `{"store":false,"contents":[` + userHi + `]}`, "API_KEY_INVALID",
			func(r *http.Request) { r.Header.Set("X-Goog-Api-Key", "other") }},
		{"key in the URL", "generateContent", `{"store":false,"contents":[` + userHi + `]}`, "sent in the URL",
			func(r *http.Request) { r.URL.RawQuery = "key=" + testKey }},
		{"stream without alt=sse", "streamGenerateContent", `{"store":false,"contents":[` + userHi + `]}`, "alt=sse", nil},
		{"store missing", "generateContent", `{"contents":[` + userHi + `]}`, "every request must send", nil},
		{"store true", "generateContent", `{"store":true,"contents":[` + userHi + `]}`, "every request must send", nil},
		{"a field the gateway never sends", "generateContent", `{"store":false,"safetySettings":[],"contents":[` + userHi + `]}`, "safetySettings", nil},
		{"temperature", "generateContent", `{"store":false,"contents":[` + userHi + `],"generationConfig":{"temperature":0}}`, "temperature", nil},
		{"candidateCount", "generateContent", `{"store":false,"contents":[` + userHi + `],"generationConfig":{"candidateCount":2}}`, "candidateCount", nil},
		{"thinkingBudget", "generateContent", `{"store":false,"contents":[` + userHi + `],"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`, "thinkingLevel", nil},
		{"minimal thinking", "generateContent", `{"store":false,"contents":[` + userHi + `],"generationConfig":{"thinkingConfig":{"thinkingLevel":"MINIMAL"}}}`, "not supported", nil},
		{"over the non-streaming output cap", "generateContent", `{"store":false,"contents":[` + userHi + `],"generationConfig":{"maxOutputTokens":40000}}`, "use streaming", nil},
		{"prefill", "generateContent", `{"store":false,"contents":[` + userHi + `,{"role":"model","parts":[{"text":"Sure"}]}]}`, "prefilled", nil},
		{"missing signature in the current turn", "generateContent",
			`{"store":false,"contents":[` + userHi + `,` + step(`{"functionCall":{"id":"c1","name":"f","args":{}}}`) + `]}`, "missing a thought_signature", nil},
		{"altered signature", "generateContent",
			`{"store":false,"contents":[` + userHi + `,` + step(`{"functionCall":{"id":"c1","name":"f","args":{}},"thoughtSignature":"`+Signature(testModel, "c2")+`"}`) + `]}`, "Corrupted", nil},
		{"another model's signature", "generateContent",
			`{"store":false,"contents":[` + userHi + `,` + step(`{"functionCall":{"id":"c1","name":"f","args":{}},"thoughtSignature":"`+Signature("gemini-other", "c1")+`"}`) + `]}`, "Corrupted", nil},
		{"a signature on a second call", "generateContent",
			`{"store":false,"contents":[` + userHi + `,{"role":"model","parts":[{"functionCall":{"id":"c1","name":"f","args":{}},"thoughtSignature":"` + sig + `"},
			{"functionCall":{"id":"c2","name":"f","args":{}},"thoughtSignature":"` + sig + `"}]},
			{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"f","response":{}}},{"functionResponse":{"id":"c2","name":"f","response":{}}}]}]}`, "not the first", nil},
		{"interleaved calls and responses", "generateContent",
			`{"store":false,"contents":[` + userHi + `,{"role":"model","parts":[{"functionCall":{"id":"c1","name":"f","args":{}},"thoughtSignature":"` + sig + `"},
			{"functionCall":{"id":"c2","name":"f","args":{}}}]},
			{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"f","response":{}}}]}]}`, "does not match", nil},
		{"a response to an unknown call", "generateContent",
			`{"store":false,"contents":[` + userHi + `,{"role":"model","parts":[{"functionCall":{"id":"c1","name":"f","args":{}},"thoughtSignature":"` + sig + `"}]},
			{"role":"user","parts":[{"functionResponse":{"id":"c9","name":"f","response":{}}}]}]}`, "answers no function call", nil},
	}
	s := newServer(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := post(t, s, tt.method, tt.body, tt.edit)
			if status != 400 || !strings.Contains(string(body), tt.want) {
				t.Errorf("HTTP %d %s, want 400 containing %q", status, body, tt.want)
			}
			var env struct {
				Error struct {
					Code   int    `json:"code"`
					Status string `json:"status"`
				} `json:"error"`
			}
			if json.Unmarshal(body, &env) != nil || env.Error.Code != 400 || env.Error.Status != "INVALID_ARGUMENT" {
				t.Errorf("body %s is not a Google 400", body)
			}
		})
	}

	t.Run("accepted: the issued signature, a skip value, an older turn without one", func(t *testing.T) {
		for _, body := range []string{
			`{"store":false,"contents":[` + userHi + `,` + step(`{"functionCall":{"id":"c1","name":"f","args":{}},"thoughtSignature":"`+sig+`"}`) + `]}`,
			`{"store":false,"contents":[` + userHi + `,` + step(`{"functionCall":{"id":"c1","name":"f","args":{}},"thoughtSignature":"skip_thought_signature_validator"}`) + `]}`,
			`{"store":false,"contents":[` + userHi + `,` + step(`{"functionCall":{"id":"c1","name":"f","args":{}}}`) + `,` + userHi + `]}`,
		} {
			if status, resp := post(t, s, "generateContent", body, nil); status != 200 {
				t.Errorf("HTTP %d %s for %s", status, resp, body)
			}
		}
	})
}

func TestScriptedBehaviors(t *testing.T) {
	s := newServer(t)
	base := `{"store":false,"contents":[` + userHi + `]}`
	s.Script(RateLimited(7 * time.Second))
	if status, body := post(t, s, "generateContent", base, nil); status != 429 || !strings.Contains(string(body), `"retryDelay":"7s"`) {
		t.Errorf("rate limited: HTTP %d %s", status, body)
	}
	s.Script(Behavior{BlockReason: "SAFETY"})
	if status, body := post(t, s, "generateContent", base, nil); status != 200 || strings.Contains(string(body), "candidates") ||
		!strings.Contains(string(body), `"blockReason":"SAFETY"`) {
		t.Errorf("blocked: HTTP %d %s", status, body)
	}
	s.Script(StreamError(503, 2))
	if status, body := post(t, s, "streamGenerateContent?alt=sse", base, nil); status != 200 || strings.Count(string(body), "data: ") != 3 ||
		!strings.Contains(string(body), `"status":"UNAVAILABLE"`) {
		t.Errorf("stream error: HTTP %d %s", status, body)
	}
	s.Script(FromOpenAI(fakeupstream.RateLimited("4")))
	if status, body := post(t, s, "generateContent", base, nil); status != 429 || !strings.Contains(string(body), `"retryDelay":"4s"`) {
		t.Errorf("translated rate limit: HTTP %d %s", status, body)
	}
	s.Script(FromOpenAI(fakeupstream.APIError(400, "invalid_request_error", "invalid_value", "temperature", "Bad value.")))
	if status, body := post(t, s, "generateContent", base, nil); status != 400 || !strings.Contains(string(body), `"message":"Bad value."`) {
		t.Errorf("translated error: HTTP %d %s", status, body)
	}
	s.Reset()
	if n := len(s.Requests()); n != 0 {
		t.Errorf("%d requests after Reset", n)
	}
}
