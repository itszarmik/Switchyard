package config

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

var priceType = reflect.TypeFor[Price]()

// fingerprint returns the first 12 hex characters of a SHA-256 over a
// canonical encoding of c. Secrets contribute a SHA-256 of their value, never
// the value itself.
func fingerprint(c *Config) string {
	h := sha256.New()
	writeCanonical(h, reflect.ValueOf(c).Elem())
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// writeCanonical writes an unambiguous encoding of v to h: every value is
// tagged with its kind, strings and collections are length-prefixed, struct
// fields are written in declaration order with their yaml names, and map
// entries in sorted key order. A nil slice or map is written as nil, not as
// empty: an absent list (such as a deployment's capabilities, which then
// come from the model) can mean something else than an empty one. A Price
// contributes its exact text.
func writeCanonical(h hash.Hash, v reflect.Value) {
	if v.Type() == secretType {
		s := v.Interface().(Secret)
		if !s.IsSet() {
			_, _ = h.Write([]byte("S0;"))
			return
		}
		sum := sha256.Sum256([]byte(s.Value()))
		_, _ = h.Write([]byte("S1" + hex.EncodeToString(sum[:]) + ";"))
		return
	}
	if v.Type() == priceType {
		// The exact text, so that 1.5 and 1.50 differ like any other edit.
		_, _ = h.Write([]byte("p"))
		writeString(h, v.Interface().(Price).String())
		return
	}
	if (v.Kind() == reflect.Slice || v.Kind() == reflect.Map) && v.IsNil() {
		_, _ = h.Write([]byte("n;"))
		return
	}
	switch v.Kind() {
	case reflect.Map:
		// Entries in sorted key order, so the encoding is deterministic.
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
		_, _ = h.Write([]byte("m" + strconv.Itoa(len(keys)) + ":"))
		for _, k := range keys {
			writeString(h, k.String())
			writeCanonical(h, v.MapIndex(k))
		}
		_, _ = h.Write([]byte(";"))
	case reflect.Struct:
		t := v.Type()
		_, _ = h.Write([]byte("{"))
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			writeString(h, yamlName(f))
			writeCanonical(h, v.Field(i))
		}
		_, _ = h.Write([]byte("}"))
	case reflect.Slice, reflect.Array:
		_, _ = h.Write([]byte("[" + strconv.Itoa(v.Len()) + ":"))
		for i := range v.Len() {
			writeCanonical(h, v.Index(i))
		}
		_, _ = h.Write([]byte("]"))
	case reflect.String:
		_, _ = h.Write([]byte("s"))
		writeString(h, v.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		_, _ = h.Write([]byte("i" + strconv.FormatInt(v.Int(), 10) + ";"))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		_, _ = h.Write([]byte("u" + strconv.FormatUint(v.Uint(), 10) + ";"))
	case reflect.Bool:
		_, _ = h.Write([]byte("b" + strconv.FormatBool(v.Bool()) + ";"))
	case reflect.Float32, reflect.Float64:
		_, _ = h.Write([]byte("f" + strconv.FormatFloat(v.Float(), 'g', -1, 64) + ";"))
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			_, _ = h.Write([]byte("n;"))
			return
		}
		writeCanonical(h, v.Elem())
	default:
		// Other kinds are not part of the schema. Fail loudly in tests
		// rather than silently hashing nothing.
		panic("config: fingerprint does not support " + v.Type().String())
	}
}

// writeString writes a length-prefixed string.
func writeString(h hash.Hash, s string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(s)) + ":" + s))
}
