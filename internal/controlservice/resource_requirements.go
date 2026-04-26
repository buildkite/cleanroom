package controlservice

import (
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
)

const mibBytes int64 = 1 << 20

func withPolicyResourceRequirements(cfg backend.FirecrackerConfig, resources *policy.Resources) backend.FirecrackerConfig {
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
