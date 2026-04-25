package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const pinnedDefaultImage = "ghcr.io/buildkite/cleanroom-base/alpine@sha256:fe2fbe4950546c0983247d71d5ff5795b064d7e603596efc57e2ea88aaaf3cb1"

func TestBenchmarkTTIUsesRawSandboxCreateAndExecIn(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	fakeHyperfine := `#!/usr/bin/env python3
import json
import pathlib
import subprocess
import sys

args = sys.argv[1:]
prepare = None
cleanup = None
export_json = None
command = None
i = 0
while i < len(args):
    arg = args[i]
    if arg in ("--runs", "--warmup"):
        i += 2
    elif arg == "--prepare":
        prepare = args[i + 1]
        i += 2
    elif arg == "--cleanup":
        cleanup = args[i + 1]
        i += 2
    elif arg == "--export-json":
        export_json = args[i + 1]
        i += 2
    else:
        command = arg
        i += 1

if prepare:
    subprocess.run(prepare, shell=True, check=True)
subprocess.run(command, shell=True, check=True)
if cleanup:
    subprocess.run(cleanup, shell=True, check=True)

pathlib.Path(export_json).write_text(json.dumps({
    "results": [{
        "command": command,
        "mean": 0.001,
        "stddev": 0.0,
        "median": 0.001,
        "min": 0.001,
        "max": 0.001,
        "times": [0.001],
        "exit_codes": [0],
    }],
}))
`
	writeLocalExecutable(t, binDir, "hyperfine", fakeHyperfine)

	callLog := filepath.Join(tmpDir, "cleanroom-calls.jsonl")
	fakeCleanroom := `#!/usr/bin/env python3
import json
import os
import signal
import sys
import time

log_path = os.environ["FAKE_CLEANROOM_LOG"]
expected_image = os.environ["EXPECTED_CLEANROOM_IMAGE"]
argv = sys.argv[1:]
with open(log_path, "a") as f:
    f.write(json.dumps(argv) + "\n")

if argv and argv[0] == "serve":
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
    signal.signal(signal.SIGINT, lambda *_: sys.exit(0))
    while True:
        time.sleep(1)

if len(argv) >= 2 and argv[0] == "sandbox" and argv[1] == "ls":
    print("[]")
    sys.exit(0)

if len(argv) >= 2 and argv[0] == "sandbox" and argv[1] == "create":
    if "-c" in argv or "--chdir" in argv:
        print("sandbox create must not receive local chdir", file=sys.stderr)
        sys.exit(3)
    if "--backend" not in argv or "darwin-vz" not in argv:
        print("sandbox create missing backend", file=sys.stderr)
        sys.exit(3)
    if "--image" not in argv or expected_image not in argv:
        print("sandbox create missing expected pinned image", file=sys.stderr)
        sys.exit(3)
    print("cr_raw_001")
    sys.exit(0)

if len(argv) >= 2 and argv[0] == "sandbox" and argv[1] == "rm":
    print("sandbox terminated")
    sys.exit(0)

if argv and argv[0] == "exec":
    if "--in" not in argv or "cr_raw_001" not in argv:
        print("exec must reuse raw sandbox via --in", file=sys.stderr)
        sys.exit(3)
    if "--backend" in argv or "--keep" in argv or "--print-sandbox-id" in argv:
        print("exec should not create a top-level sandbox", file=sys.stderr)
        sys.exit(3)
    print("benchmark")
    sys.exit(0)

print("unexpected args: " + " ".join(argv), file=sys.stderr)
sys.exit(2)
`
	fakeCleanroomPath := writeLocalExecutable(t, binDir, "cleanroom", fakeCleanroom)

	outputDir := filepath.Join(tmpDir, "out")
	cmd := exec.Command(
		"bash",
		"benchmark-tti.sh",
		"--cleanroom-bin", fakeCleanroomPath,
		"--host", "unix://"+filepath.Join(tmpDir, "cleanroom.sock"),
		"--start-server",
		"--backend", "darwin-vz",
		"--iterations", "1",
		"--warmup", "0",
		"--output-dir", outputDir,
		"--chdir", filepath.Join(tmpDir, "must-not-be-used"),
	)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_CLEANROOM_LOG="+callLog,
		"EXPECTED_CLEANROOM_IMAGE="+pinnedDefaultImage,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("benchmark-tti.sh failed: %v\n%s", err, out)
	}

	rawLog, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	if !strings.Contains(string(rawLog), `"sandbox", "create"`) {
		t.Fatalf("expected raw sandbox create call, got:\n%s", rawLog)
	}
	if !strings.Contains(string(rawLog), `"exec", "--host"`) || !strings.Contains(string(rawLog), `"--in"`) {
		t.Fatalf("expected exec --in call, got:\n%s", rawLog)
	}
}

func TestBenchmarkTTIStopsWhenSandboxCreateFails(t *testing.T) {
	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	fakeHyperfine := `#!/usr/bin/env bash
set -euo pipefail
prepare=
cleanup=
command=
while [[ $# -gt 0 ]]; do
  case "$1" in
    --runs|--warmup|--export-json)
      shift 2
      ;;
    --prepare)
      prepare=$2
      shift 2
      ;;
    --cleanup)
      cleanup=$2
      shift 2
      ;;
    *)
      command=$1
      shift
      ;;
  esac
done
if [[ -n "$prepare" ]]; then
  bash -lc "$prepare"
fi
set +e
bash -lc "$command"
status=$?
set -e
if [[ -n "$cleanup" ]]; then
  bash -lc "$cleanup"
fi
exit "$status"
`
	writeLocalExecutable(t, binDir, "hyperfine", fakeHyperfine)

	callLog := filepath.Join(tmpDir, "cleanroom-calls.log")
	fakeCleanroom := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_CLEANROOM_LOG"
if [[ "${1:-}" == sandbox && "${2:-}" == create ]]; then
  echo "create failed" >&2
  exit 42
fi
if [[ "${1:-}" == exec ]]; then
  echo "exec should not run after create failure" >&2
  exit 0
fi
if [[ "${1:-}" == sandbox && "${2:-}" == rm ]]; then
  exit 0
fi
echo "unexpected args: $*" >&2
exit 2
`
	fakeCleanroomPath := writeLocalExecutable(t, binDir, "cleanroom", fakeCleanroom)

	cmd := exec.Command(
		"bash",
		"benchmark-tti.sh",
		"--cleanroom-bin", fakeCleanroomPath,
		"--host", "unix://"+filepath.Join(tmpDir, "cleanroom.sock"),
		"--backend", "darwin-vz",
		"--iterations", "1",
		"--warmup", "0",
		"--output-dir", filepath.Join(tmpDir, "out"),
	)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_CLEANROOM_LOG="+callLog,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected benchmark-tti.sh to fail when sandbox create fails\n%s", out)
	}

	rawLog, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	if strings.Contains(string(rawLog), "exec ") {
		t.Fatalf("exec should not run after sandbox create fails, got:\n%s", rawLog)
	}
}
