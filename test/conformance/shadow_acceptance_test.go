package conformance

import (
	"testing"

	"switchyard/test/scenario"
)

// The P8 acceptance cases (docs/PLAN.md) for shadow sampling, through the
// gateway binary with the memory ledger and memory budgets.
// test/integration runs them again with the Postgres ledger and Redis
// budgets, and checks the join in the database.

// TestShadowBudgetAcceptance: with shadow sampling at rate 1.0 and a tiny
// daily shadow budget, 60 concurrent requests all succeed in the same
// latency band as without sampling; the shadow spend stays within the
// budget, the samples that did not fit are counted as dropped, and every
// stored evaluation joins to its request's usage event.
func TestShadowBudgetAcceptance(t *testing.T) {
	scenario.ShadowBudget(t, scenario.Options{Race: raceEnabled}, 60)
}

// TestShadowVerifiedSavingsAcceptance: in observe mode with shadow
// sampling, a category whose cheaper answers the judge accepts is verified
// once it has verified_min_samples, with verified savings above 0; a
// category whose answers it rejects stays unverified.
func TestShadowVerifiedSavingsAcceptance(t *testing.T) {
	scenario.VerifiedSavings(t, scenario.Options{Race: raceEnabled}, 15)
}
