package darwinvz

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/buildkite/cleanroom/internal/runtimeassets"
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
		path, err := runtimeassets.ResolveFileOrAppBundleExecutable(override, helperBinaryName, stat)
		if err != nil {
			return "", fmt.Errorf("resolve darwin-vz helper from %s=%q: %w", helperEnvVar, override, err)
		}
		return path, nil
	}

	candidates := append(runtimeassets.InstalledLibexecCandidates(executable, runtimeassets.DarwinHelperNames()...), runtimeassets.InstalledSiblingCandidates(executable, runtimeassets.DarwinHelperNames()...)...)
	candidates = append(candidates, runtimeassets.StagedDistCandidates(getwd, runtime.GOOS, runtime.GOARCH, "libexec/cleanroom/cleanroom-darwin-vz.app", "libexec/cleanroom/cleanroom-darwin-vz")...)
	candidates = append(candidates, runtimeassets.DistCandidates(getwd, runtimeassets.DarwinHelperNames()...)...)
	for _, candidate := range candidates {
		if path, err := runtimeassets.ResolveFileOrAppBundleExecutable(candidate, helperBinaryName, stat); err == nil {
			return path, nil
		}
	}

	if path, err := lookPath(helperBinaryName); err == nil {
		return path, nil
	}

	return "", fmt.Errorf(
		"%s helper binary was not found (set %s, install cleanroom with runtime assets, build prebuilt binaries with `mise run build`, or install %s in PATH)",
		helperBinaryName,
		helperEnvVar,
		helperBinaryName,
	)
}
