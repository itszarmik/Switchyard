package usage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"time"
	"unicode/utf8"

	"switchyard/internal/config"
	"switchyard/internal/quality"
)

// ShadowEval is the stored result of one shadow-sampled request (DESIGN
// §10.7): which answers were compared, and the judge's verdict. It joins
// to the request's Event by RequestID (Event.ID) and Tenant. It never holds
// the prompt or the answers.
type ShadowEval struct {
	// RequestID is the sampled request's ID, Event.ID.
	RequestID string
	// Time is when the sampled request arrived, Event.Time.
	Time time.Time
	// Tenant and KeyID identify the caller of the sampled request.
	Tenant, KeyID string
	// Category and Difficulty are the request's assessment.
	Category   string
	Difficulty float64
	// Profile is the request's routing profile, and Mode its routing mode
	// (RoutingModeEnforce or RoutingModeObserve).
	Profile, Mode string
	// CandidateDeployment gave the answer being judged and
	// ReferenceDeployment the answer it was judged against: in enforce mode
	// the served deployment against the baseline's, and in observe mode the
	// deployment the router would have chosen against the served baseline.
	CandidateDeployment, ReferenceDeployment string
	// JudgeModel is the judge deployment (model@provider).
	JudgeModel string
	// Outcome is the judge's outcome (judge.Outcome*), or "" when ErrorKind
	// is set. Acceptable reports whether the candidate was judged good
	// enough to replace the reference.
	Outcome    string
	Acceptable bool
	// Cost is what the comparison and judge calls cost: the exact cost
	// where the usage and prices were known, and the reserved estimate
	// where they were not, as charged to the shadow budget.
	Cost Money
	// ErrorKind says why the evaluation produced no verdict, or is "" when
	// it did.
	ErrorKind string
}

// Feedback is a client's score for one of its tenant's requests (POST
// /switchyard/v1/feedback). It joins to the request's Event by RequestID
// and Tenant.
type Feedback struct {
	// RequestID is the scored request's ID, Event.ID.
	RequestID string
	// Tenant and KeyID identify who gave the feedback.
	Tenant, KeyID string
	// Time is when the feedback was given.
	Time time.Time
	// Score is -1, 0 or 1.
	Score int
	// Comment is the client's optional comment, at most 500 characters. It
	// is stored and never logged.
	Comment string
}

// ShadowStore stores shadow evaluations. Implementations are safe for
// concurrent use.
type ShadowStore interface {
	// RecordShadowEval stores e. A second evaluation of the same tenant and
	// request ID is ignored: the first is kept.
	RecordShadowEval(ctx context.Context, e *ShadowEval) error
}

// FeedbackStore stores feedback. Implementations are safe for concurrent
// use.
type FeedbackStore interface {
	// RecordFeedback stores f, replacing any earlier feedback of the same
	// tenant on the same request. It returns ErrUnknownRequest when the
	// store can tell that f's tenant made no request with that ID; a store
	// that cannot tell (its ledger is written asynchronously) stores the
	// feedback with the tenant, and reports join it to the tenant's own
	// requests only.
	RecordFeedback(ctx context.Context, f *Feedback) error
}

// ErrUnknownRequest is returned by RecordFeedback when the tenant has no
// request with the given ID.
var ErrUnknownRequest = errors.New("usage: no such request for this tenant")

// checkShadowEval returns an error unless e can be stored.
func checkShadowEval(e *ShadowEval) error {
	if e == nil || e.RequestID == "" || e.Tenant == "" {
		return errors.New("usage: a shadow evaluation needs a request ID and a tenant")
	}
	return nil
}

// checkFeedback returns an error unless f can be stored. The error never
// quotes f.
func checkFeedback(f *Feedback) error {
	switch {
	case f == nil || f.RequestID == "" || f.Tenant == "":
		return errors.New("usage: feedback needs a request ID and a tenant")
	case f.Score < ScoreNegative || f.Score > ScorePositive:
		return errors.New("usage: a feedback score must be -1, 0 or 1")
	case utf8.RuneCountInString(f.Comment) > MaxFeedbackComment:
		return fmt.Errorf("usage: a feedback comment may have at most %d characters", MaxFeedbackComment)
	}
	return nil
}

// errClosed is returned by a MemoryLedger's stores after Close.
var errClosed = errors.New("usage: the ledger is closed")

// Feedback scores.
const (
	ScoreNegative = -1
	ScoreNeutral  = 0
	ScorePositive = 1
)

// MaxFeedbackComment is the longest Feedback.Comment, in characters.
const MaxFeedbackComment = 500

// reportCategories are the quality categories reports break shadow
// verification down by (DESIGN §10.2), in report order. An event of any
// other category, or of none, counts in no category.
var reportCategories = [...]string{
	quality.CategoryCoding, quality.CategoryCreative, quality.CategoryExtraction,
	quality.CategoryGeneral, quality.CategoryReasoning, quality.CategoryToolUse,
}

// categoryIndex returns c's index in reportCategories, or -1.
func categoryIndex(c string) int { return slices.Index(reportCategories[:], c) }

// WilsonZ is the normal quantile of the 90% two-sided Wilson score
// interval whose lower bound decides verification.
const WilsonZ = 1.6448536269514722

// WilsonLower returns the lower bound of the 90% Wilson score interval of
// a pass rate of acceptable out of n, or 0 when n or acceptable is 0.
func WilsonLower(acceptable, n int64) float64 {
	if n <= 0 || acceptable <= 0 {
		return 0
	}
	nf := float64(n)
	p := float64(acceptable) / nf
	z2 := WilsonZ * WilsonZ
	center := p + z2/(2*nf)
	margin := WilsonZ * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf))
	return max((center-margin)/(1+z2/nf), 0)
}

// Verification is the rule that marks a category's savings as verified:
// at least MinSamples judged shadow samples, and a Wilson lower bound
// (WilsonLower) of their pass rate of at least MinPassRate.
type Verification struct {
	MinSamples  int
	MinPassRate float64
}

// verified reports whether acceptable out of judged meets v.
func (v Verification) verified(acceptable, judged int64) bool {
	return judged >= int64(v.MinSamples) && WilsonLower(acceptable, judged) >= v.MinPassRate
}

// defaultVerification is the rule a Query without one uses.
var defaultVerification = Verification{
	MinSamples:  config.DefaultVerifiedMinSamples,
	MinPassRate: config.DefaultVerifiedMinPassRate,
}

// CategoryQuality is the shadow verification of one category in a set of
// requests.
type CategoryQuality struct {
	// Category is the quality category.
	Category string
	// Judged counts the shadow samples the judge decided, Acceptable those
	// it found acceptable, and Errors the samples that produced no verdict.
	Judged, Acceptable, Errors int64
	// PassRate is Acceptable / Judged, and WilsonLower the lower bound of
	// its 90% Wilson score interval; both are 0 without judged samples.
	PassRate, WilsonLower float64
	// Verified reports whether the category meets the report's
	// Verification, so that its savings count in VerifiedSavingsUSD.
	Verified bool
}

// FeedbackCounts counts feedback by score.
type FeedbackCounts struct {
	Negative, Neutral, Positive int64
}

// FromNanoUSD returns n nano-USD (1e-9 USD) as Money.
func FromNanoUSD(n int64) Money {
	return fromFemto(new(big.Int).Mul(big.NewInt(n), big.NewInt(1_000_000)))
}
