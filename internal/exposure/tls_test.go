package exposure

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureLocalCertificateCreatesReusableCertificateAuthority(t *testing.T) {
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
	if !first.Cert.IsCA {
		t.Fatal("expected exposure certificate to be a CA")
	}
	roots := x509.NewCertPool()
	roots.AddCert(first.Cert)
	if _, err := first.Cert.Verify(x509.VerifyOptions{
		Roots: roots,
	}); err != nil {
		t.Fatalf("expected certificate authority to verify as a trusted root: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, LocalCertificateKeyFilename)); err != nil {
		t.Fatalf("stat certificate key: %v", err)
	} else if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("unexpected certificate key mode: got %v want %v", got, want)
	}
}

func TestGenerateServerCertificateUsesLocalCertificateAuthority(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	local, err := EnsureLocalCertificate(Domain, dir)
	if err != nil {
		t.Fatalf("EnsureLocalCertificate returned error: %v", err)
	}
	cert, err := GenerateServerCertificate("buildkite."+Domain, dir)
	if err != nil {
		t.Fatalf("GenerateServerCertificate returned error: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf certificate: %v", err)
	}
	if leaf.Equal(local.Cert) {
		t.Fatal("expected server certificate to use a dynamically generated leaf")
	}
	if leaf.IsCA {
		t.Fatal("expected server certificate not to be a CA")
	}
	if len(cert.Certificate) < 2 {
		t.Fatalf("expected server certificate chain to include the local CA, got %d certificates", len(cert.Certificate))
	}
	issuer, err := x509.ParseCertificate(cert.Certificate[1])
	if err != nil {
		t.Fatalf("parse issuer certificate: %v", err)
	}
	if !issuer.Equal(local.Cert) {
		t.Fatal("expected server certificate chain to include the local CA")
	}
	if block, _ := pem.Decode(local.CertPEM); block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("expected local certificate PEM to contain a certificate")
	}
	if err := leaf.VerifyHostname("buildkite." + Domain); err != nil {
		t.Fatalf("expected leaf to verify buildkite hostname: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(local.Cert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName: "buildkite." + Domain,
		Roots:   roots,
	}); err != nil {
		t.Fatalf("expected server certificate to verify against local CA: %v", err)
	}
}

func TestGenerateServerCertificateDoesNotReplaceLegacyLeafCertificate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeLegacyLeafCertificate(t, dir, Domain)
	certPath := filepath.Join(dir, LocalCertificateFilename)
	before, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read legacy certificate: %v", err)
	}

	_, err = GenerateServerCertificate("buildkite."+Domain, dir)
	if !errors.Is(err, ErrLocalCertificateRequiresInstall) {
		t.Fatalf("expected install-required error, got %v", err)
	}
	after, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read certificate after GenerateServerCertificate: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("expected runtime server certificate generation not to replace the trusted legacy certificate")
	}

	migrated, err := EnsureLocalCertificate(Domain, dir)
	if err != nil {
		t.Fatalf("EnsureLocalCertificate returned error: %v", err)
	}
	if !migrated.Cert.IsCA {
		t.Fatal("expected dns install path to replace the legacy leaf with a CA")
	}
	if bytes.Equal(migrated.CertPEM, before) {
		t.Fatal("expected dns install path to write a new certificate authority")
	}
}

func writeLegacyLeafCertificate(t *testing.T, dir, domain string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate legacy key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate legacy serial: %v", err)
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: domain,
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{domain, "*." + domain},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create legacy certificate: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create legacy certificate dir: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err := os.WriteFile(filepath.Join(dir, LocalCertificateFilename), certPEM, 0o644); err != nil {
		t.Fatalf("write legacy certificate: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(dir, LocalCertificateKeyFilename), keyPEM, 0o600); err != nil {
		t.Fatalf("write legacy key: %v", err)
	}
}
