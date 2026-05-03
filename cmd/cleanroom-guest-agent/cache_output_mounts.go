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
			guestPath, sourcePath, err := cacheOutputMappingPaths(mountPath, mapping.GuestPath, mapping.Subpath)
			if err != nil {
				return nil, fmt.Errorf("cache output mount %d dir mapping %d: %w", i, j, err)
			}
			actions = append(actions,
				cacheOutputMountAction{Kind: cacheOutputActionMkdir, Target: sourcePath, Mode: 0o755, RequireExisting: mount.SourcePresent},
				cacheOutputMountAction{Kind: cacheOutputActionMkdir, Target: guestPath, Mode: 0o755, RequireEmpty: true},
				cacheOutputMountAction{Kind: cacheOutputActionBind, Source: sourcePath, Target: guestPath, Flags: unix.MS_BIND},
			)
		}

		for j, mapping := range mount.FileMappings {
			guestPath, sourcePath, err := cacheOutputMappingPaths(mountPath, mapping.GuestPath, mapping.Subpath)
			if err != nil {
				return nil, fmt.Errorf("cache output mount %d file mapping %d: %w", i, j, err)
			}
			mode := fs.FileMode(mapping.Mode).Perm()
			if mode == 0 {
				mode = 0o644
			}
			actions = append(actions,
				cacheOutputMountAction{Kind: cacheOutputActionMkdir, Target: filepath.Dir(guestPath), Mode: 0o755},
				cacheOutputMountAction{
					Kind:     cacheOutputActionRestoreFile,
					Source:   sourcePath,
					Target:   guestPath,
					Mode:     mode,
					Required: mount.SourcePresent,
				},
			)
		}
	}
	return actions, nil
}

func cacheOutputMappingPaths(mountPath, guestPath, subpath string) (string, string, error) {
	cleanGuestPath, err := cleanAbsoluteCacheOutputPath("guest path", guestPath)
	if err != nil {
		return "", "", err
	}
	cleanSubpath, err := cleanCacheOutputSubpath(subpath)
	if err != nil {
		return "", "", err
	}
	return cleanGuestPath, filepath.Join(mountPath, cleanSubpath), nil
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
		if err := ensureCacheOutputDir(action.Target, action.Mode, action.RequireExisting, action.RequireEmpty); err != nil {
			return err
		}
	case cacheOutputActionMount:
		if err := unix.Mount(action.Source, action.Target, action.FSType, action.Flags, action.Data); err != nil && err != unix.EBUSY {
			return fmt.Errorf("mount cache output volume %s at %s: %w", action.Source, action.Target, err)
		}
	case cacheOutputActionBind:
		if err := unix.Mount(action.Source, action.Target, "", action.Flags, ""); err != nil && err != unix.EBUSY {
			return fmt.Errorf("bind cache output path %s at %s: %w", action.Source, action.Target, err)
		}
	case cacheOutputActionRestoreFile:
		if err := restoreCacheOutputFile(action.Source, action.Target, action.Mode, action.Required); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown cache output mount action %q", action.Kind)
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

	if targetInfo, err := os.Lstat(targetPath); err == nil {
		if targetInfo.IsDir() {
			return fmt.Errorf("cache output file target %s is a directory", targetPath)
		}
		if err := os.Remove(targetPath); err != nil {
			return fmt.Errorf("replace cache output file target %s: %w", targetPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat cache output file target %s: %w", targetPath, err)
	}

	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
	if err != nil {
		return fmt.Errorf("create cache output file target %s: %w", targetPath, err)
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil {
		_ = os.Remove(targetPath)
		return fmt.Errorf("restore cache output file %s to %s: %w", sourcePath, targetPath, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(targetPath)
		return fmt.Errorf("close cache output file target %s: %w", targetPath, closeErr)
	}
	if err := os.Chmod(targetPath, mode.Perm()); err != nil {
		return fmt.Errorf("chmod cache output file target %s: %w", targetPath, err)
	}
	return nil
}
