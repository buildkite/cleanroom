//go:build darwin

package darwinvz

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/bootassets"
	"github.com/buildkite/cleanroom/internal/hosttools"
	"github.com/buildkite/cleanroom/internal/imagemgr"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/vsockexec"
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

	ensurePreparedRootFSFn func(context.Context, string) (preparedRootFS, error)
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

const preparedRuntimeRootFSVersion = "v8-darwin-vz"

var virtualizationEntitlementPattern = regexp.MustCompile(`(?s)<key>\s*com\.apple\.security\.virtualization\s*</key>\s*<true\s*/?>`)

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

GUEST_PORT="$(arg_value cleanroom_guest_port || true)"
if [ -z "$GUEST_PORT" ]; then
  GUEST_PORT="10700"
fi
export CLEANROOM_VSOCK_PORT="$GUEST_PORT"

AGENT_DEV=""
if [ -c /dev/hvc1 ]; then
  AGENT_DEV="/dev/hvc1"
elif [ -c /dev/vport1p0 ]; then
  AGENT_DEV="/dev/vport1p0"
fi

if [ -n "$AGENT_DEV" ]; then
  stty raw -echo <"$AGENT_DEV" 2>/dev/null || true
  (
    while true; do
      CLEANROOM_GUEST_TRANSPORT=stdio /usr/local/bin/cleanroom-guest-agent <"$AGENT_DEV" >"$AGENT_DEV" 2>/dev/hvc0 || true
      sleep 1
    done
  ) &
fi

while true; do
  /usr/local/bin/cleanroom-guest-agent || true
  sleep 1
done
`

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
		backend.CapabilityNetworkGuestInterface:  false,
	}
}

func (a *Adapter) Run(ctx context.Context, req backend.RunRequest) (*backend.RunResult, error) {
	return a.run(ctx, req, backend.OutputStream{})
}

func (a *Adapter) RunStream(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
	return a.run(ctx, req, stream)
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
	}
	return report, nil
}

func (a *Adapter) run(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
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
		baseDir, err := paths.RunBaseDir()
		if err != nil {
			return nil, fmt.Errorf("resolve run base directory: %w", err)
		}
		runDir = filepath.Join(baseDir, req.RunID)
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, fmt.Errorf("create run directory: %w", err)
	}

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
		warningLine := "warning: " + warningText + "\n"
		stderrPrefix += warningLine
		if stream.OnStderr != nil {
			stream.OnStderr([]byte(warningLine))
		}
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
			return nil, err
		}
		return &backend.RunResult{
			RunID:       req.RunID,
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
		return nil, err
	}
	logRunNotice(a.Name(), req.RunID, kernelNotice)

	rootFSPath, imageRef, imageDigest, rootFSNotice, err := a.resolveRootFSPath(ctx, req)
	if err != nil {
		return nil, err
	}
	logRunNotice(a.Name(), req.RunID, rootFSNotice)
	if strings.TrimSpace(imageRef) != "" {
		resolvedImageRef = imageRef
	}
	if strings.TrimSpace(imageDigest) != "" {
		resolvedImageDigest = imageDigest
	}

	rootFSPath, err = filepath.Abs(rootFSPath)
	if err != nil {
		return nil, fmt.Errorf("resolve rootfs path: %w", err)
	}
	if _, err := os.Stat(rootFSPath); err != nil {
		return nil, fmt.Errorf("rootfs %s: %w", rootFSPath, err)
	}

	vmRootFSPath := filepath.Join(runDir, "rootfs-ephemeral.ext4")
	if err := copyFile(rootFSPath, vmRootFSPath); err != nil {
		return nil, fmt.Errorf("prepare per-run rootfs: %w", err)
	}
	defer func() {
		_ = os.Remove(vmRootFSPath)
	}()

	bootArgs := fmt.Sprintf("console=hvc0 root=/dev/vda rw init=/sbin/cleanroom-init cleanroom_guest_port=%d", req.GuestPort)
	consolePath := filepath.Join(runDir, "vm.console.log")

	vmPlanPath := filepath.Join(runDir, "darwin-vz-config.json")
	if err := writeJSON(vmPlanPath, map[string]any{
		"backend":      a.Name(),
		"kernel_image": kernelPath,
		"rootfs":       vmRootFSPath,
		"vcpus":        req.VCPUs,
		"memory_mib":   req.MemoryMiB,
		"guest_port":   req.GuestPort,
		"launch_secs":  req.LaunchSeconds,
		"boot_args":    bootArgs,
	}); err != nil {
		return nil, err
	}

	helper, err := startHelperSession(ctx, runDir, req.LaunchSeconds)
	if err != nil {
		return nil, fmt.Errorf("start darwin-vz helper: %w", err)
	}
	defer func() {
		if closeErr := helper.close(); closeErr != nil && stream.OnStderr != nil {
			stream.OnStderr([]byte("warning: failed to close darwin-vz helper: " + closeErr.Error() + "\n"))
		}
	}()

	proxySocketPath := filepath.Join(runDir, helperProxySocketName)
	if err := ensureUnixSocketPathFits(proxySocketPath); err != nil {
		return nil, fmt.Errorf("proxy socket path %q is too long: %w", proxySocketPath, err)
	}
	_ = os.Remove(proxySocketPath)

	startCtx, cancelStart := context.WithTimeout(ctx, time.Duration(req.LaunchSeconds)*time.Second)
	defer cancelStart()
	startRes, err := helper.request(startCtx, helperControlRequest{
		Op:              "StartVM",
		KernelPath:      kernelPath,
		RootFSPath:      vmRootFSPath,
		VCPUs:           req.VCPUs,
		MemoryMiB:       req.MemoryMiB,
		GuestPort:       req.GuestPort,
		LaunchSeconds:   req.LaunchSeconds,
		RunDir:          runDir,
		ProxySocketPath: proxySocketPath,
		ConsoleLogPath:  consolePath,
	})
	if err != nil {
		return nil, fmt.Errorf("start vm via darwin-vz helper: %w", err)
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

	guestReq := vsockexec.ExecRequest{Command: append([]string(nil), req.Command...), TTY: req.TTY}
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
			ResizeTTY: func(cols, rows uint32) error {
				return inputSender.Send(vsockexec.ExecInputFrame{Type: "resize", Cols: cols, Rows: rows})
			},
		})
	}
	if !req.TTY {
		_ = inputSender.Send(vsockexec.ExecInputFrame{Type: "eof"})
	}

	guestRes, err := vsockexec.DecodeStreamResponse(conn, vsockexec.StreamCallbacks{
		OnStdout: stream.OnStdout,
		OnStderr: stream.OnStderr,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("guest exec canceled while waiting for response: %w", ctxErr)
		}
		return nil, helper.decorateError(fmt.Errorf("decode guest exec response over darwin-vz proxy: %w", err))
	}

	message := darwinVZResultMessage(guestRes.Error)

	return &backend.RunResult{
		RunID:       req.RunID,
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

type imageArtifact struct {
	Ref        string
	Digest     string
	RootFSPath string
	CacheHit   bool
}

func (a *Adapter) resolveRootFSPath(ctx context.Context, req backend.RunRequest) (path, imageRef, imageDigest, notice string, err error) {
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
		return preparedRootFS{
			Ref:    artifact.Ref,
			Digest: artifact.Digest,
			Path:   preparedPath,
			Hit:    true,
		}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return preparedRootFS{}, fmt.Errorf("inspect prepared runtime rootfs %q: %w", preparedPath, err)
	}

	a.runtimeImageMu.Lock()
	defer a.runtimeImageMu.Unlock()

	if _, err := os.Stat(preparedPath); err == nil {
		return preparedRootFS{
			Ref:    artifact.Ref,
			Digest: artifact.Digest,
			Path:   preparedPath,
			Hit:    true,
		}, nil
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
	if err := os.Rename(tmpPath, preparedPath); err != nil {
		_ = os.Remove(tmpPath)
		if _, statErr := os.Stat(preparedPath); statErr == nil {
			return preparedRootFS{
				Ref:    artifact.Ref,
				Digest: artifact.Digest,
				Path:   preparedPath,
				Hit:    true,
			}, nil
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
	linuxName := fmt.Sprintf("cleanroom-guest-agent-linux-%s", runtime.GOARCH)
	candidates := []string{linuxName, "cleanroom-guest-agent"}

	self, err := os.Executable()
	if err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(self), linuxName))
		candidates = append(candidates, filepath.Join(filepath.Dir(self), "cleanroom-guest-agent"))
	}

	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		resolved := candidate
		if !filepath.IsAbs(candidate) {
			p, lookErr := exec.LookPath(candidate)
			if lookErr != nil {
				continue
			}
			resolved = p
		}
		info, statErr := os.Stat(resolved)
		if statErr != nil || info.IsDir() {
			continue
		}
		ok, validateErr := isLinuxGuestAgentBinary(resolved)
		if validateErr != nil {
			return "", fmt.Errorf("validate guest agent binary %q: %w", resolved, validateErr)
		}
		if ok {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("linux guest-agent binary not found for architecture %s; run `mise run install` to build and install cleanroom-guest-agent-linux-%s", runtime.GOARCH, runtime.GOARCH)
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

func helperHasVirtualizationEntitlement(helperPath string) (bool, error) {
	cmd := exec.Command("codesign", "-d", "--entitlements", ":-", helperPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if msg != "" {
			return false, fmt.Errorf("%w: %s", err, msg)
		}
		return false, err
	}
	return hasVirtualizationEntitlement(string(output)), nil
}

func hasVirtualizationEntitlement(raw string) bool {
	entitlements := strings.TrimSpace(raw)
	if entitlements == "" {
		return false
	}

	if start := strings.Index(entitlements, "<?xml"); start >= 0 {
		entitlements = entitlements[start:]
	} else if start := strings.Index(entitlements, "<plist"); start >= 0 {
		entitlements = entitlements[start:]
	}
	return virtualizationEntitlementPattern.MatchString(entitlements)
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

func (a *Adapter) installGuestRuntimeIntoRootFS(rootFSPath, guestAgentPath string) error {
	if _, err := hosttools.ResolveE2FSProgsBinary("debugfs"); err != nil {
		return fmt.Errorf("find debugfs for runtime rootfs preparation: %w", err)
	}
	initScriptPath, err := createGuestInitScript()
	if err != nil {
		return err
	}
	defer os.Remove(initScriptPath)

	if err := injectFileIntoExt4(rootFSPath, guestAgentPath, "/usr/local/bin/cleanroom-guest-agent", 0o755); err != nil {
		return fmt.Errorf("inject guest agent into rootfs image: %w", err)
	}
	if err := injectFileIntoExt4(rootFSPath, initScriptPath, "/sbin/cleanroom-init", 0o755); err != nil {
		return fmt.Errorf("inject cleanroom init into rootfs image: %w", err)
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

func injectFileIntoExt4(imagePath, srcPath, dstPath string, mode os.FileMode) error {
	cleanDst := filepath.Clean(dstPath)
	if !strings.HasPrefix(cleanDst, "/") {
		return fmt.Errorf("destination path %q must be absolute", dstPath)
	}
	if err := ensureExt4Dir(imagePath, filepath.Dir(cleanDst)); err != nil {
		return err
	}

	if ext4PathExists(imagePath, cleanDst) {
		_ = runDebugFS(imagePath, true, fmt.Sprintf("rm %s", cleanDst))
	}
	if err := runDebugFS(imagePath, true, fmt.Sprintf("write %s %s", srcPath, cleanDst)); err != nil {
		return err
	}
	modeValue := fmt.Sprintf("%#o", uint32(0o100000)|uint32(mode.Perm()))
	return runDebugFS(imagePath, true, fmt.Sprintf("set_inode_field %s mode %s", cleanDst, modeValue))
}

func ensureExt4Dir(imagePath, dir string) error {
	cleanDir := filepath.Clean(dir)
	if cleanDir == "." || cleanDir == "/" {
		return nil
	}
	if !strings.HasPrefix(cleanDir, "/") {
		cleanDir = "/" + cleanDir
	}
	parts := strings.Split(strings.TrimPrefix(cleanDir, "/"), "/")
	cur := ""
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		cur += "/" + part
		if ext4PathExists(imagePath, cur) {
			continue
		}
		if err := runDebugFS(imagePath, true, fmt.Sprintf("mkdir %s", cur)); err != nil {
			return err
		}
	}
	return nil
}

func ext4PathExists(imagePath, path string) bool {
	return runDebugFS(imagePath, false, fmt.Sprintf("stat %s", path)) == nil
}

func runDebugFS(imagePath string, writable bool, command string) error {
	debugfsBinary, err := hosttools.ResolveE2FSProgsBinary("debugfs")
	if err != nil {
		return fmt.Errorf("find debugfs for runtime rootfs preparation: %w", err)
	}

	args := make([]string, 0, 4)
	if writable {
		args = append(args, "-w")
	}
	args = append(args, "-R", command, imagePath)
	cmd := exec.Command(debugfsBinary, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("debugfs command %q failed: %w: %s", command, err, strings.TrimSpace(string(output)))
	}
	return nil
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

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func logRunNotice(backendName, runID, notice string) {
	msg := strings.TrimSpace(notice)
	if msg == "" {
		return
	}
	id := strings.TrimSpace(runID)
	if id == "" {
		log.Printf("%s: %s", backendName, msg)
		return
	}
	log.Printf("%s run_id=%s: %s", backendName, id, msg)
}
