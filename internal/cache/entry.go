package cache

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// entryVersion is the first byte of an encoded Entry. Version 2 added
// DroppedParams. A reader that finds another version treats the entry as a
// miss (see ErrEntryVersion).
const entryVersion byte = 2

// errBadEntry reports an encoded entry that cannot be decoded.
var errBadEntry = errors.New("cache: malformed entry")

// ErrEntryVersion is wrapped by UnmarshalEntry's error for an entry encoded
// in another version, such as one stored before an upgrade. The stores
// treat such an entry as absent: a plain miss, not a failure.
var ErrEntryVersion = errors.New("cache: entry of another encoding version")

// MarshalEntry encodes e compactly for storage: the version byte, then the
// deployment and the model, each as a uvarint length and its bytes, then
// the number of dropped parameters as a uvarint and each as a uvarint
// length and its bytes, then Created as a varint of Unix nanoseconds (0
// for the zero time), then the body, which runs to the end.
func MarshalEntry(e *Entry) []byte {
	n := 1 + (4+len(e.DroppedParams))*binary.MaxVarintLen64 + len(e.Deployment) + len(e.Model) + len(e.Body)
	for _, p := range e.DroppedParams {
		n += len(p)
	}
	buf := make([]byte, 0, n)
	buf = append(buf, entryVersion)
	buf = appendString(buf, e.Deployment)
	buf = appendString(buf, e.Model)
	buf = binary.AppendUvarint(buf, uint64(len(e.DroppedParams)))
	for _, p := range e.DroppedParams {
		buf = appendString(buf, p)
	}
	var created int64
	if !e.Created.IsZero() {
		created = e.Created.UnixNano()
	}
	buf = binary.AppendVarint(buf, created)
	return append(buf, e.Body...)
}

// UnmarshalEntry decodes an entry encoded by MarshalEntry. The returned
// Body aliases data.
func UnmarshalEntry(data []byte) (*Entry, error) {
	switch {
	case len(data) == 0:
		return nil, fmt.Errorf("%w: empty", errBadEntry)
	case data[0] != entryVersion:
		return nil, fmt.Errorf("%w %d", ErrEntryVersion, data[0])
	}
	rest := data[1:]
	deployment, rest, ok := readString(rest)
	if !ok {
		return nil, fmt.Errorf("%w: deployment", errBadEntry)
	}
	model, rest, ok := readString(rest)
	if !ok {
		return nil, fmt.Errorf("%w: model", errBadEntry)
	}
	count, n := binary.Uvarint(rest)
	if n <= 0 {
		return nil, fmt.Errorf("%w: dropped parameters", errBadEntry)
	}
	rest = rest[n:]
	// Each parameter takes at least its length byte.
	if count > uint64(len(rest)) {
		return nil, fmt.Errorf("%w: dropped parameters", errBadEntry)
	}
	var dropped []string
	if count > 0 {
		dropped = make([]string, count)
	}
	for i := range dropped {
		if dropped[i], rest, ok = readString(rest); !ok {
			return nil, fmt.Errorf("%w: dropped parameters", errBadEntry)
		}
	}
	created, n := binary.Varint(rest)
	if n <= 0 {
		return nil, fmt.Errorf("%w: created", errBadEntry)
	}
	e := &Entry{Deployment: deployment, Model: model, DroppedParams: dropped, Body: rest[n:]}
	if created != 0 {
		e.Created = time.Unix(0, created).UTC()
	}
	return e, nil
}

// appendString appends s to b as a uvarint length and its bytes.
func appendString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// readString reads a uvarint length and that many bytes from b.
func readString(b []byte) (s string, rest []byte, ok bool) {
	l, n := binary.Uvarint(b)
	if n <= 0 {
		return "", nil, false
	}
	rest = b[n:]
	if l > uint64(len(rest)) {
		return "", nil, false
	}
	return string(rest[:l]), rest[l:], true
}
