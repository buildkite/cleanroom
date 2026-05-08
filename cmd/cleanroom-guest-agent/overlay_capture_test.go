//go:build linux

package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func TestScanOverlayCaptureReportsEscapedWrites(t *testing.T) {
	t.Parallel()

	upperDir := t.TempDir()
	writeOverlayCaptureTestFile(t, upperDir, "workspace/result.txt")
	writeOverlayCaptureTestFile(t, upperDir, "etc/profile")

	result, err := scanOverlayCapture(&vsockexec.OverlayCapture{
		UpperDir:            upperDir,
		DeclaredFileOutputs: []string{"/workspace/result.txt"},
	})
	if err != nil {
		t.Fatalf("scanOverlayCapture returned error: %v", err)
	}
	if result == nil {
		t.Fatal("expected overlay capture result")
	}
	assertOverlayCaptureEntry(t, result.EscapedWrites, vsockexec.OverlayCaptureEntry{Path: "/etc/profile", Kind: "write", Mode: 0o644})
}

func TestSendExitResultFailsWhenOverlayCaptureScanFails(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sendExitResult(newFrameSender(&buf), nil, &vsockexec.OverlayCapture{UpperDir: filepath.Join(t.TempDir(), "missing")})

	res, err := vsockexec.DecodeStreamResponse(&buf, vsockexec.StreamCallbacks{})
	if err != nil {
		t.Fatalf("DecodeStreamResponse returned error: %v", err)
	}
	if got, want := res.ExitCode, 1; got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
	if !strings.Contains(res.Error, "scan overlay capture") {
		t.Fatalf("unexpected error: %q", res.Error)
	}
}

func TestSendExitResultIncludesOverlayCapture(t *testing.T) {
	t.Parallel()

	upperDir := t.TempDir()
	writeOverlayCaptureTestFile(t, upperDir, "etc/profile")

	var buf bytes.Buffer
	sendExitResult(newFrameSender(&buf), nil, &vsockexec.OverlayCapture{UpperDir: upperDir})

	res, err := vsockexec.DecodeStreamResponse(&buf, vsockexec.StreamCallbacks{})
	if err != nil {
		t.Fatalf("DecodeStreamResponse returned error: %v", err)
	}
	if got, want := res.ExitCode, 0; got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
	if res.OverlayCapture == nil {
		t.Fatal("expected overlay capture result")
	}
	assertOverlayCaptureEntry(t, res.OverlayCapture.EscapedWrites, vsockexec.OverlayCaptureEntry{Path: "/etc/profile", Kind: "write", Mode: 0o644})
}

func TestOverlayCaptureLayoutUsesSiblingWorkAndMergedRoots(t *testing.T) {
	t.Parallel()

	layout, err := newOverlayCaptureLayout("/run/cleanroom/overlay-captures/dependency-cache/upper")
	if err != nil {
		t.Fatalf("newOverlayCaptureLayout returned error: %v", err)
	}
	if got, want := layout.BaseDir, "/run/cleanroom/overlay-captures/dependency-cache"; got != want {
		t.Fatalf("unexpected base dir: got %q want %q", got, want)
	}
	if got, want := layout.WorkDir, "/run/cleanroom/overlay-captures/dependency-cache/work"; got != want {
		t.Fatalf("unexpected work dir: got %q want %q", got, want)
	}
	if got, want := layout.MergedRoot, "/run/cleanroom/overlay-captures/dependency-cache/merged"; got != want {
		t.Fatalf("unexpected merged root: got %q want %q", got, want)
	}
}

func TestOverlayCaptureLayoutRejectsUnsafeUpperDir(t *testing.T) {
	t.Parallel()

	if _, err := newOverlayCaptureLayout("relative/upper"); err == nil {
		t.Fatal("expected relative upperdir to fail")
	}
	if _, err := newOverlayCaptureLayout("/"); err == nil {
		t.Fatal("expected root upperdir to fail")
	}
	if _, err := newOverlayCaptureLayout("/var/tmp/upper"); err == nil {
		t.Fatal("expected upperdir outside managed root to fail")
	}
}

func TestOverlayCaptureGuestTargetStaysUnderMergedRoot(t *testing.T) {
	t.Parallel()

	got, err := overlayCaptureGuestTarget("/merged", "/home/cleanroom/.cache")
	if err != nil {
		t.Fatalf("overlayCaptureGuestTarget returned error: %v", err)
	}
	if want := "/merged/home/cleanroom/.cache"; got != want {
		t.Fatalf("unexpected guest target: got %q want %q", got, want)
	}
}

func TestOverlayCaptureGuestTargetResolvesAbsoluteSymlinkInsideMergedRoot(t *testing.T) {
	t.Parallel()

	mergedRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mergedRoot, "var"), 0o755); err != nil {
		t.Fatalf("create var: %v", err)
	}
	if err := os.Symlink("/run", filepath.Join(mergedRoot, "var", "run")); err != nil {
		t.Fatalf("create var/run symlink: %v", err)
	}

	got, err := overlayCaptureGuestTarget(mergedRoot, "/var/run/service")
	if err != nil {
		t.Fatalf("overlayCaptureGuestTarget returned error: %v", err)
	}
	if want := filepath.Join(mergedRoot, "run", "service"); got != want {
		t.Fatalf("unexpected guest target: got %q want %q", got, want)
	}
}

func TestOverlayCaptureGuestTargetResolvesRelativeSymlinkInsideMergedRoot(t *testing.T) {
	t.Parallel()

	mergedRoot := t.TempDir()
	if err := os.Symlink("usr/lib64", filepath.Join(mergedRoot, "lib64")); err != nil {
		t.Fatalf("create lib64 symlink: %v", err)
	}

	got, err := overlayCaptureGuestTarget(mergedRoot, "/lib64/pkg")
	if err != nil {
		t.Fatalf("overlayCaptureGuestTarget returned error: %v", err)
	}
	if want := filepath.Join(mergedRoot, "usr", "lib64", "pkg"); got != want {
		t.Fatalf("unexpected guest target: got %q want %q", got, want)
	}
}

func TestOverlayCaptureInputProjectionBindUsesTargetRootWhenNotMountedOverSource(t *testing.T) {
	t.Parallel()

	source, target, readOnly, ok, err := overlayCaptureInputProjectionBind(vsockexec.ExecRequest{
		InputProjection: &vsockexec.InputProjection{
			SourceRoot:          "/workspace",
			TargetRoot:          "/run/cleanroom/input-projections/dependencies/toolchain",
			MountSourceReadOnly: false,
		},
	}, "/merged")
	if err != nil {
		t.Fatalf("overlayCaptureInputProjectionBind returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected input projection bind")
	}
	if source != "/run/cleanroom/input-projections/dependencies/toolchain" {
		t.Fatalf("unexpected source: %q", source)
	}
	if target != "/merged/run/cleanroom/input-projections/dependencies/toolchain" {
		t.Fatalf("unexpected target: %q", target)
	}
	if readOnly {
		t.Fatal("did not expect read-only bind for target-root projection")
	}
}

func TestOverlayCaptureInputProjectionBindUsesSourceRootWhenMountedReadOnly(t *testing.T) {
	t.Parallel()

	source, target, readOnly, ok, err := overlayCaptureInputProjectionBind(vsockexec.ExecRequest{
		InputProjection: &vsockexec.InputProjection{
			SourceRoot:          "/workspace",
			TargetRoot:          "/run/cleanroom/input-projections/dependencies/toolchain",
			MountSourceReadOnly: true,
		},
	}, "/merged")
	if err != nil {
		t.Fatalf("overlayCaptureInputProjectionBind returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected input projection bind")
	}
	if source != "/workspace" || target != "/merged/workspace" {
		t.Fatalf("unexpected bind: source %q target %q", source, target)
	}
	if !readOnly {
		t.Fatal("expected read-only bind for source-root projection")
	}
}

func TestBindOverlayCaptureRuntimeSocketsPreservesVarRunDockerSocketThroughRunScratch(t *testing.T) {
	t.Parallel()

	mergedRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mergedRoot, "run"), 0o755); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(mergedRoot, "var"), 0o755); err != nil {
		t.Fatalf("create var: %v", err)
	}
	if err := os.Symlink("/run", filepath.Join(mergedRoot, "var", "run")); err != nil {
		t.Fatalf("create var/run symlink: %v", err)
	}

	socketPath := listenOverlayCaptureTestSocket(t)
	var gotSource, gotTarget string
	var mounted []string
	err := bindOverlayCaptureRuntimeSocketsWith(mergedRoot, []overlayCaptureRuntimeSocket{
		{SourcePath: socketPath, GuestPath: "/var/run/docker.sock"},
	}, &mounted, func(source, target string, mounted *[]string) error {
		gotSource = source
		gotTarget = target
		*mounted = append(*mounted, target)
		return nil
	})
	if err != nil {
		t.Fatalf("bindOverlayCaptureRuntimeSocketsWith returned error: %v", err)
	}
	if gotSource != socketPath {
		t.Fatalf("unexpected source: got %q want %q", gotSource, socketPath)
	}
	if want := filepath.Join(mergedRoot, "run", "docker.sock"); gotTarget != want {
		t.Fatalf("unexpected target: got %q want %q", gotTarget, want)
	}
	if len(mounted) != 1 || mounted[0] != gotTarget {
		t.Fatalf("unexpected mounted targets: %#v", mounted)
	}
}

func TestBindOverlayCaptureRuntimeSocketsDeduplicatesVarRunSymlink(t *testing.T) {
	t.Parallel()

	mergedRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mergedRoot, "run"), 0o755); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(mergedRoot, "var"), 0o755); err != nil {
		t.Fatalf("create var: %v", err)
	}
	if err := os.Symlink("/run", filepath.Join(mergedRoot, "var", "run")); err != nil {
		t.Fatalf("create var/run symlink: %v", err)
	}

	socketPath := listenOverlayCaptureTestSocket(t)
	binds := 0
	var mounted []string
	err := bindOverlayCaptureRuntimeSocketsWith(mergedRoot, []overlayCaptureRuntimeSocket{
		{SourcePath: socketPath, GuestPath: "/run/docker.sock"},
		{SourcePath: socketPath, GuestPath: "/var/run/docker.sock"},
	}, &mounted, func(source, target string, mounted *[]string) error {
		binds++
		*mounted = append(*mounted, target)
		return nil
	})
	if err != nil {
		t.Fatalf("bindOverlayCaptureRuntimeSocketsWith returned error: %v", err)
	}
	if binds != 1 {
		t.Fatalf("unexpected bind count: got %d want 1", binds)
	}
	if want := []string{filepath.Join(mergedRoot, "run", "docker.sock")}; len(mounted) != len(want) || mounted[0] != want[0] {
		t.Fatalf("unexpected mounted targets: got %#v want %#v", mounted, want)
	}
}

func TestBindOverlayCaptureRuntimeSocketsIgnoresMissingAndNonSocketPaths(t *testing.T) {
	t.Parallel()

	mergedRoot := t.TempDir()
	regularFile := filepath.Join(t.TempDir(), "docker.sock")
	if err := os.WriteFile(regularFile, []byte("not a socket"), 0o644); err != nil {
		t.Fatalf("write regular file: %v", err)
	}

	var mounted []string
	err := bindOverlayCaptureRuntimeSocketsWith(mergedRoot, []overlayCaptureRuntimeSocket{
		{SourcePath: filepath.Join(t.TempDir(), "missing.sock"), GuestPath: "/run/docker.sock"},
		{SourcePath: regularFile, GuestPath: "/var/run/docker.sock"},
	}, &mounted, func(source, target string, mounted *[]string) error {
		t.Fatalf("unexpected bind for source %s target %s", source, target)
		return nil
	})
	if err != nil {
		t.Fatalf("bindOverlayCaptureRuntimeSocketsWith returned error: %v", err)
	}
	if len(mounted) != 0 {
		t.Fatalf("unexpected mounted targets: %#v", mounted)
	}
}

func TestOverlayCaptureRuntimeSocketDefaultsCoverDockerSocketPaths(t *testing.T) {
	t.Parallel()

	want := []overlayCaptureRuntimeSocket{
		{SourcePath: "/run/docker.sock", GuestPath: "/run/docker.sock"},
		{SourcePath: "/var/run/docker.sock", GuestPath: "/var/run/docker.sock"},
	}
	if len(overlayCaptureRuntimeSockets) != len(want) {
		t.Fatalf("unexpected runtime socket count: got %d want %d", len(overlayCaptureRuntimeSockets), len(want))
	}
	for _, socket := range overlayCaptureRuntimeSockets {
		if !strings.HasPrefix(socket.SourcePath, "/run/") && !strings.HasPrefix(socket.SourcePath, "/var/run/") {
			t.Fatalf("runtime socket source %q is outside expected runtime dirs", socket.SourcePath)
		}
		if !strings.HasPrefix(socket.GuestPath, "/run/") && !strings.HasPrefix(socket.GuestPath, "/var/run/") {
			t.Fatalf("runtime socket guest path %q is outside expected runtime dirs", socket.GuestPath)
		}
	}
	for i, socket := range want {
		if overlayCaptureRuntimeSockets[i] != socket {
			t.Fatalf("unexpected runtime socket at %d: got %#v want %#v", i, overlayCaptureRuntimeSockets[i], socket)
		}
	}
}

func TestNewGuestCommandUsesOverlayRootAsChroot(t *testing.T) {
	t.Parallel()

	cmd := newGuestCommand(vsockexec.ExecRequest{Command: []string{"sh", "-lc", "true"}, Dir: "/workspace"}, "/run/cleanroom/overlay/merged")
	if cmd.SysProcAttr == nil {
		t.Fatal("expected SysProcAttr")
	}
	if got, want := cmd.SysProcAttr.Chroot, "/run/cleanroom/overlay/merged"; got != want {
		t.Fatalf("unexpected chroot: got %q want %q", got, want)
	}
	if got, want := cmd.Dir, "/workspace"; got != want {
		t.Fatalf("unexpected command dir: got %q want %q", got, want)
	}
}

func TestNewGuestCommandNormalizesOverlayRootRelativeDirs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		dir  string
		want string
	}{
		{name: "empty", dir: "", want: "/"},
		{name: "dot", dir: ".", want: "/"},
		{name: "relative", dir: "workspace/subdir", want: "/workspace/subdir"},
		{name: "relative parent", dir: "../workspace", want: "/workspace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cmd := newGuestCommand(vsockexec.ExecRequest{Command: []string{"sh", "-lc", "true"}, Dir: tc.dir}, "/run/cleanroom/overlay/merged")
			if got := cmd.Dir; got != tc.want {
				t.Fatalf("unexpected command dir: got %q want %q", got, tc.want)
			}
		})
	}
}

func listenOverlayCaptureTestSocket(t *testing.T) string {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	return socketPath
}

func assertOverlayCaptureEntry(t *testing.T, entries []vsockexec.OverlayCaptureEntry, want vsockexec.OverlayCaptureEntry) {
	t.Helper()
	for _, got := range entries {
		if got.Path == want.Path && got.Kind == want.Kind && got.Mode&0o777 == want.Mode&0o777 {
			return
		}
	}
	t.Fatalf("missing overlay capture entry %#v in %#v", want, entries)
}

func writeOverlayCaptureTestFile(t *testing.T, root, rel string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(rel), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func setTestOverlayCaptureRoot(t *testing.T, root string) func() {
	t.Helper()
	previous := overlayCaptureRoot
	overlayCaptureRoot = root
	return func() {
		overlayCaptureRoot = previous
	}
}
