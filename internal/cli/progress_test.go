package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func captureSandboxProgressOutput(t *testing.T, fn func(*os.File) error) (string, error) {
	t.Helper()

	tmpDir := t.TempDir()
	stderrPath := filepath.Join(tmpDir, "stderr.log")
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		t.Fatalf("create stderr capture file: %v", err)
	}
	defer stderrFile.Close()

	runErr := fn(stderrFile)

	if err := stderrFile.Sync(); err != nil {
		t.Fatalf("sync stderr capture file: %v", err)
	}

	output, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatalf("read stderr capture file: %v", err)
	}
	return string(output), runErr
}

func forceSandboxProgressTTY(t *testing.T) {
	t.Helper()

	oldIsTerminal := isTerminalFunc
	oldDelay := sandboxProgressStartDelay
	oldTick := sandboxProgressTickInterval

	isTerminalFunc = func(int) bool { return true }
	sandboxProgressStartDelay = 5 * time.Millisecond
	sandboxProgressTickInterval = 5 * time.Millisecond

	t.Cleanup(func() {
		isTerminalFunc = oldIsTerminal
		sandboxProgressStartDelay = oldDelay
		sandboxProgressTickInterval = oldTick
	})
}

func TestWithSandboxProgressFailurePrintsFailure(t *testing.T) {
	forceSandboxProgressTTY(t)

	wantErr := errors.New("response missing sandbox id")
	output, err := captureSandboxProgressOutput(t, func(stderr *os.File) error {
		return withSandboxProgress(stderr, func(_ *sandboxProgress) error {
			time.Sleep(25 * time.Millisecond)
			return wantErr
		})
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected error %v, got %v", wantErr, err)
	}
	if !strings.Contains(output, "Sandbox creation failed in") {
		t.Fatalf("expected failure progress output, got %q", output)
	}
	if strings.Contains(output, "Sandbox ready in") {
		t.Fatalf("did not expect success progress output, got %q", output)
	}
}

func TestWithSandboxProgressNoColorAvoidsANSIControlSequences(t *testing.T) {
	forceSandboxProgressTTY(t)
	t.Setenv("NO_COLOR", "1")

	output, err := captureSandboxProgressOutput(t, func(stderr *os.File) error {
		return withSandboxProgress(stderr, func(_ *sandboxProgress) error {
			time.Sleep(25 * time.Millisecond)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("withSandboxProgress returned error: %v", err)
	}
	if !strings.Contains(output, "Preparing sandbox (first use may take a bit)...") {
		t.Fatalf("expected plain progress start output, got %q", output)
	}
	if !strings.Contains(output, "Sandbox ready in") {
		t.Fatalf("expected plain progress completion output, got %q", output)
	}
	if strings.Contains(output, "\x1b[") {
		t.Fatalf("did not expect ANSI escapes in no-color mode, got %q", output)
	}
	if strings.Contains(output, "\r") {
		t.Fatalf("did not expect carriage-return rewriting in no-color mode, got %q", output)
	}
}

func TestWithSandboxProgressSuppressStopsFramesBeforeStreamingOutput(t *testing.T) {
	forceSandboxProgressTTY(t)
	oldDelay := sandboxProgressStartDelay
	sandboxProgressStartDelay = 50 * time.Millisecond
	t.Cleanup(func() {
		sandboxProgressStartDelay = oldDelay
	})

	output, err := captureSandboxProgressOutput(t, func(stderr *os.File) error {
		return withSandboxProgress(stderr, func(progress *sandboxProgress) error {
			progress.suppress()
			if _, err := stderr.WriteString("Cloning into '/workspace'...\n"); err != nil {
				return err
			}
			time.Sleep(10 * time.Millisecond)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("withSandboxProgress returned error: %v", err)
	}
	idx := strings.Index(output, "Cloning into '/workspace'...")
	if idx == -1 {
		t.Fatalf("expected streamed output, got %q", output)
	}
	if strings.Contains(output[idx:], "Preparing sandbox") {
		t.Fatalf("did not expect spinner frames after streamed output starts, got %q", output)
	}
	if !strings.Contains(output, "Sandbox ready in") {
		t.Fatalf("expected completion message, got %q", output)
	}
}

func TestWriteSandboxProgressFrameClearsTheLine(t *testing.T) {
	tmpDir := t.TempDir()
	stderrPath := filepath.Join(tmpDir, "stderr.log")
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		t.Fatalf("create stderr capture file: %v", err)
	}
	defer stderrFile.Close()

	writeSandboxProgressFrame(stderrFile, "-", 950*time.Millisecond)
	writeSandboxProgressFrame(stderrFile, "\\", 1100*time.Millisecond)

	if err := stderrFile.Sync(); err != nil {
		t.Fatalf("sync stderr capture file: %v", err)
	}

	output, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatalf("read stderr capture file: %v", err)
	}
	if got := strings.Count(string(output), "\r\x1b[2K"); got != 2 {
		t.Fatalf("expected each frame to clear the line, got %q", string(output))
	}
}
