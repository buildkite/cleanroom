package firecracker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/vsockexec"
	fcvsock "github.com/firecracker-microvm/firecracker-go-sdk/vsock"
)

type Adapter struct{}

const maxWorkspaceArchiveBytes = 256 << 20
const runObservabilityFile = "run-observability.json"

func New() *Adapter {
	return &Adapter{}
}

func (a *Adapter) Name() string {
	return "firecracker"
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

	if req.RootFSPath == "" {
		appendCheck("rootfs", "warn", "rootfs not configured (required for --launch)")
	} else if _, err := os.Stat(req.RootFSPath); err != nil {
		appendCheck("rootfs", "fail", fmt.Sprintf("rootfs not accessible: %v", err))
	} else {
		appendCheck("rootfs", "pass", fmt.Sprintf("rootfs configured: %s", req.RootFSPath))
	}

	if req.GuestPort == 0 {
		appendCheck("vsock_port", "pass", fmt.Sprintf("using default guest vsock port %d", vsockexec.DefaultPort))
	} else {
		appendCheck("vsock_port", "pass", fmt.Sprintf("configured guest vsock port %d", req.GuestPort))
	}
	mode := strings.TrimSpace(strings.ToLower(req.WorkspaceMode))
	if mode == "" {
		mode = "copy"
	}
	if mode != "copy" {
		appendCheck("workspace_mode", "fail", fmt.Sprintf("workspace mode %q is not implemented for firecracker (supported: copy)", mode))
	} else {
		appendCheck("workspace_mode", "pass", "workspace mode copy is enabled")
	}
	persist := strings.TrimSpace(strings.ToLower(req.WorkspacePersist))
	if persist == "" {
		persist = "discard"
	}
	if persist != "discard" {
		appendCheck("workspace_persist", "warn", fmt.Sprintf("workspace persist %q is not implemented yet for firecracker copy mode (current behavior: discard)", persist))
	} else {
		appendCheck("workspace_persist", "pass", "workspace changes are discarded after each launched run")
	}
	access := strings.TrimSpace(strings.ToLower(req.WorkspaceAccess))
	if access == "" {
		access = "rw"
	}
	if access != "rw" && access != "ro" {
		appendCheck("workspace_access", "fail", fmt.Sprintf("invalid workspace access %q (expected rw or ro)", access))
	} else {
		appendCheck("workspace_access", "pass", fmt.Sprintf("workspace access configured as %s", access))
	}
	appendCheck("workspace_path", "pass", fmt.Sprintf("host workspace path: %s", req.WorkspaceHost))
	policyRules := 0
	policyRulesStatus := "warn"
	policyRulesMessage := "policy not loaded; cannot verify network allow entries"
	if req.Policy != nil {
		policyRules = len(req.Policy.Allow)
		policyRulesStatus = "pass"
		policyRulesMessage = fmt.Sprintf("loaded %d policy allow entries", policyRules)
	}
	appendCheck("network_policy_rules", policyRulesStatus, policyRulesMessage)

	for _, cmd := range []string{"ip", "iptables", "sysctl", "sudo"} {
		if _, err := exec.LookPath(cmd); err != nil {
			appendCheck("network_cmd_"+cmd, "fail", fmt.Sprintf("missing required host command %q", cmd))
		} else {
			appendCheck("network_cmd_"+cmd, "pass", fmt.Sprintf("found host command %q", cmd))
		}
	}
	if err := runRootCommand(context.Background(), "true"); err != nil {
		appendCheck("network_sudo_nopasswd", "warn", fmt.Sprintf("sudo -n is not ready for network setup/cleanup: %v", err))
	} else {
		appendCheck("network_sudo_nopasswd", "pass", "sudo -n works for privileged network setup")
	}
	if err := runRootCommand(context.Background(), "ip", "link", "show"); err != nil {
		appendCheck("network_sudo_ip", "warn", fmt.Sprintf("sudo -n ip link show failed: %v", err))
	} else {
		appendCheck("network_sudo_ip", "pass", "sudo -n can execute ip commands")
	}

	return report, nil
}

func (a *Adapter) Run(ctx context.Context, req backend.RunRequest) (*backend.RunResult, error) {
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
		req.GuestCID = 3
	}
	if req.GuestPort == 0 {
		req.GuestPort = vsockexec.DefaultPort
	}
	if req.LaunchSeconds <= 0 {
		req.LaunchSeconds = 30
	}
	if req.WorkspaceAccess == "" {
		req.WorkspaceAccess = "rw"
	}
	if req.WorkspaceAccess != "rw" && req.WorkspaceAccess != "ro" {
		return nil, fmt.Errorf("invalid workspace access %q: expected rw or ro", req.WorkspaceAccess)
	}
	if req.WorkspaceMode == "" {
		req.WorkspaceMode = "copy"
	}
	if req.WorkspaceMode != "copy" {
		return nil, fmt.Errorf("workspace mode %q is not implemented for firecracker yet; supported mode: copy", req.WorkspaceMode)
	}
	if req.WorkspacePersist == "" {
		req.WorkspacePersist = "discard"
	}
	if req.WorkspacePersist != "discard" {
		return nil, fmt.Errorf("workspace persist %q is not implemented for firecracker copy mode yet; supported value: discard", req.WorkspacePersist)
	}
	if req.WorkspaceHost == "" {
		req.WorkspaceHost = req.CWD
	}

	cmdPath := filepath.Join(runDir, "requested-command.json")
	if err := writeJSON(cmdPath, req.Command); err != nil {
		return nil, err
	}

	if !req.Launch {
		observation.Phase = "plan_or_host_passthrough"
		planPath := filepath.Join(runDir, "passthrough-plan.json")
		plan := map[string]any{
			"backend":      "firecracker",
			"mode":         "plan-only",
			"command_path": cmdPath,
		}
		if req.HostPassthrough {
			plan["mode"] = "host-passthrough"
			plan["not_sandboxed"] = true
		}
		if err := writeJSON(planPath, plan); err != nil {
			return nil, err
		}

		if !req.HostPassthrough {
			observation.PlanPath = planPath
			observation.RunDir = runDir
			return &backend.RunResult{
				RunID:      req.RunID,
				ExitCode:   0,
				LaunchedVM: false,
				PlanPath:   planPath,
				RunDir:     runDir,
				Message:    "firecracker execution plan generated; command not executed (set --dry-run or --host-passthrough for non-launch modes)",
			}, nil
		}

		exitCode, stdout, stderr, err := runHostPassthrough(ctx, req.CWD, req.Command)
		if err != nil {
			return nil, err
		}
		observation.PlanPath = planPath
		observation.RunDir = runDir
		observation.ExitCode = exitCode
		return &backend.RunResult{
			RunID:      req.RunID,
			ExitCode:   exitCode,
			LaunchedVM: false,
			PlanPath:   planPath,
			RunDir:     runDir,
			Message:    "host passthrough execution complete (not sandboxed)",
			Stdout:     stdout,
			Stderr:     stderr,
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

	if req.KernelImagePath == "" || req.RootFSPath == "" {
		return nil, errors.New("kernel_image and rootfs must be configured for launched execution; use --dry-run or --host-passthrough for non-launch modes")
	}
	observation.Phase = "launch"

	kernelPath, err := filepath.Abs(req.KernelImagePath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(kernelPath); err != nil {
		return nil, fmt.Errorf("kernel image %s: %w", kernelPath, err)
	}

	rootfsPath, err := filepath.Abs(req.RootFSPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(rootfsPath); err != nil {
		return nil, fmt.Errorf("rootfs %s: %w", rootfsPath, err)
	}

	vmRootFSPath := filepath.Join(runDir, "rootfs-retained.ext4")
	if !req.RetainWrites {
		vmRootFSPath = filepath.Join(runDir, "rootfs-ephemeral.ext4")
		defer os.Remove(vmRootFSPath)
	}
	if err := copyFile(rootfsPath, vmRootFSPath); err != nil {
		return nil, fmt.Errorf("prepare per-run rootfs: %w", err)
	}

	networkSetupStart := time.Now()
	networkCfg, cleanupNetwork, err := setupHostNetwork(ctx, req.RunID, req.Policy.Allow)
	if err != nil {
		return nil, fmt.Errorf("setup host network: %w", err)
	}
	observation.NetworkSetupMS = time.Since(networkSetupStart).Milliseconds()
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
				"console=ttyS0 reboot=k panic=1 pci=off init=/sbin/cleanroom-init random.trust_cpu=on cleanroom_guest_ip=%s cleanroom_guest_gw=%s cleanroom_guest_mask=24 cleanroom_guest_dns=1.1.1.1",
				networkCfg.GuestIP,
				networkCfg.HostIP,
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

	if err := fcCmd.Start(); err != nil {
		return nil, fmt.Errorf("start firecracker: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- fcCmd.Wait()
	}()
	defer stopVM(fcCmd, waitCh)

	bootCtx, bootCancel := context.WithTimeout(ctx, time.Duration(req.LaunchSeconds)*time.Second)
	defer bootCancel()

	workspaceArchive, err := createWorkspaceArchive(req.WorkspaceHost)
	if err != nil {
		return nil, fmt.Errorf("prepare workspace copy from %s: %w", req.WorkspaceHost, err)
	}
	guestReq := vsockexec.ExecRequest{
		Command:         req.Command,
		Dir:             "/workspace",
		WorkspaceTarGz:  workspaceArchive,
		WorkspaceAccess: req.WorkspaceAccess,
		Env:             buildGuestEnv(req.WorkspaceHost),
	}
	seed := make([]byte, 64)
	if _, err := cryptorand.Read(seed); err == nil {
		guestReq.EntropySeed = seed
	}
	guestResult, guestTiming, err := runGuestCommand(bootCtx, ctx, waitCh, vsockPath, req.GuestPort, guestReq)
	if err != nil {
		return nil, err
	}
	observation.VMReadyMS = guestTiming.WaitForAgent.Milliseconds()
	observation.VsockWaitMS = guestTiming.WaitForAgent.Milliseconds()
	observation.GuestExecMS = guestTiming.CommandRun.Milliseconds()
	if guestResult.Error != "" && strings.TrimSpace(guestResult.Stderr) == "" {
		guestResult.Stderr = guestResult.Error + "\n"
	}

	message := runResultMessage(req.RetainWrites, "firecracker launch and guest command execution complete")
	if guestResult.Error != "" {
		message = runResultMessage(req.RetainWrites, "firecracker launch and guest command execution completed with guest-side error detail: "+guestResult.Error)
	}

	observation.PlanPath = cfgPath
	observation.RunDir = runDir
	observation.ExitCode = guestResult.ExitCode
	observation.GuestError = guestResult.Error

	timingSummary := fmt.Sprintf("timings boot=%s vsock_wait=%s exec=%s", guestTiming.WaitForAgent, guestTiming.WaitForAgent, guestTiming.CommandRun)

	return &backend.RunResult{
		RunID:      req.RunID,
		ExitCode:   guestResult.ExitCode,
		LaunchedVM: true,
		PlanPath:   cfgPath,
		RunDir:     runDir,
		Message:    message + "; " + timingSummary,
		Stdout:     guestResult.Stdout,
		Stderr:     guestResult.Stderr,
	}, nil
}

type firecrackerRunObservation struct {
	RunID          string `json:"run_id"`
	Backend        string `json:"backend"`
	LaunchedVM     bool   `json:"launched_vm"`
	Phase          string `json:"phase"`
	PlanPath       string `json:"plan_path,omitempty"`
	RunDir         string `json:"run_dir,omitempty"`
	ExitCode       int    `json:"exit_code,omitempty"`
	GuestError     string `json:"guest_error,omitempty"`
	NetworkTap     string `json:"network_tap,omitempty"`
	NetworkHostIP  string `json:"network_host_ip,omitempty"`
	NetworkGuestIP string `json:"network_guest_ip,omitempty"`
	NetworkSetupMS int64  `json:"network_setup_ms,omitempty"`
	VMReadyMS      int64  `json:"vm_ready_ms,omitempty"`
	VsockWaitMS    int64  `json:"vsock_wait_ms,omitempty"`
	GuestExecMS    int64  `json:"guest_exec_ms,omitempty"`
	CleanupMS      int64  `json:"cleanup_ms,omitempty"`
	TotalMS        int64  `json:"total_ms,omitempty"`
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

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func runResultMessage(retainWrites bool, base string) string {
	if retainWrites {
		return base + "; rootfs writes retained in run directory"
	}
	return base + "; rootfs writes discarded after run"
}

func runHostPassthrough(ctx context.Context, cwd string, command []string) (int, string, string, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = cwd
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return 0, stdout.String(), stderr.String(), nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), stdout.String(), stderr.String(), nil
	}
	return 1, stdout.String(), stderr.String(), fmt.Errorf("run host passthrough command: %w", err)
}

type guestExecTiming struct {
	WaitForAgent time.Duration
	CommandRun   time.Duration
}

func runGuestCommand(bootCtx context.Context, execCtx context.Context, waitCh <-chan error, vsockPath string, guestPort uint32, req vsockexec.ExecRequest) (vsockexec.ExecResponse, guestExecTiming, error) {
	waitStart := time.Now()
	conn, err := dialVsockUntilReady(bootCtx, waitCh, vsockPath, guestPort)
	if err != nil {
		return vsockexec.ExecResponse{}, guestExecTiming{}, err
	}
	timing := guestExecTiming{WaitForAgent: time.Since(waitStart)}
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
	res, err := vsockexec.DecodeResponse(conn)
	if err != nil {
		if ctxErr := execCtx.Err(); ctxErr != nil {
			return vsockexec.ExecResponse{}, guestExecTiming{}, fmt.Errorf("guest exec canceled while waiting for response: %w", ctxErr)
		}
		return vsockexec.ExecResponse{}, guestExecTiming{}, fmt.Errorf("decode guest exec response: %w", err)
	}
	timing.CommandRun = time.Since(commandStart)
	return res, timing, nil
}

func dialVsockUntilReady(ctx context.Context, waitCh <-chan error, vsockPath string, guestPort uint32) (io.ReadWriteCloser, error) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		conn, err := fcvsock.DialContext(ctx, vsockPath, guestPort)
		if err == nil {
			return conn, nil
		}

		select {
		case waitErr := <-waitCh:
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

func stopVM(fcCmd *exec.Cmd, waitCh <-chan error) {
	if fcCmd.Process != nil {
		_ = fcCmd.Process.Kill()
	}
	select {
	case <-waitCh:
	case <-time.After(2 * time.Second):
	}
}

func createWorkspaceArchive(root string) ([]byte, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("workspace path is empty")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace path %s is not a directory", root)
	}

	var compressed bytes.Buffer
	limited := &limitedWriter{w: &compressed, limit: maxWorkspaceArchiveBytes}
	gzw := gzip.NewWriter(limited)
	tw := tar.NewWriter(gzw)

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Symlinks are excluded in MVP copy mode to avoid path traversal edge cases.
			return nil
		}

		var link string
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if info.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if walkErr != nil {
		_ = tw.Close()
		_ = gzw.Close()
		return nil, walkErr
	}
	if err := tw.Close(); err != nil {
		_ = gzw.Close()
		return nil, err
	}
	if err := gzw.Close(); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

type limitedWriter struct {
	w     io.Writer
	limit int
	total int
}

type hostNetworkConfig struct {
	TapName string
	HostIP  string
	GuestIP string
}

type iptablesForwardRule struct {
	Protocol string
	DestIP   string
	DestPort int
}

type ipLookupFunc func(ctx context.Context, host string) ([]net.IP, error)
type rootCommandFunc func(ctx context.Context, args ...string) error

func setupHostNetwork(ctx context.Context, runID string, allow []policy.AllowRule) (hostNetworkConfig, func(), error) {
	lookup := func(ctx context.Context, host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip4", host)
	}
	return setupHostNetworkWithDeps(ctx, runID, allow, lookup, runRootCommand)
}

func setupHostNetworkWithDeps(ctx context.Context, runID string, allow []policy.AllowRule, lookup ipLookupFunc, runCommand rootCommandFunc) (hostNetworkConfig, func(), error) {
	tapName := tapNameFromRunID(runID)
	hostIP, guestIP := hostGuestIPs(runID)
	hostCIDR := hostIP + "/24"
	guestCIDR := guestIP + "/32"
	const dnsServer = "1.1.1.1"

	forwardRules, err := resolveForwardRulesWithLookup(ctx, allow, lookup)
	if err != nil {
		return hostNetworkConfig{}, func() {}, err
	}

	setupRun := func(args ...string) error {
		return runCommand(ctx, args...)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	cleanupRun := func(args ...string) error {
		return runCommand(cleanupCtx, args...)
	}
	cleanupCmds := make([][]string, 0, 16)
	cleanup := func() {
		defer cleanupCancel()
		for i := len(cleanupCmds) - 1; i >= 0; i-- {
			_ = cleanupRun(cleanupCmds[i]...)
		}
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
	if err := setupRun("iptables", "-A", "FORWARD", "-o", tapName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("install forward return-path rule for %s: %w", tapName, err)
	}
	addCleanup("iptables", "-D", "FORWARD", "-o", tapName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT")

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
		TapName: tapName,
		HostIP:  hostIP,
		GuestIP: guestIP,
	}, cleanup, nil
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

func runRootCommand(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"-n"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = "no stderr output"
		}
		return fmt.Errorf("%s: %w (%s)", strings.Join(args, " "), err, msg)
	}
	return nil
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

func buildGuestEnv(workspaceHost string) []string {
	guestTrusted := make([]string, 0, 2)
	if _, err := os.Stat(filepath.Join(workspaceHost, ".mise.toml")); err == nil {
		guestTrusted = append(guestTrusted, "/workspace/.mise.toml")
	}
	if _, err := os.Stat(filepath.Join(workspaceHost, ".mise", "config.toml")); err == nil {
		guestTrusted = append(guestTrusted, "/workspace/.mise/config.toml")
	}
	if len(guestTrusted) == 0 {
		return nil
	}
	return []string{
		"MISE_TRUSTED_CONFIG_PATHS=" + strings.Join(guestTrusted, ":"),
	}
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.total+len(p) > l.limit {
		return 0, fmt.Errorf("workspace archive exceeds max size of %d bytes", l.limit)
	}
	n, err := l.w.Write(p)
	l.total += n
	return n, err
}
