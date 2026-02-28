//go:build darwin

package darwinvz

import (
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestDockerServiceBootArgsDisabledByDefault(t *testing.T) {
	got := dockerServiceBootArgs(nil, backend.FirecrackerConfig{})
	if got != "cleanroom_service_docker_required=0" {
		t.Fatalf("unexpected docker boot args: %q", got)
	}
}

func TestDockerServiceBootArgsUsesPolicyAndRuntimeSettings(t *testing.T) {
	compiled := &policy.CompiledPolicy{
		Services: policy.Services{
			Docker: policy.DockerService{Required: true},
		},
	}
	cfg := backend.FirecrackerConfig{
		DockerStartupSeconds: 45,
		DockerStorageDriver:  "overlay2",
		DockerIPTables:       true,
	}

	got := dockerServiceBootArgs(compiled, cfg)
	for _, want := range []string{
		"cleanroom_service_docker_required=1",
		"cleanroom_service_docker_startup_timeout=45",
		"cleanroom_service_docker_storage_driver=overlay2",
		"cleanroom_service_docker_iptables=1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in docker boot args %q", want, got)
		}
	}
}
