package sse

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestWriter(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "single line", data: `{"a":1}`, want: "data: {\"a\":1}\n\n"},
		{name: "empty", data: "", want: "data: \n\n"},
		{name: "LF lines", data: "a\nb", want: "data: a\ndata: b\n\n"},
		{name: "CRLF lines", data: "a\r\nb", want: "data: a\ndata: b\n\n"},
		{name: "CR lines", data: "a\rb", want: "data: a\ndata: b\n\n"},
		{name: "trailing newline", data: "a\n", want: "data: a\ndata: \n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			flushes := 0
			w := NewWriter(&buf, func() error { flushes++; return nil })
			if err := w.WriteData([]byte(tt.data)); err != nil {
				t.Fatalf("WriteData: %v", err)
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("wrote %q, want %q", got, tt.want)
			}
			if flushes != 1 {
				t.Errorf("flushes = %d, want 1", flushes)
			}
		})
	}
}

func TestWriterDone(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, nil)
	if err := w.WriteDone(); err != nil {
		t.Fatalf("WriteDone: %v", err)
	}
	if got, want := buf.String(), "data: [DONE]\n\n"; got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}
	if !IsDone([]byte("[DONE]")) || IsDone([]byte(" [DONE]")) {
		t.Error("IsDone is wrong")
	}
}

type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

func TestWriterErrors(t *testing.T) {
	boom := errors.New("boom")
	if err := NewWriter(failWriter{boom}, nil).WriteData([]byte("x")); !errors.Is(err, boom) {
		t.Errorf("write error = %v, want %v", err, boom)
	}
	var buf bytes.Buffer
	if err := NewWriter(&buf, func() error { return boom }).WriteData([]byte("x")); !errors.Is(err, boom) {
		t.Errorf("flush error = %v, want %v", err, boom)
	}
}

// FuzzRoundTrip checks that whatever Writer writes, Reader reads back, with
// line breaks normalized to "\n".
func FuzzRoundTrip(f *testing.F) {
	for _, s := range []string{"", "x", "a\nb", "a\r\nb\rc", "\n\n", "[DONE]", "data: x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data string) {
		var buf bytes.Buffer
		if err := NewWriter(&buf, nil).WriteData([]byte(data)); err != nil {
			t.Fatalf("WriteData: %v", err)
		}
		evs, err := readAll(NewReader(&buf, ReaderOptions{}))
		if err != nil {
			t.Fatalf("readAll: %v", err)
		}
		if len(evs) != 1 {
			t.Fatalf("read %d events, want 1", len(evs))
		}
		want := strings.ReplaceAll(strings.ReplaceAll(data, "\r\n", "\n"), "\r", "\n")
		// A leading BOM is stripped by the reader only at the start of the
		// stream, which "data: " always precedes.
		if got := string(evs[0].Data); got != want {
			t.Errorf("round trip = %q, want %q", got, want)
		}
	})
}
