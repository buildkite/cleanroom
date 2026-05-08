//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseDockerServiceConfigReadsKnownKeys(t *testing.T) {
	t.Parallel()

	cmdline := "cleanroom_service_docker_required=1" +
		" cleanroom_service_docker_startup_timeout=30" +
		" cleanroom_service_docker_storage_driver=btrfs" +
		" cleanroom_service_docker_iptables=false" +
		" cleanroom_service_docker_registry_mirror_host=mirror.example.com" +
		" cleanroom_service_docker_registry_mirror_port=5000" +
		" cleanroom_service_docker_registry_mirror_registries=docker.io,ghcr.io"

	cfg := parseDockerServiceConfig(cmdline)

	if !cfg.Required {
		t.Error("expected Required=true")
	}
	if cfg.StartupTimeoutSec != 30 {
		t.Errorf("expected StartupTimeoutSec=30, got %d", cfg.StartupTimeoutSec)
	}
	if cfg.StorageDriver != "btrfs" {
		t.Errorf("expected StorageDriver=btrfs, got %q", cfg.StorageDriver)
	}
	if cfg.IPTablesEnabled {
		t.Error("expected IPTablesEnabled=false")
	}
	if cfg.RegistryMirrorHost != "mirror.example.com" {
		t.Errorf("expected RegistryMirrorHost=mirror.example.com, got %q", cfg.RegistryMirrorHost)
	}
	if cfg.RegistryMirrorPort != 5000 {
		t.Errorf("expected RegistryMirrorPort=5000, got %d", cfg.RegistryMirrorPort)
	}
	if len(cfg.RegistryMirrorRegistries) != 2 || cfg.RegistryMirrorRegistries[0] != "docker.io" || cfg.RegistryMirrorRegistries[1] != "ghcr.io" {
		t.Errorf("unexpected RegistryMirrorRegistries: %v", cfg.RegistryMirrorRegistries)
	}
}

func TestParseDockerServiceConfigDefaults(t *testing.T) {
	t.Parallel()

	cfg := parseDockerServiceConfig("")

	if cfg.Required {
		t.Error("expected Required=false")
	}
	if cfg.StartupTimeoutSec != 20 {
		t.Errorf("expected StartupTimeoutSec=20, got %d", cfg.StartupTimeoutSec)
	}
	if cfg.StorageDriver != "overlay2" {
		t.Errorf("expected StorageDriver=overlay2, got %q", cfg.StorageDriver)
	}
	if !cfg.IPTablesEnabled {
		t.Error("expected IPTablesEnabled=true")
	}
}

func TestParseDockerServiceConfigRequiredZero(t *testing.T) {
	t.Parallel()

	cfg := parseDockerServiceConfig("cleanroom_service_docker_required=0")

	if cfg.Required {
		t.Error("expected Required=false for value 0")
	}
}

func TestParseDockerServiceConfigInvalidTimeout(t *testing.T) {
	t.Parallel()

	cfg := parseDockerServiceConfig("cleanroom_service_docker_startup_timeout=abc")

	if cfg.StartupTimeoutSec != 20 {
		t.Errorf("expected default StartupTimeoutSec=20 for invalid value, got %d", cfg.StartupTimeoutSec)
	}
}

func TestParseDockerServiceConfigInvalidMirrorPort(t *testing.T) {
	t.Parallel()

	cfg := parseDockerServiceConfig("cleanroom_service_docker_registry_mirror_port=abc")

	if cfg.RegistryMirrorPort != 0 {
		t.Errorf("expected RegistryMirrorPort=0 for invalid value, got %d", cfg.RegistryMirrorPort)
	}
}

func TestParseDockerServiceConfigIPTablesDisabled(t *testing.T) {
	t.Parallel()

	cfg := parseDockerServiceConfig("cleanroom_service_docker_iptables=0")

	if cfg.IPTablesEnabled {
		t.Error("expected IPTablesEnabled=false for value 0")
	}
}

func TestStartDockerServiceOnceSkipsWhenNotRequired(t *testing.T) {
	resetDockerServiceOnce()
	t.Cleanup(resetDockerServiceOnce)

	spawnCalled := false
	waitCalled := false

	origStart := dockerStartProcess
	origWait := dockerWaitReady
	t.Cleanup(func() {
		dockerStartProcess = origStart
		dockerWaitReady = origWait
	})

	dockerStartProcess = func(cmd *exec.Cmd) error {
		spawnCalled = true
		return nil
	}
	dockerWaitReady = func(timeoutSec int) error {
		waitCalled = true
		return nil
	}

	cfg := dockerServiceConfig{Required: false}
	if err := startDockerServiceOnce(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spawnCalled {
		t.Error("expected dockerStartProcess not to be called")
	}
	if waitCalled {
		t.Error("expected dockerWaitReady not to be called")
	}
}

func TestStartDockerServiceOnceSkipsWhenDockerdMissing(t *testing.T) {
	resetDockerServiceOnce()
	t.Cleanup(resetDockerServiceOnce)

	spawnCalled := false

	origLook := dockerLookPath
	origStart := dockerStartProcess
	t.Cleanup(func() {
		dockerLookPath = origLook
		dockerStartProcess = origStart
	})

	dockerLookPath = func(file string) (string, error) {
		return "", fmt.Errorf("dockerd not found")
	}
	dockerStartProcess = func(cmd *exec.Cmd) error {
		spawnCalled = true
		return nil
	}

	cfg := dockerServiceConfig{Required: true, StartupTimeoutSec: 20, StorageDriver: "overlay2", IPTablesEnabled: true}
	if err := startDockerServiceOnce(cfg); err != nil {
		t.Fatalf("expected nil when dockerd missing, got %v", err)
	}
	if spawnCalled {
		t.Error("expected dockerStartProcess not to be called when dockerd missing")
	}
}

func TestStartDockerServiceOnceWritesRegistryMirror(t *testing.T) {
	resetDockerServiceOnce()
	t.Cleanup(resetDockerServiceOnce)

	tmpDir := t.TempDir()

	origCertsDir := dockerCertsDirRoot
	origLook := dockerLookPath
	origStart := dockerStartProcess
	origWait := dockerWaitReady
	t.Cleanup(func() {
		dockerCertsDirRoot = origCertsDir
		dockerLookPath = origLook
		dockerStartProcess = origStart
		dockerWaitReady = origWait
	})

	dockerCertsDirRoot = tmpDir
	dockerLookPath = func(file string) (string, error) {
		return "/usr/bin/dockerd", nil
	}
	dockerStartProcess = func(cmd *exec.Cmd) error { return nil }
	dockerWaitReady = func(timeoutSec int) error { return nil }

	cfg := dockerServiceConfig{
		Required:                 true,
		StartupTimeoutSec:        20,
		StorageDriver:            "overlay2",
		IPTablesEnabled:          true,
		RegistryMirrorHost:       "mirror.example.com",
		RegistryMirrorPort:       5000,
		RegistryMirrorRegistries: []string{"docker.io"},
	}
	if err := startDockerServiceOnce(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	hostsToml := filepath.Join(tmpDir, "docker.io", "hosts.toml")
	data, err := os.ReadFile(hostsToml)
	if err != nil {
		t.Fatalf("failed to read hosts.toml: %v", err)
	}
	want := `server = "http://mirror.example.com:5000/registry/docker.io"` + "\n"
	if string(data) != want {
		t.Errorf("unexpected hosts.toml content:\n got %q\nwant %q", string(data), want)
	}
}

func TestStartDockerServiceOnceSkipsRegistriesWithSlashes(t *testing.T) {
	resetDockerServiceOnce()
	t.Cleanup(resetDockerServiceOnce)

	tmpDir := t.TempDir()

	origCertsDir := dockerCertsDirRoot
	origLook := dockerLookPath
	origStart := dockerStartProcess
	origWait := dockerWaitReady
	t.Cleanup(func() {
		dockerCertsDirRoot = origCertsDir
		dockerLookPath = origLook
		dockerStartProcess = origStart
		dockerWaitReady = origWait
	})

	dockerCertsDirRoot = tmpDir
	dockerLookPath = func(file string) (string, error) {
		return "/usr/bin/dockerd", nil
	}
	dockerStartProcess = func(cmd *exec.Cmd) error { return nil }
	dockerWaitReady = func(timeoutSec int) error { return nil }

	cfg := dockerServiceConfig{
		Required:                 true,
		StartupTimeoutSec:        20,
		StorageDriver:            "overlay2",
		IPTablesEnabled:          true,
		RegistryMirrorHost:       "mirror.example.com",
		RegistryMirrorPort:       5000,
		RegistryMirrorRegistries: []string{"bad/path", "docker.io"},
	}
	if err := startDockerServiceOnce(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	badPath := filepath.Join(tmpDir, "bad", "path", "hosts.toml")
	if _, err := os.Stat(badPath); err == nil {
		t.Error("expected bad/path registry to be skipped, but hosts.toml was written")
	}

	goodPath := filepath.Join(tmpDir, "docker.io", "hosts.toml")
	if _, err := os.Stat(goodPath); err != nil {
		t.Errorf("expected docker.io hosts.toml to exist: %v", err)
	}
}

func TestStartDockerServiceOnceSkipsSpawnWhenSocketExists(t *testing.T) {
	resetDockerServiceOnce()
	t.Cleanup(resetDockerServiceOnce)

	tmpDir := t.TempDir()
	socketFile := filepath.Join(tmpDir, "docker.sock")
	f, err := os.Create(socketFile)
	if err != nil {
		t.Fatalf("failed to create fake socket: %v", err)
	}
	f.Close()

	origStat := dockerStatSocket
	origLook := dockerLookPath
	origStart := dockerStartProcess
	origWait := dockerWaitReady
	t.Cleanup(func() {
		dockerStatSocket = origStat
		dockerLookPath = origLook
		dockerStartProcess = origStart
		dockerWaitReady = origWait
	})

	dockerStatSocket = func() (os.FileInfo, error) {
		return os.Stat(socketFile)
	}
	dockerLookPath = func(file string) (string, error) {
		return "/usr/bin/dockerd", nil
	}

	spawnCalled := false
	waitCalled := false
	dockerStartProcess = func(cmd *exec.Cmd) error {
		spawnCalled = true
		return nil
	}
	dockerWaitReady = func(timeoutSec int) error {
		waitCalled = true
		return nil
	}

	cfg := dockerServiceConfig{Required: true, StartupTimeoutSec: 20, StorageDriver: "overlay2", IPTablesEnabled: true}
	if err := startDockerServiceOnce(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spawnCalled {
		t.Error("expected dockerStartProcess NOT to be called when socket exists")
	}
	if !waitCalled {
		t.Error("expected dockerWaitReady to be called even when socket exists")
	}
}

func TestStartDockerServiceOnceIsIdempotent(t *testing.T) {
	resetDockerServiceOnce()
	t.Cleanup(resetDockerServiceOnce)

	origLook := dockerLookPath
	origStart := dockerStartProcess
	origWait := dockerWaitReady
	t.Cleanup(func() {
		dockerLookPath = origLook
		dockerStartProcess = origStart
		dockerWaitReady = origWait
	})

	spawnCount := 0
	dockerLookPath = func(file string) (string, error) {
		return "/usr/bin/dockerd", nil
	}
	dockerStartProcess = func(cmd *exec.Cmd) error {
		spawnCount++
		return nil
	}
	dockerWaitReady = func(timeoutSec int) error { return nil }

	cfg := dockerServiceConfig{Required: true, StartupTimeoutSec: 20, StorageDriver: "overlay2", IPTablesEnabled: true}
	if err := startDockerServiceOnce(cfg); err != nil {
		t.Fatalf("first call unexpected error: %v", err)
	}
	if err := startDockerServiceOnce(cfg); err != nil {
		t.Fatalf("second call unexpected error: %v", err)
	}
	if spawnCount > 1 {
		t.Errorf("expected dockerStartProcess invoked at most once, got %d", spawnCount)
	}
}

func TestStartDockerServiceOnceCachesError(t *testing.T) {
	resetDockerServiceOnce()
	t.Cleanup(resetDockerServiceOnce)

	origLook := dockerLookPath
	origStart := dockerStartProcess
	origWait := dockerWaitReady
	t.Cleanup(func() {
		dockerLookPath = origLook
		dockerStartProcess = origStart
		dockerWaitReady = origWait
	})

	spawnCount := 0
	waitCount := 0
	sentinel := errors.New("dockerd timeout")

	dockerLookPath = func(file string) (string, error) {
		return "/usr/bin/dockerd", nil
	}
	dockerStartProcess = func(cmd *exec.Cmd) error {
		spawnCount++
		return nil
	}
	dockerWaitReady = func(timeoutSec int) error {
		waitCount++
		return sentinel
	}

	cfg := dockerServiceConfig{Required: true, StartupTimeoutSec: 20, StorageDriver: "overlay2", IPTablesEnabled: true}

	err1 := startDockerServiceOnce(cfg)
	if !errors.Is(err1, sentinel) {
		t.Fatalf("expected sentinel error on first call, got %v", err1)
	}

	err2 := startDockerServiceOnce(cfg)
	if !errors.Is(err2, sentinel) {
		t.Fatalf("expected cached sentinel error on second call, got %v", err2)
	}
	if spawnCount != 1 {
		t.Errorf("expected spawn called once, got %d", spawnCount)
	}
	if waitCount != 1 {
		t.Errorf("expected wait called once, got %d", waitCount)
	}
}
