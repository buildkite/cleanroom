//go:build darwin

package darwinvz

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/bootassets"
	"github.com/buildkite/cleanroom/internal/ext4edit"
	"github.com/buildkite/cleanroom/internal/ext4image"
	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/hosttools"
	"github.com/buildkite/cleanroom/internal/imagemgr"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/volumestore"
	"github.com/buildkite/cleanroom/internal/vsockexec"
	"github.com/charmbracelet/log"
	"golang.org/x/sys/unix"
)

// Adapter runs single-execution Linux VMs on macOS via Virtualization.framework.
//
// Current scope intentionally excludes host/port egress allow rules. Policies
// with allow entries still run, but those entries are ignored.
type Adapter struct {
	imageManagerOnce sync.Once
	imageManager     imageEnsurer
	imageManagerErr  error
	newImageManager  imageManagerFactory

	guestAgentOnce sync.Once
	guestAgentPath string
	guestAgentHash string
	guestAgentErr  error

	runtimeImageMu sync.Mutex

	sandboxMu          sync.Mutex
	sandboxes          map[string]*sandboxInstance
	provisioning       map[string]struct{}
	launchSandboxVMFn  func(context.Context, string, *policy.CompiledPolicy, backend.FirecrackerConfig) (*sandboxInstance, error)
	executeInSandboxFn func(context.Context, context.Context, *sandboxInstance, backend.ExecutionRequest, backend.OutputStream) (*backend.ExecutionResult, error)
	helperRequestFn    func(context.Context, *helperSession, helperControlRequest) (helperControlResponse, error)

	ensurePreparedRootFSFn func(context.Context, string) (preparedRootFS, error)

	GatewayRegistry gatewayRegistry
	GatewayPort     int
	GatewayHost     string
}

type imageEnsurer interface {
	Ensure(context.Context, string) (imagemgr.EnsureResult, error)
}

type imageManagerFactory func() (imageEnsurer, error)

type preparedRootFS struct {
	Ref    string
	Digest string
	Path   string
	Hit    bool
}

type sandboxInstance struct {
	SandboxID           string
	RunDir              string
	ConfigPath          string
	ProxySocketPath     string
	GuestPort           uint32
	Policy              *policy.CompiledPolicy
	ImageRef            string
	ImageDigest         string
	LaunchObservability *darwinVZLaunchObservability
	NetworkMetadata     *darwinVZNetworkMetadata
	CommandTimeout      int64
	Helper              *helperSession
	VMID                string
	vmRootFSPath        string
	exitedCh            chan struct{}
	exitMu              sync.RWMutex
	exitErr             error
	exitReady           bool
}

const preparedRuntimeRootFSVersion = "v9-darwin-vz"
const runObservabilityFile = "execution-observability.json"

type darwinVZRunObservation struct {
	ExecutionID       string           `json:"execution_id"`
	Backend           string           `json:"backend"`
	LaunchedVM        bool             `json:"launched_vm"`
	ImageRef          string           `json:"image_ref,omitempty"`
	ImageDigest       string           `json:"image_digest,omitempty"`
	PlanPath          string           `json:"plan_path,omitempty"`
	RunDir            string           `json:"run_dir,omitempty"`
	ExitCode          int              `json:"exit_code,omitempty"`
	Error             string           `json:"error,omitempty"`
	GuestError        string           `json:"guest_error,omitempty"`
	NetworkMode       string           `json:"network_mode,omitempty"`
	NetworkSubnetCIDR string           `json:"network_subnet_cidr,omitempty"`
	NetworkGuestIP    string           `json:"network_guest_ip,omitempty"`
	NetworkGatewayIP  string           `json:"network_gateway_ip,omitempty"`
	NetworkPrefixLen  int              `json:"network_prefix_len,omitempty"`
	RootFSCopyMS      int64            `json:"rootfs_copy_ms,omitempty"`
	VMReadyMS         int64            `json:"vm_ready_ms,omitempty"`
	HelperTimingMS    map[string]int64 `json:"helper_timing_ms,omitempty"`
	TotalMS           int64            `json:"total_ms,omitempty"`
}

func New() *Adapter {
	return &Adapter{
		newImageManager: defaultImageManagerFactory,
	}
}

func defaultImageManagerFactory() (imageEnsurer, error) {
	return imagemgr.New(imagemgr.Options{})
}

func (a *Adapter) Name() string {
	return "darwin-vz"
}

func (a *Adapter) Capabilities() map[string]bool {
	return map[string]bool{
		backend.CapabilityNetworkDefaultDeny:     true,
		backend.CapabilityNetworkAllowlistEgress: false,
		backend.CapabilityNetworkGuestInterface:  true,
	}
}

func (a *Adapter) Run(ctx context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
	return a.run(ctx, req, backend.OutputStream{})
}

func (a *Adapter) RunStream(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
	return a.run(ctx, req, stream)
}

func (a *Adapter) ProvisionSandbox(ctx context.Context, req backend.ProvisionRequest) error {
	sandboxID := strings.TrimSpace(req.SandboxID)
	if sandboxID == "" {
		return errors.New("missing sandbox_id")
	}
	if req.Policy == nil {
		return errors.New("missing compiled policy")
	}
	return a.provisionSandbox(ctx, sandboxID, req.Policy, req.FirecrackerConfig)
}

func (a *Adapter) RunInSandbox(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
	sandboxID := strings.TrimSpace(req.SandboxID)
	if sandboxID == "" {
		return nil, errors.New("missing sandbox_id")
	}
	if len(req.Command) == 0 {
		return nil, errors.New("missing command")
	}
	if strings.TrimSpace(req.ExecutionID) == "" {
		return nil, errors.New("missing execution_id")
	}

	a.sandboxMu.Lock()
	instance, ok := a.sandboxes[sandboxID]
	a.sandboxMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown sandbox %q", sandboxID)
	}
	if err := instance.exitedErrOrNil(); err != nil {
		return nil, fmt.Errorf("sandbox %q is not running: %w", sandboxID, err)
	}

	if req.Policy == nil {
		req.Policy = instance.Policy
	}
	if req.Policy == nil {
		return nil, errors.New("missing compiled policy")
	}
	if _, policyErr := evaluateNetworkPolicy(req.Policy.NetworkDefault, len(req.Policy.Allow)); policyErr != nil {
		return nil, policyErr
	}

	runDir := strings.TrimSpace(req.RunDir)
	if runDir == "" {
		if baseDir, err := paths.ExecutionBaseDir(); err == nil {
			runDir = filepath.Join(baseDir, req.ExecutionID)
		}
	}
	if runDir != "" {
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			return nil, fmt.Errorf("create run directory: %w", err)
		}
	}
	req.RunDir = runDir
	observation := darwinVZRunObservation{
		ExecutionID: req.ExecutionID,
		Backend:     a.Name(),
		RunDir:      runDir,
		ImageRef:    instance.ImageRef,
		ImageDigest: instance.ImageDigest,
		PlanPath:    instance.ConfigPath,
	}
	applyDarwinVZNetworkMetadata(&observation, instance.NetworkMetadata)
	a.sandboxMu.Lock()
	if current, ok := a.sandboxes[sandboxID]; ok && current == instance {
		applyDarwinVZLaunchObservability(&observation, current.LaunchObservability)
		current.LaunchObservability = nil
	}
	a.sandboxMu.Unlock()
	runStart := time.Now()
	defer func() {
		if err := writeDarwinVZRunObservation(runDir, &observation, time.Since(runStart).Milliseconds()); err != nil {
			log.Warn("write darwin-vz run observability failed", "execution_id", req.ExecutionID, "error", err)
		}
	}()

	connectSeconds := req.LaunchSeconds
	if connectSeconds <= 0 {
		connectSeconds = instance.CommandTimeout
	}
	if connectSeconds <= 0 {
		connectSeconds = 30
	}
	bootCtx, cancel := context.WithTimeout(ctx, time.Duration(connectSeconds)*time.Second)
	defer cancel()

	executeInSandbox := a.executeInSandboxFn
	if executeInSandbox == nil {
		executeInSandbox = a.executeInSandbox
	}
	result, err := executeInSandbox(bootCtx, ctx, instance, req, stream)
	if err != nil {
		observation.Error = err.Error()
		return nil, err
	}
	if result != nil {
		observation.ExitCode = result.ExitCode
		observation.GuestError = result.Message
		if strings.TrimSpace(result.PlanPath) != "" {
			observation.PlanPath = result.PlanPath
		}
		if strings.TrimSpace(result.ImageRef) != "" {
			observation.ImageRef = result.ImageRef
		}
		if strings.TrimSpace(result.ImageDigest) != "" {
			observation.ImageDigest = result.ImageDigest
		}
	}
	return result, nil
}

func (a *Adapter) TerminateSandbox(_ context.Context, sandboxID string) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return errors.New("missing sandbox_id")
	}

	a.sandboxMu.Lock()
	instance, ok := a.sandboxes[sandboxID]
	if ok {
		delete(a.sandboxes, sandboxID)
	}
	a.sandboxMu.Unlock()
	if !ok {
		return nil
	}

	instance.shutdown()
	return nil
}

func (a *Adapter) CreateSnapshot(ctx context.Context, req backend.SnapshotRequest) (result *backend.SnapshotResult, retErr error) {
	sandboxID := strings.TrimSpace(req.SandboxID)
	if sandboxID == "" {
		return nil, errors.New("missing sandbox_id")
	}
	snapshotID := strings.TrimSpace(req.SnapshotID)
	if snapshotID == "" {
		return nil, errors.New("missing snapshot_id")
	}

	a.sandboxMu.Lock()
	instance, ok := a.sandboxes[sandboxID]
	a.sandboxMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown sandbox %q", sandboxID)
	}
	if err := instance.exitedErrOrNil(); err != nil {
		return nil, fmt.Errorf("sandbox %q is not running: %w", sandboxID, err)
	}

	executeInSandbox := a.executeInSandboxFn
	if executeInSandbox == nil {
		executeInSandbox = a.executeInSandbox
	}

	connectSeconds := req.FirecrackerConfig.LaunchSeconds
	if connectSeconds <= 0 {
		connectSeconds = instance.CommandTimeout
	}
	if connectSeconds <= 0 {
		connectSeconds = 30
	}
	syncCtx, cancel := context.WithTimeout(ctx, time.Duration(connectSeconds)*time.Second)
	defer cancel()
	if _, err := executeInSandbox(syncCtx, ctx, instance, backend.ExecutionRequest{
		SandboxID: sandboxID,
		Command:   []string{"sync"},
		Policy:    instance.Policy,
	}, backend.OutputStream{}); err != nil {
		return nil, fmt.Errorf("sync sandbox filesystem before snapshot: %w", err)
	}

	driver, err := snapshotVolumeDriver(req.FirecrackerConfig)
	if err != nil {
		return nil, err
	}
	helperRequest := a.helperRequestFn
	if helperRequest == nil {
		helperRequest = func(ctx context.Context, helper *helperSession, req helperControlRequest) (helperControlResponse, error) {
			return helper.request(ctx, req)
		}
	}
	if instance.Helper == nil {
		return nil, errors.New("darwin-vz sandbox helper is not available")
	}
	if strings.TrimSpace(instance.VMID) == "" {
		return nil, errors.New("darwin-vz sandbox vm id is empty")
	}
	if _, err := helperRequest(ctx, instance.Helper, helperControlRequest{Op: "PauseVM", VMID: instance.VMID}); err != nil {
		return nil, fmt.Errorf("pause darwin-vz sandbox: %w", err)
	}
	paused := true
	defer func() {
		if !paused {
			return
		}
		resumeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := helperRequest(resumeCtx, instance.Helper, helperControlRequest{Op: "ResumeVM", VMID: instance.VMID}); err != nil && retErr == nil {
			result = nil
			retErr = fmt.Errorf("resume darwin-vz sandbox after snapshot: %w", err)
		}
	}()

	snapshot, err := driver.SnapshotVolume(ctx, volumestore.SnapshotVolumeRequest{
		SnapshotID: snapshotID,
		VolumeRef:  instance.vmRootFSPath,
	})
	if err != nil {
		return nil, fmt.Errorf("persist snapshot rootfs: %w", err)
	}

	return &backend.SnapshotResult{StorageRef: snapshot.StorageRef}, nil
}

func (a *Adapter) ProvisionSandboxFromSnapshot(ctx context.Context, req backend.ProvisionFromSnapshotRequest) error {
	sandboxID := strings.TrimSpace(req.SandboxID)
	if sandboxID == "" {
		return errors.New("missing sandbox_id")
	}
	if req.Policy == nil {
		return errors.New("missing compiled policy")
	}
	storageRef := strings.TrimSpace(req.StorageRef)
	if storageRef == "" {
		return errors.New("missing snapshot storage_ref")
	}
	if _, err := os.Stat(storageRef); err != nil {
		return fmt.Errorf("snapshot rootfs %q: %w", storageRef, err)
	}

	cfg := req.FirecrackerConfig
	cfg.RootFSPath = storageRef
	return a.provisionSandbox(ctx, sandboxID, req.Policy, cfg)
}

func (a *Adapter) DeleteSnapshot(ctx context.Context, req backend.DeleteSnapshotRequest) error {
	storageRef := strings.TrimSpace(req.StorageRef)
	if storageRef == "" {
		return errors.New("missing snapshot storage_ref")
	}
	driver, err := snapshotVolumeDriver(req.FirecrackerConfig)
	if err != nil {
		return err
	}
	if err := driver.DestroySnapshot(ctx, volumestore.DestroySnapshotRequest{
		SnapshotRef: storageRef,
	}); err != nil {
		return fmt.Errorf("remove snapshot rootfs %q: %w", storageRef, err)
	}
	return nil
}

func (a *Adapter) Doctor(_ context.Context, req backend.DoctorRequest) (*backend.DoctorReport, error) {
	report := &backend.DoctorReport{Backend: a.Name()}
	appendCheck := func(name, status, message string) {
		report.Checks = append(report.Checks, backend.DoctorCheck{Name: name, Status: status, Message: message})
	}

	if runtime.GOOS == "darwin" {
		appendCheck("os", "pass", "darwin host detected")
	} else {
		appendCheck("os", "fail", fmt.Sprintf("darwin required, current OS is %s", runtime.GOOS))
	}
	appendCheck("guest_networking", "warn", guestNetworkUnavailableWarning)
	networkCfg, networkErr := resolveDarwinVZNetwork(req.FirecrackerConfig)
	if networkErr != nil {
		appendCheck("network_mode", "fail", networkErr.Error())
	} else {
		appendCheck("network_mode", "pass", fmt.Sprintf("darwin-vz network mode: %s", networkCfg.Mode))
		if networkCfg.SubnetCIDR != "" {
			appendCheck("network_subnet", "pass", fmt.Sprintf("darwin-vz vmnet subnet: %s", networkCfg.SubnetCIDR))
		}
	}

	if configured := strings.TrimSpace(req.KernelImagePath); configured == "" {
		if spec, ok := bootassets.LookupManagedKernelForHost(a.Name()); ok {
			path, _ := bootassets.ManagedKernelPathForHost(a.Name())
			appendCheck("kernel_image", "pass", fmt.Sprintf("kernel image will be auto-managed (%s -> %s)", spec.ID, path))
		} else {
			appendCheck("kernel_image", "fail", "kernel image must be configured")
		}
	} else if _, err := os.Stat(configured); err != nil {
		if spec, ok := bootassets.LookupManagedKernelForHost(a.Name()); ok {
			path, _ := bootassets.ManagedKernelPathForHost(a.Name())
			appendCheck("kernel_image", "warn", fmt.Sprintf("configured kernel image is not accessible (%v); runtime will use managed kernel (%s -> %s)", err, spec.ID, path))
		} else {
			appendCheck("kernel_image", "fail", fmt.Sprintf("kernel image not accessible: %v", err))
		}
	} else {
		appendCheck("kernel_image", "pass", fmt.Sprintf("kernel image configured: %s", configured))
	}

	if strings.TrimSpace(req.RootFSPath) == "" {
		if req.Policy != nil && strings.TrimSpace(req.Policy.ImageRef) != "" {
			appendCheck("rootfs", "pass", "rootfs will be derived from sandbox.image.ref")
		} else {
			appendCheck("rootfs", "warn", "rootfs is not configured; provide sandbox.image.ref for automatic OCI rootfs derivation")
		}
	} else if _, err := os.Stat(req.RootFSPath); err != nil {
		if req.Policy != nil && strings.TrimSpace(req.Policy.ImageRef) != "" {
			appendCheck("rootfs", "warn", fmt.Sprintf("configured rootfs is not accessible (%v); runtime will derive rootfs from sandbox.image.ref", err))
		} else {
			appendCheck("rootfs", "fail", fmt.Sprintf("rootfs not accessible: %v", err))
		}
	} else {
		appendCheck("rootfs", "pass", fmt.Sprintf("rootfs configured: %s", req.RootFSPath))
	}

	if req.Policy == nil {
		appendCheck("policy", "warn", "policy not loaded")
	} else {
		policyWarn, policyErr := evaluateNetworkPolicy(req.Policy.NetworkDefault, len(req.Policy.Allow))
		if policyErr != nil {
			appendCheck("policy_network_default", "fail", policyErr.Error())
		} else {
			appendCheck("policy_network_default", "pass", "deny-by-default policy")
			if policyWarn != "" {
				appendCheck("policy_network_allow", "warn", policyWarn)
			} else {
				appendCheck("policy_network_allow", "pass", "allow list empty")
			}
			if strings.TrimSpace(req.Policy.ImageRef) == "" {
				appendCheck("sandbox_image_ref", "fail", "sandbox.image.ref is required when rootfs is not configured")
			} else {
				appendCheck("sandbox_image_ref", "pass", fmt.Sprintf("sandbox image ref configured: %s", req.Policy.ImageRef))
			}
		}
	}

	requiresDerivedRootFS := true
	if configuredRootFS := strings.TrimSpace(req.RootFSPath); configuredRootFS != "" {
		if _, err := os.Stat(configuredRootFS); err == nil {
			requiresDerivedRootFS = false
		}
	}
	mkfsMissingStatus := "warn"
	debugfsMissingStatus := "warn"
	if requiresDerivedRootFS {
		mkfsMissingStatus = "fail"
		debugfsMissingStatus = "fail"
	}

	if mkfsPath, err := hosttools.ResolveE2FSProgsBinary("mkfs.ext4"); err != nil {
		appendCheck("mkfs_ext4", mkfsMissingStatus, fmt.Sprintf("mkfs.ext4 not available: %v", err))
	} else {
		appendCheck("mkfs_ext4", "pass", fmt.Sprintf("found mkfs.ext4 (%s) for OCI rootfs materialisation", mkfsPath))
	}

	if debugfsPath, err := hosttools.ResolveE2FSProgsBinary("debugfs"); err != nil {
		appendCheck("debugfs", debugfsMissingStatus, fmt.Sprintf("debugfs not available: %v", err))
	} else {
		appendCheck("debugfs", "pass", fmt.Sprintf("found debugfs (%s) for runtime rootfs preparation", debugfsPath))
	}

	if guestAgentPath, err := discoverGuestAgentBinary(); err != nil {
		appendCheck("guest_agent_binary", "fail", err.Error())
	} else {
		appendCheck("guest_agent_binary", "pass", fmt.Sprintf("linux guest-agent resolved at %s", guestAgentPath))
	}

	if helperPath, err := resolveHelperBinaryPath(); err != nil {
		appendCheck("helper_binary", "fail", err.Error())
	} else {
		appendCheck("helper_binary", "pass", fmt.Sprintf("darwin-vz helper resolved at %s", helperPath))
		hasEntitlement, entitlementErr := helperHasVirtualizationEntitlement(helperPath)
		switch {
		case entitlementErr != nil:
			appendCheck(
				"vm_entitlement",
				"warn",
				fmt.Sprintf(
					"could not verify com.apple.security.virtualization entitlement on %s: %v",
					helperPath,
					entitlementErr,
				),
			)
		case !hasEntitlement:
			appendCheck(
				"vm_entitlement",
				"fail",
				fmt.Sprintf(
					"%s is missing com.apple.security.virtualization entitlement; run `mise run install` to install and sign the helper",
					helperPath,
				),
			)
		default:
			appendCheck(
				"vm_entitlement",
				"pass",
				fmt.Sprintf("%s includes com.apple.security.virtualization entitlement", helperPath),
			)
		}
		if networkErr == nil && networkCfg.Mode == darwinVZNetworkModeVMNetShared {
			hasVMNetEntitlement, vmnetEntitlementErr := helperHasVMNetworkingEntitlement(helperPath)
			status, message := doctorVMNetEntitlementResult(helperPath, hasVMNetEntitlement, vmnetEntitlementErr)
			appendCheck("vmnet_entitlement", status, message)
		}
	}
	return report, nil
}

func (a *Adapter) run(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (result *backend.ExecutionResult, err error) {
	runStart := time.Now()

	if req.Policy == nil {
		return nil, errors.New("missing compiled policy")
	}
	policyWarn, policyErr := evaluateNetworkPolicy(req.Policy.NetworkDefault, len(req.Policy.Allow))
	if policyErr != nil {
		return nil, policyErr
	}
	if len(req.Command) == 0 {
		return nil, errors.New("missing command")
	}
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("darwin-vz backend is darwin-only, current OS is %s", runtime.GOOS)
	}

	runDir := req.RunDir
	if runDir == "" {
		baseDir, err := paths.ExecutionBaseDir()
		if err != nil {
			return nil, fmt.Errorf("resolve run base directory: %w", err)
		}
		runDir = filepath.Join(baseDir, req.ExecutionID)
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, fmt.Errorf("create run directory: %w", err)
	}
	observation := darwinVZRunObservation{
		ExecutionID: req.ExecutionID,
		Backend:     a.Name(),
		RunDir:      runDir,
		ImageRef:    req.Policy.ImageRef,
		ImageDigest: req.Policy.ImageDigest,
	}
	defer func() {
		if err != nil && strings.TrimSpace(observation.Error) == "" {
			observation.Error = err.Error()
		}
		if err := writeDarwinVZRunObservation(runDir, &observation, time.Since(runStart).Milliseconds()); err != nil {
			log.Warn("write darwin-vz run observability failed", "execution_id", req.ExecutionID, "error", err)
		}
	}()

	if req.VCPUs <= 0 {
		req.VCPUs = 1
	}
	if req.MemoryMiB <= 0 {
		req.MemoryMiB = 512
	}
	if req.GuestPort == 0 {
		req.GuestPort = vsockexec.DefaultPort
	}
	if req.LaunchSeconds <= 0 {
		req.LaunchSeconds = 30
	}

	cmdPath := filepath.Join(runDir, "requested-command.json")
	if err := writeJSON(cmdPath, req.Command); err != nil {
		return nil, err
	}

	warnings := buildRuntimeWarnings(policyWarn)

	stderrPrefix := ""
	for _, warningText := range warnings {
		stderrPrefix += emitExecutionWarning(stream, warningText)
	}

	resolvedImageRef := req.Policy.ImageRef
	resolvedImageDigest := req.Policy.ImageDigest

	if !req.Launch {
		planPath := filepath.Join(runDir, "plan.json")
		plan := map[string]any{
			"backend":      a.Name(),
			"mode":         "plan-only",
			"command_path": cmdPath,
		}
		if err := writeJSON(planPath, plan); err != nil {
			observation.Error = err.Error()
			return nil, err
		}
		observation.PlanPath = planPath
		return &backend.ExecutionResult{
			ExecutionID: req.ExecutionID,
			ExitCode:    0,
			LaunchedVM:  false,
			PlanPath:    planPath,
			RunDir:      runDir,
			ImageRef:    resolvedImageRef,
			ImageDigest: resolvedImageDigest,
			Message:     "darwin-vz execution plan generated; command not executed",
			Stderr:      stderrPrefix,
		}, nil
	}

	kernelPath, kernelNotice, err := a.resolveKernelPath(ctx, req.KernelImagePath)
	if err != nil {
		observation.Error = err.Error()
		return nil, err
	}
	logExecutionNotice(a.Name(), req.ExecutionID, kernelNotice)

	rootFSPath, imageRef, imageDigest, rootFSNotice, err := a.resolveRootFSPath(ctx, req)
	if err != nil {
		observation.Error = err.Error()
		return nil, err
	}
	logExecutionNotice(a.Name(), req.ExecutionID, rootFSNotice)
	if strings.TrimSpace(imageRef) != "" {
		resolvedImageRef = imageRef
	}
	if strings.TrimSpace(imageDigest) != "" {
		resolvedImageDigest = imageDigest
	}
	observation.ImageRef = resolvedImageRef
	observation.ImageDigest = resolvedImageDigest

	rootFSPath, err = filepath.Abs(rootFSPath)
	if err != nil {
		observation.Error = err.Error()
		return nil, fmt.Errorf("resolve rootfs path: %w", err)
	}
	if _, err := os.Stat(rootFSPath); err != nil {
		observation.Error = err.Error()
		return nil, fmt.Errorf("rootfs %s: %w", rootFSPath, err)
	}

	driver, err := rootFSVolumeDriver(req.FirecrackerConfig)
	if err != nil {
		return nil, err
	}
	baseVolume, err := driver.EnsureBaseVolume(ctx, volumestore.EnsureBaseVolumeRequest{
		BaseID:     strings.TrimSuffix(filepath.Base(rootFSPath), filepath.Ext(rootFSPath)),
		SourcePath: rootFSPath,
	})
	if err != nil {
		return nil, fmt.Errorf("prepare base volume: %w", err)
	}
	vmRootFSPath := filepath.Join(runDir, "rootfs-ephemeral.ext4")
	copyStart := time.Now()
	writableVolume, err := driver.CreateWritableVolume(ctx, volumestore.CreateWritableVolumeRequest{
		VolumeID:       req.ExecutionID,
		BaseRef:        baseVolume.Ref,
		AttachmentPath: vmRootFSPath,
	})
	if err != nil {
		observation.Error = err.Error()
		return nil, fmt.Errorf("prepare per-run rootfs: %w", err)
	}
	vmRootFSPath = writableVolume.AttachmentPath
	if err := ext4image.EnsureMinimumSize(ctx, vmRootFSPath, req.MinimumRootFSBytes); err != nil {
		observation.Error = err.Error()
		return nil, fmt.Errorf("resize writable rootfs: %w", err)
	}
	observation.RootFSCopyMS = time.Since(copyStart).Milliseconds()
	defer func() {
		_ = driver.DestroyVolume(context.Background(), volumestore.DestroyVolumeRequest{VolumeRef: vmRootFSPath})
	}()

	guestInitPath, guestInitNotice := guestInitExecutableForRootFS(vmRootFSPath)
	logExecutionNotice(a.Name(), req.ExecutionID, guestInitNotice)
	networkCfg, err := resolveDarwinVZNetwork(req.FirecrackerConfig)
	if err != nil {
		observation.Error = err.Error()
		return nil, err
	}
	bootArgs := fmt.Sprintf(
		"console=hvc0 root=/dev/vda rw init=%s cleanroom_guest_port=%d %s",
		guestInitPath,
		req.GuestPort,
		dockerServiceBootArgs(req.Policy, req.FirecrackerConfig),
	)
	consolePath := filepath.Join(runDir, "vm.console.log")

	vmPlanPath := filepath.Join(runDir, "darwin-vz-config.json")
	observation.PlanPath = vmPlanPath

	helper, err := startHelperSession(ctx, runDir, req.LaunchSeconds)
	if err != nil {
		observation.Error = err.Error()
		return nil, fmt.Errorf("start darwin-vz helper: %w", err)
	}
	defer func() {
		if closeErr := helper.close(); closeErr != nil && stream.OnStderr != nil {
			stream.OnStderr([]byte("warning: failed to close darwin-vz helper: " + closeErr.Error() + "\n"))
		}
	}()

	startedVM, err := startDarwinVZHelperVM(ctx, helper, darwinVZVMStartRequest{
		ConfigPath:     vmPlanPath,
		BackendName:    a.Name(),
		RunDir:         runDir,
		KernelPath:     kernelPath,
		RootFSPath:     vmRootFSPath,
		BootArgs:       bootArgs,
		ConsoleLogPath: consolePath,
		NetworkCfg:     networkCfg,
		VCPUs:          req.VCPUs,
		MemoryMiB:      req.MemoryMiB,
		GuestPort:      req.GuestPort,
		LaunchSeconds:  req.LaunchSeconds,
	})
	if err != nil {
		observation.Error = err.Error()
		return nil, err
	}
	observation.LaunchedVM = true
	applyDarwinVZHelperTimings(&observation, startedVM.TimingMS)
	applyDarwinVZNetworkMetadata(&observation, startedVM.NetworkMetadata)
	vmID := startedVM.VMID
	proxySocketPath := startedVM.ProxySocketPath
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, stopErr := helper.request(stopCtx, helperControlRequest{Op: "StopVM", VMID: vmID}); stopErr != nil && stream.OnStderr != nil {
			stream.OnStderr([]byte("warning: failed to stop darwin-vz vm: " + stopErr.Error() + "\n"))
		}
	}()

	connCtx, cancelConn := context.WithTimeout(ctx, time.Duration(req.LaunchSeconds)*time.Second)
	defer cancelConn()
	conn, err := dialUnixSocketWithRetry(connCtx, proxySocketPath)
	if err != nil {
		observation.Error = err.Error()
		return nil, fmt.Errorf("connect darwin-vz proxy socket %q: %w", proxySocketPath, err)
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return nil, fmt.Errorf("set proxy socket deadline: %w", err)
		}
	}
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	gatewayScopeToken := ""
	if a.GatewayRegistry != nil {
		token, tokenErr := randomScopeToken()
		if tokenErr != nil {
			return nil, fmt.Errorf("generate gateway scope token: %w", tokenErr)
		}
		scopeSandboxID := strings.TrimSpace(req.SandboxID)
		if scopeSandboxID == "" {
			scopeSandboxID = strings.TrimSpace(req.ExecutionID)
		}
		if scopeSandboxID == "" {
			scopeSandboxID = vmID
		}
		if err := a.GatewayRegistry.RegisterScopeToken(token, scopeSandboxID, req.Policy); err != nil {
			return nil, fmt.Errorf("register sandbox in gateway: %w", err)
		}
		gatewayScopeToken = token
		defer a.GatewayRegistry.ReleaseScopeToken(gatewayScopeToken)
	}

	guestReq := vsockexec.ExecRequest{
		Command: append([]string(nil), req.Command...),
		Env:     append([]string(nil), req.Env...),
		TTY:     req.TTY,
	}
	if a.GatewayRegistry != nil && gatewayScopeToken != "" {
		gwPort := a.GatewayPort
		if gwPort <= 0 {
			gwPort = gateway.DefaultPort
		}
		gwHost := strings.TrimSpace(a.GatewayHost)
		if gwHost == "" {
			gwHost = defaultGatewayHost
		}
		guestReq.Env = append(guestReq.Env, gatewayEnvVars(req.Policy, gwHost, gwPort, gatewayScopeToken)...)
	}
	seed := make([]byte, 64)
	if _, err := cryptorand.Read(seed); err == nil {
		guestReq.EntropySeed = seed
	}
	if err := vsockexec.EncodeRequest(conn, guestReq); err != nil {
		observation.Error = err.Error()
		return nil, fmt.Errorf("send guest exec request: %w", err)
	}

	inputSender := &inputFrameSender{w: conn}
	if stream.OnAttach != nil {
		stream.OnAttach(backend.AttachIO{
			WriteStdin: func(data []byte) error {
				return inputSender.Send(vsockexec.ExecInputFrame{Type: "stdin", Data: data})
			},
			CloseStdin: func() error {
				return inputSender.Send(vsockexec.ExecInputFrame{Type: "eof"})
			},
			ResizeTTY: func(cols, rows uint32) error {
				return inputSender.Send(vsockexec.ExecInputFrame{Type: "resize", Cols: cols, Rows: rows})
			},
		})
	}

	guestRes, err := vsockexec.DecodeStreamResponse(conn, vsockexec.StreamCallbacks{
		OnStdout: stream.OnStdout,
		OnStderr: stream.OnStderr,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			observation.Error = ctxErr.Error()
			return nil, fmt.Errorf("guest exec canceled while waiting for response: %w", ctxErr)
		}
		observation.Error = err.Error()
		return nil, helper.decorateError(fmt.Errorf("decode guest exec response over darwin-vz proxy: %w", err))
	}

	message := darwinVZResultMessage(guestRes.Error)
	if timingSummary := darwinVZTimingSummary(startedVM.TimingMS); timingSummary != "" {
		if message != "" {
			message += "; "
		}
		message += timingSummary
	}
	observation.ExitCode = guestRes.ExitCode
	observation.GuestError = guestRes.Error

	return &backend.ExecutionResult{
		ExecutionID: req.ExecutionID,
		ExitCode:    guestRes.ExitCode,
		LaunchedVM:  true,
		PlanPath:    vmPlanPath,
		RunDir:      runDir,
		ImageRef:    resolvedImageRef,
		ImageDigest: resolvedImageDigest,
		Message:     message,
		Stdout:      guestRes.Stdout,
		Stderr:      stderrPrefix + guestRes.Stderr,
	}, nil
}

func (a *Adapter) launchSandboxVM(ctx context.Context, sandboxID string, compiled *policy.CompiledPolicy, cfg backend.FirecrackerConfig) (*sandboxInstance, error) {
	if compiled == nil {
		return nil, errors.New("missing compiled policy")
	}
	if _, policyErr := evaluateNetworkPolicy(compiled.NetworkDefault, len(compiled.Allow)); policyErr != nil {
		return nil, policyErr
	}
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("darwin-vz backend is darwin-only, current OS is %s", runtime.GOOS)
	}

	if cfg.VCPUs <= 0 {
		cfg.VCPUs = 1
	}
	if cfg.MemoryMiB <= 0 {
		cfg.MemoryMiB = 512
	}
	if cfg.GuestPort == 0 {
		cfg.GuestPort = vsockexec.DefaultPort
	}
	if cfg.LaunchSeconds <= 0 {
		cfg.LaunchSeconds = 30
	}

	kernelPath, _, err := a.resolveKernelPath(ctx, cfg.KernelImagePath)
	if err != nil {
		return nil, err
	}
	rootFSPath, imageRef, imageDigest, _, err := a.resolveRootFSPath(ctx, backend.ExecutionRequest{
		Policy:            compiled,
		FirecrackerConfig: cfg,
	})
	if err != nil {
		return nil, err
	}

	runBaseDir, err := sandboxRuntimeBaseDir()
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox runtime base directory: %w", err)
	}
	runDir := filepath.Join(runBaseDir, sandboxID)
	cleanupRunDir := true
	defer func() {
		if cleanupRunDir {
			_ = os.RemoveAll(runDir)
		}
	}()
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, fmt.Errorf("create sandbox runtime directory: %w", err)
	}

	rootFSPath, err = filepath.Abs(rootFSPath)
	if err != nil {
		return nil, fmt.Errorf("resolve rootfs path: %w", err)
	}
	if _, err := os.Stat(rootFSPath); err != nil {
		return nil, fmt.Errorf("rootfs %s: %w", rootFSPath, err)
	}

	driver, err := rootFSVolumeDriver(cfg)
	if err != nil {
		return nil, err
	}
	baseVolume, err := driver.EnsureBaseVolume(ctx, volumestore.EnsureBaseVolumeRequest{
		BaseID:     strings.TrimSuffix(filepath.Base(rootFSPath), filepath.Ext(rootFSPath)),
		SourcePath: rootFSPath,
	})
	if err != nil {
		return nil, fmt.Errorf("prepare base volume: %w", err)
	}
	vmRootFSPath := filepath.Join(runDir, "rootfs-persistent.ext4")
	copyStart := time.Now()
	writableVolume, err := driver.CreateWritableVolume(ctx, volumestore.CreateWritableVolumeRequest{
		VolumeID:       sandboxID,
		BaseRef:        baseVolume.Ref,
		AttachmentPath: vmRootFSPath,
	})
	if err != nil {
		return nil, fmt.Errorf("prepare persistent rootfs: %w", err)
	}
	vmRootFSPath = writableVolume.AttachmentPath
	if err := ext4image.EnsureMinimumSize(ctx, vmRootFSPath, cfg.MinimumRootFSBytes); err != nil {
		return nil, fmt.Errorf("resize persistent rootfs: %w", err)
	}
	rootFSCopyMS := time.Since(copyStart).Milliseconds()

	guestInitPath, _ := guestInitExecutableForRootFS(vmRootFSPath)
	networkCfg, err := resolveDarwinVZNetwork(cfg)
	if err != nil {
		return nil, err
	}
	bootArgs := fmt.Sprintf(
		"console=hvc0 root=/dev/vda rw init=%s cleanroom_guest_port=%d %s",
		guestInitPath,
		cfg.GuestPort,
		dockerServiceBootArgs(compiled, cfg),
	)
	consolePath := filepath.Join(runDir, "vm.console.log")

	configPath := filepath.Join(runDir, "darwin-vz-config.json")

	helper, err := startHelperSession(ctx, runDir, cfg.LaunchSeconds)
	if err != nil {
		return nil, fmt.Errorf("start darwin-vz helper: %w", err)
	}
	helperNeedsClose := true
	defer func() {
		if helperNeedsClose {
			_ = helper.close()
		}
	}()

	startedVM, err := startDarwinVZHelperVM(ctx, helper, darwinVZVMStartRequest{
		ConfigPath:     configPath,
		BackendName:    a.Name(),
		RunDir:         runDir,
		KernelPath:     kernelPath,
		RootFSPath:     vmRootFSPath,
		BootArgs:       bootArgs,
		ConsoleLogPath: consolePath,
		NetworkCfg:     networkCfg,
		VCPUs:          cfg.VCPUs,
		MemoryMiB:      cfg.MemoryMiB,
		GuestPort:      cfg.GuestPort,
		LaunchSeconds:  cfg.LaunchSeconds,
	})
	if err != nil {
		return nil, err
	}

	instance := &sandboxInstance{
		SandboxID:       sandboxID,
		RunDir:          runDir,
		ConfigPath:      configPath,
		ProxySocketPath: startedVM.ProxySocketPath,
		GuestPort:       cfg.GuestPort,
		Policy:          compiled,
		ImageRef:        imageRef,
		ImageDigest:     imageDigest,
		LaunchObservability: &darwinVZLaunchObservability{
			RootFSCopyMS:   rootFSCopyMS,
			HelperTimingMS: startedVM.TimingMS,
			Network:        startedVM.NetworkMetadata,
		},
		NetworkMetadata: startedVM.NetworkMetadata,
		CommandTimeout:  cfg.LaunchSeconds,
		Helper:          helper,
		VMID:            startedVM.VMID,
		vmRootFSPath:    vmRootFSPath,
		exitedCh:        make(chan struct{}),
	}
	go func() {
		err, ok := <-helper.done
		if !ok {
			err = nil
		}
		if normalized := helper.normalizeHelperExitErr(err); normalized != nil {
			err = fmt.Errorf("sandbox helper exited: %w", normalized)
		} else {
			err = nil
		}
		instance.setExited(err)
		close(instance.exitedCh)
	}()

	bootCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.LaunchSeconds)*time.Second)
	defer cancel()
	if err := probeGuestExecReadyWithExit(bootCtx, helper, startedVM.ProxySocketPath, instance.exitedCh, instance.exitedErrOrNil); err != nil {
		return nil, err
	}

	helperNeedsClose = false
	cleanupRunDir = false
	return instance, nil
}

func (a *Adapter) provisionSandbox(ctx context.Context, sandboxID string, compiled *policy.CompiledPolicy, cfg backend.FirecrackerConfig) error {
	a.sandboxMu.Lock()
	if a.sandboxes == nil {
		a.sandboxes = map[string]*sandboxInstance{}
	}
	if a.provisioning == nil {
		a.provisioning = map[string]struct{}{}
	}
	if _, exists := a.sandboxes[sandboxID]; exists {
		a.sandboxMu.Unlock()
		return fmt.Errorf("sandbox %q already provisioned", sandboxID)
	}
	if _, exists := a.provisioning[sandboxID]; exists {
		a.sandboxMu.Unlock()
		return fmt.Errorf("sandbox %q is already provisioning", sandboxID)
	}
	a.provisioning[sandboxID] = struct{}{}
	a.sandboxMu.Unlock()

	instance, err := a.launchSandbox(ctx, sandboxID, compiled, cfg)
	if err != nil {
		a.sandboxMu.Lock()
		delete(a.provisioning, sandboxID)
		a.sandboxMu.Unlock()
		return err
	}

	a.sandboxMu.Lock()
	defer a.sandboxMu.Unlock()
	delete(a.provisioning, sandboxID)
	if _, exists := a.sandboxes[sandboxID]; exists {
		instance.shutdown()
		return fmt.Errorf("sandbox %q already provisioned", sandboxID)
	}
	a.sandboxes[sandboxID] = instance
	return nil
}

func (a *Adapter) launchSandbox(ctx context.Context, sandboxID string, compiled *policy.CompiledPolicy, cfg backend.FirecrackerConfig) (*sandboxInstance, error) {
	launch := a.launchSandboxVMFn
	if launch == nil {
		launch = a.launchSandboxVM
	}
	return launch(ctx, sandboxID, compiled, cfg)
}

func (a *Adapter) executeInSandbox(bootCtx context.Context, runCtx context.Context, instance *sandboxInstance, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
	if instance == nil {
		return nil, errors.New("nil sandbox instance")
	}
	if len(req.Command) == 0 {
		return nil, errors.New("missing command")
	}

	policy := req.Policy
	if policy == nil {
		policy = instance.Policy
	}
	if policy == nil {
		return nil, errors.New("missing compiled policy")
	}
	policyWarn, policyErr := evaluateNetworkPolicy(policy.NetworkDefault, len(policy.Allow))
	if policyErr != nil {
		return nil, policyErr
	}

	warnings := buildRuntimeWarnings(policyWarn)
	stderrPrefix := ""
	for _, warningText := range warnings {
		stderrPrefix += emitExecutionWarning(stream, warningText)
	}

	helper := instance.Helper
	if helper == nil {
		return nil, errors.New("darwin-vz sandbox helper is not available")
	}
	proxySocketPath := strings.TrimSpace(instance.ProxySocketPath)
	if proxySocketPath == "" {
		return nil, errors.New("darwin-vz sandbox proxy socket path is empty")
	}
	if err := instance.exitedErrOrNil(); err != nil {
		return nil, fmt.Errorf("sandbox %q is not running: %w", instance.SandboxID, err)
	}

	conn, err := dialUnixSocketWithExit(bootCtx, proxySocketPath, instance.exitedCh, instance.exitedErrOrNil)
	if err != nil {
		return nil, helper.decorateError(fmt.Errorf("connect darwin-vz proxy socket %q: %w", proxySocketPath, err))
	}
	defer conn.Close()

	if dl, ok := runCtx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return nil, fmt.Errorf("set proxy socket deadline: %w", err)
		}
	}
	go func() {
		<-runCtx.Done()
		_ = conn.Close()
	}()

	gatewayScopeToken := ""
	if a.GatewayRegistry != nil {
		token, tokenErr := randomScopeToken()
		if tokenErr != nil {
			return nil, fmt.Errorf("generate gateway scope token: %w", tokenErr)
		}
		scopeSandboxID := strings.TrimSpace(req.SandboxID)
		if scopeSandboxID == "" {
			scopeSandboxID = strings.TrimSpace(instance.SandboxID)
		}
		if scopeSandboxID == "" {
			scopeSandboxID = strings.TrimSpace(req.ExecutionID)
		}
		if err := a.GatewayRegistry.RegisterScopeToken(token, scopeSandboxID, policy); err != nil {
			return nil, fmt.Errorf("register sandbox in gateway: %w", err)
		}
		gatewayScopeToken = token
		defer a.GatewayRegistry.ReleaseScopeToken(gatewayScopeToken)
	}

	guestReq := vsockexec.ExecRequest{
		Command: append([]string(nil), req.Command...),
		Env:     append([]string(nil), req.Env...),
		TTY:     req.TTY,
	}
	if a.GatewayRegistry != nil && gatewayScopeToken != "" {
		gwPort := a.GatewayPort
		if gwPort <= 0 {
			gwPort = gateway.DefaultPort
		}
		gwHost := strings.TrimSpace(a.GatewayHost)
		if gwHost == "" {
			gwHost = defaultGatewayHost
		}
		guestReq.Env = append(guestReq.Env, gatewayEnvVars(policy, gwHost, gwPort, gatewayScopeToken)...)
	}
	seed := make([]byte, 64)
	if _, err := cryptorand.Read(seed); err == nil {
		guestReq.EntropySeed = seed
	}
	if err := vsockexec.EncodeRequest(conn, guestReq); err != nil {
		return nil, fmt.Errorf("send guest exec request: %w", err)
	}

	inputSender := &inputFrameSender{w: conn}
	if stream.OnAttach != nil {
		stream.OnAttach(backend.AttachIO{
			WriteStdin: func(data []byte) error {
				return inputSender.Send(vsockexec.ExecInputFrame{Type: "stdin", Data: data})
			},
			CloseStdin: func() error {
				return inputSender.Send(vsockexec.ExecInputFrame{Type: "eof"})
			},
			ResizeTTY: func(cols, rows uint32) error {
				return inputSender.Send(vsockexec.ExecInputFrame{Type: "resize", Cols: cols, Rows: rows})
			},
		})
	}

	guestRes, err := vsockexec.DecodeStreamResponse(conn, vsockexec.StreamCallbacks{
		OnStdout: stream.OnStdout,
		OnStderr: stream.OnStderr,
	})
	if err != nil {
		if ctxErr := runCtx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("guest exec canceled while waiting for response: %w", ctxErr)
		}
		return nil, helper.decorateError(fmt.Errorf("decode guest exec response over darwin-vz proxy: %w", err))
	}

	return &backend.ExecutionResult{
		ExecutionID: req.ExecutionID,
		ExitCode:    guestRes.ExitCode,
		LaunchedVM:  false,
		PlanPath:    instance.ConfigPath,
		RunDir:      req.RunDir,
		ImageRef:    instance.ImageRef,
		ImageDigest: instance.ImageDigest,
		Message:     darwinVZResultMessage(guestRes.Error),
		Stdout:      guestRes.Stdout,
		Stderr:      stderrPrefix + guestRes.Stderr,
	}, nil
}

func sandboxRuntimeBaseDir() (string, error) {
	base, err := paths.StateBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "sandboxes"), nil
}

func snapshotStorageBaseDir(cfg backend.FirecrackerConfig) (string, error) {
	if baseDir := strings.TrimSpace(cfg.Snapshots.BaseDir); baseDir != "" {
		return filepath.Clean(baseDir), nil
	}
	return paths.SnapshotDir()
}

func rootFSVolumeDriver(cfg backend.FirecrackerConfig) (volumestore.Driver, error) {
	return newRootFSVolumeDriver(cfg, newDarwinVZFileVolumeDriver, newDarwinVZAPFSVolumeDriver)
}

type volumeDriverFactory func(string) (volumestore.Driver, error)

func newRootFSVolumeDriver(cfg backend.FirecrackerConfig, newFileDriver, newAPFSDriver volumeDriverFactory) (volumestore.Driver, error) {
	driverName := strings.ToLower(strings.TrimSpace(cfg.Snapshots.Driver))
	if driverName == "" {
		driverName = "apfs"
	}
	snapshotBaseDir, err := snapshotStorageBaseDir(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot base directory: %w", err)
	}
	switch driverName {
	case "file":
		driver, err := newFileDriver(snapshotBaseDir)
		if err != nil {
			return nil, err
		}
		return driver, nil
	case "apfs":
		driver, err := newAPFSDriver(snapshotBaseDir)
		if err != nil {
			return nil, err
		}
		fileDriver, err := newFileDriver(snapshotBaseDir)
		if err != nil {
			return nil, err
		}
		return &fallbackVolumeDriver{
			primary:        driver,
			fallback:       fileDriver,
			shouldFallback: shouldFallbackFromAPFS,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported darwin-vz snapshot driver %q", cfg.Snapshots.Driver)
	}
}

func newDarwinVZFileVolumeDriver(snapshotBaseDir string) (volumestore.Driver, error) {
	driver, err := volumestore.NewFileDriver(volumestore.FileDriverOptions{
		SnapshotBaseDir: snapshotBaseDir,
		Namespace:       "darwin-vz",
	})
	if err != nil {
		return nil, err
	}
	return driver, nil
}

func newDarwinVZAPFSVolumeDriver(snapshotBaseDir string) (volumestore.Driver, error) {
	driver, err := volumestore.NewAPFSDriver(volumestore.APFSDriverOptions{
		SnapshotBaseDir: snapshotBaseDir,
		Namespace:       "darwin-vz",
	})
	if err != nil {
		return nil, err
	}
	return driver, nil
}

func shouldFallbackFromAPFS(err error) bool {
	return errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSYS)
}

type fallbackVolumeDriver struct {
	primary        volumestore.Driver
	fallback       volumestore.Driver
	shouldFallback func(error) bool
}

func (d *fallbackVolumeDriver) Name() string { return d.primary.Name() }

func (d *fallbackVolumeDriver) EnsureBaseVolume(ctx context.Context, req volumestore.EnsureBaseVolumeRequest) (volumestore.BaseVolume, error) {
	return d.primary.EnsureBaseVolume(ctx, req)
}

func (d *fallbackVolumeDriver) CreateWritableVolume(ctx context.Context, req volumestore.CreateWritableVolumeRequest) (volumestore.WritableVolume, error) {
	writable, err := d.primary.CreateWritableVolume(ctx, req)
	if err == nil || !d.shouldFallback(err) {
		return writable, err
	}
	return d.fallback.CreateWritableVolume(ctx, req)
}

func (d *fallbackVolumeDriver) SnapshotVolume(ctx context.Context, req volumestore.SnapshotVolumeRequest) (volumestore.Snapshot, error) {
	snapshot, err := d.primary.SnapshotVolume(ctx, req)
	if err == nil || !d.shouldFallback(err) {
		return snapshot, err
	}
	return d.fallback.SnapshotVolume(ctx, req)
}

func (d *fallbackVolumeDriver) CloneSnapshotToVolume(ctx context.Context, req volumestore.CloneSnapshotToVolumeRequest) (volumestore.WritableVolume, error) {
	volume, err := d.primary.CloneSnapshotToVolume(ctx, req)
	if err == nil || !d.shouldFallback(err) {
		return volume, err
	}
	return d.fallback.CloneSnapshotToVolume(ctx, req)
}

func (d *fallbackVolumeDriver) DestroyVolume(ctx context.Context, req volumestore.DestroyVolumeRequest) error {
	return d.primary.DestroyVolume(ctx, req)
}

func (d *fallbackVolumeDriver) DestroySnapshot(ctx context.Context, req volumestore.DestroySnapshotRequest) error {
	return d.primary.DestroySnapshot(ctx, req)
}

func snapshotVolumeDriver(cfg backend.FirecrackerConfig) (volumestore.Driver, error) {
	if !cfg.Snapshots.Enabled {
		return nil, errors.New("darwin-vz snapshots are not enabled")
	}
	return rootFSVolumeDriver(cfg)
}

func probeGuestExecReady(ctx context.Context, helper *helperSession, socketPath string) error {
	return probeGuestExecReadyWithExit(ctx, helper, socketPath, nil, nil)
}

func probeGuestExecReadyWithExit(ctx context.Context, helper *helperSession, socketPath string, exitedCh <-chan struct{}, exitedErrOrNil func() error) error {
	conn, err := dialUnixSocketWithExit(ctx, socketPath, exitedCh, exitedErrOrNil)
	if err != nil {
		if helper != nil {
			return helper.decorateError(fmt.Errorf("connect darwin-vz proxy socket %q for guest readiness probe: %w", socketPath, err))
		}
		return fmt.Errorf("connect darwin-vz proxy socket %q for guest readiness probe: %w", socketPath, err)
	}
	defer conn.Close()

	if err := vsockexec.EncodeRequest(conn, vsockexec.ExecRequest{}); err != nil {
		if helper != nil {
			return helper.decorateError(fmt.Errorf("send guest readiness probe over darwin-vz proxy: %w", err))
		}
		return fmt.Errorf("send guest readiness probe over darwin-vz proxy: %w", err)
	}
	if _, err := vsockexec.DecodeResponse(conn); err != nil {
		if helper != nil {
			return helper.decorateError(fmt.Errorf("decode guest readiness probe over darwin-vz proxy: %w", err))
		}
		return fmt.Errorf("decode guest readiness probe over darwin-vz proxy: %w", err)
	}
	return nil
}

func dialUnixSocketWithExit(ctx context.Context, socketPath string, exitedCh <-chan struct{}, exitedErrOrNil func() error) (net.Conn, error) {
	dialer := net.Dialer{}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	exitErr := func() error {
		if exitedErrOrNil == nil {
			return nil
		}
		return exitedErrOrNil()
	}

	for {
		conn, err := dialer.DialContext(ctx, "unix", socketPath)
		if err == nil {
			return conn, nil
		}
		if err := exitErr(); err != nil {
			return nil, fmt.Errorf("sandbox exited while waiting for unix socket %q: %w", socketPath, err)
		}

		select {
		case <-ctx.Done():
			if err := exitErr(); err != nil {
				return nil, fmt.Errorf("sandbox exited while waiting for unix socket %q: %w", socketPath, err)
			}
			return nil, fmt.Errorf("timed out waiting for unix socket %q: %w", socketPath, ctx.Err())
		case <-exitedCh:
			if err := exitErr(); err != nil {
				return nil, fmt.Errorf("sandbox exited while waiting for unix socket %q: %w", socketPath, err)
			}
			return nil, fmt.Errorf("sandbox exited while waiting for unix socket %q", socketPath)
		case <-ticker.C:
		}
	}
}

func (s *sandboxInstance) shutdown() {
	if s == nil {
		return
	}
	if s.Helper != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if strings.TrimSpace(s.VMID) != "" {
			_, _ = s.Helper.request(stopCtx, helperControlRequest{Op: "StopVM", VMID: s.VMID})
		}
		cancel()
		_ = s.Helper.close()
	}
	if strings.TrimSpace(s.RunDir) != "" {
		_ = os.RemoveAll(s.RunDir)
		return
	}
	if strings.TrimSpace(s.vmRootFSPath) != "" {
		_ = os.Remove(s.vmRootFSPath)
	}
}

func (s *sandboxInstance) setExited(err error) {
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	if s.exitReady {
		return
	}
	s.exitErr = err
	s.exitReady = true
}

func (s *sandboxInstance) exitedErrOrNil() error {
	s.exitMu.RLock()
	defer s.exitMu.RUnlock()
	if !s.exitReady {
		return nil
	}
	if s.exitErr == nil {
		return errors.New("sandbox exited")
	}
	return s.exitErr
}

func darwinVZResultMessage(guestErr string) string {
	guestErr = strings.TrimSpace(guestErr)
	if guestErr == "" {
		return ""
	}
	return guestErr
}

func buildRuntimeWarnings(policyWarning string) []string {
	warnings := make([]string, 0, 2)
	if trimmed := strings.TrimSpace(policyWarning); trimmed != "" {
		warnings = append(warnings, trimmed)
	}
	warnings = append(warnings, guestNetworkUnavailableWarning)
	return warnings
}

func emitExecutionWarning(stream backend.OutputStream, warningText string) string {
	warningText = strings.TrimSpace(warningText)
	if warningText == "" {
		return ""
	}
	if stream.OnWarning != nil {
		stream.OnWarning(warningText)
		return ""
	}
	warningLine := "warning: " + warningText + "\n"
	if stream.OnStderr != nil {
		stream.OnStderr([]byte(warningLine))
	}
	return warningLine
}

type imageArtifact struct {
	Ref        string
	Digest     string
	RootFSPath string
	CacheHit   bool
}

func (a *Adapter) resolveRootFSPath(ctx context.Context, req backend.ExecutionRequest) (path, imageRef, imageDigest, notice string, err error) {
	configuredPath := strings.TrimSpace(req.RootFSPath)
	if configuredPath != "" {
		if _, statErr := os.Stat(configuredPath); statErr == nil {
			return configuredPath, strings.TrimSpace(req.Policy.ImageRef), strings.TrimSpace(req.Policy.ImageDigest), "", nil
		}
		notice = fmt.Sprintf("configured rootfs %q is not accessible; deriving rootfs from sandbox.image.ref", configuredPath)
	}
	ref := strings.TrimSpace(req.Policy.ImageRef)
	if ref == "" {
		return "", "", "", "", errors.New("rootfs is not configured and sandbox.image.ref is empty")
	}

	ensurePrepared := a.ensurePreparedRootFSFn
	if ensurePrepared == nil {
		ensurePrepared = a.ensurePreparedRuntimeRootFSFromImage
	}
	prepared, err := ensurePrepared(ctx, ref)
	if err != nil {
		return "", "", "", "", err
	}

	derivation := fmt.Sprintf("derived rootfs from sandbox.image.ref digest %s (%s)", prepared.Digest, map[bool]string{true: "cache hit", false: "cache miss"}[prepared.Hit])
	if notice != "" {
		notice += "; " + derivation
	} else {
		notice = derivation
	}
	return prepared.Path, prepared.Ref, prepared.Digest, notice, nil
}

func (a *Adapter) ensurePreparedRuntimeRootFSFromImage(ctx context.Context, imageRef string) (preparedRootFS, error) {
	artifact, err := a.ensureImageArtifact(ctx, imageRef)
	if err != nil {
		return preparedRootFS{}, err
	}
	if strings.TrimSpace(artifact.RootFSPath) == "" {
		return preparedRootFS{}, errors.New("resolved image rootfs path is empty")
	}
	if _, err := os.Stat(artifact.RootFSPath); err != nil {
		return preparedRootFS{}, fmt.Errorf("resolved image rootfs %q: %w", artifact.RootFSPath, err)
	}

	guestAgentPath, guestAgentHash, err := a.getGuestAgentBinary()
	if err != nil {
		return preparedRootFS{}, err
	}

	preparedPath, err := preparedRuntimeRootFSPath(artifact.Digest, guestAgentHash)
	if err != nil {
		return preparedRootFS{}, err
	}
	if _, err := os.Stat(preparedPath); err == nil {
		if validateErr := validatePreparedRuntimeRootFS(preparedPath); validateErr == nil {
			return preparedRootFS{
				Ref:    artifact.Ref,
				Digest: artifact.Digest,
				Path:   preparedPath,
				Hit:    true,
			}, nil
		}
		// Stale/incomplete cache entries should be rebuilt instead of reused.
		_ = os.Remove(preparedPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return preparedRootFS{}, fmt.Errorf("inspect prepared runtime rootfs %q: %w", preparedPath, err)
	}

	a.runtimeImageMu.Lock()
	defer a.runtimeImageMu.Unlock()

	if _, err := os.Stat(preparedPath); err == nil {
		if validateErr := validatePreparedRuntimeRootFS(preparedPath); validateErr == nil {
			return preparedRootFS{
				Ref:    artifact.Ref,
				Digest: artifact.Digest,
				Path:   preparedPath,
				Hit:    true,
			}, nil
		}
		if removeErr := os.Remove(preparedPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return preparedRootFS{}, fmt.Errorf("remove invalid prepared runtime rootfs %q: %w", preparedPath, removeErr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return preparedRootFS{}, fmt.Errorf("inspect prepared runtime rootfs %q: %w", preparedPath, err)
	}

	preparedDir := filepath.Dir(preparedPath)
	if err := os.MkdirAll(preparedDir, 0o755); err != nil {
		return preparedRootFS{}, fmt.Errorf("create prepared rootfs cache directory %q: %w", preparedDir, err)
	}

	tmpPath := preparedPath + fmt.Sprintf(".tmp-%d", time.Now().UnixNano())
	if err := copyFile(artifact.RootFSPath, tmpPath); err != nil {
		return preparedRootFS{}, fmt.Errorf("copy rootfs image for runtime preparation: %w", err)
	}
	if err := a.installGuestRuntimeIntoRootFS(tmpPath, guestAgentPath); err != nil {
		_ = os.Remove(tmpPath)
		return preparedRootFS{}, err
	}
	if err := validatePreparedRuntimeRootFS(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return preparedRootFS{}, fmt.Errorf("validate prepared runtime rootfs %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, preparedPath); err != nil {
		_ = os.Remove(tmpPath)
		if _, statErr := os.Stat(preparedPath); statErr == nil {
			if validateErr := validatePreparedRuntimeRootFS(preparedPath); validateErr == nil {
				return preparedRootFS{
					Ref:    artifact.Ref,
					Digest: artifact.Digest,
					Path:   preparedPath,
					Hit:    true,
				}, nil
			}
			return preparedRootFS{}, fmt.Errorf("prepared runtime rootfs %q became invalid during rename race", preparedPath)
		}
		return preparedRootFS{}, fmt.Errorf("store prepared runtime rootfs %q: %w", preparedPath, err)
	}

	return preparedRootFS{
		Ref:    artifact.Ref,
		Digest: artifact.Digest,
		Path:   preparedPath,
		Hit:    false,
	}, nil
}

var preparedRuntimeRootFSRequiredPaths = []string{
	guestAgentPath,
}

func validatePreparedRuntimeRootFS(path string) error {
	for _, requiredPath := range preparedRuntimeRootFSRequiredPaths {
		if !ext4PathExists(path, requiredPath) {
			return fmt.Errorf("required runtime file %q is missing or unreadable", requiredPath)
		}
	}
	return validatePreparedRuntimeRootFSInitPathForLayout(
		ext4PathExists(path, "/bin/sh"),
		ext4PathType(path, "/sbin"),
		func(requiredPath string) bool {
			return ext4PathExists(path, requiredPath)
		},
	)
}
func preparedRuntimeRootFSPath(imageDigest, guestAgentHash string) (string, error) {
	cacheBase, err := paths.CacheBaseDir()
	if err != nil {
		return "", fmt.Errorf("resolve cache base directory: %w", err)
	}
	key := runtimeRootFSCacheKey(imageDigest, guestAgentHash)
	return filepath.Join(cacheBase, "darwin-vz", "runtime-rootfs", key+".ext4"), nil
}

func runtimeRootFSCacheKey(imageDigest, guestAgentHash string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(imageDigest) + "|" + guestAgentHash + "|" + runtime.GOARCH + "|" + preparedRuntimeRootFSVersion + "|" + guestInitScriptTemplate))
	return hex.EncodeToString(sum[:])
}

func (a *Adapter) ensureImageArtifact(ctx context.Context, imageRef string) (imageArtifact, error) {
	trimmedRef := strings.TrimSpace(imageRef)
	if trimmedRef == "" {
		return imageArtifact{}, errors.New("sandbox.image.ref is required for launched execution")
	}

	mgr, err := a.getImageManager()
	if err != nil {
		return imageArtifact{}, fmt.Errorf("initialise image manager: %w", err)
	}

	result, err := mgr.Ensure(ctx, trimmedRef)
	if err != nil {
		return imageArtifact{}, fmt.Errorf("resolve image %q: %w", trimmedRef, err)
	}
	return imageArtifact{
		Ref:        result.Record.Ref,
		Digest:     result.Record.Digest,
		RootFSPath: result.Record.RootFSPath,
		CacheHit:   result.CacheHit,
	}, nil
}

func (a *Adapter) getImageManager() (imageEnsurer, error) {
	if a.newImageManager == nil {
		a.newImageManager = defaultImageManagerFactory
	}
	a.imageManagerOnce.Do(func() {
		a.imageManager, a.imageManagerErr = a.newImageManager()
	})
	if a.imageManagerErr != nil {
		return nil, a.imageManagerErr
	}
	if a.imageManager == nil {
		return nil, errors.New("image manager factory returned nil manager")
	}
	return a.imageManager, nil
}

func (a *Adapter) getGuestAgentBinary() (string, string, error) {
	a.guestAgentOnce.Do(func() {
		a.guestAgentPath, a.guestAgentErr = discoverGuestAgentBinary()
		if a.guestAgentErr != nil {
			return
		}
		a.guestAgentHash, a.guestAgentErr = hashFileSHA256(a.guestAgentPath)
	})
	if a.guestAgentErr != nil {
		return "", "", a.guestAgentErr
	}
	if strings.TrimSpace(a.guestAgentPath) == "" || strings.TrimSpace(a.guestAgentHash) == "" {
		return "", "", errors.New("failed to resolve cleanroom guest agent binary")
	}
	return a.guestAgentPath, a.guestAgentHash, nil
}

func discoverGuestAgentBinary() (string, error) {
	return discoverGuestAgentBinaryWith(
		runtime.GOARCH,
		exec.LookPath,
		os.Executable,
		os.Getwd,
		os.Stat,
		isLinuxGuestAgentBinary,
	)
}

func discoverGuestAgentBinaryWith(
	goarch string,
	lookPath func(string) (string, error),
	executable func() (string, error),
	getwd func() (string, error),
	stat func(string) (os.FileInfo, error),
	validate func(string) (bool, error),
) (string, error) {
	linuxName := fmt.Sprintf("cleanroom-guest-agent-linux-%s", goarch)
	candidates := []string{}
	if self, err := executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(self), linuxName))
		candidates = append(candidates, filepath.Join(filepath.Dir(self), "cleanroom-guest-agent"))
	}
	if getwd != nil {
		if cwd, err := getwd(); err == nil {
			if path, err := resolvePrebuiltBinaryPathFromWorkdir(cwd, linuxName, stat); err == nil {
				candidates = append(candidates, path)
			}
		}
	}
	candidates = append(candidates, linuxName, "cleanroom-guest-agent")

	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		resolved := candidate
		if !filepath.IsAbs(candidate) {
			p, lookErr := lookPath(candidate)
			if lookErr != nil {
				continue
			}
			resolved = p
		}
		info, statErr := stat(resolved)
		if statErr != nil || info.IsDir() {
			continue
		}
		ok, validateErr := validate(resolved)
		if validateErr != nil {
			return "", fmt.Errorf("validate guest agent binary %q: %w", resolved, validateErr)
		}
		if ok {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("linux guest-agent binary not found for architecture %s; run `mise run build` or `mise run install` to make cleanroom-guest-agent-linux-%s discoverable", goarch, goarch)
}

func isLinuxGuestAgentBinary(path string) (bool, error) {
	f, err := elf.Open(path)
	if err != nil {
		// Non-ELF binaries are not valid guest binaries.
		return false, nil
	}
	defer f.Close()

	expectedMachine, ok := expectedGuestAgentELFMachine(runtime.GOARCH)
	if !ok {
		return false, fmt.Errorf("unsupported host architecture %q", runtime.GOARCH)
	}
	return f.FileHeader.Machine == expectedMachine, nil
}

func expectedGuestAgentELFMachine(goarch string) (elf.Machine, bool) {
	switch goarch {
	case "arm64":
		return elf.EM_AARCH64, true
	case "amd64":
		return elf.EM_X86_64, true
	default:
		return 0, false
	}
}

func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %q for hashing: %w", path, err)
	}
	defer f.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", fmt.Errorf("hash %q: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func injectFileIntoExt4(imagePath, srcPath, dstPath string, mode os.FileMode) error {
	return ext4edit.InjectFile(imagePath, srcPath, dstPath, mode)
}

func ext4PathExists(imagePath, path string) bool {
	return ext4edit.PathExists(imagePath, path)
}

func ext4PathType(imagePath, path string) ext4PathKind {
	switch ext4edit.PathType(imagePath, path) {
	case ext4edit.PathKindDirectory:
		return ext4PathKindDirectory
	case ext4edit.PathKindRegular:
		return ext4PathKindRegular
	case ext4edit.PathKindSymlink:
		return ext4PathKindSymlink
	default:
		return ext4PathKindUnknown
	}
}

func (a *Adapter) resolveKernelPath(ctx context.Context, configuredPath string) (path, notice string, err error) {
	resolved, err := bootassets.ResolveKernelPathForHost(ctx, a.Name(), configuredPath)
	if err != nil {
		return "", "", err
	}
	return resolved.Path, resolved.Notice, nil
}

type inputFrameSender struct {
	w  io.Writer
	mu sync.Mutex
}

func (s *inputFrameSender) Send(frame vsockexec.ExecInputFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return vsockexec.EncodeInputFrame(s.w, frame)
}

func darwinVZTimingSummary(timingMS map[string]int64) string {
	if len(timingMS) == 0 {
		return ""
	}
	keys := make([]string, 0, len(timingMS))
	for key := range timingMS {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%dms", key, timingMS[key]))
	}
	return "timings " + strings.Join(parts, " ")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func logExecutionNotice(backendName, runID, notice string) {
	msg := strings.TrimSpace(notice)
	if msg == "" {
		return
	}
	fields := []any{"backend", strings.TrimSpace(backendName)}
	id := strings.TrimSpace(runID)
	if id != "" {
		fields = append(fields, "execution_id", id)
	}
	log.Info(msg, fields...)
}

func randomScopeToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := cryptorand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
