package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"

	"switchyard/internal/quality"
	"switchyard/internal/schema"
)

// Difficulty bands of a labeled set and their least-squares targets.
const (
	BandEasy   = "easy"   // d < 0.35
	BandMedium = "medium" // 0.35 ≤ d < 0.65
	BandHard   = "hard"   // d ≥ 0.65
)

// bandTargets are the difficulties the fit aims each band at.
var bandTargets = map[string]float64{BandEasy: 0.2, BandMedium: 0.5, BandHard: 0.85}

// BandOf returns the band of difficulty d.
func BandOf(d float64) string {
	switch {
	case d < 0.35:
		return BandEasy
	case d < 0.65:
		return BandMedium
	default:
		return BandHard
	}
}

// Labeled is one prompt of a labeled set, such as
// internal/quality/testdata/sanity.json.
type Labeled struct {
	ID      string          `json:"id"`
	Band    string          `json:"band"`
	Request json.RawMessage `json:"request"`
}

// LoadLabeled reads a labeled set: a JSON array of {id, band, request}.
func LoadLabeled(path string) ([]Labeled, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the operator names the file
	if err != nil {
		return nil, fmt.Errorf("read the labeled set: %w", err)
	}
	var out []Labeled
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s: not a JSON array of labeled prompts", path)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: the labeled set is empty", path)
	}
	return out, nil
}

// WeightFit is the outcome of FitWeights.
type WeightFit struct {
	// Weights are the fitted weights, rounded to 3 decimals, by name.
	Weights map[string]float64
	// Before and After are the in-band accuracies of the default and the
	// fitted (rounded) weights, out of N prompts.
	Before, After float64
	N             int
	// Iterations is the number of gradient steps taken.
	Iterations int
}

// Fit-weights parameters.
const (
	maxWeightIterations = 200000
	weightTolerance     = 1e-12
)

// FitWeights fits the heuristic assessor's weights to the labeled bands by
// least squares: it minimizes the mean of (Σ wᵢ·fᵢ − target)², with targets
// 0.2, 0.5 and 0.85 for easy, medium and hard, by projected gradient
// descent with every weight kept in [−1, 1]. The objective is convex and
// the descent is full-batch from the default weights, so the fit is
// deterministic. The features fᵢ are read through the public Assessor:
// a heuristic whose only non-zero weight is 1 on signal i rates every
// request at exactly fᵢ, since features lie in [0, 1].
func FitWeights(set []Labeled) (WeightFit, error) {
	names := quality.SignalNames()
	probes := make([]*quality.Heuristic, len(names))
	for i, name := range names {
		w := make(map[string]float64, len(names))
		for _, n := range names {
			w[n] = 0
		}
		w[name] = 1
		h, err := quality.NewHeuristic(w)
		if err != nil {
			return WeightFit{}, fmt.Errorf("fit weights: %w", err)
		}
		probes[i] = h
	}

	feats := make([][]float64, len(set))
	targets := make([]float64, len(set))
	reqs := make([]*schema.ChatRequest, len(set))
	for i, e := range set {
		t, ok := bandTargets[e.Band]
		if !ok {
			return WeightFit{}, fmt.Errorf("fit weights: prompt %q: unknown band %q (want easy, medium or hard)", e.ID, e.Band)
		}
		req, err := schema.DecodeChatRequest(e.Request)
		if err != nil {
			return WeightFit{}, fmt.Errorf("fit weights: prompt %q: invalid request", e.ID)
		}
		reqs[i], targets[i] = req, t
		feats[i] = make([]float64, len(names))
		for j, h := range probes {
			feats[i][j] = h.Assess(req, quality.Hints{}).Difficulty
		}
	}

	defaults := quality.DefaultWeights()
	w := make([]float64, len(names))
	for j, n := range names {
		w[j] = defaults[n]
	}
	// The gradient of the mean squared error is Lipschitz with constant
	// 2·λmax(FᵀF)/n ≤ 2·trace(FᵀF)/n, so 1/that is a safe step.
	trace := 0.0
	for _, f := range feats {
		for _, v := range f {
			trace += v * v
		}
	}
	if trace == 0 {
		return WeightFit{}, errors.New("fit weights: every feature is zero")
	}
	step := float64(len(set)) / (2 * trace)
	grad := make([]float64, len(names))
	iters := 0
	for iters < maxWeightIterations {
		iters++
		clear(grad)
		for i, f := range feats {
			r := dot(w, f) - targets[i]
			for j, v := range f {
				grad[j] += 2 * r * v / float64(len(set))
			}
		}
		moved := 0.0
		for j := range w {
			nw := math.Max(-1, math.Min(1, w[j]-step*grad[j]))
			moved = math.Max(moved, math.Abs(nw-w[j]))
			w[j] = nw
		}
		if moved < weightTolerance {
			break
		}
	}

	fit := WeightFit{Weights: make(map[string]float64, len(names)), N: len(set), Iterations: iters}
	for j, n := range names {
		fit.Weights[n] = math.Round(w[j]*1000) / 1000
	}
	var err error
	if fit.Before, err = accuracy(defaults, set, reqs); err != nil {
		return WeightFit{}, err
	}
	if fit.After, err = accuracy(fit.Weights, set, reqs); err != nil {
		return WeightFit{}, err
	}
	return fit, nil
}

// accuracy is the share of set the heuristic with weights rates in its band.
func accuracy(weights map[string]float64, set []Labeled, reqs []*schema.ChatRequest) (float64, error) {
	h, err := quality.NewHeuristic(weights)
	if err != nil {
		return 0, fmt.Errorf("fit weights: %w", err)
	}
	hits := 0
	for i, e := range set {
		if BandOf(h.Assess(reqs[i], quality.Hints{}).Difficulty) == e.Band {
			hits++
		}
	}
	return float64(hits) / float64(len(set)), nil
}

func dot(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// WriteWeights writes the fit as a routing.assessor.weights config
// section, after a header comment with the accuracies. Included files
// cannot set routing, so the section belongs in the main config.
func WriteWeights(w io.Writer, fit WeightFit, origin string) error {
	header := fmt.Sprintf("# Assessor weights fitted by `switchyard eval fit-weights` to %s:\n"+
		"# in-band accuracy %.1f%% with the defaults, %.1f%% with these weights (%d prompts).\n"+
		"# Merge this section into the main config; included files cannot set routing.\n",
		origin, 100*fit.Before, 100*fit.After, fit.N)
	if _, err := io.WriteString(w, header); err != nil {
		return fmt.Errorf("write the weights: %w", err)
	}
	var weights yaml.Node
	weights.Kind = yaml.MappingNode
	for _, n := range quality.SignalNames() {
		weights.Content = append(weights.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: n},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: formatWeight(fit.Weights[n])})
	}
	doc := map[string]any{"routing": map[string]any{"assessor": map[string]any{"weights": &weights}}}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("write the weights: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("write the weights: %w", err)
	}
	return nil
}

// formatWeight formats a weight with at most 3 decimals and at least one.
func formatWeight(v float64) string {
	s := strings.TrimRight(fmt.Sprintf("%.3f", v), "0")
	if strings.HasSuffix(s, ".") {
		s += "0"
	}
	if s == "-0.0" {
		s = "0.0"
	}
	return s
}
