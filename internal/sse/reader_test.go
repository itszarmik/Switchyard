package sse

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/google/go-cmp/cmp"
)

func TestReader(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []Event
	}{
		{
			name:  "openai stream",
			input: "data: {\"a\":1}\n\ndata: {\"b\":2}\n\ndata: [DONE]\n\n",
			want:  []Event{{Data: []byte(`{"a":1}`)}, {Data: []byte(`{"b":2}`)}, {Data: []byte("[DONE]")}},
		},
		{
			name:  "CRLF line endings",
			input: "data: x\r\n\r\ndata: y\r\n\r\n",
			want:  []Event{{Data: []byte("x")}, {Data: []byte("y")}},
		},
		{
			name:  "CR line endings",
			input: "data: x\r\rdata: y\r\r",
			want:  []Event{{Data: []byte("x")}, {Data: []byte("y")}},
		},
		{
			name:  "multi-line data joined with LF",
			input: "data: line1\ndata: line2\r\ndata:line3\n\n",
			want:  []Event{{Data: []byte("line1\nline2\nline3")}},
		},
		{
			name:  "comments are skipped",
			input: ": ping\n\n:\ndata: x\n: inside\n\n",
			want:  []Event{{Data: []byte("x")}},
		},
		{
			name:  "event type and id",
			input: "event: delta\nid: 7\ndata: x\n\ndata: y\n\n",
			want:  []Event{{Type: "delta", ID: "7", Data: []byte("x")}, {ID: "7", Data: []byte("y")}},
		},
		{
			name:  "id with NUL is ignored",
			input: "id: 1\ndata: a\n\nid: 2\x003\ndata: b\n\n",
			want:  []Event{{ID: "1", Data: []byte("a")}, {ID: "1", Data: []byte("b")}},
		},
		{
			name:  "only one leading space is stripped",
			input: "data:  two\n\n",
			want:  []Event{{Data: []byte(" two")}},
		},
		{
			name:  "field without colon",
			input: "data\n\n",
			want:  []Event{{Data: []byte("")}},
		},
		{
			name:  "empty data line",
			input: "data:\ndata:\n\n",
			want:  []Event{{Data: []byte("\n")}},
		},
		{
			name:  "event without data is not dispatched",
			input: "event: ping\n\nretry: 10\nfoo: bar\n\ndata: x\n\n",
			want:  []Event{{Data: []byte("x")}},
		},
		{
			name:  "event type resets after dispatch",
			input: "event: a\ndata: 1\n\ndata: 2\n\n",
			want:  []Event{{Type: "a", Data: []byte("1")}, {Data: []byte("2")}},
		},
		{
			name:  "leading BOM is stripped",
			input: "\xEF\xBB\xBFdata: x\n\n",
			want:  []Event{{Data: []byte("x")}},
		},
		{
			name:  "incomplete final event is discarded",
			input: "data: x\n\ndata: partial\n",
			want:  []Event{{Data: []byte("x")}},
		},
		{
			name:  "unterminated final line is discarded",
			input: "data: x\n\ndata: partial",
			want:  []Event{{Data: []byte("x")}},
		},
		{
			name:  "empty stream",
			input: "",
		},
		{
			name:  "colon inside value is kept",
			input: "data: {\"url\":\"http://x\"}\n\n",
			want:  []Event{{Data: []byte(`{"url":"http://x"}`)}},
		},
	}
	readers := map[string]func(string) io.Reader{
		"whole":    func(s string) io.Reader { return strings.NewReader(s) },
		"one byte": func(s string) io.Reader { return iotest.OneByteReader(strings.NewReader(s)) },
		"half":     func(s string) io.Reader { return iotest.HalfReader(strings.NewReader(s)) },
	}
	for _, tt := range tests {
		for rname, mk := range readers {
			t.Run(tt.name+"/"+rname, func(t *testing.T) {
				got, err := readAll(NewReader(mk(tt.input), ReaderOptions{}))
				if err != nil {
					t.Fatalf("readAll: %v", err)
				}
				if diff := cmp.Diff(tt.want, got, cmp.Comparer(bytes.Equal)); diff != "" {
					t.Errorf("events (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// TestReaderDispatchesWithoutWaiting checks that an event is returned as
// soon as its blank line arrives, even if the line ends in a lone "\r" and
// the next byte is not yet available.
func TestReaderDispatchesWithoutWaiting(t *testing.T) {
	for _, term := range []string{"\n\n", "\r\n\r\n", "\r\r"} {
		t.Run(strings.ReplaceAll(strings.ReplaceAll(term, "\r", "CR"), "\n", "LF"), func(t *testing.T) {
			pr, pw := io.Pipe()
			defer pr.Close()
			r := NewReader(pr, ReaderOptions{})
			go func() {
				// The pipe blocks until each write is read, and the writer
				// never closes it, so Next can only return what was written.
				_, _ = pw.Write([]byte("data: first" + term))
			}()
			ev, err := r.Next()
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if string(ev.Data) != "first" {
				t.Errorf("data = %q, want %q", ev.Data, "first")
			}
			_ = pw.Close()
		})
	}
}

func TestReaderLimits(t *testing.T) {
	tests := []struct {
		name  string
		input string
		max   int
		want  int // events before ErrEventTooLarge
	}{
		{name: "data too long", input: "data: 0123456789\n\n", max: 8},
		{name: "line too long", input: "data: " + strings.Repeat("x", 8+lineSlack) + "\n\n", max: 8},
		{name: "unterminated line too long", input: "data: " + strings.Repeat("x", 8+lineSlack), max: 8},
		{name: "data accumulates across lines", input: "data: 1234\ndata: 1234\ndata: 1234\n\n", max: 12},
		{name: "at the limit is fine", input: "data: 12345678\n\ndata: 123456789\n\n", max: 8, want: 1},
		{name: "comment too long", input: ": " + strings.Repeat("x", 8+lineSlack) + "\n\n", max: 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReader(iotest.HalfReader(strings.NewReader(tt.input)), ReaderOptions{MaxEventSize: tt.max})
			for i := range tt.want {
				if _, err := r.Next(); err != nil {
					t.Fatalf("event %d: %v", i, err)
				}
			}
			if _, err := r.Next(); !errors.Is(err, ErrEventTooLarge) {
				t.Fatalf("Next error = %v, want ErrEventTooLarge", err)
			}
			if _, err := r.Next(); !errors.Is(err, ErrEventTooLarge) {
				t.Errorf("error is not sticky: %v", err)
			}
		})
	}
}

func TestReaderReadError(t *testing.T) {
	boom := errors.New("boom")
	r := NewReader(io.MultiReader(strings.NewReader("data: x\n\ndata: y\n"), iotest.ErrReader(boom)), ReaderOptions{})
	if _, err := r.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if _, err := r.Next(); !errors.Is(err, boom) {
		t.Fatalf("second Next error = %v, want %v", err, boom)
	}
}

func TestReaderDefaultLimit(t *testing.T) {
	big := strings.Repeat("x", DefaultMaxEventSize)
	r := NewReader(strings.NewReader("data:"+big+"\n\n"), ReaderOptions{})
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next at the default limit: %v", err)
	}
	if len(ev.Data) != DefaultMaxEventSize {
		t.Errorf("len(data) = %d, want %d", len(ev.Data), DefaultMaxEventSize)
	}
	r = NewReader(strings.NewReader("data:"+big+"x\n\n"), ReaderOptions{})
	if _, err := r.Next(); !errors.Is(err, ErrEventTooLarge) {
		t.Errorf("Next past the default limit: error = %v, want ErrEventTooLarge", err)
	}
}

func readAll(r *Reader) ([]Event, error) {
	var out []Event
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
}

// referenceParse is a direct, non-incremental transcription of the WHATWG
// event stream interpretation algorithm, used as the oracle for fuzzing.
func referenceParse(input string) []Event {
	input = strings.TrimPrefix(input, "\xEF\xBB\xBF")
	var lines []string
	for {
		i := strings.IndexAny(input, "\r\n")
		if i < 0 {
			break // an unterminated last line never completes
		}
		lines = append(lines, input[:i])
		if input[i] == '\r' && i+1 < len(input) && input[i+1] == '\n' {
			i++
		}
		input = input[i+1:]
	}
	var (
		out       []Event
		data      strings.Builder
		hasData   bool
		eventType string
		lastID    string
	)
	for _, line := range lines {
		switch {
		case line == "":
			if hasData {
				d := strings.TrimSuffix(data.String(), "\n")
				out = append(out, Event{Type: eventType, Data: []byte(d), ID: lastID})
			}
			data.Reset()
			hasData = false
			eventType = ""
		case line[0] == ':':
		default:
			name, value, found := strings.Cut(line, ":")
			if found {
				value = strings.TrimPrefix(value, " ")
			}
			switch name {
			case "data":
				data.WriteString(value)
				data.WriteByte('\n')
				hasData = true
			case "event":
				eventType = value
			case "id":
				if !strings.Contains(value, "\x00") {
					lastID = value
				}
			}
		}
	}
	return out
}

func FuzzReader(f *testing.F) {
	seeds := []string{
		"data: {\"a\":1}\n\ndata: [DONE]\n\n",
		"data: a\r\ndata: b\r\n\r\n",
		"data: a\rdata: b\r\r",
		"\xEF\xBB\xBFevent: x\nid: 1\ndata: y\n\n",
		": comment\n\ndata\n\n",
		"data: x\n\ndata: partial",
		"id: a\x00b\ndata: c\n\n",
		"\r\n\r\r\n\n",
		"\xEF\xBB",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		want := referenceParse(string(input))
		for name, rd := range map[string]io.Reader{
			"whole":    bytes.NewReader(input),
			"one byte": iotest.OneByteReader(bytes.NewReader(input)),
		} {
			got, err := readAll(NewReader(rd, ReaderOptions{}))
			if err != nil {
				t.Fatalf("%s: readAll: %v", name, err)
			}
			if diff := cmp.Diff(want, got, cmp.Comparer(bytes.Equal)); diff != "" {
				t.Fatalf("%s: events differ from the reference (-want +got):\n%s", name, diff)
			}
		}

		// Every limit is enforced: a small limit yields only events
		// within it, then ErrEventTooLarge or the end of the stream.
		const small = 16
		r := NewReader(bytes.NewReader(input), ReaderOptions{MaxEventSize: small})
		for {
			ev, err := r.Next()
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, ErrEventTooLarge) {
					t.Fatalf("small limit: unexpected error %v", err)
				}
				break
			}
			if len(ev.Data) > small {
				t.Fatalf("small limit: event of %d bytes returned", len(ev.Data))
			}
		}
	})
}
