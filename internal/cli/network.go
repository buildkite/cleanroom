package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/networkfilterstate"
)

type NetworkCommand struct {
	Install   NetworkInstallCommand   `cmd:"" help:"Install the macOS network-filter daemon"`
	Enable    NetworkEnableCommand    `cmd:"" help:"Enable the macOS network filter"`
	Disable   NetworkDisableCommand   `cmd:"" help:"Disable the macOS network filter"`
	Reset     NetworkResetCommand     `cmd:"" help:"Reset macOS network-filter preferences and daemon state"`
	Status    NetworkStatusCommand    `cmd:"" help:"Show macOS network-filter daemon and filter status"`
	Uninstall NetworkUninstallCommand `cmd:"" help:"Uninstall the macOS network-filter daemon"`
}

type NetworkInstallCommand struct{}
type NetworkEnableCommand struct{}
type NetworkDisableCommand struct{}
type NetworkResetCommand struct{}
type NetworkStatusCommand struct{}
type NetworkUninstallCommand struct{}

type networkSupportAppAction string

const (
	networkSupportAppActionEnable  networkSupportAppAction = "enable"
	networkSupportAppActionDisable networkSupportAppAction = "disable"
	networkSupportAppActionReset   networkSupportAppAction = "reset"
	networkSupportAppActionStatus  networkSupportAppAction = "status"

	networkSupportAppBundleName       = "Cleanroom.app"
	networkSupportAppExecutableName   = "Cleanroom"
	networkSupportAppBundleEnvVar     = "CLEANROOM_MACOS_APP"
	networkSupportAppExecutableEnvVar = "CLEANROOM_MACOS_APP_EXECUTABLE"
)

var errNetworkSupportAppNotFound = errors.New("network support app not found")

type networkSupportAppStatusSnapshot struct {
	AppBundlePath          string `json:"app_bundle_path"`
	ExtensionInstalled     bool   `json:"extension_installed"`
	DaemonHealthy          bool   `json:"daemon_healthy"`
	Available              bool   `json:"available"`
	Loaded                 bool   `json:"loaded"`
	Configured             bool   `json:"configured"`
	Enabled                bool   `json:"enabled"`
	NetworkFilterLastError string `json:"last_error,omitempty"`
	ProviderValidation     string `json:"provider_validation,omitempty"`
}

var runNetworkDaemonCommand = func(cmd DaemonCommand, ctx *runtimeContext) error {
	return cmd.Run(ctx)
}

type networkFilterStatusClient interface {
	Health(ctx context.Context) error
	GetStatus(ctx context.Context) (networkfilterstate.StatusSnapshot, bool, error)
	GetPolicy(ctx context.Context) (networkfilterstate.PolicySnapshot, bool, error)
}

var newNetworkFilterStatusClient = func() networkFilterStatusClient {
	return networkfilterstate.NewClient(resolveNetworkFilterDaemonURL())
}

var runNetworkSupportAppAction = func(ctx *runtimeContext, action networkSupportAppAction) error {
	return executeNetworkSupportAppAction(ctx, action)
}

var queryNetworkSupportAppStatus = func(ctx *runtimeContext) (networkSupportAppStatusSnapshot, error) {
	return queryNetworkSupportAppStatusViaApp(ctx)
}

func (c *NetworkInstallCommand) Run(ctx *runtimeContext) error {
	return runNetworkDaemonCommand(DaemonCommand{
		Action:  "install",
		Service: string(daemonServiceNetworkFilter),
		System:  true,
		Force:   true,
	}, ctx)
}

func (c *NetworkEnableCommand) Run(ctx *runtimeContext) error {
	return runNetworkSupportAppAction(ctx, networkSupportAppActionEnable)
}

func (c *NetworkDisableCommand) Run(ctx *runtimeContext) error {
	return runNetworkSupportAppAction(ctx, networkSupportAppActionDisable)
}

func (c *NetworkResetCommand) Run(ctx *runtimeContext) error {
	return runNetworkSupportAppAction(ctx, networkSupportAppActionReset)
}

func (c *NetworkStatusCommand) Run(ctx *runtimeContext) error {
	if err := runNetworkDaemonCommand(DaemonCommand{
		Action:  "status",
		Service: string(daemonServiceNetworkFilter),
		System:  true,
	}, ctx); err != nil {
		return err
	}
	if err := c.renderTelemetry(ctx); err != nil {
		return err
	}
	return c.renderSupportAppStatus(ctx)
}

func (c *NetworkUninstallCommand) Run(ctx *runtimeContext) error {
	return runNetworkDaemonCommand(DaemonCommand{
		Action:  "uninstall",
		Service: string(daemonServiceNetworkFilter),
		System:  true,
	}, ctx)
}

func (c *NetworkStatusCommand) renderTelemetry(ctx *runtimeContext) error {
	stdout := os.Stdout
	if ctx != nil && ctx.Stdout != nil {
		stdout = ctx.Stdout
	}

	client := newNetworkFilterStatusClient()
	checks := []statusCheckLine{}

	healthCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Health(healthCtx); err != nil {
		checks = append(checks, statusCheckLine{
			Name:    "api",
			Status:  "fail",
			Message: "unreachable: " + err.Error(),
		})
		_, writeErr := fmt.Fprint(stdout, renderStatusCheckReport("network filter telemetry", checks, shouldUseANSI(stdout)))
		return writeErr
	}
	checks = append(checks, statusCheckLine{
		Name:    "api",
		Status:  "pass",
		Message: "healthy at " + resolveNetworkFilterDaemonURL(),
	})

	statusCtx, cancelStatus := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelStatus()
	status, found, err := client.GetStatus(statusCtx)
	if err != nil {
		checks = append(checks, statusCheckLine{
			Name:    "filter",
			Status:  "fail",
			Message: "status query failed: " + err.Error(),
		})
		_, writeErr := fmt.Fprint(stdout, renderStatusCheckReport("network filter telemetry", checks, shouldUseANSI(stdout)))
		return writeErr
	}
	if !found {
		checks = append(checks, statusCheckLine{
			Name:    "filter",
			Status:  "warn",
			Message: "daemon returned no status snapshot",
		})
		_, writeErr := fmt.Fprint(stdout, renderStatusCheckReport("network filter telemetry", checks, shouldUseANSI(stdout)))
		return writeErr
	}
	checks = append(checks, summarizeNetworkFilterStatus(status)...)

	policyCtx, cancelPolicy := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelPolicy()
	policy, found, err := client.GetPolicy(policyCtx)
	if err != nil {
		checks = append(checks, statusCheckLine{
			Name:    "policy",
			Status:  "fail",
			Message: "policy query failed: " + err.Error(),
		})
		_, writeErr := fmt.Fprint(stdout, renderStatusCheckReport("network filter telemetry", checks, shouldUseANSI(stdout)))
		return writeErr
	}
	if !found {
		checks = append(checks, statusCheckLine{
			Name:    "policy",
			Status:  "warn",
			Message: "no active policy snapshot",
		})
		_, writeErr := fmt.Fprint(stdout, renderStatusCheckReport("network filter telemetry", checks, shouldUseANSI(stdout)))
		return writeErr
	}
	checks = append(checks, summarizeNetworkFilterPolicy(policy))
	_, err = fmt.Fprint(stdout, renderStatusCheckReport("network filter telemetry", checks, shouldUseANSI(stdout)))
	return err
}

func (c *NetworkStatusCommand) renderSupportAppStatus(ctx *runtimeContext) error {
	stdout := os.Stdout
	if ctx != nil && ctx.Stdout != nil {
		stdout = ctx.Stdout
	}

	checks := []statusCheckLine{}
	status, err := queryNetworkSupportAppStatus(ctx)
	switch {
	case err == nil:
		checks = append(checks, summarizeNetworkSupportAppStatus(status)...)
	case errors.Is(err, errNetworkSupportAppNotFound):
		checks = append(checks, statusCheckLine{
			Name:    "app",
			Status:  "warn",
			Message: "support app not found; install Cleanroom.app to use enable/disable/reset",
		})
	default:
		checks = append(checks, statusCheckLine{
			Name:    "app",
			Status:  "fail",
			Message: err.Error(),
		})
	}

	_, writeErr := fmt.Fprint(stdout, renderStatusCheckReport("network filter app", checks, shouldUseANSI(stdout)))
	return writeErr
}

func summarizeNetworkFilterStatus(status networkfilterstate.StatusSnapshot) []statusCheckLine {
	filterStatus := "warn"
	filterMessage := ""
	switch {
	case strings.TrimSpace(status.LastError) != "":
		filterStatus = "fail"
		filterMessage = status.LastError
	case !status.Available:
		filterStatus = "fail"
		filterMessage = "extension unavailable"
	case !status.Loaded:
		filterStatus = "warn"
		filterMessage = "status not loaded"
	case !status.Enabled:
		filterStatus = "warn"
		if status.Configured {
			filterMessage = "disabled"
		} else {
			filterMessage = "not configured"
		}
	default:
		filterStatus = "pass"
		filterMessage = "enabled"
	}
	details := []string{
		"available=" + strconv.FormatBool(status.Available),
		"loaded=" + strconv.FormatBool(status.Loaded),
		"enabled=" + strconv.FormatBool(status.Enabled),
		"configured=" + strconv.FormatBool(status.Configured),
	}
	if updatedAt := strings.TrimSpace(status.UpdatedAt); updatedAt != "" {
		details = append(details, "updated="+updatedAt)
	}

	providerStatus := "warn"
	providerMessage := "inactive"
	switch {
	case strings.TrimSpace(status.ProviderLastError) != "":
		providerStatus = "fail"
		providerMessage = status.ProviderLastError
	case strings.TrimSpace(status.ProviderStartedAt) != "":
		providerStatus = "pass"
		providerMessage = "started " + strings.TrimSpace(status.ProviderStartedAt)
		if updatedAt := strings.TrimSpace(status.ProviderUpdatedAt); updatedAt != "" {
			providerMessage += "; updated " + updatedAt
		}
	case status.Enabled:
		providerStatus = "fail"
		providerMessage = "has not started"
	default:
		providerStatus = "warn"
		providerMessage = "inactive"
	}

	return []statusCheckLine{
		{
			Name:    "filter",
			Status:  filterStatus,
			Message: filterMessage + " (" + strings.Join(details, ", ") + ")",
		},
		{
			Name:    "provider",
			Status:  providerStatus,
			Message: providerMessage,
		},
	}
}

func summarizeNetworkFilterPolicy(policy networkfilterstate.PolicySnapshot) statusCheckLine {
	parts := []string{}
	if action := strings.TrimSpace(policy.DefaultAction); action != "" {
		parts = append(parts, "default "+action)
	}
	parts = append(parts,
		fmt.Sprintf("%d allow rule(s)", len(policy.Allow)),
		fmt.Sprintf("%d process rule(s)", len(policy.ProcessRules)),
	)
	if updatedAt := strings.TrimSpace(policy.UpdatedAt); updatedAt != "" {
		parts = append(parts, "updated "+updatedAt)
	}
	if targetProcessPath := strings.TrimSpace(policy.TargetProcessPath); targetProcessPath != "" {
		parts = append(parts, "target "+targetProcessPath)
	}
	return statusCheckLine{
		Name:    "policy",
		Status:  "pass",
		Message: strings.Join(parts, "; "),
	}
}

func summarizeNetworkSupportAppStatus(status networkSupportAppStatusSnapshot) []statusCheckLine {
	appMessage := "installed"
	if path := strings.TrimSpace(status.AppBundlePath); path != "" {
		appMessage = "installed at " + path
	}

	preferencesStatus := "warn"
	preferencesMessage := ""
	switch {
	case strings.TrimSpace(status.NetworkFilterLastError) != "":
		preferencesStatus = "fail"
		preferencesMessage = status.NetworkFilterLastError
	case !status.ExtensionInstalled:
		preferencesStatus = "fail"
		preferencesMessage = "system extension is not bundled in Cleanroom.app"
	case !status.Available:
		preferencesStatus = "fail"
		preferencesMessage = "network filter unavailable"
	case !status.Loaded:
		preferencesStatus = "warn"
		preferencesMessage = "preferences not loaded"
	case !status.Enabled:
		preferencesStatus = "warn"
		if status.Configured {
			preferencesMessage = "disabled"
		} else {
			preferencesMessage = "not configured"
		}
	default:
		preferencesStatus = "pass"
		preferencesMessage = "enabled"
	}
	details := []string{
		"daemon_healthy=" + strconv.FormatBool(status.DaemonHealthy),
		"loaded=" + strconv.FormatBool(status.Loaded),
		"configured=" + strconv.FormatBool(status.Configured),
		"enabled=" + strconv.FormatBool(status.Enabled),
	}
	if validation := strings.TrimSpace(status.ProviderValidation); validation != "" {
		details = append(details, "provider="+validation)
	}

	return []statusCheckLine{
		{
			Name:    "app",
			Status:  "pass",
			Message: appMessage,
		},
		{
			Name:    "preferences",
			Status:  preferencesStatus,
			Message: preferencesMessage + " (" + strings.Join(details, ", ") + ")",
		},
	}
}

func resolveNetworkFilterDaemonURL() string {
	if configured := strings.TrimSpace(os.Getenv("CLEANROOM_NETWORK_FILTER_DAEMON_URL")); configured != "" {
		return configured
	}
	return networkfilterstate.DefaultBaseURL
}

func executeNetworkSupportAppAction(ctx *runtimeContext, action networkSupportAppAction) error {
	executablePath, err := resolveNetworkSupportAppExecutablePath()
	if err != nil {
		return err
	}

	stdout := os.Stdout
	if ctx != nil && ctx.Stdout != nil {
		stdout = ctx.Stdout
	}

	cmd := exec.Command(executablePath, "--network-command", string(action))
	cmd.Stdout = stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("run Cleanroom.app %s: %s", action, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return fmt.Errorf("run Cleanroom.app %s: %w", action, err)
	}
	return nil
}

func queryNetworkSupportAppStatusViaApp(_ *runtimeContext) (networkSupportAppStatusSnapshot, error) {
	executablePath, err := resolveNetworkSupportAppExecutablePath()
	if err != nil {
		return networkSupportAppStatusSnapshot{}, err
	}

	cmd := exec.Command(executablePath, "--network-command", string(networkSupportAppActionStatus), "--json")
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return networkSupportAppStatusSnapshot{}, fmt.Errorf(
				"query Cleanroom.app status: %s",
				strings.TrimSpace(string(exitErr.Stderr)),
			)
		}
		return networkSupportAppStatusSnapshot{}, fmt.Errorf("query Cleanroom.app status: %w", err)
	}

	var snapshot networkSupportAppStatusSnapshot
	if err := json.Unmarshal(output, &snapshot); err != nil {
		return networkSupportAppStatusSnapshot{}, fmt.Errorf("decode Cleanroom.app status: %w", err)
	}
	return snapshot, nil
}

func resolveNetworkSupportAppExecutablePath() (string, error) {
	return resolveNetworkSupportAppExecutablePathWith(
		os.Getenv(networkSupportAppExecutableEnvVar),
		os.Getenv(networkSupportAppBundleEnvVar),
		os.Executable,
		os.Getwd,
		os.UserHomeDir,
		os.Stat,
	)
}

func resolveNetworkSupportAppExecutablePathWith(
	executableOverride string,
	bundleOverride string,
	executable func() (string, error),
	getwd func() (string, error),
	userHomeDir func() (string, error),
	stat func(string) (os.FileInfo, error),
) (string, error) {
	if override := strings.TrimSpace(executableOverride); override != "" {
		return resolveNetworkSupportAppExecutableCandidate(override, stat)
	}
	if override := strings.TrimSpace(bundleOverride); override != "" {
		return resolveNetworkSupportAppBundleExecutablePath(override, stat)
	}

	if self, err := executable(); err == nil {
		for _, bundlePath := range networkSupportAppBundleCandidatesForExecutable(self) {
			if path, err := resolveNetworkSupportAppBundleExecutablePath(bundlePath, stat); err == nil {
				return path, nil
			}
		}
	}

	if getwd != nil {
		if cwd, err := getwd(); err == nil {
			if path, err := resolveNetworkSupportAppFromWorkdir(cwd, stat); err == nil {
				return path, nil
			}
		}
	}

	globalCandidates := []string{"/Applications/" + networkSupportAppBundleName}
	if userHomeDir != nil {
		if home, err := userHomeDir(); err == nil && strings.TrimSpace(home) != "" {
			globalCandidates = append(globalCandidates, filepath.Join(home, "Applications", networkSupportAppBundleName))
		}
	}
	for _, candidate := range globalCandidates {
		if path, err := resolveNetworkSupportAppBundleExecutablePath(candidate, stat); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf(
		"%w: set %s or %s, install Cleanroom.app to /Applications, or build it locally with `mise run build:macos-app`",
		errNetworkSupportAppNotFound,
		networkSupportAppExecutableEnvVar,
		networkSupportAppBundleEnvVar,
	)
}

func networkSupportAppBundleCandidatesForExecutable(self string) []string {
	trimmed := strings.TrimSpace(self)
	if trimmed == "" {
		return nil
	}

	seen := map[string]struct{}{}
	var candidates []string
	appendCandidate := func(path string) {
		trimmedPath := strings.TrimSpace(path)
		if trimmedPath == "" {
			return
		}
		absPath, err := filepath.Abs(trimmedPath)
		if err != nil {
			return
		}
		if _, ok := seen[absPath]; ok {
			return
		}
		seen[absPath] = struct{}{}
		candidates = append(candidates, absPath)
	}

	appendCandidate(networkSupportAppBundleForExecutable(trimmed))
	appendCandidate(filepath.Join(filepath.Dir(trimmed), networkSupportAppBundleName))

	if resolved, err := filepath.EvalSymlinks(trimmed); err == nil {
		appendCandidate(networkSupportAppBundleForExecutable(resolved))
		appendCandidate(filepath.Join(filepath.Dir(resolved), networkSupportAppBundleName))
	}

	return candidates
}

func networkSupportAppBundleForExecutable(executablePath string) string {
	trimmed := strings.TrimSpace(executablePath)
	if trimmed == "" {
		return ""
	}

	absPath, err := filepath.Abs(trimmed)
	if err != nil {
		return ""
	}

	for dir := filepath.Dir(absPath); ; dir = filepath.Dir(dir) {
		if strings.EqualFold(filepath.Ext(dir), ".app") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return ""
}

func resolveNetworkSupportAppFromWorkdir(startDir string, stat func(string) (os.FileInfo, error)) (string, error) {
	trimmedDir := strings.TrimSpace(startDir)
	if trimmedDir == "" {
		return "", errors.New("working directory is empty")
	}
	absStartDir, err := filepath.Abs(trimmedDir)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}

	for dir := absStartDir; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "dist", networkSupportAppBundleName)
		if path, err := resolveNetworkSupportAppBundleExecutablePath(candidate, stat); err == nil {
			return path, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "", errNetworkSupportAppNotFound
}

func resolveNetworkSupportAppExecutableCandidate(path string, stat func(string) (os.FileInfo, error)) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", errNetworkSupportAppNotFound
	}
	absPath, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve app executable path: %w", err)
	}
	info, err := stat(absPath)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", absPath)
	}
	return absPath, nil
}

func resolveNetworkSupportAppBundleExecutablePath(bundlePath string, stat func(string) (os.FileInfo, error)) (string, error) {
	trimmed := strings.TrimSpace(bundlePath)
	if trimmed == "" {
		return "", errNetworkSupportAppNotFound
	}
	absPath, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve app bundle path: %w", err)
	}
	info, err := stat(absPath)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", absPath)
	}
	executablePath := filepath.Join(absPath, "Contents", "MacOS", networkSupportAppExecutableName)
	executableInfo, err := stat(executablePath)
	if err != nil {
		return "", err
	}
	if executableInfo.IsDir() {
		return "", fmt.Errorf("%s is a directory", executablePath)
	}
	return executablePath, nil
}
