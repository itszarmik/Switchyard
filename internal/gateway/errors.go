package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"switchyard/internal/apierror"
	"switchyard/internal/catalog"
	"switchyard/internal/providers"
	"switchyard/internal/schema"
)

// Error codes the gateway itself sets.
const (
	codeInvalidAPIKey   = "invalid_api_key"
	codeModelNotFound   = "model_not_found"
	codeModelNotSupport = "model_not_supported"
	codeNoEligible      = "no_eligible_model"
	codeNoHealthy       = "no_healthy_upstream"
	codeInvalidTimeout  = "invalid_timeout"
	codeUpstreamError   = "upstream_error"
	codeTimeout         = "timeout"
	codeRateLimited     = "rate_limit_exceeded"
	codeCanceled        = "canceled"
	codeClientGone      = "client_disconnected"
	typeRateLimitErrors = "requests" // OpenAI's type for request-rate 429s
)

// StatusClientClosedRequest is the status recorded, and sent if anyone is
// still listening, when the client cancels a request before the response
// started. The code is nginx's convention.
const StatusClientClosedRequest = 499

func invalidRequest(err error) *Error {
	var re *schema.RequestError
	if !errors.As(err, &re) {
		re = &schema.RequestError{Code: schema.CodeInvalidJSON, Message: err.Error()}
	}
	return &Error{
		Status: http.StatusBadRequest, Type: apierror.TypeInvalidRequest,
		Code: re.Code, Param: re.Param, Message: re.Message,
	}
}

// unknownKey is the auth middleware's answer for a key it does not know
// (see knownCaller).
func unknownKey() *Error {
	return &Error{
		Status: http.StatusUnauthorized, Type: apierror.TypeInvalidRequest, Code: codeInvalidAPIKey,
		Message: "Incorrect API key provided.",
	}
}

func modelNotFound(model string) *Error {
	return &Error{
		Status: http.StatusNotFound, Type: apierror.TypeInvalidRequest, Code: codeModelNotFound,
		Message: fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", model),
	}
}

// virtualModel is the answer for a virtual model name ("auto…", or an
// alias of one) sent to an endpoint that does not route across models,
// such as embeddings, whose vectors are not interchangeable between
// models.
func virtualModel(model, endpoint string) *Error {
	return &Error{
		Status: http.StatusBadRequest, Type: apierror.TypeInvalidRequest, Code: codeModelNotSupport, Param: "model",
		Message: fmt.Sprintf("The model `%s` is a virtual model, which %s does not serve; name a concrete model.", model, endpoint),
	}
}

// wrongModelType is the answer for a model whose type the endpoint does
// not serve, such as an embedding model sent to chat completions.
func wrongModelType(model, typ, endpoint string) *Error {
	return &Error{
		Status: http.StatusBadRequest, Type: apierror.TypeInvalidRequest, Code: codeModelNotSupport, Param: "model",
		Message: fmt.Sprintf("The model `%s` is a %s model and cannot be used with %s.", model, typ, endpoint),
	}
}

// noEligibleModel is the answer when every deployment of model was
// excluded. The message names each one and why.
func noEligibleModel(model string, exclusions []catalog.Exclusion) *Error {
	parts := make([]string, len(exclusions))
	for i, x := range exclusions {
		parts[i] = x.Deployment + " (" + x.Reason + ")"
	}
	return &Error{
		Status: http.StatusBadRequest, Type: apierror.TypeInvalidRequest, Code: codeNoEligible,
		Message: fmt.Sprintf("No deployment of the model `%s` can serve this request: %s.", model, strings.Join(parts, "; ")),
	}
}

// noHealthyUpstream is the answer when every deployment that could serve
// the request was skipped for its health: an open breaker, a rate-limit
// cooldown or a saturated provider. The message names each one and why.
func noHealthyUpstream(model string, exclusions []catalog.Exclusion) *Error {
	parts := make([]string, len(exclusions))
	for i, x := range exclusions {
		parts[i] = x.Deployment + " (" + x.Reason + ")"
	}
	return &Error{
		Status: http.StatusServiceUnavailable, Type: apierror.TypeServer, Code: codeNoHealthy,
		Message: fmt.Sprintf("No deployment of the model `%s` is available right now: %s.", model, strings.Join(parts, "; ")),
	}
}

// deadlineExceeded is the answer when the request's deadline expired
// before any upstream answered.
func deadlineExceeded(d time.Duration) *Error {
	return &Error{
		Status: http.StatusGatewayTimeout, Type: apierror.TypeServer, Code: codeTimeout,
		Message: fmt.Sprintf("The request did not complete within its deadline of %s.", d),
	}
}

// invalidTimeout is the answer for an unusable X-Switchyard-Timeout-Ms.
func invalidTimeout() *Error {
	return &Error{
		Status: http.StatusBadRequest, Type: apierror.TypeInvalidRequest, Code: codeInvalidTimeout,
		Param:   HeaderTimeoutMs,
		Message: "The " + HeaderTimeoutMs + " header must be a positive whole number of milliseconds.",
	}
}

// badUpstreamResponse is the answer when a successful upstream response
// cannot be translated for the client.
func badUpstreamResponse() *Error {
	return &Error{
		Status: http.StatusBadGateway, Type: apierror.TypeServer, Code: codeUpstreamError,
		Message: "The upstream provider returned a response the gateway could not translate.",
	}
}

// upstreamError maps an adapter failure onto the response the client gets.
//
//   - the kinds that pass through (rate_limited, not_found,
//     context_length, content_filter, client_error): the upstream status,
//     with the upstream body if it is an error envelope and a generic
//     envelope otherwise; 429 keeps Retry-After.
//   - auth (401/402/403): 502 with a generic message. The upstream body is
//     never echoed; it can contain fragments of our credential.
//   - 5xx and malformed streams: 502 naming the upstream status.
//   - connection errors: 502.
//   - timeouts: 504.
//   - client cancellation: 499.
func upstreamError(err error) *Error {
	var pe *providers.Error
	if !errors.As(err, &pe) {
		pe = &providers.Error{Kind: providers.KindTransport, Message: "connection to the upstream failed"}
	}
	switch {
	case pe.Kind.PassesThrough():
		e := &Error{Status: pe.Status, RetryAfter: pe.RetryAfter, Body: pe.Body}
		if e.Status == 0 {
			e.Status = http.StatusBadGateway
		}
		if e.Body == nil {
			e.Type = apierror.TypeInvalidRequest
			e.Code = codeUpstreamError
			e.Message = fmt.Sprintf("The upstream provider rejected the request with HTTP %d.", pe.Status)
			if pe.Kind == providers.KindRateLimited {
				e.Type = typeRateLimitErrors
				e.Code = codeRateLimited
				e.Message = "The upstream provider's rate limit was reached. Please retry later."
			}
		}
		return e
	case pe.Kind == providers.KindAuth:
		return &Error{
			Status: http.StatusBadGateway, Type: apierror.TypeServer, Code: codeUpstreamError,
			Message: "The upstream provider could not authenticate the gateway.",
		}
	case pe.Kind == providers.KindServerError:
		msg := fmt.Sprintf("The upstream provider failed: %s.", pe.Message)
		if pe.Status != 0 {
			msg = fmt.Sprintf("The upstream provider returned HTTP %d.", pe.Status)
		}
		return &Error{Status: http.StatusBadGateway, Type: apierror.TypeServer, Code: codeUpstreamError, Message: msg}
	case pe.Kind == providers.KindTimeout:
		return &Error{
			Status: http.StatusGatewayTimeout, Type: apierror.TypeServer, Code: codeTimeout,
			Message: fmt.Sprintf("The upstream provider did not respond in time: %s.", pe.Message),
		}
	case pe.Kind == providers.KindCanceled:
		return &Error{
			Status: StatusClientClosedRequest, Type: apierror.TypeInvalidRequest, Code: codeCanceled,
			Message: "The request was canceled by the client.",
		}
	default: // transport
		return &Error{
			Status: http.StatusBadGateway, Type: apierror.TypeServer, Code: codeUpstreamError,
			Message: "The upstream provider could not be reached.",
		}
	}
}

// streamErrorEvent is the data payload sent when a committed stream fails.
// The OpenAI SDKs raise it as an API error.
func streamErrorEvent(err error) []byte {
	msg := "The upstream provider failed mid-stream."
	var pe *providers.Error
	if errors.As(err, &pe) {
		msg = fmt.Sprintf("The upstream provider failed mid-stream: %s.", pe.Message)
	}
	var ev struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	ev.Error.Message = msg
	ev.Error.Type = apierror.TypeServer
	ev.Error.Code = codeUpstreamError
	data, _ := json.Marshal(ev) // only strings; cannot fail
	return data
}
