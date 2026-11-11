package config

import (
	"errors"
	"math"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// maxPriceDecimals is the most decimal places a Price may have: enough for
// a nano-dollar per million tokens.
const maxPriceDecimals = 9

// errPriceSyntax is returned for a value that is not a plain decimal.
var errPriceSyntax = errors.New(`expected a price in USD per 1M tokens, such as 0.15 or "0.0375"`)

// usdAmountMessage rewords a Price decoding error for a field that holds an
// amount of USD rather than a price per 1M tokens, such as a budget's
// limit_usd.
func usdAmountMessage(err error) string {
	if errors.Is(err, errPriceSyntax) {
		return `expected an amount in USD, such as 2000 or "12.50"`
	}
	return strings.Replace(err.Error(), "a price", "an amount in USD", 1)
}

// ParsePrice parses s as a Price: a non-negative decimal such as "3",
// "0.15" or ".5" with at most 9 decimal places. Signs, exponents, NaN,
// infinities, hexadecimal and digit separators are rejected. The empty
// string is rejected too; the zero Price means "not set". Errors never
// include s.
func ParsePrice(s string) (Price, error) {
	if strings.HasPrefix(s, "-") {
		return Price{}, errors.New("a price must not be negative")
	}
	lower := strings.ToLower(s)
	if strings.Contains(lower, "nan") || strings.Contains(lower, "inf") {
		return Price{}, errors.New("a price must be a finite decimal")
	}
	intPart, frac, hasDot := strings.Cut(s, ".")
	if !allDigits(intPart) || !allDigits(frac) || (intPart == "" && frac == "") || (hasDot && frac == "") {
		if strings.ContainsAny(s, "eE") && !strings.ContainsAny(lower, "x") {
			return Price{}, errors.New("a price must be written as a plain decimal, without an exponent")
		}
		return Price{}, errPriceSyntax
	}
	if len(frac) > maxPriceDecimals {
		return Price{}, errors.New("a price may have at most 9 decimal places")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) {
		return Price{}, errors.New("a price is too large")
	}
	return Price{text: s}, nil
}

// allDigits reports whether s consists only of ASCII digits. It is true for
// the empty string.
func allDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// IsSet reports whether the price was given.
func (p Price) IsSet() bool { return p.text != "" }

// Float64 returns the price as a float64, or 0 if it is not set. Use String
// where exact decimal arithmetic matters.
func (p Price) Float64() float64 {
	if p.text == "" {
		return 0
	}
	f, _ := strconv.ParseFloat(p.text, 64) // validated by ParsePrice
	return f
}

// String returns the price exactly as written in the config, such as
// "0.0375", or "" if it is not set.
func (p Price) String() string { return p.text }

// Equal reports whether p and q have the same text, so 1.5 and 1.50 are
// different, as they are to the config version. go-cmp uses this method.
func (p Price) Equal(q Price) bool { return p.text == q.text }

// MarshalYAML encodes the price as a YAML number with its exact text, or as
// null if it is not set.
func (p Price) MarshalYAML() (any, error) {
	if p.text == "" {
		return nil, nil
	}
	tag := "!!float"
	if !strings.Contains(p.text, ".") {
		tag = "!!int"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: p.text}, nil
}

// UnmarshalYAML accepts a YAML number or a string holding a decimal, as
// ParsePrice defines it, and keeps the exact text. A null scalar leaves the
// price unset. Errors never include the value.
func (p *Price) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errPriceSyntax
	}
	switch n.ShortTag() {
	case nullTag:
		*p = Price{}
		return nil
	case intTag, floatTag, "!!str":
	default:
		return errPriceSyntax
	}
	v, err := ParsePrice(n.Value)
	if err != nil {
		return err
	}
	*p = v
	return nil
}
