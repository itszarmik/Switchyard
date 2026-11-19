package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// testKeyPair returns a self-signed PEM certificate and its PEM key,
// generated for the test.
func testKeyPair(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func TestValidateAdmin(t *testing.T) {
	const v1 = "version: 1\n"
	certPEM, keyPEM := testKeyPair(t)
	otherCert, _ := testKeyPair(t)
	files := fakeFS{"cert.pem": certPEM, "key.pem": keyPEM, "other.pem": otherCert, "junk.pem": "not pem"}
	const token = "adm-7c1e5a9b3d-0123456789abcdefghij" // 35 characters
	env := map[string]string{
		"ADMIN_TOKEN": token, "SHORT_TOKEN": token[:31], "MIN_TOKEN": token[:32],
		"SPACE_TOKEN": token[:20] + " " + token[20:], "TAB_TOKEN": "\t" + token, "CTRL_TOKEN": token + "\x7f",
	}
	weak := func(msg string) want { return errAt("server.admin_token", msg) }
	exposed := warnAt("server.admin_listen", msgAdminExposed)
	tokenRequired := errAt("server.admin_token", msgAdminTokenRequired)
	tests := []struct {
		name  string
		yaml  string
		wants []want
	}{
		// admin_token and admin_listen
		{"loopback without a token", v1 + "server: {admin_listen: '127.0.0.1:9090'}\n", nil},
		{"::1 without a token", v1 + "server: {admin_listen: '[::1]:9090'}\n", nil},
		{"localhost without a token", v1 + "server: {admin_listen: 'localhost:9090'}\n", nil},
		{"empty host without a token", v1 + "server: {admin_listen: ':9090'}\n", []want{exposed, tokenRequired}},
		{"0.0.0.0 without a token", v1 + "server: {admin_listen: '0.0.0.0:9090'}\n", []want{exposed, tokenRequired}},
		{"public ip without a token", v1 + "server: {admin_listen: '10.0.0.5:9090'}\n", []want{exposed, tokenRequired}},
		{"0.0.0.0 with a token", v1 + "server: {admin_listen: '0.0.0.0:9090', admin_token: '${ADMIN_TOKEN}'}\n", []want{exposed}},
		{"loopback with a token", v1 + "server: {admin_token: '${ADMIN_TOKEN}'}\n", nil},
		{"32-character token", v1 + "server: {admin_token: '${MIN_TOKEN}'}\n", nil},
		{"31-character token", v1 + "server: {admin_token: '${SHORT_TOKEN}'}\n", []want{weak("at least 32 characters")}},
		{"short token on 0.0.0.0", v1 + "server: {admin_listen: '0.0.0.0:9090', admin_token: '${SHORT_TOKEN}'}\n",
			[]want{exposed, weak("at least 32 characters")}},
		{"token with a space", v1 + "server: {admin_token: '${SPACE_TOKEN}'}\n", []want{weak("whitespace or control characters")}},
		{"token with a tab", v1 + "server: {admin_token: '${TAB_TOKEN}'}\n", []want{weak("whitespace or control characters")}},
		{"token with a control character", v1 + "server: {admin_token: '${CTRL_TOKEN}'}\n", []want{weak("whitespace or control characters")}},

		// tls
		{"tls off", v1 + "server: {tls: {}}\n", nil},
		{"tls on", v1 + "server: {tls: {cert_file: cert.pem, key_file: key.pem}}\n", nil},
		{"tls 1.3", v1 + "server: {tls: {cert_file: cert.pem, key_file: key.pem, min_version: '1.3'}}\n", nil},
		{"tls 1.0", v1 + "server: {tls: {cert_file: cert.pem, key_file: key.pem, min_version: '1.0'}}\n",
			[]want{errAt("server.tls.min_version", "must be 1.2 or 1.3")}},
		{"cert without key", v1 + "server: {tls: {cert_file: cert.pem}}\n", []want{errAt("server.tls.key_file", "required")}},
		{"key without cert", v1 + "server: {tls: {key_file: key.pem}}\n", []want{errAt("server.tls.cert_file", "required")}},
		{"missing files", v1 + "server: {tls: {cert_file: nope.pem, key_file: gone.pem}}\n",
			[]want{errAt("server.tls.cert_file", "cannot read"), errAt("server.tls.key_file", "cannot read")}},
		{"mismatched pair", v1 + "server: {tls: {cert_file: other.pem, key_file: key.pem}}\n", []want{errAt("server.tls", "matching")}},
		{"not pem", v1 + "server: {tls: {cert_file: junk.pem, key_file: key.pem}}\n", []want{errAt("server.tls", "PEM")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := problemsOf(t, tt.yaml, env, files)
			checkProblems(t, got, tt.wants...)
			for _, p := range got {
				if strings.Contains(p.Message, "adm-7c1e5a9b3d") || strings.Contains(p.Message, "PRIVATE KEY") {
					t.Errorf("problem %s leaks a secret", p)
				}
			}
		})
	}
}

func TestTLSMinVersion(t *testing.T) {
	tests := []struct {
		in   string
		want uint16
	}{{"", tls.VersionTLS12}, {"1.2", tls.VersionTLS12}, {"1.3", tls.VersionTLS13}}
	for _, tt := range tests {
		if got := (ServerTLS{MinVersion: tt.in}).TLSMinVersion(); got != tt.want {
			t.Errorf("TLSMinVersion(%q) = %#x, want %#x", tt.in, got, tt.want)
		}
	}
}
