package firecracker

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFileCopiesContentsAndMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src.ext4")
	dst := filepath.Join(dir, "dst.ext4")

	srcData := []byte("rootfs-data-1234567890")
	if err := os.WriteFile(src, srcData, 0o640); err != nil {
		t.Fatalf("write src: %v", err)
	}
	// Ensure destination truncate behavior is correct.
	if err := os.WriteFile(dst, []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"), 0o600); err != nil {
		t.Fatalf("write preexisting dst: %v", err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	gotData, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(gotData, srcData) {
		t.Fatalf("unexpected dst contents: got %q want %q", string(gotData), string(srcData))
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if dstInfo.Mode().Perm() != srcInfo.Mode().Perm() {
		t.Fatalf("unexpected dst mode: got %o want %o", dstInfo.Mode().Perm(), srcInfo.Mode().Perm())
	}
}
