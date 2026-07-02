package sporevm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContextEnvFromProcessCarriesSporePathEnv(t *testing.T) {
	t.Setenv("HOME", "/Users/example")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("SPOREVM_ROOTFS_CACHE_DIR", "/tmp/rootfs")
	t.Setenv("SPOREVM_RUNTIME_DIR", "")

	got := contextEnvFromProcess()
	if got["HOME"] != "/Users/example" {
		t.Fatalf("HOME = %q", got["HOME"])
	}
	if got["SPOREVM_ROOTFS_CACHE_DIR"] != "/tmp/rootfs" {
		t.Fatalf("SPOREVM_ROOTFS_CACHE_DIR = %q", got["SPOREVM_ROOTFS_CACHE_DIR"])
	}
	if got["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("PATH = %q", got["PATH"])
	}
	if _, ok := got["SPOREVM_RUNTIME_DIR"]; ok {
		t.Fatal("empty SPOREVM_RUNTIME_DIR should be omitted")
	}
}

func TestDefaultSporeExecutableLooksInPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spore")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake spore executable: %v", err)
	}
	t.Setenv("PATH", dir)

	if got := defaultSporeExecutable(); got != path {
		t.Fatalf("default spore executable = %q, want %q", got, path)
	}
}
