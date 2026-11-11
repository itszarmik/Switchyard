package router

import (
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"path"
	"slices"
	"strconv"
	"testing"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/config"
	"switchyard/internal/health"
	"switchyard/internal/policy"
	"switchyard/internal/quality"
	"switchyard/internal/schema"
)

// TestDecideProperties checks Decide on random catalogs, policies, health
// states and requests, with relaxation off (on_unmet reject), against an
// oracle written independently of the router:
//
//   - every planned deployment satisfies every hard constraint (model
//     type, deny and allow, required tags, capabilities, context, cost cap
//     and budget, health) and the soft ones (quality bar, latency target);
//   - no eligible deployment is in a better preference tier than the
//     primary, and none in its tier is cheaper (or, with the latency
//     objective, faster);
//   - Decide fails exactly when the oracle finds nothing eligible.
//
// rapid is not a module dependency, so the generator is a seeded
// math/rand/v2 source: every run checks the same cases, and a failure
// names its iteration.
func TestDecideProperties(t *testing.T) {
	const iterations = 3000
	rng := rand.New(rand.NewPCG(20261111, 5))
	vocab := schema.Vocabulary()
	// Outcome counts, so that a generator that stops producing plans (or
	// failures) is noticed.
	outcomes := map[string]int{}
	for i := range iterations {
		cat, cfg := randomCatalog(t, rng, vocab)
		tr := newTracker()
		for _, m := range cfg.Models {
			for _, d := range m.Deployments {
				if rng.IntN(8) == 0 {
					openBreaker(t, tr, m.ID+"@"+d.Provider, d.Provider)
				}
			}
		}
		r := New(cat, tr, config.Routing{Fallback: config.Fallback{MaxDeployments: 1 + rng.IntN(4)}})
		in := randomInput(t, rng, cat, cfg, vocab)
		dec, err := r.Decide(in)
		if msg := checkDecision(cat, tr, in, dec, err); msg != "" {
			t.Fatalf("iteration %d (requested %q, mode %s): %s", i, in.Requested, dec.Explanation.Mode, msg)
		}
		switch {
		case err != nil:
			outcomes["no eligible"]++
		case dec.Unhealthy:
			outcomes["unhealthy"]++
		default:
			outcomes["planned "+dec.Explanation.Mode]++
		}
	}
	t.Logf("outcomes: %v", outcomes)
	for _, k := range []string{"no eligible", "unhealthy", "planned auto", "planned pinned", "planned equivalent"} {
		if outcomes[k] < iterations/100 {
			t.Errorf("only %d of %d iterations ended %s", outcomes[k], iterations, k)
		}
	}
}

var (
	propPrices = []string{"", "0", "0.0375", "0.1", "0.15", "1", "2.5", "3", "15"}
	propTags   = []string{"zdr", "eu", "self_hosted"}
	categories = []string{
		quality.CategoryGeneral, quality.CategoryCoding, quality.CategoryReasoning,
		quality.CategoryExtraction, quality.CategoryCreative, quality.CategoryToolUse,
	}
)

// randomCatalog returns 1 to 4 providers and 1 to 8 models of 1 to 4
// deployments each.
func randomCatalog(tb testing.TB, rng *rand.Rand, vocab []schema.Capability) (*catalog.Catalog, *config.Config) {
	cfg := &config.Config{}
	np := 1 + rng.IntN(4)
	for p := range np {
		cfg.Providers = append(cfg.Providers, config.Provider{ID: "p" + strconv.Itoa(p), Type: "openai", Tags: randomSubset(rng, propTags)})
	}
	for m := range 1 + rng.IntN(8) {
		cm := config.Model{ID: "m" + strconv.Itoa(m), Type: config.ModelTypeChat, Capabilities: likelyCaps(rng, vocab)}
		if rng.IntN(10) == 0 {
			cm.Type = config.ModelTypeEmbedding
		}
		cm.ContextWindow = []int64{0, 4000, 32000, 200000}[rng.IntN(4)]
		cm.TokenRatio = []float64{0, 1, 0.5, 1.3, 4}[rng.IntN(5)] // 0 is the default, 1
		if rng.IntN(5) > 0 {
			cm.Quality = map[string]float64{}
			for _, c := range categories {
				if rng.IntN(5) < 3 {
					cm.Quality[c] = 0.3 + 0.7*rng.Float64()
				}
			}
		}
		for _, p := range rng.Perm(np)[:1+rng.IntN(np)] {
			cd := config.Deployment{
				Provider: "p" + strconv.Itoa(p), UpstreamModel: "u",
				Pricing: config.Pricing{
					Input:  price(tb, propPrices[rng.IntN(len(propPrices))]),
					Output: price(tb, propPrices[rng.IntN(len(propPrices))]),
				},
				Tags:          randomSubset(rng, propTags),
				QualityOffset: rng.Float64()*0.2 - 0.1,
				LatencyHint:   config.LatencyHint{TTFTMs: []int{0, 100, 400, 900, 2500}[rng.IntN(5)]},
			}
			if rng.IntN(3) == 0 {
				cd.Capabilities = likelyCaps(rng, vocab)
			}
			cm.Deployments = append(cm.Deployments, cd)
		}
		cfg.Models = append(cfg.Models, cm)
	}
	c, err := catalog.New(cfg)
	if err != nil {
		tb.Fatalf("catalog.New: %v", err)
	}
	return c, cfg
}

// randomInput returns an auto, concrete or model@provider request with a
// random policy whose on_unmet is reject.
func randomInput(tb testing.TB, rng *rand.Rand, cat *catalog.Catalog, cfg *config.Config, vocab []schema.Capability) Input {
	in := autoInput()
	if k := rng.IntN(3); k > 0 {
		m := cfg.Models[rng.IntN(len(cfg.Models))]
		name := m.ID
		if k == 2 {
			name += "@" + m.Deployments[rng.IntN(len(m.Deployments))].Provider
		}
		res, err := cat.Resolve(name)
		if err != nil {
			tb.Fatalf("Resolve(%q): %v", name, err)
		}
		in.Requested, in.Auto, in.Resolution = name, false, res
	}
	in.Required = randomCaps(rng, vocab, 2)
	in.InputBytes, in.EstOut = rng.Int64N(200000), rng.Int64N(5000)
	if rng.IntN(4) == 0 {
		in.ImageTokens = 1600 * (1 + rng.Int64N(3))
	}
	in.Drop = rng.IntN(2) == 0
	in.Assessment = quality.Assessment{Category: categories[rng.IntN(len(categories))], Difficulty: rng.Float64()}
	globs := []string{"*", "m0@*", "m1@*", "*@p0", "*@p1", "m2@p2", "m*@p3"}
	pick := func(n int) []string {
		var out []string
		for range rng.IntN(n + 1) {
			out = append(out, globs[rng.IntN(len(globs))])
		}
		return out
	}
	p := &in.Policy
	if rng.IntN(3) == 0 {
		p.Allow = pick(2)
	}
	if rng.IntN(3) == 0 {
		p.Deny = pick(1)
	}
	p.Prefer = pick(3)
	if rng.IntN(4) == 0 {
		p.RequireTags = randomSubset(rng, propTags[:2])
	}
	if rng.IntN(3) == 0 {
		p.MaxCostPerRequestNano = rng.Int64N(200_000_000)
	}
	if rng.IntN(3) == 0 {
		in.BudgetRemainingMicro = rng.Int64N(200000)
	}
	p.Constraints.MinQuality = 0.5 + 0.45*rng.Float64()
	if rng.IntN(2) == 0 {
		p.Constraints.MaxTTFT = time.Duration([]int{200, 1000, 3000}[rng.IntN(3)]) * time.Millisecond
	}
	if rng.IntN(3) == 0 {
		p.Constraints.Objective = policy.ObjectiveLatency
	}
	if rng.IntN(2) == 0 {
		p.Substitution = policy.SubstitutionEquivalent
		p.MaxQualityDrop = 0.1 * rng.Float64()
		if rng.IntN(2) == 0 {
			p.Constraints.QualityFloor = 0.5 + 0.45*rng.Float64()
		}
	}
	p.OnUnmet = policy.OnUnmetReject
	return in
}

func randomSubset(rng *rand.Rand, from []string) []string {
	var out []string
	for _, s := range from {
		if rng.IntN(2) == 0 {
			out = append(out, s)
		}
	}
	return out
}

// randomCaps returns up to n random capabilities.
func randomCaps(rng *rand.Rand, vocab []schema.Capability, n int) schema.CapabilitySet {
	s := schema.CapabilitySet{}
	for range rng.IntN(n + 1) {
		s[vocab[rng.IntN(len(vocab))]] = struct{}{}
	}
	return s
}

// likelyCaps returns each capability with probability 3/4.
func likelyCaps(rng *rand.Rand, vocab []schema.Capability) []string {
	out := []string{}
	for _, c := range vocab {
		if rng.IntN(4) > 0 {
			out = append(out, string(c))
		}
	}
	return out
}

// oracle is the reference view of one candidate.
type oracle struct {
	d    *catalog.Deployment
	ok   bool
	cost *big.Rat // nil when pricing is unknown
	ttft time.Duration
	tier int
}

// checkDecision returns a description of the first broken invariant, or "".
func checkDecision(cat *catalog.Catalog, tr *health.Tracker, in Input, dec Decision, err error) string {
	p := in.Policy
	pinned := !in.Auto && (in.Resolution.Pinned || p.Substitution != policy.SubstitutionEquivalent)
	var cands []*catalog.Deployment
	if pinned {
		cands = in.Resolution.Candidates
	} else {
		for _, m := range cat.Models() {
			cands = append(cands, m.Deployments...)
		}
	}
	bar := p.Constraints.MinQuality
	if !in.Auto {
		q := refPrior(in.Resolution.Model, in.Assessment.Category)
		bar = max(quality.Estimate(q, 0, in.Assessment)-p.MaxQualityDrop, p.Constraints.QualityFloor, 0)
	}
	var capRat *big.Rat
	if p.MaxCostPerRequestNano > 0 {
		capRat = big.NewRat(p.MaxCostPerRequestNano, 1_000_000_000)
	}
	if b := big.NewRat(in.BudgetRemainingMicro, 1_000_000); in.BudgetRemainingMicro >= 0 && (capRat == nil || b.Cmp(capRat) < 0) {
		capRat = b
	}

	var all []oracle
	byID := map[string]oracle{}
	for _, d := range cands {
		// The input estimate at the model's own token ratio: bytes/4 × ratio,
		// truncated, plus the images.
		estIn := int64(float64(in.InputBytes)/4*d.Model.TokenRatio) + in.ImageTokens
		o := oracle{d: d, cost: refCost(d, estIn, in.EstOut), ttft: d.LatencyHint.TTFT, tier: len(p.Prefer)}
		if o.ttft == 0 {
			o.ttft = time.Second
		}
		for i, g := range p.Prefer {
			if ok, _ := path.Match(g, d.ID); ok {
				o.tier = i
				break
			}
		}
		o.ok = d.Model.Type == config.ModelTypeChat &&
			!slices.ContainsFunc(p.Deny, func(g string) bool { ok, _ := path.Match(g, d.ID); return ok }) &&
			(len(p.Allow) == 0 || slices.ContainsFunc(p.Allow, func(g string) bool { ok, _ := path.Match(g, d.ID); return ok })) &&
			!slices.ContainsFunc(p.RequireTags, func(t string) bool { return !slices.Contains(d.Tags, t) }) &&
			refCapable(d, in.Required, in.Drop) &&
			(d.Model.ContextWindow == 0 || estIn+in.EstOut <= d.Model.ContextWindow) &&
			(capRat == nil || (o.cost != nil && o.cost.Cmp(capRat) <= 0)) &&
			tr.Deployment(d.ID, d.Provider, health.LatencyHint{TTFT: d.LatencyHint.TTFT}).Skip() == "" &&
			(pinned || quality.Estimate(refPrior(d.Model, in.Assessment.Category), d.QualityOffset, in.Assessment) >= bar-1e-9) &&
			(p.Constraints.MaxTTFT == 0 || o.ttft <= p.Constraints.MaxTTFT)
		all = append(all, o)
		byID[d.ID] = o
	}
	anyOK := slices.ContainsFunc(all, func(o oracle) bool { return o.ok })

	if err != nil || dec.Unhealthy {
		var ne *catalog.NoEligibleError
		if err != nil && !errors.As(err, &ne) {
			return "unexpected error " + err.Error()
		}
		if anyOK {
			return fmt.Sprintf("no plan (unhealthy %v, err %v), but the oracle finds an eligible deployment", dec.Unhealthy, err)
		}
		return ""
	}
	if !anyOK {
		return fmt.Sprintf("plan %v, but the oracle finds nothing eligible", planIDs(dec.Plan))
	}
	if len(dec.Plan) == 0 {
		return "empty plan"
	}
	for _, c := range dec.Plan {
		if o, found := byID[c.Deployment.ID]; !found || !o.ok {
			return "planned " + c.Deployment.ID + ", which the oracle finds ineligible"
		}
		for _, dc := range c.Dropped {
			if c.Deployment.Capabilities.Has(dc) || !in.Required.Has(dc) {
				return fmt.Sprintf("%s drops %s needlessly", c.Deployment.ID, dc)
			}
		}
	}
	best := byID[dec.Plan[0].Deployment.ID]
	latency := p.Constraints.Objective == policy.ObjectiveLatency
	for _, o := range all {
		if !o.ok {
			continue
		}
		if o.tier < best.tier {
			return fmt.Sprintf("%s (tier %d) is eligible, ahead of primary %s (tier %d)", o.d.ID, o.tier, best.d.ID, best.tier)
		}
		if o.tier > best.tier {
			continue
		}
		if latency && o.ttft < best.ttft {
			return fmt.Sprintf("%s (%v) is faster than primary %s (%v) in the same tier", o.d.ID, o.ttft, best.d.ID, best.ttft)
		}
		if !latency && refCheaper(o.cost, best.cost) {
			return fmt.Sprintf("%s (%v) is cheaper than primary %s (%v) in the same tier", o.d.ID, o.cost, best.d.ID, best.cost)
		}
	}
	if pinned && in.Resolution.Pinned && len(dec.Plan) != 1 {
		return "a model@provider plan has fallbacks"
	}
	return ""
}

// refPrior is the quality prior with its fallbacks.
func refPrior(m *catalog.Model, category string) float64 {
	if q, ok := m.Quality[category]; ok {
		return q
	}
	if q, ok := m.Quality["general"]; ok {
		return q
	}
	return 0.5
}

// refCost is the exact cost in USD, or nil when pricing is unknown.
func refCost(d *catalog.Deployment, in, out int64) *big.Rat {
	if !d.Pricing.Known {
		return nil
	}
	rat := func(p config.Price) *big.Rat {
		if !p.IsSet() {
			return new(big.Rat)
		}
		r, _ := new(big.Rat).SetString(p.String())
		return r
	}
	c := new(big.Rat).Mul(rat(d.Prices.Input), big.NewRat(in, 1_000_000))
	return c.Add(c, new(big.Rat).Mul(rat(d.Prices.Output), big.NewRat(out, 1_000_000)))
}

// refCheaper reports whether cost a ranks strictly before b: known before
// unknown, then lower.
func refCheaper(a, b *big.Rat) bool {
	if (a == nil) != (b == nil) {
		return a != nil
	}
	return a != nil && a.Cmp(b) < 0
}

// refCapable reports whether d can serve a request needing required.
func refCapable(d *catalog.Deployment, required schema.CapabilitySet, drop bool) bool {
	noSampling := required.Has(schema.CapSampling) && !d.Capabilities.Has(schema.CapSampling)
	for c := range required {
		switch {
		case d.Capabilities.Has(c), c == schema.CapSampling, drop && c.Droppable(), noSampling && c == schema.CapTemperature2:
		default:
			return false
		}
	}
	return true
}
