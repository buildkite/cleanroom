package runtimeassets

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	libexecDirName     = "cleanroom"
	GuestAgentName     = "cleanroom-guest-agent"
	RootHelperName     = "cleanroom-root-helper"
	darwinHelperBinary = "cleanroom-darwin-vz"
)

type (
	LookPathFunc   func(string) (string, error)
	ExecutableFunc func() (string, error)
	GetwdFunc      func() (string, error)
	StatFunc       func(string) (os.FileInfo, error)
	ValidateFunc   func(string) (bool, error)
)

func DarwinHelperNames() []string {
	return []string{darwinHelperBinary + ".app", darwinHelperBinary}
}

func LinuxGuestAgentName(goarch string) string {
	return fmt.Sprintf("cleanroom-guest-agent-linux-%s", strings.TrimSpace(goarch))
}

func HostStageDirName(goos, goarch string) string {
	return fmt.Sprintf("%s-%s", strings.ToLower(strings.TrimSpace(goos)), strings.TrimSpace(goarch))
}

func InstalledLibexecCandidates(executable ExecutableFunc, names ...string) []string {
	rels := make([]string, 0, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		rels = append(rels, filepath.Join("..", "libexec", libexecDirName, trimmed))
	}
	return ExecutableRelativeCandidates(executable, rels...)
}

func InstalledSiblingCandidates(executable ExecutableFunc, names ...string) []string {
	return ExecutableRelativeCandidates(executable, names...)
}

func ExecutableRelativeCandidates(executable ExecutableFunc, relPaths ...string) []string {
	if executable == nil {
		return nil
	}
	self, err := executable()
	if err != nil {
		return nil
	}
	base, err := absoluteBaseDir(self)
	if err != nil {
		return nil
	}
	return baseRelativeCandidates(base, relPaths...)
}

func DistCandidates(getwd GetwdFunc, names ...string) []string {
	return distCandidates(getwd, names...)
}

func StagedDistCandidates(getwd GetwdFunc, goos, goarch string, relPaths ...string) []string {
	stageDir := HostStageDirName(goos, goarch)
	if strings.TrimSpace(stageDir) == "-" {
		return nil
	}
	prefixed := make([]string, 0, len(relPaths))
	for _, relPath := range relPaths {
		trimmed := strings.TrimSpace(relPath)
		if trimmed == "" {
			continue
		}
		prefixed = append(prefixed, filepath.Join(stageDir, trimmed))
	}
	return distCandidates(getwd, prefixed...)
}

func distCandidates(getwd GetwdFunc, relPaths ...string) []string {
	if getwd == nil {
		return nil
	}
	startDir, err := getwd()
	if err != nil {
		return nil
	}
	absStartDir, err := filepath.Abs(strings.TrimSpace(startDir))
	if err != nil || strings.TrimSpace(absStartDir) == "" {
		return nil
	}

	rels := make([]string, 0, len(relPaths))
	for _, relPath := range relPaths {
		trimmed := strings.TrimSpace(relPath)
		if trimmed == "" {
			continue
		}
		rels = append(rels, filepath.Join("dist", trimmed))
	}

	out := []string{}
	seen := map[string]struct{}{}
	for dir := absStartDir; ; dir = filepath.Dir(dir) {
		for _, rel := range rels {
			appendUniquePath(&out, seen, filepath.Join(dir, rel))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return out
}

func ResolveFirstCandidate(candidates []string, stat StatFunc, validate ValidateFunc) (string, error) {
	if stat == nil {
		stat = os.Stat
	}

	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" {
			continue
		}
		resolved := trimmed
		if !filepath.IsAbs(resolved) {
			abs, err := filepath.Abs(resolved)
			if err != nil {
				continue
			}
			resolved = abs
		}
		if _, ok := seen[resolved]; ok {
			continue
		}
		seen[resolved] = struct{}{}

		info, err := stat(resolved)
		if err != nil || info.IsDir() {
			continue
		}
		if validate != nil {
			ok, err := validate(resolved)
			if err != nil {
				return "", fmt.Errorf("validate %q: %w", resolved, err)
			}
			if !ok {
				continue
			}
		}
		return resolved, nil
	}

	return "", os.ErrNotExist
}

func ResolveLookPath(names []string, lookPath LookPathFunc, validate ValidateFunc) (string, error) {
	if lookPath == nil {
		return "", os.ErrNotExist
	}

	seen := map[string]struct{}{}
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}

		path, err := lookPath(trimmed)
		if err != nil {
			continue
		}
		if validate != nil {
			ok, err := validate(path)
			if err != nil {
				return "", fmt.Errorf("validate %q: %w", path, err)
			}
			if !ok {
				continue
			}
		}
		return path, nil
	}

	return "", os.ErrNotExist
}

func ResolveFileOrAppBundleExecutable(path, executableName string, stat StatFunc) (string, error) {
	if stat == nil {
		stat = os.Stat
	}

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
	if !info.IsDir() {
		return absPath, nil
	}
	if !strings.EqualFold(filepath.Ext(absPath), ".app") {
		return "", fmt.Errorf("%s is a directory", absPath)
	}

	bundleExecutablePath := filepath.Join(absPath, "Contents", "MacOS", strings.TrimSpace(executableName))
	info, err = stat(bundleExecutablePath)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", bundleExecutablePath)
	}
	return bundleExecutablePath, nil
}

func ResolveLinuxGuestAgentBinary(goarch string) (string, error) {
	return ResolveLinuxGuestAgentBinaryWith(
		goarch,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
}

func ResolveLinuxGuestAgentBinaryWith(
	goarch string,
	lookPath LookPathFunc,
	executable ExecutableFunc,
	getwd GetwdFunc,
	stat StatFunc,
	validate ValidateFunc,
) (string, error) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if executable == nil {
		executable = os.Executable
	}
	if getwd == nil {
		getwd = os.Getwd
	}
	if stat == nil {
		stat = os.Stat
	}
	if validate == nil {
		validate = func(path string) (bool, error) {
			return IsLinuxGuestAgentBinary(path, goarch)
		}
	}

	linuxName := LinuxGuestAgentName(goarch)
	names := []string{linuxName, GuestAgentName}
	candidates := append(InstalledLibexecCandidates(executable, names...), InstalledSiblingCandidates(executable, names...)...)
	candidates = append(candidates, StagedDistCandidates(getwd, runtime.GOOS, goarch, filepath.Join("libexec", libexecDirName, linuxName), filepath.Join("libexec", libexecDirName, GuestAgentName))...)
	candidates = append(candidates, DistCandidates(getwd, names...)...)

	path, err := ResolveFirstCandidate(candidates, stat, validate)
	if err == nil {
		return path, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	path, err = ResolveLookPath(names, lookPath, validate)
	if err == nil {
		return path, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	return "", fmt.Errorf(
		"linux guest-agent binary not found for architecture %s; make %s available via ../libexec/cleanroom, dist/, or PATH (for local development run `mise run build`)",
		goarch,
		linuxName,
	)
}

func IsLinuxGuestAgentBinary(path, goarch string) (bool, error) {
	f, err := elf.Open(path)
	if err != nil {
		return false, nil
	}
	defer f.Close()

	expectedMachine, ok := expectedGuestAgentELFMachine(goarch)
	if !ok {
		return false, fmt.Errorf("unsupported host architecture %q", goarch)
	}
	return f.FileHeader.Machine == expectedMachine, nil
}

func expectedGuestAgentELFMachine(goarch string) (elf.Machine, bool) {
	switch strings.TrimSpace(goarch) {
	case "arm64":
		return elf.EM_AARCH64, true
	case "amd64":
		return elf.EM_X86_64, true
	default:
		return 0, false
	}
}

func absoluteBaseDir(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", errors.New("path is empty")
	}
	absPath, err := filepath.Abs(trimmed)
	if err != nil {
		return "", err
	}
	return filepath.Dir(absPath), nil
}

func baseRelativeCandidates(base string, relPaths ...string) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, rel := range relPaths {
		trimmed := strings.TrimSpace(rel)
		if trimmed == "" {
			continue
		}
		appendUniquePath(&out, seen, filepath.Clean(filepath.Join(base, trimmed)))
	}
	return out
}

func appendUniquePath(out *[]string, seen map[string]struct{}, path string) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return
	}
	if _, ok := seen[trimmed]; ok {
		return
	}
	seen[trimmed] = struct{}{}
	*out = append(*out, trimmed)
}
