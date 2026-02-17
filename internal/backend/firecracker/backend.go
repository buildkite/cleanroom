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
	if req.LaunchSeconds <= 0 {
		req.LaunchSeconds = 10
	}

	cmdPath := filepath.Join(runDir, "requested-command.json")
	if err := writeJSON(cmdPath, req.Command); err != nil {
		return nil, err
	}

	vmRootFSPath := ""
	rootfsReadOnly := true
	if !req.Launch {
		planPath := filepath.Join(runDir, "passthrough-plan.json")
		plan := map[string]any{
			"backend":       "firecracker",
			"mode":          "host-passthrough",
			"not_sandboxed": true,
			"command_path":  cmdPath,
		}
		if err := writeJSON(planPath, plan); err != nil {
			return nil, err
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

	rootfsReadOnly = false
	if req.RetainWrites {
		vmRootFSPath = filepath.Join(runDir, "rootfs-retained.ext4")
	} else {
		vmRootFSPath = filepath.Join(runDir, "rootfs-ephemeral.ext4")
		defer os.Remove(vmRootFSPath)
	}
	if err := copyFile(rootfsPath, vmRootFSPath); err != nil {
		return nil, fmt.Errorf("prepare per-run rootfs: %w", err)
	}

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
				IsReadOnly:   rootfsReadOnly,
			},
		},
		MachineConfig: machineConfig{
			VCPUCount:  req.VCPUs,
			MemSizeMiB: req.MemoryMiB,
			SMT:        false,
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

	timer := time.NewTimer(time.Duration(req.LaunchSeconds) * time.Second)
	defer timer.Stop()

	select {
	case err := <-waitCh:
		if err != nil {
			return nil, fmt.Errorf("firecracker exited early: %w", err)
		}
		return &backend.RunResult{
			RunID:      req.RunID,
			ExitCode:   0,
			LaunchedVM: true,
			PlanPath:   cfgPath,
			RunDir:     runDir,
			Message:    runResultMessage(req.RetainWrites, "firecracker exited normally (guest command execution wiring is not implemented yet)"),
		}, nil
	case <-timer.C:
		_ = fcCmd.Process.Kill()
		<-waitCh
		return &backend.RunResult{
			RunID:      req.RunID,
			ExitCode:   0,
			LaunchedVM: true,
			PlanPath:   cfgPath,
			RunDir:     runDir,
			Message:    runResultMessage(req.RetainWrites, "firecracker launched for MVP timeout window; guest command execution over vsock is pending"),
		}, nil
	case <-ctx.Done():
		_ = fcCmd.Process.Kill()
		<-waitCh
		return nil, ctx.Err()
	}
}

type firecrackerConfig struct {
	BootSource    bootSource    `json:"boot-source"`
	Drives        []drive       `json:"drives"`
	MachineConfig machineConfig `json:"machine-config"`
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
