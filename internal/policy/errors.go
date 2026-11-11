package policy

import "strings"

// InvalidHeaderError reports a malformed policy request header. The gateway
// answers it with 400 invalid_header. It names the header but never
// includes its value.
type InvalidHeaderError struct {
	// Header is the canonical header name, such as HeaderMaxCostUSD.
	Header string
	// Reason says what a valid value looks like.
	Reason string
}

// Error returns "invalid <header> header: <reason>".
func (e *InvalidHeaderError) Error() string {
	return "invalid " + e.Header + " header: " + e.Reason
}

// ModelNotSupportedError reports an "auto" or "auto:<profile>" request the
// policy does not allow: an unknown or disallowed profile, or a policy
// without a baseline_model. The gateway answers it with 400
// model_not_supported.
type ModelNotSupportedError struct {
	// Message is safe to return to the client.
	Message string
	// Allowed lists the profiles the caller may select, sorted; it is empty
	// when auto routing is disabled.
	Allowed []string
}

// Error returns Message.
func (e *ModelNotSupportedError) Error() string { return e.Message }

// msgNoBaseline is the message for an auto request under a policy without a
// baseline_model.
const msgNoBaseline = "this policy has no baseline_model; auto routing is disabled"

// CheckAuto returns a *ModelNotSupportedError if eff does not allow auto
// routing because its policy has no baseline_model (DESIGN §12.4). The
// gateway calls it for every "auto" or "auto:<profile>" request, including
// one reached through an alias.
func CheckAuto(eff Effective) error {
	if eff.BaselineModel == "" {
		return &ModelNotSupportedError{Message: msgNoBaseline}
	}
	return nil
}

// profileError returns the error for a requested profile that does not
// exist or that the policy does not allow. The name is quoted only when it
// is a well-formed profile name.
func profileError(name string, exists bool, allowed []string) *ModelNotSupportedError {
	shown := "profile"
	if validProfileName(name) {
		shown = `profile "` + name + `"`
	}
	msg := "unknown " + shown
	if exists {
		msg = shown + " is not allowed by this policy"
	}
	return &ModelNotSupportedError{
		Message: msg + "; allowed profiles: " + strings.Join(allowed, ", "),
		Allowed: allowed,
	}
}
