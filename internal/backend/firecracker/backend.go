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
	"log"
	"math"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/bootassets"
	"github.com/buildkite/cleanroom/internal/ext4edit"
	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/hosttools"
	"github.com/buildkite/cleanroom/internal/imagemgr"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/volumestore"
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

	sandboxMu                   sync.Mutex
	sandboxes                   map[string]*sandboxInstance
	provisioning                map[string]struct{}
	launchSandboxVMFn           func(context.Context, string, *policy.CompiledPolicy, backend.FirecrackerConfig) (*sandboxInstance, error)
	launchSandboxVMFromRootFSFn func(context.Context, string, *policy.CompiledPolicy, backend.FirecrackerConfig, string) (*sandboxInstance, error)
	runGuestCommandFn           func(context.Context, context.Context, <-chan struct{}, func() error, string, uint32, vsockexec.ExecRequest, backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error)

	GatewayRegistry gatewayRegistry
	GatewayPort     int
}

// gatewayRegistry is the subset of gateway.Registry used by the adapter.
type gatewayRegistry interface {
	Register(guestIP, sandboxID string, p *policy.CompiledPolicy) error
	Release(guestIP string)
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
	HostIP         string
	GuestIP        string
	fcCmd          *exec.Cmd
	exitedCh       chan struct{}
	exitMu         sync.RWMutex
	exitErr        error
	exitReady      bool
	cleanupNetwork func()
	cleanupVolume  func()
	vmRootFSPath   string
	volumeRef      string

	warnings backend.WarningEmitter
}

const runObservabilityFile = "execution-observability.json"
const vsockDialRetryInterval = 50 * time.Millisecond
const preparedRuntimeRootFSVersion = "v2-debugfs"
const defaultPrivilegedHelperPath = "/usr/local/sbin/cleanroom-root-helper"
const helperCapabilityFirecrackerNetwork = "firecracker-network"
const helperCapabilityFirecrackerTrustedDNS = "firecracker-trusted-dns"
const helperCapabilityFirecrackerZFS = "firecracker-zfs"
const defaultDownloadMaxBytes int64 = 10 * 1024 * 1024
const snapshotSyncTimeoutSeconds = 10
const networkCleanupTimeout = 5 * time.Second
const guestInitScriptPathSbin = "/sbin/cleanroom-init"
const guestInitScriptPathUsrSbin = "/usr/sbin/cleanroom-init"

var tapDeleteRetryInterval = 100 * time.Millisecond

var sendProcessSignal = func(proc *os.Process, sig syscall.Signal) error {
	if proc == nil {
		return errors.New("missing process")
	}
	return proc.Signal(sig)
}

var syncHostFilesystem = defaultSyncHostFilesystem

var rootFSVolumeStoreDriverFn = rootFSVolumeStoreDriver

var snapshotVolumeStoreDriverFn = snapshotVolumeStoreDriver

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

DOCKER_REQUIRED="$(arg_value cleanroom_service_docker_required || true)"
if [ "$DOCKER_REQUIRED" = "1" ] && command -v dockerd >/dev/null 2>&1; then
  DOCKER_STARTUP_TIMEOUT="$(arg_value cleanroom_service_docker_startup_timeout || true)"
  case "$DOCKER_STARTUP_TIMEOUT" in
    ''|*[!0-9]*) DOCKER_STARTUP_TIMEOUT="20" ;;
  esac
  if [ "$DOCKER_STARTUP_TIMEOUT" -le 0 ]; then
    DOCKER_STARTUP_TIMEOUT="20"
  fi
  DOCKER_STORAGE_DRIVER="$(arg_value cleanroom_service_docker_storage_driver || true)"
  if [ -z "$DOCKER_STORAGE_DRIVER" ]; then
    DOCKER_STORAGE_DRIVER="vfs"
  fi
  DOCKER_IPTABLES="$(arg_value cleanroom_service_docker_iptables || true)"

  DOCKER_ARGS="--host=unix:///var/run/docker.sock --storage-driver=$DOCKER_STORAGE_DRIVER"
  if [ "$DOCKER_IPTABLES" = "0" ] || [ "$DOCKER_IPTABLES" = "false" ]; then
    DOCKER_ARGS="$DOCKER_ARGS --iptables=false"
  fi

  mkdir -p /var/log /var/lib/docker /etc/docker /var/run /sys/fs/cgroup
  mount -t cgroup2 none /sys/fs/cgroup 2>/dev/null || true
  if [ ! -S /var/run/docker.sock ]; then
    dockerd $DOCKER_ARGS >/var/log/dockerd.log 2>&1 &
  fi
  i=0
  DOCKER_WAIT_TICKS=$((DOCKER_STARTUP_TIMEOUT * 10))
  while [ "$i" -lt "$DOCKER_WAIT_TICKS" ]; do
    if [ -S /var/run/docker.sock ]; then
      if command -v docker >/dev/null 2>&1; then
        if docker version >/dev/null 2>&1; then
          break
        fi
      else
        break
      fi
    fi
    sleep 0.1
    i=$((i + 1))
  done
fi

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

func (a *Adapter) Capabilities() map[string]bool {
	return map[string]bool{
		backend.CapabilityNetworkDefaultDeny:     true,
		backend.CapabilityNetworkAllowlistEgress: true,
		backend.CapabilityDNSControlOrEquivalent: true,
		backend.CapabilityNetworkGuestInterface:  true,
	}
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

	runStart := time.Now()
	runDir := strings.TrimSpace(req.RunDir)
	if runDir == "" {
		if baseDir, err := paths.ExecutionBaseDir(); err == nil {
			runDir = filepath.Join(baseDir, req.ExecutionID)
		}
	}
	observation := firecrackerRunObservation{
		ExecutionID: req.ExecutionID,
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

	if stream.OnWarning != nil {
		instance.warnings.SetHandler(stream.OnWarning)
		defer instance.warnings.SetHandler(nil)
	}

	guestResult, timing, err := a.executeInSandbox(ctx, instance, req.LaunchSeconds, req.Command, req.Env, req.TTY, stream)
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

	return &backend.ExecutionResult{
		ExecutionID: req.ExecutionID,
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
	if maxBytes == math.MaxInt64 {
		limit = maxBytes
	}
	cmd := []string{"head", "-c", strconv.FormatInt(limit, 10), "--", path}
	result, _, err := a.executeInSandbox(ctx, instance, 0, cmd, nil, false, backend.OutputStream{OnStdout: func(chunk []byte) {
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
	if len(data) == 0 && result.Stdout != "" {
		data = []byte(result.Stdout)
	}
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

	if a.GatewayRegistry != nil && instance.GuestIP != "" {
		a.GatewayRegistry.Release(instance.GuestIP)
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

	syncResp, _, err := a.executeInSandbox(ctx, instance, snapshotSyncTimeoutSeconds, []string{"sync"}, nil, false, backend.OutputStream{})
	if err != nil {
		return nil, fmt.Errorf("sync sandbox filesystem before snapshot: %w", err)
	}
	if syncResp.ExitCode != 0 {
		guestErr := strings.TrimSpace(syncResp.Error)
		if guestErr != "" {
			return nil, fmt.Errorf("sync sandbox filesystem before snapshot: guest sync command exited with code %d: %s", syncResp.ExitCode, guestErr)
		}
		return nil, fmt.Errorf("sync sandbox filesystem before snapshot: guest sync command exited with code %d", syncResp.ExitCode)
	}

	volumeRef := snapshotVolumeRef(instance)
	if volumeRef == "" {
		return nil, fmt.Errorf("sandbox %q has no snapshot-capable rootfs volume", sandboxID)
	}
	driverCfg, err := snapshotConfigForStorageRef(req.FirecrackerConfig, volumeRef)
	if err != nil {
		return nil, err
	}
	driver, err := snapshotVolumeStoreDriverFn(driverCfg)
	if err != nil {
		return nil, err
	}
	if err := pauseSandboxProcess(instance); err != nil {
		return nil, err
	}
	snapshotStorageRef := ""
	paused := true
	defer func() {
		if !paused {
			return
		}
		if err := resumeSandboxProcess(instance); err != nil && retErr == nil {
			if strings.TrimSpace(snapshotStorageRef) != "" {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				cleanupErr := driver.DestroySnapshot(cleanupCtx, volumestore.DestroySnapshotRequest{SnapshotRef: snapshotStorageRef})
				cancel()
				if cleanupErr != nil {
					result = nil
					retErr = fmt.Errorf("resume firecracker sandbox after snapshot: %w (cleanup snapshot %q failed: %v)", err, snapshotStorageRef, cleanupErr)
					return
				}
			}
			result = nil
			retErr = fmt.Errorf("resume firecracker sandbox after snapshot: %w", err)
		}
	}()
	if err := flushSnapshotHostFilesystem(ctx, driverCfg.Snapshots.Driver); err != nil {
		return nil, err
	}

	snapshot, err := driver.SnapshotVolume(ctx, volumestore.SnapshotVolumeRequest{
		SnapshotID: snapshotID,
		VolumeRef:  volumeRef,
	})
	if err != nil {
		return nil, fmt.Errorf("persist snapshot rootfs: %w", err)
	}
	snapshotStorageRef = snapshot.StorageRef

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

	launch := a.launchSandboxVMFromRootFSFn
	if launch == nil {
		launch = a.launchSandboxVMFromRootFS
	}
	instance, err := launch(ctx, sandboxID, req.Policy, req.FirecrackerConfig, storageRef)
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

func (a *Adapter) DeleteSnapshot(ctx context.Context, req backend.DeleteSnapshotRequest) error {
	storageRef := strings.TrimSpace(req.StorageRef)
	if storageRef == "" {
		return errors.New("missing snapshot storage_ref")
	}
	driverCfg, err := snapshotConfigForStorageRef(req.FirecrackerConfig, storageRef)
	if err != nil {
		return err
	}
	driver, err := snapshotVolumeStoreDriver(driverCfg)
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

func (a *Adapter) executeInSandbox(ctx context.Context, instance *sandboxInstance, launchSeconds int64, command, env []string, tty bool, stream backend.OutputStream) (vsockexec.ExecResponse, guestExecTiming, error) {
	guestReq := vsockexec.ExecRequest{
		Command: append([]string(nil), command...),
		Env:     append([]string(nil), env...),
		TTY:     tty,
	}
	seed := make([]byte, 64)
	if _, err := cryptorand.Read(seed); err == nil {
		guestReq.EntropySeed = seed
	}
	if a.GatewayRegistry != nil && instance.HostIP != "" {
		gwPort := a.GatewayPort
		if gwPort == 0 {
			gwPort = gateway.DefaultPort
		}
		guestReq.Env = append(guestReq.Env, gatewayEnvVars(instance, gwPort)...)
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

	if configured := strings.TrimSpace(req.KernelImagePath); configured == "" {
		if spec, ok := bootassets.LookupManagedKernelForHost(a.Name()); ok {
			path, _ := bootassets.ManagedKernelPathForHost(a.Name())
			appendCheck("kernel_image", "pass", fmt.Sprintf("kernel image will be auto-managed (%s -> %s)", spec.ID, path))
		} else {
			appendCheck("kernel_image", "warn", "kernel image not configured and no managed kernel asset is available for this host")
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

	if mkfsPath, err := hosttools.ResolveE2FSProgsBinary("mkfs.ext4"); err != nil {
		status := "fail"
		if runtime.GOOS == "darwin" {
			status = "warn"
		}
		appendCheck("mkfs_ext4", status, fmt.Sprintf("mkfs.ext4 not available: %v", err))
	} else {
		appendCheck("mkfs_ext4", "pass", fmt.Sprintf("found mkfs.ext4 (%s) for OCI rootfs materialisation", mkfsPath))
	}
	if debugfsPath, err := hosttools.ResolveE2FSProgsBinary("debugfs"); err != nil {
		status := "fail"
		if runtime.GOOS == "darwin" {
			status = "warn"
		}
		appendCheck("debugfs", status, fmt.Sprintf("debugfs not available: %v", err))
	} else {
		appendCheck("debugfs", "pass", fmt.Sprintf("found debugfs (%s) for runtime rootfs preparation", debugfsPath))
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

	privilegedHelperPath := resolvePrivilegedHelperPath(req.FirecrackerConfig)

	requiredCommands := []string{"ip", "iptables", "sysctl", "sudo"}
	for _, cmd := range requiredCommands {
		if _, err := exec.LookPath(cmd); err != nil {
			appendCheck("network_cmd_"+cmd, "fail", fmt.Sprintf("missing required host command %q", cmd))
		} else {
			appendCheck("network_cmd_"+cmd, "pass", fmt.Sprintf("found host command %q", cmd))
		}
	}

	if _, err := os.Stat(privilegedHelperPath); err != nil {
		appendCheck("network_helper", "fail", fmt.Sprintf("privileged helper %q is not accessible: %v", privilegedHelperPath, err))
	} else {
		appendCheck("network_helper", "pass", fmt.Sprintf("using privileged helper %q", privilegedHelperPath))

		version, err := helperVersion(context.Background(), req.FirecrackerConfig)
		if err != nil {
			appendCheck("network_helper_version", "warn", fmt.Sprintf("privileged helper version probe failed: %v", err))
		} else {
			appendCheck("network_helper_version", "pass", fmt.Sprintf("helper contract version %s", version))
		}

		caps, err := helperCapabilities(context.Background(), req.FirecrackerConfig)
		if err != nil {
			appendCheck("network_helper_capabilities", "fail", fmt.Sprintf("privileged helper capability probe failed: %v", err))
		} else {
			requiredCaps := helperRequiredCapabilities(req.FirecrackerConfig)
			missingCaps := helperMissingCapabilities(caps, requiredCaps)
			if len(missingCaps) > 0 {
				appendCheck("network_helper_capabilities", "fail", fmt.Sprintf("helper is missing required capabilities: %s (have: %s)", strings.Join(missingCaps, ", "), strings.Join(caps, ", ")))
			} else {
				appendCheck("network_helper_capabilities", "pass", fmt.Sprintf("helper capabilities: %s", strings.Join(caps, ", ")))
			}
		}
	}

	snapshotDriver := strings.ToLower(strings.TrimSpace(req.FirecrackerConfig.Snapshots.Driver))
	switch snapshotDriver {
	case "", "file":
		appendCheck("snapshot_driver", "pass", "using snapshot driver \"file\"")
	case "zfs":
		appendCheck("snapshot_driver", "pass", "using snapshot driver \"zfs\"")
		dataset := strings.TrimSpace(req.FirecrackerConfig.Snapshots.ZFSDataset)
		if dataset == "" {
			appendCheck("snapshot_zfs_dataset", "fail", "zfs snapshot driver requires snapshots.zfs_dataset")
		} else {
			appendCheck("snapshot_zfs_dataset", "pass", fmt.Sprintf("configured zfs dataset %q", dataset))
		}

		zfsBinary := ""
		if path, err := lookPathWithFallback("zfs", "/usr/sbin/zfs", "/sbin/zfs"); err != nil {
			appendCheck("snapshot_zfs_binary", "fail", fmt.Sprintf("zfs command not available: %v", err))
		} else {
			zfsBinary = path
			appendCheck("snapshot_zfs_binary", "pass", fmt.Sprintf("found zfs command %q", path))
		}

		if dataset != "" {
			if zfsBinary == "" {
				appendCheck("snapshot_zfs_dataset_access", "fail", fmt.Sprintf("unable to access zfs dataset %q: zfs command not available", dataset))
			} else {
				out, err := runRootCommandOutput(context.Background(), req.FirecrackerConfig, "zfs", "list", "-H", "-d", "0", "-o", "name", dataset)
				if err != nil {
					appendCheck("snapshot_zfs_dataset_access", "fail", fmt.Sprintf("unable to access zfs dataset %q: %v", dataset, err))
				} else if strings.TrimSpace(string(out)) != dataset {
					appendCheck("snapshot_zfs_dataset_access", "fail", fmt.Sprintf("zfs dataset probe for %q returned %q", dataset, strings.TrimSpace(string(out))))
				} else {
					appendCheck("snapshot_zfs_dataset_access", "pass", fmt.Sprintf("zfs dataset %q is accessible", dataset))
				}
			}
		}
	default:
		appendCheck("snapshot_driver", "fail", fmt.Sprintf("unsupported snapshot driver %q", req.FirecrackerConfig.Snapshots.Driver))
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

func (a *Adapter) run(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
	runStart := time.Now()
	observation := firecrackerRunObservation{
		ExecutionID: req.ExecutionID,
		Backend:     a.Name(),
		LaunchedVM:  req.Launch,
		ExitCode:    1,
	}

	if req.Policy == nil {
		return nil, errors.New("missing compiled policy")
	}
	observation.ImageRef = req.Policy.ImageRef
	observation.ImageDigest = req.Policy.ImageDigest
	if req.Policy.NetworkDefault != "deny" && req.Policy.NetworkDefault != "allow" {
		return nil, fmt.Errorf("firecracker backend requires network.default=deny or allow, got %q", req.Policy.NetworkDefault)
	}
	if len(req.Command) == 0 {
		return nil, errors.New("missing command")
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("firecracker backend is linux-only, current OS is %s", runtime.GOOS)
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
		return &backend.ExecutionResult{
			ExecutionID: req.ExecutionID,
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
	observation.Phase = "launch"

	kernelPath, kernelNotice, err := a.resolveKernelPath(ctx, req.KernelImagePath)
	if err != nil {
		return nil, err
	}
	logExecutionNotice(a.Name(), req.ExecutionID, kernelNotice)

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

	rootfsCopyStart := time.Now()
	writableVolume, cleanupVolume, err := prepareWritableRootVolume(ctx, req.FirecrackerConfig, req.ExecutionID, runDir, preparedRootFSPath)
	if err != nil {
		observation.RootFSCopyMS = durationMillisCeil(time.Since(rootfsCopyStart))
		return nil, fmt.Errorf("prepare per-run rootfs: %w", err)
	}
	if cleanupVolume != nil {
		defer cleanupVolume()
	}
	observation.RootFSCopyMS = durationMillisCeil(time.Since(rootfsCopyStart))
	vmRootFSPath := writableVolume.AttachmentPath

	networkSetupStart := time.Now()
	networkRunCommand := func(ctx context.Context, args ...string) error {
		return runRootCommand(ctx, req.FirecrackerConfig, args...)
	}
	networkRunBatch := func(ctx context.Context, commands [][]string) error {
		return runRootCommandBatch(ctx, req.FirecrackerConfig, commands)
	}
	networkCfg, cleanupNetwork, err := setupHostNetwork(ctx, req.ExecutionID, req.Policy.NetworkDefault == "allow", req.Policy.Allow, 0, networkRunCommand, networkRunBatch, nil)
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
	dockerBootArgs := dockerServiceBootArgs(req.Policy, req.FirecrackerConfig)
	fcCfg := firecrackerConfig{
		BootSource: bootSource{
			KernelImagePath: kernelPath,
			BootArgs: fmt.Sprintf(
				"console=ttyS0 reboot=k panic=1 pci=off init=/usr/sbin/cleanroom-init random.trust_cpu=on cleanroom_guest_ip=%s cleanroom_guest_gw=%s cleanroom_guest_mask=24 cleanroom_guest_dns=%s cleanroom_guest_port=%d %s",
				networkCfg.GuestIP,
				networkCfg.HostIP,
				networkCfg.GuestDNS,
				req.GuestPort,
				dockerBootArgs,
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
				GuestMac:    guestMACFromExecutionID(req.ExecutionID),
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
		TTY:     req.TTY,
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

	return &backend.ExecutionResult{
		ExecutionID: req.ExecutionID,
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
	ExecutionID        string `json:"execution_id"`
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

func (a *Adapter) RuntimeBaseKey(ctx context.Context, compiled *policy.CompiledPolicy, _ backend.FirecrackerConfig) (string, error) {
	if compiled == nil {
		return "", errors.New("missing compiled policy")
	}

	imageDigest := strings.TrimSpace(compiled.ImageDigest)
	if imageDigest == "" {
		artifact, err := a.ensureImageArtifact(ctx, compiled.ImageRef)
		if err != nil {
			return "", err
		}
		imageDigest = artifact.Digest
	}

	_, guestAgentHash, err := a.getGuestAgentBinary()
	if err != nil {
		return "", err
	}

	preparedPath, err := preparedRuntimeRootFSPath(imageDigest, guestAgentHash)
	if err != nil {
		return "", err
	}
	return "prepared-rootfs:" + preparedPath, nil
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
	if err := a.installGuestRuntimeIntoRootFS(tmpPath, guestAgentPath); err != nil {
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

func (a *Adapter) installGuestRuntimeIntoRootFS(rootFSPath, guestAgentPath string) error {
	if _, err := hosttools.ResolveE2FSProgsBinary("debugfs"); err != nil {
		return fmt.Errorf("find debugfs for runtime rootfs preparation: %w", err)
	}

	initScriptPath, err := createGuestInitScript()
	if err != nil {
		return err
	}
	defer os.Remove(initScriptPath)

	if err := ext4edit.InjectFile(rootFSPath, guestAgentPath, "/usr/local/bin/cleanroom-guest-agent", 0o755); err != nil {
		return fmt.Errorf("inject guest agent into rootfs image: %w", err)
	}
	if err := ext4edit.InjectFile(rootFSPath, initScriptPath, guestInitScriptPathUsrSbin, 0o755); err != nil {
		return fmt.Errorf("inject cleanroom init into rootfs image (%s): %w", guestInitScriptPathUsrSbin, err)
	}
	if ext4edit.PathType(rootFSPath, "/sbin") == ext4edit.PathKindDirectory {
		if err := ext4edit.InjectFile(rootFSPath, initScriptPath, guestInitScriptPathSbin, 0o755); err != nil {
			return fmt.Errorf("inject cleanroom init into rootfs image (%s): %w", guestInitScriptPathSbin, err)
		}
	}
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
	imageArtifact, err := a.ensureImageArtifact(ctx, compiled.ImageRef)
	if err != nil {
		return nil, err
	}
	preparedRootFSPath, err := a.ensurePreparedRuntimeRootFS(ctx, cfg, imageArtifact)
	if err != nil {
		return nil, err
	}
	instance, err := a.launchSandboxVMFromRootFS(ctx, sandboxID, compiled, cfg, preparedRootFSPath)
	if err != nil {
		return nil, err
	}
	instance.ImageRef = imageArtifact.Ref
	instance.ImageDigest = imageArtifact.Digest
	return instance, nil
}

func (a *Adapter) launchSandboxVMFromRootFS(ctx context.Context, sandboxID string, compiled *policy.CompiledPolicy, cfg backend.FirecrackerConfig, sourceRootFSPath string) (*sandboxInstance, error) {
	if compiled == nil {
		return nil, errors.New("missing compiled policy")
	}
	if compiled.NetworkDefault != "deny" && compiled.NetworkDefault != "allow" {
		return nil, fmt.Errorf("firecracker backend requires network.default=deny or allow, got %q", compiled.NetworkDefault)
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
	kernelPath, _, err := a.resolveKernelPath(ctx, cfg.KernelImagePath)
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

	writableVolume, cleanupVolume, err := prepareWritableRootVolume(ctx, cfg, sandboxID, runDir, sourceRootFSPath)
	if err != nil {
		return nil, fmt.Errorf("prepare persistent rootfs: %w", err)
	}
	vmRootFSPath := writableVolume.AttachmentPath

	networkRunCommand := func(ctx context.Context, args ...string) error {
		return runRootCommand(ctx, cfg, args...)
	}
	networkRunBatch := func(ctx context.Context, commands [][]string) error {
		return runRootCommandBatch(ctx, cfg, commands)
	}
	gwPort := 0
	if a.GatewayRegistry != nil {
		gwPort = a.GatewayPort
		if gwPort == 0 {
			gwPort = gateway.DefaultPort
		}
	}
	// The DNS deny callback is routed through an atomic pointer to the sandbox
	// instance which is created after network setup. The instance sets a
	// concrete warning handler when RunInSandbox is called.
	var instanceRef atomic.Pointer[sandboxInstance]
	dnsOnDeny := func(_, queryName string) {
		if inst := instanceRef.Load(); inst != nil {
			inst.warnings.Emit(fmt.Sprintf("dns query for disallowed host: %s", queryName))
		}
	}

	networkCfg, cleanupNetwork, err := setupHostNetwork(ctx, sandboxID, compiled.NetworkDefault == "allow", compiled.Allow, gwPort, networkRunCommand, networkRunBatch, dnsOnDeny)
	if err != nil {
		cleanupVolume()
		return nil, fmt.Errorf("setup host network: %w", err)
	}

	if a.GatewayRegistry != nil {
		if err := a.GatewayRegistry.Register(networkCfg.GuestIP, sandboxID, compiled); err != nil {
			cleanupNetwork()
			cleanupVolume()
			return nil, fmt.Errorf("register sandbox in gateway: %w", err)
		}
	}

	cleanupAll := func() {
		if a.GatewayRegistry != nil {
			a.GatewayRegistry.Release(networkCfg.GuestIP)
		}
		cleanupNetwork()
		cleanupVolume()
	}

	vsockPath := filepath.Join(runDir, "vsock.sock")
	dockerBootArgs := dockerServiceBootArgs(compiled, cfg)
	fcCfg := firecrackerConfig{
		BootSource: bootSource{
			KernelImagePath: kernelPath,
			BootArgs: fmt.Sprintf(
				"console=ttyS0 reboot=k panic=1 pci=off init=/usr/sbin/cleanroom-init random.trust_cpu=on cleanroom_guest_ip=%s cleanroom_guest_gw=%s cleanroom_guest_mask=24 cleanroom_guest_dns=%s cleanroom_guest_port=%d %s",
				networkCfg.GuestIP,
				networkCfg.HostIP,
				networkCfg.GuestDNS,
				cfg.GuestPort,
				dockerBootArgs,
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
			GuestMac:    guestMACFromExecutionID(sandboxID),
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
		ImageRef:       compiled.ImageRef,
		ImageDigest:    compiled.ImageDigest,
		CommandTimeout: cfg.LaunchSeconds,
		HostIP:         networkCfg.HostIP,
		GuestIP:        networkCfg.GuestIP,
		fcCmd:          fcCmd,
		exitedCh:       make(chan struct{}),
		cleanupNetwork: cleanupNetwork,
		cleanupVolume:  cleanupVolume,
		vmRootFSPath:   vmRootFSPath,
		volumeRef:      writableVolume.Ref,
	}
	instanceRef.Store(instance)
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

func (a *Adapter) resolveKernelPath(ctx context.Context, configuredPath string) (path, notice string, err error) {
	resolved, err := bootassets.ResolveKernelPathForHost(ctx, a.Name(), configuredPath)
	if err != nil {
		return "", "", err
	}
	return resolved.Path, resolved.Notice, nil
}

func sandboxRuntimeBaseDir() (string, error) {
	base, err := paths.StateBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "sandboxes"), nil
}

func preparePersistentWritableVolume(ctx context.Context, driver volumestore.Driver, sandboxID, runDir, sourceRef string, minimumBytes int64) (volumestore.WritableVolume, func(), error) {
	sourceRef = strings.TrimSpace(sourceRef)
	if sourceRef == "" {
		return volumestore.WritableVolume{}, nil, errors.New("missing persistent rootfs source")
	}

	attachmentPath := filepath.Join(runDir, "rootfs-persistent.ext4")
	resizeVolume := func(volume volumestore.WritableVolume) (volumestore.WritableVolume, func(), error) {
		cleanupVolume := func() {
			if err := driver.DestroyVolume(context.Background(), volumestore.DestroyVolumeRequest{VolumeRef: volume.Ref}); err != nil {
				log.Printf("firecracker: cleanup persistent volume %q: %v", volume.Ref, err)
			}
		}
		if err := volumestore.EnsureWritableVolumeMinimumSize(ctx, driver, volume, minimumBytes); err != nil {
			cleanupVolume()
			return volumestore.WritableVolume{}, nil, fmt.Errorf("resize persistent rootfs: %w", err)
		}
		return volume, cleanupVolume, nil
	}

	if filepath.IsAbs(sourceRef) {
		rootfsPath, err := filepath.Abs(sourceRef)
		if err != nil {
			return volumestore.WritableVolume{}, nil, err
		}
		if _, err := os.Stat(rootfsPath); err != nil {
			return volumestore.WritableVolume{}, nil, fmt.Errorf("rootfs %s: %w", rootfsPath, err)
		}

		baseVolume, err := driver.EnsureBaseVolume(ctx, volumestore.EnsureBaseVolumeRequest{
			BaseID:       strings.TrimSuffix(filepath.Base(rootfsPath), filepath.Ext(rootfsPath)),
			SourcePath:   rootfsPath,
			MinimumBytes: minimumBytes,
		})
		if err != nil {
			return volumestore.WritableVolume{}, nil, fmt.Errorf("prepare base volume: %w", err)
		}
		volume, err := driver.CreateWritableVolume(ctx, volumestore.CreateWritableVolumeRequest{
			VolumeID:       sandboxID,
			BaseRef:        baseVolume.Ref,
			AttachmentPath: attachmentPath,
		})
		if err != nil {
			return volumestore.WritableVolume{}, nil, err
		}
		return resizeVolume(volume)
	}

	volume, err := driver.CloneSnapshotToVolume(ctx, volumestore.CloneSnapshotToVolumeRequest{
		VolumeID:       sandboxID,
		SnapshotRef:    sourceRef,
		AttachmentPath: attachmentPath,
	})
	if err != nil {
		return volumestore.WritableVolume{}, nil, err
	}
	return resizeVolume(volume)
}

func prepareWritableRootVolume(ctx context.Context, cfg backend.FirecrackerConfig, volumeID, runDir, sourceRef string) (volumestore.WritableVolume, func(), error) {
	driverCfg, err := snapshotConfigForStorageRef(cfg, sourceRef)
	if err != nil {
		return volumestore.WritableVolume{}, nil, err
	}
	driver, err := rootFSVolumeStoreDriverFn(driverCfg)
	if err != nil {
		return volumestore.WritableVolume{}, nil, err
	}
	return preparePersistentWritableVolume(ctx, driver, volumeID, runDir, sourceRef, cfg.MinimumRootFSBytes)
}

func snapshotVolumeRef(instance *sandboxInstance) string {
	if instance == nil {
		return ""
	}
	if volumeRef := strings.TrimSpace(instance.volumeRef); volumeRef != "" {
		return volumeRef
	}
	return strings.TrimSpace(instance.vmRootFSPath)
}

func snapshotStorageBaseDir(cfg backend.FirecrackerConfig) (string, error) {
	if baseDir := strings.TrimSpace(cfg.Snapshots.BaseDir); baseDir != "" {
		return filepath.Clean(baseDir), nil
	}
	return paths.SnapshotDir()
}

type rootVolumeCommandRunner struct {
	cfg backend.FirecrackerConfig
}

func (r rootVolumeCommandRunner) Run(ctx context.Context, command string, args ...string) error {
	return runRootCommand(ctx, r.cfg, append([]string{command}, args...)...)
}

func (r rootVolumeCommandRunner) Output(ctx context.Context, command string, args ...string) ([]byte, error) {
	return runRootCommandOutput(ctx, r.cfg, append([]string{command}, args...)...)
}

func snapshotConfigForStorageRef(cfg backend.FirecrackerConfig, storageRef string) (backend.FirecrackerConfig, error) {
	if datasetRoot, ok := volumestore.ZFSDatasetRootFromManagedRef(storageRef); ok {
		driverName := strings.ToLower(strings.TrimSpace(cfg.Snapshots.Driver))
		if driverName != "" && driverName != "zfs" {
			return cfg, fmt.Errorf("snapshot storage_ref %q requires zfs driver, got %q", storageRef, cfg.Snapshots.Driver)
		}
		cfg.Snapshots.Driver = "zfs"
		cfg.Snapshots.ZFSDataset = datasetRoot
	}
	return cfg, nil
}

func rootFSVolumeStoreDriver(cfg backend.FirecrackerConfig) (volumestore.Driver, error) {
	driverName := strings.ToLower(strings.TrimSpace(cfg.Snapshots.Driver))
	switch driverName {
	case "", "file":
		snapshotBaseDir, err := snapshotStorageBaseDir(cfg)
		if err != nil {
			return nil, fmt.Errorf("resolve snapshot base directory: %w", err)
		}
		driver, err := volumestore.NewFileDriver(volumestore.FileDriverOptions{
			SnapshotBaseDir: snapshotBaseDir,
			Namespace:       "firecracker",
		})
		if err != nil {
			return nil, err
		}
		return driver, nil
	case "zfs":
		driver, err := volumestore.NewZFSDriver(volumestore.ZFSDriverOptions{
			DatasetRoot: cfg.Snapshots.ZFSDataset,
			Runner:      rootVolumeCommandRunner{cfg: cfg},
		})
		if err != nil {
			return nil, err
		}
		return driver, nil
	default:
		return nil, fmt.Errorf("unsupported firecracker snapshot driver %q", cfg.Snapshots.Driver)
	}
}

func snapshotVolumeStoreDriver(cfg backend.FirecrackerConfig) (volumestore.Driver, error) {
	if !cfg.Snapshots.Enabled {
		return nil, errors.New("firecracker snapshots are not enabled")
	}
	return rootFSVolumeStoreDriver(cfg)
}

func defaultSyncHostFilesystem(ctx context.Context) error {
	return runCombinedCommand(ctx, []string{"sync"}, []string{"host", "sync"})
}

func flushSnapshotHostFilesystem(ctx context.Context, driverName string) error {
	if !snapshotDriverNeedsHostSync(driverName) {
		return nil
	}
	// ZFS snapshots persist pool state, not the current host page cache.
	// Flush after the guest is paused so the snapshot captures its latest writes.
	if err := syncHostFilesystem(ctx); err != nil {
		return fmt.Errorf("sync host filesystem before snapshot: %w", err)
	}
	return nil
}

func snapshotDriverNeedsHostSync(driverName string) bool {
	return strings.EqualFold(strings.TrimSpace(driverName), "zfs")
}

func pauseSandboxProcess(instance *sandboxInstance) error {
	if instance == nil || instance.fcCmd == nil || instance.fcCmd.Process == nil {
		return errors.New("sandbox process is not available")
	}
	if err := sendProcessSignal(instance.fcCmd.Process, syscall.SIGSTOP); err != nil {
		return fmt.Errorf("pause sandbox process: %w", err)
	}
	return nil
}

func resumeSandboxProcess(instance *sandboxInstance) error {
	if instance == nil || instance.fcCmd == nil || instance.fcCmd.Process == nil {
		return errors.New("sandbox process is not available")
	}
	if err := sendProcessSignal(instance.fcCmd.Process, syscall.SIGCONT); err != nil {
		return fmt.Errorf("resume sandbox process: %w", err)
	}
	return nil
}

func (s *sandboxInstance) shutdown() {
	if s == nil {
		return
	}
	stopVM(s.fcCmd, s.exitedCh)
	if s.cleanupNetwork != nil {
		s.cleanupNetwork()
	}
	if s.cleanupVolume != nil {
		s.cleanupVolume()
	}
	if strings.TrimSpace(s.RunDir) != "" {
		_ = os.RemoveAll(s.RunDir)
		return
	}
	if strings.TrimSpace(s.vmRootFSPath) != "" && s.cleanupVolume == nil {
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

	// Provide stdin/resize handlers so the caller can forward interactive
	// input to the guest process via the same vsock connection.
	inputSender := &inputFrameSender{w: conn}
	if stream.OnAttach != nil {
		stream.OnAttach(backend.AttachIO{
			WriteStdin: func(data []byte) error {
				return inputSender.Send(vsockexec.ExecInputFrame{
					Type: "stdin",
					Data: data,
				})
			},
			CloseStdin: func() error {
				return inputSender.Send(vsockexec.ExecInputFrame{Type: "eof"})
			},
			ResizeTTY: func(cols, rows uint32) error {
				return inputSender.Send(vsockexec.ExecInputFrame{
					Type: "resize",
					Cols: cols,
					Rows: rows,
				})
			},
		})
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

type inputFrameSender struct {
	w  io.Writer
	mu sync.Mutex
}

func (s *inputFrameSender) Send(frame vsockexec.ExecInputFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return vsockexec.EncodeInputFrame(s.w, frame)
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
	GuestDNS        string
	PolicyResolveMS int64
}

type ipLookupFunc func(ctx context.Context, host string) ([]net.IP, error)
type interfaceLookupFunc func(name string) (*net.Interface, error)
type rootCommandFunc func(ctx context.Context, args ...string) error
type rootCommandBatchFunc func(ctx context.Context, commands [][]string) error

func setupHostNetwork(ctx context.Context, runID string, allowAll bool, allow []policy.AllowRule, gatewayPort int, runCommand rootCommandFunc, runBatchCommand rootCommandBatchFunc, onDeny func(sandboxID, queryName string)) (hostNetworkConfig, func(), error) {
	lookup := func(ctx context.Context, host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip4", host)
	}
	return setupHostNetworkWithDeps(ctx, runID, allowAll, allow, gatewayPort, lookup, runCommand, runBatchCommand, onDeny)
}

func setupHostNetworkWithDeps(ctx context.Context, runID string, allowAll bool, allow []policy.AllowRule, gatewayPort int, lookup ipLookupFunc, runCommand rootCommandFunc, runBatchCommand rootCommandBatchFunc, onDeny func(sandboxID, queryName string)) (hostNetworkConfig, func(), error) {
	return setupHostNetworkWithTrustedDNSFactory(ctx, runID, allowAll, allow, gatewayPort, lookup, net.InterfaceByName, runCommand, runBatchCommand, newTrustedDNSService, onDeny)
}

func setupHostNetworkWithTapLookup(ctx context.Context, runID string, allowAll bool, allow []policy.AllowRule, gatewayPort int, lookup ipLookupFunc, interfaceByName interfaceLookupFunc, runCommand rootCommandFunc, runBatchCommand rootCommandBatchFunc, onDeny func(sandboxID, queryName string)) (hostNetworkConfig, func(), error) {
	return setupHostNetworkWithTrustedDNSFactory(ctx, runID, allowAll, allow, gatewayPort, lookup, interfaceByName, runCommand, runBatchCommand, newTrustedDNSService, onDeny)
}

func setupHostNetworkWithTrustedDNSFactory(ctx context.Context, runID string, allowAll bool, allow []policy.AllowRule, gatewayPort int, lookup ipLookupFunc, interfaceByName interfaceLookupFunc, runCommand rootCommandFunc, runBatchCommand rootCommandBatchFunc, factory trustedDNSFactory, onDeny func(sandboxID, queryName string)) (hostNetworkConfig, func(), error) {
	_ = lookup
	if factory == nil {
		factory = newTrustedDNSService
	}

	tapName := tapNameFromExecutionID(runID)
	hostIP, guestIP := hostGuestIPs(runID)
	hostCIDR := hostIP + "/24"
	guestCIDR := guestIP + "/32"
	hostAddr, err := netip.ParseAddr(hostIP)
	if err != nil {
		return hostNetworkConfig{}, func() {}, fmt.Errorf("parse host ip %q: %w", hostIP, err)
	}
	guestAddr, err := netip.ParseAddr(guestIP)
	if err != nil {
		return hostNetworkConfig{}, func() {}, fmt.Errorf("parse guest ip %q: %w", guestIP, err)
	}

	setupRun := func(args ...string) error {
		return runCommand(ctx, args...)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), networkCleanupTimeout)
	cleanupCmds := make([][]string, 0, 16)
	trustedDNSCleanup := func() {}
	cleanup := func() {
		defer cleanupCancel()
		trustedDNSCleanup()
		reversed := make([][]string, 0, len(cleanupCmds))
		for i := len(cleanupCmds) - 1; i >= 0; i-- {
			reversed = append(reversed, cleanupCmds[i])
		}
		for _, args := range reversed {
			if isTapDeleteCommand(args, tapName) {
				_ = deleteTapDeviceWithRetry(cleanupCtx, tapName, tapDeleteRetryInterval, interfaceByName, runCommand)
				continue
			}
			_ = runCommand(cleanupCtx, args...)
		}
	}
	addCleanup := func(args ...string) {
		cleanupCmds = append(cleanupCmds, append([]string(nil), args...))
	}

	staleTapCleanupCtx, staleTapCleanupCancel := context.WithTimeout(context.Background(), networkCleanupTimeout)
	defer staleTapCleanupCancel()
	if _, err := interfaceByName(tapName); err == nil {
		if err := deleteTapDeviceWithRetry(staleTapCleanupCtx, tapName, tapDeleteRetryInterval, interfaceByName, runCommand); err != nil {
			return hostNetworkConfig{}, func() {}, fmt.Errorf("remove stale tap device %s: %w", tapName, err)
		}
	} else if !isNoSuchNetworkInterfaceError(err) {
		return hostNetworkConfig{}, func() {}, fmt.Errorf("lookup tap device %s: %w", tapName, err)
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

	// Disable IPv6 on TAP to prevent bypass of IPv4-only policy controls.
	if err := setupRun("sysctl", "-w", fmt.Sprintf("net.ipv6.conf.%s.disable_ipv6=1", tapName)); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("disable ipv6 on %s: %w", tapName, err)
	}

	if err := setupRun("iptables", "-A", "INPUT", "-i", tapName, "!", "-s", guestIP, "-j", "DROP"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("install anti-spoof rule for %s: %w", tapName, err)
	}
	addCleanup("iptables", "-D", "INPUT", "-i", tapName, "!", "-s", guestIP, "-j", "DROP")

	if gatewayPort > 0 {
		port := strconv.Itoa(gatewayPort)
		if err := setupRun("iptables", "-A", "INPUT", "-i", tapName, "-s", guestIP, "-p", "tcp", "--dport", port, "-j", "ACCEPT"); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install gateway accept rule for %s: %w", tapName, err)
		}
		addCleanup("iptables", "-D", "INPUT", "-i", tapName, "-s", guestIP, "-p", "tcp", "--dport", port, "-j", "ACCEPT")
	}

	for _, proto := range []string{"udp", "tcp"} {
		if err := setupRun("iptables", "-A", "INPUT", "-i", tapName, "-s", guestIP, "-p", proto, "--dport", strconv.Itoa(trustedDNSListenPort), "-j", "ACCEPT"); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install trusted dns %s accept rule for %s: %w", proto, tapName, err)
		}
		addCleanup("iptables", "-D", "INPUT", "-i", tapName, "-s", guestIP, "-p", proto, "--dport", strconv.Itoa(trustedDNSListenPort), "-j", "ACCEPT")
	}

	if err := setupRun("iptables", "-A", "INPUT", "-i", tapName, "-j", "DROP"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("install input deny rule for %s: %w", tapName, err)
	}
	addCleanup("iptables", "-D", "INPUT", "-i", tapName, "-j", "DROP")

	if err := setupRun("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", guestCIDR, "-j", "MASQUERADE"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("install nat rule for %s: %w", guestCIDR, err)
	}
	addCleanup("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", guestCIDR, "-j", "MASQUERADE")

	for _, proto := range []string{"udp", "tcp"} {
		if err := setupRun("iptables", "-t", "nat", "-A", "PREROUTING", "-i", tapName, "-p", proto, "--dport", "53", "-j", "REDIRECT", "--to-ports", strconv.Itoa(trustedDNSListenPort)); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install trusted dns %s redirect for %s: %w", proto, tapName, err)
		}
		addCleanup("iptables", "-t", "nat", "-D", "PREROUTING", "-i", tapName, "-p", proto, "--dport", "53", "-j", "REDIRECT", "--to-ports", strconv.Itoa(trustedDNSListenPort))
	}

	returnPathCleanup, err := installForwardReturnPathRule(setupRun, tapName)
	if err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("install forward return-path rule for %s: %w", tapName, err)
	}
	addCleanup(returnPathCleanup...)

	tcpChainName := ""
	udpChainName := ""
	if !allowAll {
		tcpChainName = trustedDNSTCPChainName(tapName)
		udpChainName = trustedDNSUDPChainName(tapName)

		if err := setupRun("iptables", "-N", tcpChainName); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("create trusted dns tcp chain for %s: %w", tapName, err)
		}
		addCleanup("iptables", "-X", tcpChainName)
		addCleanup("iptables", "-F", tcpChainName)
		if err := setupRun("iptables", "-N", udpChainName); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("create trusted dns udp chain for %s: %w", tapName, err)
		}
		addCleanup("iptables", "-X", udpChainName)
		addCleanup("iptables", "-F", udpChainName)

		establishedCleanup, err := installForwardEstablishedEgressRule(setupRun, tapName)
		if err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install forward established egress rule for %s: %w", tapName, err)
		}
		addCleanup(establishedCleanup...)

		if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-p", "tcp", "-j", tcpChainName); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install trusted dns tcp chain jump for %s: %w", tapName, err)
		}
		addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-p", "tcp", "-j", tcpChainName)
		if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-p", "udp", "-j", udpChainName); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install trusted dns udp chain jump for %s: %w", tapName, err)
		}
		addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-p", "udp", "-j", udpChainName)

		for _, rule := range literalIPv4AllowRules(allow) {
			for _, proto := range []string{"tcp", "udp"} {
				for _, port := range rule.Ports {
					portText := strconv.Itoa(port)
					if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-d", rule.Host, "-p", proto, "--dport", portText, "-j", "ACCEPT"); err != nil {
						cleanup()
						return hostNetworkConfig{}, func() {}, fmt.Errorf("install direct-ip %s allow rule for %s: %w", proto, rule.Host, err)
					}
					addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-d", rule.Host, "-p", proto, "--dport", portText, "-j", "ACCEPT")
				}
			}
		}
	}

	trustedDNSCleanup, err = factory(ctx, trustedDNSConfig{
		sandboxID:    runID,
		hostIP:       hostAddr,
		guestIP:      guestAddr,
		policy:       trustedDNSPolicy(allow),
		runBatch:     runBatchCommand,
		tcpChainName: tcpChainName,
		udpChainName: udpChainName,
		onDeny:       onDeny,
	})
	if err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("start trusted dns service: %w", err)
	}

	if allowAll {
		if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-j", "ACCEPT"); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install allow-all forward rule for %s: %w", tapName, err)
		}
		addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-j", "ACCEPT")
	} else {
		if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-j", "DROP"); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install default deny forward rule for %s: %w", tapName, err)
		}
		addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-j", "DROP")
	}

	return hostNetworkConfig{
		TapName:         tapName,
		HostIP:          hostIP,
		GuestIP:         guestIP,
		GuestDNS:        hostIP,
		PolicyResolveMS: 0,
	}, cleanup, nil
}

func isTapDeleteCommand(args []string, tapName string) bool {
	return len(args) == 4 && args[0] == "ip" && args[1] == "link" && args[2] == "del" && args[3] == tapName
}

func deleteTapDeviceWithRetry(ctx context.Context, tapName string, retryInterval time.Duration, interfaceByName interfaceLookupFunc, runCommand rootCommandFunc) error {
	if strings.TrimSpace(tapName) == "" {
		return nil
	}
	if retryInterval <= 0 {
		retryInterval = time.Millisecond
	}

	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()

	for {
		err := runCommand(ctx, "ip", "link", "del", tapName)
		if err == nil {
			return nil
		}
		if _, lookupErr := interfaceByName(tapName); lookupErr != nil {
			if isNoSuchNetworkInterfaceError(lookupErr) {
				return nil
			}
			return fmt.Errorf("lookup tap device %s after delete failure (%v): %w", tapName, err, lookupErr)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("delete tap device %s: %w", tapName, err)
		case <-ticker.C:
		}
	}
}

func isNoSuchNetworkInterfaceError(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return strings.Contains(opErr.Err.Error(), "no such network interface")
	}
	return strings.Contains(err.Error(), "no such network interface")
}

func installForwardReturnPathRule(setupRun func(args ...string) error, tapName string) ([]string, error) {
	return installForwardEstablishedRule(setupRun, "-o", tapName)
}

func installForwardEstablishedEgressRule(setupRun func(args ...string) error, tapName string) ([]string, error) {
	return installForwardEstablishedRule(setupRun, "-i", tapName)
}

func installForwardEstablishedRule(setupRun func(args ...string) error, directionFlag, tapName string) ([]string, error) {
	conntrackAdd := []string{"iptables", "-A", "FORWARD", directionFlag, tapName, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
	if err := setupRun(conntrackAdd...); err == nil {
		return []string{"iptables", "-D", "FORWARD", directionFlag, tapName, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}, nil
	}

	stateAdd := []string{"iptables", "-A", "FORWARD", directionFlag, tapName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
	if err := setupRun(stateAdd...); err != nil {
		return nil, err
	}
	return []string{"iptables", "-D", "FORWARD", directionFlag, tapName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"}, nil
}

func trustedDNSPolicy(allow []policy.AllowRule) *policy.CompiledPolicy {
	copied := make([]policy.AllowRule, 0, len(allow))
	for _, rule := range allow {
		copied = append(copied, policy.AllowRule{
			Host:  rule.Host,
			Ports: append([]int(nil), rule.Ports...),
		})
	}
	return &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: "deny",
		Allow:          copied,
	}
}

func literalIPv4AllowRules(allow []policy.AllowRule) []policy.AllowRule {
	out := make([]policy.AllowRule, 0, len(allow))
	for _, rule := range allow {
		addr, err := netip.ParseAddr(strings.TrimSpace(rule.Host))
		if err != nil || !addr.Is4() {
			continue
		}
		out = append(out, policy.AllowRule{
			Host:  addr.String(),
			Ports: append([]int(nil), rule.Ports...),
		})
	}
	return out
}

func resolvePrivilegedHelperPath(cfg backend.FirecrackerConfig) string {
	helperPath := strings.TrimSpace(cfg.PrivilegedHelperPath)
	if helperPath == "" {
		helperPath = defaultPrivilegedHelperPath
	}
	return helperPath
}

func helperRequiredCapabilities(cfg backend.FirecrackerConfig) []string {
	required := []string{
		helperCapabilityFirecrackerNetwork,
		helperCapabilityFirecrackerTrustedDNS,
	}

	snapshotDriver := strings.ToLower(strings.TrimSpace(cfg.Snapshots.Driver))
	if snapshotDriver == "zfs" {
		required = append(required, helperCapabilityFirecrackerZFS)
	}
	return required
}

func helperMissingCapabilities(have, required []string) []string {
	seen := make(map[string]struct{}, len(have))
	for _, cap := range have {
		cap = strings.TrimSpace(cap)
		if cap == "" {
			continue
		}
		seen[cap] = struct{}{}
	}

	missing := make([]string, 0, len(required))
	for _, cap := range required {
		if _, ok := seen[cap]; !ok {
			missing = append(missing, cap)
		}
	}
	return missing
}

func helperCapabilities(ctx context.Context, cfg backend.FirecrackerConfig) ([]string, error) {
	out, err := runRootCommandOutput(ctx, cfg, "capabilities")
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	caps := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		cap := strings.TrimSpace(line)
		if cap == "" {
			continue
		}
		if _, ok := seen[cap]; ok {
			continue
		}
		seen[cap] = struct{}{}
		caps = append(caps, cap)
	}
	return caps, nil
}

func helperVersion(ctx context.Context, cfg backend.FirecrackerConfig) (string, error) {
	out, err := runRootCommandOutput(ctx, cfg, "version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func logExecutionNotice(backendName, runID, notice string) {
	msg := strings.TrimSpace(notice)
	if msg == "" {
		return
	}
	id := strings.TrimSpace(runID)
	if id == "" {
		log.Printf("%s: %s", backendName, msg)
		return
	}
	log.Printf("%s execution_id=%s: %s", backendName, id, msg)
}

func lookPathWithFallback(binary string, candidates ...string) (string, error) {
	if path, err := exec.LookPath(binary); err == nil {
		return path, nil
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%q not found in PATH or fallback locations", binary)
}

func runRootCommand(ctx context.Context, cfg backend.FirecrackerConfig, args ...string) error {
	if len(args) == 0 {
		return errors.New("missing privileged command")
	}

	helperPath := resolvePrivilegedHelperPath(cfg)
	if strings.TrimSpace(helperPath) == "" {
		return errors.New("missing privileged helper path")
	}
	return runCombinedCommand(ctx, append([]string{"sudo", "-n", helperPath}, args...), append([]string{"helper"}, args...))
}

func runRootCommandOutput(ctx context.Context, cfg backend.FirecrackerConfig, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("missing privileged command")
	}

	helperPath := resolvePrivilegedHelperPath(cfg)
	if strings.TrimSpace(helperPath) == "" {
		return nil, errors.New("missing privileged helper path")
	}
	return runCombinedCommandOutput(ctx, append([]string{"sudo", "-n", helperPath}, args...), append([]string{"helper"}, args...))
}

func runRootCommandBatch(ctx context.Context, cfg backend.FirecrackerConfig, commands [][]string) error {
	for _, args := range commands {
		if len(args) == 0 {
			continue
		}
		if err := runRootCommand(ctx, cfg, args...); err != nil {
			return err
		}
	}
	return nil
}

func runCombinedCommand(ctx context.Context, command []string, errorContext []string) error {
	_, err := runCombinedCommandOutput(ctx, command, errorContext)
	return err
}
func runCombinedCommandOutput(ctx context.Context, command []string, errorContext []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = "no stderr output"
		}
		return nil, fmt.Errorf("%s: %w (%s)", strings.Join(errorContext, " "), err, msg)
	}
	return out, nil
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

func gatewayEnvVars(instance *sandboxInstance, gwPort int) []string {
	if instance.Policy == nil {
		return nil
	}

	var gitHosts []string
	for _, rule := range instance.Policy.Allow {
		for _, port := range rule.Ports {
			if port == 443 {
				gitHosts = append(gitHosts, rule.Host)
				break
			}
		}
	}
	if len(gitHosts) == 0 {
		return nil
	}

	gatewayAddr := fmt.Sprintf("http://%s:%d", instance.HostIP, gwPort)
	env := make([]string, 0, 1+len(gitHosts)*2)
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(gitHosts)))
	for i, host := range gitHosts {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=url.%s/git/%s/.insteadOf", i, gatewayAddr, host))
		env = append(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d=https://%s/", i, host))
	}
	return env
}

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

func tapNameFromExecutionID(runID string) string {
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

func guestMACFromExecutionID(runID string) string {
	sum := sha1.Sum([]byte(runID))
	return fmt.Sprintf("02:fc:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3])
}
