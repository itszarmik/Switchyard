package config

import (
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"unicode"
)

// msgAdminTokenRequired is the error for an exposed admin listener
// without a token.
const msgAdminTokenRequired = "required when server.admin_listen is not a loopback address (127.0.0.0/8, ::1 or localhost)"

// TLS versions server.tls.min_version accepts.
const (
	TLSVersion12 = "1.2"
	TLSVersion13 = "1.3"
)

// validateAdmin checks the admin hardening and TLS settings (DESIGN §17):
// an admin listener that other hosts can reach needs server.admin_token,
// which, when set, must be a strong single token (adminTokenProblem), and server.tls needs both files or neither, readable and holding a
// matching PEM certificate chain and key, with min_version 1.2 or 1.3.
// The certificate is read here, so that `config validate` and a reload
// catch a bad file before the server needs it; the server reads it again
// at startup.
func (st *loadState) validateAdmin(s *Server) {
	if host, _, err := net.SplitHostPort(s.AdminListen); err == nil && !isLoopbackHost(host) && !s.AdminToken.IsSet() {
		// The empty host and 0.0.0.0 listen on every interface.
		st.errorAt("server.admin_token", msgAdminTokenRequired)
	}
	if s.AdminToken.IsSet() {
		// The value is never quoted: it is a secret.
		if msg := adminTokenProblem(s.AdminToken.Value()); msg != "" {
			st.errorAt("server.admin_token", msg)
		}
	}

	t := &s.TLS
	const base = "server.tls"
	switch v := t.MinVersion; v {
	case "", TLSVersion12, TLSVersion13:
	default:
		st.errorAt(base+".min_version", "must be 1.2 or 1.3, got "+st.describe(base+".min_version", v))
	}
	switch {
	case t.CertFile == "" && t.KeyFile == "":
		return
	case t.CertFile == "":
		st.errorAt(base+".cert_file", "required when server.tls.key_file is set")
		return
	case t.KeyFile == "":
		st.errorAt(base+".key_file", "required when server.tls.cert_file is set")
		return
	}
	certPEM, certErr := st.opts.ReadFile(t.CertFile)
	if certErr != nil {
		st.errorAt(base+".cert_file", "cannot read "+st.describe(base+".cert_file", t.CertFile))
	}
	keyPEM, keyErr := st.opts.ReadFile(t.KeyFile)
	if keyErr != nil {
		st.errorAt(base+".key_file", "cannot read "+st.describe(base+".key_file", t.KeyFile))
	}
	if certErr != nil || keyErr != nil {
		return
	}
	// The parse error is not quoted: it can describe the key's contents.
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		st.errorAt(base, "cert_file and key_file must hold a PEM certificate chain and its matching PEM private key")
	}
}

// MinAdminTokenLen is the shortest server.admin_token accepted, in bytes.
// `openssl rand -base64 32` gives 44.
const MinAdminTokenLen = 32

// adminTokenProblem returns why token cannot be server.admin_token, or "".
// It must be at least MinAdminTokenLen bytes, and a single bearer token:
// no whitespace or control characters, which no Authorization header
// could carry, so a reload to such a token would lock the admin API out.
func adminTokenProblem(token string) string {
	if strings.IndexFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "must not contain whitespace or control characters"
	}
	if len(token) < MinAdminTokenLen {
		return fmt.Sprintf("must be at least %d characters (for example, from `openssl rand -base64 32`)", MinAdminTokenLen)
	}
	return ""
}

// TLSMinVersion returns the crypto/tls version of server.tls.min_version:
// tls.VersionTLS13 for 1.3, and tls.VersionTLS12 otherwise (the default).
func (t ServerTLS) TLSMinVersion() uint16 {
	if t.MinVersion == TLSVersion13 {
		return tls.VersionTLS13
	}
	return tls.VersionTLS12
}
