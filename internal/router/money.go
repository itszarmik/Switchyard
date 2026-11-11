package router

import (
	"math"
	"math/big"
	"math/bits"
	"strconv"
	"strings"

	"switchyard/internal/config"
)

// femto is an exact non-negative amount of USD in femto-USD (1e-15 USD),
// held in 128 bits so that cost estimates need no allocation. Arithmetic
// saturates at the maximum instead of wrapping.
//
// A price of n nano-USD per million tokens costs n×1e-9/1e6 USD = n femto-USD
// per token, so tokens × nano-price is an exact femto-USD amount, as in
// usage.Cost.
type femto struct{ hi, lo uint64 }

// maxFemto is the saturation value.
var maxFemto = femto{math.MaxUint64, math.MaxUint64}

// femtoPerMicro and femtoPerNano are the number of femto-USD in one
// micro-USD and in one nano-USD.
const (
	femtoPerMicro = 1_000_000_000
	femtoPerNano  = 1_000_000
)

// femtoDigits is the number of decimal places of a femto amount.
const femtoDigits = 15

// mulFemto returns a×b.
func mulFemto(a, b uint64) femto {
	hi, lo := bits.Mul64(a, b)
	return femto{hi, lo}
}

// add returns f+g, saturating.
func (f femto) add(g femto) femto {
	lo, carry := bits.Add64(f.lo, g.lo, 0)
	hi, over := bits.Add64(f.hi, g.hi, carry)
	if over != 0 {
		return maxFemto
	}
	return femto{hi, lo}
}

// cmp returns -1, 0 or +1 as f is less than, equal to or greater than g.
func (f femto) cmp(g femto) int {
	switch {
	case f.hi < g.hi:
		return -1
	case f.hi > g.hi:
		return 1
	case f.lo < g.lo:
		return -1
	case f.lo > g.lo:
		return 1
	}
	return 0
}

// String formats f as a plain decimal amount of USD without trailing zeros,
// such as "0.00035" or "0". It allocates only the result, except for
// amounts beyond 2^64 femto-USD (about 18,446 USD).
func (f femto) String() string {
	var buf [48]byte
	var digits []byte
	if f.hi == 0 {
		digits = strconv.AppendUint(buf[:0], f.lo, 10)
	} else {
		v := new(big.Int).SetUint64(f.hi)
		v.Lsh(v, 64)
		v.Or(v, new(big.Int).SetUint64(f.lo))
		digits = v.Append(buf[:0], 10)
	}
	var out [64]byte
	o := out[:0]
	// The whole part, at least "0".
	point := len(digits) - femtoDigits
	if point > 0 {
		o = append(o, digits[:point]...)
	} else {
		o = append(o, '0')
	}
	// The fraction, left-padded with zeros, without trailing zeros.
	end := len(digits)
	for end > 0 && end > point && digits[end-1] == '0' {
		end--
	}
	if end <= max(point, 0) {
		return string(o)
	}
	o = append(o, '.')
	for range -point {
		o = append(o, '0')
	}
	o = append(o, digits[max(point, 0):end]...)
	return string(o)
}

// microToFemto converts a non-negative amount of micro-USD.
func microToFemto(micro int64) femto {
	return mulFemto(uint64(max(micro, 0)), femtoPerMicro)
}

// nanoToFemto converts a non-negative amount of nano-USD.
func nanoToFemto(nano int64) femto {
	return mulFemto(uint64(max(nano, 0)), femtoPerNano)
}

// nanoPerMillion converts a set price in USD per million tokens to whole
// nano-USD per million tokens. config.ParsePrice guarantees a plain
// non-negative decimal with at most 9 decimals; a value beyond uint64
// saturates. It reports false for an unset price.
func nanoPerMillion(p config.Price) (uint64, bool) {
	s := p.String()
	if s == "" {
		return 0, false
	}
	intPart, frac, _ := strings.Cut(s, ".")
	const nanoDigits = 9
	if len(frac) > nanoDigits {
		return 0, false // unreachable for a validated Price
	}
	v, err := strconv.ParseUint(intPart+frac+strings.Repeat("0", nanoDigits-len(frac)), 10, 64)
	if err != nil {
		return math.MaxUint64, true
	}
	return v, true
}
