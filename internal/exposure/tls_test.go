package exposure

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureLocalCertificateCreatesReusableLeafCertificate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first, err := EnsureLocalCertificate(Domain, dir)
	if err != nil {
		t.Fatalf("EnsureLocalCertificate first call returned error: %v", err)
	}
	second, err := EnsureLocalCertificate(Domain, dir)
	if err != nil {
		t.Fatalf("EnsureLocalCertificate second call returned error: %v", err)
	}
	if !first.Cert.Equal(second.Cert) {
		t.Fatal("expected second call to reuse generated certificate")
	}
	if first.Cert.IsCA {
		t.Fatal("expected exposure certificate to be a leaf certificate, not a CA")
	}
	if err := first.Cert.VerifyHostname("buildkite." + Domain); err != nil {
		t.Fatalf("expected certificate to verify buildkite hostname: %v", err)
	}
	if err := first.Cert.VerifyHostname("example.com"); err == nil {
		t.Fatal("expected certificate not to verify arbitrary hostnames")
	}
	roots := x509.NewCertPool()
	roots.AddCert(first.Cert)
	if _, err := first.Cert.Verify(x509.VerifyOptions{
		DNSName: "buildkite." + Domain,
		Roots:   roots,
	}); err != nil {
		t.Fatalf("expected certificate to verify as a directly trusted leaf: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, LocalCertificateKeyFilename)); err != nil {
		t.Fatalf("stat certificate key: %v", err)
	} else if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("unexpected certificate key mode: got %v want %v", got, want)
	}
}

func TestGenerateServerCertificateUsesTrustedLeafCertificate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	local, err := EnsureLocalCertificate(Domain, dir)
	if err != nil {
		t.Fatalf("EnsureLocalCertificate returned error: %v", err)
	}
	cert, err := GenerateServerCertificate(Domain, dir)
	if err != nil {
		t.Fatalf("GenerateServerCertificate returned error: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf certificate: %v", err)
	}
	if !leaf.Equal(local.Cert) {
		t.Fatal("expected server certificate to use the locally trusted leaf certificate")
	}
	if leaf.IsCA {
		t.Fatal("expected server certificate not to be a CA")
	}
	if block, _ := pem.Decode(local.CertPEM); block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("expected local certificate PEM to contain a certificate")
	}
	if err := leaf.VerifyHostname("buildkite." + Domain); err != nil {
		t.Fatalf("expected leaf to verify buildkite hostname: %v", err)
	}
}
