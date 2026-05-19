package controlservice

import (
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestWithPolicyResourceMinimumsRaisesLowerRuntimeConfig(t *testing.T) {
	cfg := backend.FirecrackerConfig{
		VCPUs:              2,
		MemoryMiB:          1024,
		MinimumRootFSBytes: 8 << 30,
	}

	got := withPolicyResourceMinimums(cfg, &policy.Resources{
		VCPUs:       4,
		MemoryBytes: (3 << 30) + 1,
		DiskBytes:   16 << 30,
	})

	if got.VCPUs != 4 {
		t.Fatalf("unexpected vcpus: got %d want 4", got.VCPUs)
	}
	if got.MemoryMiB != 3073 {
		t.Fatalf("unexpected memory_mib: got %d want 3073", got.MemoryMiB)
	}
	if got.MinimumRootFSBytes != 16<<30 {
		t.Fatalf("unexpected minimum_rootfs_bytes: got %d want %d", got.MinimumRootFSBytes, int64(16<<30))
	}
	if got, want := got.MinimumRootFSBytesSource, backend.RootFSMinimumSourcePolicy; got != want {
		t.Fatalf("unexpected minimum_rootfs_bytes source: got %q want %q", got, want)
	}
}

func TestWithPolicyResourceMinimumsPreservesHigherRuntimeConfig(t *testing.T) {
	cfg := backend.FirecrackerConfig{
		VCPUs:                    8,
		MemoryMiB:                8192,
		MinimumRootFSBytes:       64 << 30,
		MinimumRootFSBytesSource: backend.RootFSMinimumSourceConfig,
	}

	got := withPolicyResourceMinimums(cfg, &policy.Resources{
		VCPUs:       4,
		MemoryBytes: 3 << 30,
		DiskBytes:   16 << 30,
	})

	if got.VCPUs != cfg.VCPUs {
		t.Fatalf("vcpus should preserve higher runtime ceiling: got %d want %d", got.VCPUs, cfg.VCPUs)
	}
	if got.MemoryMiB != cfg.MemoryMiB {
		t.Fatalf("memory_mib should preserve higher runtime ceiling: got %d want %d", got.MemoryMiB, cfg.MemoryMiB)
	}
	if got.MinimumRootFSBytes != cfg.MinimumRootFSBytes {
		t.Fatalf("minimum_rootfs_bytes should preserve higher runtime ceiling: got %d want %d", got.MinimumRootFSBytes, cfg.MinimumRootFSBytes)
	}
	if got.MinimumRootFSBytesSource != cfg.MinimumRootFSBytesSource {
		t.Fatalf("minimum_rootfs_bytes source should preserve higher runtime ceiling: got %q want %q", got.MinimumRootFSBytesSource, cfg.MinimumRootFSBytesSource)
	}
}

func TestWithPolicyResourceMinimumsNilResourcesIsNoOp(t *testing.T) {
	cfg := backend.FirecrackerConfig{
		VCPUs:              2,
		MemoryMiB:          1024,
		MinimumRootFSBytes: 8 << 30,
	}

	got := withPolicyResourceMinimums(cfg, nil)

	if got != cfg {
		t.Fatalf("expected nil resources to preserve config: got %#v want %#v", got, cfg)
	}
}

func TestWithBackendLaunchResourceDefaultsFillsUnsetLaunchCeilings(t *testing.T) {
	got := withBackendLaunchResourceDefaults(backend.FirecrackerConfig{})

	if got.VCPUs != backend.DefaultVCPUs {
		t.Fatalf("unexpected default vcpus: got %d want %d", got.VCPUs, backend.DefaultVCPUs)
	}
	if got.MemoryMiB != backend.DefaultMemoryMiB {
		t.Fatalf("unexpected default memory_mib: got %d want %d", got.MemoryMiB, backend.DefaultMemoryMiB)
	}
}

func TestWithBackendLaunchResourceDefaultsPreservesExplicitLaunchCeilings(t *testing.T) {
	cfg := backend.FirecrackerConfig{
		VCPUs:     4,
		MemoryMiB: 2048,
	}

	got := withBackendLaunchResourceDefaults(cfg)

	if got.VCPUs != cfg.VCPUs {
		t.Fatalf("vcpus should preserve explicit runtime ceiling: got %d want %d", got.VCPUs, cfg.VCPUs)
	}
	if got.MemoryMiB != cfg.MemoryMiB {
		t.Fatalf("memory_mib should preserve explicit runtime ceiling: got %d want %d", got.MemoryMiB, cfg.MemoryMiB)
	}
}
