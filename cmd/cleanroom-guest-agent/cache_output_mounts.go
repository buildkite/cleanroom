//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/buildkite/cleanroom/internal/vsockexec"
	"golang.org/x/sys/unix"
)

type cacheOutputMountActionKind string

const (
	cacheOutputActionMkdir       cacheOutputMountActionKind = "mkdir"
	cacheOutputActionMount       cacheOutputMountActionKind = "mount"
	cacheOutputActionBind        cacheOutputMountActionKind = "bind"
	cacheOutputActionRestoreFile cacheOutputMountActionKind = "restore-file"
)

type cacheOutputMountAction struct {
	Kind            cacheOutputMountActionKind
	Source          string
	Target          string
	VolumeRoot      string
	VolumeSubpath   string
	FSType          string
	Flags           uintptr
	Data            string
	Mode            fs.FileMode
	RequireExisting bool
	RequireEmpty    bool
	Required        bool
}

var cacheOutputMountState struct {
	sync.Mutex
	signature string
}

func setupCacheOutputMountsOnce(mounts []vsockexec.CacheOutputMount) error {
	if len(mounts) == 0 {
		return nil
	}
	signature, err := cacheOutputMountSignature(mounts)
	if err != nil {
		return err
	}

	cacheOutputMountState.Lock()
	defer cacheOutputMountState.Unlock()
	if cacheOutputMountState.signature == signature {
		return nil
	}
	if cacheOutputMountState.signature != "" {
		return errors.New("cache output mount plan changed after setup")
	}
	if err := setupCacheOutputMounts(mounts); err != nil {
		return err
	}
	cacheOutputMountState.signature = signature
	return nil
}

func cacheOutputMountSignature(mounts []vsockexec.CacheOutputMount) (string, error) {
	data, err := json.Marshal(mounts)
	if err != nil {
		return "", fmt.Errorf("encode cache output mount plan: %w", err)
	}
	return string(data), nil
}

func setupCacheOutputMounts(mounts []vsockexec.CacheOutputMount) error {
	actions, err := cacheOutputMountActions(mounts)
	if err != nil {
		return err
	}
	for _, action := range actions {
		if err := executeCacheOutputMountAction(action); err != nil {
			return err
		}
	}
	return nil
}

func cacheOutputMountActions(mounts []vsockexec.CacheOutputMount) ([]cacheOutputMountAction, error) {
	actions := make([]cacheOutputMountAction, 0, len(mounts)*3)
	for i, mount := range mounts {
		devicePath, err := cleanAbsoluteCacheOutputPath("device path", mount.DevicePath)
		if err != nil {
			return nil, fmt.Errorf("cache output mount %d: %w", i, err)
		}
		mountPath, err := cleanAbsoluteCacheOutputPath("mount path", mount.MountPath)
		if err != nil {
			return nil, fmt.Errorf("cache output mount %d: %w", i, err)
		}
		if len(mount.DirMappings) == 0 && len(mount.FileMappings) == 0 {
			return nil, fmt.Errorf("cache output mount %d: missing output mappings", i)
		}

		actions = append(actions,
			cacheOutputMountAction{Kind: cacheOutputActionMkdir, Target: mountPath, Mode: 0o755},
			cacheOutputMountAction{Kind: cacheOutputActionMount, Source: devicePath, Target: mountPath, FSType: "ext4"},
		)

		for j, mapping := range mount.DirMappings {
			guestPath, sourcePath, cleanSubpath, err := cacheOutputMappingPaths(mountPath, mapping.GuestPath, mapping.Subpath)
			if err != nil {
				return nil, fmt.Errorf("cache output mount %d dir mapping %d: %w", i, j, err)
			}
			actions = append(actions,
				cacheOutputMountAction{Kind: cacheOutputActionMkdir, Source: sourcePath, VolumeRoot: mountPath, VolumeSubpath: cleanSubpath, Mode: 0o755, RequireExisting: mount.SourcePresent},
				cacheOutputMountAction{Kind: cacheOutputActionMkdir, Target: guestPath, Mode: 0o755, RequireEmpty: true},
				cacheOutputMountAction{Kind: cacheOutputActionBind, Source: sourcePath, Target: guestPath, VolumeRoot: mountPath, VolumeSubpath: cleanSubpath, Flags: unix.MS_BIND},
			)
		}

		for j, mapping := range mount.FileMappings {
			guestPath, sourcePath, cleanSubpath, err := cacheOutputMappingPaths(mountPath, mapping.GuestPath, mapping.Subpath)
			if err != nil {
				return nil, fmt.Errorf("cache output mount %d file mapping %d: %w", i, j, err)
			}
			actions = append(actions,
				cacheOutputMountAction{Kind: cacheOutputActionMkdir, Target: filepath.Dir(guestPath), Mode: 0o755},
				cacheOutputMountAction{
					Kind:          cacheOutputActionRestoreFile,
					Source:        sourcePath,
					Target:        guestPath,
					VolumeRoot:    mountPath,
					VolumeSubpath: cleanSubpath,
					Mode:          fs.FileMode(mapping.Mode).Perm(),
					Required:      mount.SourcePresent,
				},
			)
		}
	}
	return actions, nil
}

func cacheOutputMappingPaths(mountPath, guestPath, subpath string) (string, string, string, error) {
	cleanGuestPath, err := cleanAbsoluteCacheOutputPath("guest path", guestPath)
	if err != nil {
		return "", "", "", err
	}
	cleanSubpath, err := cleanCacheOutputSubpath(subpath)
	if err != nil {
		return "", "", "", err
	}
	return cleanGuestPath, filepath.Join(mountPath, cleanSubpath), cleanSubpath, nil
}

func cleanAbsoluteCacheOutputPath(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("missing %s", name)
	}
	cleaned := filepath.Clean(value)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("%s %q is not absolute", name, value)
	}
	return cleaned, nil
}

func cleanCacheOutputSubpath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("missing volume subpath")
	}
	cleaned := filepath.Clean(value)
	if filepath.IsAbs(cleaned) || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("volume subpath %q escapes output volume", value)
	}
	return cleaned, nil
}

func executeCacheOutputMountAction(action cacheOutputMountAction) error {
	switch action.Kind {
	case cacheOutputActionMkdir:
		if action.VolumeRoot != "" {
			_, err := ensureCacheOutputVolumeDir(action.VolumeRoot, action.VolumeSubpath, action.Mode, action.RequireExisting)
			return err
		}
		return ensureCacheOutputDir(action.Target, action.Mode, action.RequireExisting, action.RequireEmpty)
	case cacheOutputActionMount:
		if err := unix.Mount(action.Source, action.Target, action.FSType, action.Flags, action.Data); err != nil && err != unix.EBUSY {
			return fmt.Errorf("mount cache output volume %s at %s: %w", action.Source, action.Target, err)
		}
	case cacheOutputActionBind:
		sourcePath := action.Source
		if action.VolumeRoot != "" {
			resolved, err := ensureCacheOutputVolumeDir(action.VolumeRoot, action.VolumeSubpath, 0, true)
			if err != nil {
				return err
			}
			sourcePath = resolved
		}
		if err := unix.Mount(sourcePath, action.Target, "", action.Flags, ""); err != nil && err != unix.EBUSY {
			return fmt.Errorf("bind cache output path %s at %s: %w", sourcePath, action.Target, err)
		}
	case cacheOutputActionRestoreFile:
		if action.VolumeRoot != "" {
			source, sourcePath, err := openCacheOutputVolumeFile(action.VolumeRoot, action.VolumeSubpath, action.Required)
			if err != nil {
				return err
			}
			if source == nil {
				return nil
			}
			defer source.Close()
			return restoreCacheOutputFileFrom(source, sourcePath, action.Target, action.Mode)
		}
		if err := restoreCacheOutputFile(action.Source, action.Target, action.Mode, action.Required); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown cache output mount action %q", action.Kind)
	}
	return nil
}

func ensureCacheOutputVolumeDir(root, subpath string, mode fs.FileMode, requireExisting bool) (string, error) {
	root, err := cleanAbsoluteCacheOutputPath("volume root", root)
	if err != nil {
		return "", err
	}
	cleanSubpath, err := cleanCacheOutputSubpath(subpath)
	if err != nil {
		return "", err
	}
	if mode == 0 {
		mode = 0o755
	}
	if err := requireCacheOutputDirNoSymlink(root); err != nil {
		return "", err
	}
	current := root
	for _, part := range strings.Split(cleanSubpath, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("cache output path %s is a symlink", current)
			}
			if !info.IsDir() {
				return "", fmt.Errorf("cache output path %s is not a directory", current)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat cache output path %s: %w", current, err)
		}
		if requireExisting {
			return "", fmt.Errorf("cache output directory %s does not exist", current)
		}
		if err := os.Mkdir(current, mode.Perm()); err != nil {
			if errors.Is(err, os.ErrExist) {
				info, statErr := os.Lstat(current)
				if statErr != nil {
					return "", fmt.Errorf("stat cache output path %s: %w", current, statErr)
				}
				if info.Mode()&os.ModeSymlink != 0 {
					return "", fmt.Errorf("cache output path %s is a symlink", current)
				}
				if !info.IsDir() {
					return "", fmt.Errorf("cache output path %s is not a directory", current)
				}
				continue
			}
			return "", fmt.Errorf("create cache output directory %s: %w", current, err)
		}
	}
	return current, nil
}

func openCacheOutputVolumeFile(root, subpath string, required bool) (*os.File, string, error) {
	root, err := cleanAbsoluteCacheOutputPath("volume root", root)
	if err != nil {
		return nil, "", err
	}
	cleanSubpath, err := cleanCacheOutputSubpath(subpath)
	if err != nil {
		return nil, "", err
	}
	if err := requireCacheOutputDirNoSymlink(root); err != nil {
		return nil, "", err
	}

	parts := strings.Split(cleanSubpath, string(filepath.Separator))
	parent := root
	for _, part := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, part)
		info, err := os.Lstat(parent)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && !required {
				return nil, filepath.Join(root, cleanSubpath), nil
			}
			return nil, "", fmt.Errorf("stat cache output path %s: %w", parent, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, "", fmt.Errorf("cache output path %s is a symlink", parent)
		}
		if !info.IsDir() {
			return nil, "", fmt.Errorf("cache output path %s is not a directory", parent)
		}
	}

	sourcePath := filepath.Join(parent, parts[len(parts)-1])
	info, err := os.Lstat(sourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !required {
			return nil, sourcePath, nil
		}
		return nil, "", fmt.Errorf("stat cache output file %s: %w", sourcePath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, "", fmt.Errorf("cache output file %s is a symlink", sourcePath)
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("cache output file %s is not a regular file", sourcePath)
	}
	fd, err := unix.Open(sourcePath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open cache output file %s: %w", sourcePath, err)
	}
	return os.NewFile(uintptr(fd), sourcePath), sourcePath, nil
}

func requireCacheOutputDirNoSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat cache output path %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("cache output path %s is a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("cache output path %s is not a directory", path)
	}
	return nil
}

func ensureCacheOutputDir(path string, mode fs.FileMode, requireExisting, requireEmpty bool) error {
	if mode == 0 {
		mode = 0o755
	}
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("cache output path %s is a symlink", path)
		}
		if !info.IsDir() {
			return fmt.Errorf("cache output path %s is not a directory", path)
		}
		if requireEmpty {
			empty, err := isEmptyCacheOutputDir(path)
			if err != nil {
				return err
			}
			if !empty {
				return fmt.Errorf("cache output directory %s is not empty", path)
			}
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat cache output path %s: %w", path, err)
	}
	if requireExisting {
		return fmt.Errorf("cache output directory %s does not exist", path)
	}
	if err := os.MkdirAll(path, mode.Perm()); err != nil {
		return fmt.Errorf("create cache output directory %s: %w", path, err)
	}
	return nil
}

func isEmptyCacheOutputDir(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, fmt.Errorf("read cache output directory %s: %w", path, err)
	}
	return len(entries) == 0, nil
}

func restoreCacheOutputFile(sourcePath, targetPath string, mode fs.FileMode, required bool) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !required {
			return nil
		}
		return fmt.Errorf("open cache output file %s: %w", sourcePath, err)
	}
	defer source.Close()
	return restoreCacheOutputFileFrom(source, sourcePath, targetPath, mode)
}

func restoreCacheOutputFileFrom(source *os.File, sourcePath, targetPath string, mode fs.FileMode) error {
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stat cache output file %s: %w", sourcePath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cache output file %s is not a regular file", sourcePath)
	}

	if mode == 0 {
		mode = info.Mode().Perm()
	}
	if mode == 0 {
		mode = 0o644
	}

	targetDir := filepath.Dir(targetPath)
	if err := ensureCacheOutputDir(targetDir, 0o755, true, false); err != nil {
		return err
	}
	if targetInfo, err := os.Lstat(targetPath); err == nil {
		if targetInfo.IsDir() {
			return fmt.Errorf("cache output file target %s is a directory", targetPath)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat cache output file target %s: %w", targetPath, err)
	}

	target, err := os.CreateTemp(targetDir, "."+filepath.Base(targetPath)+".cleanroom-cache-*")
	if err != nil {
		return fmt.Errorf("create temporary cache output file target for %s: %w", targetPath, err)
	}
	tmpPath := target.Name()
	_, copyErr := io.Copy(target, source)
	chmodErr := target.Chmod(mode.Perm())
	syncErr := target.Sync()
	closeErr := target.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("restore cache output file %s to %s: %w", sourcePath, targetPath, copyErr)
	}
	if chmodErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temporary cache output file target %s: %w", tmpPath, chmodErr)
	}
	if syncErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync temporary cache output file target %s: %w", tmpPath, syncErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temporary cache output file target %s: %w", tmpPath, closeErr)
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace cache output file target %s: %w", targetPath, err)
	}
	return nil
}

func captureCacheOutputFiles(captures []vsockexec.CacheOutputFileCapture, sourceRoot string) error {
	for i, capture := range captures {
		if err := captureCacheOutputFile(capture, sourceRoot); err != nil {
			return fmt.Errorf("capture cache output file %d: %w", i, err)
		}
	}
	return nil
}

func captureCacheOutputFile(capture vsockexec.CacheOutputFileCapture, sourceRoot string) error {
	guestPath, err := cleanAbsoluteCacheOutputPath("guest path", capture.GuestPath)
	if err != nil {
		return err
	}
	mountPath, err := cleanAbsoluteCacheOutputPath("mount path", capture.MountPath)
	if err != nil {
		return err
	}
	subpath, err := cleanCacheOutputSubpath(capture.Subpath)
	if err != nil {
		return err
	}
	if err := requireCacheOutputDirNoSymlink(mountPath); err != nil {
		return err
	}

	sourcePath, err := cacheOutputCaptureSourcePath(guestPath, sourceRoot)
	if err != nil {
		return err
	}
	source, err := openCacheOutputCaptureSource(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()

	targetDirSubpath := filepath.Dir(subpath)
	targetDir := mountPath
	if targetDirSubpath != "." {
		var err error
		targetDir, err = ensureCacheOutputVolumeDir(mountPath, targetDirSubpath, 0o755, false)
		if err != nil {
			return err
		}
	}
	if err := copyCacheOutputFile(source, sourcePath, filepath.Join(targetDir, filepath.Base(subpath)), fs.FileMode(capture.Mode).Perm()); err != nil {
		return err
	}
	if strings.TrimSpace(sourceRoot) == "" {
		return nil
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind cache output file source %s: %w", sourcePath, err)
	}
	targetPath, err := cacheOutputMaterializationTarget(string(filepath.Separator), guestPath)
	if err != nil {
		return err
	}
	return copyCacheOutputFileWithParentMode(source, sourcePath, targetPath, fs.FileMode(capture.Mode).Perm(), false)
}

func cacheOutputCaptureSourcePath(guestPath, sourceRoot string) (string, error) {
	sourceRoot = strings.TrimSpace(sourceRoot)
	if sourceRoot == "" {
		return guestPath, nil
	}
	sourceRoot, err := cleanAbsoluteCacheOutputPath("source root", sourceRoot)
	if err != nil {
		return "", err
	}
	return resolveOverlayCaptureGuestTarget(sourceRoot, guestPath, 0)
}

func cacheOutputMaterializationTarget(root, guestPath string) (string, error) {
	return resolveOverlayCaptureGuestTarget(root, guestPath, 0)
}

func openCacheOutputCaptureSource(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat cache output file source %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("cache output file source %s is a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("cache output file source %s is not a regular file", path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open cache output file source %s: %w", path, err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

func copyCacheOutputFile(source *os.File, sourcePath, targetPath string, mode fs.FileMode) error {
	return copyCacheOutputFileWithParentMode(source, sourcePath, targetPath, mode, true)
}

func copyCacheOutputFileWithParentMode(source *os.File, sourcePath, targetPath string, mode fs.FileMode, requireParent bool) error {
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stat cache output file source %s: %w", sourcePath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cache output file source %s is not a regular file", sourcePath)
	}
	if mode == 0 {
		mode = info.Mode().Perm()
	}
	if mode == 0 {
		mode = 0o644
	}

	targetDir := filepath.Dir(targetPath)
	if err := ensureCacheOutputDir(targetDir, 0o755, requireParent, false); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(targetDir, "."+filepath.Base(targetPath)+".cleanroom-cache-capture-*")
	if err != nil {
		return fmt.Errorf("create temporary cache output file target for %s: %w", targetPath, err)
	}
	tmpPath := tmp.Name()
	_, copyErr := io.Copy(tmp, source)
	chmodErr := tmp.Chmod(mode.Perm())
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("copy cache output file %s to %s: %w", sourcePath, targetPath, copyErr)
	}
	if chmodErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temporary cache output file target %s: %w", tmpPath, chmodErr)
	}
	if syncErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync temporary cache output file target %s: %w", tmpPath, syncErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temporary cache output file target %s: %w", tmpPath, closeErr)
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace cache output file target %s: %w", targetPath, err)
	}
	return nil
}
