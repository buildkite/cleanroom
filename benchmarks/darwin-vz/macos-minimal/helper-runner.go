package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/vsockexec"
)

const helperBinaryName = "cleanroom-darwin-vz"

type options struct {
	bundlePath   string
	helperPath   string
	agentName    string
	metricsPath  string
	runDir       string
	timeout      time.Duration
	validateOnly bool
	command      []string
}

type bundleManifest struct {
	SchemaVersion     int             `json:"schema_version"`
	OS                string          `json:"os"`
	Arch              string          `json:"arch"`
	MacOSVersion      string          `json:"macos_version,omitempty"`
	MacOSBuild        string          `json:"macos_build,omitempty"`
	VCPUs             int64           `json:"vcpus"`
	MemoryMiB         int64           `json:"memory_mib"`
	Disk              string          `json:"disk"`
	AuxiliaryStorage  string          `json:"auxiliary_storage"`
	HardwareModel     string          `json:"hardware_model"`
	MachineIdentifier string          `json:"machine_identifier"`
	Agent             agentManifest   `json:"agent"`
	UserAgent         *agentManifest  `json:"user_agent,omitempty"`
	Display           displayManifest `json:"display,omitempty"`
}

type agentManifest struct {
	Transport string `json:"transport"`
	Port      uint32 `json:"port"`
	Version   string `json:"version"`
	User      string `json:"user,omitempty"`
}

type displayManifest struct {
	WidthPx       int64 `json:"width_px,omitempty"`
	HeightPx      int64 `json:"height_px,omitempty"`
	PixelsPerInch int64 `json:"pixels_per_inch,omitempty"`
}

type resolvedBundle struct {
	ManifestURL           string
	Manifest              bundleManifest
	DiskPath              string
	AuxiliaryStoragePath  string
	HardwareModelPath     string
	MachineIdentifierPath string
	SelectedAgent         agentManifest
	SelectedAgentName     string
}

type controlRequest struct {
	Op                    string `json:"op"`
	DiskPath              string `json:"disk_path,omitempty"`
	AuxiliaryStoragePath  string `json:"auxiliary_storage_path,omitempty"`
	HardwareModelPath     string `json:"hardware_model_path,omitempty"`
	MachineIdentifierPath string `json:"machine_identifier_path,omitempty"`
	NetworkMode           string `json:"network_mode,omitempty"`
	VCPUs                 int64  `json:"vcpus,omitempty"`
	MemoryMiB             int64  `json:"memory_mib,omitempty"`
	GuestPort             uint32 `json:"guest_port,omitempty"`
	LaunchSeconds         int64  `json:"launch_seconds,omitempty"`
	RunDir                string `json:"run_dir,omitempty"`
	ProxySocketPath       string `json:"proxy_socket_path,omitempty"`
	VMID                  string `json:"vm_id,omitempty"`
	DisplayWidthPx        int64  `json:"display_width_px,omitempty"`
	DisplayHeightPx       int64  `json:"display_height_px,omitempty"`
	DisplayPixelsPerInch  int64  `json:"display_pixels_per_inch,omitempty"`
}

type controlResponse struct {
	OK              bool             `json:"ok"`
	Error           string           `json:"error,omitempty"`
	VMID            string           `json:"vm_id,omitempty"`
	ProxySocketPath string           `json:"proxy_socket_path,omitempty"`
	TimingMS        map[string]int64 `json:"timing_ms,omitempty"`
}

type smokeResult struct {
	Bundle         string           `json:"bundle"`
	Command        []string         `json:"command"`
	Helper         string           `json:"helper,omitempty"`
	StartedVM      bool             `json:"started_vm"`
	StartMS        int64            `json:"start_ms,omitempty"`
	ProxyConnectMS int64            `json:"proxy_connect_ms,omitempty"`
	ExecResponseMS int64            `json:"exec_response_ms,omitempty"`
	ExitCode       *int             `json:"exit_code,omitempty"`
	Error          string           `json:"error,omitempty"`
	SelectedAgent  string           `json:"selected_agent"`
	MacOSVersion   string           `json:"macos_version,omitempty"`
	MacOSBuild     string           `json:"macos_build,omitempty"`
	AgentVersion   string           `json:"agent_version"`
	VCPUs          int64            `json:"vcpus"`
	MemoryMiB      int64            `json:"memory_mib"`
	VMID           string           `json:"vm_id,omitempty"`
	HelperTimingMS map[string]int64 `json:"helper_timing_ms,omitempty"`
}

type helperSession struct {
	cmd        *exec.Cmd
	socketPath string

	stderr bytes.Buffer
	done   chan error

	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
	mu   sync.Mutex
}

func main() {
	code := 1
	if err := run(os.Args[1:], os.Stdout, os.Stderr, &code); err != nil {
		fmt.Fprintf(os.Stderr, "darwin-vz-macos-helper-runner: %v\n", err)
	}
	os.Exit(code)
}

func run(args []string, stdout io.Writer, stderr io.Writer, exitCode *int) error {
	opts, err := parseOptions(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			*exitCode = 0
			return nil
		}
		*exitCode = 2
		return err
	}

	bundle, err := loadBundle(opts.bundlePath, opts.agentName)
	if err != nil {
		*exitCode = 1
		return err
	}
	helperPath, err := resolveHelperPath(opts.helperPath)
	if err != nil && !opts.validateOnly {
		*exitCode = 1
		return err
	}

	result := smokeResult{
		Bundle:        bundle.ManifestURL,
		Command:       opts.command,
		Helper:        helperPath,
		SelectedAgent: bundle.SelectedAgentName,
		MacOSVersion:  bundle.Manifest.MacOSVersion,
		MacOSBuild:    bundle.Manifest.MacOSBuild,
		AgentVersion:  bundle.SelectedAgent.Version,
		VCPUs:         bundle.Manifest.VCPUs,
		MemoryMiB:     bundle.Manifest.MemoryMiB,
	}
	defer func() {
		if writeErr := writeMetrics(result, opts.metricsPath); writeErr != nil {
			fmt.Fprintf(stderr, "write metrics: %v\n", writeErr)
		}
	}()

	if opts.validateOnly {
		*exitCode = 0
		return nil
	}

	runDir, cleanup, err := prepareRunDir(opts.runDir)
	if err != nil {
		result.Error = err.Error()
		*exitCode = 1
		return err
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	helper, err := startHelper(ctx, helperPath, filepath.Join(runDir, "vz-helper.sock"))
	if err != nil {
		result.Error = err.Error()
		*exitCode = 1
		return err
	}
	defer helper.close()

	startedAt := time.Now()
	startRes, err := helper.request(ctx, controlRequest{
		Op:                    "StartMacOSVM",
		DiskPath:              bundle.DiskPath,
		AuxiliaryStoragePath:  bundle.AuxiliaryStoragePath,
		HardwareModelPath:     bundle.HardwareModelPath,
		MachineIdentifierPath: bundle.MachineIdentifierPath,
		NetworkMode:           "none",
		VCPUs:                 bundle.Manifest.VCPUs,
		MemoryMiB:             bundle.Manifest.MemoryMiB,
		GuestPort:             bundle.SelectedAgent.Port,
		LaunchSeconds:         int64(opts.timeout.Seconds()),
		RunDir:                runDir,
		ProxySocketPath:       filepath.Join(runDir, "vz-proxy.sock"),
		DisplayWidthPx:        bundle.Manifest.Display.WidthPx,
		DisplayHeightPx:       bundle.Manifest.Display.HeightPx,
		DisplayPixelsPerInch:  bundle.Manifest.Display.PixelsPerInch,
	})
	result.StartMS = time.Since(startedAt).Milliseconds()
	if err != nil {
		result.Error = err.Error()
		*exitCode = 1
		return err
	}
	result.StartedVM = true
	result.VMID = startRes.VMID
	result.HelperTimingMS = startRes.TimingMS
	defer stopHelperVM(helper, startRes.VMID, stderr)

	proxyPath := strings.TrimSpace(startRes.ProxySocketPath)
	if proxyPath == "" {
		proxyPath = filepath.Join(runDir, "vz-proxy.sock")
	}
	proxyConn, err := dialUnix(ctx, proxyPath)
	result.ProxyConnectMS = time.Since(startedAt).Milliseconds()
	if err != nil {
		result.Error = err.Error()
		*exitCode = 1
		return err
	}
	defer proxyConn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := proxyConn.SetDeadline(deadline); err != nil {
			result.Error = err.Error()
			*exitCode = 1
			return err
		}
		defer proxyConn.SetDeadline(time.Time{})
	}

	if err := vsockexec.EncodeRequest(proxyConn, vsockexec.ExecRequest{Command: opts.command}); err != nil {
		result.Error = err.Error()
		*exitCode = 1
		return err
	}
	if err := vsockexec.EncodeInputFrame(proxyConn, vsockexec.ExecInputFrame{Type: "eof"}); err != nil {
		result.Error = err.Error()
		*exitCode = 1
		return err
	}
	execRes, err := vsockexec.DecodeStreamResponse(proxyConn, vsockexec.StreamCallbacks{
		OnStdout: func(b []byte) { _, _ = stdout.Write(b) },
		OnStderr: func(b []byte) { _, _ = stderr.Write(b) },
	})
	result.ExecResponseMS = time.Since(startedAt).Milliseconds()
	if err != nil {
		result.Error = err.Error()
		*exitCode = 1
		return err
	}
	result.ExitCode = &execRes.ExitCode
	result.Error = execRes.Error
	*exitCode = execRes.ExitCode
	return nil
}

func parseOptions(args []string) (options, error) {
	opts := options{
		agentName: "root",
		timeout:   120 * time.Second,
		command:   []string{"/usr/bin/sw_vers"},
	}
	fs := flag.NewFlagSet("darwin-vz-macos-helper-runner", flag.ContinueOnError)
	fs.StringVar(&opts.bundlePath, "bundle", "", "bundle.json or bundle directory")
	fs.StringVar(&opts.helperPath, "helper", "", "cleanroom-darwin-vz binary or .app path")
	fs.StringVar(&opts.agentName, "agent", opts.agentName, "agent endpoint: root or user")
	fs.StringVar(&opts.metricsPath, "metrics", "", "write result JSON to path; omit to write to stderr")
	fs.StringVar(&opts.runDir, "run-dir", "", "helper run directory; defaults to a temporary directory")
	fs.BoolVar(&opts.validateOnly, "validate-only", false, "validate bundle metadata without starting the helper")
	timeoutSeconds := fs.Int("timeout", int(opts.timeout.Seconds()), "helper, VM, and command timeout in seconds")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s --bundle <bundle.json|bundle-dir> [options] [-- <command> [args...]]\n", fs.Name())
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if strings.TrimSpace(opts.bundlePath) == "" {
		return options{}, errors.New("missing --bundle")
	}
	if opts.agentName != "root" && opts.agentName != "user" {
		return options{}, errors.New("--agent must be root or user")
	}
	if *timeoutSeconds <= 0 {
		return options{}, errors.New("--timeout must be greater than zero")
	}
	opts.timeout = time.Duration(*timeoutSeconds) * time.Second
	if fs.NArg() > 0 {
		opts.command = append([]string(nil), fs.Args()...)
	}
	if len(opts.command) == 0 || strings.TrimSpace(opts.command[0]) == "" {
		return options{}, errors.New("command after -- must not be empty")
	}
	return opts, nil
}

func loadBundle(path string, agentName string) (resolvedBundle, error) {
	manifestPath, err := manifestPath(path)
	if err != nil {
		return resolvedBundle{}, err
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return resolvedBundle{}, fmt.Errorf("read bundle manifest: %w", err)
	}
	var manifest bundleManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return resolvedBundle{}, fmt.Errorf("decode bundle manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return resolvedBundle{}, err
	}
	agent, err := selectAgent(manifest, agentName)
	if err != nil {
		return resolvedBundle{}, err
	}
	baseDir := filepath.Dir(manifestPath)
	resolved := resolvedBundle{
		ManifestURL:           manifestPath,
		Manifest:              manifest,
		DiskPath:              resolveBundlePath(baseDir, manifest.Disk),
		AuxiliaryStoragePath:  resolveBundlePath(baseDir, manifest.AuxiliaryStorage),
		HardwareModelPath:     resolveBundlePath(baseDir, manifest.HardwareModel),
		MachineIdentifierPath: resolveBundlePath(baseDir, manifest.MachineIdentifier),
		SelectedAgent:         agent,
		SelectedAgentName:     agentName,
	}
	for field, path := range map[string]string{
		"disk":               resolved.DiskPath,
		"auxiliary_storage":  resolved.AuxiliaryStoragePath,
		"hardware_model":     resolved.HardwareModelPath,
		"machine_identifier": resolved.MachineIdentifierPath,
	} {
		if err := requireFile(path); err != nil {
			return resolvedBundle{}, fmt.Errorf("%s: %w", field, err)
		}
	}
	return resolved, nil
}

func manifestPath(path string) (string, error) {
	expanded := expandPath(path)
	info, err := os.Stat(expanded)
	if err != nil {
		return "", fmt.Errorf("bundle path: %w", err)
	}
	if info.IsDir() {
		expanded = filepath.Join(expanded, "bundle.json")
	}
	if err := requireFile(expanded); err != nil {
		return "", fmt.Errorf("bundle manifest: %w", err)
	}
	return filepath.Abs(expanded)
}

func validateManifest(manifest bundleManifest) error {
	if manifest.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema_version %d", manifest.SchemaVersion)
	}
	if manifest.OS != "macos" {
		return errors.New("bundle os must be macos")
	}
	if manifest.Arch != "arm64" {
		return errors.New("bundle arch must be arm64")
	}
	if manifest.VCPUs <= 0 {
		return errors.New("vcpus must be greater than zero")
	}
	if manifest.MemoryMiB < 1024 {
		return errors.New("memory_mib must be at least 1024")
	}
	if err := validateAgent(manifest.Agent, "agent"); err != nil {
		return err
	}
	if manifest.UserAgent != nil {
		if err := validateAgent(*manifest.UserAgent, "user_agent"); err != nil {
			return err
		}
		if manifest.UserAgent.Port == manifest.Agent.Port {
			return errors.New("user_agent.port must differ from agent.port")
		}
	}
	if manifest.Display.WidthPx < 0 || manifest.Display.HeightPx < 0 || manifest.Display.PixelsPerInch < 0 {
		return errors.New("display dimensions must not be negative")
	}
	return nil
}

func validateAgent(agent agentManifest, field string) error {
	if agent.Transport != "virtio_socket" {
		return fmt.Errorf("%s.transport must be virtio_socket", field)
	}
	if agent.Port == 0 {
		return fmt.Errorf("%s.port must be greater than zero", field)
	}
	if strings.TrimSpace(agent.Version) == "" {
		return fmt.Errorf("%s.version must not be empty", field)
	}
	return nil
}

func selectAgent(manifest bundleManifest, name string) (agentManifest, error) {
	switch name {
	case "root":
		return manifest.Agent, nil
	case "user":
		if manifest.UserAgent == nil {
			return agentManifest{}, errors.New("bundle does not declare user_agent")
		}
		return *manifest.UserAgent, nil
	default:
		return agentManifest{}, errors.New("--agent must be root or user")
	}
}

func resolveBundlePath(baseDir string, path string) string {
	expanded := expandPath(path)
	if filepath.IsAbs(expanded) {
		return expanded
	}
	return filepath.Join(baseDir, expanded)
}

func resolveHelperPath(path string) (string, error) {
	candidates := []string{}
	if strings.TrimSpace(path) != "" {
		candidates = append(candidates, path)
	}
	if env := strings.TrimSpace(os.Getenv("CLEANROOM_DARWIN_VZ_HELPER")); env != "" {
		candidates = append(candidates, env)
	}
	candidates = append(candidates,
		filepath.Join("dist", helperBinaryName+".app"),
		filepath.Join("dist", helperBinaryName+"-test.app"),
		filepath.Join("dist", helperBinaryName),
	)
	if found, err := exec.LookPath(helperBinaryName); err == nil {
		candidates = append(candidates, found)
	}
	for _, candidate := range candidates {
		resolved := helperExecutablePath(expandPath(candidate))
		if err := requireExecutable(resolved); err == nil {
			return filepath.Abs(resolved)
		}
	}
	return "", errors.New("cleanroom-darwin-vz helper not found; pass --helper or set CLEANROOM_DARWIN_VZ_HELPER")
}

func helperExecutablePath(path string) string {
	if strings.HasSuffix(path, ".app") {
		return filepath.Join(path, "Contents", "MacOS", helperBinaryName)
	}
	return path
}

func prepareRunDir(path string) (string, func(), error) {
	if strings.TrimSpace(path) == "" {
		dir, err := os.MkdirTemp("/tmp", "crmhr-*")
		if err != nil {
			return "", func() {}, err
		}
		return dir, func() { _ = os.RemoveAll(dir) }, nil
	}
	dir, err := filepath.Abs(expandPath(path))
	if err != nil {
		return "", func() {}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", func() {}, err
	}
	return dir, func() {}, nil
}

func startHelper(ctx context.Context, helperPath string, socketPath string) (*helperSession, error) {
	_ = os.Remove(socketPath)
	cmd := exec.Command(helperPath, "--socket", socketPath)
	session := &helperSession{
		cmd:        cmd,
		socketPath: socketPath,
		done:       make(chan error, 1),
	}
	cmd.Stderr = &session.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start helper: %w", err)
	}
	go func() {
		session.done <- cmd.Wait()
		close(session.done)
	}()
	conn, err := waitForHelper(ctx, socketPath, session.done)
	if err != nil {
		_ = session.close()
		return nil, fmt.Errorf("connect helper control socket: %w", session.decorateError(err))
	}
	session.conn = conn
	session.enc = json.NewEncoder(conn)
	session.dec = json.NewDecoder(conn)
	return session, nil
}

func waitForHelper(ctx context.Context, socketPath string, helperDone <-chan error) (net.Conn, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	dialer := net.Dialer{}
	for {
		conn, err := dialer.DialContext(ctx, "unix", socketPath)
		if err == nil {
			return conn, nil
		}
		select {
		case doneErr := <-helperDone:
			if doneErr == nil {
				return nil, errors.New("helper exited before control socket was ready")
			}
			return nil, fmt.Errorf("helper exited before control socket was ready: %w", doneErr)
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for helper control socket: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *helperSession) request(ctx context.Context, req controlRequest) (controlResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		_ = s.conn.SetDeadline(deadline)
		defer s.conn.SetDeadline(time.Time{})
	}
	if err := s.enc.Encode(req); err != nil {
		return controlResponse{}, s.decorateError(fmt.Errorf("send helper request %q: %w", req.Op, err))
	}
	var res controlResponse
	if err := s.dec.Decode(&res); err != nil {
		return controlResponse{}, s.decorateError(fmt.Errorf("decode helper response %q: %w", req.Op, err))
	}
	if !res.OK {
		msg := strings.TrimSpace(res.Error)
		if msg == "" {
			msg = "unknown helper error"
		}
		return controlResponse{}, s.decorateError(fmt.Errorf("helper %s failed: %s", req.Op, msg))
	}
	return res, nil
}

func (s *helperSession) close() error {
	if s == nil {
		return nil
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(os.Interrupt)
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			_ = s.cmd.Process.Kill()
			<-s.done
		}
	}
	_ = os.Remove(s.socketPath)
	return nil
}

func (s *helperSession) decorateError(err error) error {
	stderr := strings.TrimSpace(s.stderr.String())
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w; helper stderr: %s", err, stderr)
}

func stopHelperVM(helper *helperSession, vmID string, stderr io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := helper.request(ctx, controlRequest{Op: "StopVM", VMID: vmID}); err != nil {
		fmt.Fprintf(stderr, "stop helper vm: %v\n", err)
	}
}

func dialUnix(ctx context.Context, socketPath string) (net.Conn, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	dialer := net.Dialer{}
	for {
		conn, err := dialer.DialContext(ctx, "unix", socketPath)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for unix socket %q: %w", socketPath, ctx.Err())
		case <-ticker.C:
		}
	}
}

func writeMetrics(result smokeResult, path string) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if strings.TrimSpace(path) == "" {
		fmt.Fprintln(os.Stderr, string(encoded))
		return nil
	}
	return os.WriteFile(expandPath(path), append(encoded, '\n'), 0o644)
}

func requireFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	return nil
}

func requireExecutable(path string) error {
	if err := requireFile(path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	return nil
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}
