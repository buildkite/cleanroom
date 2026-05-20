package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/backend/darwinvz"
	"github.com/buildkite/cleanroom/internal/backend/firecracker"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type DaemonCommand struct {
	Action        string `arg:"" required:"" help:"Daemon action (install, uninstall, status, start, stop, restart)"`
	Force         bool   `help:"Start a stopped daemon during restart"`
	Restart       bool   `help:"Restart or start the daemon after install so the current definition is live"`
	InitConfig    bool   `help:"Create a default runtime config before install if the config file is missing"`
	DryRun        bool   `help:"Preview daemon install changes without mutating the service manager"`
	User          bool   `help:"Use user daemon scope (launchd only; install/uninstall/status/start/stop/restart actions)"`
	System        bool   `help:"Use system daemon scope (linux only; install/uninstall/status/start/stop/restart actions)"`
	JSON          bool   `help:"Print daemon status as JSON (status action only)"`
	Listen        string `help:"Listen endpoint for control API (defaults to runtime endpoint)"`
	GatewayListen string `help:"Listen address for the host gateway (default :8170, use :0 for ephemeral port)"`
	LogLevel      string `help:"Server log level (debug|info|warn|error)"`
	TLSCert       string `help:"Path to TLS server certificate (auto-discovered from XDG config for https)" env:"CLEANROOM_TLS_CERT"`
	TLSKey        string `help:"Path to TLS server private key (auto-discovered from XDG config for https)" env:"CLEANROOM_TLS_KEY"`
}

type daemonStatusPayload struct {
	Manager   string `json:"manager"`
	Service   string `json:"service"`
	Installed bool   `json:"installed"`
	Active    bool   `json:"active"`
	Path      string `json:"path"`
	Enabled   *bool  `json:"enabled,omitempty"`
	Domain    string `json:"domain,omitempty"`
	State     string `json:"state,omitempty"`
	Listen    string `json:"listen,omitempty"`
}

type daemonStatusResult struct {
	Report  daemonStatusReport
	Payload daemonStatusPayload
}

type daemonInstallOptions struct {
	Restart bool
	DryRun  bool
}

type daemonScope string

const (
	daemonScopeSystem daemonScope = "system"
	daemonScopeUser   daemonScope = "user"
)

var (
	serveInstallGOOS             = runtime.GOOS
	serveInstallEUID             = os.Geteuid
	serveInstallUID              = os.Getuid
	serveInstallStat             = os.Stat
	serveInstallMkdirAll         = os.MkdirAll
	serveInstallWriteFile        = os.WriteFile
	serveInstallRename           = os.Rename
	serveInstallRemoveFile       = os.Remove
	serveInstallUserHomeDir      = os.UserHomeDir
	serveInstallExecutablePath   = os.Executable
	serveInstallRunCommand       = runServeInstallCommand
	serveInstallRunCommandOutput = runServeInstallCommandOutput
	serveInstallSystemdUnitPath  = "/etc/systemd/system/" + systemdServiceName
	serveInstallLaunchdPath      = "/Library/LaunchDaemons/" + launchdServiceName + ".plist"
	serveInstallSleep            = time.Sleep
	serveInstallWaitAttempts     = 50
	serveInstallWaitPollInterval = 100 * time.Millisecond
)

func (s *DaemonCommand) Run(ctx *runtimeContext) error {
	action := strings.TrimSpace(strings.ToLower(s.Action))
	if s.JSON && action != "status" {
		return errors.New("--json is only supported with daemon status")
	}
	if s.Restart && action != "install" {
		return errors.New("--restart is only supported with daemon install")
	}
	if s.InitConfig && action != "install" {
		return errors.New("--init-config is only supported with daemon install")
	}
	if s.DryRun && action != "install" {
		return errors.New("--dry-run is only supported with daemon install")
	}
	if s.Force && action == "install" {
		return errors.New("--force is no longer supported with daemon install")
	}

	switch action {
	case "install":
		return s.installDaemon(ctx)
	case "uninstall":
		return s.uninstallDaemon(ctx)
	case "status":
		return s.daemonStatus(ctx)
	case "start":
		return s.startDaemon(ctx)
	case "stop":
		return s.stopDaemon(ctx)
	case "restart":
		return s.restartDaemon(ctx)
	default:
		return fmt.Errorf("unsupported daemon action %q", s.Action)
	}
}

func (s *DaemonCommand) requestedDaemonScope() (daemonScope, bool, error) {
	if s.User && s.System {
		return "", false, errors.New("--user and --system cannot be used together")
	}
	if s.User {
		return daemonScopeUser, true, nil
	}
	if s.System {
		return daemonScopeSystem, true, nil
	}
	return "", false, nil
}

func (s *DaemonCommand) effectiveDaemonScope() (daemonScope, error) {
	requested, hasScope, err := s.requestedDaemonScope()
	if err != nil {
		return "", err
	}

	switch serveInstallGOOS {
	case "linux":
		if hasScope && requested == daemonScopeUser {
			return "", errors.New("--user is unsupported on linux (systemd user mode is not yet implemented)")
		}
		return daemonScopeSystem, nil
	case "darwin":
		if hasScope && requested == daemonScopeSystem {
			return "", errors.New("--system is unsupported on darwin (macOS supports user launchd daemons only)")
		}
		if serveInstallEUID() == 0 {
			return "", errors.New("darwin supports user launchd daemons only; run 'cleanroom daemon' without sudo")
		}
		return daemonScopeUser, nil
	default:
		if hasScope && requested == daemonScopeUser {
			return "", fmt.Errorf("--user is unsupported on %s", serveInstallGOOS)
		}
		return daemonScopeSystem, nil
	}
}

func (s *DaemonCommand) daemonRunArgs(cwd string, scope daemonScope) ([]string, error) {
	listen := strings.TrimSpace(s.Listen)
	if listen == "" {
		if scope == daemonScopeUser {
			listen = "unix://" + endpoint.Default().Address
		} else {
			listen = defaultDaemonListen
		}
	}

	args := []string{"serve", "--listen", listen}
	if value := strings.TrimSpace(s.GatewayListen); value != "" {
		args = append(args, "--gateway-listen", value)
	}
	if value := strings.TrimSpace(s.LogLevel); value != "" {
		args = append(args, "--log-level", value)
	}
	if value := strings.TrimSpace(s.TLSCert); value != "" {
		resolved, err := resolveDaemonInstallPath(cwd, value)
		if err != nil {
			return nil, fmt.Errorf("resolve --tls-cert path: %w", err)
		}
		args = append(args, "--tls-cert", resolved)
	}
	if value := strings.TrimSpace(s.TLSKey); value != "" {
		resolved, err := resolveDaemonInstallPath(cwd, value)
		if err != nil {
			return nil, fmt.Errorf("resolve --tls-key path: %w", err)
		}
		args = append(args, "--tls-key", resolved)
	}
	return args, nil
}

func resolveDaemonInstallPath(cwd, value string) (string, error) {
	if filepath.IsAbs(value) {
		return filepath.Clean(value), nil
	}

	base := strings.TrimSpace(cwd)
	if base == "" {
		base = "."
	}
	if !filepath.IsAbs(base) {
		absBase, err := filepath.Abs(base)
		if err != nil {
			return "", fmt.Errorf("resolve absolute working directory: %w", err)
		}
		base = absBase
	}
	return filepath.Clean(filepath.Join(base, value)), nil
}

func (s *DaemonCommand) installDaemon(ctx *runtimeContext) error {
	scope, err := s.effectiveDaemonScope()
	if err != nil {
		return err
	}

	var installFn func(io.Writer, string, []string, daemonInstallOptions) error
	switch serveInstallGOOS {
	case "linux":
		installFn = installSystemdDaemon
	case "darwin":
		if scope == daemonScopeUser {
			installFn = installLaunchdUserDaemon
		} else {
			installFn = installLaunchdDaemon
		}
	default:
		return fmt.Errorf("daemon install is unsupported on %s (expected linux or darwin)", serveInstallGOOS)
	}

	if scope == daemonScopeSystem && serveInstallEUID() != 0 {
		return errors.New("daemon install requires root privileges (use sudo cleanroom daemon install)")
	}

	if s.InitConfig {
		if err := s.ensureRuntimeConfig(ctx); err != nil {
			return err
		}
	}
	if err := s.reloadRuntimeConfig(ctx); err != nil {
		return err
	}

	executablePath, err := serveInstallExecutablePath()
	if err != nil {
		return fmt.Errorf("resolve cleanroom executable path: %w", err)
	}
	if !filepath.IsAbs(executablePath) {
		executablePath, err = filepath.Abs(executablePath)
		if err != nil {
			return fmt.Errorf("resolve absolute executable path: %w", err)
		}
	}

	args, err := s.daemonRunArgs(ctx.CWD, scope)
	if err != nil {
		return err
	}
	return installFn(ctx.Stdout, executablePath, args, daemonInstallOptions{
		Restart: s.Restart,
		DryRun:  s.DryRun,
	})
}

func (s *DaemonCommand) uninstallDaemon(ctx *runtimeContext) error {
	scope, err := s.effectiveDaemonScope()
	if err != nil {
		return err
	}

	var uninstallFn func(io.Writer) error
	switch serveInstallGOOS {
	case "linux":
		uninstallFn = uninstallSystemdDaemon
	case "darwin":
		if scope == daemonScopeUser {
			uninstallFn = uninstallLaunchdUserDaemon
		} else {
			uninstallFn = uninstallLaunchdDaemon
		}
	default:
		return fmt.Errorf("daemon uninstall is unsupported on %s (expected linux or darwin)", serveInstallGOOS)
	}

	if scope == daemonScopeSystem && serveInstallEUID() != 0 {
		return errors.New("daemon uninstall requires root privileges (use sudo cleanroom daemon uninstall)")
	}

	return uninstallFn(ctx.Stdout)
}

func (s *DaemonCommand) startDaemon(ctx *runtimeContext) error {
	scope, err := s.effectiveDaemonScope()
	if err != nil {
		return err
	}

	var startFn func(io.Writer) error
	switch serveInstallGOOS {
	case "linux":
		startFn = startSystemdDaemon
	case "darwin":
		if scope == daemonScopeUser {
			startFn = startLaunchdUserDaemon
		} else {
			startFn = startLaunchdDaemon
		}
	default:
		return fmt.Errorf("daemon start is unsupported on %s (expected linux or darwin)", serveInstallGOOS)
	}

	if scope == daemonScopeSystem && serveInstallEUID() != 0 {
		return errors.New("daemon start requires root privileges (use sudo cleanroom daemon start)")
	}

	return startFn(ctx.Stdout)
}

func (s *DaemonCommand) stopDaemon(ctx *runtimeContext) error {
	scope, err := s.effectiveDaemonScope()
	if err != nil {
		return err
	}

	var stopFn func(io.Writer) error
	switch serveInstallGOOS {
	case "linux":
		stopFn = stopSystemdDaemon
	case "darwin":
		if scope == daemonScopeUser {
			stopFn = stopLaunchdUserDaemon
		} else {
			stopFn = stopLaunchdDaemon
		}
	default:
		return fmt.Errorf("daemon stop is unsupported on %s (expected linux or darwin)", serveInstallGOOS)
	}

	if scope == daemonScopeSystem && serveInstallEUID() != 0 {
		return errors.New("daemon stop requires root privileges (use sudo cleanroom daemon stop)")
	}

	return stopFn(ctx.Stdout)
}

func (s *DaemonCommand) restartDaemon(ctx *runtimeContext) error {
	scope, err := s.effectiveDaemonScope()
	if err != nil {
		return err
	}

	var restartFn func(io.Writer, bool) error
	switch serveInstallGOOS {
	case "linux":
		restartFn = restartSystemdDaemon
	case "darwin":
		if scope == daemonScopeUser {
			restartFn = restartLaunchdUserDaemon
		} else {
			restartFn = restartLaunchdDaemon
		}
	default:
		return fmt.Errorf("daemon restart is unsupported on %s (expected linux or darwin)", serveInstallGOOS)
	}

	if scope == daemonScopeSystem && serveInstallEUID() != 0 {
		return errors.New("daemon restart requires root privileges (use sudo cleanroom daemon restart)")
	}

	return restartFn(ctx.Stdout, s.Force)
}

func (s *DaemonCommand) daemonStatus(ctx *runtimeContext) error {
	scope, err := s.effectiveDaemonScope()
	if err != nil {
		return err
	}

	var statusFn func() (daemonStatusResult, error)
	switch serveInstallGOOS {
	case "linux":
		statusFn = systemdDaemonStatus
	case "darwin":
		if scope == daemonScopeUser {
			statusFn = launchdUserDaemonStatus
		} else {
			statusFn = launchdDaemonStatus
		}
	default:
		return fmt.Errorf("daemon status is unsupported on %s (expected linux or darwin)", serveInstallGOOS)
	}

	result, err := statusFn()
	if err != nil {
		return err
	}
	if s.JSON {
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result.Payload)
	}
	_, err = fmt.Fprint(ctx.Stdout, renderDaemonStatusReport(result.Report, shouldUseANSI(ctx.Stdout)))
	return err
}

func isExitError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

func runServeInstallCommand(name string, args ...string) error {
	_, err := runServeInstallCommandOutput(name, args...)
	return err
}

func runServeInstallCommandOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = "no stderr output"
		}
		parts := append([]string{name}, args...)
		return "", fmt.Errorf("%s: %w (%s)", strings.Join(parts, " "), err, msg)
	}
	return string(out), nil
}

func (s *DaemonCommand) ensureRuntimeConfig(ctx *runtimeContext) error {
	path := strings.TrimSpace(ctx.ConfigPath)
	if path == "" {
		resolved, err := runtimeconfig.Path()
		if err != nil {
			return err
		}
		path = resolved
	}

	if _, err := os.Stat(path); err == nil {
		ctx.ConfigPath = path
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	initCmd := &ConfigInitCommand{Path: path}
	if err := initCmd.Run(ctx); err != nil {
		return err
	}
	ctx.ConfigPath = path
	return nil
}

func (s *DaemonCommand) reloadRuntimeConfig(ctx *runtimeContext) error {
	path := strings.TrimSpace(ctx.ConfigPath)
	var (
		cfg runtimeconfig.Config
		err error
	)
	if path == "" {
		cfg, path, err = runtimeconfig.Load()
	} else {
		cfg, path, err = runtimeconfig.LoadPath(path)
	}
	if err != nil {
		return err
	}

	ctx.Config = cfg
	ctx.ConfigPath = path
	if ctx.Backends == nil {
		ctx.Backends = map[string]backend.Adapter{
			"firecracker": firecracker.New(),
			"darwin-vz":   darwinvz.New(),
		}
	}
	configureBackendRuntimeConfig(ctx.Backends, cfg, ctx.Version)
	return nil
}
