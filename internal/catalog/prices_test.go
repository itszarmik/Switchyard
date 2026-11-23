package catalog

import (
	"path/filepath"
	"testing"
	"time"

	"switchyard/internal/config"
)

// pricesYAML has deployments with dated price changes: "m@a" with three
// changes, in the date form, the timestamp form with an offset and UTC;
// "m@b" with none; and "c@a", whose change sets only cache_write.
const pricesYAML = `version: 1
providers:
  - {id: a, type: openai, base_url: 'https://a.example/v1'}
  - {id: b, type: openai, base_url: 'https://b.example/v1'}
models:
  - id: m
    deployments:
      - provider: a
        upstream_model: m
        pricing:
          input: "2.00"
          output: "10.00"
          cached_input: "0.10"
          source: https://a.example/pricing
          checked: "2026-11-11"
          changes:
            - {from: "2026-12-01", input: "4.00", output: "20.00"}
            - {from: "2027-01-01T09:00:00+09:00", cached_input: "0.20"}
            - {from: "2027-02-01T12:30:00Z", output: "25.00", cache_write: "1.25"}
      - {provider: b, upstream_model: m, pricing: {input: "3", output: "12"}}
  - id: c
    deployments:
      - provider: a
        upstream_model: c
        pricing:
          input: "3"
          output: "15"
          changes: [{from: 2026-12-01, cache_write: "3.75"}]
`

// prices is a deployment's prices as text: input, output, cached_input
// and cache_write.
type prices [4]string

func pricesOf(p config.Pricing) prices {
	return prices{p.Input.String(), p.Output.String(), p.CachedInput.String(), p.CacheWrite.String()}
}

func TestNewAtResolvesPriceChanges(t *testing.T) {
	cfg := loadConfig(t, pricesYAML)
	utc := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		at      string
		m, b, c prices
		next    string // "" for none
		mFloats [4]float64
	}{
		{at: "2026-11-30T23:59:59.999999999Z",
			m: prices{"2.00", "10.00", "0.10", ""}, b: prices{"3", "12", "", ""}, c: prices{"3", "15", "", ""},
			next: "2026-12-01T00:00:00Z", mFloats: [4]float64{2, 10, 0.1, 0}},
		// A date means 00:00 UTC; at the boundary the change is in force.
		{at: "2026-12-01T00:00:00Z",
			m: prices{"4.00", "20.00", "0.10", ""}, b: prices{"3", "12", "", ""}, c: prices{"3", "15", "", "3.75"},
			next: "2027-01-01T00:00:00Z", mFloats: [4]float64{4, 20, 0.1, 0}},
		// 09:00 at +09:00 is midnight UTC.
		{at: "2026-12-31T23:59:59Z",
			m: prices{"4.00", "20.00", "0.10", ""}, b: prices{"3", "12", "", ""}, c: prices{"3", "15", "", "3.75"},
			next: "2027-01-01T00:00:00Z", mFloats: [4]float64{4, 20, 0.1, 0}},
		{at: "2027-01-01T00:00:00Z",
			m: prices{"4.00", "20.00", "0.20", ""}, b: prices{"3", "12", "", ""}, c: prices{"3", "15", "", "3.75"},
			next: "2027-02-01T12:30:00Z", mFloats: [4]float64{4, 20, 0.2, 0}},
		// Each change inherits what it does not set from the one before.
		{at: "2027-02-01T12:30:00Z",
			m: prices{"4.00", "25.00", "0.20", "1.25"}, b: prices{"3", "12", "", ""}, c: prices{"3", "15", "", "3.75"},
			mFloats: [4]float64{4, 25, 0.2, 1.25}},
	}
	for _, tt := range tests {
		t.Run(tt.at, func(t *testing.T) {
			c, err := NewAt(cfg, utc(tt.at))
			if err != nil {
				t.Fatal(err)
			}
			for id, want := range map[string]prices{"m@a": tt.m, "m@b": tt.b, "c@a": tt.c} {
				res, err := c.Resolve(id)
				if err != nil {
					t.Fatal(err)
				}
				d := res.Candidates[0]
				if got := pricesOf(d.Prices); got != want {
					t.Errorf("%s prices = %q, want %q", id, got, want)
				}
				if d.Prices.Changes != nil {
					t.Errorf("%s prices keep their changes: %v", id, d.Prices.Changes)
				}
				if d.Prices.Source != cfg.Models[0].Deployments[0].Pricing.Source && id == "m@a" {
					t.Errorf("%s source = %q", id, d.Prices.Source)
				}
				if !d.Pricing.Known {
					t.Errorf("%s pricing unknown", id)
				}
			}
			res, _ := c.Resolve("m@a")
			p := res.Candidates[0].Pricing
			if got := [4]float64{p.Input, p.Output, p.CachedInput, p.CacheWrite}; got != tt.mFloats {
				t.Errorf("m@a float prices = %v, want %v", got, tt.mFloats)
			}
			var want time.Time
			if tt.next != "" {
				want = utc(tt.next)
			}
			if got := c.NextPriceChange(); !got.Equal(want) {
				t.Errorf("NextPriceChange = %v, want %v", got, want)
			}
		})
	}
	// The ranking uses the prices in force: m@b (3/12) is cheaper than
	// m@a once m@a's prices double.
	for at, want := range map[string]string{"2026-11-15T00:00:00Z": "m@a", "2026-12-15T00:00:00Z": "m@b"} {
		c, err := NewAt(cfg, utc(at))
		if err != nil {
			t.Fatal(err)
		}
		res, _ := c.Resolve("m")
		sel, err := c.Select(res, nil, 1000, 1000, false)
		if err != nil {
			t.Fatal(err)
		}
		if sel.Deployment.ID != want {
			t.Errorf("at %s the cheapest is %s, want %s", at, sel.Deployment.ID, want)
		}
	}
}

// A change can complete a set of prices; PricingKnown applies to the
// prices in force.
func TestNewAtKnownFromAChange(t *testing.T) {
	cfg := loadConfig(t, `version: 1
providers: [{id: a, type: openai, base_url: 'https://a.example/v1'}]
models:
  - id: m
    deployments:
      - provider: a
        upstream_model: m
        pricing: {input: 1, changes: [{from: 2026-12-01, output: 2}]}
`)
	for at, want := range map[time.Time]bool{
		time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC): false,
		time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC):  true,
	} {
		c, err := NewAt(cfg, at)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Models()[0].Deployments[0].Pricing.Known; got != want {
			t.Errorf("at %v Known = %v, want %v", at, got, want)
		}
	}
}

// Without changes, NewAt at any time is New, and there is no next change.
func TestNewWithoutPriceChanges(t *testing.T) {
	cfg := loadConfig(t, testCatalogYAML)
	c, err := NewAt(cfg, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !c.NextPriceChange().IsZero() {
		t.Errorf("NextPriceChange = %v, want zero", c.NextPriceChange())
	}
	res, _ := c.Resolve("claude")
	if got, want := pricesOf(res.Candidates[0].Prices), (prices{"3", "15", "0.30", "3.75"}); got != want {
		t.Errorf("prices = %q, want %q", got, want)
	}
}

// TestShippedCatalogPriceChanges: the shipped catalog has Google's
// announced changes, at 00:00 UTC on their dates.
func TestShippedCatalogPriceChanges(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "switchyard.example.yaml")
	snap, err := config.Load(path, config.LoadOptions{
		LookupEnv:          func(string) (string, bool) { return "", false },
		SecretPlaceholders: true,
	})
	if err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}
	tests := []struct {
		id            string
		from          time.Time
		before, after prices
	}{
		{"gemini-4-carbon@google", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
			prices{"2.00", "10.00", "0.10", ""}, prices{"4.00", "20.00", "0.20", ""}},
		{"gemini-3.8-flash@google", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
			prices{"0.75", "3.75", "0.075", ""}, prices{"1.50", "7.50", "0.15", ""}},
	}
	for _, tt := range tests {
		for _, at := range []struct {
			t    time.Time
			want prices
		}{{tt.from.Add(-time.Nanosecond), tt.before}, {tt.from, tt.after}, {tt.from.AddDate(1, 0, 0), tt.after}} {
			c, err := NewAt(snap.Config(), at.t)
			if err != nil {
				t.Fatal(err)
			}
			res, err := c.Resolve(tt.id)
			if err != nil {
				t.Fatal(err)
			}
			if got := pricesOf(res.Candidates[0].Prices); got != at.want {
				t.Errorf("%s at %v = %q, want %q", tt.id, at.t, got, at.want)
			}
		}
	}
	// Before the first change, the gateway's next rebuild is at Carbon's.
	c, err := NewAt(snap.Config(), time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.NextPriceChange(), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("NextPriceChange = %v, want %v", got, want)
	}
	c, err = NewAt(snap.Config(), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.NextPriceChange(), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("NextPriceChange after Carbon's = %v, want %v", got, want)
	}
}
