package anthropic

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
	"switchyard/test/fakeanthropic"
)

func TestNew(t *testing.T) {
	for _, base := range []string{"http://upstream.test", "http://upstream.test/"} {
		p := testProvider(base)
		p.Timeouts.Connect = config.Duration(1234 * time.Millisecond)
		a := newAdapter(t, p)
		if a.endpoint != "http://upstream.test/v1/messages" {
			t.Errorf("base %q: endpoint = %q", base, a.endpoint)
		}
		tr, ok := a.client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("transport is %T", a.client.Transport)
		}
		if tr.TLSHandshakeTimeout != 1234*time.Millisecond || a.dialer.Timeout != 1234*time.Millisecond {
			t.Errorf("connect timeout not applied: TLS %s, dial %s", tr.TLSHandshakeTimeout, a.dialer.Timeout)
		}
		if tr.MaxIdleConnsPerHost < 100 || tr.DisableKeepAlives || a.client.Timeout != 0 {
			t.Errorf("transport not pooled or client timeout set: %+v", tr)
		}
		// x-api-key, anthropic-version, Authorization and Accept-Encoding
		// are listed in the test provider but never forwarded.
		if fmt.Sprint(a.forward) != "[Anthropic-Beta]" {
			t.Errorf("forward = %v, want [Anthropic-Beta]", a.forward)
		}
	}
}

// TestRequestHeaders checks what reaches the upstream: the endpoint, the
// provider key in x-api-key, the API version, forwarded client headers,
// and never the client's credentials.
func TestRequestHeaders(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			f := newFake(t)
			a := newAdapter(t, testProvider(f.BaseURL()))
			h := http.Header{}
			h.Set("Authorization", "Bearer sy_client_key_must_not_be_forwarded")
			h.Set("X-Api-Key", "client-supplied-key")
			h.Set("Anthropic-Version", "1999-01-01")
			h.Set("Cookie", "session=1")
			h.Add("Anthropic-Beta", "beta-one")
			h.Add("Anthropic-Beta", "beta-two")
			h.Set("X-Not-Allowed", "no")
			if stream {
				s, err := a.ChatStream(ctxWithTimeout(t), withHeader(request(t, streamBody), h))
				if err != nil {
					t.Fatal(err)
				}
				_, _ = collect(t, s)
				_ = s.Close()
			} else if _, err := a.Chat(ctxWithTimeout(t), withHeader(request(t, plainBody), h)); err != nil {
				t.Fatal(err)
			}
			reqs := f.Requests()
			got := reqs[len(reqs)-1]
			if got.Method != http.MethodPost || got.Path != "/v1/messages" || got.Status != http.StatusOK {
				t.Errorf("upstream saw %s %s and answered %d", got.Method, got.Path, got.Status)
			}
			accept := "application/json"
			if stream {
				accept = "text/event-stream"
			}
			for k, want := range map[string]string{
				"X-Api-Key":         providerKey,
				"Anthropic-Version": "2023-06-01",
				"Content-Type":      "application/json",
				"Accept":            accept,
				"Authorization":     "",
				"Cookie":            "",
				"X-Not-Allowed":     "",
			} {
				if v := got.Header.Get(k); v != want {
					t.Errorf("upstream header %s = %q, want %q", k, v, want)
				}
			}
			if v := got.Header.Values("Anthropic-Beta"); fmt.Sprint(v) != "[beta-one beta-two]" {
				t.Errorf("Anthropic-Beta = %v", v)
			}
			if ua := got.Header.Get("User-Agent"); !strings.HasPrefix(ua, "switchyard/") {
				t.Errorf("User-Agent = %q", ua)
			}
		})
	}
}

func withHeader(r *providers.Request, h http.Header) *providers.Request {
	r.Header = h
	return r
}

func TestWithoutAPIKey(t *testing.T) {
	f := fakeanthropic.New(fakeanthropic.Options{})
	t.Cleanup(f.Close)
	p := testProvider(f.BaseURL())
	p.APIKey = config.Secret{}
	a := newAdapter(t, p)
	if _, err := a.Chat(ctxWithTimeout(t), request(t, plainBody)); err != nil {
		t.Fatal(err)
	}
	if v := f.Requests()[0].Header.Get("X-Api-Key"); v != "" {
		t.Errorf("X-Api-Key = %q, want none", v)
	}
}

// TestStatusErrors maps every documented error status onto a
// providers.Kind, for both calls: client errors keep the Anthropic error
// re-shaped into an OpenAI envelope, credential, account and server errors
// keep no body.
func TestStatusErrors(t *testing.T) {
	tests := []struct {
		fixture    string
		status     int
		header     map[string]string
		kind       providers.Kind
		body       string // "" means no body is kept
		retryAfter time.Duration
	}{
		{fixture: "error_400_invalid_request_error.json", status: 400, kind: providers.KindClientError,
			body: `{"error":{"message":"messages.1.content.0.tool_use.input: Input should be a valid dictionary","type":"invalid_request_error","param":null,"code":null}}`},
		{fixture: "error_401_authentication_error.json", status: 401, kind: providers.KindAuth},
		{fixture: "error_402_billing_error.json", status: 402, kind: providers.KindAuth},
		{fixture: "error_403_permission_error.json", status: 403, kind: providers.KindAuth},
		{fixture: "error_404_not_found_error.json", status: 404, kind: providers.KindClientError,
			body: `{"error":{"message":"model: claude-nonexistent","type":"invalid_request_error","param":null,"code":"not_found_error"}}`},
		{fixture: "error_413_request_too_large.json", status: 413, kind: providers.KindClientError,
			body: `{"error":{"message":"Request exceeds the maximum allowed number of bytes.","type":"invalid_request_error","param":null,"code":"request_too_large"}}`},
		{fixture: "error_429_rate_limit_error.json", status: 429, header: map[string]string{"Retry-After": "7"},
			kind: providers.KindRateLimited, retryAfter: 7 * time.Second,
			body: `{"error":{"message":"This request would exceed the rate limit for your organization.","type":"requests","param":null,"code":"rate_limit_exceeded"}}`},
		{fixture: "error_500_api_error.json", status: 500, kind: providers.KindServerError},
		{fixture: "error_504_timeout_error.json", status: 504, kind: providers.KindServerError},
		{fixture: "error_529_overloaded_error.json", status: 529, kind: providers.KindServerError},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tt.fixture, stream), func(t *testing.T) {
				f := newFake(t)
				b := fakeanthropic.Behavior{Status: tt.status, Body: fixture(t, tt.fixture), Header: http.Header{}}
				for k, v := range tt.header {
					b.Header.Set(k, v)
				}
				f.Script(b)
				a := newAdapter(t, testProvider(f.BaseURL()))
				err := doCall(t, a, stream)
				pe := wantKind(t, err, tt.kind)
				if pe.Status != tt.status || pe.UpstreamRequestID == "" || pe.RetryAfter != tt.retryAfter {
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
				typ := strings.TrimSuffix(tt.fixture[len("error_123_"):], ".json")
				if tt.kind != providers.KindAuth && !strings.Contains(pe.Message, "("+typ+")") {
					t.Errorf("message = %q, want it to name %s", pe.Message, typ)
				}
			})
		}
	}
}

// doCall makes one Chat or ChatStream call and returns its error; a stream
// that opens is read to its end.
func doCall(t *testing.T, a *Adapter, stream bool) error {
	t.Helper()
	if !stream {
		_, err := a.Chat(ctxWithTimeout(t), request(t, plainBody))
		return err
	}
	s, err := a.ChatStream(ctxWithTimeout(t), request(t, streamBody))
	if err != nil {
		if s != nil {
			t.Fatal("ChatStream returned a stream with an error")
		}
		return err
	}
	defer func() { _ = s.Close() }()
	_, err = collect(t, s)
	return err
}

func TestStatusErrorBodies(t *testing.T) {
	tests := []struct {
		name   string
		status int
		header map[string]string
		body   string
		kind   providers.Kind
	}{
		{name: "400 that is not JSON", status: 400, body: "nope", kind: providers.KindClientError},
		{name: "400 with an odd error type", status: 400, body: `{"type":"error","error":{"type":"Bad Type!","message":"x"}}`, kind: providers.KindClientError},
		{name: "422", status: 422, body: `{"type":"error","error":{"type":"invalid_request_error","message":"x"}}`, kind: providers.KindClientError},
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
			a := newAdapter(t, testProvider(u.URL))
			pe := wantKind(t, doCall(t, a, false), tt.kind)
			keep := tt.status == 422
			if keep != (pe.Body != nil) {
				t.Errorf("body kept = %v (%s), want %v", pe.Body != nil, pe.Body, keep)
			}
			if strings.Contains(pe.Message, "Bad Type") || strings.Contains(pe.Message, "nope") {
				t.Errorf("message quotes the body: %q", pe.Message)
			}
		})
	}
}

// TestKeyNeverEchoed checks that the provider key never leaves the adapter
// in an error, whatever the upstream echoes back: error bodies of every
// class, the request id header, and stream error events.
func TestKeyNeverEchoed(t *testing.T) {
	spellings := map[string]string{"clear": providerKey}
	for name, k := range escapedKeys {
		spellings[name] = k
	}
	for spelling, key := range spellings {
		echoes := map[string]string{
			"message": `{"type":"error","error":{"type":"invalid_request_error","message":"bad key ` + key + `"}}`,
			"type":    `{"type":"error","error":{"type":"` + key + `","message":"bad key"}}`,
		}
		for field, echo := range echoes {
			for _, status := range []int{400, 401, 403, 404, 429, 500, 529} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%d/stream=%v", spelling, field, status, stream), func(t *testing.T) {
						u := newUpstream(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
							w.Header().Set("Request-Id", "req-"+providerKey)
							w.WriteHeader(status)
							_, _ = io.WriteString(w, echo)
						})
						a := newAdapter(t, testProvider(u.URL))
						err := doCall(t, a, stream)
						// Whatever the kind, wantKind fails if the key leaks.
						pe := wantKind(t, err, providers.Kind(providers.Outcome(err)))
						if pe.Body != nil || pe.UpstreamRequestID != "" {
							t.Errorf("error kept the echoing body or request id: %+v", pe)
						}
						if v := u.lastHeader().Get("X-Api-Key"); v != providerKey {
							t.Error("the upstream did not get the key")
						}
					})
				}
			}
		}
		t.Run(spelling+"/stream error event", func(t *testing.T) {
			for _, data := range []string{
				`{"type":"error","error":{"type":"api_error","message":"` + key + `"}}`,
				`{"type":"error","error":{"type":"` + key + `","message":"m"}}`,
			} {
				u := newUpstream(t, sseHandler([]string{evStart, "event: error\ndata: " + data + "\n\n"}, nil))
				a := newAdapter(t, testProvider(u.URL))
				_ = wantKind(t, doCall(t, a, true), providers.KindServerError)
			}
		})
	}
	t.Run("the fake's 401 for a wrong key", func(t *testing.T) {
		f := fakeanthropic.New(fakeanthropic.Options{APIKey: "sk-ant-other"})
		t.Cleanup(f.Close)
		a := newAdapter(t, testProvider(f.BaseURL()))
		pe := wantKind(t, doCall(t, a, false), providers.KindAuth)
		if pe.Status != 401 || pe.Body != nil {
			t.Errorf("error = %+v", pe)
		}
	})
}

func TestClientErrorIsNotSent(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, testProvider(f.BaseURL()))
	body := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{"}}]}]}`
	for _, stream := range []bool{false, true} {
		var err error
		if stream {
			_, err = a.ChatStream(ctxWithTimeout(t), request(t, body))
		} else {
			_, err = a.Chat(ctxWithTimeout(t), request(t, body))
		}
		_ = wantKind(t, err, providers.KindClientError)
	}
	if n := len(f.Requests()); n != 0 {
		t.Errorf("upstream received %d requests, want none", n)
	}
}

// TestFakeAcceptsTranslations sends representative translated requests to
// the fake, which validates them as the Messages API does (no unknown
// fields, temperature at most 1, well-formed blocks).
func TestFakeAcceptsTranslations(t *testing.T) {
	bodies := []string{
		`{"model":"m","temperature":0.5,"top_p":0.9,"stop":["x"],"user":"u","n":1,"seed":3,"logprobs":false,"presence_penalty":0,
			"messages":[{"role":"system","content":"s"},{"role":"user","content":[{"type":"text","text":"hi"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
		`{"model":"m","tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"function","function":{"name":"f"}},
			"messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"r"}]}`,
		`{"model":"m","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hi"}]}`,
	}
	f := newFake(t)
	a := newAdapter(t, testProvider(f.BaseURL()))
	for i, b := range bodies {
		if _, err := a.Chat(ctxWithTimeout(t), request(t, b, schema.CapVision, schema.CapJSONMode)); err != nil {
			t.Errorf("request %d: %v", i, err)
		}
	}
	f.Reset()
	_, err := a.Chat(ctxWithTimeout(t), request(t, `{"model":"m","temperature":1.5,"messages":[{"role":"user","content":"hi"}]}`))
	_ = wantKind(t, err, providers.KindClientError)
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{name: "none", header: http.Header{}, want: 0},
		{name: "seconds", header: http.Header{"Retry-After": {"20"}}, want: 20 * time.Second},
		{name: "fractional", header: http.Header{"Retry-After": {"0.5"}}, want: 500 * time.Millisecond},
		{name: "date", header: http.Header{"Retry-After": {now.Add(90 * time.Second).Format(http.TimeFormat)}}, want: 90 * time.Second},
		{name: "past date", header: http.Header{"Retry-After": {now.Add(-time.Hour).Format(http.TimeFormat)}}, want: 0},
		{name: "garbage", header: http.Header{"Retry-After": {"soon"}}, want: 0},
		{name: "negative", header: http.Header{"Retry-After": {"-3"}}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryAfter(tt.header, now); got != tt.want {
				t.Errorf("retryAfter = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestChatTimeouts(t *testing.T) {
	const short = 100 * time.Millisecond
	tests := []struct {
		name    string
		handler func(u *upstream, w http.ResponseWriter, r *http.Request)
		set     func(p *config.ProviderTimeouts)
		message string
	}{
		{
			name:    "first_token bounds the response headers",
			handler: func(u *upstream, _ http.ResponseWriter, r *http.Request) { u.hang(r) },
			set:     func(p *config.ProviderTimeouts) { p.FirstToken = config.Duration(short) },
			message: "first_token timeout",
		},
		{
			name: "total bounds the body",
			handler: func(u *upstream, w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"id":`)
				w.(http.Flusher).Flush()
				u.hang(r)
			},
			set:     func(p *config.ProviderTimeouts) { p.Total = config.Duration(short) },
			message: "total timeout",
		},
		{
			name:    "total bounds the headers too",
			handler: func(u *upstream, _ http.ResponseWriter, r *http.Request) { u.hang(r) },
			set:     func(p *config.ProviderTimeouts) { p.Total = config.Duration(short) },
			message: "total timeout",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newUpstream(t, tt.handler)
			p := testProvider(u.URL)
			tt.set(&p.Timeouts)
			a := newAdapter(t, p)
			pe := wantKind(t, doCall(t, a, false), providers.KindTimeout)
			if !strings.Contains(pe.Message, tt.message) {
				t.Errorf("message = %q, want it to mention %q", pe.Message, tt.message)
			}
			waitCanceled(t, u)
		})
	}
}

func TestCanceledByCaller(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			started := make(chan struct{})
			u := newUpstream(t, func(u *upstream, _ http.ResponseWriter, r *http.Request) {
				close(started)
				u.hang(r)
			})
			a := newAdapter(t, testProvider(u.URL))
			ctx, cancel := context.WithCancel(context.Background())
			errc := make(chan error, 1)
			go func() {
				var err error
				if stream {
					_, err = a.ChatStream(ctx, request(t, streamBody))
				} else {
					_, err = a.Chat(ctx, request(t, plainBody))
				}
				errc <- err
			}()
			<-started
			cancel()
			select {
			case err := <-errc:
				_ = wantKind(t, err, providers.KindCanceled)
			case <-time.After(testTimeout):
				t.Fatal("the call did not return after cancel")
			}
			waitCanceled(t, u)
		})
	}
}

func TestConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	a := newAdapter(t, testProvider("http://"+addr))
	for _, stream := range []bool{false, true} {
		pe := wantKind(t, doCall(t, a, stream), providers.KindTransport)
		if pe.Status != 0 || pe.Err == nil {
			t.Errorf("stream=%v: transport error = %+v", stream, pe)
		}
	}
}

func TestChatOversizedBody(t *testing.T) {
	u := newUpstream(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
		chunk := strings.Repeat("x", 1<<20)
		for range maxResponseBytes>>20 + 1 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	})
	a := newAdapter(t, testProvider(u.URL))
	_ = wantKind(t, doCall(t, a, false), providers.KindServerError)
}

// TestUpstreamRequestID checks that the upstream's request-id is kept only
// when it is short printable ASCII without the provider key.
func TestUpstreamRequestID(t *testing.T) {
	tests := []struct {
		name string
		rid  string
		want string
	}{
		{name: "typical", rid: "req_018EeWyXxfu5pfWkrYcMdjWG", want: "req_018EeWyXxfu5pfWkrYcMdjWG"},
		{name: "at the length limit", rid: strings.Repeat("r", 128), want: strings.Repeat("r", 128)},
		{name: "too long", rid: strings.Repeat("r", 129)},
		{name: "not ASCII", rid: "req-é"},
		{name: "contains the key", rid: "req-" + providerKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newUpstream(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
				w.Header()["Request-Id"] = []string{tt.rid}
				_, _ = w.Write(fixture(t, "message_text.json"))
			})
			a := newAdapter(t, testProvider(u.URL))
			resp, err := a.Chat(ctxWithTimeout(t), request(t, plainBody))
			if err != nil {
				t.Fatal(err)
			}
			if resp.UpstreamRequestID != tt.want {
				t.Errorf("upstream request id = %q, want %q", resp.UpstreamRequestID, tt.want)
			}
			if !strings.Contains(u.lastBody(), `"model":"claude-test"`) {
				t.Errorf("upstream body %s", u.lastBody())
			}
		})
	}
}

// TestCallsFromFake checks the fake's scripted faults through the adapter:
// a dropped connection, a hang, and a scripted mid-stream error event.
func TestCallsFromFake(t *testing.T) {
	f := newFake(t)
	p := testProvider(f.BaseURL())
	p.Timeouts.FirstToken = config.Duration(200 * time.Millisecond)
	a := newAdapter(t, p)

	f.Script(fakeanthropic.Behavior{Fault: fakeanthropic.FaultDrop})
	_ = wantKind(t, doCall(t, a, false), providers.KindTransport)

	f.Script(fakeanthropic.Behavior{Fault: fakeanthropic.FaultHang})
	_ = wantKind(t, doCall(t, a, false), providers.KindTimeout)

	f.Script(fakeanthropic.Behavior{ErrorEvent: "overloaded_error", ErrorAfterEvents: 4})
	pe := wantKind(t, doCall(t, a, true), providers.KindServerError)
	if !strings.Contains(pe.Message, "overloaded_error") {
		t.Errorf("message = %q", pe.Message)
	}

	f.Script(fakeanthropic.Overloaded())
	pe = wantKind(t, doCall(t, a, true), providers.KindServerError)
	if pe.Status != 529 {
		t.Errorf("status = %d, want 529", pe.Status)
	}

	f.Script(fakeanthropic.Behavior{StopReason: "refusal"})
	resp, err := a.Chat(ctxWithTimeout(t), request(t, plainBody))
	if err != nil || !strings.Contains(string(resp.Body), `"finish_reason":"content_filter"`) {
		t.Errorf("refusal: %v, %s", err, resp.Body)
	}
}
