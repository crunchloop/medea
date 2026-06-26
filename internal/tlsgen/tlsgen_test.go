package tlsgen

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureServerCert(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")

	if err := EnsureServerCert(cert, key, []string{"10.0.0.5", "medea.local"}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Loadable as a TLS keypair.
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatalf("load keypair: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("10.0.0.5"); err != nil {
		t.Fatalf("SAN missing IP: %v", err)
	}
	if err := leaf.VerifyHostname("localhost"); err != nil {
		t.Fatalf("SAN missing localhost: %v", err)
	}

	// Key file is private (0600).
	info, err := os.Stat(key)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key perm = %o, want 600", perm)
	}

	// Idempotent: second call does not regenerate (cert bytes unchanged).
	before, _ := os.ReadFile(cert)
	if err := EnsureServerCert(cert, key, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(cert)
	if string(before) != string(after) {
		t.Fatal("cert regenerated on second call")
	}
}
