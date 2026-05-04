package controlservice

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/buildkite/cleanroom/internal/policy"
)

func TestBlockVolumeOutputResetCommandDoesNotFollowOutputDirSymlink(t *testing.T) {
	tempDir := t.TempDir()
	targetDir := filepath.Join(tempDir, "target")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("create target dir: %v", err)
	}
	targetFile := filepath.Join(targetDir, "keep")
	if err := os.WriteFile(targetFile, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write target file: %v", err)
	}
	outputDir := filepath.Join(tempDir, "output")
	if err := os.Symlink(targetDir, outputDir); err != nil {
		t.Fatalf("create output symlink: %v", err)
	}

	runBlockVolumeResetCommand(t, policy.StageBlockOutputs{Dirs: []string{outputDir}})

	if _, err := os.Lstat(outputDir); err != nil {
		t.Fatalf("stat reset output path: %v", err)
	}
	if info, err := os.Lstat(outputDir); err != nil {
		t.Fatalf("lstat reset output path: %v", err)
	} else if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("expected reset output path to be a directory, got symlink")
	}
	if data, err := os.ReadFile(targetFile); err != nil {
		t.Fatalf("read symlink target file after reset: %v", err)
	} else if got, want := string(data), "keep"; got != want {
		t.Fatalf("unexpected symlink target contents: got %q want %q", got, want)
	}
}

func TestBlockVolumeOutputResetCommandClearsOutputDirEntries(t *testing.T) {
	tempDir := t.TempDir()
	outputDir := filepath.Join(tempDir, "output")
	if err := os.MkdirAll(filepath.Join(outputDir, "nested"), 0o755); err != nil {
		t.Fatalf("create nested output dir: %v", err)
	}
	for _, path := range []string{
		filepath.Join(outputDir, "regular"),
		filepath.Join(outputDir, ".hidden"),
		filepath.Join(outputDir, "..prefixed"),
		filepath.Join(outputDir, "nested", "value"),
	} {
		if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
			t.Fatalf("write stale output %s: %v", path, err)
		}
	}

	runBlockVolumeResetCommand(t, policy.StageBlockOutputs{Dirs: []string{outputDir}})

	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatalf("read output dir after reset: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected output dir to be empty after reset, got %v", entries)
	}
}

func TestBlockVolumeOutputResetCommandRemovesWrongTypeFileOutput(t *testing.T) {
	tempDir := t.TempDir()
	outputFile := filepath.Join(tempDir, "file-output")
	if err := os.MkdirAll(outputFile, 0o755); err != nil {
		t.Fatalf("create wrong-type output dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputFile, "stale"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	runBlockVolumeResetCommand(t, policy.StageBlockOutputs{Files: []string{outputFile}})

	if _, err := os.Lstat(outputFile); !os.IsNotExist(err) {
		t.Fatalf("expected wrong-type file output path to be removed, got err=%v", err)
	}
}

func TestBlockVolumeOutputResetCommandRemovesFileOutputSymlinkWithoutFollowing(t *testing.T) {
	tempDir := t.TempDir()
	targetDir := filepath.Join(tempDir, "target")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("create target dir: %v", err)
	}
	targetFile := filepath.Join(targetDir, "keep")
	if err := os.WriteFile(targetFile, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write target file: %v", err)
	}
	outputFile := filepath.Join(tempDir, "file-output")
	if err := os.Symlink(targetDir, outputFile); err != nil {
		t.Fatalf("create output file symlink: %v", err)
	}

	runBlockVolumeResetCommand(t, policy.StageBlockOutputs{Files: []string{outputFile}})

	if _, err := os.Lstat(outputFile); !os.IsNotExist(err) {
		t.Fatalf("expected file output symlink to be removed, got err=%v", err)
	}
	if data, err := os.ReadFile(targetFile); err != nil {
		t.Fatalf("read symlink target file after reset: %v", err)
	} else if got, want := string(data), "keep"; got != want {
		t.Fatalf("unexpected symlink target contents: got %q want %q", got, want)
	}
}

func runBlockVolumeResetCommand(t *testing.T, outputs policy.StageBlockOutputs) {
	t.Helper()
	command := blockVolumeOutputResetCommand(outputs)
	if len(command) == 0 {
		t.Fatal("expected reset command")
	}
	cmd := exec.Command(command[0], command[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reset command failed: %v\n%s", err, out)
	}
}
