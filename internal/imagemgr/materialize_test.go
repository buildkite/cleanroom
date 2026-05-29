package imagemgr

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractTarRejectsWriteThroughAbsoluteSymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stream := tarStreamWithEntries(t,
		tarEntry{Header: &tar.Header{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "/tmp", Mode: 0o777}},
		tarEntry{Header: &tar.Header{Name: "escape/pwned", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len("owned"))}, Body: []byte("owned")},
	)

	err := extractTar(root, bytes.NewReader(stream))
	if err == nil {
		t.Fatal("expected symlink-escape tar to be rejected")
	}
}

func TestExtractTarRejectsWriteThroughRelativeEscapeSymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stream := tarStreamWithEntries(t,
		tarEntry{Header: &tar.Header{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "../../../../tmp", Mode: 0o777}},
		tarEntry{Header: &tar.Header{Name: "escape/pwned", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len("owned"))}, Body: []byte("owned")},
	)

	err := extractTar(root, bytes.NewReader(stream))
	if err == nil {
		t.Fatal("expected relative symlink-escape tar to be rejected")
	}
}

func TestExtractTarAllowsSafeInternalSymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stream := tarStreamWithEntries(t,
		tarEntry{Header: &tar.Header{Name: "usr", Typeflag: tar.TypeDir, Mode: 0o755}},
		tarEntry{Header: &tar.Header{Name: "usr/bin", Typeflag: tar.TypeDir, Mode: 0o755}},
		tarEntry{Header: &tar.Header{Name: "usr/bin/tool", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len("binary"))}, Body: []byte("binary")},
		tarEntry{Header: &tar.Header{Name: "bin", Typeflag: tar.TypeSymlink, Linkname: "usr/bin", Mode: 0o777}},
	)

	if err := extractTar(root, bytes.NewReader(stream)); err != nil {
		t.Fatalf("extractTar returned error: %v", err)
	}

	linkPath := filepath.Join(root, "bin")
	linkTarget, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("read symlink %s: %v", linkPath, err)
	}
	if got, want := linkTarget, "usr/bin"; got != want {
		t.Fatalf("unexpected symlink target: got %q want %q", got, want)
	}
}

func TestExtractTarAllowsAbsoluteInternalSymlinkTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stream := tarStreamWithEntries(t,
		tarEntry{Header: &tar.Header{Name: "run", Typeflag: tar.TypeDir, Mode: 0o755}},
		tarEntry{Header: &tar.Header{Name: "var", Typeflag: tar.TypeDir, Mode: 0o755}},
		tarEntry{Header: &tar.Header{Name: "var/run", Typeflag: tar.TypeSymlink, Linkname: "/run", Mode: 0o777}},
	)

	if err := extractTar(root, bytes.NewReader(stream)); err != nil {
		t.Fatalf("extractTar returned error: %v", err)
	}

	linkPath := filepath.Join(root, "var/run")
	linkTarget, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("read symlink %s: %v", linkPath, err)
	}
	if got, want := linkTarget, "/run"; got != want {
		t.Fatalf("unexpected symlink target: got %q want %q", got, want)
	}
}

func TestExtractTarRejectsRootFSContentOverLimit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stream := tarStreamWithEntries(t,
		tarEntry{Header: &tar.Header{Name: "large", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len("large"))}, Body: []byte("large")},
	)

	err := extractTarWithLimits(root, bytes.NewReader(stream), rootFSExtractionLimits{
		MaxContentBytes: 4,
		MaxEntries:      10,
	})
	if err == nil {
		t.Fatal("expected oversized rootfs tar to be rejected")
	}
	if _, statErr := os.Stat(filepath.Join(root, "large")); !os.IsNotExist(statErr) {
		t.Fatalf("expected oversized file not to be created, stat err=%v", statErr)
	}
}

func TestExtractTarRejectsTooManyEntries(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stream := tarStreamWithEntries(t,
		tarEntry{Header: &tar.Header{Name: "one", Typeflag: tar.TypeDir, Mode: 0o755}},
		tarEntry{Header: &tar.Header{Name: "two", Typeflag: tar.TypeDir, Mode: 0o755}},
	)

	err := extractTarWithLimits(root, bytes.NewReader(stream), rootFSExtractionLimits{
		MaxContentBytes: 1024,
		MaxEntries:      1,
	})
	if err == nil {
		t.Fatal("expected rootfs tar with too many entries to be rejected")
	}
}

func TestExtractTarCountsHardLinksAgainstContentLimit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stream := tarStreamWithEntries(t,
		tarEntry{Header: &tar.Header{Name: "base", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len("base"))}, Body: []byte("base")},
		tarEntry{Header: &tar.Header{Name: "base-link", Typeflag: tar.TypeLink, Linkname: "base", Mode: 0o644}},
	)

	err := extractTarWithLimits(root, bytes.NewReader(stream), rootFSExtractionLimits{
		MaxContentBytes: 7,
		MaxEntries:      10,
	})
	if err == nil {
		t.Fatal("expected hard link to count against rootfs content limit")
	}
	if _, statErr := os.Stat(filepath.Join(root, "base-link")); !os.IsNotExist(statErr) {
		t.Fatalf("expected rejected hard link not to be created, stat err=%v", statErr)
	}
}

type tarEntry struct {
	Header *tar.Header
	Body   []byte
}

func tarStreamWithEntries(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range entries {
		if err := tw.WriteHeader(entry.Header); err != nil {
			t.Fatalf("write tar header %q: %v", entry.Header.Name, err)
		}
		if len(entry.Body) > 0 {
			if _, err := tw.Write(entry.Body); err != nil {
				t.Fatalf("write tar body %q: %v", entry.Header.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar stream: %v", err)
	}
	return buf.Bytes()
}
