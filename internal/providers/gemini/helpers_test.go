package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"switchyard/internal/config"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
	"switchyard/test/fakegemini"
)

// testTimeout bounds every wait in these tests. It is far longer than any
// operation should take, so it only fires when something is broken.
const testTimeout = 10 * time.Second

// providerKey has the shape of a Google API key.
const providerKey = "AIzaSyFakeProviderKey0123456789abcdefghij"

// testModel is the upstream model of every request.
const testModel = "gemini-test"

// fixedNow is the clock of every adapter under test. Its Unix time,
// 1794398400, is the created field of the golden responses and chunks.
var fixedNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// fixturesDir holds the handcrafted Gemini payloads.
const fixturesDir = "../../../test/fixtures/gemini"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixturesDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testProvider(baseURL string) config.Provider {
	return config.Provider{
		ID:             "gem",
		Type:           "gemini",
		BaseURL:        baseURL,
		APIKey:         config.NewSecret(providerKey),
		ForwardHeaders: []string{"x-goog-user-project", "x-goog-api-key", "Authorization", "Accept-Encoding"},
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
	a, err := New(p, providers.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.now = func() time.Time { return fixedNow }
	t.Cleanup(a.CloseIdleConnections)
	return a
}

// newFake starts a fakegemini server that requires the provider key.
func newFake(t *testing.T) *fakegemini.Server {
	t.Helper()
	f := fakegemini.New(fakegemini.Options{APIKey: providerKey})
	t.Cleanup(f.Close)
	return f
}

// allCaps are the capabilities the catalog declares for Gemini models.
var allCaps = []schema.Capability{
	schema.CapStreaming, schema.CapStreamUsage, schema.CapTools, schema.CapParallelTools, schema.CapForcedTools,
	schema.CapVision, schema.CapJSONMode, schema.CapJSONSchema, schema.CapPromptCaching,
}

// request decodes an OpenAI-shaped body into a providers.Request for the
// upstream model testModel.
func request(t *testing.T, body string, caps ...schema.Capability) *providers.Request {
	t.Helper()
	req, err := schema.DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatalf("DecodeChatRequest: %v", err)
	}
	set := schema.CapabilitySet{}
	for _, c := range caps {
		set[c] = struct{}{}
	}
	return &providers.Request{Chat: req, UpstreamModel: testModel, Header: http.Header{}, Capabilities: set}
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
	if pe.Provider != "gem" {
		t.Errorf("provider = %q, want gem", pe.Provider)
	}
	if strings.Contains(pe.Error(), providerKey) || leaksKey(pe.Body) ||
		(pe.Err != nil && strings.Contains(pe.Err.Error(), providerKey)) {
		t.Errorf("error leaks the provider key: %v", pe)
	}
	return pe
}

// leaksKey reports whether body holds the provider key, as raw bytes or in
// any string a client decodes from it as JSON.
func leaksKey(body []byte) bool {
	if bytes.Contains(body, []byte(providerKey)) {
		return true
	}
	var v any
	if json.Unmarshal(body, &v) != nil {
		return false
	}
	var walk func(v any) bool
	walk = func(v any) bool {
		switch v := v.(type) {
		case string:
			return strings.Contains(v, providerKey)
		case []any:
			for _, e := range v {
				if walk(e) {
					return true
				}
			}
		case map[string]any:
			for k, e := range v {
				if strings.Contains(k, providerKey) || walk(e) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}

// escapedKeys are spellings of the provider key inside a JSON string that
// do not contain it as raw bytes but decode to it.
var escapedKeys = map[string]string{
	"one escaped letter": strings.Replace(providerKey, "S", `S`, 1),
	"every byte escaped": func() string {
		var b strings.Builder
		for _, r := range providerKey {
			fmt.Fprintf(&b, `\u%04x`, r)
		}
		return b.String()
	}(),
}

// jsonDiff compares two JSON documents semantically, numbers by their
// literal text, and returns a cmp diff or "".
func jsonDiff(t *testing.T, want, got []byte) string {
	t.Helper()
	decode := func(b []byte) any {
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatalf("invalid JSON %s: %v", b, err)
		}
		return v
	}
	return cmp.Diff(decode(want), decode(got))
}

// upstream is a handcrafted fake for tests that need control fakegemini
// does not give: raw SSE, hangs at exact points, odd bodies.
type upstream struct {
	*httptest.Server
	mu       sync.Mutex
	bodies   []string
	urls     []string
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
		u.urls = append(u.urls, r.URL.String())
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

func (u *upstream) last() (url string, h http.Header, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := len(u.bodies) - 1
	return u.urls[n], u.headers[n], u.bodies[n]
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

// sseHandler writes events (raw SSE text), flushing after each, then runs
// after.
func sseHandler(events []string, after func(u *upstream, w http.ResponseWriter, r *http.Request)) func(*upstream, http.ResponseWriter, *http.Request) {
	return func(u *upstream, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
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

// jsonHandler answers every request with status and body.
func jsonHandler(status int, body string) func(*upstream, http.ResponseWriter, *http.Request) {
	return func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// data frames one event.
func data(json string) string { return "data: " + json + "\n\n" }

// collect reads s to its end and returns the raw chunks and the final error.
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

const (
	plainBody  = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	streamBody = `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
)

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
