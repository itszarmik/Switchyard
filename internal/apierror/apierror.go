// Package apierror writes errors in the OpenAI-compatible envelope:
//
//	{"error":{"message":"…","type":"…","param":null,"code":"…"}}
//
// OpenAI SDKs parse this shape to surface the status, type and code to
// callers, so every gateway-generated error response goes through Write.
//
// Write cannot know on its own whether a response has already started. A
// ResponseWriter wrapper that tracks this can say so by implementing
//
//	HeaderWritten() bool
//
// Write looks for that method on the writer and on every writer reachable
// through Unwrap() http.ResponseWriter, and does nothing once headers are out.
package apierror

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// Error types, following OpenAI's conventions for the envelope's "type" field.
const (
	// TypeInvalidRequest marks errors caused by the request itself.
	TypeInvalidRequest = "invalid_request_error"
	// TypeServer marks errors on the gateway's side.
	TypeServer = "server_error"
)

// fallbackBody is sent if the envelope cannot be encoded. That cannot happen
// for string fields, but Write must never send an empty error body.
const fallbackBody = `{"error":{"message":"internal error","type":"server_error","param":null,"code":null}}` + "\n"

type envelope struct {
	Error body `json:"error"`
}

type body struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// headerWrittenReporter is implemented by ResponseWriter wrappers that know
// whether the response headers have been sent.
type headerWrittenReporter interface {
	HeaderWritten() bool
}

// Write sends {"error":{"message":…,"type":…,"param":null,"code":…}} with
// the given status. An empty code is encoded as JSON null. It sets
// Content-Type: application/json; charset=utf-8 and
// X-Content-Type-Options: nosniff.
//
// If w reports that headers were already sent (see the package
// documentation), Write does nothing: the status can no longer change, and
// appending an error body would corrupt the response already in progress.
// A status outside 100–999 is replaced by 500 rather than panicking.
func Write(w http.ResponseWriter, status int, errType, code, message string) {
	if headerWritten(w) {
		return
	}
	if status < 100 || status > 999 {
		status = http.StatusInternalServerError
	}

	env := envelope{Error: body{Message: message, Type: errType}}
	if code != "" {
		env.Error.Code = &code
	}
	data, err := json.Marshal(env)
	if err != nil {
		data = []byte(fallbackBody)
	} else {
		data = append(data, '\n')
	}

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	// A write error means the client has gone away; there is nobody left to
	// report it to.
	_, _ = w.Write(data)
}

// headerWritten reports whether w, or any writer it wraps, says the response
// headers have already been sent.
func headerWritten(w http.ResponseWriter) bool {
	for w != nil {
		if r, ok := w.(headerWrittenReporter); ok {
			return r.HeaderWritten()
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
	return false
}
