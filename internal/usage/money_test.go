package usage

import (
	"encoding/json"
	"math"
	"testing"
)

func mustMoney(t testing.TB, s string) Money {
	t.Helper()
	m, err := ParseMoney(s)
	if err != nil {
		t.Fatalf("ParseMoney(%q): %v", s, err)
	}
	return m
}

func TestParseMoneyAndString(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"0", "0"},
		{"0.0", "0"},
		{"-0", "0"},
		{"000.000", "0"},
		{"1", "1"},
		{"12.50", "12.5"},
		{".5", "0.5"},
		{"-1.5", "-1.5"},
		{"0.000000000000001", "0.000000000000001"},
		{"-0.000000000000001", "-0.000000000000001"},
		{"0.0010000000000000000", "0.001"},
		{"123456789012345678901234.123456789012345", "123456789012345678901234.123456789012345"},
		{"0007", "7"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			m := mustMoney(t, tt.in)
			if got := m.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			// The output parses back to the same amount.
			if back := mustMoney(t, m.String()); !back.Equal(m) {
				t.Errorf("round trip: %s != %s", back, m)
			}
		})
	}
}

func TestParseMoneyRejects(t *testing.T) {
	for _, in := range []string{
		"", "-", ".", "1.", "+1", "1e3", "1E-3", "0x10", "1_000", "1,5", " 1", "1 ", "--1", "NaN", "Inf",
		"0.0000000000000001", // 16 significant decimals
		"1.2.3",
	} {
		t.Run(in, func(t *testing.T) {
			if m, err := ParseMoney(in); err == nil {
				t.Fatalf("ParseMoney(%q) = %s, want an error", in, m)
			}
		})
	}
}

func TestMoneyArithmetic(t *testing.T) {
	tests := []struct {
		name     string
		a, b     string
		sum, dif string
		cmp      int
	}{
		{"zero values", "0", "0", "0", "0", 0},
		{"exact decimals", "0.1", "0.2", "0.3", "-0.1", -1},
		{"femto precision", "0.000000000000001", "0.000000000000002", "0.000000000000003", "-0.000000000000001", -1},
		{"negative", "-1.25", "0.25", "-1", "-1.5", -1},
		{"equal", "2.50", "2.5", "5", "0", 0},
		{"greater", "3", "1.999999999999999", "4.999999999999999", "1.000000000000001", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := mustMoney(t, tt.a), mustMoney(t, tt.b)
			if got := a.Add(b).String(); got != tt.sum {
				t.Errorf("Add = %s, want %s", got, tt.sum)
			}
			if got := a.Sub(b).String(); got != tt.dif {
				t.Errorf("Sub = %s, want %s", got, tt.dif)
			}
			if got := a.Cmp(b); got != tt.cmp {
				t.Errorf("Cmp = %d, want %d", got, tt.cmp)
			}
			// The operands are unchanged.
			if a.String() != mustMoney(t, tt.a).String() || b.String() != mustMoney(t, tt.b).String() {
				t.Errorf("operands changed: a=%s b=%s", a, b)
			}
		})
	}
}

func TestMoneyZeroValue(t *testing.T) {
	var z Money
	if !z.IsZero() || z.String() != "0" || z.Float64() != 0 || z.MicroUSDCeil() != 0 {
		t.Errorf("zero Money: IsZero=%v String=%q Float64=%v Micro=%d", z.IsZero(), z.String(), z.Float64(), z.MicroUSDCeil())
	}
	one := mustMoney(t, "1")
	if got := z.Add(one); !got.Equal(one) {
		t.Errorf("0 + 1 = %s", got)
	}
	if got := z.Sub(one).String(); got != "-1" {
		t.Errorf("0 - 1 = %s", got)
	}
	if !one.Sub(one).IsZero() {
		t.Errorf("1 - 1 is not zero")
	}
}

func TestMoneyMicroUSDCeil(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"0.000001", 1},
		{"0.000000000000001", 1},
		{"0.0000010000001", 2},
		{"1", 1_000_000},
		{"0.0005253", 526},
		{"-0.0000015", -1},
		{"-0.000000000000001", 0},
		{"1000000000000000", math.MaxInt64},
		{"-1000000000000000", math.MinInt64},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := mustMoney(t, tt.in).MicroUSDCeil(); got != tt.want {
				t.Errorf("MicroUSDCeil(%s) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestMoneyNanoUSDCeil(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"0.000000001", 1},
		{"0.000000000000001", 1},
		{"0.000000001000001", 2},
		{"1", 1_000_000_000},
		{"0.00000008", 80},
		{"-0.0000000015", -1},
		{"-0.000000000000001", 0},
		{"10000000000", math.MaxInt64},
		{"-10000000000", math.MinInt64},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := mustMoney(t, tt.in).NanoUSDCeil(); got != tt.want {
				t.Errorf("NanoUSDCeil(%s) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestMoneyFloat64(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want float64
	}{
		{"0.0005253", 0.0005253},
		{"-2.5", -2.5},
		{"123.456", 123.456},
	} {
		if got := mustMoney(t, tt.in).Float64(); got != tt.want {
			t.Errorf("Float64(%s) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestMoneyJSON(t *testing.T) {
	type wrap struct {
		Cost Money `json:"cost"`
	}
	b, err := json.Marshal(wrap{Cost: mustMoney(t, "0.000525300")})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"cost":"0.0005253"}` {
		t.Errorf("Marshal = %s", b)
	}
	var w wrap
	if err := json.Unmarshal([]byte(`{"cost":"-1.25"}`), &w); err != nil {
		t.Fatal(err)
	}
	if w.Cost.String() != "-1.25" {
		t.Errorf("Unmarshal = %s", w.Cost)
	}
	if err := json.Unmarshal([]byte(`{"cost":"1e3"}`), &w); err == nil {
		t.Errorf("Unmarshal accepted an exponent")
	}
}
