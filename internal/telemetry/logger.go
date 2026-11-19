// Package telemetry builds Switchyard's logger and metrics registry.
//
// NewLogger returns a JSON (or text) slog.Logger whose handler is wrapped in a
// redacting handler, so credentials never reach the log output even when a
// caller logs them by mistake. NewAsyncWriter takes the log write off the
// caller's path: the logger encodes and redacts each record as before, and
// the AsyncWriter queues the line for a background goroutine, dropping and
// counting lines when its bounded queue is full. NewRegistry returns a
// Prometheus registry with the process and Go runtime collectors and the
// build-info gauge.
package telemetry

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// LogOptions configures NewLogger.
type LogOptions struct {
	// Level is the minimum level logged. Nil means slog.LevelInfo. A
	// *slog.LevelVar may be passed so the level can change at runtime.
	Level slog.Leveler
	// Format is "json" (the default when empty) or "text". Any other value
	// falls back to json.
	Format string
	// Secrets are exact secret values to scrub from every record. Values
	// shorter than 8 characters are ignored.
	Secrets []string
}

// ParseLevel parses a log level name: debug, info, warn or error, in any
// case. Any other value is an error.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("telemetry: unknown log level %q (want debug, info, warn or error)", s)
	}
}

// NewLogger returns a logger that writes to w in the configured format,
// through a redacting handler that scrubs credentials and opts.Secrets.
// Pass an AsyncWriter as w to keep the write syscall off the caller's path;
// redaction happens before the line reaches w either way.
func NewLogger(w io.Writer, opts LogOptions) *slog.Logger {
	var level slog.Leveler = slog.LevelInfo
	if opts.Level != nil {
		level = opts.Level
	}
	hopts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	if opts.Format == "text" {
		h = slog.NewTextHandler(w, hopts)
	} else {
		h = slog.NewJSONHandler(w, hopts)
	}
	return slog.New(NewRedactingHandler(h, opts.Secrets))
}
