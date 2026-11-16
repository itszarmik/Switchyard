package eval

import (
	"math"
	"math/rand/v2"
	"testing"
)

// synthetic draws n samples from the model of DESIGN §10.4 with the given
// true Q: difficulties uniform on levels/levels … 1 (continuous when
// levels is 0), acceptable with probability 1 − d·(1 − q).
func synthetic(rng *rand.Rand, n int, q float64, levels int) []Sample {
	out := make([]Sample, n)
	for i := range out {
		var d float64
		if levels > 0 {
			d = float64(1+rng.IntN(levels)) / float64(levels)
		} else {
			d = 1 - rng.Float64() // (0, 1]
		}
		out[i] = Sample{Difficulty: d, Acceptable: rng.Float64() < 1-d*(1-q)}
	}
	return out
}

func TestMLERecovery(t *testing.T) {
	t.Parallel()
	for _, q := range []float64{0.3, 0.7, 0.9} {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewPCG(1, uint64(q*1000)))
			samples := synthetic(rng, 2000, q, 0)
			got, ok := MLE(samples)
			if !ok || math.Abs(got-q) > 0.05 {
				t.Errorf("MLE = %.4f, %v; want %.2f ± 0.05", got, ok, q)
			}
			e := EstimateQ(samples, 7)
			t.Logf("true Q %.2f: MLE %.4f, 90%% CI [%.4f, %.4f], n=%d", q, e.Q, e.CILow, e.CIHigh, e.Informative)
			if math.Abs(e.Q-got) > 1e-12 || e.CILow > e.Q || e.CIHigh < e.Q || e.Insufficient {
				t.Errorf("EstimateQ = %+v, want Q %v inside its interval", e, got)
			}
		})
	}
}

// TestMLEIsTheMaximum checks the bisection against a fine grid of the
// log-likelihood.
func TestMLEIsTheMaximum(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(2, 3))
	samples := synthetic(rng, 300, 0.6, 0)
	loglik := func(q float64) float64 {
		s := 0.0
		for _, x := range samples {
			p := 1 - x.Difficulty*(1-q)
			if x.Acceptable {
				s += math.Log(p)
			} else {
				s += math.Log(1 - p)
			}
		}
		return s
	}
	got, _ := MLE(samples)
	best, bestQ := math.Inf(-1), 0.0
	for i := 0; i <= 100000; i++ {
		q := float64(i) / 100000
		if l := loglik(q); l > best {
			best, bestQ = l, q
		}
	}
	if math.Abs(got-bestQ) > 2e-5 {
		t.Errorf("MLE %.6f, grid maximum at %.6f", got, bestQ)
	}
}

func TestIntervalCalibration(t *testing.T) {
	t.Parallel()
	const (
		trials = 200
		q      = 0.7
		n      = 200
	)
	rng := rand.New(rand.NewPCG(42, 4242))
	covered := 0
	width := 0.0
	for trial := range trials {
		e := EstimateQ(synthetic(rng, n, q, 20), uint64(trial))
		if e.CILow <= q && q <= e.CIHigh {
			covered++
		}
		width += e.CIHigh - e.CILow
	}
	rate := float64(covered) / trials
	t.Logf("90%% interval covered Q=%.1f in %d/%d trials (%.1f%%); mean width %.3f at n=%d", q, covered, trials, 100*rate, width/trials, n)
	if rate < 0.85 {
		t.Errorf("coverage %.1f%%, want at least 85%%", 100*rate)
	}
}

func TestEstimateEdgeCases(t *testing.T) {
	repeat := func(n int, s Sample) []Sample {
		out := make([]Sample, n)
		for i := range out {
			out[i] = s
		}
		return out
	}
	tests := []struct {
		name    string
		samples []Sample
		want    Estimate
	}{
		{name: "none", samples: nil, want: Estimate{Insufficient: true}},
		{name: "all d = 0", samples: append(repeat(20, Sample{0, true}), repeat(5, Sample{0, false})...),
			want: Estimate{N: 25, Insufficient: true}},
		{name: "all acceptable", samples: repeat(30, Sample{0.8, true}),
			want: Estimate{N: 30, Informative: 30, OK: true, Q: 1, CILow: 1, CIHigh: 1}},
		{name: "none acceptable", samples: repeat(30, Sample{0.5, false}),
			want: Estimate{N: 30, Informative: 30, OK: true, Q: 0, CILow: 0, CIHigh: 0}},
		{name: "few", samples: append(repeat(3, Sample{1, true}), repeat(3, Sample{1, false})...),
			want: Estimate{N: 6, Informative: 6, OK: true, Q: 0.5, Insufficient: true}},
		{name: "negative difficulty counts as 0", samples: repeat(3, Sample{-0.5, false}),
			want: Estimate{N: 3, Insufficient: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateQ(tt.samples, 1)
			if tt.name == "few" {
				// At d = 1, the MLE is the pass rate; the interval is the
				// bootstrap's, which only has to contain it.
				if math.Abs(got.Q-0.5) > 1e-9 || got.CILow > got.Q || got.CIHigh < got.Q || got.CILow < 0 || got.CIHigh > 1 {
					t.Errorf("EstimateQ = %+v", got)
				}
				got.Q, got.CILow, got.CIHigh = tt.want.Q, 0, 0
			}
			if got != tt.want {
				t.Errorf("EstimateQ = %+v, want %+v", got, tt.want)
			}
		})
	}
	t.Run("Q stays in range when the likelihood rises at 0", func(t *testing.T) {
		// One acceptable sample at d = 0.1 and many failures: L'(0) < 0.
		s := append(repeat(50, Sample{0.9, false}), Sample{0.1, true})
		if got, _ := MLE(s); got != 0 {
			t.Errorf("MLE = %v, want 0", got)
		}
	})
}

func TestEstimateDeterminism(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 9))
	samples := synthetic(rng, 150, 0.8, 0)
	a := EstimateQ(samples, Seed("m", "coding"))
	b := EstimateQ(samples, Seed("m", "coding"))
	reversed := make([]Sample, len(samples))
	for i, s := range samples {
		reversed[len(samples)-1-i] = s
	}
	c := EstimateQ(reversed, Seed("m", "coding"))
	if a != b || a != c {
		t.Errorf("not deterministic:\n%+v\n%+v\n%+v (reversed input)", a, b, c)
	}
	if d := EstimateQ(samples, Seed("m", "general")); d == a {
		t.Error("different seeds gave the same interval; the seed is unused")
	}
	if Seed("ab", "c") == Seed("a", "bc") {
		t.Error("Seed does not separate model and category")
	}
}

func BenchmarkEstimateQ(b *testing.B) {
	rng := rand.New(rand.NewPCG(1, 1))
	samples := synthetic(rng, 2000, 0.7, 0)
	for b.Loop() {
		EstimateQ(samples, 1)
	}
}
