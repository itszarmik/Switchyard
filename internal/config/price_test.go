package config

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestParsePrice(t *testing.T) {
	tests := []struct {
		in      string
		want    float64
		wantErr string // substring; "" means valid
	}{
		{in: "0", want: 0},
		{in: "3", want: 3},
		{in: "0.15", want: 0.15},
		{in: "3.00", want: 3},
		{in: ".5", want: 0.5},
		{in: "0.000000001", want: 1e-9},
		{in: "1234567.123456789", want: 1234567.123456789},
		{in: "", wantErr: "expected a price"},
		{in: ".", wantErr: "expected a price"},
		{in: "5.", wantErr: "expected a price"},
		{in: "-1", wantErr: "must not be negative"},
		{in: "-0.5", wantErr: "must not be negative"},
		{in: "+1", wantErr: "expected a price"},
		{in: "1e-6", wantErr: "without an exponent"},
		{in: "2E3", wantErr: "without an exponent"},
		{in: "NaN", wantErr: "finite"},
		{in: ".inf", wantErr: "finite"},
		{in: "Infinity", wantErr: "finite"},
		{in: "0x1F", wantErr: "expected a price"},
		{in: "1_000", wantErr: "expected a price"},
		{in: "1,5", wantErr: "expected a price"},
		{in: " 1", wantErr: "expected a price"},
		{in: "0.0000000001", wantErr: "at most 9 decimal places"},
		{in: strings.Repeat("9", 400), wantErr: "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			p, err := ParsePrice(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParsePrice(%q) error = %v, want one containing %q", tt.in, err, tt.wantErr)
				}
				if len(tt.in) > 4 && strings.Contains(err.Error(), tt.in) {
					t.Errorf("the error echoes the value: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePrice(%q): %v", tt.in, err)
			}
			if !p.IsSet() || p.String() != tt.in || p.Float64() != tt.want {
				t.Errorf("ParsePrice(%q) = {IsSet %v, String %q, Float64 %v}, want {true, %q, %v}",
					tt.in, p.IsSet(), p.String(), p.Float64(), tt.in, tt.want)
			}
		})
	}
}

func TestPriceZeroValue(t *testing.T) {
	var p Price
	if p.IsSet() || p.String() != "" || p.Float64() != 0 {
		t.Errorf("zero Price = {IsSet %v, String %q, Float64 %v}, want unset", p.IsSet(), p.String(), p.Float64())
	}
	if !p.Equal(Price{}) || p.Equal(mustPrice(t, "0")) {
		t.Error("an unset price must equal only another unset price, not 0")
	}
}

func TestPriceYAML(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    string // the kept text; "" means unset
		wantErr string
	}{
		{name: "float", yaml: "p: 0.15", want: "0.15"},
		{name: "integer", yaml: "p: 3", want: "3"},
		{name: "trailing zeros kept", yaml: "p: 3.50", want: "3.50"},
		{name: "string", yaml: `p: "0.0375"`, want: "0.0375"},
		{name: "null", yaml: "p: ~", want: ""},
		{name: "empty", yaml: "p:", want: ""},
		{name: "negative", yaml: "p: -0.1", wantErr: "must not be negative"},
		{name: "exponent", yaml: "p: 1.5e-7", wantErr: "without an exponent"},
		{name: "nan", yaml: "p: .nan", wantErr: "finite"},
		{name: "inf", yaml: "p: .inf", wantErr: "finite"},
		{name: "too precise", yaml: "p: 0.1234567891", wantErr: "at most 9 decimal places"},
		{name: "boolean", yaml: "p: true", wantErr: "expected a price"},
		{name: "sequence", yaml: "p: [1]", wantErr: "expected a price"},
		{name: "mapping", yaml: "p: {a: 1}", wantErr: "expected a price"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v struct {
				P Price `yaml:"p"`
			}
			err := yaml.Unmarshal([]byte(tt.yaml), &v)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if v.P.String() != tt.want {
				t.Errorf("price text = %q, want %q", v.P.String(), tt.want)
			}

			// MarshalYAML round-trips the exact text, as a number.
			out, err := yaml.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			wantOut := "p: " + tt.want + "\n"
			if tt.want == "" {
				wantOut = "p: null\n"
			}
			if string(out) != wantOut {
				t.Errorf("Marshal = %q, want %q", out, wantOut)
			}
		})
	}
}

// Price type errors in a config are reported with their path, and name a
// referenced value by its reference.
func TestPriceInConfig(t *testing.T) {
	root := "version: 1\nproviders:\n" + prov("a") + `models:
  - id: m
    deployments:
      - provider: a
        upstream_model: m
        pricing: {input: -1, output: "${OUT}", cached_input: 1e-3, cache_write: abc}
`
	ve := mustFail(t, root, map[string]string{"OUT": "lots"}, nil)
	checkProblems(t, ve.Problems,
		errAt("models[0].deployments[0].pricing.input", "must not be negative, got \"-1\""),
		errAt("models[0].deployments[0].pricing.output", "expected a price in USD per 1M tokens, such as 0.15 or \"0.0375\", got the value of ${OUT}"),
		errAt("models[0].deployments[0].pricing.cached_input", "without an exponent"),
		errAt("models[0].deployments[0].pricing.cache_write", "expected a price"),
	)
	if strings.Contains(ve.Error(), "lots") {
		t.Errorf("the error echoes a referenced value:\n%s", ve)
	}
}
