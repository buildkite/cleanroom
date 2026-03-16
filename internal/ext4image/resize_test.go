package ext4image

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

func stubResizeDependencies() func() {
	prevResolve := resolveE2FSProgsBinary
	prevRun := runCommand
	prevTruncate := truncateFile

	resolveE2FSProgsBinary = func(binary string) (string, error) {
		return binary + "-path", nil
	}
	runCommand = runCommandWithAllowedExitCodes
	truncateFile = os.Truncate

	return func() {
		resolveE2FSProgsBinary = prevResolve
		runCommand = prevRun
		truncateFile = prevTruncate
	}
}

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
