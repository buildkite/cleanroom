package darwinvz

import (
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/policy"
)

func dockerServiceBootArgs(compiled *policy.CompiledPolicy, cfg backend.FirecrackerConfig, gatewayPort int, routes gateway.ProxyRoutes) string {
	if compiled == nil || !compiled.RequiresDockerService() {
		return "cleanroom_service_docker_required=0"
	}

	startupSeconds := cfg.DockerStartupSeconds
	if startupSeconds <= 0 {
		startupSeconds = 20
	}

	storageDriver := sanitizeKernelArgValue(strings.TrimSpace(cfg.DockerStorageDriver))
	if storageDriver == "" {
		storageDriver = "overlay2"
	}

	iptables := 0
	if cfg.DockerIPTables {
		iptables = 1
	}

	args := fmt.Sprintf(
		"cleanroom_service_docker_required=1 cleanroom_service_docker_startup_timeout=%d cleanroom_service_docker_storage_driver=%s cleanroom_service_docker_iptables=%d",
		startupSeconds,
		storageDriver,
		iptables,
	)
	if routes.DockerHubMirror && gatewayPort > 0 {
		host := sanitizeKernelArgValue(gateway.GuestGatewayHostname)
		if host != "" {
			args += fmt.Sprintf(
				" cleanroom_service_docker_registry_mirror_host=%s cleanroom_service_docker_registry_mirror_port=%d",
				host,
				gatewayPort,
			)
		}
	}
	return args
}

func sanitizeKernelArgValue(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		isAlphaNum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if isAlphaNum || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
