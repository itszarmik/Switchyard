package policy

import (
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"switchyard/internal/config"
)

// Request headers that select a profile or tighten the policy (DESIGN §5).
const (
	HeaderProfile    = "X-Switchyard-Profile"
	HeaderMaxCostUSD = "X-Switchyard-Max-Cost-Usd"
	HeaderMinQuality = "X-Switchyard-Min-Quality"
	HeaderMaxTTFTMs  = "X-Switchyard-Max-TTFT-Ms"
)

// Limits of the header values.
const (
	maxQualityLen = 32
	maxTTFTDigits = 9
	nanoPerUSD    = 1_000_000_000
	// maxCapNano is the largest cost cap, in nano-USD.
	maxCapNano = config.MaxCostPerRequestUSD * nanoPerUSD
)

// qualityRE is a plain decimal such as 0.85, 1 or .9.
var qualityRE = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)$`)

// headerValues are the parsed policy headers of one request.
type headerValues struct {
	profile    string
	hasProfile bool
	// maxCostNano is the requested cost cap in nano-USD, or 0.
	maxCostNano   int64
	minQuality    float64
	hasMinQuality bool
	// maxTTFT is the requested latency target, or 0.
	maxTTFT time.Duration
}

// parseHeaders parses the policy headers in h. A malformed value, or a
// header given more than once, is an *InvalidHeaderError.
func parseHeaders(h map[string][]string) (headerValues, error) {
	var hv headerValues
	if v, ok, err := singleHeader(h, HeaderProfile); err != nil {
		return hv, err
	} else if ok {
		if !validProfileName(v) {
			return hv, &InvalidHeaderError{Header: HeaderProfile, Reason: "must be a profile name"}
		}
		hv.profile, hv.hasProfile = v, true
	}
	if v, ok, err := singleHeader(h, HeaderMaxCostUSD); err != nil {
		return hv, err
	} else if ok {
		p, perr := config.ParsePrice(v)
		if perr != nil || priceNano(p) == 0 {
			return hv, &InvalidHeaderError{Header: HeaderMaxCostUSD, Reason: "must be a decimal amount of USD greater than 0 with at most 9 decimal places, such as 0.05"}
		}
		hv.maxCostNano = priceNano(p)
	}
	if v, ok, err := singleHeader(h, HeaderMinQuality); err != nil {
		return hv, err
	} else if ok {
		q, qerr := strconv.ParseFloat(v, 64)
		if len(v) > maxQualityLen || !qualityRE.MatchString(v) || qerr != nil || q < 0 || q > 1 {
			return hv, &InvalidHeaderError{Header: HeaderMinQuality, Reason: "must be a decimal between 0 and 1, such as 0.85"}
		}
		hv.minQuality, hv.hasMinQuality = q, true
	}
	if v, ok, err := singleHeader(h, HeaderMaxTTFTMs); err != nil {
		return hv, err
	} else if ok {
		ms, merr := strconv.ParseInt(v, 10, 64)
		if len(v) > maxTTFTDigits || !allDigits(v) || merr != nil || ms <= 0 {
			return hv, &InvalidHeaderError{Header: HeaderMaxTTFTMs, Reason: "must be a whole number of milliseconds greater than 0"}
		}
		hv.maxTTFT = time.Duration(ms) * time.Millisecond
	}
	return hv, nil
}

// singleHeader returns the value of the header name in h, trimmed of
// surrounding spaces, matching the name case-insensitively. A header that
// is present must have exactly one non-empty value.
func singleHeader(h map[string][]string, name string) (string, bool, error) {
	values := headerValuesOf(h, name)
	switch {
	case values == nil:
		return "", false, nil
	case len(values) != 1:
		return "", false, &InvalidHeaderError{Header: name, Reason: "must be given once"}
	}
	v := strings.TrimSpace(values[0])
	if v == "" {
		return "", false, &InvalidHeaderError{Header: name, Reason: "must not be empty"}
	}
	return v, true, nil
}

// headerValuesOf returns every value of the header name in h, matching the
// name case-insensitively. Keys that differ only in case are read in sorted
// order, so the result is deterministic. It returns nil when the header is
// absent, and an empty non-nil slice when it is present without values.
func headerValuesOf(h map[string][]string, name string) []string {
	var keys []string
	for k := range h {
		if strings.EqualFold(k, name) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	slices.Sort(keys)
	out := []string{}
	for _, k := range keys {
		out = append(out, h[k]...)
	}
	return out
}

// validProfileName reports whether s may name a profile: the config ID
// rules.
func validProfileName(s string) bool { return config.ValidID(s) }

// allDigits reports whether s is non-empty and only ASCII digits.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// priceNano converts a USD amount to nano-USD and clamps it to maxCapNano.
// The conversion is exact, since config.ParsePrice allows at most 9 decimal
// places; a Price built otherwise with more is rounded up to the next
// nano-USD, never down to 0, which would mean no cap. It returns 0 for an
// unset or zero amount.
func priceNano(p config.Price) int64 {
	if !p.IsSet() {
		return 0
	}
	r, ok := new(big.Rat).SetString(p.String())
	if !ok || r.Sign() <= 0 {
		return 0
	}
	r.Mul(r, new(big.Rat).SetInt64(nanoPerUSD))
	q, m := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Int64() > maxCapNano {
		return maxCapNano
	}
	return q.Int64()
}
