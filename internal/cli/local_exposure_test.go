package cli

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/buildkite/cleanroom/internal/exposure"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type localExposureTestAdapter struct {
	capabilities map[string]bool
}

type localExposureSuspendAdapter struct {
	localExposureTestAdapter
}

func (a *localExposureTestAdapter) Name() string { return "firecracker" }

func (a *localExposureTestAdapter) Capabilities() map[string]bool {
	return a.capabilities
}

func (a *localExposureTestAdapter) ProvisionSandbox(context.Context, backend.ProvisionRequest) error {
	return nil
}

func (a *localExposureTestAdapter) RunInSandbox(context.Context, backend.ExecutionRequest, backend.OutputStream) (*backend.ExecutionResult, error) {
	return nil, nil
}

func (a *localExposureTestAdapter) TerminateSandbox(context.Context, string) error {
	return nil
}

func (a *localExposureSuspendAdapter) SuspendSandbox(context.Context, string) error {
	return nil
}

func (a *localExposureSuspendAdapter) ResumeSandbox(context.Context, string) error {
	return nil
}

func TestStartClientExposuresRejectsUnsupportedSandboxPortDial(t *testing.T) {
	client, sandboxID := newLocalExposureTestClient(t, &localExposureTestAdapter{
		capabilities: map[string]bool{backend.CapabilitySandboxPortDial: false},
	})

	manager, _, err := startClientExposures(context.Background(), client, sandboxID, []*cleanroomv1.PortExposure{{
		Protocol:  exposureProtocolTCP,
		GuestPort: 3000,
	}})
	if manager != nil {
		_ = manager.Close()
	}
	if err == nil {
		t.Fatal("expected startClientExposures to reject unsupported sandbox port dialing")
	}
	if !strings.Contains(err.Error(), "does not support sandbox port dialing") {
		t.Fatalf("unexpected startClientExposures error: %v", err)
	}
}

func TestStartClientExposuresAllowsSuspendedWakeableSandbox(t *testing.T) {
	client, sandboxID := newLocalExposureTestClient(t, &localExposureSuspendAdapter{
		localExposureTestAdapter: localExposureTestAdapter{
			capabilities: map[string]bool{
				backend.CapabilitySandboxPortDial: true,
				backend.CapabilitySandboxSuspend:  true,
			},
		},
	})
	if _, err := client.SuspendSandbox(context.Background(), &cleanroomv1.SuspendSandboxRequest{SandboxId: sandboxID}); err != nil {
		t.Fatalf("SuspendSandbox returned error: %v", err)
	}

	manager, _, err := startClientExposures(context.Background(), client, sandboxID, []*cleanroomv1.PortExposure{{
		Protocol:  exposureProtocolTCP,
		HostPort:  int32(freeLocalExposureTCPPort(t)),
		GuestPort: 3000,
	}})
	if manager != nil {
		_ = manager.Close()
	}
	if err != nil {
		t.Fatalf("startClientExposures returned error: %v", err)
	}
}

func TestPrevalidateRequestedExposuresChecksHTTPSCertificate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tlsDir, err := exposure.DefaultTLSDir()
	if err != nil {
		t.Fatalf("DefaultTLSDir returned error: %v", err)
	}
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		t.Fatalf("create TLS dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tlsDir, exposure.LocalCertificateFilename), []byte("not a certificate"), 0o644); err != nil {
		t.Fatalf("write invalid certificate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tlsDir, exposure.LocalCertificateKeyFilename), []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write invalid key: %v", err)
	}

	err = prevalidateRequestedExposures(&runtimeContext{Loader: failingLoader{}}, t.TempDir(), []*cleanroomv1.PortExposure{{
		Protocol:  exposureProtocolHTTPS,
		Name:      "buildkite",
		GuestPort: 3000,
	}})
	if err == nil {
		t.Fatal("expected HTTPS exposure prevalidation to fail on invalid local certificate")
	}
	if !strings.Contains(err.Error(), "certificate PEM") {
		t.Fatalf("unexpected prevalidation error: %v", err)
	}
}

func newLocalExposureTestClient(t *testing.T, adapter backend.Adapter) (*controlclient.Client, string) {
	t.Helper()
	service := &controlservice.Service{
		Config: runtimeconfig.Config{DefaultBackend: "firecracker"},
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
	}
	httpServer := httptest.NewServer(controlserver.New(service, nil).Handler())
	t.Cleanup(httpServer.Close)

	client, err := controlclient.New(endpoint.Endpoint{Scheme: "http", BaseURL: httpServer.URL})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	createResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy: &cleanroomv1.Policy{
			Version:        1,
			ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			NetworkDefault: "deny",
		},
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := strings.TrimSpace(createResp.GetSandbox().GetSandboxId())
	if sandboxID == "" {
		t.Fatal("CreateSandbox returned empty sandbox id")
	}
	return client, sandboxID
}

func freeLocalExposureTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on free TCP port: %v", err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listen address %q: %v", ln.Addr().String(), err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse listen port %q: %v", port, err)
	}
	return n
}
