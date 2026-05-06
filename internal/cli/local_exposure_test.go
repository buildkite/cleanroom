package cli

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
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
	}}, nil)
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

type localExposureLoader struct {
	repository policy.RepositoryConfig
	err        error
}

func (l localExposureLoader) LoadAndCompile(string) (*policy.CompiledPolicy, string, error) {
	return nil, "", errors.New("unexpected LoadAndCompile call")
}

func (l localExposureLoader) LoadRepository(string) (policy.RepositoryConfig, string, error) {
	if l.err != nil {
		return policy.RepositoryConfig{}, "", l.err
	}
	return l.repository, "/repo/cleanroom.yaml", nil
}

func TestResolveExposureCertificateDomainsMergesRuntimeAndPolicyDomains(t *testing.T) {
	t.Parallel()

	ctx := &runtimeContext{
		CWD: "/repo",
		Config: runtimeconfig.Config{
			Exposure: runtimeconfig.ExposureConfig{
				CertificateDomains: []string{"*.buildkite.cleanroom.localhost"},
			},
		},
		Loader: localExposureLoader{
			repository: policy.RepositoryConfig{
				Mode: "none",
				ExposureCertificateDomains: []string{
					"api.buildkite.cleanroom.localhost",
					"*.buildkite.cleanroom.localhost",
				},
			},
		},
	}

	got, err := resolveExposureCertificateDomains(ctx, ctx.CWD)
	if err != nil {
		t.Fatalf("resolveExposureCertificateDomains returned error: %v", err)
	}
	want := []string{"*.buildkite.cleanroom.localhost", "api.buildkite.cleanroom.localhost"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected merged domains: got %v want %v", got, want)
	}
}

func TestResolveExposureCertificateDomainsUsesExplicitWorkingDirectory(t *testing.T) {
	t.Parallel()

	var loadedPath string
	ctx := &runtimeContext{
		CWD: "/shell",
		Loader: localExposureLoaderFunc(func(path string) (policy.RepositoryConfig, string, error) {
			loadedPath = path
			if path != "/repo" {
				return policy.RepositoryConfig{}, "", policy.ErrPolicyNotFound
			}
			return policy.RepositoryConfig{
				Mode: "none",
				ExposureCertificateDomains: []string{
					"*.buildkite.cleanroom.localhost",
					"api.buildkite.cleanroom.localhost",
				},
			}, "/repo/cleanroom.yaml", nil
		}),
	}

	got, err := resolveExposureCertificateDomains(ctx, "/repo")
	if err != nil {
		t.Fatalf("resolveExposureCertificateDomains returned error: %v", err)
	}
	if loadedPath != "/repo" {
		t.Fatalf("expected loader to use explicit cwd, got %q", loadedPath)
	}
	want := []string{"*.buildkite.cleanroom.localhost", "api.buildkite.cleanroom.localhost"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected merged domains: got %v want %v", got, want)
	}
}

func TestResolveRequestedExposureCertificateDomainsSkipsPolicyLoadWithoutHTTPS(t *testing.T) {
	t.Parallel()

	ctx := &runtimeContext{
		CWD: "/repo",
		Loader: localExposureLoader{
			err: errors.New("unexpected repository load"),
		},
	}

	got, err := resolveRequestedExposureCertificateDomains(ctx, "", []*cleanroomv1.PortExposure{{
		Protocol:  exposureProtocolTCP,
		GuestPort: 3000,
	}})
	if err != nil {
		t.Fatalf("resolveRequestedExposureCertificateDomains returned error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil domains for TCP-only exposure, got %v", got)
	}
}

func TestResolveRequestedExposureCertificateDomainsLoadsPolicyForHTTPS(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("unexpected repository load")
	ctx := &runtimeContext{
		CWD: "/repo",
		Loader: localExposureLoader{
			err: wantErr,
		},
	}

	_, err := resolveRequestedExposureCertificateDomains(ctx, "", []*cleanroomv1.PortExposure{{
		Protocol:  exposureProtocolHTTPS,
		GuestPort: 3000,
	}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected error %v, got %v", wantErr, err)
	}
}

type localExposureLoaderFunc func(string) (policy.RepositoryConfig, string, error)

func (f localExposureLoaderFunc) LoadAndCompile(string) (*policy.CompiledPolicy, string, error) {
	return nil, "", errors.New("unexpected LoadAndCompile call")
}

func (f localExposureLoaderFunc) LoadRepository(path string) (policy.RepositoryConfig, string, error) {
	return f(path)
}
