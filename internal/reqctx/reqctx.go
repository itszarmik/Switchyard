// Package reqctx carries request-scoped values through context.Context: the
// gateway-assigned request ID, a logger that already includes it, and the
// annotations that end up on the request's access-log line. Every layer can
// then log with the request ID without importing the HTTP server.
package reqctx

import (
	"context"
	"log/slog"
	"sync"
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	loggerKey
	annotationsKey
)

// WithRequestID returns a copy of ctx that carries the gateway-assigned request ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request ID carried by ctx, or "" if there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// WithLogger returns a copy of ctx that carries l as the request-scoped logger.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// Logger returns the request-scoped logger carried by ctx. Outside a request
// it falls back to slog.Default(), so it never returns nil.
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// Annotations collects the attributes that the layers serving a request add
// to its one access-log line, such as the tenant or the completion's token
// counts. The HTTP server creates it, stores it with WithAnnotations and
// logs Attrs once the request is done. It is safe for concurrent use.
type Annotations struct {
	mu    sync.Mutex
	attrs []slog.Attr
}

// WithAnnotations returns a copy of ctx that carries a, so that Annotate
// adds to it.
func WithAnnotations(ctx context.Context, a *Annotations) context.Context {
	return context.WithValue(ctx, annotationsKey, a)
}

// Annotate adds attrs to the access-log line of the request ctx belongs to.
// Outside a request, with no Annotations in ctx, it does nothing.
func Annotate(ctx context.Context, attrs ...slog.Attr) {
	a, _ := ctx.Value(annotationsKey).(*Annotations)
	if a == nil {
		return
	}
	a.mu.Lock()
	a.attrs = append(a.attrs, attrs...)
	a.mu.Unlock()
}

// Attrs returns the attributes added so far, in the order they were added.
// The slice must not be modified.
func (a *Annotations) Attrs() []slog.Attr {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.attrs
}
