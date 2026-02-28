package hosttools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	e2fsPrefixesOnce   sync.Once
	e2fsCachedPrefixes []string
)

const darwinE2FSProgsInstallHint = "install it with `brew install e2fsprogs`"

// ResolveE2FSProgsBinary resolves a requested e2fsprogs binary by checking:
// 1. PATH
// 2. Known Homebrew e2fsprogs locations on macOS.
func ResolveE2FSProgsBinary(binary string) (string, error) {
	return resolveBinary(binary, exec.LookPath, os.Stat, candidateBinaryPaths(binary, e2fsProgsPrefixes()))
}

func resolveBinary(
	binary string,
	lookPath func(string) (string, error),
	stat func(string) (os.FileInfo, error),
	candidates []string,
) (string, error) {
	trimmed := strings.TrimSpace(binary)
	if trimmed == "" {
		return "", fmt.Errorf("binary name is required")
	}

	if path, err := lookPath(trimmed); err == nil {
		return path, nil
	}

	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		info, err := stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		return candidate, nil
	}

	if len(candidates) == 0 {
		msg := fmt.Sprintf("%s not found in PATH", trimmed)
		if hint := e2fsProgsInstallHint(); hint != "" {
			msg += "; " + hint
		}
		return "", errors.New(msg)
	}
	msg := fmt.Sprintf("%s not found in PATH or known Homebrew locations", trimmed)
	if hint := e2fsProgsInstallHint(); hint != "" {
		msg += "; " + hint
	}
	return "", errors.New(msg)
}

func candidateBinaryPaths(binary string, prefixes []string) []string {
	trimmedBinary := strings.TrimSpace(binary)
	if trimmedBinary == "" {
		return nil
	}

	seen := map[string]struct{}{}
	out := make([]string, 0, len(prefixes)*2)
	appendCandidate := func(path string) {
		if strings.TrimSpace(path) == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}

	for _, prefix := range prefixes {
		trimmedPrefix := strings.TrimSpace(prefix)
		if trimmedPrefix == "" {
			continue
		}
		appendCandidate(filepath.Join(trimmedPrefix, "sbin", trimmedBinary))
		appendCandidate(filepath.Join(trimmedPrefix, "bin", trimmedBinary))
	}
	return out
}

func e2fsProgsPrefixes() []string {
	if runtime.GOOS != "darwin" {
		return nil
	}

	e2fsPrefixesOnce.Do(func() {
		prefixes := make([]string, 0, 3)
		seen := map[string]struct{}{}
		appendPrefix := func(value string) {
			trimmed := strings.TrimSpace(value)
			if trimmed == "" {
				return
			}
			if _, ok := seen[trimmed]; ok {
				return
			}
			seen[trimmed] = struct{}{}
			prefixes = append(prefixes, trimmed)
		}

		if brewPath, err := exec.LookPath("brew"); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, brewPath, "--prefix", "e2fsprogs").Output()
			if err == nil {
				appendPrefix(string(out))
			}
		}
		appendPrefix("/opt/homebrew/opt/e2fsprogs")
		appendPrefix("/usr/local/opt/e2fsprogs")

		e2fsCachedPrefixes = prefixes
	})

	return append([]string(nil), e2fsCachedPrefixes...)
}

func e2fsProgsInstallHint() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	return darwinE2FSProgsInstallHint
}
