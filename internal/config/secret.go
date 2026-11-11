package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// redacted is what a set Secret prints instead of its value.
const redacted = "[REDACTED]"

// NewSecret returns a Secret holding v.
func NewSecret(v string) Secret {
	if v == "" {
		return Secret{}
	}
	return Secret{v: &v}
}

// Value returns the plaintext. It is the only way to read a Secret's
// contents; never pass the result to a logger or an error message.
func (s Secret) Value() string {
	if s.v == nil {
		return ""
	}
	return *s.v
}

// IsSet reports whether the secret holds a non-empty value.
func (s Secret) IsSet() bool { return s.Value() != "" }

// placeholder is what every printing and serializing method emits.
func (s Secret) placeholder() string {
	if !s.IsSet() {
		return ""
	}
	return redacted
}

// String returns "[REDACTED]" for a set secret and "" for an empty one.
func (s Secret) String() string { return s.placeholder() }

// GoString returns the same placeholder as String, so %#v never reveals the
// value.
func (s Secret) GoString() string { return s.placeholder() }

// Format implements fmt.Formatter so that every verb, including %v, %+v,
// %#v, %s, %q and %x, prints the placeholder rather than the value. %q quotes
// the placeholder; every other verb prints it as %s would, honoring width and
// alignment flags.
func (s Secret) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = fmt.Fprintf(f, fmt.FormatString(f, 'q'), s.placeholder())
		return
	}
	_, _ = fmt.Fprintf(f, fmt.FormatString(f, 's'), s.placeholder())
}

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.placeholder()) }

// MarshalJSON encodes the placeholder as a JSON string.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(s.placeholder()) }

// MarshalText returns the placeholder, so text encoders never see the value.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.placeholder()), nil }

// MarshalYAML encodes the placeholder as a YAML string.
func (s Secret) MarshalYAML() (any, error) { return s.placeholder(), nil }

// UnmarshalYAML accepts a scalar and keeps its text as the secret value. A
// null scalar leaves the secret empty. A scalar that YAML would resolve to
// another type, such as a key made only of digits, is still taken as text.
// Errors never include the value.
func (s *Secret) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errors.New("expected a string")
	}
	if n.ShortTag() == nullTag {
		*s = Secret{}
		return nil
	}
	*s = NewSecret(n.Value)
	return nil
}

// SecretValues returns the plaintext of every non-empty Secret in the config,
// without duplicates, for the log scrubber. It finds them by reflection, so it
// stays correct as fields are added.
func (c *Config) SecretValues() []string {
	if c == nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	walkSecrets(reflect.ValueOf(c).Elem(), "", func(_ string, s Secret) {
		if v := s.Value(); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	})
	return out
}

var secretType = reflect.TypeFor[Secret]()

// walkSecrets calls fn for every Secret reachable from v, with its config
// path built from the yaml tags (for example "providers[1].api_key").
func walkSecrets(v reflect.Value, path string, fn func(path string, s Secret)) {
	if v.Type() == secretType {
		fn(path, v.Interface().(Secret))
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			walkSecrets(v.Field(i), joinPath(path, yamlName(f)), fn)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			walkSecrets(v.Index(i), indexPath(path, i), fn)
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			walkSecrets(v.Elem(), path, fn)
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			walkSecrets(iter.Value(), joinPath(path, fmt.Sprint(iter.Key().Interface())), fn)
		}
	}
}

// joinPath appends a mapping key to a config path.
func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// indexPath appends a sequence index to a config path.
func indexPath(path string, i int) string {
	return path + "[" + strconv.Itoa(i) + "]"
}
