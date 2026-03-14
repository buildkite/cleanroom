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

func writeRunObservability(t *testing.T, baseDir, runID string, observed map[string]any, modTime time.Time) string {
	t.Helper()

	runDir := filepath.Join(baseDir, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("create run dir: %v", err)
	}
	payload, err := json.Marshal(observed)
	if err != nil {
		t.Fatalf("marshal observability payload: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run-observability.json"), payload, 0o644); err != nil {
		t.Fatalf("write observability payload: %v", err)
	}
	if err := os.Chtimes(runDir, modTime, modTime); err != nil {
		t.Fatalf("set run dir modtime: %v", err)
	}
	return runDir
}

func TestStatusCommandRunListsRuns(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	baseDir, err := paths.RunBaseDir()
	if err != nil {
		t.Fatalf("resolve run base dir: %v", err)
	}
	writeRunObservability(t, baseDir, "run-a", map[string]any{"exit_code": 0}, statusTestBaseTime.Add(-time.Hour))
	writeRunObservability(t, baseDir, "run-b", map[string]any{"exit_code": 1}, statusTestBaseTime)

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err = (&StatusCommand{}).Run(&runtimeContext{Stdout: stdout})
	if err != nil {
		t.Fatalf("StatusCommand.Run returned error: %v", err)
	}

	output := readStdout()
	assertContainsAll(t, output, "runs in "+baseDir+":", "- run-a", "- run-b")
}

func TestStatusCommandRunInspectsRequestedRun(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	baseDir, err := paths.RunBaseDir()
	if err != nil {
		t.Fatalf("resolve run base dir: %v", err)
	}
	runDir := writeRunObservability(t, baseDir, "run-inspect", map[string]any{
		"exit_code": 0,
		"message":   "ok",
	}, statusTestBaseTime)

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err = (&StatusCommand{RunID: "run-inspect"}).Run(&runtimeContext{Stdout: stdout})
	if err != nil {
		t.Fatalf("StatusCommand.Run returned error: %v", err)
	}

	output := readStdout()
	assertContainsAll(t, output,
		"run: "+runDir,
		"observability ("+filepath.Join(runDir, "run-observability.json")+"):",
		"\"exit_code\": 0",
		"\"message\": \"ok\"",
	)
}

func TestStatusCommandRunLastRunChoosesNewest(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	baseDir, err := paths.RunBaseDir()
	if err != nil {
		t.Fatalf("resolve run base dir: %v", err)
	}
	oldDir := writeRunObservability(t, baseDir, "run-old", map[string]any{"exit_code": 1}, statusTestBaseTime.Add(-2*time.Hour))
	newDir := writeRunObservability(t, baseDir, "run-new", map[string]any{"exit_code": 0}, statusTestBaseTime)

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err = (&StatusCommand{LastRun: true}).Run(&runtimeContext{Stdout: stdout})
	if err != nil {
		t.Fatalf("StatusCommand.Run returned error: %v", err)
	}

	output := readStdout()
	if !strings.Contains(output, "run: "+newDir) {
		t.Fatalf("expected output to inspect newest run %q, got %q", newDir, output)
	}
	if strings.Contains(output, "run: "+oldDir) {
		t.Fatalf("expected output to avoid older run %q, got %q", oldDir, output)
	}
}
