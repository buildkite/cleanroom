package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/gateway"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type DoctorCommand struct {
	Chdir   string `short:"c" help:"Change to this directory before running commands"`
	Backend string `help:"Execution backend to diagnose (defaults to runtime config or host default)"`
	JSON    bool   `help:"Print doctor report as JSON"`
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
	capabilities := backend.CapabilitiesForAdapter(adapter)
	gwCredentials := gateway.NewEnvCredentialProvider()
	gwHosts := gwCredentials.ConfiguredHosts()
	gwRoutes := gateway.Routes()
	credSummary := "none configured"
	if len(gwHosts) > 0 {
		credSummary = strings.Join(gwHosts, ", ")
	}
	routeSummary := strings.Join(gwRoutes, ", ")

	checks := []backend.DoctorCheck{
		{Name: "runtime_config", Status: "pass", Message: fmt.Sprintf("using runtime config path %s", ctx.ConfigPath)},
		{Name: "backend", Status: "pass", Message: fmt.Sprintf("selected backend %s", backendName)},
		{
			Name:    "gateway_listen",
			Status:  "pass",
			Message: fmt.Sprintf("default listen %s (port %d; override with cleanroom serve --gateway-listen)", gateway.DefaultListenAddr, gateway.DefaultPort),
		},
		{
			Name:    "gateway_routes",
			Status:  "pass",
			Message: fmt.Sprintf("enabled routes: %s", routeSummary),
		},
		{
			Name:    "gateway_credentials",
			Status:  "pass",
			Message: fmt.Sprintf("configured credential hosts: %s", credSummary),
		},
	}
	for _, key := range backend.SortedCapabilityKeys(capabilities) {
		status := "warn"
		message := fmt.Sprintf("%s: unsupported", key)
		if capabilities[key] {
			status = "pass"
			message = fmt.Sprintf("%s: supported", key)
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
			FirecrackerConfig: mergeBackendConfig(backendName, 0, ctx.Config),
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

var capabilityNameReplacer = strings.NewReplacer(".", "_", "-", "_")

func capabilityCheckName(key string) string {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return "capability_unknown"
	}
	return "capability_" + capabilityNameReplacer.Replace(trimmed)
}

func mergeBackendConfig(backendName string, launchSeconds int64, cfg runtimeconfig.Config) backend.FirecrackerConfig {
	out := backend.FirecrackerConfig{
		BinaryPath:           cfg.Backends.Firecracker.BinaryPath,
		KernelImagePath:      cfg.Backends.Firecracker.KernelImage,
		RootFSPath:           cfg.Backends.Firecracker.RootFS,
		DockerStartupSeconds: cfg.Backends.Firecracker.Services.Docker.StartupTimeoutSeconds,
		DockerStorageDriver:  cfg.Backends.Firecracker.Services.Docker.StorageDriver,
		DockerIPTables:       cfg.Backends.Firecracker.Services.Docker.IPTables,
		PrivilegedMode:       cfg.Backends.Firecracker.PrivilegedMode,
		PrivilegedHelperPath: cfg.Backends.Firecracker.PrivilegedHelperPath,
		VCPUs:                cfg.Backends.Firecracker.VCPUs,
		MemoryMiB:            cfg.Backends.Firecracker.MemoryMiB,
		GuestCID:             cfg.Backends.Firecracker.GuestCID,
		GuestPort:            cfg.Backends.Firecracker.GuestPort,
		LaunchSeconds:        cfg.Backends.Firecracker.LaunchSeconds,
	}
	if backendName == "darwin-vz" {
		out.KernelImagePath = cfg.Backends.DarwinVZ.KernelImage
		out.RootFSPath = cfg.Backends.DarwinVZ.RootFS
		out.DockerStartupSeconds = cfg.Backends.DarwinVZ.Services.Docker.StartupTimeoutSeconds
		out.DockerStorageDriver = cfg.Backends.DarwinVZ.Services.Docker.StorageDriver
		out.DockerIPTables = cfg.Backends.DarwinVZ.Services.Docker.IPTables
		out.VCPUs = cfg.Backends.DarwinVZ.VCPUs
		out.MemoryMiB = cfg.Backends.DarwinVZ.MemoryMiB
		out.GuestPort = cfg.Backends.DarwinVZ.GuestPort
		out.LaunchSeconds = cfg.Backends.DarwinVZ.LaunchSeconds
	}

	out.Launch = true
	if launchSeconds != 0 {
		out.LaunchSeconds = launchSeconds
	}
	return out
}
