package runtimeconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DefaultBackend string    `yaml:"default_backend"`
	Workspace      Workspace `yaml:"workspace"`
	Backends       Backends  `yaml:"backends"`
}

type Workspace struct {
	Mode    string `yaml:"mode"`    // copy|mount
	Persist string `yaml:"persist"` // discard|commit
	Access  string `yaml:"access"`  // rw|ro
}

type Backends struct {
	Firecracker FirecrackerConfig `yaml:"firecracker"`
}

type FirecrackerConfig struct {
	BinaryPath    string `yaml:"binary_path"`
	KernelImage   string `yaml:"kernel_image"`
	RootFS        string `yaml:"rootfs"`
	VCPUs         int64  `yaml:"vcpus"`
	MemoryMiB     int64  `yaml:"memory_mib"`
	GuestCID      uint32 `yaml:"guest_cid"`
	GuestPort     uint32 `yaml:"guest_port"`
	RetainWrites  bool   `yaml:"retain_writes"`
	LaunchSeconds int64  `yaml:"launch_seconds"` // VM boot/guest-agent readiness timeout
}

func Path() (string, error) {
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome != "" {
		return filepath.Join(configHome, "cleanroom", "config.yaml"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "cleanroom", "config.yaml"), nil
}

func Load() (Config, string, error) {
	path, err := Path()
	if err != nil {
		return Config{}, "", err
	}

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, path, nil
		}
		return Config{}, path, fmt.Errorf("read %s: %w", path, err)
	}

	cfg := Config{}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, path, fmt.Errorf("parse %s: %w", path, err)
	}

	cfg.DefaultBackend = strings.TrimSpace(cfg.DefaultBackend)
	cfg.Workspace.Mode = strings.TrimSpace(strings.ToLower(cfg.Workspace.Mode))
	if cfg.Workspace.Mode == "" {
		cfg.Workspace.Mode = "copy"
	}
	cfg.Workspace.Persist = strings.TrimSpace(strings.ToLower(cfg.Workspace.Persist))
	if cfg.Workspace.Persist == "" {
		cfg.Workspace.Persist = "discard"
	}
	cfg.Workspace.Access = strings.TrimSpace(strings.ToLower(cfg.Workspace.Access))
	if cfg.Workspace.Access == "" {
		cfg.Workspace.Access = "rw"
	}
	return cfg, path, nil
}
