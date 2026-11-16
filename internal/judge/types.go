// Package judge compares a candidate answer with a reference answer using an
// LLM judge (DESIGN §10.7). Both `switchyard eval` and shadow sampling use it.
//
// Judging sends the prompt and both answers to the judge deployment.
package judge

import (
	"context"

	"switchyard/internal/schema"
)

// Outcomes of one comparison.
const (
	// OutcomeCandidateBetter: the judge preferred the candidate.
	OutcomeCandidateBetter = "candidate_better"
	// OutcomeEquivalent: the answers are of equal quality.
	OutcomeEquivalent = "equivalent"
	// OutcomeReferenceBetter: the judge preferred the reference.
	OutcomeReferenceBetter = "reference_better"
	// OutcomeInconsistent: the two presentation orders disagreed.
	OutcomeInconsistent = "inconsistent"
)

// Pair is one comparison to judge.
type Pair struct {
	// Request is the client's chat request (the prompt both answers respond
	// to).
	Request *schema.ChatRequest
	// Category is the assessed quality category, for the judge's rubric.
	Category string
	// Candidate and Reference are the answers' text (content, plus tool
	// calls rendered as text).
	Candidate, Reference string
}

// Verdict is the judge's decision on a Pair.
type Verdict struct {
	// Outcome is one of the Outcome constants.
	Outcome string
	// Acceptable reports whether the candidate is good enough to replace
	// the reference: Outcome is candidate_better or equivalent.
	Acceptable bool
	// JudgeModel is the deployment that judged ("model@provider").
	JudgeModel string
	// CostNano is the judge calls' cost in nano-USD, rounded up: each call
	// at its reported usage, and each call that reached the upstream but
	// reported no usage (a timeout, a transport error, a reply without
	// usage) or that failed in a way that may have been billed
	// (providers.Kind.MayHaveBilled) at its estimate (see
	// LLM.CallCostNano), so that it is never less than the provider could
	// bill. It is -1 when that is unknown: such a call was made and the
	// deployment's prices are unknown.
	CostNano int64
}

// Judge compares answers. Implementations must be safe for concurrent use.
type Judge interface {
	Judge(ctx context.Context, p Pair) (Verdict, error)
}

// Meter accounts for the upstream calls of one judgment as they happen, so
// that a caller with a budget reserves each call's worst case just before
// it is made, rather than every call's at once.
type Meter interface {
	// Reserve is called before each upstream call with the call's worst
	// case in nano-USD: its input at the bound of router.InputTokenBound
	// and its output at the judge's max tokens. When it returns an error,
	// the call is not made and the judgment fails with that error.
	Reserve(ctx context.Context, estNano int64) error
	// Settle is called after each call Reserve admitted, with the call's
	// charge in nano-USD, priced as in Verdict.CostNano, or -1 when that
	// is unknown.
	Settle(costNano int64)
}

// MeteredJudge is a Judge that can report each upstream call to a Meter.
// *LLM implements it.
type MeteredJudge interface {
	Judge
	// JudgeMetered is Judge, with m told of every upstream call. A nil m
	// makes it Judge.
	JudgeMetered(ctx context.Context, p Pair, m Meter) (Verdict, error)
}
