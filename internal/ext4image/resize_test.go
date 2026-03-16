package ext4image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEnsureMinimumSizeNoOpWhenAlreadyLarge(t *testing.T) {
	restore := stubResizeDependencies()
	defer restore()

	imagePath := createSizedTempFile(t, 8<<20)
	var commands [][]string
	runCommand = func(context.Context, string, []string, ...int) error {
		commands = append(commands, nil)
		return nil
	}

	if err := EnsureMinimumSize(context.Background(), imagePath, 4<<20); err != nil {
		t.Fatalf("EnsureMinimumSize returned error: %v", err)
	}
	if len(commands) != 0 {
		t.Fatalf("expected no resize commands, got %d", len(commands))
	}
	info, err := os.Stat(imagePath)
	if err != nil {
		t.Fatalf("stat resized image: %v", err)
	}
	if got, want := info.Size(), int64(8<<20); got != want {
		t.Fatalf("unexpected image size: got %d want %d", got, want)
	}
}

func TestEnsureMinimumSizeGrowsAndRoundsUp(t *testing.T) {
	restore := stubResizeDependencies()
	defer restore()

	imagePath := createSizedTempFile(t, 5<<20)
	var gotCommands [][]string
	runCommand = func(_ context.Context, binary string, args []string, allowedExitCodes ...int) error {
		record := append([]string{binary}, append([]string(nil), args...)...)
		gotCommands = append(gotCommands, record)
		return nil
	}

	minimumBytes := int64((9 << 20) + 1)
	if err := EnsureMinimumSize(context.Background(), imagePath, minimumBytes); err != nil {
		t.Fatalf("EnsureMinimumSize returned error: %v", err)
	}

	wantCommands := [][]string{
		{"e2fsck-path", "-fy", imagePath},
		{"resize2fs-path", imagePath},
	}
	if !reflect.DeepEqual(gotCommands, wantCommands) {
		t.Fatalf("unexpected resize commands: got %#v want %#v", gotCommands, wantCommands)
	}

	info, err := os.Stat(imagePath)
	if err != nil {
		t.Fatalf("stat resized image: %v", err)
	}
	if got, want := info.Size(), int64(12<<20); got != want {
		t.Fatalf("unexpected image size: got %d want %d", got, want)
	}
}

func TestEnsureMinimumSizePropagatesResolveError(t *testing.T) {
	restore := stubResizeDependencies()
	defer restore()

	imagePath := createSizedTempFile(t, 4<<20)
	resolveE2FSProgsBinary = func(binary string) (string, error) {
		if binary == "e2fsck" {
			return "", errors.New("missing tool")
		}
		return binary + "-path", nil
	}

	err := EnsureMinimumSize(context.Background(), imagePath, 8<<20)
	if err == nil {
		t.Fatal("expected resize to fail")
	}
	if !strings.Contains(err.Error(), "missing tool") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureMinimumSizeGrowsBlockDeviceWithoutTruncate(t *testing.T) {
	restore := stubResizeDependencies()
	defer restore()

	imagePath := "/dev/zvol/tank/cleanroom/sandboxes/sandbox-1"
	statPath = func(string) (os.FileInfo, error) {
		return fakeFileInfo{mode: os.ModeDevice}, nil
	}
	evalSymlinks = func(string) (string, error) {
		return "/dev/zd42", nil
	}
	readFile = func(path string) ([]byte, error) {
		wantPath := filepath.Join("/sys/class/block", "zd42", "size")
		if path != wantPath {
			return nil, fmt.Errorf("unexpected size path %q", path)
		}
		return []byte("24576\n"), nil
	}
	truncateCalled := false
	truncateFile = func(string, int64) error {
		truncateCalled = true
		return nil
	}

	var gotCommands [][]string
	runCommand = func(_ context.Context, binary string, args []string, allowedExitCodes ...int) error {
		record := append([]string{binary}, append([]string(nil), args...)...)
		gotCommands = append(gotCommands, record)
		return nil
	}

	if err := EnsureMinimumSize(context.Background(), imagePath, int64((9<<20)+1)); err != nil {
		t.Fatalf("EnsureMinimumSize returned error: %v", err)
	}

	wantCommands := [][]string{
		{"e2fsck-path", "-fy", imagePath},
		{"resize2fs-path", imagePath},
	}
	if !reflect.DeepEqual(gotCommands, wantCommands) {
		t.Fatalf("unexpected resize commands: got %#v want %#v", gotCommands, wantCommands)
	}
	if truncateCalled {
		t.Fatal("expected block-device resize to avoid truncate")
	}
}

func TestEnsureMinimumSizeRejectsTooSmallBlockDevice(t *testing.T) {
	restore := stubResizeDependencies()
	defer restore()

	imagePath := "/dev/zvol/tank/cleanroom/sandboxes/sandbox-1"
	statPath = func(string) (os.FileInfo, error) {
		return fakeFileInfo{mode: os.ModeDevice}, nil
	}
	evalSymlinks = func(string) (string, error) {
		return "/dev/zd42", nil
	}
	readFile = func(string) ([]byte, error) {
		return []byte("16384\n"), nil
	}

	var called bool
	runCommand = func(context.Context, string, []string, ...int) error {
		called = true
		return nil
	}

	err := EnsureMinimumSize(context.Background(), imagePath, int64((9<<20)+1))
	if err == nil {
		t.Fatal("expected block-device resize to fail")
	}
	if !strings.Contains(err.Error(), "below requested minimum") {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Fatal("expected resize commands to be skipped")
	}
}

func stubResizeDependencies() func() {
	prevResolve := resolveE2FSProgsBinary
	prevRun := runCommand
	prevStat := statPath
	prevTruncate := truncateFile
	prevEvalSymlinks := evalSymlinks
	prevReadFile := readFile

	resolveE2FSProgsBinary = func(binary string) (string, error) {
		return binary + "-path", nil
	}
	runCommand = runCommandWithAllowedExitCodes
	statPath = os.Stat
	truncateFile = os.Truncate
	evalSymlinks = filepath.EvalSymlinks
	readFile = os.ReadFile

	return func() {
		resolveE2FSProgsBinary = prevResolve
		runCommand = prevRun
		statPath = prevStat
		truncateFile = prevTruncate
		evalSymlinks = prevEvalSymlinks
		readFile = prevReadFile
	}
}

type fakeFileInfo struct {
	mode os.FileMode
	size int64
}

func (f fakeFileInfo) Name() string       { return "fake" }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

func createSizedTempFile(t *testing.T, size int64) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write temp image: %v", err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatalf("truncate temp image: %v", err)
	}
	return path
}
