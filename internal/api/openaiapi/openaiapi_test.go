package openaiapi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"switchyard/internal/api/openaiapi"
	"switchyard/internal/config"
)

// upstreamCompletion is a realistic chat.completion body, with formatting
// and fields (some unknown to any SDK) that must survive byte for byte.
const upstreamCompletion = `{
  "id": "chatcmpl-1",
  "object": "chat.completion",
  "created": 1700000000,
  "model": "upstream-gpt-2026-01-01",
  "choices": [{"index": 0, "message": {"role": "assistant", "content": "Hi <b>&amp;</b> é 😀", "refusal": null, "annotations": []}, "logprobs": null, "finish_reason": "stop"}],
  "usage": {"prompt_tokens": 9, "completion_tokens": 3, "total_tokens": 12, "prompt_tokens_details": {"cached_tokens": 0}},
  "service_tier": "default",
  "system_fingerprint": "fp_x",
  "x_vendor_extension": {"nested": [1, 2.50, "three", null], "big": 123456789012345678901234567890}
}`

func TestChatPassthrough(t *testing.T) {
	e := newEnv(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("x-request-id", "req-up-1")
		w.Header().Set("Openai-Processing-Ms", "12")
		_, _ = io.WriteString(w, upstreamCompletion)
	})
	clientBody := `{"model": "gpt-test", "messages": [{"role": "user", "content": "hello"}],
		"temperature": 0.2, "x_unknown": {"keep": [1, "two"]}, "metadata": {"k": "v"}}`
	header := http.Header{
		"X-Allowed":           {"yes"},
		"Openai-Organization": {"org-1"},
		"X-Not-Allowed":       {"no"},
		"Cookie":              {"session=abc"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	resp := e.post(ctx, t, clientBody, header)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	if string(body) != upstreamCompletion {
		t.Errorf("body is not the upstream body byte for byte:\n%s", body)
	}
	for k, want := range map[string]string{
		"Content-Type":                     "application/json",
		"X-Switchyard-Model":               "gpt-test",
		"X-Switchyard-Provider":            "fake",
		"X-Switchyard-Upstream-Request-Id": "req-up-1",
		"X-Switchyard-Attempts":            "1",
		"X-Switchyard-Route-Reason":        "cheapest_eligible",
		"Openai-Processing-Ms":             "", // upstream headers are not copied
	} {
		if v := resp.Header.Get(k); v != want {
			t.Errorf("header %s = %q, want %q", k, v, want)
		}
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("X-Request-Id missing")
	}

	got := e.up.got()
	if len(got) != 1 {
		t.Fatalf("upstream got %d requests", len(got))
	}
	wantUp := `{"model":"upstream-gpt","messages":[{"role": "user", "content": "hello"}],` +
		`"temperature":0.2,"x_unknown":{"keep": [1, "two"]},"metadata":{"k": "v"}}`
	if got[0].body != wantUp {
		t.Errorf("upstream body:\n got %s\nwant %s", got[0].body, wantUp)
	}
	h := got[0].header
	for k, want := range map[string]string{
		"Authorization":       "Bearer " + providerKey,
		"X-Allowed":           "yes",
		"Openai-Organization": "org-1",
		"X-Not-Allowed":       "",
		"Cookie":              "",
	} {
		if v := h.Get(k); v != want {
			t.Errorf("upstream header %s = %q, want %q", k, v, want)
		}
	}
	for k, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, clientKey) {
				t.Errorf("the client key reached the upstream in %s", k)
			}
		}
	}

	rec := e.requestLine(t)
	for k, want := range map[string]any{
		"requested_model": "gpt-test", "deployment": "gpt-test@fake", "stream": false, "status": float64(200),
		"prompt_tokens": float64(9), "completion_tokens": float64(3), "upstream_request_id": "req-up-1", "error_code": "",
	} {
		if rec[k] != want {
			t.Errorf("request line %s = %v, want %v", k, rec[k], want)
		}
	}
	if e.metricValue(t, "switchyard_upstream_requests_total", "gpt-test", "ok", "fake") != 1 {
		t.Error("upstream_requests_total{outcome=ok} not incremented")
	}
}

// OpenAI-shaped stream events, with "usage":null on every chunk but the
// last, as OpenAI sends them when include_usage is set.
var (
	chunkRole    = `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"upstream-gpt","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}`
	chunkContent = `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"upstream-gpt","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}],"usage":null,"x_extra":1}`
	chunkFinish  = `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"upstream-gpt","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`
	chunkUsage   = `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"upstream-gpt","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`
)

func streamUpstream(_ *upstream, w http.ResponseWriter, _ *http.Request) {
	sseHeaders(w)
	sse(w,
		"data: "+chunkRole+"\n\n",
		": keep-alive\n\n",
		"data: "+chunkContent+"\r\n\r\n",
		"data: "+chunkFinish+"\n\n",
		"data: "+chunkUsage+"\n\n",
		"data: [DONE]\n\n",
	)
}

func TestChatStream(t *testing.T) {
	tests := []struct {
		name          string
		streamOptions string
		wantUp        string // the stream_options the upstream must get
		wantUsage     bool
	}{
		{name: "no stream_options", wantUp: `{"include_usage":true}`},
		{name: "include_usage false", streamOptions: `,"stream_options":{"include_usage":false,"x":1}`, wantUp: `{"include_usage":true,"x":1}`},
		{name: "include_usage true", streamOptions: `,"stream_options":{"include_usage":true}`, wantUp: `{"include_usage":true}`, wantUsage: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, streamUpstream)
			resp, body := e.call(t, `{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"hi"}]`+tt.streamOptions+`}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body %s", resp.StatusCode, body)
			}
			for k, want := range map[string]string{
				"Content-Type":                     "text/event-stream",
				"Cache-Control":                    "no-cache",
				"X-Accel-Buffering":                "no",
				"X-Switchyard-Model":               "gpt-test",
				"X-Switchyard-Provider":            "fake",
				"X-Switchyard-Upstream-Request-Id": "req-up-1",
				"X-Switchyard-Attempts":            "1",
				"X-Switchyard-Route-Reason":        "cheapest_eligible",
			} {
				if v := resp.Header.Get(k); v != want {
					t.Errorf("header %s = %q, want %q", k, v, want)
				}
			}
			want := "data: " + chunkRole + "\n\n" + "data: " + chunkContent + "\n\n" + "data: " + chunkFinish + "\n\n"
			if tt.wantUsage {
				want += "data: " + chunkUsage + "\n\n"
			}
			want += "data: [DONE]\n\n"
			if body != want {
				t.Errorf("stream body:\n%s\nwant:\n%s", body, want)
			}

			var up map[string]json.RawMessage
			if err := json.Unmarshal([]byte(e.up.got()[0].body), &up); err != nil {
				t.Fatal(err)
			}
			if string(up["stream_options"]) != tt.wantUp {
				t.Errorf("upstream stream_options = %s, want %s", up["stream_options"], tt.wantUp)
			}

			// Usage is captured whether or not the client sees it.
			rec := e.requestLine(t)
			if rec["prompt_tokens"] != float64(7) || rec["completion_tokens"] != float64(2) || rec["stream"] != true {
				t.Errorf("request line = %v", rec)
			}
			if ttft, _ := rec["ttft_ms"].(float64); ttft <= 0 {
				t.Errorf("ttft_ms = %v", rec["ttft_ms"])
			}
		})
	}
}

// TestStreamCommitPoint checks that nothing is committed before the first
// upstream event: a failure up to that point is an ordinary HTTP error, and
// one after it is an error event in a 200 stream.
func TestStreamCommitPoint(t *testing.T) {
	const short = 150 * time.Millisecond
	tests := []struct {
		name       string
		handler    func(u *upstream, w http.ResponseWriter, r *http.Request)
		firstToken time.Duration
		wantStatus int
		wantCode   string
	}{
		{
			name: "headers, then the upstream ends before any event",
			handler: func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
				sseHeaders(w)
				sse(w, ": comment only\n\n")
			},
			wantStatus: http.StatusBadGateway, wantCode: "upstream_error",
		},
		{
			name: "headers, then no event within first_token",
			handler: func(u *upstream, w http.ResponseWriter, r *http.Request) {
				sseHeaders(w)
				sse(w, ": waiting\n\n")
				u.hang(r)
			},
			firstToken: short, wantStatus: http.StatusGatewayTimeout, wantCode: "timeout",
		},
		{
			name: "an error event first",
			handler: func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
				sseHeaders(w)
				sse(w, `data: {"error":{"message":"overloaded","type":"server_error"}}`+"\n\n")
			},
			wantStatus: http.StatusBadGateway, wantCode: "upstream_error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.handler, func(c *config.Config) {
				if tt.firstToken > 0 {
					c.Providers[0].Timeouts.FirstToken = config.Duration(tt.firstToken)
				}
			})
			resp, body := e.call(t, `{"model":"gpt-test","stream":true,"messages":[{}]}`)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", resp.StatusCode, tt.wantStatus, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want a JSON error", ct)
			}
			env := decodeError(t, body)
			if deref(env.Error.Code) != tt.wantCode || env.Error.Type != "server_error" {
				t.Errorf("error = %+v", env.Error)
			}
		})
	}
}

func TestStreamErrorAfterCommit(t *testing.T) {
	tests := []struct {
		name    string
		after   func(u *upstream, w http.ResponseWriter, r *http.Request)
		idle    time.Duration
		message string
	}{
		{
			name:    "upstream ends without [DONE]",
			after:   func(*upstream, http.ResponseWriter, *http.Request) {},
			message: "without [DONE]",
		},
		{
			name: "upstream drops the connection",
			after: func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
				if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
					_ = conn.Close()
				}
			},
			message: "connection",
		},
		{
			name: "upstream sends an error event",
			after: func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
				sse(w, `data: {"error":{"message":"internal","type":"server_error"}}`+"\n\n")
			},
			message: "error mid-stream",
		},
		{
			name:    "upstream goes idle",
			after:   func(u *upstream, _ http.ResponseWriter, r *http.Request) { u.hang(r) },
			idle:    150 * time.Millisecond,
			message: "idle timeout",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, func(u *upstream, w http.ResponseWriter, r *http.Request) {
				sseHeaders(w)
				sse(w, "data: "+chunkRole+"\n\n", "data: "+chunkContent+"\n\n")
				tt.after(u, w, r)
			}, func(c *config.Config) {
				if tt.idle > 0 {
					c.Providers[0].Timeouts.Idle = config.Duration(tt.idle)
				}
			})
			resp, body := e.call(t, `{"model":"gpt-test","stream":true,"messages":[{}]}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			prefix := "data: " + chunkRole + "\n\n" + "data: " + chunkContent + "\n\n"
			rest, ok := strings.CutPrefix(body, prefix)
			if !ok {
				t.Fatalf("stream does not start with the two chunks:\n%s", body)
			}
			if strings.Contains(rest, "data: [DONE]") {
				t.Errorf("a failed stream ended with [DONE]:\n%s", body)
			}
			payload, ok := strings.CutPrefix(rest, "data: ")
			if !ok || !strings.HasSuffix(payload, "\n\n") || strings.Count(payload, "\n\n") != 1 {
				t.Fatalf("want exactly one error event after the chunks, got %q", rest)
			}
			var ev map[string]map[string]any
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				t.Fatalf("error event %q: %v", payload, err)
			}
			if ev["error"]["type"] != "server_error" || ev["error"]["code"] != "upstream_error" {
				t.Errorf("error event = %v", ev)
			}
			if msg, _ := ev["error"]["message"].(string); !strings.Contains(msg, tt.message) {
				t.Errorf("message = %q, want it to mention %q", msg, tt.message)
			}
			if rec := e.requestLine(t); rec["error_code"] != "upstream_error" || rec["status"] != float64(200) {
				t.Errorf("request line = %v", rec)
			}
		})
	}
}

func TestUpstreamErrors(t *testing.T) {
	envelope := `{"error":{"message":"Invalid value for 'temperature'.","type":"invalid_request_error","param":"temperature","code":"invalid_value"}}`
	leaky := `{"error":{"message":"Incorrect API key provided: sk-provi*****************cdef. ` + providerKey + `","type":"invalid_request_error","code":"invalid_api_key"}}`
	tests := []struct {
		name       string
		status     int
		body       string
		header     map[string]string
		wantStatus int
		wantBody   string // exact body; empty means check wantCode instead
		wantCode   string
		wantType   string
		retryAfter string
		outcome    string
	}{
		{name: "400 envelope", status: 400, body: envelope, wantStatus: 400, wantBody: envelope, outcome: "client_error"},
		{name: "404 envelope", status: 404, body: envelope, wantStatus: 404, wantBody: envelope, outcome: "not_found"},
		{name: "422 envelope", status: 422, body: envelope, wantStatus: 422, wantBody: envelope, outcome: "client_error"},
		{name: "400 not an envelope", status: 400, body: "<html>bad</html>", wantStatus: 400, wantCode: "upstream_error", wantType: "invalid_request_error", outcome: "client_error"},
		{
			name: "429 keeps Retry-After", status: 429, body: envelope, header: map[string]string{"Retry-After": "7"},
			wantStatus: 429, wantBody: envelope, retryAfter: "7", outcome: "rate_limited",
		},
		{
			name: "429 not an envelope", status: 429, body: "slow down", header: map[string]string{"Retry-After": "2"},
			wantStatus: 429, wantCode: "rate_limit_exceeded", wantType: "requests", retryAfter: "2", outcome: "rate_limited",
		},
		{name: "401 is never echoed", status: 401, body: leaky, wantStatus: 502, wantCode: "upstream_error", wantType: "server_error", outcome: "auth"},
		{name: "403 is never echoed", status: 403, body: leaky, wantStatus: 502, wantCode: "upstream_error", wantType: "server_error", outcome: "auth"},
		{name: "500", status: 500, body: envelope, wantStatus: 502, wantCode: "upstream_error", wantType: "server_error", outcome: "server_error"},
		{name: "503", status: 503, body: "unavailable", wantStatus: 502, wantCode: "upstream_error", wantType: "server_error", outcome: "server_error"},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tt.name, stream), func(t *testing.T) {
				e := newEnv(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("x-request-id", "req-up-err")
					for k, v := range tt.header {
						w.Header().Set(k, v)
					}
					w.WriteHeader(tt.status)
					_, _ = io.WriteString(w, tt.body)
				})
				resp, body := e.call(t, fmt.Sprintf(`{"model":"gpt-test","messages":[{}],"stream":%v}`, stream))
				if resp.StatusCode != tt.wantStatus {
					t.Fatalf("status = %d, want %d; body %s", resp.StatusCode, tt.wantStatus, body)
				}
				if tt.wantBody != "" && body != tt.wantBody {
					t.Errorf("body = %s, want the upstream body %s", body, tt.wantBody)
				}
				if tt.wantBody == "" {
					env := decodeError(t, body)
					if deref(env.Error.Code) != tt.wantCode || env.Error.Type != tt.wantType || env.Error.Message == "" {
						t.Errorf("error = %+v, want type %q code %q", env.Error, tt.wantType, tt.wantCode)
					}
					if tt.status >= 500 && !strings.Contains(env.Error.Message, fmt.Sprint(tt.status)) {
						t.Errorf("message %q does not name the upstream status", env.Error.Message)
					}
				}
				if strings.Contains(body, providerKey) || strings.Contains(body, "sk-provi") {
					t.Errorf("the response leaks the provider key: %s", body)
				}
				if v := resp.Header.Get("Retry-After"); v != tt.retryAfter {
					t.Errorf("Retry-After = %q, want %q", v, tt.retryAfter)
				}
				if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Errorf("Content-Type = %q", ct)
				}
				if v := resp.Header.Get("X-Switchyard-Upstream-Request-Id"); v != "req-up-err" {
					t.Errorf("X-Switchyard-Upstream-Request-Id = %q", v)
				}
				if v := resp.Header.Get("X-Switchyard-Model"); v != "gpt-test" {
					t.Errorf("X-Switchyard-Model = %q", v)
				}
				waitFor(t, "the upstream outcome metric", func() bool {
					return e.metricValue(t, "switchyard_upstream_requests_total", "gpt-test", tt.outcome, "fake") == 1
				})
				if strings.Contains(e.logs.String(), providerKey) {
					t.Error("the logs contain the provider key")
				}
			})
		}
	}
}

func TestUpstreamUnreachableAndTimeouts(t *testing.T) {
	const short = 150 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedURL := "http://" + ln.Addr().String() + "/v1"
	_ = ln.Close()

	tests := []struct {
		name       string
		handler    func(u *upstream, w http.ResponseWriter, r *http.Request)
		mutate     func(*config.Config)
		stream     bool
		wantStatus int
		wantCode   string
		outcome    string
	}{
		{
			name:       "connection refused",
			handler:    func(*upstream, http.ResponseWriter, *http.Request) {},
			mutate:     func(c *config.Config) { c.Providers[0].BaseURL = closedURL },
			wantStatus: http.StatusBadGateway, wantCode: "upstream_error", outcome: "transport",
		},
		{
			name:       "connection refused, stream",
			handler:    func(*upstream, http.ResponseWriter, *http.Request) {},
			mutate:     func(c *config.Config) { c.Providers[0].BaseURL = closedURL },
			stream:     true,
			wantStatus: http.StatusBadGateway, wantCode: "upstream_error", outcome: "transport",
		},
		{
			name:       "total, no headers",
			handler:    func(u *upstream, _ http.ResponseWriter, r *http.Request) { u.hang(r) },
			mutate:     func(c *config.Config) { c.Providers[0].Timeouts.Total = config.Duration(short) },
			wantStatus: http.StatusGatewayTimeout, wantCode: "timeout", outcome: "timeout",
		},
		{
			name:       "first_token, stream",
			handler:    func(u *upstream, _ http.ResponseWriter, r *http.Request) { u.hang(r) },
			mutate:     func(c *config.Config) { c.Providers[0].Timeouts.FirstToken = config.Duration(short) },
			stream:     true,
			wantStatus: http.StatusGatewayTimeout, wantCode: "timeout", outcome: "timeout",
		},
		{
			name: "total, body stalls",
			handler: func(u *upstream, w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"id":`)
				w.(http.Flusher).Flush()
				u.hang(r)
			},
			mutate:     func(c *config.Config) { c.Providers[0].Timeouts.Total = config.Duration(short) },
			wantStatus: http.StatusGatewayTimeout, wantCode: "timeout", outcome: "timeout",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.handler, tt.mutate)
			resp, body := e.call(t, fmt.Sprintf(`{"model":"gpt-test","messages":[{}],"stream":%v}`, tt.stream))
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", resp.StatusCode, tt.wantStatus, body)
			}
			if env := decodeError(t, body); deref(env.Error.Code) != tt.wantCode || env.Error.Type != "server_error" {
				t.Errorf("error = %+v", env.Error)
			}
			waitFor(t, "the upstream outcome metric", func() bool {
				return e.metricValue(t, "switchyard_upstream_requests_total", "gpt-test", tt.outcome, "fake") == 1
			})
		})
	}
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			e := newEnv(t, func(u *upstream, w http.ResponseWriter, r *http.Request) {
				if stream {
					sseHeaders(w)
					sse(w, "data: "+chunkRole+"\n\n", "data: "+chunkContent+"\n\n")
				}
				u.hang(r)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var canceledAt time.Time
			if stream {
				resp := e.post(ctx, t, `{"model":"gpt-test","stream":true,"messages":[{}]}`, nil)
				defer func() { _ = resp.Body.Close() }()
				readEvents(t, bufio.NewReader(resp.Body), 2)
				canceledAt = time.Now()
				cancel()
			} else {
				errc := make(chan error, 1)
				go func() {
					req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/v1/chat/completions",
						strings.NewReader(`{"model":"gpt-test","messages":[{}]}`))
					resp, err := http.DefaultClient.Do(req)
					if err == nil {
						_ = resp.Body.Close()
					}
					errc <- err
				}()
				waitFor(t, "the upstream to receive the request", func() bool { return len(e.up.got()) == 1 })
				canceledAt = time.Now()
				cancel()
				<-errc
			}
			select {
			case doneAt := <-e.up.doneAt:
				if d := doneAt.Sub(canceledAt); d > 100*time.Millisecond {
					t.Errorf("the upstream request was canceled %s after the client left, want ≤ 100ms", d)
				}
			case <-time.After(testTimeout):
				t.Fatal("the upstream request was never canceled")
			}
			rec := e.requestLine(t)
			if code := rec["error_code"]; code != "client_disconnected" && code != "canceled" {
				t.Errorf("request line error_code = %v", code)
			}
			waitFor(t, "the canceled outcome", func() bool {
				return e.metricValue(t, "switchyard_upstream_requests_total", "gpt-test", "canceled", "fake") == 1
			})
		})
	}
}

func TestRequestTooLarge(t *testing.T) {
	e := newEnv(t, func(*upstream, http.ResponseWriter, *http.Request) {})
	big := `{"model":"gpt-test","messages":[{"role":"user","content":"` + strings.Repeat("x", maxBody) + `"}]}`
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	// No Content-Length, so the limit is hit while the handler reads.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/v1/chat/completions",
		io.MultiReader(strings.NewReader(big)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	if env := decodeError(t, string(body)); deref(env.Error.Code) != "request_too_large" || env.Error.Type != "invalid_request_error" {
		t.Errorf("error = %+v", env.Error)
	}
	if len(e.up.got()) != 0 {
		t.Error("the upstream was called")
	}
	if rec := e.requestLine(t); rec["status"] != float64(413) || rec["error_code"] != "request_too_large" {
		t.Errorf("request line = %v", rec)
	}
}

func TestInvalidRequests(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		param string
		code  string
	}{
		{name: "invalid JSON", body: `{"model": "gpt-test",`, param: "<null>", code: "invalid_json"},
		{name: "not an object", body: `["gpt-test"]`, param: "<null>", code: "invalid_json"},
		{name: "missing model", body: `{"messages":[{"role":"user","content":"x"}]}`, param: "model", code: "missing_required_parameter"},
		{name: "non-string model", body: `{"model":42,"messages":[{}]}`, param: "model", code: "invalid_type"},
		{name: "model too long", body: `{"model":"` + longModel + `","messages":[{}]}`, param: "model", code: "invalid_value"},
		{name: "missing messages", body: `{"model":"gpt-test"}`, param: "messages", code: "missing_required_parameter"},
		{name: "empty messages", body: `{"model":"gpt-test","messages":[]}`, param: "messages", code: "empty_array"},
		{name: "non-array messages", body: `{"model":"gpt-test","messages":"hi"}`, param: "messages", code: "invalid_type"},
		{name: "non-boolean stream", body: `{"model":"gpt-test","messages":[{}],"stream":"true"}`, param: "stream", code: "invalid_type"},
	}
	e := newEnv(t, func(*upstream, http.ResponseWriter, *http.Request) {})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := e.call(t, tt.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, body %s", resp.StatusCode, body)
			}
			env := decodeError(t, body)
			if env.Error.Type != "invalid_request_error" || deref(env.Error.Param) != tt.param || deref(env.Error.Code) != tt.code {
				t.Errorf("error = type %q param %q code %q, want param %q code %q",
					env.Error.Type, deref(env.Error.Param), deref(env.Error.Code), tt.param, tt.code)
			}
		})
	}
	if len(e.up.got()) != 0 {
		t.Error("the upstream was called for an invalid request")
	}
	// The over-long model is neither logged nor echoed.
	if logs := e.logs.String(); strings.Contains(logs, longModel[:64]) {
		t.Errorf("the logs contain the over-long model:\n%s", logs)
	}
}

// longModel is a model name over schema.MaxModelLength.
var longModel = strings.Repeat("m", 300)

func TestModelNotFound(t *testing.T) {
	e := newEnv(t, func(*upstream, http.ResponseWriter, *http.Request) {})
	resp, body := e.call(t, `{"model":"no-such-model","messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	env := decodeError(t, body)
	if env.Error.Type != "invalid_request_error" || deref(env.Error.Code) != "model_not_found" ||
		env.Error.Message != "The model `no-such-model` does not exist or you do not have access to it." {
		t.Errorf("error = %+v", env.Error)
	}
	if resp.Header.Get("X-Switchyard-Model") != "" {
		t.Error("X-Switchyard-Model set for an unknown model")
	}
	if e.metricValue(t, "switchyard_requests_total", "/v1/chat/completions", "4xx") != 1 {
		t.Error("requests_total{4xx} not incremented")
	}
}

// TestRoutingHeadersAndErrors checks the catalog routing over HTTP: the
// X-Switchyard-* headers and the routing errors.
func TestRoutingHeadersAndErrors(t *testing.T) {
	e := newEnv(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}, func(c *config.Config) { c.Routing.DropUnsupportedParams = true })

	resp, body := e.call(t, `{"model":"gpt-alias","messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Switchyard-Alias") != "gpt-alias" ||
		resp.Header.Get("X-Switchyard-Model") != "gpt-test" || resp.Header.Get("X-Switchyard-Provider") != "fake" ||
		resp.Header.Get("X-Switchyard-Dropped-Params") != "" {
		t.Errorf("alias: status %d, headers %v, body %s", resp.StatusCode, resp.Header, body)
	}

	// claude-test only streams; with dropping on, n and seed are removed
	// before the (unreachable) upstream is called, and the header says so.
	resp, _ = e.call(t, `{"model":"claude-test","n":2,"seed":1,"messages":[{"role":"user","content":"x"}]}`)
	if got := resp.Header.Get("X-Switchyard-Dropped-Params"); got != "n, seed" {
		t.Errorf("X-Switchyard-Dropped-Params = %q (status %d)", got, resp.StatusCode)
	}

	for _, tc := range []struct{ path, body, code string }{
		{"/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"x"}]}`, "model_not_supported"},
		{"/v1/chat/completions", `{"model":"embed-test","messages":[{"role":"user","content":"x"}]}`, "model_not_supported"},
		{"/v1/chat/completions", `{"model":"claude-test","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f"}}]}`, "no_eligible_model"},
		{"/v1/embeddings", `{"model":"gpt-test","input":"x"}`, "model_not_supported"},
		{"/v1/completions", `{"model":"claude-test","prompt":"x","echo":true}`, "unsupported_parameter"},
	} {
		resp, body := e.callPath(t, tc.path, tc.body)
		env := decodeError(t, body)
		if resp.StatusCode != http.StatusBadRequest || deref(env.Error.Code) != tc.code || env.Error.Type != "invalid_request_error" {
			t.Errorf("%s %s: status %d, error %+v", tc.path, tc.body, resp.StatusCode, env.Error)
		}
	}
	if v := e.metricValue(t, "switchyard_requests_total", "/v1/completions", "4xx"); v != 1 {
		t.Errorf("requests_total{/v1/completions,4xx} = %v, want 1", v)
	}
	if v := e.metricValue(t, "switchyard_requests_total", "/v1/embeddings", "4xx"); v != 1 {
		t.Errorf("requests_total{/v1/embeddings,4xx} = %v, want 1", v)
	}
}

// TestCompletionsAndEmbeddings checks the two new endpoints end to end
// against an OpenAI-wire upstream.
func TestCompletionsAndEmbeddings(t *testing.T) {
	e := newEnv(t, func(_ *upstream, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			_, _ = io.WriteString(w, `{"object":"list","data":[],"model":"upstream-embed","usage":{"prompt_tokens":2,"total_tokens":2}}`)
		case "/v1/chat/completions":
			sseHeaders(w)
			sse(w, `data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`+"\n\n",
				`data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n",
				"data: [DONE]\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}, func(c *config.Config) {
		c.Models[0].Capabilities = slices.DeleteFunc(slices.Clone(c.Models[0].Capabilities), func(s string) bool { return s == "completions" })
	})
	resp, body := e.callPath(t, "/v1/embeddings", `{"model":"embed-test","input":["a","b"]}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Switchyard-Model") != "embed-test" ||
		body != `{"object":"list","data":[],"model":"upstream-embed","usage":{"prompt_tokens":2,"total_tokens":2}}` {
		t.Errorf("embeddings: status %d, headers %v, body %s", resp.StatusCode, resp.Header, body)
	}
	if got := e.up.got(); len(got) != 1 || got[0].body != `{"model":"upstream-embed","input":["a","b"]}` {
		t.Errorf("embeddings upstream %+v", got)
	}

	// gpt-test lacks the completions capability here, so the shim serves
	// it.
	resp, body = e.callPath(t, "/v1/completions", `{"model":"gpt-test","prompt":"p","stream":true}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Switchyard-Shim") != "chat" ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("completions: status %d, headers %v", resp.StatusCode, resp.Header)
	}
	want := `data: {"id":"cmpl-9","object":"text_completion","created":1,"model":"m","choices":[{"text":"ok","index":0,"logprobs":null,"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	if body != want {
		t.Errorf("completions stream\n got %q\nwant %q", body, want)
	}
	waitFor(t, "the completions request line", func() bool {
		for _, l := range e.logs.find(t, "request") {
			if l["path"] == "/v1/completions" {
				return l["deployment"] == "gpt-test@fake" && l["requested_model"] == "gpt-test"
			}
		}
		return false
	})
}

func TestModels(t *testing.T) {
	before := time.Now().Unix()
	e := newEnv(t, func(*upstream, http.ResponseWriter, *http.Request) {})
	after := time.Now().Unix()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.url+"/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status = %d, Content-Type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) != 4 {
		t.Fatalf("list = %+v", list)
	}
	for i, id := range []string{"gpt-test", "claude-test", "embed-test", "gpt-alias"} {
		m := list.Data[i]
		if m.ID != id || m.Object != "model" || m.OwnedBy != "switchyard" || m.Created < before || m.Created > after {
			t.Errorf("data[%d] = %+v", i, m)
		}
	}
}

func TestNoSecretsInLogs(t *testing.T) {
	e := newEnv(t, func(_ *upstream, w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("X-Allowed"), "fail") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"bad key `+providerKey+`"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[]}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	for _, h := range []http.Header{nil, {"X-Allowed": {"fail"}}} {
		resp := e.post(ctx, t, `{"model":"gpt-test","messages":[{"role":"user","content":"PROMPT-CONTENT"}]}`, h)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if strings.Contains(string(body), providerKey) {
			t.Errorf("response leaks the provider key: %s", body)
		}
	}
	waitFor(t, "two request lines", func() bool { return len(e.requestLines(t)) == 2 })
	logs := e.logs.String()
	for _, secret := range []string{providerKey, clientKey, "PROMPT-CONTENT"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain %q", secret)
		}
	}
}

// A client that trickles its body slower than the body read timeout gets
// 408 once the timeout passes, and the handler returns.
func TestBodyReadTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	e := newEnvWith(t, func(*upstream, http.ResponseWriter, *http.Request) {}, openaiapi.Options{BodyReadTimeout: timeout})
	conn, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, rawRequest(1000)+`{"model":"gpt-test",`); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() { // one more byte every 50ms: never enough in time
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if _, err := io.WriteString(conn, " "); err != nil {
					return
				}
			}
		}
	}()

	start := time.Now()
	if err := conn.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if elapsed := time.Since(start); elapsed < timeout {
		t.Errorf("answered after %s, before the %s timeout", elapsed, timeout)
	}
	if resp.StatusCode != http.StatusRequestTimeout || !resp.Close {
		t.Fatalf("status %d (close %v), body %s; want 408 and Connection: close", resp.StatusCode, resp.Close, body)
	}
	if env := decodeError(t, string(body)); env.Error.Type != "invalid_request_error" || deref(env.Error.Code) != "request_timeout" {
		t.Errorf("error = %+v", env.Error)
	}
	e.waitIdle(t)
	if len(e.up.got()) != 0 {
		t.Error("the upstream was called")
	}
}

// A body that arrives in time is served normally, and the read deadline
// does not outlive the body read: the stream below runs past it.
func TestBodyReadTimeoutClearedAfterRead(t *testing.T) {
	const timeout = 200 * time.Millisecond
	e := newEnvWith(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
		sseHeaders(w)
		sse(w, "data: "+chunkRole+"\n\n")
		time.Sleep(3 * timeout)
		sse(w, "data: "+chunkUsage+"\n\n", "data: [DONE]\n\n")
	}, openaiapi.Options{BodyReadTimeout: timeout})
	resp, body := e.call(t, `{"model":"gpt-test","stream":true,"messages":[{}]}`)
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("status %d, body %q; want a complete stream", resp.StatusCode, body)
	}
	if rec := e.requestLine(t); rec["error_code"] != "" {
		t.Errorf("request line = %v", rec)
	}
}

// A client that never reads a large JSON response holds the handler only
// until the write timeout.
func TestWriteTimeoutNonStream(t *testing.T) {
	const timeout = 300 * time.Millisecond
	// Far more than the socket buffers hold, so the write blocks.
	big := `{"id":"c1","object":"chat.completion","choices":[],"pad":"` + strings.Repeat("x", 16<<20) + `"}`
	e := newEnvWith(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, big)
	}, openaiapi.Options{WriteTimeout: timeout})

	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
		}); err != nil {
			return err
		}
		return serr
	}}
	conn, err := d.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	body := `{"model":"gpt-test","messages":[{}]}`
	if _, err := io.WriteString(conn, rawRequest(len(body))+body); err != nil {
		t.Fatal(err)
	}
	// Never read: the handler must still return.
	waitFor(t, "the upstream call", func() bool { return len(e.up.got()) == 1 })
	e.waitIdle(t)
}
