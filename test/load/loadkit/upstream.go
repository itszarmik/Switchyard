// Package loadkit holds what the SLO bench (test/load slo) and the soak
// (test/soak) share: a minimal OpenAI-wire upstream with a fixed latency
// and an injected error rate that can time each request, a minimal OTLP
// collector that counts the spans it receives, a description of the host,
// and percentiles.
//
// The upstream is deliberately small, unlike test/fakeupstream: it does
// no recording or scripting, so at thousands of requests a second it costs
// little CPU, and it measures its own handling time per request, so a
// load generator can subtract it from the latency it sees.
package loadkit

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// SeqHeader carries a request's sequence number. When it is present and
// timings are recorded, the upstream records how long it spent on the
// request under that number. A gateway forwards it when its provider lists
// it in forward_headers.
const SeqHeader = "X-Load-Seq"

// Content is the text of every answer, split into stream chunks.
const Content = "The quick brown fox jumps over the lazy dog."

// UpstreamOptions configure an Upstream.
type UpstreamOptions struct {
	// Latency delays the response headers of a non-streaming answer and of
	// an error, and the first event of a stream (whose headers are sent at
	// once), like a model's time to first token.
	Latency time.Duration
	// Chunks is how many content events a stream sends; default 8.
	Chunks int
	// ChunkDelay is the pause between consecutive stream events.
	ChunkDelay time.Duration
	// ErrorRate is the fraction of requests, drawn at random, answered
	// with a 500 server_error instead.
	ErrorRate float64
	// Seed seeds the error draw, so runs are repeatable.
	Seed uint64
	// RecordTimings records each request's handling time by SeqHeader,
	// for Timing.
	RecordTimings bool
}

// Upstream is a running fake chat completions upstream at BaseURL.
type Upstream struct {
	opts UpstreamOptions
	ts   *httptest.Server

	requests atomic.Int64
	injected atomic.Int64

	mu      sync.Mutex // guards rng and timings
	rng     *rand.Rand
	timings map[int]time.Duration
}

// NewUpstream starts an Upstream. Call Close when done with it.
func NewUpstream(opts UpstreamOptions) *Upstream {
	if opts.Chunks <= 0 {
		opts.Chunks = 8
	}
	u := &Upstream{
		opts:    opts,
		rng:     rand.New(rand.NewPCG(opts.Seed, opts.Seed^0x9e3779b97f4a7c15)), // #nosec G404 -- a repeatable fault draw, not a secret
		timings: make(map[int]time.Duration),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", u.serveChat)
	u.ts = httptest.NewUnstartedServer(mux)
	u.ts.Config.ReadHeaderTimeout = 10 * time.Second
	u.ts.Start()
	return u
}

// BaseURL is the API root, for a provider's base_url.
func (u *Upstream) BaseURL() string { return u.ts.URL + "/v1" }

// Close shuts the server down.
func (u *Upstream) Close() { u.ts.Close() }

// Requests counts the chat completion requests received.
func (u *Upstream) Requests() int64 { return u.requests.Load() }

// Injected counts the requests answered with an injected 500.
func (u *Upstream) Injected() int64 { return u.injected.Load() }

// Timing returns how long the upstream spent on the request with sequence
// number seq: from the start of its handler to just before its last write.
func (u *Upstream) Timing(seq int) (time.Duration, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	d, ok := u.timings[seq]
	return d, ok
}

// chatRequest is the part of a request the upstream reads.
type chatRequest struct {
	Model         string `json:"model"`
	Stream        bool   `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// Token counts of every answer.
const (
	promptTokens     = 12
	completionTokens = 24
)

func (u *Upstream) serveChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	u.requests.Add(1)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	var req chatRequest
	if err == nil {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		http.Error(w, `{"error":{"message":"bad request","type":"invalid_request_error","param":null,"code":null}}`, http.StatusBadRequest)
		return
	}
	seq, timed := 0, false
	if u.opts.RecordTimings {
		seq, err = strconv.Atoi(r.Header.Get(SeqHeader))
		timed = err == nil
	}
	done := func() {
		if timed {
			d := time.Since(start)
			u.mu.Lock()
			u.timings[seq] = d
			u.mu.Unlock()
		}
	}

	if !sleep(r, u.opts.Latency) {
		return
	}
	if u.fail() {
		u.injected.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		done()
		_, _ = io.WriteString(w, `{"error":{"message":"injected upstream error","type":"server_error","param":null,"code":null}}`)
		return
	}
	if req.Stream {
		u.stream(w, r, req, done)
		return
	}
	resp := fmt.Sprintf(`{"id":"chatcmpl-load","object":"chat.completion","created":1767225600,"model":%q,`+
		`"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		req.Model, Content, promptTokens, completionTokens, promptTokens+completionTokens)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(resp)))
	done()
	_, _ = io.WriteString(w, resp)
}

// stream sends the answer as server-sent events, with a usage chunk when
// the request asks for one.
func (u *Upstream) stream(w http.ResponseWriter, r *http.Request, req chatRequest, done func()) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	chunk := func(delta, finish, usage string) string {
		return fmt.Sprintf(`data: {"id":"chatcmpl-load","object":"chat.completion.chunk","created":1767225600,"model":%q,"choices":[{"index":0,"delta":%s,"finish_reason":%s}]%s}`+"\n\n",
			req.Model, delta, finish, usage)
	}
	events := []string{chunk(`{"role":"assistant","content":""}`, "null", "")}
	n := u.opts.Chunks
	for i := range n {
		part := Content[i*len(Content)/n : (i+1)*len(Content)/n]
		events = append(events, chunk(fmt.Sprintf(`{"content":%q}`, part), "null", ""))
	}
	events = append(events, chunk(`{}`, `"stop"`, ""))
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		events = append(events, fmt.Sprintf(`data: {"id":"chatcmpl-load","object":"chat.completion.chunk","created":1767225600,"model":%q,"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`+"\n\n",
			req.Model, promptTokens, completionTokens, promptTokens+completionTokens))
	}
	for i, e := range events {
		if i > 0 && !sleep(r, u.opts.ChunkDelay) {
			return
		}
		if _, err := io.WriteString(w, e); err != nil {
			return
		}
		flusher.Flush()
	}
	done()
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// fail draws whether this request gets an injected error.
func (u *Upstream) fail() bool {
	if u.opts.ErrorRate <= 0 {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.rng.Float64() < u.opts.ErrorRate
}

// sleep waits d, and reports false if the client went away first.
func sleep(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		return false
	}
}
