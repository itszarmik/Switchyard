package auth

import (
	"log/slog"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/codes"

	"switchyard/internal/apierror"
	"switchyard/internal/reqctx"
	"switchyard/internal/server"
	"switchyard/internal/tracing"
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

// Options configures Middleware.
type Options struct {
	// Guard, when set, is called with every request once its key has been
	// checked, before anything is written, with whether it authenticated.
	// When it returns false it has answered the request itself (the app's
	// per-address limit on failed authentications answers 429:
	// ratelimit.AuthFailures.Check), and the middleware writes nothing
	// more and logs nothing.
	Guard func(w http.ResponseWriter, r *http.Request, authenticated bool) bool
}

// Middleware returns middleware that authenticates every request against
// a, a *Store or, to follow config reloads, a *Keyring.
//
// It reads "Authorization: Bearer <key>", with the scheme matched
// case-insensitively. A missing, repeated or malformed header, or a key a
// does not know, gets 401 with WWW-Authenticate: Bearer and the error
// {"type":"invalid_request_error","code":"invalid_api_key"}; the failure is
// logged at DEBUG with a reason, never with the key.
//
// On success the request context carries the Principal (see FromContext),
// its request logger gains tenant and key_id attributes, and so does the
// request's access-log line (see reqctx.Annotate). The check is traced as
// a tracing.SpanAuth span, a child of the request's span that ends before
// next runs; it records the tenant and the key's ID, never the key.
func Middleware(a Authenticator, o Options) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, span := tracing.Start(r.Context(), tracing.SpanAuth)
			key, reason := bearerToken(r.Header)
			var (
				p  Principal
				ok bool
			)
			if reason == "" {
				if p, ok = a.Authenticate(key); !ok {
					reason = reasonUnknown
				}
			}
			if o.Guard != nil && !o.Guard(w, r, reason == "") {
				span.SetStatus(codes.Error, "auth_failures_limited")
				span.End()
				return
			}
			if reason != "" {
				span.SetStatus(codes.Error, reason)
				span.End()
				reject(w, r, reason)
				return
			}
			if span.IsRecording() {
				span.SetAttributes(tracing.AttrTenant.String(p.TenantID), tracing.AttrKeyID.String(p.KeyID))
			}
			span.End()
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
