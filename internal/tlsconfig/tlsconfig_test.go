package tlsconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveServerReturnsNilWithoutCompletePair(t *testing.T) {
	t.Helper()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	tlsDir := filepath.Join(configHome, "cleanroom", "tls")
	if err := os.MkdirAll(tlsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	writePEMFile(t, filepath.Join(tlsDir, "server.pem"), "CERTIFICATE", []byte("not-a-cert"))

	cfg, err := ResolveServer(Options{})
	if err != nil {
		t.Fatalf("ResolveServer returned error: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil server config when key pair is incomplete, got %#v", cfg)
	}
}

func TestResolveServerDiscoversCertificatePair(t *testing.T) {
	t.Helper()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	writeServerTLSMaterial(t, configHome, net.ParseIP("127.0.0.1"))

	cfg, err := ResolveServer(Options{})
	if err != nil {
		t.Fatalf("ResolveServer returned error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected discovered server TLS config")
	}
	if got, want := len(cfg.Certificates), 1; got != want {
		t.Fatalf("unexpected certificate count: got %d want %d", got, want)
	}
}

func TestResolveClientRejectsClientCertificates(t *testing.T) {
	t.Helper()

	_, err := ResolveClient(Options{CertPath: "/tmp/client.pem"})
	if err == nil {
		t.Fatal("expected client certificate rejection")
	}
	if !strings.Contains(err.Error(), "client certificates are not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveClientLoadsExplicitCA(t *testing.T) {
	t.Helper()

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	writeCACertificate(t, caPath)

	cfg, err := ResolveClient(Options{CAPath: caPath})
	if err != nil {
		t.Fatalf("ResolveClient returned error: %v", err)
	}
	if cfg == nil || cfg.RootCAs == nil {
		t.Fatalf("expected client RootCAs, got %#v", cfg)
	}
}

func TestResolveClientDiscoversCAFromXDGConfig(t *testing.T) {
	t.Helper()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	tlsDir := filepath.Join(configHome, "cleanroom", "tls")
	if err := os.MkdirAll(tlsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	writeCACertificate(t, filepath.Join(tlsDir, "ca.pem"))

	cfg, err := ResolveClient(Options{})
	if err != nil {
		t.Fatalf("ResolveClient returned error: %v", err)
	}
	if cfg == nil || cfg.RootCAs == nil {
		t.Fatalf("expected discovered client RootCAs, got %#v", cfg)
	}
}

func TestResolveClientRejectsInvalidCAFile(t *testing.T) {
	t.Helper()

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, []byte("not-a-certificate"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	_, err := ResolveClient(Options{CAPath: caPath})
	if err == nil {
		t.Fatal("expected invalid CA error")
	}
	if !strings.Contains(err.Error(), "no valid certificates") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func writeServerTLSMaterial(t *testing.T, configHome string, ip net.IP) {
	t.Helper()

	tlsDir := filepath.Join(configHome, "cleanroom", "tls")
	if err := os.MkdirAll(tlsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}

	now := time.Now().UTC()
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey returned error: %v", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-1 * time.Minute),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{ip},
		DNSNames:     []string{"localhost"},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, serverTemplate, &serverKey.PublicKey, serverKey)
	if err != nil {
		t.Fatalf("CreateCertificate returned error: %v", err)
	}
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey returned error: %v", err)
	}

	writePEMFile(t, filepath.Join(tlsDir, "server.pem"), "CERTIFICATE", serverDER)
	writePEMFile(t, filepath.Join(tlsDir, "server.key"), "EC PRIVATE KEY", serverKeyDER)
}

func writeCACertificate(t *testing.T, path string) {
	t.Helper()

	now := time.Now().UTC()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey returned error: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cleanroom-test-ca"},
		NotBefore:             now.Add(-1 * time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("CreateCertificate returned error: %v", err)
	}
	writePEMFile(t, path, "CERTIFICATE", caDER)
}

func writePEMFile(t *testing.T, path, blockType string, der []byte) {
	t.Helper()

	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) returned error: %v", path, err)
	}
}
