package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/buildkite/cleanroom/internal/endpoint"
)

type DaemonCommand struct {
	Action        string `arg:"" required:"" help:"Daemon action (install, uninstall, status, start, stop)"`
	Force         bool   `help:"Overwrite existing daemon service file (daemon install only)"`
	User          bool   `help:"Use user daemon scope (launchd only; install/uninstall/status/start/stop actions)"`
	System        bool   `help:"Use system daemon scope (linux only; install/uninstall/status/start/stop actions)"`
	Listen        string `help:"Listen endpoint for control API (defaults to runtime endpoint)"`
	GatewayListen string `help:"Listen address for the host gateway (default :8170, use :0 for ephemeral port)"`
	LogLevel      string `help:"Server log level (debug|info|warn|error)"`
	TLSCert       string `help:"Path to TLS server certificate (auto-discovered from XDG config for https)" env:"CLEANROOM_TLS_CERT"`
	TLSKey        string `help:"Path to TLS server private key (auto-discovered from XDG config for https)" env:"CLEANROOM_TLS_KEY"`
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
	serveInstallRemoveFile       = os.Remove
	serveInstallUserHomeDir      = os.UserHomeDir
	serveInstallExecutablePath   = os.Executable
	serveInstallRunCommand       = runServeInstallCommand
	serveInstallRunCommandOutput = runServeInstallCommandOutput
	serveInstallSystemdUnitPath  = "/etc/systemd/system/" + systemdServiceName
	serveInstallLaunchdPath      = "/Library/LaunchDaemons/" + launchdServiceName + ".plist"
)

func (s *DaemonCommand) Run(ctx *runtimeContext) error {
	switch strings.TrimSpace(strings.ToLower(s.Action)) {
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

	var installFn func(io.Writer, string, []string, bool) error
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
	return installFn(ctx.Stdout, executablePath, args, s.Force)
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

func (s *DaemonCommand) daemonStatus(ctx *runtimeContext) error {
	scope, err := s.effectiveDaemonScope()
	if err != nil {
		return err
	}

	var statusFn func(*os.File) error
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

	return statusFn(ctx.Stdout)
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
