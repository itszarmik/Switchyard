package conformance

import (
	"testing"

	"switchyard/test/scenario"
)

// The P4 acceptance cases (docs/PLAN.md) through the gateway binary, with
// the memory ledger and memory budgets. test/integration runs the same
// scenarios with the Postgres ledger and Redis budgets.

// TestCostAcceptance: fixture requests to each provider (OpenAI wire,
// Anthropic, embeddings) cost exactly the hand-computed decimals, cached
// input and cache writes included, in X-Switchyard-Cost-Usd and in the
// ledger; and the ledger total equals the cost_usd_total metric.
func TestCostAcceptance(t *testing.T) {
	scenario.ExactCost(t, scenario.Options{Race: raceEnabled})
}

// TestBudgetAcceptance: 1,000 concurrent requests against a budget with
// room for 50. With exact estimates the final spend never exceeds the
// limit; with under-estimates it exceeds it by at most their sum.
// Rejections are 429 budget_exceeded with Retry-After and never reach the
// upstream.
func TestBudgetAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra int
	}{{"exact estimates", 0}, {"under-estimates", 37}} {
		t.Run(tc.name, func(t *testing.T) {
			scenario.BudgetConcurrency(t, scenario.Options{Race: raceEnabled}, 1000, tc.extra)
		})
	}
}

// TestCostAnthropicAbortedStream: an Anthropic stream that breaks off
// before message_delta is recorded with its reported input and its
// relayed output, estimated, not message_start's partial output count.
func TestCostAnthropicAbortedStream(t *testing.T) {
	scenario.AnthropicAbortedStream(t, scenario.Options{Race: raceEnabled})
}
