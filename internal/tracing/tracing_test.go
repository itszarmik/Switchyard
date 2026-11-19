package tracing

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"switchyard/internal/config"
	"switchyard/internal/version"
)

// Incoming W3C traceparents: the same trace and parent span, sampled and
// not sampled.
const (
	parentTraceID       = "4bf92f3577b34da6a3ce929d0e0e4736"
	parentSpanID        = "00f067aa0ba902b7"
	sampledParent       = "00-" + parentTraceID + "-" + parentSpanID + "-01"
	unsampledParent     = "00-" + parentTraceID + "-" + parentSpanID + "-00"
	testServiceName     = "switchyard-test"
	testServiceVersion  = "v9.9.9-test"
	testCollectorURL    = "http://collector.test:4318"
	testInstrumentation = scopeName
)

// discard is a logger that drops everything.
func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestProvider returns an enabled Provider that exports every span, as
// it ends, to the returned in-memory exporter.
func newTestProvider(t *testing.T, ratio float64) (*Provider, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	cfg := config.Tracing{
		Enabled: true, Endpoint: config.NewSecret(testCollectorURL),
		SampleRatio: ratio, ServiceName: testServiceName,
	}
	p, err := newProvider(context.Background(), cfg, version.Info{Version: testServiceVersion}, discard(),
		options{exporter: exp, syncExport: true})
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p, exp
}

// attrMap returns attrs keyed by attribute name.
func attrMap(attrs []attribute.KeyValue) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value, len(attrs))
	for _, a := range attrs {
		m[a.Key] = a.Value
	}
	return m
}

func TestNewDisabled(t *testing.T) {
	p, err := New(context.Background(), config.Tracing{SampleRatio: 1, ServiceName: "x"}, version.Info{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, span := p.Tracer().Start(context.Background(), "root")
	if span.IsRecording() || span.SpanContext().IsValid() {
		t.Errorf("disabled tracer started a recording or valid span: %+v", span.SpanContext())
	}
	span.End()
	if _, _, ok := IDs(ctx); ok {
		t.Error("IDs reported a span context with tracing disabled and no parent")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestNewInvalid(t *testing.T) {
	const secret = "Zq8sUperSecretTracePass"
	tests := []struct {
		name string
		cfg  config.Tracing
	}{
		{"bad endpoint", config.Tracing{Enabled: true, Endpoint: config.NewSecret("ftp://u:" + secret + "@sinkhost.invalid"), SampleRatio: 0.5}},
		{"query", config.Tracing{Enabled: true, Endpoint: config.NewSecret("https://sinkhost.invalid/?token=" + secret), SampleRatio: 0.5}},
		{"no endpoint", config.Tracing{Enabled: true, SampleRatio: 0.5}},
		{"ratio", config.Tracing{Enabled: true, Endpoint: config.NewSecret("https://sinkhost.invalid"), SampleRatio: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(context.Background(), tt.cfg, version.Info{}, discard())
			if err == nil {
				t.Fatal("New: want an error")
			}
			if containsAny(err.Error(), secret, "sinkhost") {
				t.Errorf("error echoes the endpoint: %v", err)
			}
		})
	}
}

func TestParentBasedSampling(t *testing.T) {
	tests := []struct {
		name        string
		ratio       float64
		traceparent string
		wantSampled bool
	}{
		{"ratio 0, no parent", 0, "", false},
		{"ratio 1, no parent", 1, "", true},
		{"ratio 0, sampled parent", 0, sampledParent, true},
		{"ratio 1, unsampled parent", 1, unsampledParent, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, exp := newTestProvider(t, tt.ratio)
			ctx := context.Background()
			if tt.traceparent != "" {
				ctx = p.prop.Extract(ctx, mapCarrier{"traceparent": tt.traceparent})
			}
			ctx, span := p.Tracer().Start(ctx, "root")
			span.End()

			traceID, _, ok := IDs(ctx)
			if !ok {
				t.Fatal("IDs: no span context")
			}
			if tt.traceparent != "" && traceID != parentTraceID {
				t.Errorf("trace ID = %s, want the parent's %s", traceID, parentTraceID)
			}
			if got := len(exp.GetSpans()); got != map[bool]int{true: 1, false: 0}[tt.wantSampled] {
				t.Errorf("exported %d spans, want sampled=%v", got, tt.wantSampled)
			}
		})
	}
}

// mapCarrier is a propagation.TextMapCarrier over a map.
type mapCarrier map[string]string

func (c mapCarrier) Get(k string) string { return c[k] }
func (c mapCarrier) Set(k, v string)     { c[k] = v }
func (c mapCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

func TestStart(t *testing.T) {
	t.Run("child of a recording span", func(t *testing.T) {
		p, exp := newTestProvider(t, 1)
		ctx, root := p.Tracer().Start(context.Background(), "root")
		cctx, child := Start(ctx, SpanAuth, AttrTenant.String("acme"))
		if !child.IsRecording() {
			t.Fatal("child is not recording")
		}
		_, gchild := Start(cctx, SpanUpstreamAttempt, AttrAttemptNumber.Int(2))
		gchild.End()
		child.End()
		root.End()

		spans := exp.GetSpans()
		if len(spans) != 3 {
			t.Fatalf("exported %d spans, want 3", len(spans))
		}
		byName := make(map[string]tracetest.SpanStub)
		for _, s := range spans {
			byName[s.Name] = s
		}
		auth, attempt := byName[SpanAuth], byName[SpanUpstreamAttempt]
		if auth.Parent.SpanID() != root.SpanContext().SpanID() {
			t.Errorf("auth parent = %s, want root %s", auth.Parent.SpanID(), root.SpanContext().SpanID())
		}
		if attempt.Parent.SpanID() != auth.SpanContext.SpanID() {
			t.Errorf("upstream.attempt parent = %s, want auth %s", attempt.Parent.SpanID(), auth.SpanContext.SpanID())
		}
		if got := attrMap(auth.Attributes)[AttrTenant].AsString(); got != "acme" {
			t.Errorf("auth %s = %q, want acme", AttrTenant, got)
		}
		if got := attrMap(attempt.Attributes)[AttrAttemptNumber].AsInt64(); got != 2 {
			t.Errorf("attempt.number = %d, want 2", got)
		}
		if got := auth.InstrumentationScope.Name; got != testInstrumentation {
			t.Errorf("scope = %q, want %q", got, testInstrumentation)
		}
	})

	t.Run("no span in ctx", func(t *testing.T) {
		ctx := context.Background()
		got, span := Start(ctx, SpanRoute)
		if got != ctx || span.IsRecording() {
			t.Errorf("Start without a span changed ctx or recorded")
		}
		span.End()
	})

	t.Run("sampled-out parent keeps its IDs", func(t *testing.T) {
		p, exp := newTestProvider(t, 0)
		ctx, root := p.Tracer().Start(context.Background(), "root")
		got, span := Start(ctx, SpanRoute)
		if got != ctx || !span.SpanContext().Equal(root.SpanContext()) {
			t.Error("Start under a non-recording span did not return it unchanged")
		}
		span.End()
		root.End()
		if n := len(exp.GetSpans()); n != 0 {
			t.Errorf("exported %d spans, want 0", n)
		}
	})
}

func TestIDs(t *testing.T) {
	p, _ := newTestProvider(t, 1)
	ctx, span := p.Tracer().Start(context.Background(), "root")
	defer span.End()
	traceID, spanID, ok := IDs(ctx)
	sc := span.SpanContext()
	if !ok || traceID != sc.TraceID().String() || spanID != sc.SpanID().String() {
		t.Errorf("IDs = %s, %s, %v; want %s, %s, true", traceID, spanID, ok, sc.TraceID(), sc.SpanID())
	}
	if _, _, ok := IDs(context.Background()); ok {
		t.Error("IDs(Background) reported a span context")
	}
	remote := trace.ContextWithRemoteSpanContext(context.Background(), sc)
	if got, _, ok := IDs(remote); !ok || got != sc.TraceID().String() {
		t.Errorf("IDs(remote) = %s, %v", got, ok)
	}
}

func TestSetGenAI(t *testing.T) {
	tests := []struct {
		name string
		in   GenAI
		want map[attribute.Key]attribute.Value
	}{
		{"full", GenAI{System: "openai", RequestModel: "gpt-5", ResponseModel: "gpt-5-2026-08-01", InputTokens: 12, OutputTokens: 34},
			map[attribute.Key]attribute.Value{
				"gen_ai.system": attribute.StringValue("openai"), "gen_ai.provider.name": attribute.StringValue("openai"),
				"gen_ai.request.model": attribute.StringValue("gpt-5"), "gen_ai.response.model": attribute.StringValue("gpt-5-2026-08-01"),
				"gen_ai.usage.input_tokens": attribute.Int64Value(12), "gen_ai.usage.output_tokens": attribute.Int64Value(34),
			}},
		{"request only", GenAI{RequestModel: "m"},
			map[attribute.Key]attribute.Value{"gen_ai.request.model": attribute.StringValue("m")}},
		{"zero output tokens", GenAI{InputTokens: 5},
			map[attribute.Key]attribute.Value{"gen_ai.usage.input_tokens": attribute.Int64Value(5), "gen_ai.usage.output_tokens": attribute.Int64Value(0)}},
		{"empty", GenAI{}, map[attribute.Key]attribute.Value{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, exp := newTestProvider(t, 1)
			_, span := p.Tracer().Start(context.Background(), "s")
			SetGenAI(span, tt.in)
			span.End()
			got := attrMap(exp.GetSpans()[0].Attributes)
			if len(got) != len(tt.want) {
				t.Errorf("attributes = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v", k, got[k].String(), v.String())
				}
			}
		})
	}
	// A non-recording span is left alone.
	SetGenAI(trace.SpanFromContext(context.Background()), GenAI{System: "openai"})
}

// TestStartUntracedAllocs: with no recording span in the context (tracing
// disabled, or a sampled-out trace), Start and End cost no allocation, so
// the pipeline's spans are free when tracing is off.
func TestStartUntracedAllocs(t *testing.T) {
	remote := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, Remote: true,
	}))
	for name, ctx := range map[string]context.Context{"no span": context.Background(), "remote parent": remote} {
		if n := testing.AllocsPerRun(1000, func() {
			_, span := Start(ctx, SpanRoute)
			span.End()
		}); n != 0 {
			t.Errorf("%s: %v allocations per Start and End, want 0", name, n)
		}
	}
}
