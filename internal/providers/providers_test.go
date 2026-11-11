package providers

import (
	"errors"
	"testing"
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
