package gateway

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"switchyard/internal/config"
	"switchyard/internal/tracing"
)

// tracedDo serves body as t1 under a recording root span, which it ends,
// and returns the spans recorded, by name, and the root.
func tracedDo(t *testing.T, e *cacheEnv, body string) (map[string][]sdktrace.ReadOnlySpan, sdktrace.ReadOnlySpan) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, root := tp.Tracer("test").Start(context.Background(), "root")
	if r := e.doAs(ctx, t1, EndpointChatCompletions, body, nil); r.status != 200 {
		t.Fatalf("status %d (%v)", r.status, r.err)
	}
	e.wait()
	root.End()
	byName := map[string][]sdktrace.ReadOnlySpan{}
	var rootSpan sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "root" {
			rootSpan = s
			continue
		}
		byName[s.Name()] = append(byName[s.Name()], s)
	}
	return byName, rootSpan
}

func attrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	m := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes() {
		m[kv.Key] = kv.Value
	}
	return m
}

// TestPipelineSpans: a chat request under a recording span gets one child
// span per pipeline step, the attempt and the root carry their metadata
// attributes, and no span holds the prompt or the answer.
func TestPipelineSpans(t *testing.T) {
	budgets := []config.Budget{{Scope: config.BudgetScopeTenant, Window: config.BudgetWindowDay, LimitUSD: mustPrice(t, "100")}}
	const prompt = "secret-prompt-text-Zq9"
	body := `{"model":"m","messages":[{"role":"user","content":"` + prompt + `"}]}`
	e := newCacheEnv(t, exactOn, budgets, CacheOptions{}, nil)
	spans, root := tracedDo(t, e, body)

	for _, name := range []string{tracing.SpanPolicy, tracing.SpanCacheExact, tracing.SpanRoute, tracing.SpanBudgetAdmit,
		tracing.SpanUpstreamAttempt, tracing.SpanUsageRecord} {
		got := spans[name]
		if len(got) != 1 {
			t.Errorf("%d %s spans, want 1", len(got), name)
			continue
		}
		if got[0].Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("%s is not a child of the root span", name)
		}
	}
	if s := spans[tracing.SpanCacheExact]; len(s) == 1 && attrs(s[0])[tracing.AttrCacheStatus].AsString() != "miss" {
		t.Errorf("cache.exact attributes %v", attrs(s[0]))
	}
	if s := spans[tracing.SpanUpstreamAttempt]; len(s) == 1 {
		a := attrs(s[0])
		if a[tracing.AttrAttemptNumber].AsInt64() != 1 || a[tracing.AttrProvider].AsString() != "a" ||
			a[tracing.AttrDeployment].AsString() != "m@a" || a[tracing.AttrOutcome].AsString() != "ok" ||
			a["gen_ai.system"].AsString() != "openai" || a["gen_ai.request.model"].AsString() != "m-a" ||
			a["gen_ai.usage.output_tokens"].AsInt64() == 0 {
			t.Errorf("upstream.attempt attributes %v", a)
		}
	}
	ra := attrs(root)
	if ra[tracing.AttrTenant].AsString() != "t1" || ra[tracing.AttrKeyID].AsString() != "k1" ||
		ra[tracing.AttrCacheStatus].AsString() != "miss" || ra[tracing.AttrRouteReason].AsString() != reasonCheapest ||
		ra[tracing.AttrAttempts].AsInt64() != 1 || ra[tracing.AttrCostUSD].AsFloat64() <= 0 ||
		ra["gen_ai.request.model"].AsString() != "m" || ra["gen_ai.response.model"].AsString() != "m" ||
		ra["gen_ai.usage.input_tokens"].AsInt64() == 0 {
		t.Errorf("root attributes %v", ra)
	}
	for name, ss := range spans {
		for _, s := range ss {
			for _, kv := range s.Attributes() {
				if strings.Contains(kv.Value.String(), prompt) || strings.Contains(kv.Value.String(), "hello") {
					t.Errorf("span %s attribute %s holds content: %s", name, kv.Key, kv.Value.String())
				}
			}
		}
	}

	// A hit: cache.exact says so, and no upstream attempt is traced.
	spans, root = tracedDo(t, e, body)
	if s := spans[tracing.SpanCacheExact]; len(s) != 1 || attrs(s[0])[tracing.AttrCacheStatus].AsString() != "hit" {
		t.Errorf("hit: cache.exact spans %v", s)
	}
	if n := len(spans[tracing.SpanUpstreamAttempt]); n != 0 {
		t.Errorf("hit: %d upstream.attempt spans", n)
	}
	if got := attrs(root)[tracing.AttrCacheStatus].AsString(); got != "exact" {
		t.Errorf("hit: root cache_status %q", got)
	}
}

// TestSemanticSpans: a semantic lookup is a cache.semantic span with the
// embedding and the search as its children.
func TestSemanticSpans(t *testing.T) {
	sem := config.CachePolicy{Exact: config.ExactCachePolicy{Enabled: true}, Semantic: config.SemanticCachePolicy{Enabled: true}}
	e := newCacheEnv(t, sem, nil, CacheOptions{}, nil)
	e.setSemantic(t, fakeEmbedder{vecs: map[string][]float32{"hi": {1, 0}}})
	spans, root := tracedDo(t, e, chatReq)
	s := spans[tracing.SpanCacheSemantic]
	if len(s) != 1 || s[0].Parent().SpanID() != root.SpanContext().SpanID() {
		t.Fatalf("cache.semantic spans %v", s)
	}
	for _, name := range []string{tracing.SpanEmbed, tracing.SpanSearch} {
		c := spans[name]
		if len(c) != 1 || c[0].Parent().SpanID() != s[0].SpanContext().SpanID() {
			t.Errorf("%s spans %v, want one child of cache.semantic", name, c)
		}
	}
}
