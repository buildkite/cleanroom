package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	backendfirecracker "github.com/buildkite/cleanroom/internal/backend/firecracker"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"gopkg.in/yaml.v3"
)

var detectFirecrackerHostSupport = backendfirecracker.DetectHostSupport

type PolicyCommand struct {
	Validate PolicyValidateCommand `cmd:"" help:"Validate policy configuration"`
}

type PolicyValidateCommand struct {
	Chdir string `short:"c" help:"Change to this directory before running commands"`
	JSON  bool   `help:"Print compiled policy as JSON"`
}

type ConfigCommand struct {
	Init     ConfigInitCommand     `cmd:"" help:"Create a runtime config file with defaults"`
	Validate ConfigValidateCommand `cmd:"" help:"Validate runtime config"`
}

type ConfigInitCommand struct {
	Path           string `help:"Output path (default: $XDG_CONFIG_HOME/cleanroom/config.yaml)"`
	Force          bool   `help:"Overwrite existing config file"`
	DefaultBackend string `help:"Default backend value for config (firecracker|darwin-vz)"`
}

type ConfigValidateCommand struct {
	Path string `help:"Runtime config path (default: $XDG_CONFIG_HOME/cleanroom/config.yaml)"`
	JSON bool   `help:"Print validated runtime config as JSON"`
}

func (c *PolicyValidateCommand) Run(ctx *runtimeContext) error {
	cwd, err := resolveCWD(ctx.CWD, c.Chdir)
	if err != nil {
		return err
	}
	compiled, source, err := ctx.Loader.LoadAndCompile(cwd)
	if err != nil {
		return err
	}

	if c.JSON {
		payload := map[string]any{
			"source": source,
			"policy": compiled,
		}
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	color := shouldUseANSI(ctx.Stdout)
	var out strings.Builder
	out.WriteString(renderStatusValueLine("policy valid", source, defaultTerminalPalette().info, color))
	out.WriteByte('\n')
	out.WriteString(renderKeyValueLine("", "policy hash", compiled.Hash, color, defaultTerminalPalette()))
	out.WriteByte('\n')
	_, err = fmt.Fprint(ctx.Stdout, out.String())
	return err
}

func (c *ConfigInitCommand) Run(ctx *runtimeContext) error {
	path, err := resolveRuntimeConfigPath(ctx.CWD, c.Path)
	if err != nil {
		return err
	}

	defaultBackend := strings.TrimSpace(c.DefaultBackend)
	if defaultBackend == "" {
		defaultBackend = hostDefaultBackend()
	}
	switch defaultBackend {
	case "firecracker", "darwin-vz":
	default:
		return fmt.Errorf("unsupported default backend %q (expected firecracker or darwin-vz)", defaultBackend)
	}

	if st, err := os.Stat(path); err == nil && !st.IsDir() && !c.Force {
		return fmt.Errorf("runtime config already exists at %s (use --force to overwrite)", path)
	} else if err == nil && st.IsDir() {
		return fmt.Errorf("runtime config path %s is a directory", path)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	firecrackerSnapshots, warnings := defaultFirecrackerSnapshotConfig(context.Background(), defaultBackend == "firecracker" || hostDefaultBackend() == "firecracker")

	payload, err := yaml.Marshal(defaultRuntimeConfig(defaultBackend, firecrackerSnapshots))
	if err != nil {
		return fmt.Errorf("marshal runtime config template: %w", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write runtime config %s: %w", path, err)
	}
	for _, warning := range warnings {
		if _, err := fmt.Fprint(ctx.stderr(), renderNoticeLine("warning", warning, defaultTerminalPalette().warn, shouldUseANSI(ctx.stderr()))); err != nil {
			return err
		}
	}

	_, err = fmt.Fprintln(ctx.Stdout, renderStatusValueLine("runtime config written", path, defaultTerminalPalette().info, shouldUseANSI(ctx.Stdout)))
	return err
}

func (c *ConfigValidateCommand) Run(ctx *runtimeContext) error {
	path, err := resolveRuntimeConfigPath(ctx.CWD, c.Path)
	if err != nil {
		return err
	}
	if st, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("runtime config does not exist at %s", path)
		}
		return fmt.Errorf("stat %s: %w", path, err)
	} else if st.IsDir() {
		return fmt.Errorf("runtime config path %s is a directory", path)
	}

	cfg, resolvedPath, err := runtimeconfig.LoadPath(path)
	if err != nil {
		return err
	}

	if c.JSON {
		payload := map[string]any{
			"path":            resolvedPath,
			"default_backend": cfg.DefaultBackend,
			"config":          cfg,
		}
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	color := shouldUseANSI(ctx.Stdout)
	var out strings.Builder
	out.WriteString(renderStatusValueLine("runtime config valid", resolvedPath, defaultTerminalPalette().info, color))
	out.WriteByte('\n')
	out.WriteString(renderKeyValueLine("", "default backend", cfg.DefaultBackend, color, defaultTerminalPalette()))
	out.WriteByte('\n')
	_, err = fmt.Fprint(ctx.Stdout, out.String())
	return err
}

func resolveRuntimeConfigPath(cwd, value string) (string, error) {
	path := strings.TrimSpace(value)
	if path == "" {
		return runtimeconfig.Path()
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Join(cwd, path), nil
}

func hostDefaultBackend() string {
	return runtimeconfig.DefaultBackendForHost()
}

func defaultFirecrackerSnapshotConfig(ctx context.Context, emitRuntimeWarnings bool) (runtimeconfig.SnapshotConfig, []string) {
	support := detectFirecrackerHostSupport(ctx, backend.FirecrackerConfig{})
	snapshot := runtimeconfig.SnapshotConfig{Enabled: false, Driver: "file"}

	if !support.SnapshotsUsable {
		warnings := []string{}
		if emitRuntimeWarnings {
			warnings = append(warnings, "firecracker snapshots remain disabled: "+support.SnapshotMessage)
		}
		return snapshot, warnings
	}

	snapshot.Enabled = true
	if support.ZFSUsable {
		snapshot.Driver = "zfs"
		snapshot.ZFSDataset = support.ZFSDatasetRoot
		return snapshot, nil
	}

	return snapshot, []string{"firecracker snapshots default to driver=file: " + support.ZFSMessage}
}

func defaultRuntimeConfig(defaultBackend string, firecrackerSnapshots runtimeconfig.SnapshotConfig) runtimeconfig.Config {
	return runtimeconfig.Config{
		DefaultBackend: defaultBackend,
		Backends: runtimeconfig.Backends{
			Firecracker: runtimeconfig.FirecrackerConfig{
				BinaryPath:  "firecracker",
				KernelImage: "",
				RootFS:      "",
				Services: runtimeconfig.ServicesConfig{
					Docker: runtimeconfig.DockerServiceConfig{
						StartupTimeoutSeconds: 20,
						StorageDriver:         "vfs",
						IPTables:              false,
					},
				},
				Snapshots:            firecrackerSnapshots,
				PrivilegedHelperPath: "/usr/local/sbin/cleanroom-root-helper",
				VCPUs:                2,
				MemoryMiB:            1024,
				GuestCID:             3,
				GuestPort:            10700,
				LaunchSeconds:        30,
			},
			DarwinVZ: runtimeconfig.DarwinVZConfig{
				KernelImage: "",
				RootFS:      "",
				Services: runtimeconfig.ServicesConfig{
					Docker: runtimeconfig.DockerServiceConfig{
						StartupTimeoutSeconds: 20,
						StorageDriver:         "vfs",
						IPTables:              false,
					},
				},
				Snapshots: runtimeconfig.SnapshotConfig{
					Enabled: false,
					Driver:  "apfs",
				},
				VCPUs:         2,
				MemoryMiB:     1024,
				GuestPort:     10700,
				LaunchSeconds: 30,
			},
		},
	}
}
