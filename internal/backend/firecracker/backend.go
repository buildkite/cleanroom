package firecracker

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/imagemgr"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/vsockexec"
	fcvsock "github.com/firecracker-microvm/firecracker-go-sdk/vsock"
)

type imageEnsurer interface {
	Ensure(context.Context, string) (imagemgr.EnsureResult, error)
}

type imageManagerFactory func() (imageEnsurer, error)

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

	sandboxMu         sync.Mutex
	sandboxes         map[string]*sandboxInstance
	provisioning      map[string]struct{}
	launchSandboxVMFn func(context.Context, string, *policy.CompiledPolicy, backend.FirecrackerConfig) (*sandboxInstance, error)
	runGuestCommandFn func(context.Context, context.Context, <-chan struct{}, func() error, string, uint32, vsockexec.ExecRequest, backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error)
}

type sandboxInstance struct {
	SandboxID      string
	RunDir         string
	ConfigPath     string
	VsockPath      string
	GuestPort      uint32
	Policy         *policy.CompiledPolicy
	ImageRef       string
	ImageDigest    string
	CommandTimeout int64
	fcCmd          *exec.Cmd
	exitedCh       chan struct{}
	exitMu         sync.RWMutex
	exitErr        error
	exitReady      bool
	cleanupNetwork func()
	vmRootFSPath   string
}

const runObservabilityFile = "run-observability.json"
const vsockDialRetryInterval = 50 * time.Millisecond
const preparedRuntimeRootFSVersion = "v1"
const privilegedModeSudo = "sudo"
const privilegedModeHelper = "helper"
const defaultPrivilegedHelperPath = "/usr/local/sbin/cleanroom-root-helper"
const defaultDownloadMaxBytes int64 = 10 * 1024 * 1024

const guestInitScriptTemplate = `#!/bin/sh
set -eu

mount -t proc proc /proc 2>/dev/null || true
mount -t sysfs sysfs /sys 2>/dev/null || true
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mkdir -p /dev/pts /run /tmp
mount -t devpts devpts /dev/pts 2>/dev/null || true
mount -t tmpfs tmpfs /run 2>/dev/null || true
mount -t tmpfs tmpfs /tmp 2>/dev/null || true

export HOME=/root
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/root/.local/bin

cmdline="$(cat /proc/cmdline 2>/dev/null || true)"
arg_value() {
  key="$1"
  for token in $cmdline; do
    case "$token" in
      "$key"=*) echo "${token#*=}"; return 0 ;;
    esac
  done
  return 1
}

GUEST_IP="$(arg_value cleanroom_guest_ip || true)"
GUEST_GW="$(arg_value cleanroom_guest_gw || true)"
GUEST_MASK="$(arg_value cleanroom_guest_mask || true)"
GUEST_DNS="$(arg_value cleanroom_guest_dns || true)"
GUEST_PORT="$(arg_value cleanroom_guest_port || true)"

if command -v ip >/dev/null 2>&1 && [ -n "$GUEST_IP" ]; then
  [ -n "$GUEST_MASK" ] || GUEST_MASK="24"
  ip link set dev eth0 up 2>/dev/null || true
  ip addr flush dev eth0 2>/dev/null || true
  ip addr add "$GUEST_IP/$GUEST_MASK" dev eth0 2>/dev/null || true
  if [ -n "$GUEST_GW" ]; then
    ip route add default via "$GUEST_GW" dev eth0 2>/dev/null || true
  fi
  if [ -n "$GUEST_DNS" ]; then
    printf 'nameserver %s\n' "$GUEST_DNS" > /etc/resolv.conf 2>/dev/null || true
  fi
fi

if [ -z "$GUEST_PORT" ]; then
  GUEST_PORT="10700"
fi
export CLEANROOM_VSOCK_PORT="$GUEST_PORT"

while true; do
  /usr/local/bin/cleanroom-guest-agent || true
  sleep 1
done
`

func New() *Adapter {
	return &Adapter{newImageManager: defaultImageManagerFactory}
}

func defaultImageManagerFactory() (imageEnsurer, error) {
	return imagemgr.New(imagemgr.Options{})
}

func (a *Adapter) Name() string {
	return "firecracker"
}

func (a *Adapter) Run(ctx context.Context, req backend.RunRequest) (*backend.RunResult, error) {
	return a.run(ctx, req, backend.OutputStream{})
}

func (a *Adapter) RunStream(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
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

	launch := a.launchSandboxVMFn
	if launch == nil {
		launch = a.launchSandboxVM
	}

	instance, err := launch(ctx, sandboxID, req.Policy, req.FirecrackerConfig)
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

func (a *Adapter) RunInSandbox(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
	sandboxID := strings.TrimSpace(req.SandboxID)
	if sandboxID == "" {
		return nil, errors.New("missing sandbox_id")
	}
	if len(req.Command) == 0 {
		return nil, errors.New("missing command")
	}
	if strings.TrimSpace(req.RunID) == "" {
		return nil, errors.New("missing run_id")
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

	runStart := time.Now()
	runDir := strings.TrimSpace(req.RunDir)
	if runDir == "" {
		if baseDir, err := paths.RunBaseDir(); err == nil {
			runDir = filepath.Join(baseDir, req.RunID)
		}
	}
	observation := firecrackerRunObservation{
		RunID:       req.RunID,
		Backend:     a.Name(),
		LaunchedVM:  false,
		ImageRef:    instance.ImageRef,
		ImageDigest: instance.ImageDigest,
		PlanPath:    instance.ConfigPath,
		RunDir:      runDir,
	}
	writeObservation := func() {
		if runDir == "" {
			return
		}
		observation.TotalMS = time.Since(runStart).Milliseconds()
		obsPath := filepath.Join(runDir, runObservabilityFile)
		_ = writeJSON(obsPath, observation)
	}
	defer writeObservation()
	if runDir != "" {
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			return nil, fmt.Errorf("create run directory: %w", err)
		}
	}

	guestResult, timing, err := a.executeInSandbox(ctx, instance, req.LaunchSeconds, req.Command, stream)
	if err != nil {
		observation.ExitCode = 1
		observation.GuestError = err.Error()
		return nil, err
	}
	observation.ExitCode = guestResult.ExitCode
	observation.GuestError = guestResult.Error
	observation.GuestExecMS = timing.CommandRun.Milliseconds()
	observation.VsockWaitMS = timing.WaitForAgent.Milliseconds()

	message := runResultMessage("guest command execution complete")
	if guestResult.Error != "" {
		message = runResultMessage("guest command execution completed with guest-side error detail: " + guestResult.Error)
	}

	return &backend.RunResult{
		RunID:       req.RunID,
		ExitCode:    guestResult.ExitCode,
		LaunchedVM:  false,
		PlanPath:    instance.ConfigPath,
		RunDir:      runDir,
		ImageRef:    instance.ImageRef,
		ImageDigest: instance.ImageDigest,
		Message:     message,
		Stdout:      guestResult.Stdout,
		Stderr:      guestResult.Stderr,
	}, nil
}

func (a *Adapter) DownloadSandboxFile(ctx context.Context, sandboxID, path string, maxBytes int64) ([]byte, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, errors.New("missing sandbox_id")
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("missing path")
	}
	if !strings.HasPrefix(path, "/") {
		return nil, errors.New("invalid path: must be absolute")
	}
	if maxBytes <= 0 {
		maxBytes = defaultDownloadMaxBytes
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

	var stdout bytes.Buffer
	limit := maxBytes + 1
	cmd := []string{"head", "-c", strconv.FormatInt(limit, 10), "--", path}
	result, _, err := a.executeInSandbox(ctx, instance, 0, cmd, backend.OutputStream{OnStdout: func(chunk []byte) {
		_, _ = stdout.Write(chunk)
	}})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		msg := strings.TrimSpace(result.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(result.Error)
		}
		if msg == "" {
			msg = "read file command failed"
		}
		return nil, errors.New(msg)
	}

	data := stdout.Bytes()
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file %q exceeds max_bytes=%d", path, maxBytes)
	}
	return append([]byte(nil), data...), nil
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

func (a *Adapter) executeInSandbox(ctx context.Context, instance *sandboxInstance, launchSeconds int64, command []string, stream backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
	guestReq := vsockexec.ExecRequest{Command: append([]string(nil), command...)}
	seed := make([]byte, 64)
	if _, err := cryptorand.Read(seed); err == nil {
		guestReq.EntropySeed = seed
	}

	connectSeconds := launchSeconds
	if connectSeconds <= 0 {
		connectSeconds = instance.CommandTimeout
	}
	if connectSeconds <= 0 {
		connectSeconds = 30
	}
	bootCtx, bootCancel := context.WithTimeout(ctx, time.Duration(connectSeconds)*time.Second)
	defer bootCancel()

	runGuestCommandFn := a.runGuestCommandFn
	if runGuestCommandFn == nil {
		runGuestCommandFn = runGuestCommand
	}

	return runGuestCommandFn(bootCtx, ctx, instance.exitedCh, instance.exitedErrOrNil, instance.VsockPath, instance.GuestPort, guestReq, stream)
}

func (a *Adapter) Doctor(_ context.Context, req backend.DoctorRequest) (*backend.DoctorReport, error) {
	report := &backend.DoctorReport{
		Backend: a.Name(),
	}

	appendCheck := func(name, status, message string) {
		report.Checks = append(report.Checks, backend.DoctorCheck{
			Name:    name,
			Status:  status,
			Message: message,
		})
	}

	if runtime.GOOS == "linux" {
		appendCheck("os", "pass", "linux host detected")
	} else {
		appendCheck("os", "fail", fmt.Sprintf("linux required, current OS is %s", runtime.GOOS))
	}

	binary := req.BinaryPath
	if binary == "" {
		binary = "firecracker"
	}
	if _, err := exec.LookPath(binary); err != nil {
		appendCheck("binary", "fail", fmt.Sprintf("firecracker binary %q not found in PATH", binary))
	} else {
		appendCheck("binary", "pass", fmt.Sprintf("found firecracker binary %q", binary))
	}

	if _, err := os.Stat("/dev/kvm"); err != nil {
		appendCheck("kvm", "fail", "missing /dev/kvm")
	} else {
		if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
			appendCheck("kvm", "fail", fmt.Sprintf("cannot open /dev/kvm read-write: %v", err))
		} else {
			_ = f.Close()
			appendCheck("kvm", "pass", "/dev/kvm is accessible")
		}
	}

	if req.KernelImagePath == "" {
		appendCheck("kernel_image", "warn", "kernel image not configured (required for --launch)")
	} else if _, err := os.Stat(req.KernelImagePath); err != nil {
		appendCheck("kernel_image", "fail", fmt.Sprintf("kernel image not accessible: %v", err))
	} else {
		appendCheck("kernel_image", "pass", fmt.Sprintf("kernel image configured: %s", req.KernelImagePath))
	}
	if guestAgentPath, _, err := a.getGuestAgentBinary(); err != nil {
		appendCheck("guest_agent_binary", "fail", err.Error())
	} else {
		appendCheck("guest_agent_binary", "pass", fmt.Sprintf("found cleanroom guest agent %q", guestAgentPath))
	}

	imageRefStatus := "warn"
	imageRefMessage := "policy not loaded; cannot validate sandbox.image.ref"
	if req.Policy != nil {
		if strings.TrimSpace(req.Policy.ImageRef) == "" {
			imageRefStatus = "fail"
			imageRefMessage = "sandbox.image.ref is required for launched execution"
		} else {
			imageRefStatus = "pass"
			imageRefMessage = fmt.Sprintf("sandbox image ref configured: %s", req.Policy.ImageRef)
		}
	}
	appendCheck("sandbox_image_ref", imageRefStatus, imageRefMessage)

	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		appendCheck("mkfs_ext4", "fail", "mkfs.ext4 is required to materialise OCI rootfs images")
	} else {
		appendCheck("mkfs_ext4", "pass", "found mkfs.ext4 for OCI rootfs materialisation")
	}

	if req.GuestPort == 0 {
		appendCheck("vsock_port", "pass", fmt.Sprintf("using default guest vsock port %d", vsockexec.DefaultPort))
	} else {
		appendCheck("vsock_port", "pass", fmt.Sprintf("configured guest vsock port %d", req.GuestPort))
	}
	policyRules := 0
	policyRulesStatus := "warn"
	policyRulesMessage := "policy not loaded; cannot verify network allow entries"
	if req.Policy != nil {
		policyRules = len(req.Policy.Allow)
		policyRulesStatus = "pass"
		policyRulesMessage = fmt.Sprintf("loaded %d policy allow entries", policyRules)
	}
	appendCheck("network_policy_rules", policyRulesStatus, policyRulesMessage)

	privilegedMode, privilegedHelperPath := resolvePrivilegedExecution(req.FirecrackerConfig)
	appendCheck("network_privileged_mode", "pass", fmt.Sprintf("using privileged command mode %q", privilegedMode))

	requiredCommands := []string{"ip", "iptables", "sysctl", "sudo"}
	for _, cmd := range requiredCommands {
		if _, err := exec.LookPath(cmd); err != nil {
			appendCheck("network_cmd_"+cmd, "fail", fmt.Sprintf("missing required host command %q", cmd))
		} else {
			appendCheck("network_cmd_"+cmd, "pass", fmt.Sprintf("found host command %q", cmd))
		}
	}

	if privilegedMode == privilegedModeHelper {
		if _, err := os.Stat(privilegedHelperPath); err != nil {
			appendCheck("network_helper", "fail", fmt.Sprintf("privileged helper %q is not accessible: %v", privilegedHelperPath, err))
		} else {
			appendCheck("network_helper", "pass", fmt.Sprintf("using privileged helper %q", privilegedHelperPath))
		}
	}

	if err := runRootCommand(context.Background(), req.FirecrackerConfig, "true"); err != nil {
		appendCheck("network_privileged_probe", "warn", fmt.Sprintf("privileged command probe failed: %v", err))
	} else {
		appendCheck("network_privileged_probe", "pass", "privileged command probe succeeded")
	}
	if err := runRootCommand(context.Background(), req.FirecrackerConfig, "ip", "link", "show"); err != nil {
		appendCheck("network_privileged_ip", "warn", fmt.Sprintf("privileged ip link show failed: %v", err))
	} else {
		appendCheck("network_privileged_ip", "pass", "privileged ip command execution succeeded")
	}

	return report, nil
}

func (a *Adapter) run(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
	runStart := time.Now()
	observation := firecrackerRunObservation{
		RunID:      req.RunID,
		Backend:    a.Name(),
		LaunchedVM: req.Launch,
		ExitCode:   1,
	}

	if req.Policy == nil {
		return nil, errors.New("missing compiled policy")
	}
	observation.ImageRef = req.Policy.ImageRef
	observation.ImageDigest = req.Policy.ImageDigest
	if req.Policy.NetworkDefault != "deny" {
		return nil, fmt.Errorf("firecracker backend requires deny-by-default policy, got %q", req.Policy.NetworkDefault)
	}
	if len(req.Command) == 0 {
		return nil, errors.New("missing command")
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("firecracker backend is linux-only, current OS is %s", runtime.GOOS)
	}

	runDir := req.RunDir
	if runDir == "" {
		baseDir, err := paths.RunBaseDir()
		if err != nil {
			return nil, fmt.Errorf("resolve run base directory: %w", err)
		}
		runDir = filepath.Join(baseDir, req.RunID)
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	observationPath := filepath.Join(runDir, runObservabilityFile)
	writeObservation := func() {
		observation.TotalMS = time.Since(runStart).Milliseconds()
		_ = writeJSON(observationPath, observation)
	}
	defer writeObservation()

	if req.VCPUs <= 0 {
		req.VCPUs = 1
	}
	if req.MemoryMiB <= 0 {
		req.MemoryMiB = 512
	}
	if req.GuestCID == 0 {
		req.GuestCID = randomGuestCID()
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

	if !req.Launch {
		observation.Phase = "plan"
		planPath := filepath.Join(runDir, "plan.json")
		plan := map[string]any{
			"backend":      "firecracker",
			"mode":         "plan-only",
			"command_path": cmdPath,
		}
		if err := writeJSON(planPath, plan); err != nil {
			return nil, err
		}

		observation.PlanPath = planPath
		observation.RunDir = runDir
		return &backend.RunResult{
			RunID:       req.RunID,
			ExitCode:    0,
			LaunchedVM:  false,
			PlanPath:    planPath,
			RunDir:      runDir,
			ImageRef:    req.Policy.ImageRef,
			ImageDigest: req.Policy.ImageDigest,
			Message:     "firecracker execution plan generated; command not executed",
		}, nil
	}

	binary := req.BinaryPath
	if binary == "" {
		binary = "firecracker"
	}
	firecrackerPath, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("firecracker binary not found (%q): %w", binary, err)
	}

	if req.KernelImagePath == "" {
		return nil, errors.New("kernel_image must be configured for launched execution")
	}
	observation.Phase = "launch"

	kernelPath, err := filepath.Abs(req.KernelImagePath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(kernelPath); err != nil {
		return nil, fmt.Errorf("kernel image %s: %w", kernelPath, err)
	}

	imageArtifact, err := a.ensureImageArtifact(ctx, req.Policy.ImageRef)
	if err != nil {
		return nil, err
	}
	observation.ImageRef = imageArtifact.Ref
	observation.ImageDigest = imageArtifact.Digest
	observation.ImageCacheHit = imageArtifact.CacheHit

	preparedRootFSPath, err := a.ensurePreparedRuntimeRootFS(ctx, req.FirecrackerConfig, imageArtifact)
	if err != nil {
		return nil, err
	}

	rootfsPath, err := filepath.Abs(preparedRootFSPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(rootfsPath); err != nil {
		return nil, fmt.Errorf("rootfs %s: %w", rootfsPath, err)
	}

	vmRootFSPath := filepath.Join(runDir, "rootfs-ephemeral.ext4")
	defer os.Remove(vmRootFSPath)
	rootfsCopyStart := time.Now()
	if err := copyFile(rootfsPath, vmRootFSPath); err != nil {
		observation.RootFSCopyMS = durationMillisCeil(time.Since(rootfsCopyStart))
		return nil, fmt.Errorf("prepare per-run rootfs: %w", err)
	}
	observation.RootFSCopyMS = durationMillisCeil(time.Since(rootfsCopyStart))

	networkSetupStart := time.Now()
	networkRunCommand := func(ctx context.Context, args ...string) error {
		return runRootCommand(ctx, req.FirecrackerConfig, args...)
	}
	networkRunBatch := func(ctx context.Context, commands [][]string) error {
		return runRootCommandBatch(ctx, req.FirecrackerConfig, commands)
	}
	networkCfg, cleanupNetwork, err := setupHostNetwork(ctx, req.RunID, req.Policy.Allow, networkRunCommand, networkRunBatch)
	if err != nil {
		return nil, fmt.Errorf("setup host network: %w", err)
	}
	observation.NetworkSetupMS = time.Since(networkSetupStart).Milliseconds()
	observation.PolicyResolveMS = networkCfg.PolicyResolveMS
	observation.NetworkTap = networkCfg.TapName
	observation.NetworkGuestIP = networkCfg.GuestIP
	observation.NetworkHostIP = networkCfg.HostIP
	cleanupMeasured := func() {
		cleanupStart := time.Now()
		cleanupNetwork()
		observation.CleanupMS = time.Since(cleanupStart).Milliseconds()
	}
	defer cleanupMeasured()

	vsockPath := filepath.Join(runDir, "vsock.sock")
	fcCfg := firecrackerConfig{
		BootSource: bootSource{
			KernelImagePath: kernelPath,
			BootArgs: fmt.Sprintf(
				"console=ttyS0 reboot=k panic=1 pci=off init=/sbin/cleanroom-init random.trust_cpu=on cleanroom_guest_ip=%s cleanroom_guest_gw=%s cleanroom_guest_mask=24 cleanroom_guest_dns=1.1.1.1 cleanroom_guest_port=%d",
				networkCfg.GuestIP,
				networkCfg.HostIP,
				req.GuestPort,
			),
		},
		Drives: []drive{
			{
				DriveID:      "rootfs",
				PathOnHost:   vmRootFSPath,
				IsRootDevice: true,
				IsReadOnly:   false,
			},
		},
		MachineConfig: machineConfig{
			VCPUCount:  req.VCPUs,
			MemSizeMiB: req.MemoryMiB,
			SMT:        false,
		},
		Vsock: &vsockConfig{
			VsockID:  "cleanroom-vsock",
			GuestCID: req.GuestCID,
			UDSPath:  vsockPath,
		},
		NetworkInterfaces: []networkInterface{
			{
				IfaceID:     "eth0",
				HostDevName: networkCfg.TapName,
				GuestMac:    guestMACFromRunID(req.RunID),
			},
		},
		Entropy: &entropyConfig{},
	}

	cfgPath := filepath.Join(runDir, "firecracker-config.json")
	if err := writeJSON(cfgPath, fcCfg); err != nil {
		return nil, err
	}

	apiSocket := filepath.Join(runDir, "firecracker.sock")
	stdoutPath := filepath.Join(runDir, "firecracker.stdout.log")
	stderrPath := filepath.Join(runDir, "firecracker.stderr.log")

	stdoutFile, err := os.Create(stdoutPath)
	if err != nil {
		return nil, err
	}
	defer stdoutFile.Close()

	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		return nil, err
	}
	defer stderrFile.Close()

	launchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	fcCmd := exec.CommandContext(launchCtx, firecrackerPath, "--api-sock", apiSocket, "--config-file", cfgPath)
	fcCmd.Stdout = stdoutFile
	fcCmd.Stderr = stderrFile

	firecrackerStart := time.Now()
	if err := fcCmd.Start(); err != nil {
		observation.FirecrackerStartMS = durationMillisCeil(time.Since(firecrackerStart))
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	observation.FirecrackerStartMS = durationMillisCeil(time.Since(firecrackerStart))
	vmProcessStart := time.Now()

	processExited := make(chan struct{})
	var (
		processExitMu  sync.RWMutex
		processExitErr error
	)
	processExitErrFn := func() error {
		processExitMu.RLock()
		defer processExitMu.RUnlock()
		if processExitErr == nil {
			return errors.New("firecracker exited")
		}
		return processExitErr
	}
	go func() {
		err := fcCmd.Wait()
		processExitMu.Lock()
		processExitErr = err
		processExitMu.Unlock()
		close(processExited)
	}()
	defer stopVM(fcCmd, processExited)

	bootCtx, bootCancel := context.WithTimeout(ctx, time.Duration(req.LaunchSeconds)*time.Second)
	defer bootCancel()

	guestReq := vsockexec.ExecRequest{
		Command: req.Command,
	}
	seed := make([]byte, 64)
	if _, err := cryptorand.Read(seed); err == nil {
		guestReq.EntropySeed = seed
	}
	guestResult, guestTiming, err := runGuestCommand(bootCtx, ctx, processExited, processExitErrFn, vsockPath, req.GuestPort, guestReq, stream)
	if err != nil {
		return nil, err
	}
	vmReady := guestTiming.AgentReadyAt.Sub(vmProcessStart)
	if vmReady < 0 {
		vmReady = 0
	}
	observation.VMReadyMS = vmReady.Milliseconds()
	observation.VsockWaitMS = guestTiming.WaitForAgent.Milliseconds()
	observation.GuestExecMS = guestTiming.CommandRun.Milliseconds()
	if guestResult.Error != "" && strings.TrimSpace(guestResult.Stderr) == "" {
		guestResult.Stderr = guestResult.Error + "\n"
	}

	message := runResultMessage("firecracker launch and guest command execution complete")
	if guestResult.Error != "" {
		message = runResultMessage("firecracker launch and guest command execution completed with guest-side error detail: " + guestResult.Error)
	}

	observation.PlanPath = cfgPath
	observation.RunDir = runDir
	observation.ExitCode = guestResult.ExitCode
	observation.GuestError = guestResult.Error

	timingSummary := fmt.Sprintf("timings boot=%s vsock_wait=%s exec=%s", vmReady, guestTiming.WaitForAgent, guestTiming.CommandRun)

	return &backend.RunResult{
		RunID:       req.RunID,
		ExitCode:    guestResult.ExitCode,
		LaunchedVM:  true,
		PlanPath:    cfgPath,
		RunDir:      runDir,
		ImageRef:    imageArtifact.Ref,
		ImageDigest: imageArtifact.Digest,
		Message:     message + "; " + timingSummary,
		Stdout:      guestResult.Stdout,
		Stderr:      guestResult.Stderr,
	}, nil
}

type firecrackerRunObservation struct {
	RunID              string `json:"run_id"`
	Backend            string `json:"backend"`
	LaunchedVM         bool   `json:"launched_vm"`
	ImageRef           string `json:"image_ref,omitempty"`
	ImageDigest        string `json:"image_digest,omitempty"`
	ImageCacheHit      bool   `json:"image_cache_hit,omitempty"`
	Phase              string `json:"phase"`
	PlanPath           string `json:"plan_path,omitempty"`
	RunDir             string `json:"run_dir,omitempty"`
	ExitCode           int    `json:"exit_code,omitempty"`
	GuestError         string `json:"guest_error,omitempty"`
	NetworkTap         string `json:"network_tap,omitempty"`
	NetworkHostIP      string `json:"network_host_ip,omitempty"`
	NetworkGuestIP     string `json:"network_guest_ip,omitempty"`
	PolicyResolveMS    int64  `json:"policy_resolve_ms,omitempty"`
	RootFSCopyMS       int64  `json:"rootfs_copy_ms,omitempty"`
	FirecrackerStartMS int64  `json:"firecracker_start_ms,omitempty"`
	NetworkSetupMS     int64  `json:"network_setup_ms,omitempty"`
	VMReadyMS          int64  `json:"vm_ready_ms,omitempty"`
	VsockWaitMS        int64  `json:"vsock_wait_ms,omitempty"`
	GuestExecMS        int64  `json:"guest_exec_ms,omitempty"`
	CleanupMS          int64  `json:"cleanup_ms,omitempty"`
	TotalMS            int64  `json:"total_ms,omitempty"`
}

type firecrackerConfig struct {
	BootSource        bootSource         `json:"boot-source"`
	Drives            []drive            `json:"drives"`
	MachineConfig     machineConfig      `json:"machine-config"`
	Vsock             *vsockConfig       `json:"vsock,omitempty"`
	NetworkInterfaces []networkInterface `json:"network-interfaces,omitempty"`
	Entropy           *entropyConfig     `json:"entropy,omitempty"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type machineConfig struct {
	VCPUCount  int64 `json:"vcpu_count"`
	MemSizeMiB int64 `json:"mem_size_mib"`
	SMT        bool  `json:"smt"`
}

type vsockConfig struct {
	VsockID  string `json:"vsock_id"`
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type networkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMac    string `json:"guest_mac,omitempty"`
}

type entropyConfig struct{}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
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
	if err := out.Chmod(info.Mode().Perm()); err != nil {
		return err
	}

	if !tryCloneFile(out, in) {
		if _, err := in.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := out.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			return err
		}
	}
	return out.Sync()
}

type imageArtifact struct {
	Ref        string
	Digest     string
	RootFSPath string
	CacheHit   bool
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

func (a *Adapter) ensurePreparedRuntimeRootFS(ctx context.Context, cfg backend.FirecrackerConfig, image imageArtifact) (string, error) {
	sourcePath := strings.TrimSpace(image.RootFSPath)
	if sourcePath == "" {
		return "", errors.New("resolved image rootfs path is empty")
	}
	if _, err := os.Stat(sourcePath); err != nil {
		return "", fmt.Errorf("resolved image rootfs %q: %w", sourcePath, err)
	}

	guestAgentPath, guestAgentHash, err := a.getGuestAgentBinary()
	if err != nil {
		return "", err
	}

	preparedPath, err := preparedRuntimeRootFSPath(image.Digest, guestAgentHash)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(preparedPath); err == nil {
		return preparedPath, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect prepared runtime rootfs %q: %w", preparedPath, err)
	}

	a.runtimeImageMu.Lock()
	defer a.runtimeImageMu.Unlock()

	if _, err := os.Stat(preparedPath); err == nil {
		return preparedPath, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect prepared runtime rootfs %q: %w", preparedPath, err)
	}

	preparedDir := filepath.Dir(preparedPath)
	if err := os.MkdirAll(preparedDir, 0o755); err != nil {
		return "", fmt.Errorf("create prepared rootfs cache directory %q: %w", preparedDir, err)
	}

	tmpPath := preparedPath + fmt.Sprintf(".tmp-%d", time.Now().UnixNano())
	if err := copyFile(sourcePath, tmpPath); err != nil {
		return "", fmt.Errorf("copy rootfs image for runtime preparation: %w", err)
	}
	if err := a.installGuestRuntimeIntoRootFS(ctx, cfg, tmpPath, guestAgentPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, preparedPath); err != nil {
		_ = os.Remove(tmpPath)
		if _, statErr := os.Stat(preparedPath); statErr == nil {
			return preparedPath, nil
		}
		return "", fmt.Errorf("store prepared runtime rootfs %q: %w", preparedPath, err)
	}
	return preparedPath, nil
}

func preparedRuntimeRootFSPath(imageDigest, guestAgentHash string) (string, error) {
	cacheBase, err := paths.CacheBaseDir()
	if err != nil {
		return "", fmt.Errorf("resolve cache base directory: %w", err)
	}
	key := runtimeRootFSCacheKey(imageDigest, guestAgentHash)
	return filepath.Join(cacheBase, "firecracker", "runtime-rootfs", key+".ext4"), nil
}

func runtimeRootFSCacheKey(imageDigest, guestAgentHash string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(imageDigest) + "|" + guestAgentHash + "|" + preparedRuntimeRootFSVersion + "|" + guestInitScriptTemplate))
	return hex.EncodeToString(sum[:])
}

func (a *Adapter) installGuestRuntimeIntoRootFS(ctx context.Context, cfg backend.FirecrackerConfig, rootFSPath, guestAgentPath string) error {
	// Keep mount points in /tmp so helper-mode path allowlisting stays stable
	// regardless of TMPDIR environment overrides.
	mountDir, err := os.MkdirTemp("/tmp", "cleanroom-firecracker-rootfs-*")
	if err != nil {
		return fmt.Errorf("create temporary rootfs mount directory: %w", err)
	}
	defer os.RemoveAll(mountDir)

	if err := runRootCommand(ctx, cfg, "mount", "-o", "loop", rootFSPath, mountDir); err != nil {
		return fmt.Errorf("mount rootfs image for runtime preparation: %w", err)
	}
	mounted := true
	defer func() {
		if mounted {
			_ = runRootCommand(context.Background(), cfg, "umount", mountDir)
		}
	}()

	initScriptPath, err := createGuestInitScript()
	if err != nil {
		return err
	}
	defer os.Remove(initScriptPath)

	if err := runRootCommand(ctx, cfg, "mkdir", "-p", filepath.Join(mountDir, "usr/local/bin"), filepath.Join(mountDir, "sbin")); err != nil {
		return fmt.Errorf("prepare runtime directories in mounted rootfs: %w", err)
	}
	if err := runRootCommand(ctx, cfg, "install", "-m", "0755", guestAgentPath, filepath.Join(mountDir, "usr/local/bin/cleanroom-guest-agent")); err != nil {
		return fmt.Errorf("install guest agent into mounted rootfs: %w", err)
	}
	if err := runRootCommand(ctx, cfg, "install", "-m", "0755", initScriptPath, filepath.Join(mountDir, "sbin/cleanroom-init")); err != nil {
		return fmt.Errorf("install cleanroom init into mounted rootfs: %w", err)
	}
	if err := runRootCommand(ctx, cfg, "umount", mountDir); err != nil {
		return fmt.Errorf("unmount prepared rootfs image: %w", err)
	}
	mounted = false
	return nil
}

func createGuestInitScript() (string, error) {
	f, err := os.CreateTemp("", "cleanroom-init-*.sh")
	if err != nil {
		return "", fmt.Errorf("create guest init script: %w", err)
	}
	if _, err := f.WriteString(guestInitScriptTemplate); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write guest init script: %w", err)
	}
	if err := f.Chmod(0o755); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("chmod guest init script: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("close guest init script: %w", err)
	}
	return f.Name(), nil
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
	if p, err := exec.LookPath("cleanroom-guest-agent"); err == nil {
		return p, nil
	}
	self, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(self), "cleanroom-guest-agent")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("cleanroom-guest-agent binary not found in PATH; run `mise run install` first")
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

func (a *Adapter) launchSandboxVM(ctx context.Context, sandboxID string, compiled *policy.CompiledPolicy, cfg backend.FirecrackerConfig) (*sandboxInstance, error) {
	if compiled == nil {
		return nil, errors.New("missing compiled policy")
	}
	if compiled.NetworkDefault != "deny" {
		return nil, fmt.Errorf("firecracker backend requires deny-by-default policy, got %q", compiled.NetworkDefault)
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("firecracker backend is linux-only, current OS is %s", runtime.GOOS)
	}

	if cfg.VCPUs <= 0 {
		cfg.VCPUs = 1
	}
	if cfg.MemoryMiB <= 0 {
		cfg.MemoryMiB = 512
	}
	if cfg.GuestCID == 0 {
		cfg.GuestCID = randomGuestCID()
	}
	if cfg.GuestPort == 0 {
		cfg.GuestPort = vsockexec.DefaultPort
	}
	if cfg.LaunchSeconds <= 0 {
		cfg.LaunchSeconds = 30
	}

	binary := cfg.BinaryPath
	if binary == "" {
		binary = "firecracker"
	}
	firecrackerPath, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("firecracker binary not found (%q): %w", binary, err)
	}
	if cfg.KernelImagePath == "" {
		return nil, errors.New("kernel_image must be configured for launched execution")
	}
	kernelPath, err := filepath.Abs(cfg.KernelImagePath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(kernelPath); err != nil {
		return nil, fmt.Errorf("kernel image %s: %w", kernelPath, err)
	}

	imageArtifact, err := a.ensureImageArtifact(ctx, compiled.ImageRef)
	if err != nil {
		return nil, err
	}
	preparedRootFSPath, err := a.ensurePreparedRuntimeRootFS(ctx, cfg, imageArtifact)
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
		return nil, err
	}

	rootfsPath, err := filepath.Abs(preparedRootFSPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(rootfsPath); err != nil {
		return nil, fmt.Errorf("rootfs %s: %w", rootfsPath, err)
	}

	vmRootFSPath := filepath.Join(runDir, "rootfs-persistent.ext4")
	if err := copyFile(rootfsPath, vmRootFSPath); err != nil {
		return nil, fmt.Errorf("prepare persistent rootfs: %w", err)
	}

	networkRunCommand := func(ctx context.Context, args ...string) error {
		return runRootCommand(ctx, cfg, args...)
	}
	networkRunBatch := func(ctx context.Context, commands [][]string) error {
		return runRootCommandBatch(ctx, cfg, commands)
	}
	networkCfg, cleanupNetwork, err := setupHostNetwork(ctx, sandboxID, compiled.Allow, networkRunCommand, networkRunBatch)
	if err != nil {
		_ = os.Remove(vmRootFSPath)
		return nil, fmt.Errorf("setup host network: %w", err)
	}

	cleanupAll := func() {
		cleanupNetwork()
		_ = os.Remove(vmRootFSPath)
	}

	vsockPath := filepath.Join(runDir, "vsock.sock")
	fcCfg := firecrackerConfig{
		BootSource: bootSource{
			KernelImagePath: kernelPath,
			BootArgs: fmt.Sprintf(
				"console=ttyS0 reboot=k panic=1 pci=off init=/sbin/cleanroom-init random.trust_cpu=on cleanroom_guest_ip=%s cleanroom_guest_gw=%s cleanroom_guest_mask=24 cleanroom_guest_dns=1.1.1.1 cleanroom_guest_port=%d",
				networkCfg.GuestIP,
				networkCfg.HostIP,
				cfg.GuestPort,
			),
		},
		Drives: []drive{{
			DriveID:      "rootfs",
			PathOnHost:   vmRootFSPath,
			IsRootDevice: true,
			IsReadOnly:   false,
		}},
		MachineConfig: machineConfig{
			VCPUCount:  cfg.VCPUs,
			MemSizeMiB: cfg.MemoryMiB,
			SMT:        false,
		},
		Vsock: &vsockConfig{
			VsockID:  "cleanroom-vsock",
			GuestCID: cfg.GuestCID,
			UDSPath:  vsockPath,
		},
		NetworkInterfaces: []networkInterface{{
			IfaceID:     "eth0",
			HostDevName: networkCfg.TapName,
			GuestMac:    guestMACFromRunID(sandboxID),
		}},
		Entropy: &entropyConfig{},
	}

	configPath := filepath.Join(runDir, "firecracker-config.json")
	if err := writeJSON(configPath, fcCfg); err != nil {
		cleanupAll()
		return nil, err
	}

	apiSocket := filepath.Join(runDir, "firecracker.sock")
	stdoutPath := filepath.Join(runDir, "firecracker.stdout.log")
	stderrPath := filepath.Join(runDir, "firecracker.stderr.log")
	stdoutFile, err := os.Create(stdoutPath)
	if err != nil {
		cleanupAll()
		return nil, err
	}
	defer stdoutFile.Close()
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		cleanupAll()
		return nil, err
	}
	defer stderrFile.Close()

	fcCmd := exec.Command(firecrackerPath, "--api-sock", apiSocket, "--config-file", configPath)
	fcCmd.Stdout = stdoutFile
	fcCmd.Stderr = stderrFile
	if err := fcCmd.Start(); err != nil {
		cleanupAll()
		return nil, fmt.Errorf("start firecracker: %w", err)
	}

	instance := &sandboxInstance{
		SandboxID:      sandboxID,
		RunDir:         runDir,
		ConfigPath:     configPath,
		VsockPath:      vsockPath,
		GuestPort:      cfg.GuestPort,
		Policy:         compiled,
		ImageRef:       imageArtifact.Ref,
		ImageDigest:    imageArtifact.Digest,
		CommandTimeout: cfg.LaunchSeconds,
		fcCmd:          fcCmd,
		exitedCh:       make(chan struct{}),
		cleanupNetwork: cleanupNetwork,
		vmRootFSPath:   vmRootFSPath,
	}
	go func() {
		err := fcCmd.Wait()
		instance.setExited(err)
		close(instance.exitedCh)
	}()

	bootCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.LaunchSeconds)*time.Second)
	defer cancel()
	conn, err := dialVsockUntilReady(bootCtx, instance.exitedCh, instance.exitedErrOrNil, vsockPath, cfg.GuestPort)
	if err != nil {
		stopVM(fcCmd, instance.exitedCh)
		cleanupAll()
		return nil, err
	}
	_ = conn.Close()
	cleanupRunDir = false
	return instance, nil
}

func sandboxRuntimeBaseDir() (string, error) {
	base, err := paths.StateBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "sandboxes"), nil
}

func (s *sandboxInstance) shutdown() {
	if s == nil {
		return
	}
	stopVM(s.fcCmd, s.exitedCh)
	if s.cleanupNetwork != nil {
		s.cleanupNetwork()
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
		return errors.New("vm exited")
	}
	return s.exitErr
}

func runResultMessage(base string) string {
	return base + "; rootfs writes discarded after run"
}

type guestExecTiming struct {
	WaitForAgent time.Duration
	AgentReadyAt time.Time
	CommandRun   time.Duration
}

func runGuestCommand(bootCtx context.Context, execCtx context.Context, processExited <-chan struct{}, processExitErr func() error, vsockPath string, guestPort uint32, req vsockexec.ExecRequest, stream backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
	waitStart := time.Now()
	conn, err := dialVsockUntilReady(bootCtx, processExited, processExitErr, vsockPath, guestPort)
	if err != nil {
		return vsockexec.ExecResponse{}, guestExecTiming{}, err
	}
	readyAt := time.Now()
	timing := guestExecTiming{
		WaitForAgent: readyAt.Sub(waitStart),
		AgentReadyAt: readyAt,
	}
	defer conn.Close()
	if dl, ok := execCtx.Deadline(); ok {
		if deadlineConn, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
			if err := deadlineConn.SetDeadline(dl); err != nil {
				return vsockexec.ExecResponse{}, guestExecTiming{}, fmt.Errorf("set vsock deadline: %w", err)
			}
		}
	}
	// Ensure blocked reads/writes are interrupted when context is canceled.
	go func() {
		<-execCtx.Done()
		_ = conn.Close()
	}()

	if err := vsockexec.EncodeRequest(conn, req); err != nil {
		return vsockexec.ExecResponse{}, guestExecTiming{}, fmt.Errorf("send guest exec request: %w", err)
	}

	commandStart := time.Now()
	res, err := vsockexec.DecodeStreamResponse(conn, vsockexec.StreamCallbacks{
		OnStdout: stream.OnStdout,
		OnStderr: stream.OnStderr,
	})
	if err != nil {
		if ctxErr := execCtx.Err(); ctxErr != nil {
			return vsockexec.ExecResponse{}, guestExecTiming{}, fmt.Errorf("guest exec canceled while waiting for response: %w", ctxErr)
		}
		return vsockexec.ExecResponse{}, guestExecTiming{}, fmt.Errorf("decode guest exec response: %w", err)
	}
	timing.CommandRun = time.Since(commandStart)
	return res, timing, nil
}

type callbackWriter struct {
	cb func([]byte)
}

func (w callbackWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if w.cb != nil {
		w.cb(append([]byte(nil), p...))
	}
	return len(p), nil
}

func dialVsockUntilReady(ctx context.Context, processExited <-chan struct{}, processExitErr func() error, vsockPath string, guestPort uint32) (io.ReadWriteCloser, error) {
	ticker := time.NewTicker(vsockDialRetryInterval)
	defer ticker.Stop()

	for {
		conn, err := fcvsock.DialContext(ctx, vsockPath, guestPort)
		if err == nil {
			return conn, nil
		}

		select {
		case <-processExited:
			waitErr := processExitErr()
			if waitErr == nil {
				return nil, errors.New("firecracker exited before vsock guest agent became ready")
			}
			return nil, fmt.Errorf("firecracker exited before vsock guest agent became ready: %w", waitErr)
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for vsock guest agent (%s): %w", vsockPath, ctx.Err())
		case <-ticker.C:
		}
	}
}

func stopVM(fcCmd *exec.Cmd, processExited <-chan struct{}) {
	if fcCmd == nil {
		return
	}
	if fcCmd.Process != nil {
		_ = fcCmd.Process.Kill()
	}
	select {
	case <-processExited:
	case <-time.After(2 * time.Second):
	}
}

type hostNetworkConfig struct {
	TapName         string
	HostIP          string
	GuestIP         string
	PolicyResolveMS int64
}

type iptablesForwardRule struct {
	Protocol string
	DestIP   string
	DestPort int
}

type ipLookupFunc func(ctx context.Context, host string) ([]net.IP, error)
type rootCommandFunc func(ctx context.Context, args ...string) error
type rootCommandBatchFunc func(ctx context.Context, commands [][]string) error

func setupHostNetwork(ctx context.Context, runID string, allow []policy.AllowRule, runCommand rootCommandFunc, runBatchCommand rootCommandBatchFunc) (hostNetworkConfig, func(), error) {
	lookup := func(ctx context.Context, host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip4", host)
	}
	return setupHostNetworkWithDeps(ctx, runID, allow, lookup, runCommand, runBatchCommand)
}

func setupHostNetworkWithDeps(ctx context.Context, runID string, allow []policy.AllowRule, lookup ipLookupFunc, runCommand rootCommandFunc, runBatchCommand rootCommandBatchFunc) (hostNetworkConfig, func(), error) {
	tapName := tapNameFromRunID(runID)
	hostIP, guestIP := hostGuestIPs(runID)
	hostCIDR := hostIP + "/24"
	guestCIDR := guestIP + "/32"
	const dnsServer = "1.1.1.1"

	if runBatchCommand == nil {
		runBatchCommand = func(ctx context.Context, commands [][]string) error {
			for _, args := range commands {
				_ = runCommand(ctx, args...)
			}
			return nil
		}
	}

	policyResolveStart := time.Now()
	forwardRules, err := resolveForwardRulesWithLookup(ctx, allow, lookup)
	policyResolveMS := durationMillisCeil(time.Since(policyResolveStart))
	if err != nil {
		return hostNetworkConfig{}, func() {}, err
	}

	setupRun := func(args ...string) error {
		return runCommand(ctx, args...)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	cleanupCmds := make([][]string, 0, 16)
	cleanup := func() {
		defer cleanupCancel()
		reversed := make([][]string, 0, len(cleanupCmds))
		for i := len(cleanupCmds) - 1; i >= 0; i-- {
			reversed = append(reversed, cleanupCmds[i])
		}
		_ = runBatchCommand(cleanupCtx, reversed)
	}
	addCleanup := func(args ...string) {
		cleanupCmds = append(cleanupCmds, append([]string(nil), args...))
	}

	if err := setupRun("ip", "tuntap", "add", "dev", tapName, "mode", "tap", "user", strconv.Itoa(os.Getuid())); err != nil {
		return hostNetworkConfig{}, func() {}, fmt.Errorf("create tap device %s: %w", tapName, err)
	}
	addCleanup("ip", "link", "del", tapName)
	if err := setupRun("ip", "addr", "add", hostCIDR, "dev", tapName); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("assign host ip to %s: %w", tapName, err)
	}
	if err := setupRun("ip", "link", "set", "dev", tapName, "up"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("bring tap %s up: %w", tapName, err)
	}
	if err := setupRun("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("enable ipv4 forwarding: %w", err)
	}
	if err := setupRun("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", guestCIDR, "-j", "MASQUERADE"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("install nat rule for %s: %w", guestCIDR, err)
	}
	addCleanup("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", guestCIDR, "-j", "MASQUERADE")
	returnPathCleanup, err := installForwardReturnPathRule(setupRun, tapName)
	if err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("install forward return-path rule for %s: %w", tapName, err)
	}
	addCleanup(returnPathCleanup...)

	// Allow guest DNS to the configured resolver so host-based policy entries remain usable.
	if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-p", "udp", "-d", dnsServer, "--dport", "53", "-j", "ACCEPT"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("install dns udp rule for %s: %w", tapName, err)
	}
	addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-p", "udp", "-d", dnsServer, "--dport", "53", "-j", "ACCEPT")
	if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-p", "tcp", "-d", dnsServer, "--dport", "53", "-j", "ACCEPT"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("install dns tcp rule for %s: %w", tapName, err)
	}
	addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-p", "tcp", "-d", dnsServer, "--dport", "53", "-j", "ACCEPT")

	for _, rule := range forwardRules {
		port := strconv.Itoa(rule.DestPort)
		if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-p", rule.Protocol, "-d", rule.DestIP, "--dport", port, "-j", "ACCEPT"); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install allow rule %s %s:%d: %w", rule.Protocol, rule.DestIP, rule.DestPort, err)
		}
		addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-p", rule.Protocol, "-d", rule.DestIP, "--dport", port, "-j", "ACCEPT")
	}
	if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-j", "DROP"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("install default deny forward rule for %s: %w", tapName, err)
	}
	addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-j", "DROP")

	return hostNetworkConfig{
		TapName:         tapName,
		HostIP:          hostIP,
		GuestIP:         guestIP,
		PolicyResolveMS: policyResolveMS,
	}, cleanup, nil
}

func installForwardReturnPathRule(setupRun func(args ...string) error, tapName string) ([]string, error) {
	conntrackAdd := []string{"iptables", "-A", "FORWARD", "-o", tapName, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
	if err := setupRun(conntrackAdd...); err == nil {
		return []string{"iptables", "-D", "FORWARD", "-o", tapName, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}, nil
	}

	stateAdd := []string{"iptables", "-A", "FORWARD", "-o", tapName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
	if err := setupRun(stateAdd...); err != nil {
		return nil, err
	}
	return []string{"iptables", "-D", "FORWARD", "-o", tapName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"}, nil
}

func resolveForwardRules(ctx context.Context, allow []policy.AllowRule) ([]iptablesForwardRule, error) {
	lookup := func(ctx context.Context, host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip4", host)
	}
	return resolveForwardRulesWithLookup(ctx, allow, lookup)
}

func resolveForwardRulesWithLookup(ctx context.Context, allow []policy.AllowRule, lookup ipLookupFunc) ([]iptablesForwardRule, error) {
	rules := make([]iptablesForwardRule, 0, len(allow)*2)
	seen := map[string]struct{}{}
	for _, entry := range allow {
		ips, err := lookup(ctx, entry.Host)
		if err != nil {
			return nil, fmt.Errorf("resolve policy host %q: %w", entry.Host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("resolve policy host %q: no ipv4 addresses", entry.Host)
		}
		for _, ip := range ips {
			ipv4 := ip.To4()
			if ipv4 == nil {
				continue
			}
			ipStr := ipv4.String()
			for _, port := range entry.Ports {
				for _, proto := range []string{"tcp", "udp"} {
					key := fmt.Sprintf("%s|%s|%d", proto, ipStr, port)
					if _, ok := seen[key]; ok {
						continue
					}
					seen[key] = struct{}{}
					rules = append(rules, iptablesForwardRule{
						Protocol: proto,
						DestIP:   ipStr,
						DestPort: port,
					})
				}
			}
		}
	}
	return rules, nil
}

func resolvePrivilegedExecution(cfg backend.FirecrackerConfig) (mode string, helperPath string) {
	mode = strings.ToLower(strings.TrimSpace(cfg.PrivilegedMode))
	if mode == "" {
		mode = privilegedModeSudo
	}
	helperPath = strings.TrimSpace(cfg.PrivilegedHelperPath)
	if helperPath == "" {
		helperPath = defaultPrivilegedHelperPath
	}
	return mode, helperPath
}

func runRootCommand(ctx context.Context, cfg backend.FirecrackerConfig, args ...string) error {
	if len(args) == 0 {
		return errors.New("missing privileged command")
	}

	mode, helperPath := resolvePrivilegedExecution(cfg)
	switch mode {
	case privilegedModeSudo:
		return runCombinedCommand(ctx, append([]string{"sudo", "-n"}, args...), args)
	case privilegedModeHelper:
		if strings.TrimSpace(helperPath) == "" {
			return errors.New("privileged helper mode requires helper path")
		}
		return runCombinedCommand(ctx, append([]string{"sudo", "-n", helperPath}, args...), append([]string{"helper"}, args...))
	default:
		return fmt.Errorf("unsupported privileged command mode %q", mode)
	}
}

func runRootCommandBatch(ctx context.Context, cfg backend.FirecrackerConfig, commands [][]string) error {
	for _, args := range commands {
		if len(args) == 0 {
			continue
		}
		_ = runRootCommand(ctx, cfg, args...)
	}
	return nil
}

func runCombinedCommand(ctx context.Context, command []string, errorContext []string) error {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = "no stderr output"
		}
		return fmt.Errorf("%s: %w (%s)", strings.Join(errorContext, " "), err, msg)
	}
	return nil
}

func durationMillisCeil(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	ms := d.Milliseconds()
	if ms == 0 {
		return 1
	}
	return ms
}

func randomGuestCID() uint32 {
	var buf [4]byte
	if _, err := cryptorand.Read(buf[:]); err != nil {
		return 3
	}
	// Valid vsock CID range: 3 to 2^32-2 (0xFFFFFFFE).
	cid := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])
	return cid%(0xFFFFFFFE-3) + 3
}

func tapNameFromRunID(runID string) string {
	filtered := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, runID)
	filtered = strings.ToLower(filtered)
	if len(filtered) > 13 {
		filtered = filtered[len(filtered)-13:]
	}
	if filtered == "" {
		filtered = "cleanroomtap"
	}
	return "cr" + filtered
}

func hostGuestIPs(runID string) (string, string) {
	sum := sha1.Sum([]byte(runID))
	o2 := int(sum[0])
	o3 := int(sum[1])
	if o2 == 0 {
		o2 = 1
	}
	if o3 == 0 {
		o3 = 1
	}
	hostIP := fmt.Sprintf("10.%d.%d.1", o2, o3)
	guestIP := fmt.Sprintf("10.%d.%d.2", o2, o3)
	return hostIP, guestIP
}

func guestMACFromRunID(runID string) string {
	sum := sha1.Sum([]byte(runID))
	return fmt.Sprintf("02:fc:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3])
}
