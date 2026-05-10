//go:build darwin

package darwinvz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/policy"
)

type darwinVZLaunchObservability struct {
	RootFSCopyMS   int64
	HelperTimingMS map[string]int64
	Network        *darwinVZNetworkMetadata
	Recorded       bool
}

const (
	darwinVZTimingLaunchPolicyValidate            = "launch_policy_validate"
	darwinVZTimingKernelResolve                   = "kernel_resolve"
	darwinVZTimingRootFSResolve                   = "rootfs_resolve"
	darwinVZTimingRunDirPrepare                   = "run_dir_prepare"
	darwinVZTimingRootFSPreflight                 = "rootfs_preflight"
	darwinVZTimingRootFSBaseVolumePrepare         = "rootfs_base_volume_prepare"
	darwinVZTimingRootFSWritableVolumeCreateClone = "rootfs_writable_volume_create_clone"
	darwinVZTimingRootFSMinimumSizeResize         = "rootfs_minimum_size_resize"
	darwinVZTimingRootFSInspectValidate           = "rootfs_inspect_validate"
	darwinVZTimingGuestBootConfig                 = "guest_boot_config"
	darwinVZTimingHelperSessionStart              = "helper_session_start"
	darwinVZTimingCacheOutputPrepare              = "cache_output_prepare"
	darwinVZTimingHelperStartVM                   = "helper_start_vm"
	darwinVZTimingVirtualizationPIDLookup         = "virtualization_pid_lookup"
	darwinVZTimingGuestExecReadyProbe             = "guest_exec_ready_probe"
	darwinVZTimingLaunchSandboxTotal              = "launch_sandbox_total"
	darwinVZRootFSTimingExpectedPhaseCount        = 4
	darwinVZAdaptiveMemoryStartMiB                = 1024
)

type darwinVZNetworkMetadata struct {
	Mode       string
	SubnetCIDR string
	GuestIP    string
	GatewayIP  string
	PrefixLen  int
}

type darwinVZConfigFile struct {
	Backend                       string `json:"backend"`
	KernelImage                   string `json:"kernel_image"`
	RootFS                        string `json:"rootfs"`
	VCPUs                         int64  `json:"vcpus"`
	MemoryMiB                     int64  `json:"memory_mib"`
	InitialMemoryBalloonTargetMiB int64  `json:"initial_memory_balloon_target_mib,omitempty"`
	GuestPort                     uint32 `json:"guest_port"`
	LaunchSeconds                 int64  `json:"launch_secs"`
	NetworkMode                   string `json:"network_mode,omitempty"`
	NetworkSubnetCIDR             string `json:"network_subnet_cidr,omitempty"`
	NetworkGuestIP                string `json:"network_guest_ip,omitempty"`
	NetworkGatewayIP              string `json:"network_gateway_ip,omitempty"`
	NetworkPrefixLen              int    `json:"network_prefix_len,omitempty"`
	BootArgs                      string `json:"boot_args"`
}

type darwinVZVMStartRequest struct {
	SandboxID                     string
	ConfigPath                    string
	BackendName                   string
	RunDir                        string
	KernelPath                    string
	RootFSPath                    string
	SidecarDiskPaths              []string
	BootArgs                      string
	ConsoleLogPath                string
	NetworkCfg                    darwinVZNetwork
	HostGatewayURL                string
	GatewayPort                   int
	Policy                        *policy.CompiledPolicy
	VCPUs                         int64
	MemoryMiB                     int64
	InitialMemoryBalloonTargetMiB int64
	GuestPort                     uint32
	LaunchSeconds                 int64
}

type darwinVZVMStartResult struct {
	VMID            string
	ProxySocketPath string
	NetworkMetadata *darwinVZNetworkMetadata
	TimingMS        map[string]int64
	FileHandleGW    *fileHandleGateway
}

func startDarwinVZHelperVM(ctx context.Context, helper *helperSession, req darwinVZVMStartRequest) (*darwinVZVMStartResult, error) {
	if err := writeDarwinVZConfig(
		req.ConfigPath,
		req.BackendName,
		req.KernelPath,
		req.RootFSPath,
		req.BootArgs,
		req.VCPUs,
		req.MemoryMiB,
		req.InitialMemoryBalloonTargetMiB,
		req.GuestPort,
		req.LaunchSeconds,
		req.NetworkCfg,
		nil,
	); err != nil {
		return nil, err
	}

	proxySocketPath := filepath.Join(req.RunDir, helperProxySocketName)
	if err := ensureUnixSocketPathFits(proxySocketPath); err != nil {
		return nil, fmt.Errorf("proxy socket path %q is too long: %w", proxySocketPath, err)
	}
	_ = os.Remove(proxySocketPath)

	var fileHandleGW *fileHandleGateway
	if req.NetworkCfg.Mode == darwinVZNetworkModeFileHandle {
		gateway, err := startFileHandleGateway(ctx, fileHandleGatewayConfig{
			SandboxID:      req.SandboxID,
			RunDir:         req.RunDir,
			SubnetCIDR:     req.NetworkCfg.SubnetCIDR,
			GatewayPort:    req.GatewayPort,
			HostGatewayURL: req.HostGatewayURL,
			Policy:         req.Policy,
		})
		if err != nil {
			return nil, fmt.Errorf("start file-handle gateway: %w", err)
		}
		fileHandleGW = gateway
	}
	releaseFileHandleGW := fileHandleGW
	defer func() {
		if releaseFileHandleGW != nil {
			_ = releaseFileHandleGW.Close()
		}
	}()

	startCtx, cancelStart := context.WithTimeout(ctx, time.Duration(req.LaunchSeconds)*time.Second)
	defer cancelStart()
	startRes, err := helper.request(startCtx, helperControlRequest{
		Op:                            "StartVM",
		KernelPath:                    req.KernelPath,
		RootFSPath:                    req.RootFSPath,
		SidecarDiskPaths:              append([]string(nil), req.SidecarDiskPaths...),
		BootArgs:                      req.BootArgs,
		NetworkMode:                   req.NetworkCfg.Mode,
		VMNetSubnetCIDR:               req.NetworkCfg.SubnetCIDR,
		VCPUs:                         req.VCPUs,
		MemoryMiB:                     req.MemoryMiB,
		InitialMemoryBalloonTargetMiB: req.InitialMemoryBalloonTargetMiB,
		GuestPort:                     req.GuestPort,
		LaunchSeconds:                 req.LaunchSeconds,
		RunDir:                        req.RunDir,
		FileHandleSocketPath:          fileHandleGW.SocketPath(),
		ProxySocketPath:               proxySocketPath,
		ConsoleLogPath:                req.ConsoleLogPath,
	})
	if err != nil {
		return nil, fmt.Errorf("start vm via darwin-vz helper: %w", err)
	}

	networkMetadata := helperResponseNetworkMetadata(req.NetworkCfg.Mode, startRes)
	if err := writeDarwinVZConfig(
		req.ConfigPath,
		req.BackendName,
		req.KernelPath,
		req.RootFSPath,
		req.BootArgs,
		req.VCPUs,
		req.MemoryMiB,
		req.InitialMemoryBalloonTargetMiB,
		req.GuestPort,
		req.LaunchSeconds,
		req.NetworkCfg,
		networkMetadata,
	); err != nil {
		return nil, err
	}

	vmID := strings.TrimSpace(startRes.VMID)
	if vmID == "" {
		return nil, errors.New("darwin-vz helper returned empty vm_id")
	}
	if p := strings.TrimSpace(startRes.ProxySocketPath); p != "" {
		proxySocketPath = p
	}
	if strings.TrimSpace(proxySocketPath) == "" {
		return nil, errors.New("darwin-vz helper returned empty proxy socket path")
	}
	if err := ensureUnixSocketPathFits(proxySocketPath); err != nil {
		return nil, fmt.Errorf("proxy socket path %q is too long: %w", proxySocketPath, err)
	}
	releaseFileHandleGW = nil

	return &darwinVZVMStartResult{
		VMID:            vmID,
		ProxySocketPath: proxySocketPath,
		NetworkMetadata: networkMetadata,
		TimingMS:        cloneHelperTimingMS(startRes.TimingMS),
		FileHandleGW:    fileHandleGW,
	}, nil
}

func writeDarwinVZConfig(
	path, backendName, kernelPath, rootFSPath, bootArgs string,
	vcpus int64,
	memoryMiB int64,
	initialMemoryBalloonTargetMiB int64,
	guestPort uint32,
	launchSeconds int64,
	networkCfg darwinVZNetwork,
	networkMetadata *darwinVZNetworkMetadata,
) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	cfg := darwinVZConfigFile{
		Backend:                       backendName,
		KernelImage:                   kernelPath,
		RootFS:                        rootFSPath,
		VCPUs:                         vcpus,
		MemoryMiB:                     memoryMiB,
		InitialMemoryBalloonTargetMiB: initialMemoryBalloonTargetMiB,
		GuestPort:                     guestPort,
		LaunchSeconds:                 launchSeconds,
		NetworkMode:                   networkCfg.Mode,
		BootArgs:                      bootArgs,
	}
	if networkMetadata != nil {
		cfg.NetworkMode = networkMetadata.Mode
		cfg.NetworkSubnetCIDR = networkMetadata.SubnetCIDR
		cfg.NetworkGuestIP = networkMetadata.GuestIP
		cfg.NetworkGatewayIP = networkMetadata.GatewayIP
		cfg.NetworkPrefixLen = networkMetadata.PrefixLen
	} else if networkCfg.SubnetCIDR != "" {
		cfg.NetworkSubnetCIDR = networkCfg.SubnetCIDR
	}
	return writeJSON(path, cfg)
}

func writeDarwinVZRunObservation(runDir string, observation *darwinVZRunObservation, totalMS int64) error {
	if strings.TrimSpace(runDir) == "" || observation == nil {
		return nil
	}
	obsCopy := *observation
	obsCopy.TotalMS = totalMS
	if len(observation.HelperTimingMS) > 0 {
		obsCopy.HelperTimingMS = cloneHelperTimingMS(observation.HelperTimingMS)
	}
	return writeJSON(filepath.Join(runDir, runObservabilityFile), obsCopy)
}

func applyDarwinVZHelperTimings(observation *darwinVZRunObservation, timingMS map[string]int64) {
	if observation == nil || len(timingMS) == 0 {
		return
	}
	observation.HelperTimingMS = mergeHelperTimingMS(observation.HelperTimingMS, timingMS)
	if vmReadyMS, ok := timingMS["vm_ready"]; ok {
		observation.VMReadyMS = vmReadyMS
	}
}

func helperResponseNetworkMetadata(mode string, res helperControlResponse) *darwinVZNetworkMetadata {
	normalizedMode := strings.TrimSpace(mode)
	subnetCIDR := strings.TrimSpace(res.VMNetSubnetCIDR)
	guestIP := strings.TrimSpace(res.VMNetGuestIPv4)
	gatewayIP := strings.TrimSpace(res.VMNetGatewayIPv4)
	if normalizedMode == "" && subnetCIDR == "" && guestIP == "" && gatewayIP == "" && res.VMNetPrefixLen == 0 {
		return nil
	}
	return &darwinVZNetworkMetadata{
		Mode:       normalizedMode,
		SubnetCIDR: subnetCIDR,
		GuestIP:    guestIP,
		GatewayIP:  gatewayIP,
		PrefixLen:  res.VMNetPrefixLen,
	}
}

func applyDarwinVZNetworkMetadata(observation *darwinVZRunObservation, metadata *darwinVZNetworkMetadata) {
	if observation == nil || metadata == nil {
		return
	}
	observation.NetworkMode = metadata.Mode
	observation.NetworkSubnetCIDR = metadata.SubnetCIDR
	observation.NetworkGuestIP = metadata.GuestIP
	observation.NetworkGatewayIP = metadata.GatewayIP
	observation.NetworkPrefixLen = metadata.PrefixLen
}

func applyDarwinVZLaunchObservability(observation *darwinVZRunObservation, launch *darwinVZLaunchObservability) {
	if observation == nil || launch == nil {
		return
	}
	if launch.RootFSCopyMS > 0 {
		observation.RootFSCopyMS = launch.RootFSCopyMS
	}
	applyDarwinVZNetworkMetadata(observation, launch.Network)
	applyDarwinVZHelperTimings(observation, launch.HelperTimingMS)
	if launch.RootFSCopyMS > 0 || len(launch.HelperTimingMS) > 0 || launch.Network != nil {
		observation.LaunchedVM = true
	}
}

func cloneHelperTimingMS(timingMS map[string]int64) map[string]int64 {
	if len(timingMS) == 0 {
		return nil
	}
	out := make(map[string]int64, len(timingMS))
	for key, value := range timingMS {
		out[key] = value
	}
	return out
}

func mergeHelperTimingMS(dst, src map[string]int64) map[string]int64 {
	if len(src) == 0 {
		return cloneHelperTimingMS(dst)
	}
	out := make(map[string]int64, len(dst)+len(src))
	for key, value := range dst {
		out[key] = value
	}
	for key, value := range src {
		out[key] = value
	}
	return out
}

func recordDarwinVZPhaseTiming(timingMS map[string]int64, phase string, start time.Time) {
	if timingMS == nil {
		return
	}
	timingMS[phase] = time.Since(start).Milliseconds()
}

func recordDarwinVZPhaseTimingDuration(timingMS map[string]int64, phase string, duration time.Duration) {
	if timingMS == nil {
		return
	}
	timingMS[phase] = duration.Milliseconds()
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
