package eval

import (
	"cmp"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"slices"
)

// The statistics of the quality estimate (DESIGN §10.4, §10.7).
const (
	// Resamples is the number of bootstrap resamples behind each interval.
	Resamples = 1000
	// Confidence is the coverage of the reported interval.
	Confidence = 0.90
	// MinInformative is the number of informative samples below which an
	// estimate is marked insufficient.
	MinInformative = 10
)

// Sample is one judged comparison: the difficulty d of the prompt and
// whether the candidate's answer was acceptable.
type Sample struct {
	Difficulty float64
	Acceptable bool
}

// Estimate is the maximum-likelihood estimate of a model's quality prior
// Q in one category, under the model of DESIGN §10.4:
//
//	P(acceptable | d) = 1 − d·(1 − Q)
//
// Samples with d = 0 are acceptable with probability 1 whatever Q is, so
// they carry no information and are left out.
type Estimate struct {
	// N is the number of samples given; Informative the number with d > 0.
	N, Informative int
	// OK reports that there was at least one informative sample, so Q and
	// the interval are defined.
	OK bool
	// Q is the estimate, in [0, 1].
	Q float64
	// CILow and CIHigh bound the 90% percentile-bootstrap interval of Q.
	// They always contain Q.
	CILow, CIHigh float64
	// Insufficient reports fewer than MinInformative informative samples.
	Insufficient bool
}

// group is a set of informative samples with the same difficulty and
// outcome; count is how many there are (in a resample).
type group struct {
	d          float64
	acceptable bool
	count      int
}

// EstimateQ estimates Q from samples, with a bootstrap interval of
// Resamples resamples drawn from a generator seeded with seed. The result
// depends only on the multiset of samples and the seed, not on their
// order.
func EstimateQ(samples []Sample, seed uint64) Estimate {
	e := Estimate{N: len(samples)}
	informative := make([]Sample, 0, len(samples))
	for _, s := range samples {
		d := clamp01(s.Difficulty)
		if d > 0 {
			informative = append(informative, Sample{Difficulty: d, Acceptable: s.Acceptable})
		}
	}
	e.Informative = len(informative)
	e.Insufficient = e.Informative < MinInformative
	if e.Informative == 0 {
		return e
	}
	e.OK = true

	// Sorting makes the bootstrap independent of the input order, and
	// grouping equal samples makes each likelihood evaluation O(groups).
	slices.SortFunc(informative, func(a, b Sample) int {
		if c := cmp.Compare(a.Difficulty, b.Difficulty); c != 0 {
			return c
		}
		return boolCompare(a.Acceptable, b.Acceptable)
	})
	groups := make([]group, 0, len(informative))
	of := make([]int, len(informative)) // sample index -> group index
	for i, s := range informative {
		if n := len(groups); n == 0 || groups[n-1].d != s.Difficulty || groups[n-1].acceptable != s.Acceptable {
			groups = append(groups, group{d: s.Difficulty, acceptable: s.Acceptable})
		}
		groups[len(groups)-1].count++
		of[i] = len(groups) - 1
	}
	e.Q = mle(groups)

	// A seeded generator, for reproducible intervals; nothing secret.
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) // #nosec G404 -- reproducibility, not secrecy
	boot := make([]group, len(groups))
	ests := make([]float64, Resamples)
	n := len(informative)
	for b := range ests {
		for g := range boot {
			boot[g] = group{d: groups[g].d, acceptable: groups[g].acceptable}
		}
		for range n {
			boot[of[rng.IntN(n)]].count++
		}
		ests[b] = mle(boot)
	}
	slices.Sort(ests)
	alpha := (1 - Confidence) / 2
	e.CILow = min(quantile(ests, alpha), e.Q)
	e.CIHigh = max(quantile(ests, 1-alpha), e.Q)
	return e
}

// MLE returns the maximum-likelihood estimate of Q from samples, ignoring
// those with d = 0; ok is false when no sample is informative.
func MLE(samples []Sample) (q float64, ok bool) {
	var groups []group
	for _, s := range samples {
		if d := clamp01(s.Difficulty); d > 0 {
			groups = append(groups, group{d: d, acceptable: s.Acceptable, count: 1})
		}
	}
	if len(groups) == 0 {
		return 0, false
	}
	return mle(groups), true
}

// mle maximizes the log-likelihood over Q in [0, 1]:
//
//	L(Q) = Σ_acceptable log(1 − d + d·Q) + Σ_unacceptable log(d·(1 − Q))
//
// L is concave (each term is the log of an affine function of Q), so its
// maximum on [0, 1] is the boundary or the single root of
//
//	L'(Q) = Σ_acceptable d / (1 − d + d·Q) − n_unacceptable / (1 − Q),
//
// which is strictly decreasing; bisection finds it to well below 1e-12.
// Groups with count 0 are ignored.
func mle(groups []group) float64 {
	var unacceptable, acceptable int
	for _, g := range groups {
		if g.acceptable {
			acceptable += g.count
		} else {
			unacceptable += g.count
		}
	}
	switch {
	case unacceptable == 0:
		return 1
	case acceptable == 0:
		return 0
	}
	deriv := func(q float64) float64 {
		s := 0.0
		for _, g := range groups {
			if g.acceptable && g.count > 0 {
				s += float64(g.count) * g.d / (1 - g.d + g.d*q)
			}
		}
		return s - float64(unacceptable)/(1-q)
	}
	if deriv(0) <= 0 {
		return 0
	}
	lo, hi := 0.0, 1.0
	for range 60 {
		mid := (lo + hi) / 2
		if deriv(mid) > 0 {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// quantile returns the p-quantile of sorted, interpolating linearly
// between order statistics.
func quantile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	pos := p * float64(len(sorted)-1)
	i := int(math.Floor(pos))
	if i >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	frac := pos - float64(i)
	return sorted[i] + frac*(sorted[i+1]-sorted[i])
}

// Seed returns the bootstrap seed of a (model, category) pair: a hash of
// both, so that every fit of the same results is identical.
func Seed(model, category string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(model))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(category))
	return h.Sum64()
}

// clamp01 clamps v to [0, 1]; NaN becomes 0.
func clamp01(v float64) float64 {
	switch {
	case v > 1:
		return 1
	case v >= 0:
		return v
	default:
		return 0
	}
}

// boolCompare orders false before true.
func boolCompare(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	default:
		return 1
	}
}
