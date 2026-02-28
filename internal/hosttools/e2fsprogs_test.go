package hosttools

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestResolveBinaryPrefersLookPath(t *testing.T) {
	t.Parallel()

	got, err := resolveBinary(
		"mkfs.ext4",
		func(string) (string, error) { return "/usr/bin/mkfs.ext4", nil },
		func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		[]string{"/opt/homebrew/opt/e2fsprogs/sbin/mkfs.ext4"},
	)
	if err != nil {
		t.Fatalf("resolveBinary returned error: %v", err)
	}
	if got != "/usr/bin/mkfs.ext4" {
		t.Fatalf("unexpected resolved path: got %q", got)
	}
}

func TestResolveBinaryFallsBackToCandidate(t *testing.T) {
	t.Parallel()

	candidate := "/opt/homebrew/opt/e2fsprogs/sbin/debugfs"
	got, err := resolveBinary(
		"debugfs",
		func(string) (string, error) { return "", errors.New("not found") },
		func(path string) (os.FileInfo, error) {
			if path == candidate {
				return &fakeFileInfo{}, nil
			}
			return nil, os.ErrNotExist
		},
		[]string{candidate},
	)
	if err != nil {
		t.Fatalf("resolveBinary returned error: %v", err)
	}
	if got != candidate {
		t.Fatalf("unexpected resolved path: got %q want %q", got, candidate)
	}
}

func TestResolveBinaryReturnsHelpfulError(t *testing.T) {
	t.Parallel()

	_, err := resolveBinary(
		"mkfs.ext4",
		func(string) (string, error) { return "", errors.New("not found") },
		func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		[]string{"/opt/homebrew/opt/e2fsprogs/sbin/mkfs.ext4"},
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "mkfs.ext4") {
		t.Fatalf("expected binary name in error, got %v", err)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(err.Error(), "brew install e2fsprogs") {
		t.Fatalf("expected brew hint on darwin, got %v", err)
	}
}

func TestCandidateBinaryPathsIncludesSbinAndBin(t *testing.T) {
	t.Parallel()

	got := candidateBinaryPaths("mkfs.ext4", []string{"/opt/homebrew/opt/e2fsprogs"})
	if len(got) != 2 {
		t.Fatalf("unexpected candidate count: %d", len(got))
	}
	if got[0] != "/opt/homebrew/opt/e2fsprogs/sbin/mkfs.ext4" {
		t.Fatalf("unexpected first candidate: %q", got[0])
	}
	if got[1] != "/opt/homebrew/opt/e2fsprogs/bin/mkfs.ext4" {
		t.Fatalf("unexpected second candidate: %q", got[1])
	}
}

type fakeFileInfo struct{}

func (*fakeFileInfo) Name() string       { return "bin" }
func (*fakeFileInfo) Size() int64        { return 1 }
func (*fakeFileInfo) Mode() os.FileMode  { return 0o755 }
func (*fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (*fakeFileInfo) IsDir() bool        { return false }
func (*fakeFileInfo) Sys() any           { return nil }
