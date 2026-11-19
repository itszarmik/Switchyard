package conformance

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"switchyard/test/conformance/harness"
	"switchyard/test/fakeupstream"
)

// otlpCollector is a fake OTLP/HTTP collector: it decodes every protobuf
// export posted to /v1/traces and keeps the spans and the raw bodies.
type otlpCollector struct {
	srv *httptest.Server

	mu      sync.Mutex
	spans   []*tracepb.Span
	raw     [][]byte
	errs    []error
	changed chan struct{} // closed and replaced on every export
}

func newOTLPCollector(t *testing.T) *otlpCollector {
	t.Helper()
	c := &otlpCollector{changed: make(chan struct{})}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		var td tracepb.TracesData
		if err == nil && (r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/x-protobuf") {
			err = fmt.Errorf("export to %s as %s", r.URL.Path, r.Header.Get("Content-Type"))
		}
		if err == nil {
			err = proto.Unmarshal(b, &td)
		}
		c.mu.Lock()
		c.raw = append(c.raw, b)
		if err != nil {
			c.errs = append(c.errs, err)
		}
		for _, rs := range td.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				c.spans = append(c.spans, ss.GetSpans()...)
			}
		}
		close(c.changed)
		c.changed = make(chan struct{})
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// trace waits until the collector holds a trace whose root span is
// named root and that has a span of every name in want, and returns its
// spans by name and its root.
func (c *otlpCollector) trace(t *testing.T, root string, want ...string) (map[string][]*tracepb.Span, *tracepb.Span) {
	t.Helper()
	deadline := time.After(30 * time.Second) // the batch processor exports every 5 s
	for {
		c.mu.Lock()
		byTrace := map[string]map[string][]*tracepb.Span{}
		roots := map[string]*tracepb.Span{}
		for _, s := range c.spans {
			id := hex.EncodeToString(s.GetTraceId())
			if byTrace[id] == nil {
				byTrace[id] = map[string][]*tracepb.Span{}
			}
			byTrace[id][s.GetName()] = append(byTrace[id][s.GetName()], s)
			if s.GetName() == root && len(s.GetParentSpanId()) == 0 {
				roots[id] = s
			}
		}
		changed, errs := c.changed, c.errs
		c.mu.Unlock()
		if len(errs) > 0 {
			t.Fatalf("the collector could not decode an export: %v", errs)
		}
		for id, r := range roots {
			complete := true
			for _, name := range want {
				if len(byTrace[id][name]) == 0 {
					complete = false
				}
			}
			if complete {
				return byTrace[id], r
			}
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("no trace rooted at %q with spans %v reached the collector", root, want)
		}
	}
}

// rawExports returns every export body received so far.
func (c *otlpCollector) rawExports() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.raw...)
}

// spanAttrs returns a span's attributes as strings, ints and floats
// rendered with %v.
func spanAttrs(s *tracepb.Span) map[string]string {
	m := map[string]string{}
	for _, kv := range s.GetAttributes() {
		v := kv.GetValue()
		switch {
		case v.GetStringValue() != "":
			m[kv.GetKey()] = v.GetStringValue()
		case v.GetDoubleValue() != 0:
			m[kv.GetKey()] = fmt.Sprint(v.GetDoubleValue())
		default:
			m[kv.GetKey()] = fmt.Sprint(v.GetIntValue())
		}
	}
	return m
}

// tracingConfig renders a traced gateway's config: the fake upstream as
// provider up, with the chat model cm and the embedding model emb, the
// collector as telemetry.tracing.endpoint (sampling everything and
// propagating upstream), the default policy's cache block, and tenant t1,
// rate-limited and under a budget, with the key of hash.
func tracingConfig(up *fakeupstream.Server, collector, cache, extra, hash string) []byte {
	return fmt.Appendf(nil, `version: 1
server: {listen: "127.0.0.1:0", admin_listen: "127.0.0.1:0", shutdown_delay: 0s, shutdown_grace: 5s}
logging: {level: info, format: json}
telemetry:
  tracing: {enabled: true, endpoint: %q, sample_ratio: 1, propagate_upstream: true}
%s
%s
policies:
  - id: default
    cache: %s
tenants:
  - id: t1
    rate_limit: {requests_per_minute: 1000}
    budgets: [{scope: tenant, window: day, limit_usd: "100"}]
    keys: [{id: k1, hash: %q}]
`, collector, cacheModels(up.BaseURL(), 2), extra, cache, hash)
}

// TestTraceSpanTree checks PLAN P7's tracing criterion through the
// binary: one chat request, with the exact cache on, a rate limit and a
// budget, is one trace in which the root server span has a child for
// every pipeline step (auth, ratelimit, policy, cache.exact, route,
// budget.admit, upstream.attempt, usage.record), the attempt and the
// root carry the GenAI and switchyard attributes, the upstream received
// the attempt's traceparent, and nothing exported holds content or a
// secret. A semantic-cache request adds cache.semantic, with embed and
// search under it.
func TestTraceSpanTree(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the gateway; skipped with -short")
	}
	const prompt = "trace-prompt-content-Vb7Qx2"
	body := `{"model":"cm","messages":[{"role":"user","content":"` + prompt + `"}]}`
	root := "POST /v1/chat/completions"

	t.Run("exact", func(t *testing.T) {
		col := newOTLPCollector(t)
		up := fakeupstream.New(fakeupstream.Options{APIKey: providerKey})
		t.Cleanup(up.Close)
		key, hash, err := harness.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		g := harness.RunYAML(t, tracingConfig(up, col.srv.URL, `{exact: {enabled: true}}`, "", hash),
			harness.RunOptions{Race: raceEnabled, Env: []string{"PROVIDER_KEY=" + providerKey}})
		status, resp := postChat(t, g, key, body)
		if status != http.StatusOK {
			t.Fatalf("chat: %d %s", status, resp)
		}

		steps := []string{"auth", "ratelimit", "policy", "cache.exact", "route", "budget.admit", "upstream.attempt", "usage.record"}
		spans, rootSpan := col.trace(t, root, steps...)
		for _, name := range steps {
			if n := len(spans[name]); n != 1 {
				t.Errorf("%d %s spans, want 1", n, name)
			}
			if s := spans[name][0]; !bytes.Equal(s.GetParentSpanId(), rootSpan.GetSpanId()) {
				t.Errorf("%s is not a child of the root span", name)
			}
		}
		if n := len(spans["cache.semantic"]); n != 0 {
			t.Errorf("%d cache.semantic spans with only the exact cache on", n)
		}
		ra := spanAttrs(rootSpan)
		for k, want := range map[string]string{
			"http.request.method": "POST", "http.route": "/v1/chat/completions", "http.response.status_code": "200",
			"switchyard.tenant": "t1", "switchyard.key_id": "k1", "switchyard.cache_status": "miss",
			"switchyard.route_reason": "cheapest_eligible", "switchyard.attempts": "1",
			"gen_ai.system": "openai", "gen_ai.request.model": "cm", "gen_ai.response.model": "cm",
		} {
			if ra[k] != want {
				t.Errorf("root %s = %q, want %q (all: %v)", k, ra[k], want, ra)
			}
		}
		for _, k := range []string{"switchyard.cost_usd", "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens"} {
			if v := ra[k]; v == "" || v == "0" {
				t.Errorf("root %s = %q, want a value", k, v)
			}
		}
		attempt := spans["upstream.attempt"][0]
		aa := spanAttrs(attempt)
		for k, want := range map[string]string{
			"attempt.number": "1", "switchyard.provider": "up", "switchyard.deployment": "cm@up",
			"switchyard.outcome": "ok", "gen_ai.system": "openai", "gen_ai.request.model": "cm-up",
		} {
			if aa[k] != want {
				t.Errorf("upstream.attempt %s = %q, want %q (all: %v)", k, aa[k], want, aa)
			}
		}
		// The upstream got the attempt's trace context.
		reqs := up.Requests()
		if len(reqs) != 1 {
			t.Fatalf("%d upstream requests, want 1", len(reqs))
		}
		want := fmt.Sprintf("00-%x-%x-01", attempt.GetTraceId(), attempt.GetSpanId())
		if got := reqs[0].Header.Get("Traceparent"); got != want {
			t.Errorf("upstream traceparent %q, want %q (the attempt span)", got, want)
		}
		for i, b := range col.rawExports() {
			for name, v := range map[string]string{"prompt": prompt, "answer": fakeupstream.Text, "gateway key": key, "provider key": providerKey} {
				if bytes.Contains(b, []byte(v)) {
					t.Errorf("export %d holds the %s", i, name)
				}
			}
		}
	})

	t.Run("semantic", func(t *testing.T) {
		col := newOTLPCollector(t)
		up := fakeupstream.New(fakeupstream.Options{APIKey: providerKey, Embed: func(string) []float32 { return []float32{1, 0} }})
		t.Cleanup(up.Close)
		key, hash, err := harness.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		g := harness.RunYAML(t, tracingConfig(up, col.srv.URL, `{exact: {enabled: true}, semantic: {enabled: true}}`,
			"semantic_cache: {embedding_model: emb}", hash),
			harness.RunOptions{Race: raceEnabled, Env: []string{"PROVIDER_KEY=" + providerKey}})
		if status, resp := postChat(t, g, key, body); status != http.StatusOK {
			t.Fatalf("chat: %d %s", status, resp)
		}
		spans, rootSpan := col.trace(t, root, "cache.exact", "cache.semantic", "embed", "search")
		sem := spans["cache.semantic"][0]
		if !bytes.Equal(sem.GetParentSpanId(), rootSpan.GetSpanId()) {
			t.Error("cache.semantic is not a child of the root span")
		}
		for _, name := range []string{"embed", "search"} {
			if s := spans[name]; len(s) != 1 || !bytes.Equal(s[0].GetParentSpanId(), sem.GetSpanId()) {
				t.Errorf("%s spans %v, want one child of cache.semantic", name, s)
			}
		}
		if got := spanAttrs(sem)["switchyard.cache_status"]; got != "miss" {
			t.Errorf("cache.semantic cache_status %q, want miss", got)
		}
		for i, b := range col.rawExports() {
			if bytes.Contains(b, []byte(prompt)) {
				t.Errorf("export %d holds the prompt", i)
			}
		}
	})
}

// postChat sends a chat completion with key and returns the status and
// body.
func postChat(t *testing.T, g *harness.Gateway, key, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, g.PublicURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: callTimeout}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}
