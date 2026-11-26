package gemini

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
	"switchyard/test/fakegemini"
	"switchyard/test/fakeupstream"
)

func TestNew(t *testing.T) {
	if _, err := New(testProvider("http://[::1"), providers.Options{}); err == nil || strings.Contains(err.Error(), "[::1") {
		t.Errorf("New with a bad base_url: %v, want an error that does not echo it", err)
	}
	a := newAdapter(t, testProvider("https://generativelanguage.googleapis.com/v1beta/"))
	if got, want := a.endpoint("gemini-3.8-flash", false), "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.8-flash:generateContent"; got != want {
		t.Errorf("endpoint = %s, want %s", got, want)
	}
	if got, want := a.endpoint("models/gemini-4-carbon", true), "https://generativelanguage.googleapis.com/v1beta/models/gemini-4-carbon:streamGenerateContent?alt=sse"; got != want {
		t.Errorf("stream endpoint = %s, want %s", got, want)
	}
}

// TestRequestHeaders checks what goes upstream besides the body: the
// method URL, the key in x-goog-api-key only (never in the URL, never in
// Authorization), and only the forward_headers the adapter does not set
// itself.
func TestRequestHeaders(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			u := newUpstream(t, func(u *upstream, w http.ResponseWriter, r *http.Request) {
				if stream {
					sseHandler([]string{string(fixture(t, "stream_text.sse"))}, nil)(u, w, r)
					return
				}
				jsonHandler(200, string(fixture(t, "generate_text.json")))(u, w, r)
			})
			a := newAdapter(t, testProvider(u.URL+"/v1beta"))
			req := request(t, plainBody)
			req.Header.Set("X-Goog-User-Project", "proj-1")
			req.Header.Set("X-Goog-Api-Key", "client-key")
			req.Header.Set("Authorization", "Bearer sy_client")
			req.Header.Set("Accept-Encoding", "br")
			if stream {
				s, err := a.ChatStream(ctxWithTimeout(t), req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = collect(t, s)
				_ = s.Close()
			} else if _, err := a.Chat(ctxWithTimeout(t), req); err != nil {
				t.Fatal(err)
			}
			url, h, body := u.last()
			wantURL := "/v1beta/models/" + testModel + ":generateContent"
			if stream {
				wantURL = "/v1beta/models/" + testModel + ":streamGenerateContent?alt=sse"
			}
			if url != wantURL || strings.Contains(url, providerKey) {
				t.Errorf("URL %s, want %s", url, wantURL)
			}
			if h.Get("X-Goog-Api-Key") != providerKey || h.Get("Authorization") != "" {
				t.Errorf("x-goog-api-key %q, Authorization %q: want the provider key, no Authorization", h.Get("X-Goog-Api-Key"), h.Get("Authorization"))
			}
			if h.Get("X-Goog-User-Project") != "proj-1" || h.Get("Accept-Encoding") == "br" {
				t.Errorf("forwarded headers %v", h)
			}
			if !strings.HasPrefix(h.Get("User-Agent"), "switchyard/") || h.Get("Content-Type") != "application/json" {
				t.Errorf("User-Agent %q, Content-Type %q", h.Get("User-Agent"), h.Get("Content-Type"))
			}
			if !strings.Contains(body, `"store":false`) {
				t.Errorf("body %s does not send store false", body)
			}
		})
	}
}

func TestWithoutAPIKey(t *testing.T) {
	u := newUpstream(t, jsonHandler(200, string(fixture(t, "generate_text.json"))))
	p := testProvider(u.URL)
	p.APIKey = config.Secret{}
	if _, err := newAdapter(t, p).Chat(ctxWithTimeout(t), request(t, plainBody)); err != nil {
		t.Fatal(err)
	}
	if _, h, _ := u.last(); h.Values("X-Goog-Api-Key") != nil {
		t.Errorf("x-goog-api-key sent without a key: %q", h.Values("X-Goog-Api-Key"))
	}
}

// TestStatusErrors maps every error fixture onto a providers.Kind, for
// both calls: the client-visible kinds keep the Google error re-shaped
// into an OpenAI envelope; credential, account, timeout and server errors
// keep no body.
func TestStatusErrors(t *testing.T) {
	tests := []struct {
		fixture    string
		status     int
		kind       providers.Kind
		body       string // "" means no body is kept
		retryAfter time.Duration
	}{
		{fixture: "error_400_invalid_argument.json", status: 400, kind: providers.KindClientError,
			body: `{"error":{"message":"Function call is missing a thought_signature in functionCall parts. This is required for tools to work correctly, and missing thought_signature may lead to degraded model performance.","type":"invalid_request_error","param":null,"code":null}}`},
		{fixture: "error_400_api_key_invalid.json", status: 400, kind: providers.KindAuth},
		{fixture: "error_400_failed_precondition.json", status: 400, kind: providers.KindAuth},
		{fixture: "error_400_input_too_long.json", status: 400, kind: providers.KindContextLength,
			body: `{"error":{"message":"The input token count (1210441) exceeds the maximum number of tokens allowed (1048576).","type":"invalid_request_error","param":null,"code":null}}`},
		{fixture: "error_402_resource_exhausted.json", status: 402, kind: providers.KindAuth},
		{fixture: "error_403_permission_denied.json", status: 403, kind: providers.KindAuth},
		{fixture: "error_404_not_found.json", status: 404, kind: providers.KindNotFound,
			body: `{"error":{"message":"models/gemini-nonexistent is not found for API version v1beta, or is not supported for generateContent. Call ListModels to see the list of available models and their supported methods.","type":"invalid_request_error","param":null,"code":"not_found"}}`},
		{fixture: "error_408_deadline_exceeded.json", status: 408, kind: providers.KindTimeout},
		{fixture: "error_429_resource_exhausted.json", status: 429, kind: providers.KindRateLimited, retryAfter: 7 * time.Second,
			body: `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"requests","param":null,"code":"rate_limit_exceeded"}}`},
		{fixture: "error_500_internal.json", status: 500, kind: providers.KindServerError},
		{fixture: "error_503_unavailable.json", status: 503, kind: providers.KindServerError},
		{fixture: "error_504_deadline_exceeded.json", status: 504, kind: providers.KindTimeout},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tt.fixture, stream), func(t *testing.T) {
				f := newFake(t)
				f.Script(fakegemini.Behavior{Status: tt.status, Body: fixture(t, tt.fixture)})
				a := newAdapter(t, testProvider(f.BaseURL()))
				pe := wantKind(t, doCall(t, a, stream), tt.kind)
				if pe.Status != tt.status || pe.RetryAfter != tt.retryAfter {
					t.Errorf("error = %+v", pe)
				}
				if tt.body == "" && pe.Body != nil {
					t.Errorf("body kept: %s", pe.Body)
				}
				if tt.body != "" {
					if diff := jsonDiff(t, []byte(tt.body), pe.Body); diff != "" {
						t.Errorf("body (-want +got):\n%s", diff)
					}
				}
				var env errorEnvelope
				_ = json.Unmarshal(fixture(t, tt.fixture), &env)
				readsBody := tt.status != 402 && tt.status != 403
				if readsBody && !strings.Contains(pe.Message, "("+env.Error.Status+")") {
					t.Errorf("message = %q, want it to name %s", pe.Message, env.Error.Status)
				}
				if strings.Contains(pe.Message, env.Error.Message) {
					t.Errorf("message quotes the upstream message: %q", pe.Message)
				}
			})
		}
	}
}

// TestRetryAfterHeader checks that a 429 without RetryInfo falls back to a
// Retry-After header, and that RetryInfo wins over it.
func TestRetryAfterHeader(t *testing.T) {
	for name, tt := range map[string]struct {
		body string
		want time.Duration
	}{
		"header only":         {`{"error":{"code":429,"message":"m","status":"RESOURCE_EXHAUSTED"}}`, 3 * time.Second},
		"RetryInfo wins":      {string(fixture(t, "error_429_resource_exhausted.json")), 7 * time.Second},
		"fractional seconds":  {`{"error":{"code":429,"message":"m","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"1.5s"}]}}`, 1500 * time.Millisecond},
		"unusable RetryInfo":  {`{"error":{"code":429,"message":"m","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"soon"}]}}`, 3 * time.Second},
		"unrelated details":   {`{"error":{"code":429,"message":"m","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure"}]}}`, 3 * time.Second},
		"a body that is text": {`slow down`, 3 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			u := newUpstream(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, tt.body)
			})
			pe := wantKind(t, doCall(t, newAdapter(t, testProvider(u.URL)), false), providers.KindRateLimited)
			if pe.RetryAfter != tt.want {
				t.Errorf("RetryAfter = %s, want %s", pe.RetryAfter, tt.want)
			}
		})
	}
}

func TestStatusErrorBodies(t *testing.T) {
	tests := []struct {
		name   string
		status int
		header map[string]string
		body   string
		kind   providers.Kind
		keep   bool
	}{
		{name: "400 that is not JSON", status: 400, body: "nope", kind: providers.KindClientError},
		{name: "400 with an odd status name", status: 400, body: `{"error":{"code":400,"message":"x","status":"Bad Status!"}}`, kind: providers.KindClientError},
		{name: "422", status: 422, body: `{"error":{"code":422,"message":"x","status":"INVALID_ARGUMENT"}}`, kind: providers.KindClientError, keep: true},
		{name: "401", status: 401, body: `{"error":{"code":401,"message":"x","status":"UNAUTHENTICATED"}}`, kind: providers.KindAuth},
		{name: "413", status: 413, body: `{"error":{"code":413,"message":"too big","status":"INVALID_ARGUMENT"}}`, kind: providers.KindContextLength, keep: true},
		{name: "redirect is not followed", status: 307, header: map[string]string{"Location": "/elsewhere"}, kind: providers.KindServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newUpstream(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
				for k, v := range tt.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			pe := wantKind(t, doCall(t, newAdapter(t, testProvider(u.URL)), false), tt.kind)
			if tt.keep != (pe.Body != nil) {
				t.Errorf("body kept = %v (%s), want %v", pe.Body != nil, pe.Body, tt.keep)
			}
			if strings.Contains(pe.Message, "Bad Status") || strings.Contains(pe.Message, "nope") {
				t.Errorf("message quotes the body: %q", pe.Message)
			}
		})
	}
}

// TestKeyNeverEchoed checks that the provider key never leaves the adapter
// in an error, whatever the upstream echoes back: error bodies of every
// class, the responseId, and stream error events.
func TestKeyNeverEchoed(t *testing.T) {
	spellings := map[string]string{"clear": providerKey}
	for name, k := range escapedKeys {
		spellings[name] = k
	}
	for spelling, key := range spellings {
		echoes := map[string]string{
			"message": `{"error":{"code":%d,"message":"API key ` + key + ` not valid","status":"INVALID_ARGUMENT"}}`,
			"details": `{"error":{"code":%d,"message":"m","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"` + key + `"}]}}`,
		}
		for field, echo := range echoes {
			for _, status := range []int{400, 403, 404, 429, 500, 503} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%d/stream=%v", spelling, field, status, stream), func(t *testing.T) {
						u := newUpstream(t, jsonHandler(status, fmt.Sprintf(echo, status)))
						err := doCall(t, newAdapter(t, testProvider(u.URL)), stream)
						// Whatever the kind, wantKind fails if the key leaks.
						pe := wantKind(t, err, providers.Kind(providers.Outcome(err)))
						if pe.Body != nil {
							t.Errorf("error kept the echoing body: %s", pe.Body)
						}
					})
				}
			}
		}
		t.Run(spelling+"/stream error event and responseId", func(t *testing.T) {
			for _, ev := range []string{
				`{"error":{"code":500,"message":"` + key + `","status":"INTERNAL"}}`,
				`{"error":{"code":400,"message":"m","status":"INVALID_ARGUMENT","details":[{"@type":"x","reason":"` + key + `"}]}}`,
			} {
				u := newUpstream(t, sseHandler([]string{data(`{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"responseId":"` + key + `"}`), data(ev)}, nil))
				s, err := newAdapter(t, testProvider(u.URL)).ChatStream(ctxWithTimeout(t), request(t, streamBody))
				if err != nil {
					t.Fatal(err)
				}
				_, err = collect(t, s)
				pe := wantKind(t, err, providers.Kind(providers.Outcome(err)))
				if pe.Body != nil || pe.UpstreamRequestID != "" || s.UpstreamRequestID() != "" {
					t.Errorf("error kept the echoing body or response id: %+v", pe)
				}
				_ = s.Close()
			}
		})
	}
	t.Run("the fake's answer to a wrong key", func(t *testing.T) {
		f := fakegemini.New(fakegemini.Options{APIKey: "AIzaSyOtherKeyOtherKeyOtherKeyOtherKey00"})
		t.Cleanup(f.Close)
		pe := wantKind(t, doCall(t, newAdapter(t, testProvider(f.BaseURL())), false), providers.KindAuth)
		if pe.Status != 400 || pe.Body != nil {
			t.Errorf("error = %+v", pe)
		}
	})
}

// TestUnsupportedIsNotSent checks that a request the adapter cannot
// translate is rejected locally, without an upstream call.
func TestUnsupportedIsNotSent(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, testProvider(f.BaseURL()))
	for _, body := range []string{
		`{"model":"m","n":3,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"Sure"}]}`,
	} {
		for _, stream := range []bool{false, true} {
			var err error
			if stream {
				_, err = a.ChatStream(ctxWithTimeout(t), request(t, body, schema.CapVision))
			} else {
				_, err = a.Chat(ctxWithTimeout(t), request(t, body, schema.CapVision))
			}
			_ = wantKind(t, err, providers.KindUnsupported)
		}
	}
	if n := len(f.Requests()); n != 0 {
		t.Errorf("upstream received %d requests, want none", n)
	}
}

// TestFakeAcceptsTranslations sends representative translated requests to
// the fake, which validates them strictly: store false, no sampling or
// unknown fields, well-formed parts, function responses that answer the
// calls before them.
func TestFakeAcceptsTranslations(t *testing.T) {
	bodies := []string{
		`{"model":"m","max_tokens":100,"stop":["x"],"reasoning_effort":"minimal","user":"u","n":1,"logprobs":false,"presence_penalty":0,
			"messages":[{"role":"system","content":"s"},{"role":"user","content":[{"type":"text","text":"hi"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
		`{"model":"m","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"f"}},
			"messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"r"},{"role":"user","content":"and?"}]}`,
		`{"model":"m","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"a","schema":{"type":"object"}}},"messages":[{"role":"user","content":"hi"}]}`,
	}
	f := newFake(t)
	a := newAdapter(t, testProvider(f.BaseURL()))
	for i, b := range bodies {
		for _, stream := range []bool{false, true} {
			req := request(t, b, allCaps...)
			if stream {
				s, err := a.ChatStream(ctxWithTimeout(t), req)
				if err != nil {
					t.Errorf("request %d (stream): %v", i, err)
					continue
				}
				_, _ = collect(t, s)
				_ = s.Close()
			} else if _, err := a.Chat(ctxWithTimeout(t), req); err != nil {
				t.Errorf("request %d: %v", i, describe(err))
			}
		}
	}
}

func describe(err error) string {
	var pe *providers.Error
	if errors.As(err, &pe) && pe.Body != nil {
		return pe.Error() + ": " + string(pe.Body)
	}
	return err.Error()
}

// completionOf decodes a chat.completion body.
type (
	completionOf struct {
		Choices []choiceOf   `json:"choices"`
		Usage   schema.Usage `json:"usage"`
	}
	choiceOf struct {
		Message struct {
			Content   *string      `json:"content"`
			ToolCalls []toolCallOf `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	}
	toolCallOf struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
)

func decodeCompletion(t *testing.T, body []byte) completionOf {
	t.Helper()
	var c completionOf
	if err := json.Unmarshal(body, &c); err != nil || len(c.Choices) != 1 {
		t.Fatalf("completion %s: %v", body, err)
	}
	return c
}

// TestLongOutput checks the long-output path: a non-streaming request for
// more than 32,768 output tokens, which Google refuses on generateContent
// (and so does the fake), is sent to streamGenerateContent and comes back
// as a normal chat.completion: content, tool calls, finish reason and
// usage. Up to 32,768 it stays on generateContent.
func TestLongOutput(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, testProvider(f.BaseURL()))

	f.Script(fakegemini.Behavior{Usage: &fakegemini.Usage{Prompt: 9, Cached: 4, Candidates: 40000, Thoughts: 2000}})
	resp, err := a.Chat(ctxWithTimeout(t), request(t, `{"model":"m","max_completion_tokens":65536,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("Chat: %v", describe(err))
	}
	c := decodeCompletion(t, resp.Body)
	if c.Choices[0].Message.Content == nil || *c.Choices[0].Message.Content != fakeupstream.Text || c.Choices[0].FinishReason != "stop" {
		t.Errorf("completion %s", resp.Body)
	}
	if u := resp.Usage; u == nil || u.PromptTokens != 9 || u.CompletionTokens != 42000 ||
		u.PromptTokensDetails.CachedTokens != 4 || u.CompletionTokensDetails.ReasoningTokens != 2000 {
		t.Errorf("usage %+v", resp.Usage)
	}
	if resp.TTFT <= 0 || resp.Status != 200 {
		t.Errorf("status %d, TTFT %s", resp.Status, resp.TTFT)
	}
	reqs := f.Requests()
	if len(reqs) != 1 || !strings.HasSuffix(reqs[0].Path, ":streamGenerateContent") ||
		!strings.Contains(string(reqs[0].Body), `"maxOutputTokens":65536`) {
		t.Fatalf("requests %+v: want one streamGenerateContent call with the full maxOutputTokens", reqs)
	}

	f.Reset()
	resp, err = a.Chat(ctxWithTimeout(t), request(t, `{"model":"m","max_tokens":40000,"messages":[{"role":"user","content":"weather?"}],
		"tools":[{"type":"function","function":{"name":"get_weather"}},{"type":"function","function":{"name":"get_time"}}]}`, allCaps...))
	if err != nil {
		t.Fatalf("Chat with tools: %v", describe(err))
	}
	if c := decodeCompletion(t, resp.Body); len(c.Choices[0].Message.ToolCalls) != 2 || c.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("completion %s, want two tool calls", resp.Body)
	}

	f.Reset()
	if _, err := a.Chat(ctxWithTimeout(t), request(t, `{"model":"m","max_tokens":32768,"messages":[{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatalf("Chat at the cap: %v", err)
	}
	if reqs := f.Requests(); len(reqs) != 1 || !strings.HasSuffix(reqs[0].Path, ":generateContent") {
		t.Errorf("at 32,768 the request went to %s, want generateContent", reqs[0].Path)
	}

	t.Run("the catalog's limit when the client sets none", func(t *testing.T) {
		for _, tt := range []struct {
			name, body string
			catalog    int64
			stream     bool
		}{
			{"no max_tokens, catalog 65,536", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, 65536, true},
			{"no max_tokens, catalog 32,768", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, 32768, false},
			{"no max_tokens, catalog unknown", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, 0, false},
			{"max_tokens 1,000 wins over catalog 65,536", `{"model":"m","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`, 65536, false},
		} {
			t.Run(tt.name, func(t *testing.T) {
				f.Reset()
				req := request(t, tt.body)
				req.MaxOutputTokens = tt.catalog
				resp, err := a.Chat(ctxWithTimeout(t), req)
				if err != nil {
					t.Fatalf("Chat: %v", describe(err))
				}
				if c := decodeCompletion(t, resp.Body); c.Choices[0].FinishReason != "stop" {
					t.Errorf("completion %s", resp.Body)
				}
				reqs := f.Requests()
				if len(reqs) != 1 || strings.HasSuffix(reqs[0].Path, ":streamGenerateContent") != tt.stream {
					t.Fatalf("requests %+v: want streamGenerateContent = %v", reqs, tt.stream)
				}
				// The catalog's limit only picks the path: without a client
				// limit none is sent, so Google's streaming default applies.
				if tt.catalog > 0 && !strings.Contains(tt.body, "max_tokens") && strings.Contains(string(reqs[0].Body), "maxOutputTokens") {
					t.Errorf("body %s sends maxOutputTokens without a client limit", reqs[0].Body)
				}
			})
		}
	})

	t.Run("failures", func(t *testing.T) {
		long := `{"model":"m","max_tokens":40000,"messages":[{"role":"user","content":"hi"}]}`
		for name, tt := range map[string]struct {
			b    fakegemini.Behavior
			kind providers.Kind
		}{
			"an error event":         {fakegemini.StreamError(503, 2), providers.KindServerError},
			"a 429":                  {fakegemini.RateLimited(5 * time.Second), providers.KindRateLimited},
			"a dropped connection":   {fakegemini.Behavior{Fault: fakegemini.FaultDrop, FaultAfterChunks: 3}, providers.KindTransport},
			"a blocked prompt":       {fakegemini.Behavior{BlockReason: "SAFETY"}, ""},
			"a missing signature":    {fakegemini.Behavior{FinishReason: "MISSING_THOUGHT_SIGNATURE"}, providers.KindClientError},
			"a malformed call":       {fakegemini.Behavior{FinishReason: "MALFORMED_FUNCTION_CALL"}, providers.KindUnsupported},
			"a total timeout (hang)": {fakegemini.Behavior{Fault: fakegemini.FaultHang, FaultAfterChunks: 1}, providers.KindTimeout},
		} {
			t.Run(name, func(t *testing.T) {
				f.Reset()
				f.Script(tt.b)
				p := testProvider(f.BaseURL())
				p.Timeouts.Total = config.Duration(300 * time.Millisecond)
				p.Timeouts.Idle = config.Duration(time.Millisecond) // a non-streaming call has no idle timeout
				resp, err := newAdapter(t, p).Chat(ctxWithTimeout(t), request(t, long))
				if tt.kind == "" {
					if err != nil || decodeCompletion(t, resp.Body).Choices[0].FinishReason != "content_filter" {
						t.Fatalf("blocked prompt: %v", err)
					}
					return
				}
				pe := wantKind(t, err, tt.kind)
				if tt.kind == providers.KindTimeout && pe.Timeout != providers.TimeoutTotal {
					t.Errorf("timeout %q, want total", pe.Timeout)
				}
			})
		}
	})
}

// TestCallsFromFake checks the fake's scripted faults and answers through
// the adapter.
func TestCallsFromFake(t *testing.T) {
	f := newFake(t)
	p := testProvider(f.BaseURL())
	p.Timeouts.FirstToken = config.Duration(200 * time.Millisecond)
	p.Timeouts.Total = config.Duration(time.Second) // bounds the non-streaming hang
	a := newAdapter(t, p)

	f.Script(fakegemini.Behavior{Fault: fakegemini.FaultDrop})
	_ = wantKind(t, doCall(t, a, false), providers.KindTransport)

	f.Script(fakegemini.Behavior{Fault: fakegemini.FaultHang})
	_ = wantKind(t, doCall(t, a, false), providers.KindTimeout)

	f.Script(fakegemini.StreamError(503, 3))
	pe := wantKind(t, doCall(t, a, true), providers.KindServerError)
	if !strings.Contains(pe.Message, "UNAVAILABLE") {
		t.Errorf("message = %q", pe.Message)
	}

	for _, stream := range []bool{false, true} {
		f.Script(fakegemini.Behavior{BlockReason: "PROHIBITED_CONTENT"})
		body := plainBody
		if stream {
			body = streamBody
		}
		if stream {
			s, err := a.ChatStream(ctxWithTimeout(t), request(t, body))
			if err != nil {
				t.Fatal(err)
			}
			got, _ := collect(t, s)
			_ = s.Close()
			if len(got) != 2 || !strings.Contains(got[1], `"finish_reason":"content_filter"`) {
				t.Errorf("blocked stream: %v", got)
			}
			continue
		}
		resp, err := a.Chat(ctxWithTimeout(t), request(t, body))
		if err != nil || decodeCompletion(t, resp.Body).Choices[0].FinishReason != "content_filter" {
			t.Errorf("blocked prompt: %v", err)
		}
	}
}

// TestSignaturesThroughFake runs multi-turn tool use against the fake,
// which, like Google, rejects a current-turn step whose first function
// call has no thought signature, and accepts only the signature it issued
// for that call and model: the ids the adapter returned carry the
// signatures back byte for byte, for parallel calls and for sequential
// steps, with no state in the adapter.
func TestSignaturesThroughFake(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, testProvider(f.BaseURL()))
	tools := `"tools":[{"type":"function","function":{"name":"get_weather"}},{"type":"function","function":{"name":"get_time"}}]`
	history := []string{`{"role":"user","content":"weather and time in Paris?"}`}
	call := func(model string) completionOf {
		t.Helper()
		req := request(t, `{"model":"m","messages":[`+strings.Join(history, ",")+`],`+tools+`}`, allCaps...)
		req.UpstreamModel = model
		resp, err := a.Chat(ctxWithTimeout(t), req)
		if err != nil {
			t.Fatalf("turn %d: %v", len(history), describe(err))
		}
		return decodeCompletion(t, resp.Body)
	}
	// answer appends the assistant message with calls and one tool message
	// per call, with ids edited by edit.
	answer := func(c completionOf, edit func(string) string) {
		var tcs, results []string
		for _, tc := range c.Choices[0].Message.ToolCalls {
			id := edit(tc.ID)
			tcs = append(tcs, `{"id":"`+id+`","type":"function","function":{"name":"`+tc.Function.Name+`","arguments":`+string(mustJSONString(tc.Function.Arguments))+`}}`)
			results = append(results, `{"role":"tool","tool_call_id":"`+id+`","content":"ok"}`)
		}
		history = append(history, `{"role":"assistant","tool_calls":[`+strings.Join(tcs, ",")+`]}`)
		history = append(history, results...)
	}
	same := func(id string) string { return id }

	// Step 1 returns two parallel calls; steps 2 and 3 follow without a
	// user message between them, so every step is in the current turn and
	// validated.
	for step := 1; step <= 3; step++ {
		c := call(testModel)
		if len(c.Choices[0].Message.ToolCalls) != 2 {
			t.Fatalf("step %d: %d tool calls, want 2", step, len(c.Choices[0].Message.ToolCalls))
		}
		answer(c, same)
	}
	call(testModel) // the fake validated all three steps
	last := f.Requests()[len(f.Requests())-1].Body
	sig := fakegemini.Signature(testModel, fakeupstream.ToolCallID(0, 0))
	if n := strings.Count(string(last), `"thoughtSignature":"`+sig+`"`); n != 3 {
		t.Errorf("the last request carries the signature %d times, want once per step (3): %s", n, last)
	}
	if strings.Contains(string(last), SkipSignature) {
		t.Errorf("the last request uses the skip value: %s", last)
	}

	t.Run("a history from another Gemini model gets the skip value", func(t *testing.T) {
		f.Reset()
		call("gemini-other")
		if len(f.Requests()) != 1 {
			t.Fatal("no request")
		}
		body := string(f.Requests()[0].Body)
		if n := strings.Count(body, `"thoughtSignature":"`+SkipSignature+`"`); n != 3 || strings.Contains(body, sig) {
			t.Errorf("request to another model: %d skip values, want one per step (3), and no signature: %s", n, body)
		}
		// The calls and responses carry the decoded Gemini ids, not the
		// whole client-facing ids.
		if strings.Contains(body, toolIDPrefix) || !strings.Contains(body, `"id":"`+fakeupstream.ToolCallID(0, 0)+`"`) {
			t.Errorf("request to another model sends the client-facing ids: %s", body)
		}
	})
	t.Run("an unsigned call of ours gets the skip value, which Google accepts", func(t *testing.T) {
		unsigned := encodeToolCallID(testModel, "fc_1", "")
		f.Reset()
		history = []string{`{"role":"user","content":"weather?"}`,
			`{"role":"assistant","tool_calls":[{"id":"` + unsigned + `","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}`,
			`{"role":"tool","tool_call_id":"` + unsigned + `","content":"18C"}`}
		call(testModel) // the fake, like Google, rejects a current-turn step without a signature
		if body := string(f.Requests()[0].Body); !strings.Contains(body, `"id":"fc_1","name":"get_weather","args":{}},"thoughtSignature":"`+SkipSignature+`"`) {
			t.Errorf("unsigned call: %s", body)
		}
	})
	t.Run("ids a client rewrote get the skip value", func(t *testing.T) {
		history = history[:1]
		answer(call(testModel), func(id string) string { return "call_" + id[len(id)-8:] })
		f.Reset()
		call(testModel)
		body := string(f.Requests()[0].Body)
		if !strings.Contains(body, `"thoughtSignature":"`+SkipSignature+`"`) {
			t.Errorf("rewritten ids: %s", body)
		}
	})
	t.Run("a tampered id gets the skip value", func(t *testing.T) {
		history = history[:1]
		answer(call(testModel), func(id string) string {
			b := []byte(id)
			b[len(b)/2] ^= 'a' ^ 'b'
			if b[len(b)/2] == '-' || b[len(b)/2] == '_' {
				b[len(b)/2] = 'x'
			}
			return string(b)
		})
		f.Reset()
		call(testModel)
		if body := string(f.Requests()[0].Body); !strings.Contains(body, SkipSignature) {
			t.Errorf("tampered id: %s", body)
		}
	})
	t.Run("streamed ids carry the signature too", func(t *testing.T) {
		history = history[:1]
		req := request(t, `{"model":"m","stream":true,"messages":[`+strings.Join(history, ",")+`],`+tools+`}`, allCaps...)
		s, err := a.ChatStream(ctxWithTimeout(t), req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := collect(t, s)
		_ = s.Close()
		c := completionOf{Choices: make([]choiceOf, 1)}
		for _, raw := range got {
			var ch struct {
				Choices []struct {
					Delta struct {
						ToolCalls []toolCallOf `json:"tool_calls"`
					} `json:"delta"`
				} `json:"choices"`
			}
			_ = json.Unmarshal([]byte(raw), &ch)
			if len(ch.Choices) > 0 {
				c.Choices[0].Message.ToolCalls = append(c.Choices[0].Message.ToolCalls, ch.Choices[0].Delta.ToolCalls...)
			}
		}
		answer(c, same)
		f.Reset()
		call(testModel)
		if body := string(f.Requests()[0].Body); !strings.Contains(body, sig) {
			t.Errorf("streamed ids lost the signature: %s", body)
		}
	})
}
