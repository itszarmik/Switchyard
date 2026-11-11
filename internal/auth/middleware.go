package auth

import (
	"log/slog"
	"net/http"
	"strings"

	"switchyard/internal/apierror"
	"switchyard/internal/reqctx"
	"switchyard/internal/server"
)

const (
	// errCode and errMessage are OpenAI's for a bad key. The message never
	// includes the presented key.
	errCode    = "invalid_api_key"
	errMessage = "Incorrect API key provided."
)

// Reasons logged at DEBUG when authentication fails.
const (
	reasonMissing   = "missing_authorization"
	reasonMalformed = "malformed_authorization"
	reasonUnknown   = "unknown_key"
)

// Middleware returns middleware that authenticates every request against s.
//
// It reads "Authorization: Bearer <key>", with the scheme matched
// case-insensitively. A missing, repeated or malformed header, or a key s
// does not know, gets 401 with WWW-Authenticate: Bearer and the error
// {"type":"invalid_request_error","code":"invalid_api_key"}; the failure is
// logged at DEBUG with a reason, never with the key.
//
// On success the request context carries the Principal (see FromContext),
// its request logger gains tenant and key_id attributes, and so does the
// request's access-log line (see reqctx.Annotate).
func Middleware(s *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, reason := bearerToken(r.Header)
			if reason != "" {
				reject(w, r, reason)
				return
			}
			p, ok := s.Authenticate(key)
			if !ok {
				reject(w, r, reasonUnknown)
				return
			}
			ctx := newContext(r.Context(), p)
			ctx = reqctx.WithLogger(ctx, reqctx.Logger(ctx).With("tenant", p.TenantID, "key_id", p.KeyID))
			// The server's access-log line is logged outside this
			// middleware, with the logger from before it.
			reqctx.Annotate(ctx, slog.String("tenant", p.TenantID), slog.String("key_id", p.KeyID))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerToken extracts the token from the request's single Authorization
// header, which must be "Bearer" (in any case), one or more spaces, and a
// token without whitespace. It returns a failure reason instead of a token
// when the header is missing or malformed.
func bearerToken(h http.Header) (token, reason string) {
	values := h.Values("Authorization")
	switch {
	case len(values) == 0:
		return "", reasonMissing
	case len(values) > 1:
		return "", reasonMalformed
	}
	scheme, rest, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", reasonMalformed
	}
	token = strings.TrimLeft(rest, " ")
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", reasonMalformed
	}
	return token, ""
}

// reject logs the failure at DEBUG and writes the 401 response. The body
// is never read, so the connection is closed after the response rather
// than drained: an unauthenticated client cannot hold it by stalling.
func reject(w http.ResponseWriter, r *http.Request, reason string) {
	ctx := r.Context()
	reqctx.Logger(ctx).DebugContext(ctx, "authentication failed", "reason", reason)
	server.AbandonBody(w, r)
	w.Header().Set("WWW-Authenticate", "Bearer")
	apierror.Write(w, http.StatusUnauthorized, apierror.TypeInvalidRequest, errCode, errMessage)
}
