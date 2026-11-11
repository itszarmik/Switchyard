package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// testTimeout bounds every wait in these tests. It is far longer than any
// operation should take, so it only fires when something is broken.
const testTimeout = 10 * time.Second

const providerKey = "sk-provider-secret-0123456789abcdef"

// upstream is a fake OpenAI-wire server that records what it received.
type upstream struct {
	*httptest.Server
	mu       sync.Mutex
	bodies   []string
	headers  []http.Header
	canceled chan struct{} // closed when a request's context ends early
	release  chan struct{} // closed at cleanup to unblock waiting handlers
}

func newUpstream(t *testing.T, h func(u *upstream, w http.ResponseWriter, r *http.Request)) *upstream {
	t.Helper()
	u := &upstream{canceled: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, string(body))
		u.headers = append(u.headers, r.Header.Clone())
		u.mu.Unlock()
		context.AfterFunc(r.Context(), func() { once.Do(func() { close(u.canceled) }) })
		h(u, w, r)
	}))
	t.Cleanup(u.Close)
	t.Cleanup(func() { close(u.release) })
	return u
}

// hang blocks until the request is canceled or the test ends.
func (u *upstream) hang(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-u.release:
	}
}

func (u *upstream) lastBody() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.bodies[len(u.bodies)-1]
}

func (u *upstream) lastHeader() http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.headers[len(u.headers)-1]
}

func testProvider(baseURL string) config.Provider {
	return config.Provider{
		ID:             "fake",
		Type:           "openai",
		BaseURL:        baseURL,
		APIKey:         config.NewSecret(providerKey),
		ForwardHeaders: []string{"x-allowed", "OpenAI-Organization", "Accept-Encoding"},
		Timeouts: config.ProviderTimeouts{
			Connect:    config.Duration(time.Second),
			FirstToken: config.Duration(testTimeout),
			Idle:       config.Duration(testTimeout),
			Total:      config.Duration(testTimeout),
		},
	}
}

func newAdapter(t *testing.T, p config.Provider) *Adapter {
	t.Helper()
	a, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.CloseIdleConnections)
	return a
}

func chatRequest(t *testing.T, body string, header http.Header) *providers.Request {
	t.Helper()
	req, err := schema.DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeChatRequest: %v", err)
	}
	if header == nil {
		header = http.Header{}
	}
	return &providers.Request{Chat: req, UpstreamModel: "upstream-model", Header: header}
}

func ctxWithTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

func wantKind(t *testing.T, err error, kind providers.Kind) *providers.Error {
	t.Helper()
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v (%T), want a *providers.Error", err, err)
	}
	if pe.Kind != kind {
		t.Fatalf("kind = %s (%v), want %s", pe.Kind, pe, kind)
	}
	if pe.Provider != "fake" {
		t.Errorf("provider = %q, want fake", pe.Provider)
	}
	if strings.Contains(pe.Error(), providerKey) || strings.Contains(string(pe.Body), providerKey) {
		t.Errorf("error leaks the provider key: %v", pe)
	}
	return pe
}

func TestNewTransport(t *testing.T) {
	p := testProvider("http://upstream.test/v1/")
	p.Timeouts.Connect = config.Duration(1234 * time.Millisecond)
	a := newAdapter(t, p)
	if a.endpoint != "http://upstream.test/v1/chat/completions" {
		t.Errorf("endpoint = %q", a.endpoint)
	}
	tr, ok := a.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", a.client.Transport)
	}
	if tr.TLSHandshakeTimeout != 1234*time.Millisecond {
		t.Errorf("TLSHandshakeTimeout = %s", tr.TLSHandshakeTimeout)
	}
	if a.dialer.Timeout != 1234*time.Millisecond || a.dialer.KeepAlive <= 0 {
		t.Errorf("dialer timeout = %s, keep-alive = %s", a.dialer.Timeout, a.dialer.KeepAlive)
	}
	if tr.DialContext == nil || tr.MaxIdleConnsPerHost < 100 || tr.IdleConnTimeout <= 0 || tr.DisableKeepAlives {
		t.Errorf("transport not pooled: %+v", tr)
	}
	if a.client.Timeout != 0 {
		t.Error("client.Timeout must be 0; the adapter's own timeouts apply")
	}
	if b := newAdapter(t, p); b.client == a.client || b.client.Transport == a.client.Transport {
		t.Error("adapters share an HTTP client or transport")
	}
	if want := []string{"X-Allowed", "Openai-Organization"}; fmt.Sprint(a.forward) != fmt.Sprint(want) {
		t.Errorf("forward = %v, want %v (Accept-Encoding is never forwarded)", a.forward, want)
	}
}

func TestChatConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	a := newAdapter(t, testProvider("http://"+addr+"/v1"))
	for _, stream := range []bool{false, true} {
		req := chatRequest(t, fmt.Sprintf(`{"model":"m","messages":[{}],"stream":%v}`, stream), nil)
		if stream {
			_, err = a.ChatStream(ctxWithTimeout(t), req)
		} else {
			_, err = a.Chat(ctxWithTimeout(t), req)
		}
		pe := wantKind(t, err, providers.KindTransport)
		if pe.Status != 0 || pe.Err == nil {
			t.Errorf("stream=%v: transport error = %+v", stream, pe)
		}
	}
}

func TestChatRequest(t *testing.T) {
	u := newUpstream(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-request-id", "req-1")
		_, _ = io.WriteString(w, `{"id":"c","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`)
	})
	a := newAdapter(t, testProvider(u.URL+"/v1"))
	h := http.Header{}
	h.Set("Authorization", "Bearer sy_client_key_must_not_be_forwarded")
	h.Set("Cookie", "session=1")
	h.Set("X-Allowed", "yes")
	h.Add("X-Allowed", "twice")
	h.Set("X-Not-Allowed", "no")
	h.Set("Accept-Encoding", "br")
	h.Set("User-Agent", "client/1")
	req := chatRequest(t, `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}],"x_unknown":{"a":[1]}}`, h)

	resp, err := a.Chat(ctxWithTimeout(t), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Status != http.StatusOK || resp.UpstreamRequestID != "req-1" || resp.Usage == nil || resp.Usage.TotalTokens != 7 {
		t.Errorf("response = %+v", resp)
	}
	if want := `{"model":"upstream-model","messages":[{"role":"user","content":"hi"}],"x_unknown":{"a":[1]}}`; u.lastBody() != want {
		t.Errorf("upstream body = %s, want %s", u.lastBody(), want)
	}
	got := u.lastHeader()
	checks := map[string]string{
		"Authorization":   "Bearer " + providerKey,
		"Content-Type":    "application/json",
		"Accept":          "application/json",
		"Cookie":          "",
		"X-Not-Allowed":   "",
		"Accept-Encoding": "gzip", // the transport's own, not the client's
	}
	for k, want := range checks {
		if v := got.Get(k); v != want {
			t.Errorf("upstream header %s = %q, want %q", k, v, want)
		}
	}
	if v := got.Values("X-Allowed"); fmt.Sprint(v) != "[yes twice]" {
		t.Errorf("X-Allowed = %v", v)
	}
	if ua := got.Get("User-Agent"); !strings.HasPrefix(ua, "switchyard/") {
		t.Errorf("User-Agent = %q", ua)
	}
}

func TestChatWithoutAPIKey(t *testing.T) {
	u := newUpstream(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	p := testProvider(u.URL)
	p.APIKey = config.Secret{}
	a := newAdapter(t, p)
	h := http.Header{"Authorization": {"Bearer sy_client"}}
	if _, err := a.Chat(ctxWithTimeout(t), chatRequest(t, `{"model":"m","messages":[{}]}`, h)); err != nil {
		t.Fatal(err)
	}
	if v := u.lastHeader().Get("Authorization"); v != "" {
		t.Errorf("Authorization = %q, want none", v)
	}
}

func TestChatStatusErrors(t *testing.T) {
	envelope := `{"error":{"message":"bad","type":"invalid_request_error","param":"x","code":"c"}}`
	tests := []struct {
		name       string
		status     int
		body       string
		header     map[string]string
		kind       providers.Kind
		keepBody   bool
		retryAfter time.Duration
	}{
		{name: "400 envelope", status: 400, body: envelope, kind: providers.KindClientError, keepBody: true},
		{name: "400 text", status: 400, body: "nope", kind: providers.KindClientError},
		{name: "400 error string", status: 400, body: `{"error":"bad"}`, kind: providers.KindClientError},
		{name: "400 echoing the key", status: 400, body: `{"error":{"message":"bad key ` + providerKey + `","type":"invalid_request_error"}}`, kind: providers.KindClientError},
		{name: "429 echoing the key", status: 429, body: `{"error":{"message":"limit for ` + providerKey + `"}}`, kind: providers.KindRateLimited},
		{name: "404", status: 404, body: envelope, kind: providers.KindClientError, keepBody: true},
		{name: "422", status: 422, body: envelope, kind: providers.KindClientError, keepBody: true},
		{name: "429 seconds", status: 429, body: envelope, header: map[string]string{"Retry-After": "7"}, kind: providers.KindRateLimited, keepBody: true, retryAfter: 7 * time.Second},
		{name: "429 ms", status: 429, body: envelope, header: map[string]string{"Retry-After-Ms": "1500"}, kind: providers.KindRateLimited, keepBody: true, retryAfter: 1500 * time.Millisecond},
		{name: "401 never keeps the body", status: 401, body: `{"error":{"message":"Incorrect API key provided: sk-provi***cdef"}}`, kind: providers.KindAuth},
		{name: "403", status: 403, body: envelope, kind: providers.KindAuth},
		{name: "500", status: 500, body: envelope, kind: providers.KindServerError},
		{name: "503", status: 503, body: "busy", kind: providers.KindServerError},
		{name: "redirect is not followed", status: 307, header: map[string]string{"Location": "/elsewhere"}, kind: providers.KindServerError},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tt.name, stream), func(t *testing.T) {
				u := newUpstream(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("x-request-id", "req-err")
					for k, v := range tt.header {
						w.Header().Set(k, v)
					}
					w.WriteHeader(tt.status)
					_, _ = io.WriteString(w, tt.body)
				})
				a := newAdapter(t, testProvider(u.URL))
				req := chatRequest(t, fmt.Sprintf(`{"model":"m","messages":[{}],"stream":%v}`, stream), nil)
				var err error
				if stream {
					var s providers.Stream
					s, err = a.ChatStream(ctxWithTimeout(t), req)
					if s != nil {
						_ = s.Close()
						t.Fatal("ChatStream returned a stream for an error status")
					}
				} else {
					_, err = a.Chat(ctxWithTimeout(t), req)
				}
				pe := wantKind(t, err, tt.kind)
				if pe.Status != tt.status || pe.UpstreamRequestID != "req-err" || pe.RetryAfter != tt.retryAfter {
					t.Errorf("error = %+v", pe)
				}
				if tt.keepBody != (pe.Body != nil) {
					t.Errorf("body kept = %v, want %v", pe.Body != nil, tt.keepBody)
				}
				if pe.Body != nil && string(pe.Body) != tt.body {
					t.Errorf("body = %s, want %s", pe.Body, tt.body)
				}
				if strings.Contains(pe.Message, tt.body) && tt.body != "" {
					t.Errorf("message quotes the upstream body: %q", pe.Message)
				}
			})
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
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
		{name: "ms fallback", header: http.Header{"Retry-After-Ms": {"250"}}, want: 250 * time.Millisecond},
		{name: "seconds win over ms", header: http.Header{"Retry-After": {"2"}, "Retry-After-Ms": {"250"}}, want: 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryAfter(tt.header, now); got != tt.want {
				t.Errorf("retryAfter = %s, want %s", got, tt.want)
			}
		})
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
	_, err := a.Chat(ctxWithTimeout(t), chatRequest(t, `{"model":"m","messages":[{}]}`, nil))
	_ = wantKind(t, err, providers.KindServerError)
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
			_, err := a.Chat(ctxWithTimeout(t), chatRequest(t, `{"model":"m","messages":[{}]}`, nil))
			pe := wantKind(t, err, providers.KindTimeout)
			if !strings.Contains(pe.Message, tt.message) {
				t.Errorf("message = %q, want it to mention %q", pe.Message, tt.message)
			}
			waitCanceled(t, u)
		})
	}
}

// waitCanceled waits until the upstream saw its request canceled.
func waitCanceled(t *testing.T, u *upstream) {
	t.Helper()
	select {
	case <-u.canceled:
	case <-time.After(testTimeout):
		t.Fatal("the upstream request was not canceled")
	}
}

func TestChatCanceledByCaller(t *testing.T) {
	started := make(chan struct{})
	u := newUpstream(t, func(u *upstream, _ http.ResponseWriter, r *http.Request) {
		close(started)
		u.hang(r)
	})
	a := newAdapter(t, testProvider(u.URL))
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := a.Chat(ctx, chatRequest(t, `{"model":"m","messages":[{}]}`, nil))
		errc <- err
	}()
	<-started
	cancel()
	select {
	case err := <-errc:
		_ = wantKind(t, err, providers.KindCanceled)
	case <-time.After(testTimeout):
		t.Fatal("Chat did not return after cancel")
	}
	waitCanceled(t, u)
}

// TestUpstreamRequestID checks that the upstream's x-request-id, which is
// returned to the client and logged, is kept only when it is short
// printable ASCII without the provider key.
func TestUpstreamRequestID(t *testing.T) {
	tests := []struct {
		name string
		rid  string
		want string
	}{
		{name: "typical", rid: "req_0123abcd", want: "req_0123abcd"},
		{name: "at the length limit", rid: strings.Repeat("r", 128), want: strings.Repeat("r", 128)},
		{name: "too long", rid: strings.Repeat("r", 129)},
		{name: "not ASCII", rid: "req-é"},
		{name: "tab", rid: "req\tx"},
		{name: "contains the key", rid: "req-" + providerKey},
	}
	for _, tt := range tests {
		for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
			t.Run(fmt.Sprintf("%s/%d", tt.name, status), func(t *testing.T) {
				u := newUpstream(t, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
					w.Header()["X-Request-Id"] = []string{tt.rid}
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"message":"bad"}}`)
				})
				a := newAdapter(t, testProvider(u.URL))
				resp, err := a.Chat(ctxWithTimeout(t), chatRequest(t, `{"model":"m","messages":[{}]}`, nil))
				var got string
				if status == http.StatusOK {
					if err != nil {
						t.Fatal(err)
					}
					got = resp.UpstreamRequestID
				} else {
					got = wantKind(t, err, providers.KindClientError).UpstreamRequestID
				}
				if got != tt.want {
					t.Errorf("upstream request id = %q, want %q", got, tt.want)
				}
			})
		}
	}
}
