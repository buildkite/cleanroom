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
	if (routes.DockerHubMirror || len(routes.DockerRegistryMirrors) > 0) && gatewayPort > 0 {
		host := sanitizeKernelArgValue(gateway.GuestGatewayHostname)
		if host != "" {
			args += fmt.Sprintf(
				" cleanroom_service_docker_registry_mirror_host=%s cleanroom_service_docker_registry_mirror_port=%d",
				host,
				gatewayPort,
			)
		}
	}
	if len(routes.DockerRegistryMirrors) > 0 && gatewayPort > 0 {
		if registries := sanitizeRegistryMirrorList(routes.DockerRegistryMirrors); registries != "" {
			args += fmt.Sprintf(" cleanroom_service_docker_registry_mirror_registries=%s", registries)
		}
	}
	return args
}

func sanitizeRegistryMirrorList(registries []string) string {
	out := make([]string, 0, len(registries))
	for _, registry := range registries {
		registry = strings.ToLower(strings.TrimSpace(registry))
		if registry == "" {
			continue
		}
		valid := true
		for _, r := range registry {
			isAlphaNum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			if !isAlphaNum && r != '.' && r != '-' && r != ':' {
				valid = false
				break
			}
		}
		if valid {
			out = append(out, registry)
		}
	}
	return strings.Join(out, ",")
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
