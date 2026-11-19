package telemetry

import (
	"bytes"
	"cmp"
	"context"
	"encoding"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// Redacted replaces every secret value the redacting handler removes.
const Redacted = "[REDACTED]"

// minSecretLen is the shortest exact secret value that is scrubbed. Shorter
// values would match too much ordinary text and mangle the logs.
const minSecretLen = 8

// panicked replaces a value whose String, Error or MarshalJSON method
// panicked, since the panic message may contain anything.
const panicked = "!PANIC"

// Bounds on the reflection walk that looks for sensitive keys inside
// slog.Any values. Deeper or larger values are not inspected further, which
// also stops the walk on reference cycles.
const (
	maxWalkDepth = 32
	maxWalkNodes = 10000
)

// credentialPattern is a credential-shaped substring and its replacement.
type credentialPattern struct {
	re   *regexp.Regexp
	repl string
}

// credentialPatterns are compiled once and never modified.
var credentialPatterns = [...]credentialPattern{
	// RFC 6750 bearer tokens: "Bearer" in any case, whitespace, then token68.
	{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9\-._~+/]{8,}=*`), "Bearer " + Redacted},
	// OpenAI and Anthropic keys (sk-…, sk-proj-…, sk-ant-…).
	{regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`), Redacted},
	// Switchyard gateway keys.
	{regexp.MustCompile(`sy_[A-Za-z0-9_-]{20,}`), Redacted},
	// Google API keys.
	{regexp.MustCompile(`AIza[0-9A-Za-z_-]{30,}`), Redacted},
	// The password in URL userinfo: scheme://user:PASSWORD@. The password
	// match is greedy so an unescaped "@" inside it cannot leak a suffix.
	{regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://[^\s:/?#@"]*):[^\s/?#"]+@`), "${1}:" + Redacted + "@"},
}

// NewRedactingHandler returns a handler that scrubs credentials from every
// record before passing it to next. It redacts:
//
//   - the whole value of attrs (and groups) with a sensitive key, such as
//     authorization, api_key or password; keys are compared lowercased with
//     "-", "_" and "." removed, and keys merely ending in "token" (input_tokens,
//     first_token) are left alone;
//   - inside slog.Any values, the values of map entries and struct fields
//     whose key, field name or json tag is sensitive, such as the
//     Authorization entry of an http.Header; such a value is replaced by its
//     JSON encoding with those entries redacted;
//   - credential-shaped substrings (bearer tokens, sk-…, sy_…, AIza… keys and
//     URL passwords) in the message, string values, error texts, Stringer
//     output and the JSON encoding of other values;
//   - every occurrence of the given secret values that are at least 8 bytes
//     long, including their JSON-escaped forms.
//
// A value whose String, Error or MarshalJSON method panics is replaced by
// "!PANIC", since the panic message could contain a secret.
//
// Attrs added with WithAttrs are scrubbed once, up front. A record with
// nothing to redact is passed through unchanged without extra allocations,
// except that values of kind slog.KindAny other than errors are encoded so
// they can be inspected.
func NewRedactingHandler(next slog.Handler, secrets []string) slog.Handler {
	return &redactingHandler{next: next, r: newRedactor(secrets)}
}

type redactingHandler struct {
	next slog.Handler
	r    *redactor
	// redactAll is set once a group with a sensitive name has been opened
	// with WithGroup: everything logged inside it is redacted.
	redactAll bool
}

// Enabled reports whether the inner handler handles records at level.
func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle scrubs r and passes it on. It never modifies r: when something must
// be redacted it builds a new record.
func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	msg, msgChanged := h.r.scrub(r.Message)

	// Find the first attr that changes, if any.
	first := -1
	var firstAttr slog.Attr
	i := 0
	r.Attrs(func(a slog.Attr) bool {
		if s, changed := h.scrubAttr(a); changed {
			first, firstAttr = i, s
			return false
		}
		i++
		return true
	})
	if !msgChanged && first < 0 {
		return h.next.Handle(ctx, r)
	}

	nr := slog.NewRecord(r.Time, r.Level, msg, r.PC)
	if first >= 0 {
		i = 0
		r.Attrs(func(a slog.Attr) bool {
			switch {
			case i < first:
				nr.AddAttrs(a)
			case i == first:
				nr.AddAttrs(firstAttr)
			default:
				s, _ := h.scrubAttr(a)
				nr.AddAttrs(s)
			}
			i++
			return true
		})
	} else {
		r.Attrs(func(a slog.Attr) bool {
			nr.AddAttrs(a)
			return true
		})
	}
	return h.next.Handle(ctx, nr)
}

// AddSecrets adds exact secret values to the set scrubbed by the redacting
// handler behind l, and by every logger derived from it with With or
// WithGroup, which share the set. Values already in the set stay in it:
// lines about requests still running on an older configuration may carry
// the values it held. It reports false, and does nothing, when l's handler
// is not one made by NewRedactingHandler (or NewLogger). Values shorter
// than 8 characters are ignored, as at construction. It is safe to call
// while logging; a line being scrubbed concurrently uses the set as it was
// when that line's scrubbing began.
//
// Attrs added with WithAttrs before the call were scrubbed against the old
// set and are not scrubbed again.
func AddSecrets(l *slog.Logger, values []string) bool {
	h, ok := l.Handler().(*redactingHandler)
	if !ok {
		return false
	}
	h.r.add(values)
	return true
}

// WithAttrs scrubs attrs once and passes them to the inner handler.
func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i], _ = h.scrubAttr(a)
	}
	return &redactingHandler{next: h.next.WithAttrs(out), r: h.r, redactAll: h.redactAll}
}

// WithGroup opens a group on the inner handler, preserving the structure.
func (h *redactingHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &redactingHandler{
		next:      h.next.WithGroup(name),
		r:         h.r,
		redactAll: h.redactAll || isSensitiveKey(name),
	}
}

// scrubAttr applies the handler's rules to a.
func (h *redactingHandler) scrubAttr(a slog.Attr) (slog.Attr, bool) {
	if h.redactAll {
		if a.Equal(slog.Attr{}) {
			return a, false
		}
		return slog.String(a.Key, Redacted), true
	}
	return h.r.scrubAttr(a)
}

// redactor holds the exact secret values and applies the redaction rules.
// It is safe for concurrent use. The secret set only grows, through add,
// which publishes a new immutable slice; scrub reads it with one atomic
// load.
type redactor struct {
	secrets atomic.Pointer[[]string]
	mu      sync.Mutex // serializes add
}

func newRedactor(secrets []string) *redactor {
	r := &redactor{}
	kept := secretForms(nil, secrets)
	r.secrets.Store(&kept)
	return r
}

// add adds secrets to the set, keeping the ones it already has.
func (r *redactor) add(secrets []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := secretForms(*r.secrets.Load(), secrets)
	r.secrets.Store(&kept)
}

// secretForms returns a new slice holding have plus every secret at least
// minSecretLen long and its JSON-escaped forms, without duplicates, longest
// first.
func secretForms(have, secrets []string) []string {
	kept := slices.Clone(have)
	for _, s := range secrets {
		if len(s) < minSecretLen {
			continue
		}
		// The JSON encoding of a composite value escapes characters such as
		// '"', '\', control characters and, by default, '&', '<' and '>', so the
		// escaped forms must be scrubbed too.
		for _, form := range [...]string{s, jsonEscape(s, true), jsonEscape(s, false)} {
			if !slices.Contains(kept, form) {
				kept = append(kept, form)
			}
		}
	}
	// Replace longer secrets first, so a secret that contains another is
	// removed whole.
	slices.SortFunc(kept, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	return kept
}

// jsonEscape returns s as it appears inside a JSON string, with or without
// HTML escaping.
func jsonEscape(s string, escapeHTML bool) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(escapeHTML)
	if err := enc.Encode(s); err != nil {
		return s // unreachable: a string always encodes
	}
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	return string(out[1 : len(out)-1])
}

// scrub returns s with secrets and credential patterns replaced, and whether
// anything was replaced. It does not allocate when there is nothing to do.
func (r *redactor) scrub(s string) (string, bool) {
	changed := false
	for _, secret := range *r.secrets.Load() {
		if strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, Redacted)
			changed = true
		}
	}
	if mayContainCredential(s) {
		for _, p := range credentialPatterns {
			if p.re.MatchString(s) {
				s = p.re.ReplaceAllString(s, p.repl)
				changed = true
			}
		}
	}
	return s, changed
}

// scrubAttr applies the key, pattern and secret rules to a, recursing into
// groups and resolving LogValuers. It reports whether a changed.
func (r *redactor) scrubAttr(a slog.Attr) (slog.Attr, bool) {
	if a.Key != "" && isSensitiveKey(a.Key) {
		return slog.String(a.Key, Redacted), true
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		if s, changed := r.scrub(v.String()); changed {
			return slog.String(a.Key, s), true
		}
	case slog.KindGroup:
		if g, changed := r.scrubGroup(v.Group()); changed {
			return slog.Attr{Key: a.Key, Value: slog.GroupValue(g...)}, true
		}
	case slog.KindAny:
		if s, changed := r.scrubAny(v.Any()); changed {
			return slog.String(a.Key, s), true
		}
	}
	return a, false
}

// scrubGroup scrubs the attrs of a group. It returns attrs itself when
// nothing changed, and a new slice otherwise.
func (r *redactor) scrubGroup(attrs []slog.Attr) ([]slog.Attr, bool) {
	for i, a := range attrs {
		s, changed := r.scrubAttr(a)
		if !changed {
			continue
		}
		out := make([]slog.Attr, len(attrs))
		copy(out, attrs[:i])
		out[i] = s
		for j := i + 1; j < len(attrs); j++ {
			out[j], _ = r.scrubAttr(attrs[j])
		}
		return out, true
	}
	return attrs, false
}

// scrubAny inspects a slog.KindAny value. When the value contains something
// to redact it returns the text that should replace it:
//
//   - the error text for errors, and the text of byte slices;
//   - the JSON encoding, with the sensitive entries redacted, for values
//     holding a map entry or struct field with a sensitive key;
//   - the String output for Stringers;
//   - otherwise the JSON encoding (what the JSON handler prints), or the fmt
//     rendering (what the text handler prints) when only that one changed or
//     the value cannot be encoded as JSON.
//
// Every replacement is scrubbed for secrets and credential patterns.
func (r *redactor) scrubAny(x any) (replacement string, changed bool) {
	if x == nil {
		return "", false
	}
	if err, ok := x.(error); ok {
		return r.scrubSafely(x, err.Error)
	}
	rv := reflect.ValueOf(x)
	if isByteSlice(rv.Type()) {
		if s, changed := r.scrub(string(rv.Bytes())); changed {
			return s, true
		}
	}

	// The inner handler would call a panicking String or MarshalJSON method
	// again and print the panic message, so replace the value instead.
	defer func() {
		if recover() != nil {
			replacement, changed = panicText(rv), true
		}
	}()

	if (&keyWalker{}).sensitive(rv, 0) {
		return r.redactKeys(rv), true
	}

	var text string
	stringer, isStringer := x.(fmt.Stringer)
	if isStringer {
		text = stringer.String()
	} else {
		text = fmt.Sprintf("%+v", x)
	}
	scrubbedText, textChanged := r.scrub(text)
	// fmt prints a panic in a nested String method, message included.
	textPanicked := !isStringer && strings.Contains(text, "(PANIC=")

	enc, err := json.Marshal(x)
	var scrubbedJSON string
	var jsonChanged bool
	if err == nil {
		scrubbedJSON, jsonChanged = r.scrub(string(enc))
	}

	switch {
	case !textChanged && !jsonChanged && !textPanicked:
		return "", false
	case isStringer:
		return scrubbedText, true
	case err != nil && textPanicked:
		return panicked, true
	case err != nil:
		return scrubbedText, true
	case jsonChanged || textPanicked:
		return scrubbedJSON, true
	default:
		// The text changed but the JSON did not: the JSON may hold the
		// secret in a form scrub does not recognize, so use the text.
		return scrubbedText, true
	}
}

// scrubSafely scrubs the output of text, replacing x with "!PANIC" (or
// "<nil>" for a nil pointer, as slog prints it) if text panics.
func (r *redactor) scrubSafely(x any, text func() string) (s string, changed bool) {
	defer func() {
		if recover() != nil {
			s, changed = panicText(reflect.ValueOf(x)), true
		}
	}()
	return r.scrub(text())
}

// panicText is what replaces a value whose method panicked. slog prints a
// nil pointer whose method panics as "<nil>", which reveals nothing.
func panicText(v reflect.Value) string {
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return "<nil>"
	}
	return panicked
}

// isByteSlice reports whether t is a slice of bytes, including named types
// such as json.RawMessage.
func isByteSlice(t reflect.Type) bool {
	return t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8
}

// keyWalker looks for map entries and struct fields with a sensitive key
// that hold a non-zero value. It counts the nodes it visits so that huge
// or cyclic values cannot make it run for long.
type keyWalker struct {
	nodes int
}

// sensitive reports whether v holds a sensitive entry at any depth. It also
// looks at unexported struct fields, which the text handler prints.
func (w *keyWalker) sensitive(v reflect.Value, depth int) bool {
	w.nodes++
	if depth > maxWalkDepth || w.nodes > maxWalkNodes || !v.IsValid() {
		return false
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && w.sensitive(v.Elem(), depth+1)
	case reflect.Map:
		stringKeys := v.Type().Key().Kind() == reflect.String
		iter := v.MapRange()
		for iter.Next() {
			val := iter.Value()
			if stringKeys && isSensitiveKey(iter.Key().String()) && !val.IsZero() {
				return true
			}
			if w.sensitive(val, depth+1) {
				return true
			}
		}
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			f, fv := t.Field(i), v.Field(i)
			if (isSensitiveKey(f.Name) || isSensitiveKey(jsonName(f))) && !fv.IsZero() {
				return true
			}
			if w.sensitive(fv, depth+1) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return false
		}
		for i := range v.Len() {
			if w.sensitive(v.Index(i), depth+1) {
				return true
			}
		}
	}
	return false
}

// redactKeys returns the scrubbed JSON encoding of v with the value of every
// sensitive map entry and struct field replaced by Redacted.
func (r *redactor) redactKeys(v reflect.Value) string {
	tree := (&keyWalker{}).redacted(v, 0)
	enc, err := json.Marshal(tree)
	if err != nil {
		// Values JSON cannot encode, such as channels, print as addresses.
		s, _ := r.scrub(fmt.Sprintf("%+v", tree))
		return s
	}
	s, _ := r.scrub(string(enc))
	return s
}

var (
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// redacted converts v into a value that encodes to the same JSON as v, except
// that sensitive entries hold Redacted. Values with their own JSON or text
// encoding are kept as they are. Unexported struct fields are left out, as
// encoding/json leaves them out.
func (w *keyWalker) redacted(v reflect.Value, depth int) any {
	w.nodes++
	if depth > maxWalkDepth || w.nodes > maxWalkNodes || !v.IsValid() || !v.CanInterface() {
		return nil
	}
	if t := v.Type(); t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType) {
		if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil() {
			return nil
		}
		return v.Interface()
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return w.redacted(v.Elem(), depth+1)
	case reflect.Map:
		if v.IsNil() {
			return nil
		}
		out := make(map[string]any, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			k := iter.Key()
			var key string
			if k.Kind() == reflect.String {
				key = k.String()
			} else {
				key = fmt.Sprint(k.Interface())
			}
			if isSensitiveKey(key) {
				out[key] = Redacted
			} else {
				out[key] = w.redacted(iter.Value(), depth+1)
			}
		}
		return out
	case reflect.Struct:
		out := make(map[string]any)
		w.structFields(v, depth, out)
		return out
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return nil
		}
		if v.Type().Elem().Kind() == reflect.Uint8 && v.Kind() == reflect.Slice {
			return string(v.Bytes())
		}
		out := make([]any, v.Len())
		for i := range v.Len() {
			out[i] = w.redacted(v.Index(i), depth+1)
		}
		return out
	default:
		return v.Interface()
	}
}

// structFields adds the exported fields of struct v to out under their JSON
// names, flattening embedded structs as encoding/json does.
func (w *keyWalker) structFields(v reflect.Value, depth int, out map[string]any) {
	t := v.Type()
	for i := range t.NumField() {
		f, fv := t.Field(i), v.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == "-" {
			continue
		}
		if f.Anonymous && tag == "" {
			if fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					continue
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				w.structFields(fv, depth+1, out)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		name := jsonName(f)
		if isSensitiveKey(f.Name) || isSensitiveKey(name) {
			out[name] = Redacted
			continue
		}
		out[name] = w.redacted(fv, depth+1)
	}
}

// jsonName returns the key encoding/json writes a struct field under.
func jsonName(f reflect.StructField) string {
	if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
		return name
	}
	return f.Name
}

// mayContainCredential is a cheap, allocation-free prefilter for the
// credential patterns: a string that fails it cannot match any of them.
func mayContainCredential(s string) bool {
	return strings.Contains(s, "sk-") ||
		strings.Contains(s, "sy_") ||
		strings.Contains(s, "AIza") ||
		strings.Contains(s, "://") ||
		containsBearer(s)
}

// containsBearer reports whether s contains "bearer" in any ASCII case.
func containsBearer(s string) bool {
	for i := 0; i+len("bearer") <= len(s); i++ {
		if s[i]|0x20 == 'b' && strings.EqualFold(s[i:i+len("bearer")], "bearer") {
			return true
		}
	}
	return false
}

// isSensitiveKey reports whether an attr key names a value that must be
// redacted whole. The key is compared lowercased, without "-", "_" and ".".
func isSensitiveKey(key string) bool {
	var buf [64]byte
	k := normalizeKey(buf[:0], key)
	switch string(k) {
	case "authorization", "proxyauthorization", "cookie", "setcookie",
		"apikey", "xapikey", "password", "passwd", "secret", "clientsecret",
		"token", "accesstoken", "refreshtoken", "idtoken", "bearertoken",
		"authtoken", "xauthtoken", "dsn", "privatekey":
		return true
	}
	// Deliberately no "token" suffix: input_tokens and first_token are not
	// secrets.
	for _, suffix := range [...]string{
		"apikey", "secret", "password", "passwd", "privatekey",
		"secretkey", "accesskey", "signingkey", "sessiontoken",
		"credential", "credentials", "passphrase", "databaseurl",
	} {
		if len(k) >= len(suffix) && string(k[len(k)-len(suffix):]) == suffix {
			return true
		}
	}
	return false
}

// normalizeKey appends key to dst, ASCII-lowercased and without "-", "_"
// and ".".
func normalizeKey(dst []byte, key string) []byte {
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c == '-' || c == '_' || c == '.':
			continue
		case 'A' <= c && c <= 'Z':
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	return dst
}
