// Package sse reads and writes Server-Sent Events (the text/event-stream
// format of the WHATWG HTML standard), as used by OpenAI-wire streaming.
//
// Reader parses an event stream incrementally: it dispatches each event as
// soon as its terminating blank line arrives, never waiting for more input.
// Writer emits data events and flushes each one to the client at once.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// DefaultMaxEventSize bounds an event's data when ReaderOptions leaves
// MaxEventSize at zero.
const DefaultMaxEventSize = 8 << 20 // 8 MiB

// lineSlack is how much longer than MaxEventSize a line may be, so that a
// data line carrying a maximum-size payload still fits with its field name.
const lineSlack = 64

// ErrEventTooLarge is returned when a line or an event's accumulated data
// exceeds the reader's maximum size. The stream cannot be resumed after it.
var ErrEventTooLarge = errors.New("sse: event exceeds the maximum size")

// bom is the UTF-8 byte order mark, which may start a stream.
var bom = []byte{0xEF, 0xBB, 0xBF}

// Event is one dispatched event.
type Event struct {
	// Type is the event field, or "" when the stream did not set one, which
	// the standard treats as "message".
	Type string
	// Data is the data payload: the values of the event's data fields,
	// joined with "\n". It is owned by the caller.
	Data []byte
	// ID is the last event ID seen so far in the stream, as the standard
	// defines it: it persists across events until changed.
	ID string
}

// ReaderOptions configures a Reader.
type ReaderOptions struct {
	// MaxEventSize bounds the size of one event's data, in bytes, and the
	// length of one line to that plus 64 bytes for the field name. Zero
	// means DefaultMaxEventSize.
	MaxEventSize int
}

// Reader parses an event stream. It is not safe for concurrent use.
type Reader struct {
	br      *bufio.Reader
	max     int // bounds an event's data
	maxLine int // bounds one line

	started bool // the leading BOM has been checked
	skipLF  bool // the previous line ended in "\r"; a following "\n" belongs to it
	line    []byte

	data      []byte
	hasData   bool
	eventType string
	lastID    string
	err       error // sticky error
}

// NewReader returns a Reader that reads the event stream from r.
func NewReader(r io.Reader, opts ReaderOptions) *Reader {
	limit := opts.MaxEventSize
	if limit <= 0 {
		limit = DefaultMaxEventSize
	}
	return &Reader{br: bufio.NewReaderSize(r, 32<<10), max: limit, maxLine: limit + lineSlack}
}

// Next returns the next event. Comments, retry fields and unknown fields
// are skipped, and so are events without data, as the standard specifies.
// At the end of the stream Next returns io.EOF; an event left incomplete
// by the end of the stream is discarded. Read errors other than io.EOF are
// returned as is, and ErrEventTooLarge when a limit is exceeded. Every
// error is sticky.
func (r *Reader) Next() (Event, error) {
	if r.err != nil {
		return Event{}, r.err
	}
	for {
		line, err := r.readLine()
		if err != nil {
			r.err = err
			return Event{}, err
		}
		if len(line) == 0 {
			if ev, ok := r.dispatch(); ok {
				return ev, nil
			}
			continue
		}
		if err := r.processField(line); err != nil {
			r.err = err
			return Event{}, err
		}
	}
}

// dispatch ends the current event. It reports false, after resetting the
// event, if the event had no data.
func (r *Reader) dispatch() (Event, bool) {
	defer func() {
		r.data = r.data[:0]
		r.hasData = false
		r.eventType = ""
	}()
	if !r.hasData {
		return Event{}, false
	}
	data := r.data
	// Every data line appended a "\n"; the standard removes the last one.
	data = data[:len(data)-1]
	return Event{Type: r.eventType, Data: bytes.Clone(data), ID: r.lastID}, true
}

// processField handles one non-empty line.
func (r *Reader) processField(line []byte) error {
	if line[0] == ':' {
		return nil // comment
	}
	name, value := line, []byte(nil)
	if i := bytes.IndexByte(line, ':'); i >= 0 {
		name, value = line[:i], line[i+1:]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
	}
	switch string(name) {
	case "data":
		if len(r.data)+len(value) > r.max {
			return ErrEventTooLarge
		}
		r.data = append(r.data, value...)
		r.data = append(r.data, '\n')
		r.hasData = true
	case "event":
		r.eventType = string(value)
	case "id":
		if bytes.IndexByte(value, 0) < 0 {
			r.lastID = string(value)
		}
	}
	// retry and unknown fields are ignored.
	return nil
}

// readLine returns the next line without its terminator. Lines end in
// "\r\n", "\n" or "\r". The returned slice is valid until the next call.
func (r *Reader) readLine() ([]byte, error) {
	r.line = r.line[:0]
	if r.skipLF {
		r.skipLF = false
		b, err := r.br.ReadByte()
		if err != nil {
			return nil, err
		}
		if b != '\n' {
			_ = r.br.UnreadByte() // cannot fail right after ReadByte
		}
	}
	if !r.started {
		r.started = true
		r.skipBOM()
	}
	for {
		if r.br.Buffered() == 0 {
			if _, err := r.br.Peek(1); err != nil {
				// Whatever was read of an unterminated line is discarded
				// along with the incomplete event.
				return nil, err
			}
		}
		buf, _ := r.br.Peek(r.br.Buffered())
		if i := bytes.IndexAny(buf, "\r\n"); i >= 0 {
			if len(r.line)+i > r.maxLine {
				return nil, ErrEventTooLarge
			}
			r.line = append(r.line, buf[:i]...)
			r.skipLF = buf[i] == '\r'
			_, _ = r.br.Discard(i + 1)
			return r.line, nil
		}
		if len(r.line)+len(buf) > r.maxLine {
			return nil, ErrEventTooLarge
		}
		r.line = append(r.line, buf...)
		_, _ = r.br.Discard(len(buf))
	}
}

// skipBOM discards a byte order mark at the start of the stream. It waits
// for more input only while what has arrived is a prefix of the mark.
func (r *Reader) skipBOM() {
	for {
		buf, _ := r.br.Peek(r.br.Buffered())
		if len(buf) >= len(bom) {
			if bytes.HasPrefix(buf, bom) {
				_, _ = r.br.Discard(len(bom))
			}
			return
		}
		if !bytes.HasPrefix(bom, buf) {
			return
		}
		if _, err := r.br.Peek(len(buf) + 1); err != nil {
			return // readLine reports the error
		}
	}
}
