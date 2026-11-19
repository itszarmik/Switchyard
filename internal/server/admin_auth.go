package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"switchyard/internal/apierror"
	"switchyard/internal/reqctx"
)

// adminAPIPrefix is the path prefix of the admin API, whose routes the
// admin token guards. /metrics, /healthz and /readyz stay open, for
// scrapers and probes.
const adminAPIPrefix = "/admin/v1/"

// The admin token error, OpenAI-shaped. The message never includes the
// presented token.
const (
	adminTokenCode    = "invalid_admin_token"
	adminTokenMessage = "Missing or invalid admin token; send it as Authorization: Bearer <token>."
)

// isAdminAPI reports whether the ServeMux pattern ("[METHOD ][HOST]/PATH")
// is under /admin/v1/.
func isAdminAPI(pattern string) bool {
	if _, rest, found := strings.Cut(pattern, " "); found {
		pattern = strings.TrimLeft(rest, " \t")
	}
	if i := strings.IndexByte(pattern, '/'); i > 0 {
		pattern = pattern[i:] // drop a host
	}
	return strings.HasPrefix(pattern, adminAPIPrefix)
}

// requireAdminToken wraps h so that, while an admin token is set, a
// request must carry it as "Authorization: Bearer <token>" (the scheme in
// any case). The comparison is of SHA-256 digests in constant time, so
// neither the token's bytes nor its length leak through timing. A missing,
// repeated, malformed or wrong header gets 401 invalid_admin_token, with
// WWW-Authenticate: Bearer; the body is not read. With an admin guard
// (SetAdminGuard), every checked request is reported to it first, and one
// it refuses gets its 429 instead, with no log line.
func (s *Server) requireAdminToken(h http.Handler) http.Handler {
	guard := s.adminGuard
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := s.adminToken.Load()
		if want == nil {
			h.ServeHTTP(w, r)
			return
		}
		token, ok := adminBearer(r.Header)
		got := sha256.Sum256([]byte(token))
		ok = subtle.ConstantTimeCompare(got[:], want[:]) == 1 && ok
		if guard != nil && !guard.Check(w, r, ok) {
			return
		}
		if !ok {
			ctx := r.Context()
			reqctx.Logger(ctx).WarnContext(ctx, "admin authentication failed", "remote_ip", RemoteIP(r), "path", r.URL.Path)
			AbandonBody(w, r)
			w.Header().Set("WWW-Authenticate", "Bearer")
			apierror.Write(w, http.StatusUnauthorized, apierror.TypeInvalidRequest, adminTokenCode, adminTokenMessage)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// adminBearer returns the token of the request's single Authorization
// header, "Bearer" (in any case), spaces and the token. ok is false when
// the header is missing, repeated or malformed.
func adminBearer(h http.Header) (token string, ok bool) {
	values := h.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, rest, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimLeft(rest, " ")
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}
