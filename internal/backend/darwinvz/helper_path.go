package darwinvz

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	helperBinaryName = "cleanroom-darwin-vz"
	helperEnvVar     = "CLEANROOM_DARWIN_VZ_HELPER"
)

func resolveHelperBinaryPath() (string, error) {
	return resolveHelperBinaryPathWith(os.Getenv(helperEnvVar), exec.LookPath, os.Executable, os.Stat)
}

func resolveHelperBinaryPathWith(
	envOverride string,
	lookPath func(string) (string, error),
	executable func() (string, error),
	stat func(string) (os.FileInfo, error),
) (string, error) {
	if override := strings.TrimSpace(envOverride); override != "" {
		path, err := resolveHelperCandidatePath(override, stat)
		if err != nil {
			return "", fmt.Errorf("resolve darwin-vz helper from %s=%q: %w", helperEnvVar, override, err)
		}
		return path, nil
	}

	if self, err := executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), helperBinaryName)
		if path, err := resolveHelperCandidatePath(sibling, stat); err == nil {
			return path, nil
		}
	}

	if path, err := lookPath(helperBinaryName); err == nil {
		return path, nil
	}

	return "", fmt.Errorf(
		"%s helper binary was not found (set %s or install %s in PATH)",
		helperBinaryName,
		helperEnvVar,
		helperBinaryName,
	)
}

func resolveHelperCandidatePath(path string, stat func(string) (os.FileInfo, error)) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", errors.New("path is empty")
	}

	absPath, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}
	info, err := stat(absPath)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", absPath)
	}
	return absPath, nil
}
