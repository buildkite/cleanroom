package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/vsockexec"
	fcvsock "github.com/firecracker-microvm/firecracker-go-sdk/vsock"
)

type Adapter struct{}

func New() *Adapter {
	return &Adapter{}
}

func (a *Adapter) Name() string {
	return "firecracker"
}

func (a *Adapter) Run(ctx context.Context, req backend.RunRequest) (*backend.RunResult, error) {
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
		runDir = filepath.Join(os.TempDir(), "cleanroom", req.RunID)
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}

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

	cmdPath := filepath.Join(runDir, "requested-command.json")
	if err := writeJSON(cmdPath, req.Command); err != nil {
		return nil, err
	}

	if !req.Launch {
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
			return &backend.RunResult{
				RunID:      req.RunID,
				ExitCode:   0,
				LaunchedVM: false,
				PlanPath:   planPath,
				RunDir:     runDir,
				Message:    "firecracker execution plan generated; command not executed (set --launch or --host-passthrough)",
			}, nil
		}

		exitCode, err := runHostPassthrough(ctx, req.CWD, req.Command)
		if err != nil {
			return nil, err
		}
		return &backend.RunResult{
			RunID:      req.RunID,
			ExitCode:   exitCode,
			LaunchedVM: false,
			PlanPath:   planPath,
			RunDir:     runDir,
			Message:    "host passthrough execution complete (not sandboxed)",
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
		return nil, errors.New("--kernel-image and --rootfs are required when --launch is set")
	}

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

	vsockPath := filepath.Join(runDir, "vsock.sock")
	fcCfg := firecrackerConfig{
		BootSource: bootSource{
			KernelImagePath: kernelPath,
			BootArgs:        "console=ttyS0 reboot=k panic=1 pci=off",
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

	execCtx, execCancel := context.WithTimeout(ctx, time.Duration(req.LaunchSeconds)*time.Second)
	defer execCancel()

	guestResult, err := runGuestCommand(execCtx, waitCh, vsockPath, req.GuestPort, req.Command)
	if err != nil {
		return nil, err
	}

	if _, err := io.WriteString(os.Stdout, guestResult.Stdout); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(os.Stderr, guestResult.Stderr); err != nil {
		return nil, err
	}

	message := runResultMessage(req.RetainWrites, "firecracker launch and guest command execution complete")
	if guestResult.Error != "" {
		message = runResultMessage(req.RetainWrites, "firecracker launch and guest command execution completed with guest-side error detail: "+guestResult.Error)
	}

	return &backend.RunResult{
		RunID:      req.RunID,
		ExitCode:   guestResult.ExitCode,
		LaunchedVM: true,
		PlanPath:   cfgPath,
		RunDir:     runDir,
		Message:    message,
	}, nil
}

type firecrackerConfig struct {
	BootSource    bootSource    `json:"boot-source"`
	Drives        []drive       `json:"drives"`
	MachineConfig machineConfig `json:"machine-config"`
	Vsock         *vsockConfig  `json:"vsock,omitempty"`
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

func runHostPassthrough(ctx context.Context, cwd string, command []string) (int, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = cwd
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	err := cmd.Run()
	if err == nil {
		return 0, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 1, fmt.Errorf("run host passthrough command: %w", err)
}

func runGuestCommand(ctx context.Context, waitCh <-chan error, vsockPath string, guestPort uint32, command []string) (vsockexec.ExecResponse, error) {
	conn, err := dialVsockUntilReady(ctx, waitCh, vsockPath, guestPort)
	if err != nil {
		return vsockexec.ExecResponse{}, err
	}
	defer conn.Close()

	if err := vsockexec.EncodeRequest(conn, vsockexec.ExecRequest{Command: command}); err != nil {
		return vsockexec.ExecResponse{}, fmt.Errorf("send guest exec request: %w", err)
	}

	res, err := vsockexec.DecodeResponse(conn)
	if err != nil {
		return vsockexec.ExecResponse{}, fmt.Errorf("decode guest exec response: %w", err)
	}
	return res, nil
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
