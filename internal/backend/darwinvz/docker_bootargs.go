package darwinvz

import (
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
)

func dockerServiceBootArgs(compiled *policy.CompiledPolicy, cfg backend.FirecrackerConfig) string {
	if compiled == nil || !compiled.RequiresDockerService() {
		return "cleanroom_service_docker_required=0"
	}

	startupSeconds := cfg.DockerStartupSeconds
	if startupSeconds <= 0 {
		startupSeconds = 20
	}

	storageDriver := sanitizeKernelArgValue(strings.TrimSpace(cfg.DockerStorageDriver))
	if storageDriver == "" {
		storageDriver = "vfs"
	}

	iptables := 0
	if cfg.DockerIPTables {
		iptables = 1
	}

	return fmt.Sprintf(
		"cleanroom_service_docker_required=1 cleanroom_service_docker_startup_timeout=%d cleanroom_service_docker_storage_driver=%s cleanroom_service_docker_iptables=%d",
		startupSeconds,
		storageDriver,
		iptables,
	)
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
