package overlaycapture

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestScanAllowsDeclaredFileOutputsAndAncestors(t *testing.T) {
	t.Parallel()

	upperDir := t.TempDir()
	writeFile(t, upperDir, "usr/local/bin/tool")
	mkdir(t, upperDir, "root/.local/share/mise")

	result, err := Scan(upperDir, Options{
		BaselinePaths:       []string{"/root", "/root/.local", "/root/.local/share", "/root/.local/share/mise"},
		DeclaredFileOutputs: []string{"/usr/local/bin/tool"},
	})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if len(result.EscapedWrites) != 0 {
		t.Fatalf("unexpected escaped writes: %#v", result.EscapedWrites)
	}
	assertEntry(t, result.Entries, Entry{Path: "/usr/local/bin/tool", Kind: EntryKindWrite, Mode: 0o644})
}

func TestScanReportsEscapedWrites(t *testing.T) {
	t.Parallel()

	upperDir := t.TempDir()
	writeFile(t, upperDir, "workspace/dist/result.txt")
	writeFile(t, upperDir, "etc/profile")

	result, err := Scan(upperDir, Options{
		DeclaredFileOutputs: []string{"/workspace/dist/result.txt"},
	})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	assertEntry(t, result.EscapedWrites, Entry{Path: "/etc/profile", Kind: EntryKindWrite, Mode: 0o644})
	if hasEntry(result.EscapedWrites, "/workspace/dist/result.txt", EntryKindWrite) {
		t.Fatalf("declared file output reported as escaped: %#v", result.EscapedWrites)
	}
}

func TestScanIgnoresScratchPrefixWithoutIgnoringSibling(t *testing.T) {
	t.Parallel()

	upperDir := t.TempDir()
	writeFile(t, upperDir, "tmp/cache")
	writeFile(t, upperDir, "tmpish/cache")

	result, err := Scan(upperDir, Options{
		IgnoredPrefixes: []string{"/tmp"},
	})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	if hasEntry(result.EscapedWrites, "/tmp/cache", EntryKindWrite) {
		t.Fatalf("ignored scratch write reported as escaped: %#v", result.EscapedWrites)
	}
	assertEntry(t, result.EscapedWrites, Entry{Path: "/tmpish/cache", Kind: EntryKindWrite, Mode: 0o644})
}

func TestScanDoesNotTreatRegularWhiteoutNamedFilesAsDeletes(t *testing.T) {
	t.Parallel()

	upperDir := t.TempDir()
	writeFile(t, upperDir, "home/user/.wh.deleted")
	writeFile(t, upperDir, "var/lib/app/.wh..wh..opq")

	result, err := Scan(upperDir, Options{})
	if err != nil {
		t.Fatalf("Scan returned error: %v", err)
	}
	assertEntry(t, result.Entries, Entry{Path: "/home/user/.wh.deleted", Kind: EntryKindWrite})
	assertEntry(t, result.EscapedWrites, Entry{Path: "/home/user/.wh.deleted", Kind: EntryKindWrite})
	assertEntry(t, result.Entries, Entry{Path: "/var/lib/app/.wh..wh..opq", Kind: EntryKindWrite})
	assertEntry(t, result.EscapedWrites, Entry{Path: "/var/lib/app/.wh..wh..opq", Kind: EntryKindWrite})
}

func TestScanDoesNotAllowDeleteOfBaselinePath(t *testing.T) {
	t.Parallel()

	filter, err := newFilter(Options{
		BaselinePaths: []string{"/mnt", "/mnt/cache"},
	})
	if err != nil {
		t.Fatalf("newFilter returned error: %v", err)
	}
	if filter.allowed(Entry{Path: "/mnt/cache", Kind: EntryKindDelete}) {
		t.Fatal("baseline delete was allowed")
	}
}

func TestScanRejectsEscapingOptions(t *testing.T) {
	t.Parallel()

	_, err := Scan(t.TempDir(), Options{
		DeclaredFileOutputs: []string{"../result"},
	})
	if err == nil {
		t.Fatal("expected relative declared file output to fail")
	}
}

func TestScanRejectsRootIgnoredPrefix(t *testing.T) {
	t.Parallel()

	_, err := Scan(t.TempDir(), Options{
		IgnoredPrefixes: []string{"/"},
	})
	if err == nil {
		t.Fatal("expected root ignored prefix to fail")
	}
}

func writeFile(t *testing.T, root, rel string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(rel), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func mkdir(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatalf("create %s: %v", rel, err)
	}
}

func assertEntry(t *testing.T, entries []Entry, want Entry) {
	t.Helper()
	if slices.ContainsFunc(entries, func(got Entry) bool {
		return got.Path == want.Path && got.Kind == want.Kind && (want.Mode == 0 || got.Mode.Perm() == want.Mode.Perm())
	}) {
		return
	}
	t.Fatalf("missing entry %#v in %#v", want, entries)
}

func hasEntry(entries []Entry, path string, kind EntryKind) bool {
	return slices.ContainsFunc(entries, func(entry Entry) bool {
		return entry.Path == path && entry.Kind == kind
	})
}
