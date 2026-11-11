package sse

import (
	"bytes"
	"fmt"
	"io"
)

// doneData is the payload of the sentinel event that ends an OpenAI stream.
const doneData = "[DONE]"

// Flusher sends buffered output to the client. http.ResponseController's
// Flush method matches it.
type Flusher func() error

// Writer writes data events and flushes each one. It is not safe for
// concurrent use.
type Writer struct {
	w     io.Writer
	flush Flusher
	buf   bytes.Buffer
}

// NewWriter returns a Writer that writes events to w and calls flush after
// each one. A nil flush means w needs no flushing.
func NewWriter(w io.Writer, flush Flusher) *Writer {
	return &Writer{w: w, flush: flush}
}

// WriteData writes one event, "data: <payload>\n\n", and flushes it. A
// payload that spans several lines (separated by "\n", "\r\n" or "\r")
// becomes one data field per line, so the reader rebuilds it exactly with
// "\n" separators.
func (w *Writer) WriteData(data []byte) error {
	w.buf.Reset()
	for {
		i := bytes.IndexAny(data, "\r\n")
		if i < 0 {
			break
		}
		w.field(data[:i])
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			i++
		}
		data = data[i+1:]
	}
	w.field(data)
	w.buf.WriteByte('\n')
	return w.send()
}

// WriteDone writes the "data: [DONE]" event that ends an OpenAI stream.
func (w *Writer) WriteDone() error {
	return w.WriteData([]byte(doneData))
}

// WriteComment writes one comment line, ": <text>\n\n", and flushes it.
// Readers ignore comments; they keep idle connections alive. Line breaks
// in text are replaced with spaces, so the comment stays one line.
func (w *Writer) WriteComment(text string) error {
	w.buf.Reset()
	w.buf.WriteString(": ")
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '\r' || c == '\n' {
			c = ' '
		}
		w.buf.WriteByte(c)
	}
	w.buf.WriteString("\n\n")
	return w.send()
}

// IsDone reports whether data is the payload of the [DONE] sentinel.
func IsDone(data []byte) bool {
	return string(data) == doneData
}

func (w *Writer) field(value []byte) {
	w.buf.WriteString("data: ")
	w.buf.Write(value)
	w.buf.WriteByte('\n')
}

func (w *Writer) send() error {
	if _, err := w.w.Write(w.buf.Bytes()); err != nil {
		return fmt.Errorf("sse: write event: %w", err)
	}
	if w.flush != nil {
		if err := w.flush(); err != nil {
			return fmt.Errorf("sse: flush event: %w", err)
		}
	}
	return nil
}
