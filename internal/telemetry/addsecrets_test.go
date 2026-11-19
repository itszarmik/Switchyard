package telemetry

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestAddSecrets(t *testing.T) {
	const (
		oldSecret = "old-secret-value-111"
		newSecret = "new+secret<value>222" // JSON-escaped forms differ
		short     = "short7"
	)
	tests := []struct {
		name      string
		log       func(l *slog.Logger)
		wantGone  []string
		wantShown []string
	}{
		{
			name:     "a message",
			log:      func(l *slog.Logger) { l.Info("rotated to " + newSecret) },
			wantGone: []string{newSecret},
		},
		{
			name:     "an attr and an Any value, JSON-escaped",
			log:      func(l *slog.Logger) { l.Info("x", "note", newSecret, "v", map[string]string{"k": newSecret}) },
			wantGone: []string{"new+secret", `<value>`},
		},
		{
			name:     "the old value stays scrubbed",
			log:      func(l *slog.Logger) { l.Info("x", "note", oldSecret) },
			wantGone: []string{oldSecret},
		},
		{
			name:      "short values are ignored",
			log:       func(l *slog.Logger) { l.Info("x", "note", short) },
			wantShown: []string{short},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			root := NewLogger(&buf, LogOptions{Secrets: []string{oldSecret}})
			// Derived before the call: shares the set.
			derived := root.With("request_id", "r1").WithGroup("g")
			if !AddSecrets(root, []string{newSecret, short}) {
				t.Fatal("AddSecrets on a NewLogger logger = false")
			}
			for _, l := range []*slog.Logger{root, derived} {
				buf.Reset()
				tt.log(l)
				out := buf.String()
				for _, s := range tt.wantGone {
					if strings.Contains(out, s) {
						t.Errorf("%q appears after AddSecrets: %s", s, out)
					}
				}
				for _, s := range tt.wantShown {
					if !strings.Contains(out, s) {
						t.Errorf("%q was scrubbed: %s", s, out)
					}
				}
			}
		})
	}
	t.Run("before the call the new value is not known", func(t *testing.T) {
		var buf bytes.Buffer
		l := NewLogger(&buf, LogOptions{})
		l.Info("x", "note", newSecret)
		if !strings.Contains(buf.String(), "new+secret") {
			t.Fatalf("scrubbed before it was added: %s", buf.String())
		}
	})
	t.Run("a logger without the redacting handler", func(t *testing.T) {
		if AddSecrets(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), []string{newSecret}) {
			t.Fatal("AddSecrets on a plain logger = true")
		}
	})
}

// TestAddSecretsConcurrent runs AddSecrets while other goroutines log, for
// the race detector, and checks every value added is scrubbed afterwards.
func TestAddSecretsConcurrent(t *testing.T) {
	var out syncBuffer
	l := NewLogger(&out, LogOptions{})
	const rounds = 50
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := range 200 {
				l.Info("line", "w", w, "i", i, "note", "rotated-secret-00")
			}
		})
	}
	wg.Go(func() {
		for i := range rounds {
			AddSecrets(l, []string{fmt.Sprintf("rotated-secret-%02d", i)})
		}
	})
	wg.Wait()
	before := len(out.String())
	for i := range rounds {
		l.Info(fmt.Sprintf("rotated-secret-%02d", i))
	}
	if tail := out.String()[before:]; strings.Contains(tail, "rotated-secret") {
		t.Fatalf("an added value is not scrubbed: %s", tail)
	}
}
