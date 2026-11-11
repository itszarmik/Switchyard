package catalog

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"testing"

	"switchyard/internal/config"
	"switchyard/internal/schema"
)

// TestSelectProperties checks Select on random catalogs and requests:
//   - the chosen deployment has every required capability that was not
//     dropped; only droppable ones are dropped, and only when drop is set,
//     except sampling (and temperature_2 along with it), which is always
//     dropped;
//   - the chosen deployment fits the context window;
//   - no eligible candidate ranks before it: none with a known price is
//     cheaper, and if it has an unknown price, no eligible one has a known
//     price; among equal costs it comes first in config order;
//   - every ineligible candidate has an Exclusion, and only those do;
//   - with no eligible candidate, the error lists every candidate.
//
// rapid is not a module dependency, so the generator is a seeded
// math/rand/v2 source: every run checks the same cases, and a failure
// names its iteration.
func TestSelectProperties(t *testing.T) {
	const iterations = 3000
	rng := rand.New(rand.NewPCG(20261111, 2))
	vocab := schema.Vocabulary()
	for i := range iterations {
		cfg, model := randomConfig(rng, vocab)
		c, err := New(cfg)
		if err != nil {
			t.Fatalf("iteration %d: New: %v", i, err)
		}
		name := model
		if rng.IntN(4) == 0 {
			d := cfg.Models[0].Deployments[rng.IntN(len(cfg.Models[0].Deployments))]
			name = model + "@" + d.Provider
		}
		res, err := c.Resolve(name)
		if err != nil {
			t.Fatalf("iteration %d: Resolve(%q): %v", i, name, err)
		}
		required := randomCaps(rng, vocab, 4)
		estIn, estOut := rng.Int64N(300000), rng.Int64N(20000)
		drop := rng.IntN(2) == 0

		sel, err := c.Select(res, required, estIn, estOut, drop)
		if msg := checkSelection(res, required, estIn, estOut, drop, sel, err); msg != "" {
			t.Fatalf("iteration %d (model %s, required %v, in %d, out %d, drop %v): %s",
				i, name, required.Sorted(), estIn, estOut, drop, msg)
		}
	}
}

// randomConfig returns a config with one model of 1 to 5 deployments with
// random capabilities, prices and context window, and the model's ID.
func randomConfig(rng *rand.Rand, vocab []schema.Capability) (*config.Config, string) {
	cfg := &config.Config{}
	m := config.Model{ID: "m", Type: config.ModelTypeChat, Capabilities: capNames(randomCaps(rng, vocab, 8))}
	if rng.IntN(3) > 0 {
		m.ContextWindow = 1000 + rng.Int64N(400000)
	}
	for j := range 1 + rng.IntN(5) {
		p := "p" + strconv.Itoa(j)
		cfg.Providers = append(cfg.Providers, config.Provider{ID: p, Type: "openai"})
		d := config.Deployment{Provider: p, UpstreamModel: "u"}
		if rng.IntN(2) == 0 {
			d.Capabilities = capNames(randomCaps(rng, vocab, 10))
		}
		// A few distinct prices make ties likely; some are unknown.
		prices := []string{"0", "0.5", "1", "3", "15"}
		if rng.IntN(5) > 0 {
			d.Pricing.Input = mustParse(prices[rng.IntN(len(prices))])
		}
		if rng.IntN(5) > 0 {
			d.Pricing.Output = mustParse(prices[rng.IntN(len(prices))])
		}
		m.Deployments = append(m.Deployments, d)
	}
	cfg.Models = []config.Model{m}
	return cfg, m.ID
}

func mustParse(s string) config.Price {
	p, err := config.ParsePrice(s)
	if err != nil {
		panic(err)
	}
	return p
}

// randomCaps returns up to n random capabilities from vocab.
func randomCaps(rng *rand.Rand, vocab []schema.Capability, n int) schema.CapabilitySet {
	s := schema.CapabilitySet{}
	for range rng.IntN(n + 1) {
		s[vocab[rng.IntN(len(vocab))]] = struct{}{}
	}
	return s
}

func capNames(s schema.CapabilitySet) []string {
	out := []string{}
	for _, c := range s.Sorted() {
		out = append(out, string(c))
	}
	return out
}

// eligible is the reference implementation of the hard filters: whether d
// can serve the request, and what it would drop.
func eligible(d *Deployment, required schema.CapabilitySet, estIn, estOut int64, drop bool) (bool, []schema.Capability) {
	var dropped []schema.Capability
	noSampling := required.Has(schema.CapSampling) && !d.Capabilities.Has(schema.CapSampling)
	for _, c := range required.Sorted() {
		if d.Capabilities.Has(c) {
			continue
		}
		always := c == schema.CapSampling || (noSampling && c == schema.CapTemperature2)
		if !always && (!drop || !c.Droppable()) {
			return false, nil
		}
		dropped = append(dropped, c)
	}
	if cw := d.Model.ContextWindow; cw > 0 && estIn+estOut > cw {
		return false, nil
	}
	return true, dropped
}

// checkSelection returns a description of the first property sel violates,
// or "".
func checkSelection(res Resolution, required schema.CapabilitySet, estIn, estOut int64, drop bool, sel Selection, err error) string {
	ok := make(map[*Deployment]bool)
	var anyEligible bool
	for _, d := range res.Candidates {
		ok[d], _ = eligible(d, required, estIn, estOut, drop)
		anyEligible = anyEligible || ok[d]
	}
	if !anyEligible {
		var ne *NoEligibleError
		if !errors.As(err, &ne) {
			return fmt.Sprintf("no candidate is eligible, but Select returned %v, %v", sel.Deployment, err)
		}
		if len(ne.Exclusions) != len(res.Candidates) {
			return fmt.Sprintf("%d exclusions for %d candidates", len(ne.Exclusions), len(res.Candidates))
		}
		for i, x := range ne.Exclusions {
			if x.Deployment != res.Candidates[i].ID || x.Reason == "" {
				return fmt.Sprintf("exclusion %d = %+v", i, x)
			}
		}
		return ""
	}
	if err != nil {
		return "a candidate is eligible, but Select failed: " + err.Error()
	}
	chosen := sel.Deployment
	if !ok[chosen] {
		return "chose ineligible " + chosen.ID
	}

	// Every required capability is present or dropped, and only droppable
	// ones, under drop, are dropped.
	_, wantDropped := eligible(chosen, required, estIn, estOut, drop)
	if fmt.Sprint(wantDropped) != fmt.Sprint(sel.Dropped) {
		return fmt.Sprintf("dropped %v, want %v", sel.Dropped, wantDropped)
	}
	for _, c := range required.Sorted() {
		dropped := false
		for _, x := range sel.Dropped {
			dropped = dropped || x == c
		}
		if !chosen.Capabilities.Has(c) && !dropped {
			return fmt.Sprintf("%s lacks required %s, which was not dropped", chosen.ID, c)
		}
		if dropped && !c.Droppable() {
			return fmt.Sprintf("dropped hard capability %s", c)
		}
		if dropped && !drop && c != schema.CapSampling && c != schema.CapTemperature2 {
			return fmt.Sprintf("dropped %s without the drop flag", c)
		}
		if !dropped && c == schema.CapSampling && !chosen.Capabilities.Has(c) {
			return "sampling was not dropped"
		}
	}

	// No eligible candidate ranks before the chosen one.
	cost := func(d *Deployment) float64 {
		return float64(estIn)*d.Pricing.Input/1e6 + float64(estOut)*d.Pricing.Output/1e6
	}
	before := true // candidates before chosen in config order
	for _, d := range res.Candidates {
		if d == chosen {
			before = false
			continue
		}
		if !ok[d] {
			continue
		}
		switch {
		case d.Pricing.Known && !chosen.Pricing.Known:
			return fmt.Sprintf("chose %s with unknown pricing over %s with known pricing", chosen.ID, d.ID)
		case d.Pricing.Known && cost(d) < cost(chosen):
			return fmt.Sprintf("%s costs %g, less than chosen %s at %g", d.ID, cost(d), chosen.ID, cost(chosen))
		case before && d.Pricing.Known == chosen.Pricing.Known && (!d.Pricing.Known || cost(d) == cost(chosen)):
			return fmt.Sprintf("%s ties with %s but comes first in config order", d.ID, chosen.ID)
		}
	}

	// Exactly the ineligible candidates are excluded, in order.
	var wantExcluded, gotExcluded []string
	for _, d := range res.Candidates {
		if !ok[d] {
			wantExcluded = append(wantExcluded, d.ID)
		}
	}
	for _, x := range sel.Exclusions {
		gotExcluded = append(gotExcluded, x.Deployment)
	}
	if fmt.Sprint(wantExcluded) != fmt.Sprint(gotExcluded) {
		return fmt.Sprintf("excluded %v, want %v", gotExcluded, wantExcluded)
	}
	return ""
}
