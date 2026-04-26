package backend

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSandboxFileUploadCommandAppliesMtime(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "artifact.txt")
	mtime := time.Unix(1700000123, 0)
	cmdArgs, err := SandboxFileUploadCommand(path, 0o640, mtime)
	if err != nil {
		t.Fatalf("SandboxFileUploadCommand returned error: %v", err)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = strings.NewReader("payload")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("upload command failed: %v\n%s", err, string(output))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if got, want := string(data), "payload"; got != want {
		t.Fatalf("unexpected uploaded payload: got %q want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat uploaded file: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o640); got != want {
		t.Fatalf("unexpected uploaded mode: got %04o want %04o", got, want)
	}
	if got, want := info.ModTime().Unix(), mtime.Unix(); got != want {
		t.Fatalf("unexpected uploaded mtime: got %d want %d", got, want)
	}
}

func TestSandboxFileUploadCommandRejectsDirectoryTarget(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "artifact-dir")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("create target directory: %v", err)
	}
	cmdArgs, err := SandboxFileUploadCommand(path, 0o640, time.Time{})
	if err != nil {
		t.Fatalf("SandboxFileUploadCommand returned error: %v", err)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = strings.NewReader("payload")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected upload command to reject directory target")
	}
	if !strings.Contains(string(output), "destination is a directory") {
		t.Fatalf("unexpected command output: %s", string(output))
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("read target directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected target directory to stay empty, found %d entries", len(entries))
	}
}
