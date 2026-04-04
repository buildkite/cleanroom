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

	"github.com/buildkite/cleanroom/internal/endpoint"
)

type DaemonCommand struct {
	Action                 string   `arg:"" required:"" help:"Daemon action (install, uninstall, status, start, stop)"`
	Service                string   `help:"Daemon service kind (control or network-filter)" hidden:""`
	Force                  bool     `help:"Overwrite existing daemon service file (daemon install only)"`
	User                   bool     `help:"Use user daemon scope (launchd only; install/uninstall/status/start/stop actions)"`
	System                 bool     `help:"Use system daemon scope (install/uninstall/status/start/stop actions)"`
	JSON                   bool     `help:"Print daemon status as JSON (status action only)"`
	Listen                 string   `help:"Listen endpoint for control API (defaults to runtime endpoint)"`
	GatewayListen          string   `help:"Listen address for the host gateway (default :8170, use :0 for ephemeral port)"`
	LogLevel               string   `help:"Server log level (debug|info|warn|error)"`
	TLSCert                string   `help:"Path to TLS server certificate (auto-discovered from XDG config for https)" env:"CLEANROOM_TLS_CERT"`
	TLSKey                 string   `help:"Path to TLS server private key (auto-discovered from XDG config for https)" env:"CLEANROOM_TLS_KEY"`
	LaunchdRunAtLoad       string   `name:"launchd-run-at-load" hidden:"" help:"Override launchd RunAtLoad for daemon install (darwin only)"`
	LaunchdKeepAlive       string   `name:"launchd-keep-alive" hidden:"" help:"Override launchd KeepAlive for daemon install (darwin only)"`
	LaunchdWorkingDir      string   `name:"launchd-working-directory" hidden:"" help:"Override launchd WorkingDirectory for daemon install (darwin only)"`
	LaunchdStdoutPath      string   `name:"launchd-stdout-path" hidden:"" help:"Override launchd StandardOutPath for daemon install (darwin only)"`
	LaunchdStderrPath      string   `name:"launchd-stderr-path" hidden:"" help:"Override launchd StandardErrorPath for daemon install (darwin only)"`
	LaunchdEnvironmentVars []string `name:"launchd-env" hidden:"" help:"Additional launchd environment entry KEY=VALUE for daemon install (darwin only)"`
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

type daemonScope string
type daemonService string

const (
	daemonScopeSystem daemonScope = "system"
	daemonScopeUser   daemonScope = "user"

	daemonServiceControl       daemonService = "control"
	daemonServiceNetworkFilter daemonService = "network-filter"
)

var (
	serveInstallGOOS               = runtime.GOOS
	serveInstallEUID               = os.Geteuid
	serveInstallUID                = os.Getuid
	serveInstallStat               = os.Stat
	serveInstallMkdirAll           = os.MkdirAll
	serveInstallWriteFile          = os.WriteFile
	serveInstallRename             = os.Rename
	serveInstallRemoveFile         = os.Remove
	serveInstallUserHomeDir        = os.UserHomeDir
	serveInstallExecutablePath     = os.Executable
	serveInstallRunCommand         = runServeInstallCommand
	serveInstallRunCommandOutput   = runServeInstallCommandOutput
	serveInstallSystemdUnitPath    = "/etc/systemd/system/" + systemdServiceName
	serveInstallLaunchdPath        = "/Library/LaunchDaemons/" + launchdSystemServiceName + ".plist"
	serveInstallLaunchdNetworkPath = "/Library/LaunchDaemons/" + launchdNetworkServiceName + ".plist"
)

func (s *DaemonCommand) requestedDaemonService() (daemonService, error) {
	switch strings.TrimSpace(strings.ToLower(s.Service)) {
	case "", string(daemonServiceControl):
		return daemonServiceControl, nil
	case string(daemonServiceNetworkFilter):
		return daemonServiceNetworkFilter, nil
	default:
		return "", fmt.Errorf("unsupported daemon service %q", s.Service)
	}
}

func (s *DaemonCommand) Run(ctx *runtimeContext) error {
	action := strings.TrimSpace(strings.ToLower(s.Action))
	if s.JSON && action != "status" {
		return errors.New("--json is only supported with daemon status")
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

func (s *DaemonCommand) effectiveDaemonScope(service daemonService) (daemonScope, error) {
	requested, hasScope, err := s.requestedDaemonScope()
	if err != nil {
		return "", err
	}

	if service == daemonServiceNetworkFilter {
		switch serveInstallGOOS {
		case "darwin":
			if hasScope && requested != daemonScopeSystem {
				return "", errors.New("network-filter daemon only supports --system on darwin")
			}
			return daemonScopeSystem, nil
		default:
			return "", fmt.Errorf("network-filter daemon is unsupported on %s", serveInstallGOOS)
		}
	}

	switch serveInstallGOOS {
	case "linux":
		if hasScope && requested == daemonScopeUser {
			return "", errors.New("--user is unsupported on linux (systemd user mode is not yet implemented)")
		}
		return daemonScopeSystem, nil
	case "darwin":
		if !hasScope {
			if serveInstallEUID() == 0 {
				return "", errors.New("darwin requires an explicit daemon scope when running as root; use 'sudo cleanroom daemon <action> --system' or rerun without sudo for the user daemon")
			}
			return daemonScopeUser, nil
		}
		if requested == daemonScopeUser && serveInstallEUID() == 0 {
			return "", errors.New("darwin user daemons must be installed without sudo; rerun 'cleanroom daemon <action> --user' as your user")
		}
		return requested, nil
	default:
		if hasScope && requested == daemonScopeUser {
			return "", fmt.Errorf("--user is unsupported on %s", serveInstallGOOS)
		}
		return daemonScopeSystem, nil
	}
}

func (s *DaemonCommand) daemonRunArgs(cwd string, scope daemonScope, service daemonService) ([]string, error) {
	if service == daemonServiceNetworkFilter {
		return []string{"serve-network-filter"}, nil
	}

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

func launchdBoolOverride(raw string, fallback bool) bool {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "true":
		return true
	case "false":
		return false
	default:
		return fallback
	}
}

func launchdEnvironmentMap(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid launchd environment entry %q: expected KEY=VALUE", entry)
		}
		values[key] = value
	}
	return values, nil
}

func (s *DaemonCommand) launchdConfig(cwd string, scope daemonScope, service daemonService, executablePath string, args []string) (launchdServiceConfig, error) {
	spec, err := launchdServiceSpecForScope(scope, service)
	if err != nil {
		return launchdServiceConfig{}, err
	}

	env, err := launchdEnvironmentMap(s.LaunchdEnvironmentVars)
	if err != nil {
		return launchdServiceConfig{}, err
	}

	cfg := launchdServiceConfig{
		Label:          spec.Name,
		ExecutablePath: executablePath,
		Arguments:      append([]string(nil), args...),
		RunAtLoad:      launchdBoolOverride(s.LaunchdRunAtLoad, true),
		KeepAlive:      launchdBoolOverride(s.LaunchdKeepAlive, true),
		Environment:    env,
	}

	if value := strings.TrimSpace(s.LaunchdWorkingDir); value != "" {
		resolved, err := resolveDaemonInstallPath(cwd, value)
		if err != nil {
			return launchdServiceConfig{}, fmt.Errorf("resolve --launchd-working-directory path: %w", err)
		}
		cfg.WorkingDirectory = resolved
	}
	if value := strings.TrimSpace(s.LaunchdStdoutPath); value != "" {
		resolved, err := resolveDaemonInstallPath(cwd, value)
		if err != nil {
			return launchdServiceConfig{}, fmt.Errorf("resolve --launchd-stdout-path path: %w", err)
		}
		cfg.StdoutPath = resolved
	}
	if value := strings.TrimSpace(s.LaunchdStderrPath); value != "" {
		resolved, err := resolveDaemonInstallPath(cwd, value)
		if err != nil {
			return launchdServiceConfig{}, fmt.Errorf("resolve --launchd-stderr-path path: %w", err)
		}
		cfg.StderrPath = resolved
	}

	return cfg, nil
}

func (s *DaemonCommand) installDaemon(ctx *runtimeContext) error {
	service, err := s.requestedDaemonService()
	if err != nil {
		return err
	}
	scope, err := s.effectiveDaemonScope(service)
	if err != nil {
		return err
	}

	switch serveInstallGOOS {
	case "linux", "darwin":
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

	args, err := s.daemonRunArgs(ctx.CWD, scope, service)
	if err != nil {
		return err
	}

	switch serveInstallGOOS {
	case "linux":
		return installSystemdDaemon(ctx.Stdout, executablePath, args, s.Force)
	case "darwin":
		spec, err := launchdServiceSpecForScope(scope, service)
		if err != nil {
			return err
		}
		cfg, err := s.launchdConfig(ctx.CWD, scope, service, executablePath, args)
		if err != nil {
			return err
		}
		return installLaunchdService(ctx.Stdout, spec, cfg, s.Force)
	}

	return fmt.Errorf("daemon install is unsupported on %s (expected linux or darwin)", serveInstallGOOS)
}

func (s *DaemonCommand) uninstallDaemon(ctx *runtimeContext) error {
	service, err := s.requestedDaemonService()
	if err != nil {
		return err
	}
	scope, err := s.effectiveDaemonScope(service)
	if err != nil {
		return err
	}

	var uninstallFn func(io.Writer) error
	switch serveInstallGOOS {
	case "linux":
		uninstallFn = uninstallSystemdDaemon
	case "darwin":
		spec, err := launchdServiceSpecForScope(scope, service)
		if err != nil {
			return err
		}
		uninstallFn = func(stdout io.Writer) error { return uninstallLaunchdService(stdout, spec) }
	default:
		return fmt.Errorf("daemon uninstall is unsupported on %s (expected linux or darwin)", serveInstallGOOS)
	}

	if scope == daemonScopeSystem && serveInstallEUID() != 0 {
		return errors.New("daemon uninstall requires root privileges (use sudo cleanroom daemon uninstall)")
	}

	return uninstallFn(ctx.Stdout)
}

func (s *DaemonCommand) startDaemon(ctx *runtimeContext) error {
	service, err := s.requestedDaemonService()
	if err != nil {
		return err
	}
	scope, err := s.effectiveDaemonScope(service)
	if err != nil {
		return err
	}

	var startFn func(io.Writer) error
	switch serveInstallGOOS {
	case "linux":
		startFn = startSystemdDaemon
	case "darwin":
		spec, err := launchdServiceSpecForScope(scope, service)
		if err != nil {
			return err
		}
		startFn = func(stdout io.Writer) error { return startLaunchdService(stdout, spec) }
	default:
		return fmt.Errorf("daemon start is unsupported on %s (expected linux or darwin)", serveInstallGOOS)
	}

	if scope == daemonScopeSystem && serveInstallEUID() != 0 {
		return errors.New("daemon start requires root privileges (use sudo cleanroom daemon start)")
	}

	return startFn(ctx.Stdout)
}

func (s *DaemonCommand) stopDaemon(ctx *runtimeContext) error {
	service, err := s.requestedDaemonService()
	if err != nil {
		return err
	}
	scope, err := s.effectiveDaemonScope(service)
	if err != nil {
		return err
	}

	var stopFn func(io.Writer) error
	switch serveInstallGOOS {
	case "linux":
		stopFn = stopSystemdDaemon
	case "darwin":
		spec, err := launchdServiceSpecForScope(scope, service)
		if err != nil {
			return err
		}
		stopFn = func(stdout io.Writer) error { return stopLaunchdService(stdout, spec) }
	default:
		return fmt.Errorf("daemon stop is unsupported on %s (expected linux or darwin)", serveInstallGOOS)
	}

	if scope == daemonScopeSystem && serveInstallEUID() != 0 {
		return errors.New("daemon stop requires root privileges (use sudo cleanroom daemon stop)")
	}

	return stopFn(ctx.Stdout)
}

func (s *DaemonCommand) daemonStatus(ctx *runtimeContext) error {
	service, err := s.requestedDaemonService()
	if err != nil {
		return err
	}
	scope, err := s.effectiveDaemonScope(service)
	if err != nil {
		return err
	}

	var statusFn func() (daemonStatusResult, error)
	switch serveInstallGOOS {
	case "linux":
		statusFn = systemdDaemonStatus
	case "darwin":
		spec, err := launchdServiceSpecForScope(scope, service)
		if err != nil {
			return err
		}
		statusFn = func() (daemonStatusResult, error) { return launchdDaemonStatusResult(spec) }
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
