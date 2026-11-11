package telemetry

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{in: "debug", want: slog.LevelDebug},
		{in: "info", want: slog.LevelInfo},
		{in: "warn", want: slog.LevelWarn},
		{in: "error", want: slog.LevelError},
		{in: "DEBUG", want: slog.LevelDebug},
		{in: "Warn", want: slog.LevelWarn},
		{in: "", wantErr: true},
		{in: "warning", wantErr: true},
		{in: "trace", wantErr: true},
		{in: " info", wantErr: true},
		{in: "4", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseLevel(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseLevel(%q) = %v, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLevel(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ParseLevel(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNewLoggerFormats(t *testing.T) {
	tests := []struct {
		name     string
		format   string
		wantJSON bool
	}{
		{name: "empty is json", format: "", wantJSON: true},
		{name: "json", format: "json", wantJSON: true},
		{name: "text", format: "text", wantJSON: false},
		{name: "unknown falls back to json", format: "yaml", wantJSON: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := NewLogger(&buf, LogOptions{Format: tt.format, Secrets: []string{"supersecretvalue"}})
			l.Info("hello", "k", "v", "leak", "x-supersecretvalue-y")
			out := buf.String()

			if strings.Contains(out, "supersecretvalue") {
				t.Errorf("secret leaked: %s", out)
			}
			if tt.wantJSON {
				var m map[string]any
				if err := json.Unmarshal([]byte(out), &m); err != nil {
					t.Fatalf("output is not JSON: %v: %s", err, out)
				}
				if m["msg"] != "hello" || m["k"] != "v" || m["leak"] != "x-[REDACTED]-y" || m["level"] != "INFO" {
					t.Errorf("unexpected JSON record: %v", m)
				}
				return
			}
			for _, want := range []string{"level=INFO", "msg=hello", "k=v", "leak=x-[REDACTED]-y"} {
				if !strings.Contains(out, want) {
					t.Errorf("text output %q lacks %q", out, want)
				}
			}
		})
	}
}

func TestNewLoggerLevel(t *testing.T) {
	t.Run("nil level is info", func(t *testing.T) {
		var buf bytes.Buffer
		l := NewLogger(&buf, LogOptions{})
		l.Debug("hidden")
		l.Info("shown")
		if out := buf.String(); strings.Contains(out, "hidden") || !strings.Contains(out, "shown") {
			t.Fatalf("output = %q, want only the info record", out)
		}
	})
	t.Run("LevelVar changes at runtime", func(t *testing.T) {
		var buf bytes.Buffer
		var lv slog.LevelVar
		lv.Set(slog.LevelWarn)
		l := NewLogger(&buf, LogOptions{Level: &lv})
		l.Info("before")
		lv.Set(slog.LevelDebug)
		l.Debug("after")
		if out := buf.String(); strings.Contains(out, "before") || !strings.Contains(out, "after") {
			t.Fatalf("output = %q, want only the record logged after lowering the level", out)
		}
	})
}
