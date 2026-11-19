package tracing

import (
	"math"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// tracesData converts spans to OTLP. TracesData is wire-compatible with the
// collector's ExportTraceServiceRequest (both hold the ResourceSpans in
// field 1), and its package does not link gRPC, unlike the collector's.
// Spans are grouped by resource, then by instrumentation scope, in the
// order they first appear.
func tracesData(spans []sdktrace.ReadOnlySpan) *tracepb.TracesData {
	type scopeKey struct {
		res   attribute.Distinct
		scope instrumentation.Scope
	}
	out := &tracepb.TracesData{}
	byResource := make(map[attribute.Distinct]*tracepb.ResourceSpans)
	byScope := make(map[scopeKey]*tracepb.ScopeSpans)
	for _, s := range spans {
		if s == nil {
			continue
		}
		res := s.Resource()
		rk := res.Equivalent()
		rs := byResource[rk]
		if rs == nil {
			rs = &tracepb.ResourceSpans{
				Resource:  &resourcepb.Resource{Attributes: iterValues(res.Iter())},
				SchemaUrl: res.SchemaURL(),
			}
			byResource[rk] = rs
			out.ResourceSpans = append(out.ResourceSpans, rs)
		}
		scope := s.InstrumentationScope()
		sk := scopeKey{res: rk, scope: scope}
		ss := byScope[sk]
		if ss == nil {
			ss = &tracepb.ScopeSpans{
				Scope: &commonpb.InstrumentationScope{
					Name: scope.Name, Version: scope.Version, Attributes: iterValues(scope.Attributes.Iter()),
				},
				SchemaUrl: scope.SchemaURL,
			}
			byScope[sk] = ss
			rs.ScopeSpans = append(rs.ScopeSpans, ss)
		}
		ss.Spans = append(ss.Spans, spanProto(s))
	}
	return out
}

// spanProto converts one span.
func spanProto(s sdktrace.ReadOnlySpan) *tracepb.Span {
	sc := s.SpanContext()
	traceID, spanID := sc.TraceID(), sc.SpanID()
	st := s.Status()
	out := &tracepb.Span{
		TraceId:                traceID[:],
		SpanId:                 spanID[:],
		TraceState:             sc.TraceState().String(),
		Flags:                  spanFlags(sc.TraceFlags(), s.Parent()),
		Name:                   s.Name(),
		Kind:                   spanKind(s.SpanKind()),
		StartTimeUnixNano:      unixNano(s.StartTime()),
		EndTimeUnixNano:        unixNano(s.EndTime()),
		Attributes:             keyValues(s.Attributes()),
		DroppedAttributesCount: clampUint32(s.DroppedAttributes()),
		DroppedEventsCount:     clampUint32(s.DroppedEvents()),
		DroppedLinksCount:      clampUint32(s.DroppedLinks()),
		Status:                 &tracepb.Status{Code: statusCode(st.Code), Message: st.Description},
	}
	if p := s.Parent().SpanID(); p.IsValid() {
		out.ParentSpanId = p[:]
	}
	for _, e := range s.Events() {
		out.Events = append(out.Events, &tracepb.Span_Event{
			Name:                   e.Name,
			TimeUnixNano:           unixNano(e.Time),
			Attributes:             keyValues(e.Attributes),
			DroppedAttributesCount: clampUint32(e.DroppedAttributeCount),
		})
	}
	for _, l := range s.Links() {
		lt, ls := l.SpanContext.TraceID(), l.SpanContext.SpanID()
		out.Links = append(out.Links, &tracepb.Span_Link{
			TraceId:                lt[:],
			SpanId:                 ls[:],
			TraceState:             l.SpanContext.TraceState().String(),
			Flags:                  spanFlags(l.SpanContext.TraceFlags(), l.SpanContext),
			Attributes:             keyValues(l.Attributes),
			DroppedAttributesCount: clampUint32(l.DroppedAttributeCount),
		})
	}
	return out
}

// spanFlags returns the OTLP flags: the W3C trace flags in the low byte,
// plus whether remote is known (always) and whether it is remote.
func spanFlags(tf trace.TraceFlags, remote trace.SpanContext) uint32 {
	f := uint32(tf) | uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_HAS_IS_REMOTE_MASK)
	if remote.IsRemote() {
		f |= uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_IS_REMOTE_MASK)
	}
	return f
}

// spanKind maps a span kind; the OTLP enum differs from the API's only in
// name.
func spanKind(k trace.SpanKind) tracepb.Span_SpanKind {
	switch k {
	case trace.SpanKindInternal:
		return tracepb.Span_SPAN_KIND_INTERNAL
	case trace.SpanKindServer:
		return tracepb.Span_SPAN_KIND_SERVER
	case trace.SpanKindClient:
		return tracepb.Span_SPAN_KIND_CLIENT
	case trace.SpanKindProducer:
		return tracepb.Span_SPAN_KIND_PRODUCER
	case trace.SpanKindConsumer:
		return tracepb.Span_SPAN_KIND_CONSUMER
	default:
		return tracepb.Span_SPAN_KIND_UNSPECIFIED
	}
}

// statusCode maps a status code. The numbering differs: the API has
// Unset, Error, Ok and OTLP has Unset, Ok, Error.
func statusCode(c codes.Code) tracepb.Status_StatusCode {
	switch c {
	case codes.Ok:
		return tracepb.Status_STATUS_CODE_OK
	case codes.Error:
		return tracepb.Status_STATUS_CODE_ERROR
	default:
		return tracepb.Status_STATUS_CODE_UNSET
	}
}

// unixNano returns t in nanoseconds since the epoch, 0 for times before it.
func unixNano(t time.Time) uint64 {
	return uint64(max(0, t.UnixNano())) //nolint:gosec // G115: clamped to non-negative
}

// clampUint32 clamps a count to the uint32 range of the OTLP fields.
func clampUint32(n int) uint32 {
	return uint32(min(max(n, 0), math.MaxUint32)) //nolint:gosec // G115: clamped
}

// keyValues converts attributes.
func keyValues(attrs []attribute.KeyValue) []*commonpb.KeyValue {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]*commonpb.KeyValue, len(attrs))
	for i, a := range attrs {
		out[i] = &commonpb.KeyValue{Key: string(a.Key), Value: anyValue(a.Value)}
	}
	return out
}

// iterValues converts the attributes of a set.
func iterValues(it attribute.Iterator) []*commonpb.KeyValue {
	if it.Len() == 0 {
		return nil
	}
	out := make([]*commonpb.KeyValue, 0, it.Len())
	for it.Next() {
		a := it.Attribute()
		out = append(out, &commonpb.KeyValue{Key: string(a.Key), Value: anyValue(a.Value)})
	}
	return out
}

// anyValue converts one attribute value, of any type.
func anyValue(v attribute.Value) *commonpb.AnyValue {
	switch v.Type() {
	case attribute.BOOL:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v.AsBool()}}
	case attribute.INT64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v.AsInt64()}}
	case attribute.FLOAT64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v.AsFloat64()}}
	case attribute.STRING:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v.AsString()}}
	case attribute.BYTESLICE:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: v.AsByteSlice()}}
	case attribute.BOOLSLICE:
		return arrayValue(v.AsBoolSlice(), attribute.BoolValue)
	case attribute.INT64SLICE:
		return arrayValue(v.AsInt64Slice(), attribute.Int64Value)
	case attribute.FLOAT64SLICE:
		return arrayValue(v.AsFloat64Slice(), attribute.Float64Value)
	case attribute.STRINGSLICE:
		return arrayValue(v.AsStringSlice(), attribute.StringValue)
	case attribute.SLICE:
		return arrayValue(v.AsSlice(), func(v attribute.Value) attribute.Value { return v })
	case attribute.MAP:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: keyValues(v.AsMap())}}}
	default: // attribute.EMPTY, or a type added later
		return &commonpb.AnyValue{}
	}
}

// arrayValue converts a slice attribute, element by element.
func arrayValue[T any](vs []T, value func(T) attribute.Value) *commonpb.AnyValue {
	arr := &commonpb.ArrayValue{Values: make([]*commonpb.AnyValue, len(vs))}
	for i, v := range vs {
		arr.Values[i] = anyValue(value(v))
	}
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: arr}}
}
