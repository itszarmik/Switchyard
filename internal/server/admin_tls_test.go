package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selfSigned writes a self-signed certificate for 127.0.0.1 and its key to
// a temporary directory, and returns their paths and the certificate.
func selfSigned(t *testing.T) (certFile, keyFile string, cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "switchyard-test"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, cert
}

// TestTLS: with a certificate the public listener serves HTTPS, refuses
// TLS versions below its minimum, and the admin listener stays plain HTTP.
func TestTLS(t *testing.T) {
	certFile, keyFile, cert := selfSigned(t)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	tests := []struct {
		name      string
		min       uint16
		clientMax uint16
		wantErr   bool
		wantVer   uint16
	}{
		{name: "default min, TLS 1.3 client", clientMax: tls.VersionTLS13, wantVer: tls.VersionTLS13},
		{name: "default min, TLS 1.2 client", clientMax: tls.VersionTLS12, wantVer: tls.VersionTLS12},
		{name: "min 1.3, TLS 1.2 client", min: tls.VersionTLS13, clientMax: tls.VersionTLS12, wantErr: true},
		{name: "min 1.3, TLS 1.3 client", min: tls.VersionTLS13, clientMax: tls.VersionTLS13, wantVer: tls.VersionTLS13},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, logs := newTestServer(t, func(o *Options) {
				o.TLSCertFile, o.TLSKeyFile, o.TLSMinVersion = certFile, keyFile, tt.min
			})
			startServer(t, s)
			tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MaxVersion: tt.clientMax}}
			t.Cleanup(tr.CloseIdleConnections)
			c := &http.Client{Transport: tr, Timeout: testTimeout}
			resp, err := c.Get("https://" + s.PublicAddr() + "/healthz")
			if tt.wantErr {
				if err == nil {
					resp.Body.Close()
					t.Fatal("the handshake below the minimum version succeeded")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.TLS == nil || resp.TLS.Version != tt.wantVer {
				t.Errorf("status %d, TLS %+v; want 200 over version %#x", resp.StatusCode, resp.TLS, tt.wantVer)
			}
			if r := do(t, newClient(t), http.MethodGet, "http://"+s.AdminAddr()+"/metrics", nil); r.status != http.StatusOK {
				t.Errorf("admin /metrics over plain HTTP: %d", r.status)
			}
			if l := logs.find(t, "listening"); len(l) != 1 || l[0]["public_tls"] != true {
				t.Errorf("listening line %v: want public_tls true", l)
			}
		})
	}
}

func TestTLSOptions(t *testing.T) {
	certFile, keyFile, _ := selfSigned(t)
	missing := filepath.Join(t.TempDir(), "missing.pem")
	tests := []struct {
		name       string
		cert, key  string
		min        uint16
		wantSubstr string
	}{
		{name: "cert without key", cert: certFile, wantSubstr: "must be set together"},
		{name: "key without cert", key: keyFile, wantSubstr: "must be set together"},
		{name: "unreadable cert", cert: missing, key: keyFile, wantSubstr: "load the TLS certificate"},
		{name: "key and cert swapped", cert: keyFile, key: certFile, wantSubstr: "load the TLS certificate"},
		{name: "TLS 1.1", cert: certFile, key: keyFile, min: tls.VersionTLS11, wantSubstr: "TLSMinVersion"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := testOptions(&logBuffer{})
			opts.TLSCertFile, opts.TLSKeyFile, opts.TLSMinVersion = tt.cert, tt.key, tt.min
			_, err := New(opts)
			if err == nil || !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("New: %v; want an error containing %q", err, tt.wantSubstr)
			}
		})
	}
}
