package controlservice

import (
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

func TestWithRepositoryBootstrapRootFSMinimumSetsSource(t *testing.T) {
	got := withRepositoryBootstrapRootFSMinimum(
		backend.FirecrackerConfig{},
		&policy.CompiledPolicy{},
		&repositorycheckout.Checkout{},
	)

	if got.MinimumRootFSBytes != repositoryBootstrapMinimumRootFSBytes {
		t.Fatalf("unexpected minimum_rootfs_bytes: got %d want %d", got.MinimumRootFSBytes, repositoryBootstrapMinimumRootFSBytes)
	}
	if got.MinimumRootFSBytesSource != backend.RootFSMinimumSourceRepositoryBootstrap {
		t.Fatalf("unexpected minimum_rootfs_bytes source: got %q want %q", got.MinimumRootFSBytesSource, backend.RootFSMinimumSourceRepositoryBootstrap)
	}
}

func TestWithRepositoryBootstrapRootFSMinimumPreservesHigherConfigSource(t *testing.T) {
	cfg := backend.FirecrackerConfig{
		MinimumRootFSBytes:       64 << 30,
		MinimumRootFSBytesSource: backend.RootFSMinimumSourceConfig,
	}

	got := withRepositoryBootstrapRootFSMinimum(
		cfg,
		&policy.CompiledPolicy{Docker: policy.DockerService{Required: true}},
		&repositorycheckout.Checkout{},
	)

	if got.MinimumRootFSBytes != cfg.MinimumRootFSBytes {
		t.Fatalf("minimum_rootfs_bytes should preserve higher runtime ceiling: got %d want %d", got.MinimumRootFSBytes, cfg.MinimumRootFSBytes)
	}
	if got.MinimumRootFSBytesSource != cfg.MinimumRootFSBytesSource {
		t.Fatalf("minimum_rootfs_bytes source should preserve higher runtime ceiling: got %q want %q", got.MinimumRootFSBytesSource, cfg.MinimumRootFSBytesSource)
	}
}
