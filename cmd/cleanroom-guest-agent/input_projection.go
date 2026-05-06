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
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/buildkite/cleanroom/internal/vsockexec"
	"golang.org/x/sys/unix"
)

var inputProjectionRoot = "/run/cleanroom/input-projections"
var inputProjectionTimestamp = time.Unix(0, 0).UTC()

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
	if err := normalizeInputProjectionTimes(targetRoot); err != nil {
		return err
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

	if !doublestar.ValidatePattern(input) {
		return nil, fmt.Errorf("invalid input projection glob %q", input)
	}

	sourceFS := os.DirFS(sourceRoot)
	matches, err := doublestar.Glob(sourceFS, input, doublestar.WithFilesOnly())
	if err != nil {
		return nil, fmt.Errorf("expand input projection glob %q: %w", input, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("input projection glob %q matched no files", input)
	}
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		normalized, err := cleanInputProjectionRelativePath(match)
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
	targetPath := filepath.Join(targetRoot, filepath.FromSlash(rel))

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("create input projection directory %s: %w", filepath.Dir(targetPath), err)
	}
	source, mode, err := openInputProjectionFile(sourceRoot, rel)
	if err != nil {
		return err
	}
	defer source.Close()

	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return fmt.Errorf("create input projection file %s: %w", targetPath, err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return fmt.Errorf("copy input projection file %s: %w", rel, err)
	}
	if err := target.Chmod(mode.Perm()); err != nil {
		_ = target.Close()
		return fmt.Errorf("chmod input projection file %s: %w", targetPath, err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close input projection file %s: %w", targetPath, err)
	}
	return nil
}

func openInputProjectionFile(sourceRoot, rel string) (*os.File, os.FileMode, error) {
	dirFD, err := unix.Open(sourceRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if inputProjectionPathIsSymlink(sourceRoot) {
			return nil, 0, fmt.Errorf("input projection source root %s is a symlink", sourceRoot)
		}
		return nil, 0, fmt.Errorf("open input projection source root %s: %w", sourceRoot, err)
	}
	defer func() {
		if dirFD >= 0 {
			_ = unix.Close(dirFD)
		}
	}()

	components := strings.Split(rel, "/")
	currentPath := sourceRoot
	for i, component := range components {
		currentPath = filepath.Join(currentPath, component)
		if i < len(components)-1 {
			nextFD, err := unix.Openat(dirFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				if inputProjectionComponentIsSymlink(dirFD, component) {
					return nil, 0, fmt.Errorf("input projection path %s contains a symlink component", currentPath)
				}
				return nil, 0, fmt.Errorf("open input projection directory %s: %w", currentPath, err)
			}
			_ = unix.Close(dirFD)
			dirFD = nextFD
			continue
		}

		fileFD, err := unix.Openat(dirFD, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			if inputProjectionComponentIsSymlink(dirFD, component) {
				return nil, 0, fmt.Errorf("input projection file %s is a symlink", currentPath)
			}
			return nil, 0, fmt.Errorf("open input projection file %s: %w", currentPath, err)
		}

		var stat unix.Stat_t
		if err := unix.Fstat(fileFD, &stat); err != nil {
			_ = unix.Close(fileFD)
			return nil, 0, fmt.Errorf("stat input projection file %s: %w", currentPath, err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG {
			_ = unix.Close(fileFD)
			return nil, 0, fmt.Errorf("input projection file %s is not a regular file", currentPath)
		}
		return os.NewFile(uintptr(fileFD), currentPath), os.FileMode(stat.Mode & 0o777), nil
	}

	return nil, 0, fmt.Errorf("input projection path %q cannot be empty", rel)
}

func inputProjectionPathIsSymlink(path string) bool {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return false
	}
	return stat.Mode&unix.S_IFMT == unix.S_IFLNK
}

func inputProjectionComponentIsSymlink(dirFD int, name string) bool {
	var stat unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	return stat.Mode&unix.S_IFMT == unix.S_IFLNK
}

func normalizeInputProjectionTimes(targetRoot string) error {
	return filepath.WalkDir(targetRoot, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk input projection path %s: %w", path, err)
		}
		if err := os.Chtimes(path, inputProjectionTimestamp, inputProjectionTimestamp); err != nil {
			return fmt.Errorf("set deterministic input projection timestamp %s: %w", path, err)
		}
		return nil
	})
}

func bindInputProjectionOverSource(sourceRoot, targetRoot string) (func(), error) {
	runtime.LockOSThread()
	oldNS, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("open current mount namespace: %w", err)
	}
	cleanup := func() {
		defer runtime.UnlockOSThread()
		defer oldNS.Close()
		if err := unix.Setns(int(oldNS.Fd()), unix.CLONE_NEWNS); err != nil {
			fmt.Fprintf(os.Stderr, "restore input projection mount namespace: %v\n", err)
			return
		}
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
	if err := hideInputProjectionBackingRoot(); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}

func hideInputProjectionBackingRoot() error {
	root, err := cleanInputProjectionRoot("root", inputProjectionRoot)
	if err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", root, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC|unix.MS_RDONLY, "size=4k,mode=0555"); err != nil {
		return fmt.Errorf("hide input projection backing root %s: %w", root, err)
	}
	return nil
}
