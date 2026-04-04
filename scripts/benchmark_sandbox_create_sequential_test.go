package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeLocalExecutable(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestBenchmarkSandboxCreateSequentialScript(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}

	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	fakeStateFile := filepath.Join(tmpDir, "fake-cleanroom-state.json")
	fakeStateDir := filepath.Join(tmpDir, "state", "cleanroom")
	fakeZFSFile := filepath.Join(tmpDir, "fake-zfs-datasets.txt")
	fakeCleanroom := `#!/usr/bin/env python3
import datetime
import json
import os
import pathlib
import sys

state_file = pathlib.Path(os.environ["FAKE_STATE_FILE"])
state_dir = pathlib.Path(os.environ["FAKE_STATE_DIR"])
zfs_file = pathlib.Path(os.environ["FAKE_ZFS_FILE"])
dataset_root = "tank/cleanroom"

def load_state():
    if state_file.exists():
        return json.loads(state_file.read_text())
    return {"counter": 0, "sandboxes": {}}

def save_state(state):
    state_file.parent.mkdir(parents=True, exist_ok=True)
    state_file.write_text(json.dumps(state))

def load_datasets():
    if not zfs_file.exists():
        return [dataset_root, dataset_root + "/sandboxes"]
    items = [line.strip() for line in zfs_file.read_text().splitlines() if line.strip()]
    if dataset_root not in items:
        items.insert(0, dataset_root)
    if dataset_root + "/sandboxes" not in items:
        items.append(dataset_root + "/sandboxes")
    return items

def save_datasets(items):
    zfs_file.parent.mkdir(parents=True, exist_ok=True)
    zfs_file.write_text("\n".join(items) + "\n")

def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")

argv = sys.argv[1:]
if argv[:2] == ["doctor", "--json"] or (len(argv) >= 2 and argv[0] == "doctor" and "--json" in argv):
    payload = {
        "snapshot": {"enabled": True, "driver": "zfs", "base_dir": str(state_dir / "snapshots")},
        "checks": [
            {"name": "snapshot_zfs_dataset", "status": "pass", "message": 'configured zfs dataset "tank/cleanroom"'},
        ],
    }
    print(json.dumps(payload))
    raise SystemExit(0)

if len(argv) >= 2 and argv[0] == "sandbox" and argv[1] == "create":
    state = load_state()
    state["counter"] += 1
    sid = f"cr-bench-{state['counter']:04d}"
    payload = {
        "sandbox_id": sid,
        "status": "ready",
        "backend": "firecracker",
        "created_at": now(),
        "updated_at": now(),
    }
    state["sandboxes"][sid] = payload | {"status": 4}
    save_state(state)
    run_dir = state_dir / "sandboxes" / sid
    run_dir.mkdir(parents=True, exist_ok=True)
    datasets = load_datasets()
    dataset = dataset_root + "/sandboxes/" + sid
    if dataset not in datasets:
        datasets.append(dataset)
    save_datasets(datasets)
    print(json.dumps(payload))
    raise SystemExit(0)

if len(argv) >= 2 and argv[0] == "sandbox" and argv[1] == "rm":
    sid = argv[-1]
    state = load_state()
    sandbox = state["sandboxes"].setdefault(sid, {"sandbox_id": sid})
    sandbox["status"] = 4
    save_state(state)
    run_dir = state_dir / "sandboxes" / sid
    if run_dir.exists():
        for child in run_dir.iterdir():
            if child.is_file() or child.is_symlink():
                child.unlink()
        run_dir.rmdir()
    datasets = [item for item in load_datasets() if item != dataset_root + "/sandboxes/" + sid]
    save_datasets(datasets)
    print("sandbox terminated")
    raise SystemExit(0)

if len(argv) >= 2 and argv[0] == "sandbox" and argv[1] == "ls":
    state = load_state()
    print(json.dumps(list(state["sandboxes"].values())))
    raise SystemExit(0)

print("unexpected args: " + " ".join(argv), file=sys.stderr)
raise SystemExit(2)
`
	fakeZFS := `#!/usr/bin/env python3
import os
import pathlib
import sys

datasets_file = pathlib.Path(os.environ["FAKE_ZFS_FILE"])
items = []
if datasets_file.exists():
    items = [line.strip() for line in datasets_file.read_text().splitlines() if line.strip()]

argv = sys.argv[1:]
if len(argv) == 6 and argv[:5] == ["list", "-H", "-o", "name", "-r"]:
    root = argv[5]
    for item in items:
        if item == root or item.startswith(root + "/"):
            print(item)
    raise SystemExit(0)

print("unexpected zfs args: " + " ".join(argv), file=sys.stderr)
raise SystemExit(2)
`
	writeLocalExecutable(t, binDir, "cleanroom", fakeCleanroom)
	writeLocalExecutable(t, binDir, "zfs", fakeZFS)

	outputDir := filepath.Join(tmpDir, "out")
	scriptPath, err := filepath.Abs("benchmark-sandbox-create-sequential.py")
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	cmd := exec.Command(
		python,
		scriptPath,
		"--cleanroom-bin", filepath.Join(binDir, "cleanroom"),
		"--iterations", "3",
		"--warmup", "1",
		"--output-dir", outputDir,
		"--state-dir", fakeStateDir,
		"--zfs-dataset", "tank/cleanroom",
	)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_STATE_FILE="+fakeStateFile,
		"FAKE_STATE_DIR="+fakeStateDir,
		"FAKE_ZFS_FILE="+fakeZFSFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("benchmark script failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "cleanup: pass") {
		t.Fatalf("expected cleanup success in output, got:\n%s", out)
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
		Config    struct {
			Iterations int `json:"iterations"`
			Warmup     int `json:"warmup"`
		} `json:"config"`
		ObservedBackends []string         `json:"observed_backends"`
		Warmup           map[string]any   `json:"warmup"`
		Runs             []map[string]any `json:"runs"`
		Cleanup          struct {
			Passed                 bool     `json:"passed"`
			RetainedStoppedIDs     []string `json:"control_plane_retained_stopped_ids"`
			ActiveLeftovers        []any    `json:"active_leftovers"`
			RunDirsPresent         []string `json:"run_dirs_present"`
			ZFSDatasetsPresent     []string `json:"zfs_datasets_present"`
			ControlPlaneMissingIDs []string `json:"control_plane_missing_ids"`
			ControlPlaneNonStopped []any    `json:"control_plane_non_stopped_leftovers"`
		} `json:"cleanup"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal output json: %v", err)
	}

	if got, want := payload.Benchmark, "sandbox-create-sequential"; got != want {
		t.Fatalf("unexpected benchmark: got %q want %q", got, want)
	}
	if got, want := payload.Config.Iterations, 3; got != want {
		t.Fatalf("unexpected iterations: got %d want %d", got, want)
	}
	if got, want := payload.Config.Warmup, 1; got != want {
		t.Fatalf("unexpected warmup: got %d want %d", got, want)
	}
	if payload.Warmup == nil {
		t.Fatal("expected warmup record")
	}
	if got, want := len(payload.Runs), 3; got != want {
		t.Fatalf("unexpected run count: got %d want %d", got, want)
	}
	if got, want := len(payload.Cleanup.RetainedStoppedIDs), 4; got != want {
		t.Fatalf("unexpected retained stopped ids: got %d want %d", got, want)
	}
	if !payload.Cleanup.Passed {
		t.Fatalf("expected cleanup passed, got %+v", payload.Cleanup)
	}
	if len(payload.Cleanup.ActiveLeftovers) != 0 {
		t.Fatalf("expected no active leftovers, got %+v", payload.Cleanup.ActiveLeftovers)
	}
	if len(payload.Cleanup.RunDirsPresent) != 0 {
		t.Fatalf("expected no run dirs present, got %+v", payload.Cleanup.RunDirsPresent)
	}
	if len(payload.Cleanup.ZFSDatasetsPresent) != 0 {
		t.Fatalf("expected no zfs leftovers, got %+v", payload.Cleanup.ZFSDatasetsPresent)
	}
	if len(payload.Cleanup.ControlPlaneMissingIDs) != 0 {
		t.Fatalf("expected no missing ids, got %+v", payload.Cleanup.ControlPlaneMissingIDs)
	}
	if len(payload.Cleanup.ControlPlaneNonStopped) != 0 {
		t.Fatalf("expected no non-stopped leftovers, got %+v", payload.Cleanup.ControlPlaneNonStopped)
	}
}
