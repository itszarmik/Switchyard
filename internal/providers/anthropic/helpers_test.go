package anthropic

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
)

// testTimeout bounds every wait in these tests. It is far longer than any
// operation should take, so it only fires when something is broken.
const testTimeout = 10 * time.Second

const providerKey = "sk-ant-provider-secret-0123456789abcdef"

// fixedNow is the clock of every adapter under test. Its Unix time,
// 1794398400, is the created field of the golden responses and chunks.
var fixedNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// fixturesDir holds the handcrafted Messages API payloads.
const fixturesDir = "../../../test/fixtures/anthropic"

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
		ID:             "anth",
		Type:           "anthropic",
		BaseURL:        baseURL,
		APIKey:         config.NewSecret(providerKey),
		ForwardHeaders: []string{"anthropic-beta", "x-api-key", "Anthropic-Version", "Authorization", "Accept-Encoding"},
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
	a.now = func() time.Time { return fixedNow }
	t.Cleanup(a.CloseIdleConnections)
	return a
}

// request decodes an OpenAI-shaped body into a providers.Request for the
// upstream model claude-test.
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
	return &providers.Request{Chat: req, UpstreamModel: "claude-test", Header: http.Header{}, Capabilities: set}
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
	if pe.Provider != "anth" {
		t.Errorf("provider = %q, want anth", pe.Provider)
	}
	if strings.Contains(pe.Error(), providerKey) || leaksKey(pe.Body) {
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
	"one escaped dash": strings.Replace(providerKey, "-", `\u002d`, 1),
	"every byte escaped": func() string {
		var b strings.Builder
		for _, r := range providerKey {
			fmt.Fprintf(&b, `\u%04x`, r)
		}
		return b.String()
	}(),
	"escaped first letter, upper-case hex": `\u0073` + strings.ReplaceAll(providerKey[1:], "-", `\u002D`),
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

// upstream is a handcrafted fake for tests that need control the
// fakeanthropic server does not give: raw SSE, hangs at exact points,
// pings.
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

func (u *upstream) lastHeader() http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.headers[len(u.headers)-1]
}

func (u *upstream) lastBody() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.bodies[len(u.bodies)-1]
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
		w.Header().Set("Request-Id", "req-s")
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
