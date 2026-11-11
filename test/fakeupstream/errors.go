package fakeupstream

import (
	"encoding/json"
	"net/http"
)

// Error types used by OpenAI's error envelope.
const (
	typeInvalidRequest = "invalid_request_error"
	typeServer         = "server_error"
)

// apiError is one OpenAI error envelope. Empty param and code are sent as
// JSON null, as OpenAI does.
type apiError struct {
	status  int // only used by parseChatRequest
	message string
	typ     string
	param   string
	code    string
}

// envelope encodes e as {"error":{"message","type","param","code"}}.
func (e apiError) envelope() []byte {
	type body struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    *string `json:"code"`
	}
	nullable := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	b, err := json.Marshal(struct {
		Error body `json:"error"`
	}{body{Message: e.message, Type: e.typ, Param: nullable(e.param), Code: nullable(e.code)}})
	if err != nil {
		// Marshaling strings and string pointers cannot fail.
		panic("fakeupstream: encoding an error envelope: " + err.Error())
	}
	return b
}

// APIError returns a Behavior that fails every chat completion with status
// and an OpenAI error envelope. An empty param or code is sent as null.
func APIError(status int, typ, code, param, message string) Behavior {
	return Behavior{
		Status: status,
		Body:   apiError{message: message, typ: typ, param: param, code: code}.envelope(),
	}
}

// RateLimited returns a Behavior that fails every chat completion with
// OpenAI's 429 rate-limit error and the given Retry-After header value.
func RateLimited(retryAfter string) Behavior {
	b := Behavior{Status: http.StatusTooManyRequests, Header: http.Header{}}
	if retryAfter != "" {
		b.Header.Set("Retry-After", retryAfter)
	}
	return b
}

// StatusBody is the OpenAI error envelope the fake sends for status when a
// Behavior sets Status without a Body:
//
//   - 400: invalid_request_error, no code
//   - 401: invalid_request_error, code invalid_api_key; the message contains
//     a masked key, as OpenAI's does
//   - 429: type requests, code rate_limit_exceeded
//   - 500: server_error
//   - 503: server_error, "overloaded"
//   - any other 4xx or 5xx: invalid_request_error or server_error with the
//     status text
func StatusBody(status int) []byte {
	var e apiError
	switch status {
	case http.StatusBadRequest:
		e = apiError{message: "Invalid request.", typ: typeInvalidRequest}
	case http.StatusUnauthorized:
		e = apiError{
			message: "Incorrect API key provided: sk-f************************7890. You can find your API key at https://platform.openai.com/account/api-keys.",
			typ:     typeInvalidRequest,
			code:    "invalid_api_key",
		}
	case http.StatusTooManyRequests:
		e = apiError{
			message: "Rate limit reached for requests per min (RPM): Limit 3, Used 3, Requested 1. Please try again in 20s.",
			typ:     "requests",
			code:    "rate_limit_exceeded",
		}
	case http.StatusInternalServerError:
		e = apiError{message: "The server had an error while processing your request. Sorry about that!", typ: typeServer}
	case http.StatusServiceUnavailable:
		e = apiError{message: "The engine is currently overloaded, please try again later.", typ: typeServer}
	default:
		e = apiError{message: http.StatusText(status), typ: typeServer}
		if status < 500 {
			e.typ = typeInvalidRequest
		}
	}
	return e.envelope()
}
