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
	return resolveHelperBinaryPathWith(os.Getenv(helperEnvVar), exec.LookPath, os.Executable, os.Getwd, os.Stat)
}

func resolveHelperBinaryPathWith(
	envOverride string,
	lookPath func(string) (string, error),
	executable func() (string, error),
	getwd func() (string, error),
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
		siblingAppBundle := sibling + ".app"
		if path, err := resolveHelperCandidatePath(siblingAppBundle, stat); err == nil {
			return path, nil
		}
		if path, err := resolveHelperCandidatePath(sibling, stat); err == nil {
			return path, nil
		}
	}

	if getwd != nil {
		if cwd, err := getwd(); err == nil {
			if path, err := resolvePrebuiltBinaryPathFromWorkdir(cwd, helperBinaryName, stat); err == nil {
				return path, nil
			}
		}
	}

	if path, err := lookPath(helperBinaryName); err == nil {
		return path, nil
	}

	return "", fmt.Errorf(
		"%s helper binary was not found (set %s, build prebuilt binaries with `mise run build`, or install %s in PATH)",
		helperBinaryName,
		helperEnvVar,
		helperBinaryName,
	)
}

func resolvePrebuiltBinaryPathFromWorkdir(startDir, binaryName string, stat func(string) (os.FileInfo, error)) (string, error) {
	trimmedDir := strings.TrimSpace(startDir)
	if trimmedDir == "" {
		return "", errors.New("working directory is empty")
	}
	absStartDir, err := filepath.Abs(trimmedDir)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	trimmedName := strings.TrimSpace(binaryName)
	if trimmedName == "" {
		return "", errors.New("binary name is empty")
	}

	for dir := absStartDir; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "dist", trimmedName)
		if path, err := resolveHelperCandidatePath(candidate+".app", stat); err == nil {
			return path, nil
		}
		if path, err := resolveHelperCandidatePath(candidate, stat); err == nil {
			return path, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "", fmt.Errorf("prebuilt binary %q not found under dist/ from %s", trimmedName, absStartDir)
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
		if strings.EqualFold(filepath.Ext(absPath), ".app") {
			return resolveHelperBundleExecutablePath(absPath, stat)
		}
		return "", fmt.Errorf("%s is a directory", absPath)
	}
	return absPath, nil
}

func resolveHelperBundleExecutablePath(appPath string, stat func(string) (os.FileInfo, error)) (string, error) {
	bundleExecutablePath := filepath.Join(appPath, "Contents", "MacOS", helperBinaryName)
	info, err := stat(bundleExecutablePath)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", bundleExecutablePath)
	}
	return bundleExecutablePath, nil
}
