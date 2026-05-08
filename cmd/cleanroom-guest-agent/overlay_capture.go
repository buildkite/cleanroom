//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/buildkite/cleanroom/internal/overlaycapture"
	"github.com/buildkite/cleanroom/internal/vsockexec"
	"golang.org/x/sys/unix"
)

type overlayCaptureLayout struct {
	BaseDir    string
	UpperDir   string
	WorkDir    string
	MergedRoot string
}

type overlayCaptureRuntimeSocket struct {
	SourcePath string
	GuestPath  string
}

type overlayCaptureFileBinder func(source, target string, mounted *[]string) error

var overlayCaptureRoot = "/run/cleanroom/overlay-captures"
var overlayCaptureVirtualMounts = []string{"/dev", "/proc", "/sys"}
var overlayCaptureScratchMounts = []struct {
	Path string
	Data string
}{
	{Path: "/tmp", Data: "mode=1777"},
	{Path: "/var/tmp", Data: "mode=1777"},
	{Path: "/run", Data: "mode=0755"},
}
var overlayCaptureRuntimeSockets = []overlayCaptureRuntimeSocket{
	{SourcePath: "/run/docker.sock", GuestPath: "/run/docker.sock"},
	{SourcePath: "/var/run/docker.sock", GuestPath: "/var/run/docker.sock"},
}

func setupOverlayCapture(req vsockexec.ExecRequest) (string, func(), error) {
	capture := req.OverlayCapture
	if capture == nil {
		return "", func() {}, nil
	}

	layout, err := newOverlayCaptureLayout(capture.UpperDir)
	if err != nil {
		return "", nil, err
	}
	namespaceCleanup, err := enterOverlayCaptureMountNamespace()
	if err != nil {
		return "", nil, err
	}
	mounted := []string{}
	cleanup := func() {
		for i := len(mounted) - 1; i >= 0; i-- {
			_ = unix.Unmount(mounted[i], unix.MNT_DETACH)
		}
		_ = os.RemoveAll(layout.WorkDir)
		_ = os.RemoveAll(layout.MergedRoot)
		namespaceCleanup()
	}

	if err := prepareOverlayCaptureLayout(layout); err != nil {
		cleanup()
		return "", nil, err
	}
	overlayData := fmt.Sprintf("lowerdir=/,upperdir=%s,workdir=%s", layout.UpperDir, layout.WorkDir)
	if err := unix.Mount("cleanroom-overlay-capture", layout.MergedRoot, "overlay", 0, overlayData); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("mount overlay capture root %s: %w", layout.MergedRoot, err)
	}
	mounted = append(mounted, layout.MergedRoot)

	for _, scratch := range overlayCaptureScratchMounts {
		if err := mountOverlayCaptureScratchPath(layout.MergedRoot, scratch.Path, scratch.Data, &mounted); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	if err := bindOverlayCaptureInputProjection(req, layout.MergedRoot, &mounted); err != nil {
		cleanup()
		return "", nil, err
	}
	for _, guestPath := range capture.BaselinePaths {
		if err := bindOverlayCaptureGuestPath(layout.MergedRoot, guestPath, false, &mounted); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	for _, guestPath := range overlayCaptureVirtualMounts {
		if err := bindOverlayCaptureGuestPath(layout.MergedRoot, guestPath, false, &mounted); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanup()
			return "", nil, err
		}
	}
	if err := bindOverlayCaptureRuntimeSockets(layout.MergedRoot, &mounted); err != nil {
		cleanup()
		return "", nil, err
	}
	return layout.MergedRoot, cleanup, nil
}

func enterOverlayCaptureMountNamespace() (func(), error) {
	runtime.LockOSThread()
	oldNS, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("open current overlay capture mount namespace: %w", err)
	}
	cleanup := func() {
		defer runtime.UnlockOSThread()
		defer oldNS.Close()
		if err := unix.Setns(int(oldNS.Fd()), unix.CLONE_NEWNS); err != nil {
			fmt.Fprintf(os.Stderr, "restore overlay capture mount namespace: %v\n", err)
			return
		}
	}
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		cleanup()
		return nil, fmt.Errorf("create overlay capture mount namespace: %w", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		cleanup()
		return nil, fmt.Errorf("make overlay capture mount namespace private: %w", err)
	}
	return cleanup, nil
}

func newOverlayCaptureLayout(upperDir string) (overlayCaptureLayout, error) {
	upperDir, err := cleanOverlayCaptureAbsolutePath("overlay upperdir", upperDir)
	if err != nil {
		return overlayCaptureLayout{}, err
	}
	root, err := cleanOverlayCaptureAbsolutePath("overlay capture root", overlayCaptureRoot)
	if err != nil {
		return overlayCaptureLayout{}, err
	}
	if upperDir == root || !strings.HasPrefix(upperDir, root+string(filepath.Separator)) {
		return overlayCaptureLayout{}, fmt.Errorf("overlay upperdir %q must be under %s", upperDir, root)
	}
	baseDir := filepath.Dir(upperDir)
	return overlayCaptureLayout{
		BaseDir:    baseDir,
		UpperDir:   upperDir,
		WorkDir:    filepath.Join(baseDir, "work"),
		MergedRoot: filepath.Join(baseDir, "merged"),
	}, nil
}

func prepareOverlayCaptureLayout(layout overlayCaptureLayout) error {
	if err := os.MkdirAll(layout.BaseDir, 0o755); err != nil {
		return fmt.Errorf("create overlay capture base directory %s: %w", layout.BaseDir, err)
	}
	for _, dir := range []string{layout.UpperDir, layout.WorkDir, layout.MergedRoot} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("clear overlay capture directory %s: %w", dir, err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create overlay capture directory %s: %w", dir, err)
		}
	}
	return nil
}

func bindOverlayCaptureInputProjection(req vsockexec.ExecRequest, mergedRoot string, mounted *[]string) error {
	source, target, readOnly, ok, err := overlayCaptureInputProjectionBind(req, mergedRoot)
	if err != nil || !ok {
		return err
	}
	return bindOverlayCapturePath(source, target, readOnly, mounted)
}

func overlayCaptureInputProjectionBind(req vsockexec.ExecRequest, mergedRoot string) (string, string, bool, bool, error) {
	projection := req.InputProjection
	if projection == nil {
		return "", "", false, false, nil
	}
	source := ""
	targetGuestPath := ""
	readOnly := false
	if projection.MountSourceReadOnly {
		sourceRoot, err := cleanOverlayCaptureAbsolutePath("input projection source root", projection.SourceRoot)
		if err != nil {
			return "", "", false, false, err
		}
		source = sourceRoot
		targetGuestPath = sourceRoot
		readOnly = true
	} else {
		targetRoot, err := cleanOverlayCaptureAbsolutePath("input projection target root", projection.TargetRoot)
		if err != nil {
			return "", "", false, false, err
		}
		source = targetRoot
		targetGuestPath = targetRoot
	}
	target, err := overlayCaptureGuestTarget(mergedRoot, targetGuestPath)
	if err != nil {
		return "", "", false, false, err
	}
	return source, target, readOnly, true, nil
}

func bindOverlayCaptureGuestPath(mergedRoot, guestPath string, readOnly bool, mounted *[]string) error {
	source, err := cleanOverlayCaptureAbsolutePath("overlay capture guest path", guestPath)
	if err != nil {
		return err
	}
	target, err := overlayCaptureGuestTarget(mergedRoot, source)
	if err != nil {
		return err
	}
	return bindOverlayCapturePath(source, target, readOnly, mounted)
}

func bindOverlayCapturePath(source, target string, readOnly bool, mounted *[]string) error {
	info, err := os.Stat(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return err
		}
		return fmt.Errorf("stat overlay capture bind source %s: %w", source, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("overlay capture bind source %s is not a directory", source)
	}
	if err := ensureOverlayCaptureDir(target); err != nil {
		return err
	}
	if err := unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind overlay capture path %s at %s: %w", source, target, err)
	}
	*mounted = append(*mounted, target)
	if readOnly {
		if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("remount overlay capture bind %s read-only: %w", target, err)
		}
	}
	return nil
}

func bindOverlayCaptureRuntimeSockets(mergedRoot string, mounted *[]string) error {
	return bindOverlayCaptureRuntimeSocketsWith(mergedRoot, overlayCaptureRuntimeSockets, mounted, bindOverlayCaptureFile)
}

func bindOverlayCaptureRuntimeSocketsWith(mergedRoot string, sockets []overlayCaptureRuntimeSocket, mounted *[]string, bindFile overlayCaptureFileBinder) error {
	mountedTargets := make(map[string]struct{}, len(sockets))
	for _, socket := range sockets {
		source, target, ok, err := overlayCaptureRuntimeSocketBind(mergedRoot, socket)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, exists := mountedTargets[target]; exists {
			continue
		}
		if err := bindFile(source, target, mounted); err != nil {
			return err
		}
		mountedTargets[target] = struct{}{}
	}
	return nil
}

func overlayCaptureRuntimeSocketBind(mergedRoot string, socket overlayCaptureRuntimeSocket) (string, string, bool, error) {
	source, err := cleanOverlayCaptureAbsolutePath("overlay capture runtime socket source", socket.SourcePath)
	if err != nil {
		return "", "", false, err
	}
	target, err := overlayCaptureGuestTarget(mergedRoot, socket.GuestPath)
	if err != nil {
		return "", "", false, err
	}
	info, err := os.Stat(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("stat overlay capture runtime socket %s: %w", source, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return "", "", false, nil
	}
	return source, target, true, nil
}

func bindOverlayCaptureFile(source, target string, mounted *[]string) error {
	if err := ensureOverlayCaptureFile(target); err != nil {
		return err
	}
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind overlay capture file %s at %s: %w", source, target, err)
	}
	*mounted = append(*mounted, target)
	return nil
}

func mountOverlayCaptureScratchPath(mergedRoot, guestPath, data string, mounted *[]string) error {
	target, err := overlayCaptureGuestTarget(mergedRoot, guestPath)
	if err != nil {
		return err
	}
	if err := ensureOverlayCaptureDir(target); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", target, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, data); err != nil {
		return fmt.Errorf("mount overlay capture scratch path %s: %w", guestPath, err)
	}
	*mounted = append(*mounted, target)
	return nil
}

func cleanOverlayCaptureAbsolutePath(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("missing %s", name)
	}
	cleaned := filepath.Clean(value)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("%s %q is not absolute", name, value)
	}
	if cleaned == "/" {
		return "", fmt.Errorf("%s must not be /", name)
	}
	return cleaned, nil
}

func overlayCaptureGuestTarget(mergedRoot, guestPath string) (string, error) {
	mergedRoot, err := cleanOverlayCaptureAbsolutePath("overlay merged root", mergedRoot)
	if err != nil {
		return "", err
	}
	guestPath, err = cleanOverlayCaptureAbsolutePath("overlay capture guest path", guestPath)
	if err != nil {
		return "", err
	}
	return resolveOverlayCaptureGuestTarget(mergedRoot, guestPath, 0)
}

func resolveOverlayCaptureGuestTarget(mergedRoot, guestPath string, depth int) (string, error) {
	if depth > 32 {
		return "", fmt.Errorf("overlay capture guest path %q has too many symlinks", guestPath)
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(guestPath), string(filepath.Separator)), string(filepath.Separator))
	currentTarget := filepath.Clean(mergedRoot)
	currentGuest := string(filepath.Separator)
	for i, part := range parts {
		if part == "" {
			continue
		}
		candidate := filepath.Join(currentTarget, part)
		info, err := os.Lstat(candidate)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(candidate)
			if err != nil {
				return "", fmt.Errorf("read overlay capture path symlink %s: %w", candidate, err)
			}
			remaining := filepath.Join(parts[i+1:]...)
			baseGuest := filepath.Dir(filepath.Join(currentGuest, part))
			nextGuest := ""
			if filepath.IsAbs(link) {
				nextGuest = filepath.Join(link, remaining)
			} else {
				nextGuest = filepath.Join(baseGuest, link, remaining)
			}
			return resolveOverlayCaptureGuestTarget(mergedRoot, nextGuest, depth+1)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat overlay capture path %s: %w", candidate, err)
		}
		currentTarget = candidate
		currentGuest = filepath.Join(currentGuest, part)
	}
	return currentTarget, nil
}

func ensureOverlayCaptureDir(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("overlay capture path %s is not a directory", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat overlay capture path %s: %w", path, err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create overlay capture path %s: %w", path, err)
	}
	return nil
}

func ensureOverlayCaptureFile(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if info.IsDir() {
			return fmt.Errorf("overlay capture path %s is a directory", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat overlay capture path %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create overlay capture path parent %s: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create overlay capture file %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close overlay capture file %s: %w", path, err)
	}
	return nil
}

func scanOverlayCapture(capture *vsockexec.OverlayCapture) (*vsockexec.OverlayCaptureResult, error) {
	if capture == nil {
		return nil, nil
	}
	result, err := overlaycapture.Scan(capture.UpperDir, overlaycapture.Options{
		BaselinePaths:       append([]string(nil), capture.BaselinePaths...),
		DeclaredFileOutputs: append([]string(nil), capture.DeclaredFileOutputs...),
		IgnoredPrefixes:     append([]string(nil), capture.IgnoredPrefixes...),
	})
	if err != nil {
		return nil, fmt.Errorf("scan overlay capture: %w", err)
	}
	return &vsockexec.OverlayCaptureResult{
		Entries:       overlayCaptureEntries(result.Entries),
		EscapedWrites: overlayCaptureEntries(result.EscapedWrites),
	}, nil
}

func overlayCaptureEntries(entries []overlaycapture.Entry) []vsockexec.OverlayCaptureEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]vsockexec.OverlayCaptureEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, vsockexec.OverlayCaptureEntry{
			Path: entry.Path,
			Kind: string(entry.Kind),
			Mode: uint32(entry.Mode),
		})
	}
	return out
}
