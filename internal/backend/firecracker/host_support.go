package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
)

type HostSupport struct {
	RuntimeUsable    bool
	SnapshotsUsable  bool
	ZFSUsable        bool
	ZFSDatasetRoot   string
	RuntimeMessage   string
	SnapshotMessage  string
	ZFSMessage       string
	ZFSCandidates    []string
	HelperPath       string
	HelperVersion    string
	HelperCaps       []string
	RequiredCommands []string
}

var hostSupportLookPath = exec.LookPath

var hostSupportResolveCommand = resolveHostSupportCommandPath

var hostSupportGOOS = runtime.GOOS

var hostSupportCommandOutput = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
	return runCombinedCommandOutput(ctx, append([]string{binary}, args...), append([]string{binary}, args...))
}

func DetectHostSupport(ctx context.Context, cfg backend.FirecrackerConfig) HostSupport {
	result := HostSupport{
		HelperPath:       resolvePrivilegedHelperPath(cfg),
		RequiredCommands: []string{"sudo", "ip", "iptables", "sysctl"},
	}

	if hostSupportGOOS != "linux" {
		result.RuntimeMessage = fmt.Sprintf("firecracker host runtime bootstrap is only available on linux (current host: %s)", hostSupportGOOS)
		result.SnapshotMessage = result.RuntimeMessage
		result.ZFSMessage = result.RuntimeMessage
		return result
	}

	missingCommands := make([]string, 0, len(result.RequiredCommands))
	for _, command := range result.RequiredCommands {
		if _, err := hostSupportResolveCommand(command); err != nil {
			missingCommands = append(missingCommands, command)
		}
	}
	if len(missingCommands) > 0 {
		result.RuntimeMessage = fmt.Sprintf("missing required host commands: %s", strings.Join(missingCommands, ", "))
		result.SnapshotMessage = result.RuntimeMessage
		result.ZFSMessage = result.RuntimeMessage
		return result
	}

	if _, err := os.Stat(result.HelperPath); err != nil {
		result.RuntimeMessage = fmt.Sprintf("privileged helper %q is not accessible: %v", result.HelperPath, err)
		result.SnapshotMessage = result.RuntimeMessage
		result.ZFSMessage = result.RuntimeMessage
		return result
	}

	version, err := helperVersion(ctx, cfg)
	if err != nil {
		result.RuntimeMessage = fmt.Sprintf("privileged helper version probe failed: %v", err)
		result.SnapshotMessage = result.RuntimeMessage
		result.ZFSMessage = result.RuntimeMessage
		return result
	}
	result.HelperVersion = version

	caps, err := helperCapabilities(ctx, cfg)
	if err != nil {
		result.RuntimeMessage = fmt.Sprintf("privileged helper capability probe failed: %v", err)
		result.SnapshotMessage = result.RuntimeMessage
		result.ZFSMessage = result.RuntimeMessage
		return result
	}
	result.HelperCaps = append(result.HelperCaps, caps...)

	runtimeMissingCaps := helperMissingCapabilities(caps, []string{
		helperCapabilityFirecrackerNetwork,
		helperCapabilityFirecrackerTrustedDNS,
	})
	if len(runtimeMissingCaps) > 0 {
		result.RuntimeMessage = fmt.Sprintf("privileged helper is missing required capabilities: %s", strings.Join(runtimeMissingCaps, ", "))
		result.SnapshotMessage = result.RuntimeMessage
		result.ZFSMessage = result.RuntimeMessage
		return result
	}

	if err := runRootCommand(ctx, cfg, "true"); err != nil {
		result.RuntimeMessage = fmt.Sprintf("privileged command probe failed: %v", err)
		result.SnapshotMessage = result.RuntimeMessage
		result.ZFSMessage = result.RuntimeMessage
		return result
	}
	if err := runRootCommand(ctx, cfg, "ip", "link", "show"); err != nil {
		result.RuntimeMessage = fmt.Sprintf("privileged ip probe failed: %v", err)
		result.SnapshotMessage = result.RuntimeMessage
		result.ZFSMessage = result.RuntimeMessage
		return result
	}

	result.RuntimeUsable = true
	result.SnapshotsUsable = true
	result.RuntimeMessage = fmt.Sprintf("helper-based firecracker host runtime is usable via %s (helper contract version %s)", result.HelperPath, result.HelperVersion)
	result.SnapshotMessage = "firecracker snapshot runtime is usable"

	if missingCaps := helperMissingCapabilities(caps, []string{helperCapabilityFirecrackerZFS}); len(missingCaps) > 0 {
		result.ZFSMessage = fmt.Sprintf("privileged helper is missing required capabilities: %s", strings.Join(missingCaps, ", "))
		return result
	}

	zfsPath, err := lookPathWithFallback("zfs", "/usr/sbin/zfs", "/sbin/zfs")
	if err != nil {
		result.ZFSMessage = fmt.Sprintf("zfs command not available: %v", err)
		return result
	}

	requestedDataset := strings.TrimSpace(cfg.Snapshots.ZFSDataset)
	if requestedDataset != "" {
		if err := validateZFSDatasetRoot(ctx, cfg, requestedDataset); err != nil {
			result.ZFSMessage = err.Error()
			return result
		}
		result.ZFSUsable = true
		result.ZFSDatasetRoot = requestedDataset
		result.ZFSMessage = fmt.Sprintf("configured Cleanroom ZFS dataset root %q is usable", requestedDataset)
		return result
	}

	candidates, err := discoverCleanroomZFSDatasetRoots(ctx, zfsPath)
	if err != nil {
		result.ZFSMessage = fmt.Sprintf("unable to enumerate ZFS datasets: %v", err)
		return result
	}
	result.ZFSCandidates = append(result.ZFSCandidates, candidates...)

	switch len(candidates) {
	case 0:
		result.ZFSMessage = "no existing Cleanroom ZFS dataset root matched cleanroom or */cleanroom"
		return result
	case 1:
		if err := validateZFSDatasetRoot(ctx, cfg, candidates[0]); err != nil {
			result.ZFSMessage = err.Error()
			return result
		}
		result.ZFSUsable = true
		result.ZFSDatasetRoot = candidates[0]
		result.ZFSMessage = fmt.Sprintf("auto-detected Cleanroom ZFS dataset root %q", candidates[0])
		return result
	default:
		result.ZFSMessage = fmt.Sprintf("multiple Cleanroom ZFS dataset roots detected: %s", strings.Join(candidates, ", "))
		return result
	}
}

func discoverCleanroomZFSDatasetRoots(ctx context.Context, zfsPath string) ([]string, error) {
	out, err := hostSupportCommandOutput(ctx, zfsPath, "list", "-H", "-o", "name")
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var candidates []string
	for _, line := range strings.Split(string(out), "\n") {
		candidate := strings.TrimSpace(line)
		if !isCleanroomZFSDatasetRoot(candidate) {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		candidates = append(candidates, candidate)
	}
	slices.Sort(candidates)
	return candidates, nil
}

func validateZFSDatasetRoot(ctx context.Context, cfg backend.FirecrackerConfig, dataset string) error {
	return validateZFSDatasetRootWithRunner(ctx, newPrivilegedCommandRunner(cfg), dataset)
}

func isCleanroomZFSDatasetRoot(dataset string) bool {
	dataset = strings.Trim(strings.TrimSpace(dataset), "/")
	if dataset == "" {
		return false
	}
	if dataset == "cleanroom" {
		return true
	}
	parts := strings.Split(dataset, "/")
	return parts[len(parts)-1] == "cleanroom"
}

func resolveHostSupportCommandPath(command string) (string, error) {
	switch strings.TrimSpace(command) {
	case "sudo":
		return lookPathWithFallback(command, "/usr/bin/sudo", "/bin/sudo")
	case "ip":
		return lookPathWithFallback(command, "/usr/sbin/ip", "/sbin/ip")
	case "iptables":
		return lookPathWithFallback(command, "/usr/sbin/iptables", "/sbin/iptables")
	case "sysctl":
		return lookPathWithFallback(command, "/usr/sbin/sysctl", "/sbin/sysctl")
	default:
		return hostSupportLookPath(command)
	}
}
