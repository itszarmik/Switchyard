package fakegemini

import (
	"fmt"
	"net/http"
	"time"
)

// Status is the google.rpc.Code name Google sends with an HTTP status, as
// its error page documents: 400 INVALID_ARGUMENT, 401 UNAUTHENTICATED, 402
// and 429 RESOURCE_EXHAUSTED, 403 PERMISSION_DENIED, 404 NOT_FOUND, 408 and
// 504 DEADLINE_EXCEEDED, 500 INTERNAL, 503 UNAVAILABLE; other 4xx are
// FAILED_PRECONDITION and other 5xx INTERNAL.
func Status(code int) string {
	switch code {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusPaymentRequired, http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return "DEADLINE_EXCEEDED"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	}
	if code >= 500 {
		return "INTERNAL"
	}
	return "FAILED_PRECONDITION"
}

// ErrorBody encodes Google's error body for HTTP status code:
// {"error":{"code","message","status","details"}}. details are
// google.rpc detail objects, such as RetryInfo or ErrorInfo.
func ErrorBody(code int, message string, details ...any) []byte {
	type detail struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
		Details []any  `json:"details,omitempty"`
	}
	return mustMarshal(struct {
		Error detail `json:"error"`
	}{detail{Code: code, Message: message, Status: Status(code), Details: details}})
}

// errorInfo is a google.rpc.ErrorInfo detail with reason.
func errorInfo(reason string) any {
	return map[string]any{
		"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": reason, "domain": "googleapis.com",
		"metadata": map[string]string{"service": "generativelanguage.googleapis.com"},
	}
}

// RetryInfo is a google.rpc.RetryInfo detail asking to retry after d.
func RetryInfo(d time.Duration) any {
	return map[string]string{
		"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": fmt.Sprintf("%gs", d.Seconds()),
	}
}

// StatusBody is the error body the fake sends for status when a Behavior
// sets Status without a Body, with a message like the API's.
func StatusBody(code int) []byte {
	msg := http.StatusText(code)
	switch code {
	case http.StatusBadRequest:
		msg = "Request contains an invalid argument."
	case http.StatusTooManyRequests:
		msg = "You exceeded your current quota, please check your plan and billing details."
	case http.StatusInternalServerError:
		msg = "An internal error has occurred. Please retry."
	case http.StatusServiceUnavailable:
		msg = "The model is overloaded. Please try again later."
	}
	if msg == "" {
		msg = "Error"
	}
	return ErrorBody(code, msg)
}

// APIError returns a Behavior that fails every request with code and a
// Google error with message.
func APIError(code int, message string, details ...any) Behavior {
	return Behavior{Status: code, Body: ErrorBody(code, message, details...)}
}

// RateLimited returns a Behavior that fails every request with a 429
// RESOURCE_EXHAUSTED whose RetryInfo asks for retryAfter (none if 0).
func RateLimited(retryAfter time.Duration) Behavior {
	if retryAfter <= 0 {
		return Behavior{Status: http.StatusTooManyRequests}
	}
	return APIError(http.StatusTooManyRequests, "You exceeded your current quota, please check your plan and billing details.", RetryInfo(retryAfter))
}

// StreamError returns a Behavior whose streams send a Google error event
// with code after events events.
func StreamError(code int, events int) Behavior {
	return Behavior{ErrorEvent: StatusBody(code), ErrorAfterEvents: events}
}
