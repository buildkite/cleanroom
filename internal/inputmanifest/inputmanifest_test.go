package inputmanifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDigestIsDeterministicAcrossInputAndGlobOrder(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.lock"), "a")
	writeFile(t, filepath.Join(root, "b.lock"), "b")

	first, firstManifest, err := Digest(root, []string{"b.lock", "*.lock"})
	if err != nil {
		t.Fatalf("Digest returned error: %v", err)
	}
	second, secondManifest, err := Digest(root, []string{"*.lock", "a.lock"})
	if err != nil {
		t.Fatalf("Digest returned error: %v", err)
	}
	if first != second {
		t.Fatalf("digest changed across equivalent inputs: got %q want %q", second, first)
	}
	if got, want := len(firstManifest.Entries), 2; got != want {
		t.Fatalf("unexpected first manifest entry count: got %d want %d", got, want)
	}
	if got, want := len(secondManifest.Entries), 2; got != want {
		t.Fatalf("unexpected second manifest entry count: got %d want %d", got, want)
	}
	if got, want := firstManifest.Entries[0].Path, "a.lock"; got != want {
		t.Fatalf("entries are not sorted: got first path %q want %q", got, want)
	}
}

func TestDigestChangesWhenFileContentChanges(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "go.sum")
	writeFile(t, path, "before")

	before, _, err := Digest(root, []string{"go.sum"})
	if err != nil {
		t.Fatalf("Digest returned error: %v", err)
	}
	writeFile(t, path, "after")
	after, _, err := Digest(root, []string{"go.sum"})
	if err != nil {
		t.Fatalf("Digest returned error: %v", err)
	}
	if before == after {
		t.Fatalf("digest did not change after file content mutation: %q", before)
	}
}

func TestBuildRejectsNonRegularInputs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "locks"), 0o755); err != nil {
		t.Fatalf("Mkdir returned error: %v", err)
	}
	if err := os.Symlink("real.lock", filepath.Join(root, "link.lock")); err != nil {
		t.Fatalf("Symlink returned error: %v", err)
	}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "directory", input: "locks", want: "is a directory"},
		{name: "symlink", input: "link.lock", want: "is a symlink"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Build(root, []string{tc.input})
			if err == nil {
				t.Fatal("expected Build to reject non-regular input")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestBuildRejectsMissingLiteralAndEmptyGlob(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "missing literal", input: "go.sum", want: "does not exist"},
		{name: "empty glob", input: "*.lock", want: "matched no files"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Build(root, []string{tc.input})
			if err == nil {
				t.Fatal("expected Build to fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) returned error: %v", path, err)
	}
}
