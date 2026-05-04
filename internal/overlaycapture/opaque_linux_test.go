//go:build linux

package overlaycapture

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestScanReportsOverlayWhiteoutXattrAsDelete(t *testing.T) {
	t.Parallel()

	upperDir := t.TempDir()
	whiteout := filepath.Join(upperDir, "workspace", ".wh.cache")
	if err := os.MkdirAll(filepath.Dir(whiteout), 0o755); err != nil {
		t.Fatalf("create whiteout parent: %v", err)
	}
	if err := os.WriteFile(whiteout, nil, 0o644); err != nil {
		t.Fatalf("write whiteout marker: %v", err)
	}
	if err := unix.Setxattr(whiteout, "user.overlay.whiteout", nil, 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skipf("xattrs unsupported on temp filesystem: %v", err)
		}
		t.Fatalf("set whiteout xattr: %v", err)
	}

	result, err := Scan(upperDir, Options{})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	assertEntry(t, result.Entries, Entry{Path: "/workspace/cache", Kind: EntryKindDelete})
	assertEntry(t, result.EscapedWrites, Entry{Path: "/workspace/cache", Kind: EntryKindDelete})
}
