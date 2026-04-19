package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBenchmarkRepositoryBootstrapTool(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	scenarios := []struct {
		name       string
		hasSeedRun bool
	}{
		{name: "cold-host", hasSeedRun: false},
		{name: "warm-repository-store", hasSeedRun: true},
		{name: "warm-workspace-stage", hasSeedRun: true},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			binDir := filepath.Join(tmpDir, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatalf("mkdir bin: %v", err)
			}

			fakeCleanroom := `#!/usr/bin/env python3
import json
import os
import pathlib
import signal
import sys
import time

state_home = pathlib.Path(os.environ["XDG_STATE_HOME"])
state_file = state_home / "fake-cleanroom-state.json"

def load_state():
    if state_file.exists():
        return json.loads(state_file.read_text())
    return {"counter": 0}

def save_state(state):
    state_file.parent.mkdir(parents=True, exist_ok=True)
    state_file.write_text(json.dumps(state))

def exit_now(*_args):
    raise SystemExit(0)

argv = sys.argv[1:]
if argv and argv[0] == "serve":
    signal.signal(signal.SIGTERM, exit_now)
    signal.signal(signal.SIGINT, exit_now)
    while True:
        time.sleep(1)

if len(argv) >= 2 and argv[0] == "sandbox" and argv[1] == "ls":
    print("[]")
    raise SystemExit(0)

if len(argv) >= 2 and argv[0] == "sandbox" and argv[1] == "rm":
    print("sandbox terminated")
    raise SystemExit(0)

if argv and argv[0] == "create":
    state = load_state()
    state["counter"] += 1
    save_state(state)
    sid = f"cr-bench-{state['counter']:04d}"
    print(json.dumps({
        "sandboxId": sid,
        "backend": "firecracker"
    }))
    raise SystemExit(0)

print("unexpected args: " + " ".join(argv), file=sys.stderr)
raise SystemExit(2)
`
			writeLocalExecutable(t, binDir, "cleanroom", fakeCleanroom)

			outputDir := filepath.Join(tmpDir, "out")
			toolPath, err := filepath.Abs("benchmark_repository_bootstrap")
			if err != nil {
				t.Fatalf("resolve tool path: %v", err)
			}

			cmd := exec.Command(
				goBin,
				"run",
				toolPath,
				"--cleanroom-bin", filepath.Join(binDir, "cleanroom"),
				"--scenario", scenario.name,
				"--iterations", "2",
				"--warmup", "1",
				"--output-dir", outputDir,
				"--chdir", tmpDir,
			)
			cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("benchmark tool failed: %v\n%s", err, out)
			}

			matches, err := filepath.Glob(filepath.Join(outputDir, "*.json"))
			if err != nil {
				t.Fatalf("glob output: %v", err)
			}
			if len(matches) != 1 {
				t.Fatalf("expected one output json file, got %d", len(matches))
			}

			raw, err := os.ReadFile(matches[0])
			if err != nil {
				t.Fatalf("read output json: %v", err)
			}

			var payload struct {
				Benchmark string `json:"benchmark"`
				Scenario  string `json:"scenario"`
				Config    struct {
					Iterations int    `json:"iterations"`
					Warmup     int    `json:"warmup"`
					Backend    string `json:"backend"`
				} `json:"config"`
				Seed       *map[string]any  `json:"seed"`
				WarmupRuns []map[string]any `json:"warmup_runs"`
				Runs       []struct {
					Label   string  `json:"label"`
					Backend string  `json:"backend"`
					Elapsed float64 `json:"elapsed_seconds"`
				} `json:"runs"`
				Summary struct {
					Mean   float64 `json:"mean"`
					Median float64 `json:"median"`
					Min    float64 `json:"min"`
					Max    float64 `json:"max"`
				} `json:"summary"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("unmarshal output json: %v", err)
			}

			if got, want := payload.Benchmark, "repository-bootstrap"; got != want {
				t.Fatalf("unexpected benchmark: got %q want %q", got, want)
			}
			if got, want := payload.Scenario, scenario.name; got != want {
				t.Fatalf("unexpected scenario: got %q want %q", got, want)
			}
			if got, want := payload.Config.Iterations, 2; got != want {
				t.Fatalf("unexpected iterations: got %d want %d", got, want)
			}
			if got, want := payload.Config.Warmup, 1; got != want {
				t.Fatalf("unexpected warmup: got %d want %d", got, want)
			}
			if got, want := len(payload.WarmupRuns), 1; got != want {
				t.Fatalf("unexpected warmup run count: got %d want %d", got, want)
			}
			if got, want := len(payload.Runs), 2; got != want {
				t.Fatalf("unexpected measured run count: got %d want %d", got, want)
			}
			if scenario.hasSeedRun && payload.Seed == nil {
				t.Fatal("expected seed run metadata")
			}
			if !scenario.hasSeedRun && payload.Seed != nil {
				t.Fatalf("expected no seed metadata, got %+v", payload.Seed)
			}
			for _, run := range payload.Runs {
				if run.Label == "" {
					t.Fatal("expected run label")
				}
				if got, want := run.Backend, "firecracker"; got != want {
					t.Fatalf("unexpected run backend: got %q want %q", got, want)
				}
				if run.Elapsed < 0 {
					t.Fatalf("expected non-negative elapsed time, got %f", run.Elapsed)
				}
			}
			if payload.Summary.Mean < 0 || payload.Summary.Median < 0 || payload.Summary.Min < 0 || payload.Summary.Max < 0 {
				t.Fatalf("expected non-negative summary, got %+v", payload.Summary)
			}
		})
	}
}
