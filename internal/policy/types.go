// Package policy resolves the effective policy for a request: the tenant's or
// key's policy, then the first matching governance rule, then the first
// matching quality rule on top of it, then header tightening (DESIGN §13).
// The result is a plain value the router consumes.
package policy

import (
	"time"

	"switchyard/internal/config"
)

// Routing modes.
const (
	ModeEnforce = "enforce"
	ModeObserve = "observe"
)

// Substitution modes.
const (
	SubstitutionPinned     = "pinned"
	SubstitutionEquivalent = "equivalent"
)

// What happens when no candidate meets the soft constraints.
const (
	OnUnmetBestEffort = "best_effort"
	OnUnmetReject     = "reject"
)

// Profile objectives.
const (
	ObjectiveCost    = "cost"
	ObjectiveLatency = "latency"
)

// Request holds the request facts policies and rules match on. The gateway
// fills it after decoding and assessing the request.
type Request struct {
	// Model is the client's model field as sent.
	Model string
	// Profile is the profile the client selected, from "auto:<p>" or the
	// X-Switchyard-Profile header; "" if none.
	Profile string
	// Task and Difficulty come from the assessor (or client hints).
	Task       string
	Difficulty float64
	// KeyID and KeyTags identify the caller's API key.
	KeyID   string
	KeyTags []string
	// Headers are the request headers (for rule matching and tightening).
	Headers map[string][]string
	// Tags are the validated X-Switchyard-Tags.
	Tags map[string]string
	// Stream, HasTools and HasImages describe the request.
	Stream, HasTools, HasImages bool
	// EstInputTokens is the gateway's input-token estimate.
	EstInputTokens int64
}

// Constraints are the routing objective and soft constraints in force.
type Constraints struct {
	// MinQuality is the quality bar for auto requests (DESIGN §10.4).
	MinQuality float64
	// QualityFloor is the part of MinQuality that rules and the
	// X-Switchyard-Min-Quality header set, as opposed to the profile: the
	// larger of the min_quality of the rule in force (the quality rule's,
	// else the governance rule's) and an accepted header value; 0 when
	// neither sets one. Equivalent substitution, where the profile's bar
	// does not apply, still holds substitutes to it.
	QualityFloor float64
	// Objective is ObjectiveCost or ObjectiveLatency.
	Objective string
	// MaxTTFT, when > 0, is a soft latency target.
	MaxTTFT time.Duration
}

// Effective is the resolved policy for one request. It is immutable once
// returned.
type Effective struct {
	// PolicyID names the base policy ("" for built-in defaults).
	PolicyID string
	// MatchedRule names the governance rule that applied (the first that
	// matched), or "".
	MatchedRule string
	// MatchedQualityRule names the quality rule that applied on top of it
	// (the first that matched), or "".
	MatchedQualityRule string
	// Profile is the profile in force for auto requests, after rules,
	// headers and budget downgrade.
	Profile string
	// Constraints come from Profile, adjusted by rules and headers.
	Constraints Constraints
	// AllowedProfiles is the set callers may select; empty means all.
	AllowedProfiles []string
	// Allow, Deny, Prefer and RequireTags are as in config.Policy, after
	// rule overrides.
	Allow, Deny, Prefer, RequireTags []string
	// Substitution is SubstitutionPinned or SubstitutionEquivalent, with
	// MaxQualityDrop for the latter.
	Substitution   string
	MaxQualityDrop float64
	// RoutingMode is ModeEnforce or ModeObserve.
	RoutingMode string
	// BaselineModel is the savings baseline (and the observe-mode model) for
	// auto requests.
	BaselineModel string
	// MaxCostPerRequestNano is the hard per-request cost cap in nano-USD,
	// exactly: a cap has at most 9 decimal places. 0 means no cap.
	MaxCostPerRequestNano int64
	// OnUnmet is OnUnmetBestEffort or OnUnmetReject.
	OnUnmet string
	// ShadowSampleRate is the base policy's shadow.sample_rate, the
	// fraction of its requests sampled for shadow evaluation (DESIGN
	// §10.7); 0 turns sampling off. Rules do not change it.
	ShadowSampleRate float64
	// Cache is the cache configuration in force, with defaults applied:
	// the base policy's cache block, replaced whole by the governance
	// rule's set.cache when the rule in force sets one. Quality rules and
	// headers do not change it (the X-Switchyard-Cache header only
	// narrows it, in the gateway).
	Cache config.CachePolicy
	// Warnings explain header values that were ignored because they would
	// have loosened the policy. The gateway returns them in the
	// X-Switchyard-Policy-Warning response header.
	Warnings []string
}
