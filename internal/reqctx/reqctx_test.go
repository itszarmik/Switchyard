package reqctx

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestRequestID(t *testing.T) {
	ctx := context.Background()
	if got := RequestID(ctx); got != "" {
		t.Fatalf("RequestID on empty context = %q, want empty", got)
	}
	ctx = WithRequestID(ctx, "0192f1a4-7c1e-7d2b-9a51-3c1f8e6b2a40")
	if got := RequestID(ctx); got != "0192f1a4-7c1e-7d2b-9a51-3c1f8e6b2a40" {
		t.Fatalf("RequestID = %q", got)
	}
}

func TestLoggerFallsBackToDefault(t *testing.T) {
	if Logger(context.Background()) == nil {
		t.Fatal("Logger on empty context returned nil")
	}
	if Logger(WithLogger(context.Background(), nil)) == nil {
		t.Fatal("Logger with a nil logger stored returned nil")
	}
}

func TestLoggerRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, nil)).With("request_id", "abc")
	Logger(WithLogger(context.Background(), l)).Info("hello")
	if !strings.Contains(buf.String(), `"request_id":"abc"`) {
		t.Fatalf("log line missing request_id: %s", buf.String())
	}
}

func TestAnnotate(t *testing.T) {
	t.Run("without a collector it does nothing", func(t *testing.T) {
		Annotate(context.Background(), slog.String("k", "v")) // must not panic
	})
	t.Run("collects in order across derived contexts", func(t *testing.T) {
		a := &Annotations{}
		ctx := WithAnnotations(context.Background(), a)
		Annotate(ctx, slog.String("tenant", "t1"))
		type key struct{}
		Annotate(context.WithValue(ctx, key{}, 1), slog.Int("status", 200), slog.Bool("stream", true))
		var got []string
		for _, attr := range a.Attrs() {
			got = append(got, attr.String())
		}
		if want := "tenant=t1 status=200 stream=true"; strings.Join(got, " ") != want {
			t.Errorf("attrs = %q, want %q", strings.Join(got, " "), want)
		}
	})
	t.Run("a nil collector does nothing", func(t *testing.T) {
		var a *Annotations
		Annotate(WithAnnotations(context.Background(), a), slog.String("k", "v"))
		if a.Attrs() != nil {
			t.Error("a nil collector has attrs")
		}
	})
}
