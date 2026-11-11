package router

import (
	"math"
	"strconv"
)

// Explanation is the routing decision and its reasons, in the DESIGN §9.7
// shape. It is JSON-serializable and deterministic: the same Input and the
// same catalog and health snapshot give byte-identical JSON. Money is a
// decimal string of USD; quality figures are rounded to 4 decimal places.
// Slices are never nil, so they encode as [] rather than null.
type Explanation struct {
	// Policy is the base policy's ID ("" for the built-in defaults).
	Policy string `json:"policy"`
	// MatchedRule is the governance rule that applied, or "".
	MatchedRule string `json:"matched_rule"`
	// MatchedQualityRule is the quality rule that applied on top of it,
	// or "".
	MatchedQualityRule string `json:"matched_quality_rule"`
	// Profile is the profile in force.
	Profile string `json:"profile"`
	// Mode is how candidates were chosen: ModeAuto, ModePinned or
	// ModeEquivalent.
	Mode string `json:"mode"`
	// Downgrade explains a budget soft-threshold profile downgrade, such
	// as "budget acme/month soft threshold crossed; profile cheap", or "".
	Downgrade string `json:"downgrade,omitempty"`
	// Assessment is the request's assessment.
	Assessment AssessmentInfo `json:"assessment"`
	// Estimates are the token estimates used for cost and context. The
	// input is at the requested model's token_ratio, or 1 for auto; each
	// candidate's cost uses its own model's.
	Estimates Estimates `json:"estimates"`
	// QualityBar is the bar every eligible candidate's q̂ met, before any
	// relaxation: the profile's min_quality as adjusted by rules and
	// headers in auto mode; in equivalent mode the requested model's q̂
	// minus max_quality_drop, or the quality floor of rules and headers if
	// higher; 0 in pinned mode, which does not filter on quality.
	QualityBar float64 `json:"quality_bar"`
	// Candidates are every candidate considered, in catalog order.
	Candidates []CandidateInfo `json:"candidates"`
	// Plan is the deployments the request may be sent to, primary first.
	Plan []string `json:"plan"`
	// Relaxed lists the soft checks dropped, in order: RelaxedLatency,
	// RelaxedQuality.
	Relaxed []string `json:"relaxed"`
	// DroppedParams lists, sorted, the capabilities whose parameters are
	// removed for the primary deployment.
	DroppedParams []string `json:"dropped_params"`
	// Warnings are the policy's warnings about ignored request headers.
	Warnings []string `json:"warnings"`
}

// AssessmentInfo is the assessment as explained. It never holds request
// content.
type AssessmentInfo struct {
	Category   string   `json:"category"`
	Difficulty float64  `json:"difficulty"`
	Signals    []string `json:"signals"`
	Hinted     bool     `json:"hinted"`
}

// Estimates are the token estimates of the request.
type Estimates struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// CandidateInfo explains one candidate deployment.
type CandidateInfo struct {
	// Deployment is the deployment ID, model@provider.
	Deployment string `json:"deployment"`
	// Model is the catalog model ID.
	Model string `json:"model"`
	// Q is q̂ for the request; 0 for a candidate that is not a chat model.
	Q float64 `json:"q"`
	// EstCostUSD is the estimated cost, or "" when pricing is unknown.
	EstCostUSD string `json:"est_cost_usd,omitempty"`
	// EstTTFTMs is the estimated time to first token in milliseconds.
	EstTTFTMs int64 `json:"est_ttft_ms"`
	// Tier is the index of the first policy prefer glob that matches the
	// deployment, or the number of globs when none does.
	Tier int `json:"tier"`
	// Eligible reports that the candidate passed every hard filter and the
	// soft checks in force after relaxation.
	Eligible bool `json:"eligible"`
	// ExcludedBy is why an ineligible candidate was excluded.
	ExcludedBy string `json:"excluded_by,omitempty"`
	// Rank is the 1-based position of an eligible candidate in the
	// ranking; 0 (omitted) for an ineligible one.
	Rank int `json:"rank,omitempty"`
	// Notes flag estimates that used a fallback: NoteNoQualityPrior,
	// NoteNoLatency, NotePricingUnknown.
	Notes []string `json:"notes,omitempty"`
}

// Candidate notes.
const (
	// NoteNoQualityPrior: the model has no prior for the category or for
	// general, so q = 0.5 was used.
	NoteNoQualityPrior = "no quality prior"
	// NoteNoLatency: neither measurements nor a latency hint exist, so
	// est_ttft = 1000 ms was assumed.
	NoteNoLatency = "no latency estimate; assumed 1000ms"
	// NotePricingUnknown: the deployment's prices are not set.
	NotePricingUnknown = "pricing unknown"
)

// round4 rounds v to 4 decimal places for display.
func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

// formatPair formats two quality figures, a < b, for a reason: rounded to
// 4 decimal places, or in full when rounding would make them look equal.
func formatPair(a, b float64) (string, string) {
	ra, rb := round4(a), round4(b)
	if ra >= rb {
		return strconv.FormatFloat(a, 'f', -1, 64), strconv.FormatFloat(b, 'f', -1, 64)
	}
	return strconv.FormatFloat(ra, 'f', -1, 64), strconv.FormatFloat(rb, 'f', -1, 64)
}
