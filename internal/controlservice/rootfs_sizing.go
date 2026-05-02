package controlservice

import (
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

const (
	repositoryBootstrapMinimumRootFSBytes       int64 = 8 << 30
	repositoryBootstrapDockerMinimumRootFSBytes int64 = 16 << 30
)

func withRepositoryBootstrapRootFSMinimum(
	cfg backend.FirecrackerConfig,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
) backend.FirecrackerConfig {
	if repository == nil {
		return cfg
	}

	minimumBytes := repositoryBootstrapMinimumRootFSBytes
	if compiled != nil && compiled.Docker.Required {
		minimumBytes = repositoryBootstrapDockerMinimumRootFSBytes
	}
	if cfg.MinimumRootFSBytes < minimumBytes {
		cfg.MinimumRootFSBytes = minimumBytes
	}
	return cfg
}
