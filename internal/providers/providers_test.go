package providers

import (
	"errors"
	"fmt"
	"testing"

	"switchyard/internal/schema"
)

// TestKindPolicy is the error taxonomy of DESIGN §14.1.
func TestKindPolicy(t *testing.T) {
	tests := []struct {
		kind                                    Kind
		retry, fallBack, breaker, passesThrough bool
	}{
		{KindTransport, true, true, true, false},
		{KindTimeout, true, true, true, false},
		{KindServerError, true, true, true, false},
		{KindRateLimited, true, true, false, true},
		{KindAuth, false, true, true, false},
		{KindNotFound, false, true, false, true},
		{KindContextLength, false, true, false, true},
		{KindContentFilter, false, false, false, true},
		{KindClientError, false, false, false, true},
		{KindUnsupported, false, true, false, true},
		{KindCanceled, false, false, false, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			if got := tt.kind.Retryable(); got != tt.retry {
				t.Errorf("Retryable = %v, want %v", got, tt.retry)
			}
			if got := tt.kind.FallsBack(); got != tt.fallBack {
				t.Errorf("FallsBack = %v, want %v", got, tt.fallBack)
			}
			if got := tt.kind.CountsTowardBreaker(); got != tt.breaker {
				t.Errorf("CountsTowardBreaker = %v, want %v", got, tt.breaker)
			}
			if got := tt.kind.PassesThrough(); got != tt.passesThrough {
				t.Errorf("PassesThrough = %v, want %v", got, tt.passesThrough)
			}
		})
	}
}

func TestOutcome(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, OutcomeOK},
		{"provider error", &Error{Kind: KindNotFound}, "not_found"},
		{"wrapped", errors.Join(errors.New("x"), &Error{Kind: KindContextLength}), "context_length"},
		{"other", errors.New("boom"), "transport"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Outcome(tt.err); got != tt.want {
				t.Errorf("Outcome = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRetryable(t *testing.T) {
	total := &Error{Kind: KindTimeout, Timeout: TimeoutTotal}
	tests := []struct {
		name   string
		err    error
		stream bool
		want   bool
	}{
		{"non-stream total timeout", total, false, false},
		{"stream total timeout", total, true, true},
		{"non-stream deadline", &Error{Kind: KindTimeout}, false, true},
		{"first_token", &Error{Kind: KindTimeout, Timeout: TimeoutFirstToken}, true, true},
		{"idle", &Error{Kind: KindTimeout, Timeout: TimeoutIdle}, true, true},
		{"wrapped total", errors.Join(errors.New("x"), total), false, false},
		{"server error", &Error{Kind: KindServerError}, false, true},
		{"client error", &Error{Kind: KindClientError}, false, false},
		{"unsupported", &Error{Kind: KindUnsupported}, false, false},
		{"other", errors.New("boom"), false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Retryable(tt.err, tt.stream); got != tt.want {
				t.Errorf("Retryable = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestMayHaveBilled checks the package helpers: an *Error that says the
// upstream billed it was billed whatever its kind, and carries its usage;
// otherwise the kind decides, and a non-*Error counts as transport.
func TestMayHaveBilled(t *testing.T) {
	u := &schema.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}
	tests := []struct {
		name   string
		err    error
		billed bool
		usage  *schema.Usage
	}{
		{"billed answer with usage", &Error{Kind: KindUnsupported, Billed: true, BilledUsage: u}, true, u},
		{"wrapped", fmt.Errorf("call: %w", &Error{Kind: KindClientError, Billed: true, BilledUsage: u}), true, u},
		{"billed answer without usage", &Error{Kind: KindUnsupported, Billed: true}, true, nil},
		{"usage without Billed is ignored", &Error{Kind: KindUnsupported, BilledUsage: u}, false, nil},
		{"unsupported", &Error{Kind: KindUnsupported}, false, nil},
		{"client error", &Error{Kind: KindClientError}, false, nil},
		{"server error", &Error{Kind: KindServerError}, true, nil},
		{"not an *Error", errors.New("boom"), true, nil},
	}
	for _, tt := range tests {
		if got := MayHaveBilled(tt.err); got != tt.billed {
			t.Errorf("%s: MayHaveBilled = %v, want %v", tt.name, got, tt.billed)
		}
		if got := BilledUsage(tt.err); got != tt.usage {
			t.Errorf("%s: BilledUsage = %v, want %v", tt.name, got, tt.usage)
		}
	}
}
