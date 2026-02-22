package tlsbootstrap

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateCA(t *testing.T) {
	t.Parallel()

	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	cert := parseCert(t, ca.CertPEM)
	if !cert.IsCA {
		t.Fatal("expected CA certificate")
	}
	if cert.Subject.CommonName != caCommonName {
		t.Fatalf("expected CN %q, got %q", caCommonName, cert.Subject.CommonName)
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatal("expected CertSign key usage")
	}

	parseKey(t, ca.KeyPEM)
}

func TestIssueCert(t *testing.T) {
	t.Parallel()

	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	leaf, err := IssueCert(ca.CertPEM, ca.KeyPEM, "test-server", []string{"example.com", "127.0.0.1", "::1"})
	if err != nil {
		t.Fatalf("IssueCert: %v", err)
	}

	cert := parseCert(t, leaf.CertPEM)
	if cert.IsCA {
		t.Fatal("leaf should not be CA")
	}
	if cert.Subject.CommonName != "test-server" {
		t.Fatalf("expected CN %q, got %q", "test-server", cert.Subject.CommonName)
	}

	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "example.com" {
		t.Fatalf("expected DNS SAN [example.com], got %v", cert.DNSNames)
	}
	expectedIPs := []string{"127.0.0.1", "::1"}
	if len(cert.IPAddresses) != len(expectedIPs) {
		t.Fatalf("expected %d IP SANs, got %d", len(expectedIPs), len(cert.IPAddresses))
	}
	for i, expected := range expectedIPs {
		if !cert.IPAddresses[i].Equal(net.ParseIP(expected)) {
			t.Fatalf("expected IP SAN %s, got %s", expected, cert.IPAddresses[i])
		}
	}

	hasServerAuth := false
	hasClientAuth := false
	for _, usage := range cert.ExtKeyUsage {
		if usage == x509.ExtKeyUsageServerAuth {
			hasServerAuth = true
		}
		if usage == x509.ExtKeyUsageClientAuth {
			hasClientAuth = true
		}
	}
	if !hasServerAuth || !hasClientAuth {
		t.Fatalf("expected both ServerAuth and ClientAuth EKU, got %v", cert.ExtKeyUsage)
	}
}

func TestIssueCertVerifiesAgainstCA(t *testing.T) {
	t.Parallel()

	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	leaf, err := IssueCert(ca.CertPEM, ca.KeyPEM, "test-leaf", []string{"localhost"})
	if err != nil {
		t.Fatalf("IssueCert: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM) {
		t.Fatal("failed to add CA to pool")
	}

	cert := parseCert(t, leaf.CertPEM)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("leaf cert failed verification against CA: %v", err)
	}
}

func TestIssueCertRejectsWrongCA(t *testing.T) {
	t.Parallel()

	ca1, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA (1): %v", err)
	}
	ca2, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA (2): %v", err)
	}

	leaf, err := IssueCert(ca1.CertPEM, ca1.KeyPEM, "test-leaf", []string{"localhost"})
	if err != nil {
		t.Fatalf("IssueCert: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca2.CertPEM)

	cert := parseCert(t, leaf.CertPEM)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err == nil {
		t.Fatal("expected verification to fail with wrong CA")
	}
}

func TestMutualTLSHandshake(t *testing.T) {
	t.Parallel()

	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	serverKP, err := IssueCert(ca.CertPEM, ca.KeyPEM, "server", []string{"localhost", "127.0.0.1"})
	if err != nil {
		t.Fatalf("issue server cert: %v", err)
	}

	clientKP, err := IssueCert(ca.CertPEM, ca.KeyPEM, "client", nil)
	if err != nil {
		t.Fatalf("issue client cert: %v", err)
	}

	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(ca.CertPEM)

	serverCert, err := tls.X509KeyPair(serverKP.CertPEM, serverKP.KeyPEM)
	if err != nil {
		t.Fatalf("load server keypair: %v", err)
	}

	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		tlsConn := conn.(*tls.Conn)
		done <- tlsConn.Handshake()
	}()

	clientCert, err := tls.X509KeyPair(clientKP.CertPEM, clientKP.KeyPEM)
	if err != nil {
		t.Fatalf("load client keypair: %v", err)
	}

	clientTLS := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
	}

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer conn.Close()

	if err := <-done; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
}

func TestMutualTLSRejectsNoClientCert(t *testing.T) {
	t.Parallel()

	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	serverKP, err := IssueCert(ca.CertPEM, ca.KeyPEM, "server", []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue server cert: %v", err)
	}

	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(ca.CertPEM)

	serverCert, err := tls.X509KeyPair(serverKP.CertPEM, serverKP.KeyPEM)
	if err != nil {
		t.Fatalf("load server keypair: %v", err)
	}

	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		tlsConn := conn.(*tls.Conn)
		done <- tlsConn.Handshake()
	}()

	clientTLS := &tls.Config{
		RootCAs:    caPool,
		MinVersion: tls.VersionTLS13,
	}

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
	if err != nil {
		// Connection refused at dial is also acceptable.
		return
	}
	defer conn.Close()

	if err := <-done; err == nil {
		t.Fatal("expected server handshake to fail without client cert")
	}
}

func TestInit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := Init(dir, false); err != nil {
		t.Fatalf("Init: %v", err)
	}

	expectedFiles := []string{"ca.pem", "ca.key", "server.pem", "server.key", "client.pem", "client.key"}
	for _, name := range expectedFiles {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
		if filepath.Ext(name) == ".key" {
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("expected %s to have 0600 permissions, got %o", name, info.Mode().Perm())
			}
		}
	}
}

func TestInitRefusesOverwrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := Init(dir, false); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := Init(dir, false); err == nil {
		t.Fatal("expected Init to refuse overwriting existing CA")
	}
}

func TestInitForceOverwrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := Init(dir, false); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := Init(dir, true); err != nil {
		t.Fatalf("Init with force: %v", err)
	}
}

func parseCert(t *testing.T, pemData []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemData)
	if block == nil {
		t.Fatal("failed to decode PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}

func parseKey(t *testing.T, pemData []byte) *ecdsa.PrivateKey {
	t.Helper()
	block, _ := pem.Decode(pemData)
	if block == nil {
		t.Fatal("failed to decode key PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse EC key: %v", err)
	}
	return key
}
