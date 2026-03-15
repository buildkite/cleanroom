package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type DoctorCommand struct {
	Chdir   string `short:"c" help:"Change to this directory before running commands"`
	Backend string `help:"Execution backend to diagnose (defaults to runtime config or host default)"`
	JSON    bool   `help:"Print doctor report as JSON"`
}

type snapshotDoctorConfig struct {
	Enabled   bool   `json:"enabled"`
	Driver    string `json:"driver"`
	BaseDir   string `json:"base_dir"`
	Defaulted bool   `json:"defaulted"`
}

func (d *DoctorCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, d.Chdir)
	if err != nil {
		return err
	}
	backendName := resolveBackendName(d.Backend, ctx.Config.DefaultBackend)
	adapter, ok := ctx.Backends[backendName]
	if !ok {
		return fmt.Errorf("unknown backend %q", backendName)
	}
	adapterCapabilities := backend.CapabilitiesForAdapter(adapter)
	capabilities := applyRuntimeCapabilityOverrides(adapterCapabilities, backendName, ctx.Config)
	gwCredentials := gateway.NewEnvCredentialProvider()
	gwHosts := gwCredentials.ConfiguredHosts()
	gwRoutes := gateway.Routes()
	credSummary := "none configured"
	if len(gwHosts) > 0 {
		credSummary = strings.Join(gwHosts, ", ")
	}
	routeSummary := strings.Join(gwRoutes, ", ")
	snapshotCfg, snapshotCheck, hasSnapshotCfg := snapshotDoctorConfigForBackend(backendName, ctx.Config)

	checks := []backend.DoctorCheck{
		{Name: "runtime_config", Status: "pass", Message: fmt.Sprintf("using runtime config path %s", ctx.ConfigPath)},
		{Name: "backend", Status: "pass", Message: fmt.Sprintf("selected backend %s", backendName)},
	}
	if hasSnapshotCfg {
		checks = append(checks, snapshotCheck)
	}
	checks = append(checks,
		backend.DoctorCheck{
			Name:    "gateway_listen",
			Status:  "pass",
			Message: fmt.Sprintf("default listen %s (port %d; override with cleanroom serve --gateway-listen)", gateway.DefaultListenAddr, gateway.DefaultPort),
		},
		backend.DoctorCheck{
			Name:    "gateway_routes",
			Status:  "pass",
			Message: fmt.Sprintf("enabled routes: %s", routeSummary),
		},
		backend.DoctorCheck{
			Name:    "gateway_credentials",
			Status:  "pass",
			Message: fmt.Sprintf("configured credential hosts: %s", credSummary),
		},
	)
	for _, key := range backend.SortedCapabilityKeys(capabilities) {
		status := "warn"
		message := fmt.Sprintf("%s: unsupported", key)
		if capabilities[key] {
			status = "pass"
			message = fmt.Sprintf("%s: supported", key)
		} else if disabledMessage, ok := disabledSnapshotCapabilityMessage(key, backendName, ctx.Config, ctx.ConfigPath, adapterCapabilities); ok {
			message = disabledMessage
		}
		checks = append(checks, backend.DoctorCheck{
			Name:    capabilityCheckName(key),
			Status:  status,
			Message: message,
		})
	}

	compiled, source, err := ctx.Loader.LoadAndCompile(cwd)
	if err != nil {
		checks = append(checks, backend.DoctorCheck{
			Name:    "repository_policy",
			Status:  "warn",
			Message: fmt.Sprintf("policy not loaded from %s: %v", cwd, err),
		})
	} else {
		checks = append(checks, backend.DoctorCheck{
			Name:    "repository_policy",
			Status:  "pass",
			Message: fmt.Sprintf("policy loaded from %s (hash %s)", source, compiled.Hash),
		})
	}

	type doctorCapable interface {
		Doctor(context.Context, backend.DoctorRequest) (*backend.DoctorReport, error)
	}
	if checker, ok := adapter.(doctorCapable); ok {
		report, err := checker.Doctor(context.Background(), backend.DoctorRequest{
			Policy:            compiled,
			FirecrackerConfig: runtimeconfig.MergeBackendConfig(ctx.Config, backendName, 0),
		})
		if err != nil {
			return err
		}
		checks = append(checks, report.Checks...)
	} else {
		checks = append(checks, backend.DoctorCheck{
			Name:    "backend_doctor",
			Status:  "warn",
			Message: "selected backend does not expose doctor diagnostics",
		})
	}

	if d.JSON {
		payload := map[string]any{
			"backend":      backendName,
			"capabilities": backend.CloneCapabilities(capabilities),
			"checks":       checks,
			"gateway": map[string]any{
				"default_listen":   gateway.DefaultListenAddr,
				"default_port":     gateway.DefaultPort,
				"routes":           gwRoutes,
				"credential_hosts": gwHosts,
			},
		}
		if hasSnapshotCfg {
			payload["snapshot"] = snapshotCfg
		}
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	_, err = fmt.Fprint(ctx.Stdout, renderDoctorReport(backendName, checks, shouldUseANSI(ctx.Stdout)))
	return err
}

func resolveBackendName(requested, configuredDefault string) string {
	if requested != "" {
		return requested
	}
	if configuredDefault != "" {
		return configuredDefault
	}
	return runtimeconfig.DefaultBackendForHost()
}

func snapshotDoctorConfigForBackend(backendName string, cfg runtimeconfig.Config) (snapshotDoctorConfig, backend.DoctorCheck, bool) {
	snapshotCfg, ok := runtimeconfig.SnapshotConfigForBackend(cfg, backendName)
	if !ok {
		return snapshotDoctorConfig{}, backend.DoctorCheck{}, false
	}

	driver := runtimeconfig.SnapshotDriverOrDefault(backendName, snapshotCfg.Driver)
	defaulted := strings.TrimSpace(snapshotCfg.Driver) == ""
	baseDir := strings.TrimSpace(snapshotCfg.BaseDir)
	if baseDir != "" {
		baseDir = filepath.Clean(baseDir)
	} else if resolved, err := paths.SnapshotDir(); err == nil {
		baseDir = resolved
	} else {
		baseDir = fmt.Sprintf("<unresolved: %v>", err)
	}

	info := snapshotDoctorConfig{
		Enabled:   snapshotCfg.Enabled,
		Driver:    driver,
		BaseDir:   baseDir,
		Defaulted: defaulted,
	}
	return info, backend.DoctorCheck{
		Name:    "snapshot_config",
		Status:  "pass",
		Message: formatSnapshotDoctorCheck(info),
	}, true
}

func formatSnapshotDoctorCheck(cfg snapshotDoctorConfig) string {
	driver := cfg.Driver
	if cfg.Defaulted {
		driver += " (defaulted)"
	}
	return fmt.Sprintf("enabled=%t driver=%s base_dir=%s", cfg.Enabled, driver, cfg.BaseDir)
}

var capabilityNameReplacer = strings.NewReplacer(".", "_", "-", "_")

func capabilityCheckName(key string) string {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return "capability_unknown"
	}
	return "capability_" + capabilityNameReplacer.Replace(trimmed)
}

func applyRuntimeCapabilityOverrides(caps map[string]bool, backendName string, cfg runtimeconfig.Config) map[string]bool {
	out := backend.CloneCapabilities(caps)
	snapshotCfg, ok := runtimeconfig.SnapshotConfigForBackend(cfg, backendName)
	if ok && snapshotCfg.Enabled {
		return out
	}
	out[backend.CapabilitySandboxSnapshot] = false
	return out
}

func disabledSnapshotCapabilityMessage(capabilityKey, backendName string, cfg runtimeconfig.Config, configPath string, adapterCaps map[string]bool) (string, bool) {
	if !isSnapshotCapabilityKey(capabilityKey) {
		return "", false
	}
	if !adapterCaps[capabilityKey] {
		return "", false
	}
	snapshotCfg, ok := runtimeconfig.SnapshotConfigForBackend(cfg, backendName)
	if !ok || snapshotCfg.Enabled {
		return "", false
	}
	return fmt.Sprintf("%s: disabled by runtime config (set %s in %s)", capabilityKey, snapshotConfigEnableHint(backendName), configPath), true
}

func isSnapshotCapabilityKey(key string) bool {
	switch strings.TrimSpace(key) {
	case backend.CapabilitySandboxSnapshot:
		return true
	default:
		return false
	}
}

func snapshotConfigEnableHint(backendName string) string {
	switch strings.TrimSpace(backendName) {
	case "darwin-vz":
		return "backends.darwin-vz.snapshots.enabled: true"
	case "firecracker":
		return "backends.firecracker.snapshots.enabled: true"
	default:
		return "backends.<backend>.snapshots.enabled: true"
	}
}
