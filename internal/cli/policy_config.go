package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	backenddarwinvz "github.com/buildkite/cleanroom/internal/backend/darwinvz"
	backendfirecracker "github.com/buildkite/cleanroom/internal/backend/firecracker"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"gopkg.in/yaml.v3"
)

var detectFirecrackerHostSupport = backendfirecracker.DetectHostSupport
var detectDarwinVZSnapshotSupport = backenddarwinvz.DetectSnapshotSupport

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
	darwinVZSnapshots := runtimeconfig.SnapshotConfig{Enabled: false, Driver: "apfs"}
	if defaultBackend == "darwin-vz" {
		var darwinVZWarnings []string
		darwinVZSnapshots, darwinVZWarnings = defaultDarwinVZSnapshotConfig(true)
		warnings = append(warnings, darwinVZWarnings...)
	}

	payload, err := marshalRuntimeConfigTemplate(defaultRuntimeConfig(defaultBackend, firecrackerSnapshots, darwinVZSnapshots))
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

func defaultDarwinVZSnapshotConfig(emitRuntimeWarnings bool) (runtimeconfig.SnapshotConfig, []string) {
	snapshot := runtimeconfig.SnapshotConfig{Enabled: false, Driver: "apfs"}
	support := detectDarwinVZSnapshotSupport()
	if !support.Usable {
		warnings := []string{}
		if emitRuntimeWarnings {
			message := strings.TrimSpace(support.Message)
			if message == "" {
				message = "darwin-vz snapshots remain disabled: snapshot runtime is not usable"
			}
			warnings = append(warnings, message)
		}
		return snapshot, warnings
	}

	snapshot.Enabled = true
	return snapshot, nil
}

type runtimeConfigTemplate struct {
	DefaultBackend string                     `yaml:"default_backend,omitempty"`
	Backends       runtimeConfigTemplateNodes `yaml:"backends"`
}

type runtimeConfigTemplateNodes struct {
	Firecracker *runtimeConfigFirecracker `yaml:"firecracker,omitempty"`
	DarwinVZ    *runtimeConfigDarwinVZ    `yaml:"darwin-vz,omitempty"`
}

type runtimeConfigFirecracker struct {
	BinaryPath           string                `yaml:"binary_path,omitempty"`
	KernelImage          string                `yaml:"kernel_image,omitempty"`
	RootFS               string                `yaml:"rootfs,omitempty"`
	Services             runtimeConfigServices `yaml:"services,omitempty"`
	Snapshots            runtimeConfigSnapshot `yaml:"snapshots,omitempty"`
	PrivilegedHelperPath string                `yaml:"privileged_helper_path,omitempty"`
	VCPUs                int64                 `yaml:"vcpus,omitempty"`
	MemoryMiB            int64                 `yaml:"memory_mib,omitempty"`
	GuestCID             uint32                `yaml:"guest_cid,omitempty"`
	GuestPort            uint32                `yaml:"guest_port,omitempty"`
	LaunchSeconds        int64                 `yaml:"launch_seconds,omitempty"`
}

type runtimeConfigDarwinVZ struct {
	KernelImage        string                `yaml:"kernel_image,omitempty"`
	RootFS             string                `yaml:"rootfs,omitempty"`
	MinimumRootFSBytes string                `yaml:"minimum_rootfs_bytes,omitempty"`
	Services           runtimeConfigServices `yaml:"services,omitempty"`
	Snapshots          runtimeConfigSnapshot `yaml:"snapshots,omitempty"`
	VCPUs              int64                 `yaml:"vcpus,omitempty"`
	MemoryMiB          int64                 `yaml:"memory_mib,omitempty"`
	GuestPort          uint32                `yaml:"guest_port,omitempty"`
	LaunchSeconds      int64                 `yaml:"launch_seconds,omitempty"`
}

type runtimeConfigServices struct {
	Docker runtimeConfigDockerService `yaml:"docker,omitempty"`
}

type runtimeConfigDockerService struct {
	StartupTimeoutSeconds int64  `yaml:"startup_timeout_seconds,omitempty"`
	StorageDriver         string `yaml:"storage_driver,omitempty"`
	IPTables              bool   `yaml:"iptables,omitempty"`
}

type runtimeConfigSnapshot struct {
	Enabled               bool   `yaml:"enabled,omitempty"`
	Driver                string `yaml:"driver,omitempty"`
	BaseDir               string `yaml:"base_dir,omitempty"`
	ZFSDataset            string `yaml:"zfs_dataset,omitempty"`
	QuiesceTimeoutSeconds int64  `yaml:"quiesce_timeout_seconds,omitempty"`
}

func defaultRuntimeConfig(defaultBackend string, firecrackerSnapshots, darwinVZSnapshots runtimeconfig.SnapshotConfig) runtimeConfigTemplate {
	tpl := runtimeConfigTemplate{
		Backends: runtimeConfigTemplateNodes{},
	}

	switch defaultBackend {
	case "darwin-vz":
		tpl.Backends.DarwinVZ = &runtimeConfigDarwinVZ{
			KernelImage:        "",
			RootFS:             "",
			MinimumRootFSBytes: "4GiB",
			Services: runtimeConfigServices{
				Docker: runtimeConfigDockerService{
					StartupTimeoutSeconds: 20,
					StorageDriver:         "vfs",
					IPTables:              false,
				},
			},
			Snapshots:     runtimeConfigSnapshot(darwinVZSnapshots),
			VCPUs:         2,
			MemoryMiB:     4096,
			GuestPort:     10700,
			LaunchSeconds: 30,
		}
	default:
		tpl.Backends.Firecracker = &runtimeConfigFirecracker{
			BinaryPath:  "firecracker",
			KernelImage: "",
			RootFS:      "",
			Services: runtimeConfigServices{
				Docker: runtimeConfigDockerService{
					StartupTimeoutSeconds: 20,
					StorageDriver:         "vfs",
					IPTables:              false,
				},
			},
			Snapshots:            runtimeConfigSnapshot(firecrackerSnapshots),
			PrivilegedHelperPath: "/usr/local/sbin/cleanroom-root-helper",
			VCPUs:                2,
			MemoryMiB:            1024,
			GuestCID:             3,
			GuestPort:            10700,
			LaunchSeconds:        30,
		}
	}

	return tpl
}

func marshalRuntimeConfigTemplate(cfg runtimeConfigTemplate) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
