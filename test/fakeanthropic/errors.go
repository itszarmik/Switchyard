package fakeanthropic

import (
	"encoding/json"
	"net/http"
)

// Anthropic error types (https://platform.claude.com/docs/en/api/errors).
const (
	typeInvalidRequest  = "invalid_request_error"
	typeAuthentication  = "authentication_error"
	typeBilling         = "billing_error"
	typePermission      = "permission_error"
	typeNotFound        = "not_found_error"
	typeRequestTooLarge = "request_too_large"
	typeRateLimit       = "rate_limit_error"
	typeAPI             = "api_error"
	typeTimeout         = "timeout_error"
	typeOverloaded      = "overloaded_error"
)

// StatusOverloaded is Anthropic's 529, sent when the API is overloaded.
const StatusOverloaded = 529

// ErrorType is the Anthropic error type of an HTTP status, as documented:
// 400 invalid_request_error, 401 authentication_error, 402 billing_error,
// 403 permission_error, 404 not_found_error, 413 request_too_large, 429
// rate_limit_error, 500 api_error, 504 timeout_error, 529
// overloaded_error; other 4xx are invalid_request_error and other 5xx
// api_error.
func ErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return typeInvalidRequest
	case http.StatusUnauthorized:
		return typeAuthentication
	case http.StatusPaymentRequired:
		return typeBilling
	case http.StatusForbidden:
		return typePermission
	case http.StatusNotFound:
		return typeNotFound
	case http.StatusRequestEntityTooLarge:
		return typeRequestTooLarge
	case http.StatusTooManyRequests:
		return typeRateLimit
	case http.StatusGatewayTimeout:
		return typeTimeout
	case StatusOverloaded:
		return typeOverloaded
	}
	if status >= 500 {
		return typeAPI
	}
	return typeInvalidRequest
}

// errorBody encodes {"type":"error","error":{"type","message"},"request_id"}.
func errorBody(typ, message, requestID string) []byte {
	type detail struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	b, err := json.Marshal(struct {
		Type      string `json:"type"`
		Error     detail `json:"error"`
		RequestID string `json:"request_id,omitempty"`
	}{"error", detail{typ, message}, requestID})
	if err != nil {
		// Marshaling strings cannot fail.
		panic("fakeanthropic: encoding an error: " + err.Error())
	}
	return b
}

// StatusBody is the error body the fake sends for status when a Behavior
// sets Status without a Body: the documented error type for the status
// (see [ErrorType]) with a message like the API's.
func StatusBody(status int) []byte {
	msg := http.StatusText(status)
	switch status {
	case http.StatusBadRequest:
		msg = "Invalid request."
	case http.StatusUnauthorized:
		msg = "invalid x-api-key"
	case http.StatusTooManyRequests:
		msg = "This request would exceed the rate limit for your organization."
	case http.StatusInternalServerError:
		msg = "Internal server error"
	case StatusOverloaded:
		msg = "Overloaded"
	}
	if msg == "" {
		msg = "Error"
	}
	return errorBody(ErrorType(status), msg, "")
}

// APIError returns a Behavior that fails every request with status and an
// Anthropic error of type typ.
func APIError(status int, typ, message string) Behavior {
	return Behavior{Status: status, Body: errorBody(typ, message, "")}
}

// RateLimited returns a Behavior that fails every request with a 429
// rate_limit_error and the given retry-after header value.
func RateLimited(retryAfter string) Behavior {
	b := Behavior{Status: http.StatusTooManyRequests, Header: http.Header{}}
	if retryAfter != "" {
		b.Header.Set("Retry-After", retryAfter)
	}
	return b
}

// Overloaded returns a Behavior that fails every request with a 529
// overloaded_error.
func Overloaded() Behavior {
	return Behavior{Status: StatusOverloaded}
}

// errorEvent is the SSE error event of type typ.
func errorEvent(typ string) event {
	msg := "Overloaded"
	if typ != typeOverloaded {
		msg = "An error occurred."
	}
	b, err := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
	if err != nil {
		panic("fakeanthropic: encoding an error event: " + err.Error())
	}
	return event{name: "error", data: b}
}
