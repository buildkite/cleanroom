package cli

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type localExposureTestAdapter struct {
	capabilities map[string]bool
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
