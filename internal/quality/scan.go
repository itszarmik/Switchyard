package quality

import (
	"unicode/utf16"
	"unicode/utf8"
)

// This file holds a minimal, allocation-free reader for the raw JSON values
// the heuristic looks at (message content, tools, tool_choice,
// response_format). The request body has already been validated as JSON by
// schema.DecodeChatRequest, but every function here still bounds-checks and
// gives up (returning -1 or nothing) on malformed input rather than
// panicking.

// skipWS returns the index of the first non-whitespace byte at or after i.
func skipWS(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// lowerASCII lowercases an ASCII letter and returns any other byte as is.
func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// appendString decodes the JSON string that starts at b[i] (an opening
// quote), appending its text to dst with ASCII letters lowercased, but only
// while len(dst) < limit. It returns dst, the index just past the closing
// quote, or -1 if the string is malformed, and the length in bytes of the
// whole decoded text, limit or not (up to the malformation, if any).
// Scanning continues to the closing quote after the limit is reached, so
// callers can keep walking the surrounding value.
func appendString(dst, b []byte, i, limit int) ([]byte, int, int) {
	if i >= len(b) || b[i] != '"' {
		return dst, -1, 0
	}
	n := 0
	i++
	for i < len(b) {
		c := b[i]
		switch {
		case c == '"':
			return dst, i + 1, n
		case c != '\\':
			if len(dst) < limit {
				dst = append(dst, lowerASCII(c))
			}
			n++
			i++
			continue
		}
		// An escape sequence.
		if i+1 >= len(b) {
			return dst, -1, n
		}
		esc := b[i+1]
		i += 2
		var out byte
		switch esc {
		case 'n':
			out = '\n'
		case 't':
			out = '\t'
		case 'r':
			out = '\r'
		case 'b', 'f':
			out = ' '
		case '"', '\\', '/':
			out = esc
		case 'u':
			r, next := decodeUnicodeEscape(b, i)
			if next < 0 {
				return dst, -1, n
			}
			i = next
			if len(dst) < limit {
				if 'A' <= r && r <= 'Z' {
					r += 'a' - 'A'
				}
				dst = utf8.AppendRune(dst, r)
			}
			n += utf8.RuneLen(r)
			continue
		default:
			return dst, -1, n
		}
		if len(dst) < limit {
			dst = append(dst, out)
		}
		n++
	}
	return dst, -1, n
}

// decodeUnicodeEscape decodes the four hex digits at b[i:] of a \u escape,
// combining a UTF-16 surrogate pair when a second \u escape follows. It
// returns the rune (utf8.RuneError for an unpaired surrogate) and the index
// after the escape, or -1 if the digits are malformed.
func decodeUnicodeEscape(b []byte, i int) (rune, int) {
	r, ok := hex4(b, i)
	if !ok {
		return 0, -1
	}
	i += 4
	if !utf16.IsSurrogate(r) {
		return r, i
	}
	if i+6 <= len(b) && b[i] == '\\' && b[i+1] == 'u' {
		if r2, ok := hex4(b, i+2); ok {
			if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
				return dec, i + 6
			}
		}
	}
	return utf8.RuneError, i
}

// hex4 parses four hex digits at b[i:].
func hex4(b []byte, i int) (rune, bool) {
	if i+4 > len(b) {
		return 0, false
	}
	var r rune
	for _, c := range b[i : i+4] {
		r <<= 4
		switch {
		case '0' <= c && c <= '9':
			r |= rune(c - '0')
		case 'a' <= c && c <= 'f':
			r |= rune(c - 'a' + 10)
		case 'A' <= c && c <= 'F':
			r |= rune(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return r, true
}

// skipString returns the index just past the JSON string starting at b[i],
// or -1 if it is malformed.
func skipString(b []byte, i int) int {
	if i >= len(b) || b[i] != '"' {
		return -1
	}
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return -1
}

// skipValue returns the index just past the JSON value starting at b[i]
// (after any leading whitespace), or -1 if it is malformed.
func skipValue(b []byte, i int) int {
	i = skipWS(b, i)
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = skipString(b, i)
				if i < 0 {
					return -1
				}
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return -1
	default:
		// A number or a literal: up to the next delimiter.
		for i < len(b) {
			switch b[i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return i
			}
			i++
		}
		return i
	}
}

// rawStringBytes returns the bytes between the quotes of the JSON string at
// b[i], without unescaping, and the index past the closing quote. It is
// used for object keys and short enum values, which are compared verbatim.
func rawStringBytes(b []byte, i int) ([]byte, int) {
	end := skipString(b, i)
	if end < 0 {
		return nil, -1
	}
	return b[i+1 : end-1], end
}

// forEachMember calls fn for each member of the JSON object raw with the
// key's raw bytes and the index where the member's value starts. fn returns
// the index just past the value it consumed, or -1 to stop. forEachMember
// returns the index past the object, or -1 if raw is not an object or is
// malformed.
func forEachMember(raw []byte, i int, fn func(key []byte, at int) int) int {
	i = skipWS(raw, i)
	if i >= len(raw) || raw[i] != '{' {
		return -1
	}
	i = skipWS(raw, i+1)
	if i < len(raw) && raw[i] == '}' {
		return i + 1
	}
	for i < len(raw) {
		key, next := rawStringBytes(raw, skipWS(raw, i))
		if next < 0 {
			return -1
		}
		i = skipWS(raw, next)
		if i >= len(raw) || raw[i] != ':' {
			return -1
		}
		i = fn(key, skipWS(raw, i+1))
		if i < 0 {
			return -1
		}
		i = skipWS(raw, i)
		if i >= len(raw) {
			return -1
		}
		switch raw[i] {
		case ',':
			i++
		case '}':
			return i + 1
		default:
			return -1
		}
	}
	return -1
}

// forEachElement calls fn with the start index of each element of the JSON
// array at raw[i]. fn returns the index just past the element, or -1 to
// stop. It returns the number of elements visited.
func forEachElement(raw []byte, i int, fn func(at int) int) int {
	i = skipWS(raw, i)
	if i >= len(raw) || raw[i] != '[' {
		return 0
	}
	i = skipWS(raw, i+1)
	if i < len(raw) && raw[i] == ']' {
		return 0
	}
	n := 0
	for i < len(raw) {
		i = fn(skipWS(raw, i))
		if i < 0 {
			return n
		}
		n++
		i = skipWS(raw, i)
		if i >= len(raw) || raw[i] != ',' {
			return n
		}
		i++
	}
	return n
}

// countElements returns the number of elements of the JSON array raw, or
// 0 if raw is not an array.
func countElements(raw []byte) int {
	return forEachElement(raw, 0, func(at int) int { return skipValue(raw, at) })
}

// appendContent appends the text of a message content value to dst: the
// string itself, or the text members of an array of content parts, each
// followed by a newline. Text beyond limit is dropped. It also returns the
// length in bytes of all of the text, limit or not, without the newlines:
// only text counts, never other parts such as inline images.
func appendContent(dst, raw []byte, limit int) ([]byte, int) {
	i := skipWS(raw, 0)
	if i >= len(raw) {
		return dst, 0
	}
	total := 0
	switch raw[i] {
	case '"':
		dst, _, total = appendString(dst, raw, i, limit)
	case '[':
		forEachElement(raw, i, func(at int) int {
			if at >= len(raw) || raw[at] != '{' {
				return skipValue(raw, at)
			}
			return forEachMember(raw, at, func(key []byte, v int) int {
				if string(key) != "text" || v >= len(raw) || raw[v] != '"' {
					return skipValue(raw, v)
				}
				var next, n int
				dst, next, n = appendString(dst, raw, v, limit)
				total += n
				if next >= 0 && len(dst) < limit {
					dst = append(dst, '\n')
				}
				return next
			})
		})
	}
	return dst, total
}

// stringMember returns the raw bytes of the string member key of the JSON
// object raw, and false if raw is not an object or the member is absent or
// not a string. The bytes are not unescaped. A key that appears more than
// once yields its last value, as in encoding/json.
func stringMember(raw []byte, key string) ([]byte, bool) {
	var val []byte
	found := false
	forEachMember(raw, 0, func(k []byte, v int) int {
		if string(k) != key {
			return skipValue(raw, v)
		}
		if v < len(raw) && raw[v] == '"' {
			s, next := rawStringBytes(raw, v)
			val, found = s, next >= 0
			return next
		}
		val, found = nil, false
		return skipValue(raw, v)
	})
	return val, found
}
