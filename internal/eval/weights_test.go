package eval

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.yaml.in/yaml/v3"

	"switchyard/internal/quality"
	"switchyard/internal/schema"
)

const sanityPath = "../quality/testdata/sanity.json"

// TestFeatureProbes checks the premise of FitWeights: the probed features,
// weighted by the defaults, reproduce the default heuristic's difficulty.
func TestFeatureProbes(t *testing.T) {
	set, err := LoadLabeled(sanityPath)
	if err != nil {
		t.Fatal(err)
	}
	names := quality.SignalNames()
	def := quality.DefaultWeights()
	h, err := quality.NewHeuristic(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range set {
		req, err := schema.DecodeChatRequest(e.Request)
		if err != nil {
			t.Fatal(err)
		}
		d := 0.0
		for _, n := range names {
			probe, err := quality.NewHeuristic(map[string]float64{
				"length": 0, "code": 0, "math": 0, "reasoning_cues": 0, "constraints": 0, "conversation_depth": 0,
				"tools": 0, "strict_schema": 0, "simple_task": 0, "creative": 0, "bias": 0, n: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			f := probe.Assess(req, quality.Hints{}).Difficulty
			if f < 0 || f > 1 {
				t.Fatalf("%s: feature %s = %v outside [0, 1]", e.ID, n, f)
			}
			d += def[n] * f
		}
		if got := h.Assess(req, quality.Hints{}).Difficulty; math.Abs(clamp01(d)-got) > 1e-9 {
			t.Errorf("%s: probed difficulty %v, heuristic %v", e.ID, clamp01(d), got)
		}
	}
}

func TestFitWeights(t *testing.T) {
	set, err := LoadLabeled(sanityPath)
	if err != nil {
		t.Fatal(err)
	}
	fit, err := FitWeights(set)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("in-band accuracy on %d prompts: defaults %.1f%%, fitted %.1f%% (%d iterations); weights %v",
		fit.N, 100*fit.Before, 100*fit.After, fit.Iterations, fit.Weights)
	if fit.After < 0.80 {
		t.Errorf("fitted accuracy %.1f%%, want at least 80%%", 100*fit.After)
	}
	if fit.N != len(set) || len(fit.Weights) != len(quality.SignalNames()) {
		t.Errorf("fit covers %d prompts and %d weights", fit.N, len(fit.Weights))
	}
	for n, w := range fit.Weights {
		if w < -1 || w > 1 {
			t.Errorf("weight %s = %v outside [-1, 1]", n, w)
		}
	}
	if _, err := quality.NewHeuristic(fit.Weights); err != nil {
		t.Errorf("fitted weights rejected: %v", err)
	}

	again, err := FitWeights(set)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(fit, again); diff != "" {
		t.Errorf("not deterministic (-first +second):\n%s", diff)
	}

	var buf bytes.Buffer
	if err := WriteWeights(&buf, fit, "sanity.json"); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Routing struct {
			Assessor struct {
				Weights map[string]float64 `yaml:"weights"`
			} `yaml:"assessor"`
		} `yaml:"routing"`
	}
	if err := yaml.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("weights YAML: %v\n%s", err, buf.String())
	}
	if diff := cmp.Diff(fit.Weights, doc.Routing.Assessor.Weights); diff != "" {
		t.Errorf("written weights (-fit +file):\n%s", diff)
	}
	if !strings.Contains(buf.String(), "in-band accuracy") {
		t.Errorf("missing accuracy header:\n%s", buf.String())
	}
}

func TestFitWeightsErrors(t *testing.T) {
	req := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	tests := []struct {
		name string
		set  []Labeled
	}{
		{name: "unknown band", set: []Labeled{{ID: "a", Band: "trivial", Request: req}}},
		{name: "invalid request", set: []Labeled{{ID: "a", Band: BandEasy, Request: []byte(`{}`)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := FitWeights(tt.set); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestBandOf(t *testing.T) {
	for d, want := range map[float64]string{0: BandEasy, 0.349: BandEasy, 0.35: BandMedium, 0.649: BandMedium, 0.65: BandHard, 1: BandHard} {
		if got := BandOf(d); got != want {
			t.Errorf("BandOf(%v) = %s, want %s", d, got, want)
		}
	}
}
