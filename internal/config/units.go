package config

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	nullTag  = "!!null"
	intTag   = "!!int"
	boolTag  = "!!bool"
	floatTag = "!!float"
)

// errDurationSyntax is returned for anything that is not a Go duration string.
var errDurationSyntax = errors.New(`expected a duration such as "60s"`)

// Std returns d as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String formats d as a Go duration string, such as "1m30s".
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalYAML encodes d as a Go duration string.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalYAML accepts a non-negative Go duration string such as "60s" or
// "2m30s". Bare numbers such as 60 are rejected because their unit would be
// ambiguous. A null scalar leaves d unchanged. Errors never include the value.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errDurationSyntax
	}
	if n.ShortTag() == nullTag {
		return nil
	}
	if _, err := strconv.ParseFloat(n.Value, 64); err == nil {
		return errDurationSyntax
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return errDurationSyntax
	}
	if v < 0 {
		return errors.New("duration must not be negative")
	}
	*d = Duration(v)
	return nil
}

// Byte size units. Decimal units are powers of 1000, binary units powers of
// 1024.
const (
	kb  = 1000
	mb  = 1000 * kb
	gb  = 1000 * mb
	kib = 1024
	mib = 1024 * kib
	gib = 1024 * mib
)

// byteUnits maps each accepted unit suffix to its size in bytes.
var byteUnits = map[string]int64{
	"B": 1, "KB": kb, "MB": mb, "GB": gb, "KiB": kib, "MiB": mib, "GiB": gib,
}

// formatUnits is the order String tries units in: binary first, then
// decimal, each from largest to smallest.
var formatUnits = []string{"GiB", "MiB", "KiB", "GB", "MB", "KB"}

// byteSizeRE matches an unsigned integer with an optional unit, separated by
// at most one space. The unit is validated against byteUnits separately so
// that an unknown unit gets its own message.
var byteSizeRE = regexp.MustCompile(`^([0-9]+)(?: ?([A-Za-z]+))?$`)

var errByteSizeSyntax = errors.New(`expected a byte size such as "10MiB" or a whole number of bytes`)

// Int64 returns b as a number of bytes.
func (b ByteSize) Int64() int64 { return int64(b) }

// String formats b with the largest unit that divides it exactly, preferring
// binary units: 10485760 is "10MiB", 1000 is "1KB" and 1500 is "1500B".
func (b ByteSize) String() string {
	v := int64(b)
	if v != 0 {
		for _, u := range formatUnits {
			size := byteUnits[u]
			if v%size == 0 {
				return strconv.FormatInt(v/size, 10) + u
			}
		}
	}
	return strconv.FormatInt(v, 10) + "B"
}

// MarshalYAML encodes b in the form String produces.
func (b ByteSize) MarshalYAML() (any, error) { return b.String(), nil }

// UnmarshalYAML accepts a non-negative whole number of bytes, or a whole
// number followed by a unit (B, KB, MB, GB, KiB, MiB or GiB, case-sensitive)
// with at most one space in between, such as "10MiB" or "64 KiB". Fractions,
// negative values, unknown units and values that overflow int64 are rejected.
// A null scalar leaves b unchanged. Errors never include the value.
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errByteSizeSyntax
	}
	if n.ShortTag() == nullTag {
		return nil
	}
	v, err := parseByteSize(n.Value)
	if err != nil {
		return err
	}
	*b = v
	return nil
}

// parseByteSize implements ByteSize.UnmarshalYAML for a scalar's text.
func parseByteSize(s string) (ByteSize, error) {
	m := byteSizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, errByteSizeSyntax
	}
	mult := int64(1)
	if m[2] != "" {
		u, ok := byteUnits[m[2]]
		if !ok {
			return 0, errors.New("unknown byte size unit; use B, KB, MB, GB, KiB, MiB or GiB")
		}
		mult = u
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n > math.MaxInt64/mult {
		return 0, errors.New("byte size is too large")
	}
	return ByteSize(n * mult), nil
}
