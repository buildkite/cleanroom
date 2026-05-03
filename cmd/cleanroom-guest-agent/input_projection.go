//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/buildkite/cleanroom/internal/vsockexec"
	"golang.org/x/sys/unix"
)

var inputProjectionRoot = "/run/cleanroom/input-projections"

func setupInputProjection(req vsockexec.ExecRequest) (vsockexec.ExecRequest, func(), error) {
	projection := req.InputProjection
	if projection == nil {
		return req, func() {}, nil
	}

	sourceRoot, err := cleanInputProjectionRoot("source root", projection.SourceRoot)
	if err != nil {
		return req, nil, err
	}
	targetRoot, err := cleanInputProjectionTargetRoot(projection.TargetRoot)
	if err != nil {
		return req, nil, err
	}
	if len(projection.Files) == 0 {
		return req, nil, errors.New("input projection has no files")
	}

	if err := materializeInputProjection(sourceRoot, targetRoot, projection.Files); err != nil {
		return req, nil, err
	}

	cleanup := func() {}
	if projection.MountSourceReadOnly {
		cleanup, err = bindInputProjectionOverSource(sourceRoot, targetRoot)
		if err != nil {
			return req, nil, err
		}
		if strings.TrimSpace(req.Dir) == "" {
			req.Dir = sourceRoot
		}
	} else if strings.TrimSpace(req.Dir) == "" {
		req.Dir = targetRoot
	}
	return req, cleanup, nil
}

func cleanInputProjectionRoot(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("missing input projection %s", name)
	}
	cleaned := filepath.Clean(value)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("input projection %s %q is not absolute", name, value)
	}
	if cleaned == "/" {
		return "", fmt.Errorf("input projection %s must not be /", name)
	}
	return cleaned, nil
}

func cleanInputProjectionTargetRoot(value string) (string, error) {
	root, err := cleanInputProjectionRoot("target root", value)
	if err != nil {
		return "", err
	}
	if root == inputProjectionRoot || !strings.HasPrefix(root, inputProjectionRoot+"/") {
		return "", fmt.Errorf("input projection target root %q must be under %s", value, inputProjectionRoot)
	}
	return root, nil
}

func materializeInputProjection(sourceRoot, targetRoot string, inputs []string) error {
	if err := os.RemoveAll(targetRoot); err != nil {
		return fmt.Errorf("clear input projection target %s: %w", targetRoot, err)
	}
	if err := os.MkdirAll(targetRoot, 0o755); err != nil {
		return fmt.Errorf("create input projection target %s: %w", targetRoot, err)
	}

	files, err := expandInputProjectionFiles(sourceRoot, inputs)
	if err != nil {
		return err
	}
	for _, rel := range files {
		if err := copyInputProjectionFile(sourceRoot, targetRoot, rel); err != nil {
			return err
		}
	}
	return nil
}

func expandInputProjectionFiles(sourceRoot string, inputs []string) ([]string, error) {
	seen := make(map[string]struct{}, len(inputs))
	var out []string
	for _, input := range inputs {
		normalized, err := cleanInputProjectionRelativePath(input)
		if err != nil {
			return nil, err
		}
		matches, err := inputProjectionMatches(sourceRoot, normalized)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			if _, ok := seen[match]; ok {
				continue
			}
			seen[match] = struct{}{}
			out = append(out, match)
		}
	}
	sort.Strings(out)
	return out, nil
}

func inputProjectionMatches(sourceRoot, input string) ([]string, error) {
	if !strings.ContainsAny(input, "*?[") {
		if _, err := os.Lstat(filepath.Join(sourceRoot, filepath.FromSlash(input))); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("input projection file %q does not exist", input)
			}
			return nil, fmt.Errorf("stat input projection file %q: %w", input, err)
		}
		return []string{input}, nil
	}

	pattern := filepath.Join(sourceRoot, filepath.FromSlash(input))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid input projection glob %q: %w", input, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("input projection glob %q matched no files", input)
	}
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		rel, err := filepath.Rel(sourceRoot, match)
		if err != nil {
			return nil, fmt.Errorf("resolve input projection match %q: %w", match, err)
		}
		normalized, err := cleanInputProjectionRelativePath(filepath.ToSlash(rel))
		if err != nil {
			return nil, err
		}
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out, nil
}

func cleanInputProjectionRelativePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("input projection path cannot be empty")
	}
	if strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("input projection path %q must be relative", value)
	}
	normalized := path.Clean(strings.ReplaceAll(value, "\\", "/"))
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", fmt.Errorf("input projection path %q must stay within the source root", value)
	}
	return normalized, nil
}

func copyInputProjectionFile(sourceRoot, targetRoot, rel string) error {
	sourcePath := filepath.Join(sourceRoot, filepath.FromSlash(rel))
	targetPath := filepath.Join(targetRoot, filepath.FromSlash(rel))

	info, err := os.Lstat(sourcePath)
	if err != nil {
		return fmt.Errorf("stat input projection file %s: %w", sourcePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("input projection file %s is a symlink", sourcePath)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("input projection file %s is not a regular file", sourcePath)
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("create input projection directory %s: %w", filepath.Dir(targetPath), err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open input projection file %s: %w", sourcePath, err)
	}
	defer source.Close()

	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create input projection file %s: %w", targetPath, err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return fmt.Errorf("copy input projection file %s: %w", rel, err)
	}
	if err := target.Chmod(info.Mode().Perm()); err != nil {
		_ = target.Close()
		return fmt.Errorf("chmod input projection file %s: %w", targetPath, err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close input projection file %s: %w", targetPath, err)
	}
	return nil
}

func bindInputProjectionOverSource(sourceRoot, targetRoot string) (func(), error) {
	runtime.LockOSThread()
	oldNS, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("open current mount namespace: %w", err)
	}
	cleanup := func() {
		if err := unix.Setns(int(oldNS.Fd()), unix.CLONE_NEWNS); err != nil {
			fmt.Fprintf(os.Stderr, "restore input projection mount namespace: %v\n", err)
			_ = oldNS.Close()
			return
		}
		_ = oldNS.Close()
		runtime.UnlockOSThread()
	}

	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		cleanup()
		return nil, fmt.Errorf("create input projection mount namespace: %w", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		cleanup()
		return nil, fmt.Errorf("make input projection mount namespace private: %w", err)
	}
	if err := unix.Mount(targetRoot, sourceRoot, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		cleanup()
		return nil, fmt.Errorf("bind input projection %s over %s: %w", targetRoot, sourceRoot, err)
	}
	if err := unix.Mount("", sourceRoot, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_REC, ""); err != nil {
		cleanup()
		return nil, fmt.Errorf("remount input projection %s read-only: %w", sourceRoot, err)
	}
	return cleanup, nil
}
