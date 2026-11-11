package usage

import (
	"errors"
	"math"
	"math/big"
	"strings"
)

// femtoDigits is the number of decimal places Money keeps: amounts are whole
// femto-USD (1e-15 USD), the scale of the ledger's NUMERIC(38,15) columns.
// Prices have at most 9 decimals per million tokens and token counts are
// integers, so every cost is a whole number of femto-USD.
const femtoDigits = 15

// femtoPerMicro is the number of femto-USD in one micro-USD.
const femtoPerMicro = 1_000_000_000

// Money is an exact amount of USD, held as a whole number of femto-USD
// (1e-15 USD). It is an immutable value: every operation returns a new
// Money and never changes its operands, so Money is safe to copy and to
// share between goroutines. The zero value is 0 USD.
type Money struct {
	// femto is nil for zero and is never modified after construction.
	femto *big.Int
}

// errMoneySyntax is returned by ParseMoney for text that is not a plain
// decimal.
var errMoneySyntax = errors.New("usage: expected a decimal amount of USD such as 12.5 or -0.000001")

// ParseMoney parses a plain decimal such as "12.5", "-0.000001" or "0", with
// an optional leading minus sign and at most 15 significant decimal places
// (trailing zeros beyond that are accepted). Exponents, a leading plus
// sign, digit separators and the empty string are rejected. Errors never
// include s.
func ParseMoney(s string) (Money, error) {
	neg := strings.HasPrefix(s, "-")
	body := strings.TrimPrefix(s, "-")
	intPart, frac, hasDot := strings.Cut(body, ".")
	if !allDigits(intPart) || !allDigits(frac) || (intPart == "" && frac == "") || (hasDot && frac == "") {
		return Money{}, errMoneySyntax
	}
	frac = strings.TrimRight(frac, "0")
	if len(frac) > femtoDigits {
		return Money{}, errors.New("usage: an amount of USD may have at most 15 decimal places")
	}
	digits := intPart + frac + strings.Repeat("0", femtoDigits-len(frac))
	v, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return Money{}, errMoneySyntax
	}
	if neg {
		v.Neg(v)
	}
	return fromFemto(v), nil
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

// fromFemto wraps v, which the caller must not modify afterwards.
func fromFemto(v *big.Int) Money {
	if v.Sign() == 0 {
		return Money{}
	}
	return Money{femto: v}
}

// int returns the amount in femto-USD. The result must not be modified.
func (m Money) int() *big.Int {
	if m.femto == nil {
		return new(big.Int)
	}
	return m.femto
}

// Add returns m + o.
func (m Money) Add(o Money) Money {
	if o.femto == nil {
		return m
	}
	if m.femto == nil {
		return o
	}
	return fromFemto(new(big.Int).Add(m.femto, o.femto))
}

// Sub returns m - o.
func (m Money) Sub(o Money) Money {
	if o.femto == nil {
		return m
	}
	return fromFemto(new(big.Int).Sub(m.int(), o.femto))
}

// Cmp compares m and o and returns -1 if m < o, 0 if m == o and +1 if m > o.
func (m Money) Cmp(o Money) int { return m.int().Cmp(o.int()) }

// Equal reports whether m and o are the same amount. go-cmp uses this
// method.
func (m Money) Equal(o Money) bool { return m.Cmp(o) == 0 }

// IsZero reports whether m is 0 USD.
func (m Money) IsZero() bool { return m.femto == nil || m.femto.Sign() == 0 }

// String returns m as a plain decimal with trailing zeros trimmed and never
// an exponent, such as "0.0003492", "-1.5" or "0".
func (m Money) String() string {
	if m.IsZero() {
		return "0"
	}
	abs := new(big.Int).Abs(m.femto)
	q, r := new(big.Int).QuoRem(abs, pow10(femtoDigits), new(big.Int))
	var b strings.Builder
	if m.femto.Sign() < 0 {
		b.WriteByte('-')
	}
	b.WriteString(q.String())
	if r.Sign() != 0 {
		frac := r.String()
		frac = strings.Repeat("0", femtoDigits-len(frac)) + frac
		b.WriteByte('.')
		b.WriteString(strings.TrimRight(frac, "0"))
	}
	return b.String()
}

// Float64 returns the nearest float64 to m. It is for metrics and other
// approximate uses; use String or the Money methods where exactness matters.
func (m Money) Float64() float64 {
	if m.IsZero() {
		return 0
	}
	f, _ := new(big.Rat).SetFrac(m.femto, pow10(femtoDigits)).Float64()
	return f
}

// MicroUSDCeil returns m in whole micro-USD (1e-6 USD), rounded up towards
// positive infinity, for budget counters, where over-counting is the safe
// error. Amounts beyond the int64 range saturate at math.MaxInt64 or
// math.MinInt64.
func (m Money) MicroUSDCeil() int64 {
	if m.IsZero() {
		return 0
	}
	// QuoRem truncates towards zero, which is already the ceiling for a
	// negative amount.
	q, r := new(big.Int).QuoRem(m.femto, big.NewInt(femtoPerMicro), new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	switch {
	case q.IsInt64():
		return q.Int64()
	case q.Sign() > 0:
		return math.MaxInt64
	default:
		return math.MinInt64
	}
}

// MarshalText encodes m as String does, so that JSON carries money as a
// decimal string.
func (m Money) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

// UnmarshalText decodes text as ParseMoney does.
func (m *Money) UnmarshalText(text []byte) error {
	v, err := ParseMoney(string(text))
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// pow10 returns 10^n as a new big.Int.
func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}
