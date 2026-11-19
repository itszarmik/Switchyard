package tracing

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
)

// IDs used by the conversion tests.
var (
	tTraceID  = trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	tSpanID   = trace.SpanID{1, 1, 1, 1, 1, 1, 1, 1}
	tParentID = trace.SpanID{2, 2, 2, 2, 2, 2, 2, 2}
	tLinkID   = trace.SpanID{3, 3, 3, 3, 3, 3, 3, 3}
)

func str(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}
func i64(v int64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}
}
func arr(vs ...*commonpb.AnyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vs}}}
}
func kv(k string, v *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: v}
}

func TestSpanConversion(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	ts, err := trace.ParseTraceState("vendor=v1")
	if err != nil {
		t.Fatal(err)
	}
	res := resource.NewWithAttributes("https://opentelemetry.io/schemas/1.43.0", attribute.String("service.name", "sy"))
	scope := instrumentation.Scope{Name: "switchyard", Version: "v1", SchemaURL: "https://scope.example"}
	stub := tracetest.SpanStub{
		Name: "POST /v1/chat/completions",
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: tTraceID, SpanID: tSpanID, TraceFlags: trace.FlagsSampled, TraceState: ts,
		}),
		Parent: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: tTraceID, SpanID: tParentID, TraceFlags: trace.FlagsSampled, Remote: true,
		}),
		SpanKind:  trace.SpanKindServer,
		StartTime: start,
		EndTime:   start.Add(1500 * time.Millisecond),
		Attributes: []attribute.KeyValue{
			attribute.String("s", "v"),
			attribute.Bool("b", true),
			attribute.Int64("i", -7),
			attribute.Float64("f", 0.25),
			attribute.StringSlice("ss", []string{"a", "b"}),
			attribute.Int64Slice("is", []int64{1, 2}),
			attribute.BoolSlice("bs", []bool{false}),
			attribute.Float64Slice("fs", []float64{1.5}),
		},
		Events: []sdktrace.Event{{
			Name: "retry", Time: start.Add(time.Second),
			Attributes: []attribute.KeyValue{attribute.Int("attempt.number", 2)}, DroppedAttributeCount: 1,
		}},
		Links: []sdktrace.Link{{
			SpanContext: trace.NewSpanContext(trace.SpanContextConfig{TraceID: tTraceID, SpanID: tLinkID}),
			Attributes:  []attribute.KeyValue{attribute.String("why", "shadow")}, DroppedAttributeCount: 2,
		}},
		Status:               sdktrace.Status{Code: codes.Error, Description: "upstream failed"},
		DroppedAttributes:    3,
		DroppedEvents:        4,
		DroppedLinks:         5,
		Resource:             res,
		InstrumentationScope: scope,
	}

	got := tracesData([]sdktrace.ReadOnlySpan{stub.Snapshot()})
	want := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", str("sy"))}},
		SchemaUrl: "https://opentelemetry.io/schemas/1.43.0",
		ScopeSpans: []*tracepb.ScopeSpans{{
			Scope:     &commonpb.InstrumentationScope{Name: "switchyard", Version: "v1"},
			SchemaUrl: "https://scope.example",
			Spans: []*tracepb.Span{{
				TraceId:           tTraceID[:],
				SpanId:            tSpanID[:],
				TraceState:        "vendor=v1",
				ParentSpanId:      tParentID[:],
				Flags:             0x01 | 0x100 | 0x200, // sampled, remote known, parent remote
				Name:              "POST /v1/chat/completions",
				Kind:              tracepb.Span_SPAN_KIND_SERVER,
				StartTimeUnixNano: uint64(start.UnixNano()),
				EndTimeUnixNano:   uint64(start.Add(1500 * time.Millisecond).UnixNano()),
				Attributes: []*commonpb.KeyValue{
					kv("s", str("v")),
					kv("b", &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}),
					kv("i", i64(-7)),
					kv("f", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.25}}),
					kv("ss", arr(str("a"), str("b"))),
					kv("is", arr(i64(1), i64(2))),
					kv("bs", arr(&commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: false}})),
					kv("fs", arr(&commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 1.5}})),
				},
				DroppedAttributesCount: 3,
				Events: []*tracepb.Span_Event{{
					Name: "retry", TimeUnixNano: uint64(start.Add(time.Second).UnixNano()),
					Attributes: []*commonpb.KeyValue{kv("attempt.number", i64(2))}, DroppedAttributesCount: 1,
				}},
				DroppedEventsCount: 4,
				Links: []*tracepb.Span_Link{{
					TraceId: tTraceID[:], SpanId: tLinkID[:], Flags: 0x100,
					Attributes: []*commonpb.KeyValue{kv("why", str("shadow"))}, DroppedAttributesCount: 2,
				}},
				DroppedLinksCount: 5,
				Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "upstream failed"},
			}},
		}},
	}}}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("tracesData (-want +got):\n%s", diff)
	}

	// ExportTraceServiceRequest is "repeated ResourceSpans resource_spans =
	// 1", the same as TracesData. Check the encoding has exactly that
	// shape, without importing the collector package, which links gRPC.
	body, err := proto.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []*tracepb.ResourceSpans
	for len(body) > 0 {
		num, typ, n := protowire.ConsumeTag(body)
		if n < 0 || num != 1 || typ != protowire.BytesType {
			t.Fatalf("top-level field %d (type %d), want only field 1, length-delimited", num, typ)
		}
		msg, m := protowire.ConsumeBytes(body[n:])
		if m < 0 {
			t.Fatal("truncated field 1")
		}
		rs := &tracepb.ResourceSpans{}
		if err := proto.Unmarshal(msg, rs); err != nil {
			t.Fatalf("decode field 1 as ResourceSpans: %v", err)
		}
		decoded = append(decoded, rs)
		body = body[n+m:]
	}
	if diff := cmp.Diff(want.ResourceSpans, decoded, protocmp.Transform()); diff != "" {
		t.Errorf("decoded ResourceSpans (-want +got):\n%s", diff)
	}
}

func TestConversionDetails(t *testing.T) {
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"kind internal", spanKind(trace.SpanKindInternal), tracepb.Span_SPAN_KIND_INTERNAL},
		{"kind client", spanKind(trace.SpanKindClient), tracepb.Span_SPAN_KIND_CLIENT},
		{"kind producer", spanKind(trace.SpanKindProducer), tracepb.Span_SPAN_KIND_PRODUCER},
		{"kind consumer", spanKind(trace.SpanKindConsumer), tracepb.Span_SPAN_KIND_CONSUMER},
		{"kind unspecified", spanKind(trace.SpanKindUnspecified), tracepb.Span_SPAN_KIND_UNSPECIFIED},
		{"status ok", statusCode(codes.Ok), tracepb.Status_STATUS_CODE_OK},
		{"status unset", statusCode(codes.Unset), tracepb.Status_STATUS_CODE_UNSET},
		{"time before the epoch", unixNano(time.Unix(-1, 0)), uint64(0)},
		{"negative count", clampUint32(-1), uint32(0)},
		{"huge count", clampUint32(1 << 40), uint32(1<<32 - 1)},
		{"flags, local parent", spanFlags(0, trace.SpanContext{}), uint32(0x100)},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}

	values := []struct {
		name string
		in   attribute.Value
		want *commonpb.AnyValue
	}{
		{"bytes", attribute.ByteSliceValue([]byte{1, 2}), &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{1, 2}}}},
		{"slice", attribute.SliceValue(attribute.StringValue("a"), attribute.Int64Value(1)), arr(str("a"), i64(1))},
		{"map", attribute.MapValue(attribute.String("k", "v")),
			&commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{kv("k", str("v"))}}}}},
		{"empty", attribute.Value{}, &commonpb.AnyValue{}},
	}
	for _, tt := range values {
		if diff := cmp.Diff(tt.want, anyValue(tt.in), protocmp.Transform()); diff != "" {
			t.Errorf("anyValue(%s) (-want +got):\n%s", tt.name, diff)
		}
	}
}

func TestConversionGrouping(t *testing.T) {
	resA := resource.NewSchemaless(attribute.String("service.name", "a"))
	resB := resource.NewSchemaless(attribute.String("service.name", "b"))
	scope1, scope2 := instrumentation.Scope{Name: "one"}, instrumentation.Scope{Name: "two"}
	span := func(name string, res *resource.Resource, scope instrumentation.Scope) sdktrace.ReadOnlySpan {
		return tracetest.SpanStub{Name: name, Resource: res, InstrumentationScope: scope}.Snapshot()
	}
	got := tracesData([]sdktrace.ReadOnlySpan{
		span("a1", resA, scope1), span("b1", resB, scope1), nil, span("a2", resA, scope2), span("a3", resA, scope1),
	})
	// resource -> scope -> span names, in first-seen order.
	var shape [][]string
	for _, rs := range got.ResourceSpans {
		var row []string
		for _, ss := range rs.ScopeSpans {
			names := ss.Scope.Name + ":"
			for _, s := range ss.Spans {
				names += " " + s.Name
			}
			row = append(row, names)
		}
		shape = append(shape, row)
	}
	want := [][]string{{"one: a1 a3", "two: a2"}, {"one: b1"}}
	if diff := cmp.Diff(want, shape); diff != "" {
		t.Errorf("grouping (-want +got):\n%s", diff)
	}
	if len(tracesData(nil).ResourceSpans) != 0 {
		t.Error("tracesData(nil) is not empty")
	}
}
