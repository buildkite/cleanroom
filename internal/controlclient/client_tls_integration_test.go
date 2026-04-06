package controlclient

import (
	"context"
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

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type tlsTestAdapter struct{}

func (tlsTestAdapter) Name() string { return "firecracker" }

func (tlsTestAdapter) Provision(context.Context, backend.ProvisionRequest) error { return nil }

func (tlsTestAdapter) Run(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
	return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
}

func (tlsTestAdapter) Terminate(context.Context, string) error { return nil }

func TestHTTPSControlPlaneDiscoversTLSMaterial(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	writeTLSMaterial(t, configHome, net.ParseIP("127.0.0.1"))

	addr := reserveTCPAddr(t)
	ep := endpoint.Endpoint{
		Scheme:  "https",
		Address: "https://" + addr,
		BaseURL: "https://" + addr,
	}

	service := &controlservice.Service{
		Config: runtimeconfig.Config{DefaultBackend: "firecracker"},
		Backends: map[string]backend.SandboxAdapter{
			"firecracker": tlsTestAdapter{},
		},
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- controlserver.Serve(runCtx, ep, controlserver.New(service, nil).Handler(), nil, nil)
	}()

	waitForTLSServer(t, addr)

	client, err := New(ep)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx, rpcCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rpcCancel()

	createResp, err := client.CreateSandbox(ctx, &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  tlsTestPolicy(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()
	if sandboxID == "" {
		t.Fatal("expected sandbox id")
	}

	getResp, err := client.GetSandbox(ctx, &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetSandboxId(), sandboxID; got != want {
		t.Fatalf("GetSandbox id mismatch: got %q want %q", got, want)
	}

	untrustedConfigHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", untrustedConfigHome)

	untrustedClient, err := New(ep)
	if err != nil {
		t.Fatalf("New for untrusted client returned error: %v", err)
	}
	_, err = untrustedClient.ListSandboxes(ctx, &cleanroomv1.ListSandboxesRequest{})
	if err == nil {
		t.Fatal("expected TLS verification failure without discovered CA")
	}
	if !strings.Contains(err.Error(), "unknown authority") && !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("unexpected TLS verification error: %v", err)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
}

func tlsTestPolicy() *cleanroomv1.Policy {
	return &cleanroomv1.Policy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
	}
}

func reserveTCPAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen returned error: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("listener.Close returned error: %v", err)
	}
	return addr
}

func waitForTLSServer(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for TLS server on %s", addr)
}

func writeTLSMaterial(t *testing.T, configHome string, ip net.IP) {
	t.Helper()

	tlsDir := filepath.Join(configHome, "cleanroom", "tls")
	if err := os.MkdirAll(tlsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}

	now := time.Now().UTC()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey for CA returned error: %v", err)
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
		t.Fatalf("CreateCertificate for CA returned error: %v", err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey for server returned error: %v", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-1 * time.Minute),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{ip},
		DNSNames:     []string{"localhost"},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("CreateCertificate for server returned error: %v", err)
	}

	writePEMFile(t, filepath.Join(tlsDir, "ca.pem"), "CERTIFICATE", caDER)

	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey returned error: %v", err)
	}
	writePEMFile(t, filepath.Join(tlsDir, "server.pem"), "CERTIFICATE", serverDER)
	writePEMFile(t, filepath.Join(tlsDir, "server.key"), "EC PRIVATE KEY", serverKeyDER)
}

func writePEMFile(t *testing.T, path, blockType string, der []byte) {
	t.Helper()

	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) returned error: %v", path, err)
	}
}
