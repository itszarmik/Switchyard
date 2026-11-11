// Package quality estimates whether a model is good enough for a request
// (DESIGN §10): an Assessor rates the request's category and difficulty, and
// Estimate combines that with a model's quality prior.
package quality

import (
	"switchyard/internal/schema"
)

// Categories (DESIGN §10.2). A model without a prior for a category uses
// its general prior.
const (
	CategoryGeneral    = "general"
	CategoryCoding     = "coding"
	CategoryReasoning  = "reasoning"
	CategoryExtraction = "extraction"
	CategoryCreative   = "creative"
	CategoryToolUse    = "tool_use"
)

// Hints are the caller's own view of the request, from the
// X-Switchyard-Task and X-Switchyard-Difficulty headers. They override the
// heuristic.
type Hints struct {
	// Task is a category name, or "".
	Task string
	// Difficulty is in [0, 1]; it applies only when HasDifficulty is true.
	Difficulty    float64
	HasDifficulty bool
}

// Assessment is the outcome of assessing one request.
type Assessment struct {
	// Category is one of the Category constants.
	Category string
	// Difficulty is in [0, 1].
	Difficulty float64
	// Signals names the heuristic signals that fired, for explanations
	// (e.g. "code_block", "long_prompt"). Never request content.
	Signals []string
	// Hinted reports that the caller's hints decided Category or
	// Difficulty.
	Hinted bool
}

// Assessor rates a request. Implementations must be safe for concurrent use
// and must not call the network (DESIGN §10.3).
type Assessor interface {
	Assess(req *schema.ChatRequest, h Hints) Assessment
}

// Estimate returns q̂ for a model with quality prior q (for the
// assessment's category) on a request of difficulty d, plus the
// deployment's quality offset, clamped to [0, 1] (DESIGN §10.4):
//
//	q̂ = 1 − d·(1 − q) + offset
func Estimate(q, offset float64, a Assessment) float64 {
	v := 1 - a.Difficulty*(1-q) + offset
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}
