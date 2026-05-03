//go:build darwin

package darwinvz

import (
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestDockerServiceBootArgsDisabledByDefault(t *testing.T) {
	got := dockerServiceBootArgs(nil, backend.FirecrackerConfig{}, 0, gateway.ProxyRoutes{})
	if got != "cleanroom_service_docker_required=0" {
		t.Fatalf("unexpected docker boot args: %q", got)
	}
}

func TestDockerServiceBootArgsUsesPolicyAndRuntimeSettings(t *testing.T) {
	compiled := &policy.CompiledPolicy{
		Docker: policy.DockerService{Required: true},
	}
	cfg := backend.FirecrackerConfig{
		DockerStartupSeconds: 45,
		DockerStorageDriver:  "overlay2",
		DockerIPTables:       true,
	}

	got := dockerServiceBootArgs(compiled, cfg, 8170, gateway.ProxyRoutes{DockerHubMirror: true})
	for _, want := range []string{
		"cleanroom_service_docker_required=1",
		"cleanroom_service_docker_startup_timeout=45",
		"cleanroom_service_docker_storage_driver=overlay2",
		"cleanroom_service_docker_iptables=1",
		"cleanroom_service_docker_registry_mirror_host=gateway.cleanroom.internal",
		"cleanroom_service_docker_registry_mirror_port=8170",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in docker boot args %q", want, got)
		}
	}
}

func TestDockerServiceBootArgsIncludesConfiguredRegistryMirrors(t *testing.T) {
	compiled := &policy.CompiledPolicy{
		Docker: policy.DockerService{Required: true},
	}

	got := dockerServiceBootArgs(compiled, backend.FirecrackerConfig{}, 8170, gateway.ProxyRoutes{
		DockerHubMirror:       true,
		DockerRegistryMirrors: []string{"public.ecr.aws", "ghcr.io", "bad/path"},
	})
	for _, want := range []string{
		"cleanroom_service_docker_registry_mirror_host=gateway.cleanroom.internal",
		"cleanroom_service_docker_registry_mirror_port=8170",
		"cleanroom_service_docker_registry_mirror_registries=public.ecr.aws,ghcr.io",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in docker boot args %q", want, got)
		}
	}
}

func TestDockerServiceBootArgsConfiguredRegistryMirrorsCarryGatewayEndpoint(t *testing.T) {
	compiled := &policy.CompiledPolicy{
		Docker: policy.DockerService{Required: true},
	}

	got := dockerServiceBootArgs(compiled, backend.FirecrackerConfig{}, 8170, gateway.ProxyRoutes{
		DockerRegistryMirrors: []string{"public.ecr.aws"},
	})
	for _, want := range []string{
		"cleanroom_service_docker_registry_mirror_host=gateway.cleanroom.internal",
		"cleanroom_service_docker_registry_mirror_port=8170",
		"cleanroom_service_docker_registry_mirror_registries=public.ecr.aws",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in docker boot args %q", want, got)
		}
	}
}

func TestDockerServiceBootArgsSkipsMirrorWithoutLiveRoute(t *testing.T) {
	compiled := &policy.CompiledPolicy{
		Docker: policy.DockerService{Required: true},
	}

	got := dockerServiceBootArgs(compiled, backend.FirecrackerConfig{}, 8170, gateway.ProxyRoutes{})
	if strings.Contains(got, "cleanroom_service_docker_registry_mirror_host") {
		t.Fatalf("did not expect docker mirror host in boot args %q", got)
	}
	if strings.Contains(got, "cleanroom_service_docker_registry_mirror_port") {
		t.Fatalf("did not expect docker mirror port in boot args %q", got)
	}
	if strings.Contains(got, "cleanroom_service_docker_registry_mirror_registries") {
		t.Fatalf("did not expect docker mirror registries in boot args %q", got)
	}
}
