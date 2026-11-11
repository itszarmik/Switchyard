package openai

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"switchyard/internal/config"
	"switchyard/internal/providers"
)

const streamBody = `{"model":"m","messages":[{}],"stream":true}`

// sseHandler writes events (each a complete "data: …\n\n" block or other
// raw SSE text), flushing after each, then runs after.
func sseHandler(events []string, after func(u *upstream, w http.ResponseWriter, r *http.Request)) func(*upstream, http.ResponseWriter, *http.Request) {
	return func(u *upstream, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-request-id", "req-s")
		w.WriteHeader(http.StatusOK)
		for _, ev := range events {
			_, _ = io.WriteString(w, ev)
			w.(http.Flusher).Flush()
		}
		if after != nil {
			after(u, w, r)
		}
	}
}

func collect(t *testing.T, s providers.Stream) ([]string, error) {
	t.Helper()
	var out []string
	for {
		c, err := s.Next()
		if err != nil {
			return out, err
		}
		out = append(out, string(c.Raw))
	}
}

func TestStream(t *testing.T) {
	events := []string{
		": keep-alive comment\n\n",
		"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}],\"usage\":null}\n\n",
		"event: ping\ndata: {}\n\n", // not a message event: skipped
		"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\r\n\r\n",
		"data: not json is forwarded as is\n\n",
		"data: {\"id\":\"c\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n",
		"data: [DONE]\n\n",
	}
	u := newUpstream(t, sseHandler(events, nil))
	a := newAdapter(t, testProvider(u.URL))
	s, err := a.ChatStream(ctxWithTimeout(t), chatRequest(t, streamBody, nil))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer func() { _ = s.Close() }()
	if s.UpstreamRequestID() != "req-s" || s.TTFT() <= 0 {
		t.Errorf("rid = %q, ttft = %s", s.UpstreamRequestID(), s.TTFT())
	}
	got, err := collect(t, s)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("stream ended with %v, want io.EOF", err)
	}
	want := []string{
		`{"id":"c","choices":[{"index":0,"delta":{"role":"assistant"}}],"usage":null}`,
		`{"id":"c","choices":[{"index":0,"delta":{"content":"hi"}}],"usage":null}`,
		`not json is forwarded as is`,
		`{"id":"c","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("chunks:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if u := s.Usage(); u == nil || u.PromptTokens != 5 || u.CompletionTokens != 2 {
		t.Errorf("usage = %+v", u)
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next after EOF = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if body := u.lastBody(); !strings.Contains(body, `"stream_options":{"include_usage":true}`) ||
		u.lastHeader().Get("Accept") != "text/event-stream" {
		t.Errorf("upstream request: %s, Accept %q", body, u.lastHeader().Get("Accept"))
	}
}

func TestStreamDoneFirst(t *testing.T) {
	u := newUpstream(t, sseHandler([]string{"data: [DONE]\n\n"}, nil))
	a := newAdapter(t, testProvider(u.URL))
	s, err := a.ChatStream(ctxWithTimeout(t), chatRequest(t, streamBody, nil))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next = %v, want io.EOF", err)
	}
}

func TestStreamFailures(t *testing.T) {
	const short = 100 * time.Millisecond
	chunk := "data: {\"choices\":[{\"index\":0}]}\n\n"
	// pings sends events that are not forwarded, every 10ms, until the
	// request ends.
	pings := func(_ *upstream, w http.ResponseWriter, r *http.Request) {
		for {
			if _, err := io.WriteString(w, "event: ping\ndata: {}\n\n"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-time.After(10 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
	}
	tests := []struct {
		name string
		h    func(*upstream, http.ResponseWriter, *http.Request)
		set  func(*config.ProviderTimeouts)
		// before is the number of chunks read before the failure; -1 means
		// ChatStream itself fails.
		before  int
		kind    providers.Kind
		message string
	}{
		{
			name:   "first_token: headers but no event",
			h:      sseHandler([]string{": waiting\n\n"}, func(u *upstream, _ http.ResponseWriter, r *http.Request) { u.hang(r) }),
			set:    func(p *config.ProviderTimeouts) { p.FirstToken = config.Duration(short) },
			before: -1, kind: providers.KindTimeout, message: "first_token timeout",
		},
		{
			name:   "first_token: no headers",
			h:      func(u *upstream, _ http.ResponseWriter, r *http.Request) { u.hang(r) },
			set:    func(p *config.ProviderTimeouts) { p.FirstToken = config.Duration(short) },
			before: -1, kind: providers.KindTimeout, message: "first_token timeout",
		},
		{
			name:   "first_token: only events that are not forwarded",
			h:      sseHandler(nil, pings),
			set:    func(p *config.ProviderTimeouts) { p.FirstToken = config.Duration(short) },
			before: -1, kind: providers.KindTimeout, message: "first_token timeout",
		},
		{
			name:   "idle: only events that are not forwarded after the first",
			h:      sseHandler([]string{chunk}, pings),
			set:    func(p *config.ProviderTimeouts) { p.Idle = config.Duration(short) },
			before: 1, kind: providers.KindTimeout, message: "idle timeout",
		},
		{
			name:   "idle after the first event",
			h:      sseHandler([]string{chunk, chunk}, func(u *upstream, _ http.ResponseWriter, r *http.Request) { u.hang(r) }),
			set:    func(p *config.ProviderTimeouts) { p.Idle = config.Duration(short) },
			before: 2, kind: providers.KindTimeout, message: "idle timeout",
		},
		{
			name: "total over a steady stream",
			h: sseHandler(nil, func(u *upstream, w http.ResponseWriter, r *http.Request) {
				for {
					if _, err := io.WriteString(w, chunk); err != nil {
						return
					}
					w.(http.Flusher).Flush()
					select {
					case <-time.After(10 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
				}
			}),
			set: func(p *config.ProviderTimeouts) {
				p.Total = config.Duration(300 * time.Millisecond)
				p.Idle = config.Duration(time.Second)
			},
			before: -2, kind: providers.KindTimeout, message: "total timeout",
		},
		{
			name:   "end without [DONE]",
			h:      sseHandler([]string{chunk}, nil),
			before: 1, kind: providers.KindServerError, message: "without [DONE]",
		},
		{
			name:   "error event first",
			h:      sseHandler([]string{"data: {\"error\":{\"message\":\"overloaded\"}}\n\n"}, nil),
			before: -1, kind: providers.KindServerError, message: "error mid-stream",
		},
		{
			name:   "error event later",
			h:      sseHandler([]string{chunk, "data: {\"error\":{\"message\":\"overloaded\"}}\n\n"}, nil),
			before: 1, kind: providers.KindServerError, message: "error mid-stream",
		},
		{
			name:   "event of type error",
			h:      sseHandler([]string{chunk, "event: error\ndata: {}\n\n"}, nil),
			before: 1, kind: providers.KindServerError, message: "error event",
		},
		{
			name: "connection dropped",
			h: sseHandler([]string{chunk}, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
				conn, _, err := http.NewResponseController(w).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}),
			before: 1, kind: providers.KindTransport, message: "connection",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := newUpstream(t, tt.h)
			p := testProvider(u.URL)
			if tt.set != nil {
				tt.set(&p.Timeouts)
			}
			a := newAdapter(t, p)
			s, err := a.ChatStream(ctxWithTimeout(t), chatRequest(t, streamBody, nil))
			if tt.before == -1 {
				if s != nil {
					t.Fatal("ChatStream returned a stream")
				}
			} else {
				if err != nil {
					t.Fatalf("ChatStream: %v", err)
				}
				var got []string
				got, err = collect(t, s)
				if tt.before >= 0 && len(got) != tt.before {
					t.Errorf("read %d chunks before the failure, want %d", len(got), tt.before)
				}
				if _, again := s.Next(); !errors.Is(again, err) {
					t.Errorf("error is not sticky: %v then %v", err, again)
				}
				_ = s.Close()
			}
			pe := wantKind(t, err, tt.kind)
			if !strings.Contains(pe.Message, tt.message) {
				t.Errorf("message = %q, want it to mention %q", pe.Message, tt.message)
			}
		})
	}
}

func TestStreamCloseCancelsUpstream(t *testing.T) {
	u := newUpstream(t, sseHandler([]string{"data: {\"choices\":[]}\n\n"}, func(u *upstream, _ http.ResponseWriter, r *http.Request) {
		u.hang(r)
	}))
	a := newAdapter(t, testProvider(u.URL))
	s, err := a.ChatStream(ctxWithTimeout(t), chatRequest(t, streamBody, nil))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if _, err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	_ = s.Close()
	waitCanceled(t, u)
}

// TestStreamReusesConnection checks that a stream read to its [DONE] gives
// its connection back to the pool, including when the upstream finishes the
// response a moment after [DONE], and that an upstream that keeps the body
// open after [DONE] only delays Close by the drain bound.
func TestStreamReusesConnection(t *testing.T) {
	tests := []struct {
		name      string
		after     time.Duration // pause between [DONE] and the end of the body
		streams   int
		wantConns int64 // at most
	}{
		{name: "body ends with [DONE]", streams: 20, wantConns: 1},
		{name: "body ends 2ms after [DONE]", after: 2 * time.Millisecond, streams: 20, wantConns: 1},
		{name: "body stays open after [DONE]", after: 5 * drainTimeout, streams: 2, wantConns: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var conns atomic.Int64
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				for _, ev := range []string{
					"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n",
					"data: {\"id\":\"c\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n",
					"data: [DONE]\n\n",
				} {
					_, _ = io.WriteString(w, ev)
					w.(http.Flusher).Flush()
				}
				if tt.after > 0 {
					select {
					case <-time.After(tt.after):
					case <-r.Context().Done():
					}
				}
			}))
			srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
				if s == http.StateNew {
					conns.Add(1)
				}
			}
			srv.Start()
			defer srv.Close()
			a := newAdapter(t, testProvider(srv.URL+"/v1"))

			for i := range tt.streams {
				s, err := a.ChatStream(ctxWithTimeout(t), chatRequest(t, streamBody, nil))
				if err != nil {
					t.Fatalf("stream %d: %v", i, err)
				}
				if _, err := collect(t, s); !errors.Is(err, io.EOF) {
					t.Fatalf("stream %d ended with %v, want io.EOF", i, err)
				}
				start := time.Now()
				_ = s.Close()
				if d := time.Since(start); d > drainTimeout+time.Second {
					t.Errorf("stream %d: Close took %s, want at most about %s", i, d, drainTimeout)
				}
			}
			if n := conns.Load(); n > tt.wantConns {
				t.Errorf("%d streams used %d upstream connections, want at most %d", tt.streams, n, tt.wantConns)
			}
		})
	}
}

// TestIdleExcludesCallerTime checks that the idle timeout measures only
// the wait for the upstream: a caller that holds each chunk longer than
// idle (as the gateway does while a slow client reads) does not time the
// stream out, because the upstream had sent the next event long before.
func TestIdleExcludesCallerTime(t *testing.T) {
	const idle = 100 * time.Millisecond
	chunk := "data: {\"choices\":[{\"index\":0}]}\n\n"
	u := newUpstream(t, sseHandler([]string{chunk}, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
		time.Sleep(idle / 5)
		_, _ = io.WriteString(w, chunk+chunk+"data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	p := testProvider(u.URL)
	p.Timeouts.Idle = config.Duration(idle)
	a := newAdapter(t, p)
	s, err := a.ChatStream(ctxWithTimeout(t), chatRequest(t, streamBody, nil))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer func() { _ = s.Close() }()
	for i := range 3 {
		time.Sleep(4 * idle) // the caller holds the previous chunk
		if _, err := s.Next(); err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next after the last chunk = %v, want io.EOF", err)
	}
}
