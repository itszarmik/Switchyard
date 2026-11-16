// Package shadow measures the quality of routed traffic by shadow sampling
// (DESIGN §10.7): for a deterministic sample of requests, it asks a
// comparison deployment for a second answer in the background, has an LLM
// judge compare the two, and stores the verdict, joined to the request's
// usage event, so reports can show which categories' savings are verified.
//
// In enforce mode the served answer is the candidate and the model the
// request's savings are measured against gives the reference (the
// policy's baseline model for auto, the requested model for an equivalent
// substitute; requests served by the model they name are not sampled); in observe mode the deployment the
// router would have chosen gives the candidate and the served baseline
// answer is the reference. Either way the judge decides whether the
// cheaper answer could replace the baseline's.
//
// Shadow sampling sends the request and both answers to the comparison and
// judge deployments, which must pass the request's policy. It is opt-in per
// policy, runs after the client's response, bounded by a concurrency
// limit, a queue bounded in samples and bytes, per-call output and time
// limits, and a daily per-tenant budget in the budget store (scope
// shadow:<tenant>/day), reserved for each call at its worst case before it
// is made and settled at the actual cost after. Content is never logged
// or stored: results hold verdicts and metadata only.
package shadow

import (
	"crypto/sha256"
	"encoding/binary"
	"time"

	"switchyard/internal/catalog"
	"switchyard/internal/policy"
	"switchyard/internal/schema"
)

// Routing modes of a Sample, as the usage events record them.
const (
	ModeEnforce = "enforce"
	ModeObserve = "observe"
)

// Outcomes counted in switchyard_shadow_samples_total.
const (
	// OutcomeJudged: the judge returned a verdict, which was stored.
	OutcomeJudged = "judged"
	// OutcomeDroppedBudget: the tenant's daily shadow budget had no room
	// for the comparison call, or for a judge call (the sample is then
	// stored with ErrorJudgeBudget).
	OutcomeDroppedBudget = "dropped_budget"
	// OutcomeDroppedQueue: the queue was full, or the runner closed before
	// the sample started.
	OutcomeDroppedQueue = "dropped_queue"
	// OutcomeDroppedPolicy: the sample's policy does not permit the
	// comparison or the judge deployment.
	OutcomeDroppedPolicy = "dropped_policy"
	// OutcomeError: the sample was invalid or could not be priced, the
	// budget store failed, or the comparison or the judge call failed.
	OutcomeError = "error"
)

// Error kinds stored in usage.ShadowEval.ErrorKind when an evaluation that
// was admitted produced no verdict. A comparison failure is
// ErrorComparison followed by the failure kind (providers.Kind), such as
// "comparison_timeout".
const (
	ErrorComparison     = "comparison_"
	ErrorNoAnswer       = "comparison_no_answer"
	ErrorAnswerTooLarge = "comparison_answer_too_large"
	ErrorJudge          = "judge_failed"
	// ErrorJudgeBudget: the budget refused a judge call; the sample stops
	// with its spend so far charged and is counted as dropped_budget.
	ErrorJudgeBudget = "judge_budget"
	ErrorCanceled    = "canceled"
)

// Answer is one answer to a sampled request.
type Answer struct {
	// Deployment gave the answer (model@provider).
	Deployment string
	// Text is the answer as the judge sees it (see ResponseText and
	// TextBuilder). It is sent to the judge and never stored.
	Text string
}

// Sample is one request offered for shadow evaluation.
type Sample struct {
	// RequestID, Tenant and KeyID identify the request; RequestID is its
	// usage event's ID, which the result joins to.
	RequestID, Tenant, KeyID string
	// Time is when the request arrived (its usage event's Time).
	Time time.Time
	// Request is the request as served, after dropped parameters. The
	// comparison call sends it without streaming, with its output capped.
	Request *schema.ChatRequest
	// Category and Difficulty are the request's assessment.
	Category   string
	Difficulty float64
	// Served is the answer the client received.
	Served Answer
	// Comparison is the deployment asked for the other answer: the
	// baseline model's in enforce mode, the router's choice in observe
	// mode.
	Comparison *catalog.Deployment
	// Mode is ModeEnforce (the candidate is Served and the reference
	// Comparison's answer) or ModeObserve (the candidate is Comparison's
	// answer and the reference Served).
	Mode string
	// Profile is the request's routing profile.
	Profile string
	// Policy is the request's effective policy. When set, the sample is
	// dropped unless it permits both the comparison and the judge
	// deployment (router.PolicyReason).
	Policy *policy.Effective
}

// roles returns the candidate and reference deployments and texts of s,
// given the comparison answer.
func (s *Sample) roles(comparison string) (candDep, candText, refDep, refText string) {
	if s.Mode == ModeObserve {
		return s.Comparison.ID, comparison, s.Served.Deployment, s.Served.Text
	}
	return s.Served.Deployment, s.Served.Text, s.Comparison.ID, comparison
}

// Sampled reports whether the request with the given ID is in a sample of
// the given rate, in [0, 1]. The decision is a hash of the ID, so every
// replica, and every test run, decides the same for the same request.
func Sampled(requestID string, rate float64) bool {
	switch {
	case !(rate > 0): // NaN too
		return false
	case rate >= 1:
		return true
	}
	h := sha256.Sum256([]byte(requestID))
	// The top 53 bits as a uniform fraction in [0, 1).
	u := float64(binary.BigEndian.Uint64(h[:8])>>11) / (1 << 53)
	return u < rate
}
