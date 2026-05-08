//go:build linux

package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type dockerServiceConfig struct {
	Required                 bool
	StartupTimeoutSec        int
	StorageDriver            string
	IPTablesEnabled          bool
	RegistryMirrorHost       string
	RegistryMirrorPort       int
	RegistryMirrorRegistries []string
}

var dockerLookPath = exec.LookPath
var dockerStartProcess = defaultDockerStartProcess
var dockerWaitReady = defaultDockerWaitReady
var dockerCertsDirRoot = "/etc/docker/certs.d"
var dockerStatSocket = defaultDockerStatSocket

func defaultDockerStatSocket() (os.FileInfo, error) {
	return os.Stat("/var/run/docker.sock")
}

var dockerServiceOnce struct {
	sync.Mutex
	done   bool
	result error
}

func resetDockerServiceOnce() {
	dockerServiceOnce.Lock()
	defer dockerServiceOnce.Unlock()
	dockerServiceOnce.done = false
	dockerServiceOnce.result = nil
}

func parseDockerServiceConfig(cmdline string) dockerServiceConfig {
	cfg := dockerServiceConfig{
		StartupTimeoutSec: 20,
		StorageDriver:     "overlay2",
		IPTablesEnabled:   true,
	}

	if v, ok := kernelCmdlineValue(cmdline, "cleanroom_service_docker_required"); ok {
		cfg.Required = v == "1"
	}

	if v, ok := kernelCmdlineValue(cmdline, "cleanroom_service_docker_startup_timeout"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.StartupTimeoutSec = n
		}
	}

	if v, ok := kernelCmdlineValue(cmdline, "cleanroom_service_docker_storage_driver"); ok {
		if strings.TrimSpace(v) != "" {
			cfg.StorageDriver = v
		}
	}

	if v, ok := kernelCmdlineValue(cmdline, "cleanroom_service_docker_iptables"); ok {
		cfg.IPTablesEnabled = v != "0" && v != "false"
	}

	if v, ok := kernelCmdlineValue(cmdline, "cleanroom_service_docker_registry_mirror_host"); ok {
		cfg.RegistryMirrorHost = v
	}

	if v, ok := kernelCmdlineValue(cmdline, "cleanroom_service_docker_registry_mirror_port"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RegistryMirrorPort = n
		}
	}

	if v, ok := kernelCmdlineValue(cmdline, "cleanroom_service_docker_registry_mirror_registries"); ok {
		parts := strings.Split(v, ",")
		cfg.RegistryMirrorRegistries = parts
	}

	return cfg
}

func startDockerServiceOnce(cfg dockerServiceConfig) error {
	dockerServiceOnce.Lock()
	defer dockerServiceOnce.Unlock()

	if dockerServiceOnce.done {
		return dockerServiceOnce.result
	}

	dockerServiceOnce.result = doStartDockerService(cfg)
	dockerServiceOnce.done = true
	return dockerServiceOnce.result
}

func doStartDockerService(cfg dockerServiceConfig) error {
	if !cfg.Required {
		return nil
	}

	dockerdPath, err := dockerLookPath("dockerd")
	if err != nil {
		return nil
	}

	for _, dir := range []string{"/var/log", "/var/lib/docker", "/etc/docker", "/var/run", "/sys/fs/cgroup"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}

	if err := unix.Mount("none", "/sys/fs/cgroup", "cgroup2", 0, ""); err != nil && err != unix.EBUSY {
		log.Printf("mount cgroup2: %v", err)
	}

	mirrorSet := cfg.RegistryMirrorHost != "" && cfg.RegistryMirrorPort > 0 && len(cfg.RegistryMirrorRegistries) > 0
	mirrorEndpointSet := cfg.RegistryMirrorHost != "" && cfg.RegistryMirrorPort > 0
	if mirrorSet {
		for _, registry := range cfg.RegistryMirrorRegistries {
			if registry == "" || strings.ContainsAny(registry, " \t\n\r") || strings.Contains(registry, "/") {
				continue
			}
			dir := filepath.Join(dockerCertsDirRoot, registry)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create registry cert dir %s: %w", dir, err)
			}
			content := fmt.Sprintf("server = \"http://%s:%d/registry/%s\"\n", cfg.RegistryMirrorHost, cfg.RegistryMirrorPort, registry)
			if err := os.WriteFile(filepath.Join(dir, "hosts.toml"), []byte(content), 0o644); err != nil {
				return fmt.Errorf("write hosts.toml for %s: %w", registry, err)
			}
		}
	}

	_, statErr := dockerStatSocket()
	if statErr != nil {
		if !os.IsNotExist(statErr) {
			return fmt.Errorf("stat docker socket: %w", statErr)
		}

		logFile, err := os.OpenFile("/var/log/dockerd.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("open dockerd log: %w", err)
		}

		args := []string{
			"--host=unix:///var/run/docker.sock",
			"--storage-driver=" + cfg.StorageDriver,
		}
		if !cfg.IPTablesEnabled {
			args = append(args, "--iptables=false")
		}
		if mirrorEndpointSet {
			args = append(args,
				fmt.Sprintf("--registry-mirror=http://%s:%d", cfg.RegistryMirrorHost, cfg.RegistryMirrorPort),
				fmt.Sprintf("--insecure-registry=%s:%d", cfg.RegistryMirrorHost, cfg.RegistryMirrorPort),
			)
		}

		cmd := exec.Command(dockerdPath, args...)
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		if err := dockerStartProcess(cmd); err != nil {
			logFile.Close()
			return fmt.Errorf("start dockerd: %w", err)
		}
		logFile.Close()
	}

	return dockerWaitReady(cfg.StartupTimeoutSec)
}

func defaultDockerStartProcess(cmd *exec.Cmd) error {
	return cmd.Start()
}

func defaultDockerWaitReady(timeoutSec int) error {
	const socketPath = "/var/run/docker.sock"
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			_, writeErr := fmt.Fprint(conn, "GET /version HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
			if writeErr == nil {
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 32)
				n, _ := conn.Read(buf)
				conn.Close()
				if strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200") {
					return nil
				}
			} else {
				conn.Close()
			}
		}
		if time.Now().After(deadline) {
			elapsed := time.Duration(timeoutSec) * time.Second
			return fmt.Errorf("dockerd not ready after %v", elapsed)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
