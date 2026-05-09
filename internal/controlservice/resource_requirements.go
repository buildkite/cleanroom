package controlservice

import (
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
)

const mibBytes int64 = 1 << 20

func withBackendLaunchResourceDefaults(cfg backend.FirecrackerConfig) backend.FirecrackerConfig {
	if cfg.VCPUs <= 0 {
		cfg.VCPUs = backend.DefaultVCPUs
	}
	if cfg.MemoryMiB <= 0 {
		cfg.MemoryMiB = backend.DefaultMemoryMiB
	}
	return cfg
}

// withPolicyResourceMinimums translates sandbox.resources floors into the
// backend launch envelope.
//
// Policy resources are minimum workload requirements, not exact allocations.
// The selected runtime config may already provide larger effective ceilings; in
// that case the larger runtime values are preserved. Backends may expose those
// ceilings as fixed launch config or another backend-specific mechanism.
func withPolicyResourceMinimums(cfg backend.FirecrackerConfig, resources *policy.Resources) backend.FirecrackerConfig {
	if resources == nil {
		return cfg
	}
	if resources.VCPUs > cfg.VCPUs {
		cfg.VCPUs = resources.VCPUs
	}
	if memoryMiB := bytesToMiBCeil(resources.MemoryBytes); memoryMiB > cfg.MemoryMiB {
		cfg.MemoryMiB = memoryMiB
	}
	if resources.DiskBytes > cfg.MinimumRootFSBytes {
		cfg.MinimumRootFSBytes = resources.DiskBytes
	}
	return cfg
}

func bytesToMiBCeil(value int64) int64 {
	if value <= 0 {
		return 0
	}
	return 1 + (value-1)/mibBytes
}
