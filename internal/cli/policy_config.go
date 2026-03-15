package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"gopkg.in/yaml.v3"
)

type PolicyCommand struct {
	Validate PolicyValidateCommand `cmd:"" help:"Validate policy configuration"`
}

type PolicyValidateCommand struct {
	Chdir string `short:"c" help:"Change to this directory before running commands"`
	JSON  bool   `help:"Print compiled policy as JSON"`
}

type ConfigCommand struct {
	Init ConfigInitCommand `cmd:"" help:"Create a runtime config file with defaults"`
}

type ConfigInitCommand struct {
	Path           string `help:"Output path (default: $XDG_CONFIG_HOME/cleanroom/config.yaml)"`
	Force          bool   `help:"Overwrite existing config file"`
	DefaultBackend string `help:"Default backend value for config (firecracker|darwin-vz)"`
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
	path := strings.TrimSpace(c.Path)
	if path == "" {
		resolved, err := runtimeconfig.Path()
		if err != nil {
			return err
		}
		path = resolved
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(ctx.CWD, path)
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

	payload, err := yaml.Marshal(defaultRuntimeConfig(defaultBackend))
	if err != nil {
		return fmt.Errorf("marshal runtime config template: %w", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write runtime config %s: %w", path, err)
	}

	_, err = fmt.Fprintln(ctx.Stdout, renderStatusValueLine("runtime config written", path, defaultTerminalPalette().info, shouldUseANSI(ctx.Stdout)))
	return err
}

func hostDefaultBackend() string {
	return runtimeconfig.DefaultBackendForHost()
}

func defaultRuntimeConfig(defaultBackend string) runtimeconfig.Config {
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
				Snapshots: runtimeconfig.SnapshotConfig{
					Enabled: false,
					Driver:  "file",
				},
				PrivilegedMode:       "sudo",
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
