package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"switchyard/internal/config"
	"switchyard/internal/schema"
)

// exactKeyPrefix starts every exact-cache key; v1 is the key format.
const exactKeyPrefix = "sy:x:v1:"

// partitionDomain separates semantic partition hashes from any other hash
// of the same fields.
const partitionDomain = "sy:s:v1"

// excludedFields are the request fields that never affect the output, or
// are replaced: model by the resolved target, stream and stream_options
// because a hit is replayed in either form, and user, metadata, store and
// service_tier because they only label or bill the request.
var excludedFields = map[string]bool{
	"model":          true,
	"stream":         true,
	"stream_options": true,
	"user":           true,
	"metadata":       true,
	"store":          true,
	"service_tier":   true,
}

// ScopeID returns the scope string of a request, which every exact key and
// semantic partition starts with: "t:<tenant>" for the tenant scope (also
// for "" and any unknown scope, which config validation rejects),
// "t:<tenant>:k:<keyID>" for the key scope, and "t:<tenant>:u:<hex
// sha256(user)>" for the user scope, which falls back to the key scope
// when user is empty.
//
// Tenant and key IDs are written as they are when they follow the config
// ID rules ([a-z0-9_-]); any other byte is percent-escaped, so ':' never
// appears inside an ID and no two (tenant, key, user) triples produce the
// same string.
func ScopeID(scope, tenant, keyID, user string) string {
	s := "t:" + escapeID(tenant)
	switch scope {
	case config.CacheScopeKey:
		return s + ":k:" + escapeID(keyID)
	case config.CacheScopeUser:
		if user == "" {
			return s + ":k:" + escapeID(keyID)
		}
		sum := sha256.Sum256([]byte(user))
		return s + ":u:" + hex.EncodeToString(sum[:])
	default:
		return s
	}
}

// escapeID returns id with every byte outside [a-z0-9_-] written as %XX.
// The mapping is injective, and identity for valid config IDs.
func escapeID(id string) string {
	safe := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
	}
	n := 0
	for i := range len(id) {
		if !safe(id[i]) {
			n++
		}
	}
	if n == 0 {
		return id
	}
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(id) + 2*n)
	for i := range len(id) {
		c := id[i]
		if safe(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0xf])
	}
	return b.String()
}

// Canonical returns the canonical form of req for the resolved target (the
// catalog model ID, or "auto:<profile>@<policyID>" for auto): sorted-key
// JSON, recursively, with array order kept, of every request field except
// model, stream, stream_options, user, metadata, store and service_tier,
// plus "model" set to target. Unknown fields are included. Numbers keep
// their literal text, so large integers such as seeds never collide
// through float rounding. It fails if a field is not valid JSON or not
// valid UTF-8, or has a \u escape of a lone UTF-16 surrogate, since two
// different invalid strings could otherwise decode to the same value
// (encoding/json decodes every lone surrogate to U+FFFD) while upstreams
// receive them as sent.
func Canonical(req *schema.ChatRequest, target string) ([]byte, error) {
	m, err := canonicalMap(req, target)
	if err != nil {
		return nil, err
	}
	return encodeCanonical(m)
}

// canonicalMap decodes every canonical member of req, with model set to
// target.
func canonicalMap(req *schema.ChatRequest, target string) (map[string]any, error) {
	m := make(map[string]any)
	for name, raw := range req.Members() {
		if excludedFields[name] {
			continue
		}
		if !utf8.Valid(raw) {
			return nil, fmt.Errorf("cache: canonical form: field %q is not valid UTF-8", name)
		}
		if hasLoneSurrogate(raw) {
			return nil, fmt.Errorf("cache: canonical form: field %q has a lone UTF-16 surrogate escape", name)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("cache: canonical form: field %q: %w", name, err)
		}
		m[name] = v
	}
	m["model"] = target
	return m, nil
}

// hasLoneSurrogate reports whether the JSON text raw has a \u escape of a
// UTF-16 surrogate (U+D800 to U+DFFF) that is not a high surrogate
// followed at once by an escaped low one. Backslashes appear only in
// strings in valid JSON, so every one starts an escape.
func hasLoneSurrogate(raw []byte) bool {
	if !bytes.Contains(raw, []byte(`\u`)) {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		r, ok := escapedRune(raw[i:])
		switch {
		case !ok:
			i++ // a two-character escape, such as \" or \\
		case r >= 0xDC00 && r <= 0xDFFF:
			return true
		case r >= 0xD800 && r <= 0xDBFF:
			if low, ok := escapedRune(raw[i+6:]); !ok || low < 0xDC00 || low > 0xDFFF {
				return true
			}
			i += 11
		default:
			i += 5
		}
	}
	return false
}

// escapedRune returns the code unit of the \uXXXX escape b starts with.
func escapedRune(b []byte) (rune, bool) {
	if len(b) < 6 || b[0] != '\\' || b[1] != 'u' {
		return 0, false
	}
	v, err := strconv.ParseUint(string(b[2:6]), 16, 16)
	if err != nil {
		return 0, false
	}
	return rune(v), true
}

// encodeCanonical encodes v as JSON. encoding/json writes map keys sorted
// at every level and json.Number values verbatim; HTML escaping is off, so
// the form matches the request text.
func encodeCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("cache: canonical form: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// ExactKey returns the exact-cache key of a canonical request:
// sy:x:v1:<scope>:<nsver>:<hex sha256(canonical)>.
func ExactKey(scopeID string, nsver int64, canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return exactKeyPrefix + scopeID + ":" + strconv.FormatInt(nsver, 10) + ":" + hex.EncodeToString(sum[:])
}

// namespaceKey returns the state-Redis key that holds tenant's namespace
// version: sy:ns:v1:<tenant>, with the tenant escaped as in ScopeID.
func namespaceKey(tenant string) string {
	return "sy:ns:v1:" + escapeID(tenant)
}

// Partition returns the semantic partition of req and the text of its last
// user message, which is compared semantically. The partition is the hex
// SHA-256 of the scope, the namespace version, the target, and the
// canonical form (see Canonical) with the last message's content removed;
// the last message's other members (its role and name, say) stay in it and
// must match exactly.
//
// ok is false when req is not eligible for the semantic cache: the last
// message is not a user message; its content is not a string or text-only
// parts (which are joined with "\n"); the text is empty or longer than
// maxChars characters; any message has an image or audio part; n is set to
// anything but 1; or the canonical form fails.
func Partition(req *schema.ChatRequest, target, scopeID string, nsver int64, maxChars int) (partition, text string, ok bool) {
	msgs := req.Messages()
	if len(msgs) == 0 {
		return "", "", false
	}
	last := msgs[len(msgs)-1]
	if last.Role != "user" {
		return "", "", false
	}
	text, ok = messageText(last.Content)
	if !ok || strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > maxChars {
		return "", "", false
	}
	for _, m := range msgs {
		if hasMediaPart(m.Content) {
			return "", "", false
		}
	}
	if raw, present := req.Field("n"); present && string(raw) != "null" {
		if n, isInt := req.N(); !isInt || n != 1 {
			return "", "", false
		}
	}

	m, err := canonicalMap(req, target)
	if err != nil {
		return "", "", false
	}
	if err := dropLastContent(m); err != nil {
		return "", "", false
	}
	canonical, err := encodeCanonical(m)
	if err != nil {
		return "", "", false
	}
	h := sha256.New()
	for _, field := range []string{partitionDomain, scopeID, strconv.FormatInt(nsver, 10), target} {
		writeField(h, []byte(field))
	}
	writeField(h, canonical)
	return hex.EncodeToString(h.Sum(nil)), text, true
}

// writeField writes b to w with its length first, so that no two
// different field sequences hash the same bytes.
func writeField(w io.Writer, b []byte) {
	var n [binary.MaxVarintLen64]byte
	_, _ = w.Write(n[:binary.PutUvarint(n[:], uint64(len(b)))])
	_, _ = w.Write(b)
}

// errNoLastMessage reports a canonical map whose messages are not an array
// ending in an object.
var errNoLastMessage = errors.New("cache: the last message is not an object")

// dropLastContent replaces the last message in the canonical map m with a
// copy that has no content member.
func dropLastContent(m map[string]any) error {
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return errNoLastMessage
	}
	last, ok := msgs[len(msgs)-1].(map[string]any)
	if !ok {
		return errNoLastMessage
	}
	trimmed := make(map[string]any, len(last))
	for k, v := range last {
		if k != "content" {
			trimmed[k] = v
		}
	}
	out := make([]any, len(msgs))
	copy(out, msgs)
	out[len(out)-1] = trimmed
	m["messages"] = out
	return nil
}

// messageText returns a message content's text: the string itself, or
// the text of every part joined with "\n" when every part is a text part.
// Member names are matched exactly, as the upstream matches them.
func messageText(content json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s, true
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(content, &parts) != nil || len(parts) == 0 {
		return "", false
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		var typ, text string
		if json.Unmarshal(p["type"], &typ) != nil || typ != "text" || json.Unmarshal(p["text"], &text) != nil {
			return "", false
		}
		texts = append(texts, text)
	}
	return strings.Join(texts, "\n"), true
}

// mediaPartTypes are the content part types the semantic cache refuses:
// images and audio.
var mediaPartTypes = map[string]bool{"image_url": true, "input_audio": true}

// hasMediaPart reports whether content is an array with an image or audio
// part.
func hasMediaPart(content json.RawMessage) bool {
	var parts []map[string]json.RawMessage
	if json.Unmarshal(content, &parts) != nil {
		return false
	}
	for _, p := range parts {
		var typ string
		if json.Unmarshal(p["type"], &typ) == nil && mediaPartTypes[typ] {
			return true
		}
	}
	return false
}

// Deterministic reports whether req asks for a deterministic answer:
// temperature is 0, or a seed is set. A request without temperature is
// not deterministic, since providers default to 1.
func Deterministic(req *schema.ChatRequest) bool {
	if t, ok := req.Temperature(); ok && t == 0 {
		return true
	}
	_, ok := req.Seed()
	return ok
}
