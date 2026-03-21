package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/paths"
)

var statusTestBaseTime = time.Date(2026, time.March, 14, 10, 0, 0, 0, time.UTC)

func writeExecutionObservability(t *testing.T, baseDir, executionID string, observed map[string]any, modTime time.Time) string {
	t.Helper()

	artifactsDir := filepath.Join(baseDir, executionID)
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		t.Fatalf("create execution artifacts dir: %v", err)
	}
	payload, err := json.Marshal(observed)
	if err != nil {
		t.Fatalf("marshal observability payload: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifactsDir, "execution-observability.json"), payload, 0o644); err != nil {
		t.Fatalf("write observability payload: %v", err)
	}
	if err := os.Chtimes(artifactsDir, modTime, modTime); err != nil {
		t.Fatalf("set execution artifacts dir modtime: %v", err)
	}
	return artifactsDir
}

func TestStatusCommandRunListsExecutionsNewestFirst(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	baseDir, err := paths.ExecutionBaseDir()
	if err != nil {
		t.Fatalf("resolve execution base dir: %v", err)
	}
	writeExecutionObservability(t, baseDir, "exec-a", map[string]any{"exit_code": 0}, statusTestBaseTime.Add(-time.Hour))
	writeExecutionObservability(t, baseDir, "exec-b", map[string]any{"exit_code": 1}, statusTestBaseTime)

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err = (&StatusCommand{}).Run(&runtimeContext{Stdout: stdout})
	if err != nil {
		t.Fatalf("StatusCommand.Run returned error: %v", err)
	}

	output := readStdout()
	assertContainsAll(t, output, "retained executions in "+baseDir+":", "ID", "MODIFIED", "exec-a", "exec-b")
	if strings.Index(output, "exec-b") > strings.Index(output, "exec-a") {
		t.Fatalf("expected newest execution first, got %q", output)
	}
}

func TestStatusCommandRunInspectsRequestedExecution(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	baseDir, err := paths.ExecutionBaseDir()
	if err != nil {
		t.Fatalf("resolve execution base dir: %v", err)
	}
	artifactsDir := writeExecutionObservability(t, baseDir, "exec-inspect", map[string]any{
		"exit_code": 0,
		"message":   "ok",
	}, statusTestBaseTime)

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err = (&StatusCommand{ExecutionID: "exec-inspect"}).Run(&runtimeContext{Stdout: stdout})
	if err != nil {
		t.Fatalf("StatusCommand.Run returned error: %v", err)
	}

	output := readStdout()
	assertContainsAll(t, output,
		"execution artifacts: "+artifactsDir,
		"observability ("+filepath.Join(artifactsDir, "execution-observability.json")+"):",
		"\"exit_code\": 0",
		"\"message\": \"ok\"",
	)
}

func TestStatusCommandRunLastExecutionChoosesNewest(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	baseDir, err := paths.ExecutionBaseDir()
	if err != nil {
		t.Fatalf("resolve execution base dir: %v", err)
	}
	oldDir := writeExecutionObservability(t, baseDir, "exec-old", map[string]any{"exit_code": 1}, statusTestBaseTime.Add(-2*time.Hour))
	newDir := writeExecutionObservability(t, baseDir, "exec-new", map[string]any{"exit_code": 0}, statusTestBaseTime)

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err = (&StatusCommand{Last: true}).Run(&runtimeContext{Stdout: stdout})
	if err != nil {
		t.Fatalf("StatusCommand.Run returned error: %v", err)
	}

	output := readStdout()
	if !strings.Contains(output, "execution artifacts: "+newDir) {
		t.Fatalf("expected output to inspect newest execution artifacts %q, got %q", newDir, output)
	}
	if strings.Contains(output, "execution artifacts: "+oldDir) {
		t.Fatalf("expected output to avoid older execution artifacts %q, got %q", oldDir, output)
	}
}
