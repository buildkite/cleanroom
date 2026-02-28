package darwinvz

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestResolveHelperBinaryPathPrefersEnvOverride(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	override := tmp + "/cleanroom-darwin-vz-override"
	if err := os.WriteFile(override, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write override helper: %v", err)
	}

	got, err := resolveHelperBinaryPathWith(
		override,
		func(string) (string, error) { return "", errors.New("not found") },
		func() (string, error) { return "", errors.New("no executable") },
		os.Stat,
	)
	if err != nil {
		t.Fatalf("resolveHelperBinaryPathWith returned error: %v", err)
	}
	if got != override {
		t.Fatalf("unexpected helper path: got %q want %q", got, override)
	}
}

func TestResolveHelperBinaryPathUsesSiblingBeforePath(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	self := tmp + "/cleanroom"
	sibling := tmp + "/cleanroom-darwin-vz"
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write self binary: %v", err)
	}
	if err := os.WriteFile(sibling, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write sibling helper: %v", err)
	}

	got, err := resolveHelperBinaryPathWith(
		"",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-darwin-vz", nil },
		func() (string, error) { return self, nil },
		os.Stat,
	)
	if err != nil {
		t.Fatalf("resolveHelperBinaryPathWith returned error: %v", err)
	}
	if got != sibling {
		t.Fatalf("unexpected helper path: got %q want %q", got, sibling)
	}
}

func TestResolveHelperBinaryPathFallsBackToPATH(t *testing.T) {
	t.Parallel()

	got, err := resolveHelperBinaryPathWith(
		"",
		func(string) (string, error) { return "/usr/local/bin/cleanroom-darwin-vz", nil },
		func() (string, error) { return "", errors.New("no executable") },
		func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
	)
	if err != nil {
		t.Fatalf("resolveHelperBinaryPathWith returned error: %v", err)
	}
	if got != "/usr/local/bin/cleanroom-darwin-vz" {
		t.Fatalf("unexpected helper path: got %q", got)
	}
}

func TestResolveHelperBinaryPathReturnsActionableError(t *testing.T) {
	t.Parallel()

	_, err := resolveHelperBinaryPathWith(
		"",
		func(string) (string, error) { return "", errors.New("not found") },
		func() (string, error) { return "", errors.New("no executable") },
		func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "cleanroom-darwin-vz") || !strings.Contains(got, "CLEANROOM_DARWIN_VZ_HELPER") {
		t.Fatalf("unexpected error: %v", err)
	}
}
