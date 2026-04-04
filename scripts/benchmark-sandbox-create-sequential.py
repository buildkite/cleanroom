#!/usr/bin/env python3

from __future__ import annotations

import argparse
import json
import os
import pathlib
import re
import shutil
import statistics
import subprocess
import sys
import time
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from typing import Any


def default_host() -> str:
    runtime_dir = os.environ.get("XDG_RUNTIME_DIR", "").strip()
    if runtime_dir:
        return f"unix://{runtime_dir}/cleanroom/cleanroom.sock"
    return "unix:///tmp/cleanroom/cleanroom.sock"


def default_state_dir() -> pathlib.Path:
    state_home = os.environ.get("XDG_STATE_HOME", "").strip()
    if state_home:
        return pathlib.Path(state_home) / "cleanroom"
    return pathlib.Path.home() / ".local" / "state" / "cleanroom"


def default_cleanroom_bin() -> str:
    resolved = shutil.which("cleanroom")
    if resolved:
        return resolved
    dist_bin = pathlib.Path("dist/cleanroom")
    if dist_bin.is_file() and os.access(dist_bin, os.X_OK):
        return str(dist_bin)
    return "cleanroom"


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Create sandboxes sequentially after a warmup run, measure create and "
            "terminate latency, and verify host cleanup."
        )
    )
    parser.add_argument(
        "--host",
        default=default_host(),
        help=(
            "Control-plane endpoint. Full cleanup verification assumes this is the "
            "local host control plane."
        ),
    )
    parser.add_argument(
        "--backend",
        default="",
        help="Optional backend override for sandbox creation.",
    )
    parser.add_argument(
        "--image",
        default="",
        help="Optional sandbox image override.",
    )
    parser.add_argument(
        "-n",
        "--iterations",
        type=int,
        default=100,
        help="Measured sandbox create/terminate iterations (default: 100).",
    )
    parser.add_argument(
        "--warmup",
        type=int,
        default=1,
        help="Warmup create/terminate runs before measurement (default: 1).",
    )
    parser.add_argument(
        "--launch-seconds",
        type=int,
        default=0,
        help="Optional launch timeout override passed to sandbox create.",
    )
    parser.add_argument(
        "--state-dir",
        default=str(default_state_dir()),
        help="Host cleanroom state directory used for run-dir cleanup checks.",
    )
    parser.add_argument(
        "--zfs-dataset",
        default="",
        help=(
            "Optional ZFS dataset root for cleanup verification. If omitted and the "
            "script is run on the host, it is auto-detected from cleanroom doctor "
            "when possible."
        ),
    )
    parser.add_argument(
        "--output-dir",
        default="benchmarks/results",
        help="Directory for the benchmark JSON report.",
    )
    parser.add_argument(
        "--cleanroom-bin",
        default=default_cleanroom_bin(),
        help="Path to the cleanroom binary.",
    )
    return parser.parse_args()


class CommandError(RuntimeError):
    def __init__(self, argv: list[str], returncode: int, stdout: str, stderr: str) -> None:
        self.argv = argv
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr
        joined = " ".join(argv)
        super().__init__(f"command failed ({returncode}): {joined}")


@dataclass
class RunRecord:
    kind: str
    iteration: int
    sandbox_id: str
    backend: str
    status: str
    create_ms: float
    terminate_ms: float
    total_ms: float
    service_create_ms: float | None


def run_command(argv: list[str]) -> subprocess.CompletedProcess[str]:
    proc = subprocess.run(argv, capture_output=True, text=True)
    if proc.returncode != 0:
        raise CommandError(argv, proc.returncode, proc.stdout, proc.stderr)
    return proc


def run_timed(argv: list[str]) -> tuple[subprocess.CompletedProcess[str], float]:
    started = time.perf_counter_ns()
    proc = run_command(argv)
    finished = time.perf_counter_ns()
    return proc, (finished - started) / 1_000_000.0


def cleanroom_cmd(args: argparse.Namespace, *subcommand: str) -> list[str]:
    return [args.cleanroom_bin, *subcommand]


def sandbox_create_cmd(args: argparse.Namespace) -> list[str]:
    cmd = cleanroom_cmd(args, "sandbox", "create", "--host", args.host, "--json")
    if args.backend:
        cmd.extend(["--backend", args.backend])
    if args.image:
        cmd.extend(["--image", args.image])
    if args.launch_seconds > 0:
        cmd.extend(["--launch-seconds", str(args.launch_seconds)])
    return cmd


def sandbox_rm_cmd(args: argparse.Namespace, sandbox_id: str) -> list[str]:
    return cleanroom_cmd(args, "sandbox", "rm", "--host", args.host, sandbox_id)


def sandbox_ls_cmd(args: argparse.Namespace) -> list[str]:
    return cleanroom_cmd(args, "sandbox", "ls", "--host", args.host, "--all", "--json")


def doctor_cmd(args: argparse.Namespace) -> list[str]:
    cmd = cleanroom_cmd(args, "doctor", "--json")
    if args.backend:
        cmd.extend(["--backend", args.backend])
    return cmd


def parse_json_output(raw: str, context: str) -> Any:
    try:
        return json.loads(raw)
    except json.JSONDecodeError as exc:
        raise RuntimeError(f"{context}: expected JSON output, got: {raw!r}") from exc


def normalize_status(value: Any) -> str:
    if isinstance(value, bool):
        return str(value).lower()
    if isinstance(value, int):
        status_by_code = {
            0: "unspecified",
            1: "provisioning",
            2: "ready",
            3: "stopping",
            4: "stopped",
            5: "failed",
        }
        return status_by_code.get(value, str(value))
    if isinstance(value, str):
        text = value.strip().lower()
        if text.startswith("sandbox_status_"):
            text = text[len("sandbox_status_") :]
        return text
    return str(value)


def parse_rfc3339(value: Any) -> datetime | None:
    if not isinstance(value, str):
        return None
    text = value.strip()
    if not text:
        return None
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        return datetime.fromisoformat(text)
    except ValueError:
        return None


def service_create_ms(payload: dict[str, Any]) -> float | None:
    created = parse_rfc3339(payload.get("created_at"))
    updated = parse_rfc3339(payload.get("updated_at"))
    if created is None or updated is None:
        return None
    return (updated - created).total_seconds() * 1000.0


def percentile(sorted_values: list[float], pct: float) -> float:
    if not sorted_values:
        raise ValueError("percentile requires at least one value")
    if len(sorted_values) == 1:
        return sorted_values[0]
    rank = (len(sorted_values) - 1) * pct
    lower = int(rank)
    upper = min(lower + 1, len(sorted_values) - 1)
    weight = rank - lower
    return sorted_values[lower] * (1.0 - weight) + sorted_values[upper] * weight


def summarize(values: list[float]) -> dict[str, float | int]:
    ordered = sorted(values)
    summary: dict[str, float | int] = {
        "count": len(values),
        "mean": statistics.fmean(values),
        "min": ordered[0],
        "max": ordered[-1],
        "p50": percentile(ordered, 0.50),
        "p95": percentile(ordered, 0.95),
        "p99": percentile(ordered, 0.99),
    }
    if len(values) > 1:
        summary["stdev"] = statistics.stdev(values)
    return summary


def is_local_host(host: str) -> bool:
    return host.startswith("unix://") or host.startswith("http://localhost") or host.startswith("http://127.0.0.1")


def detect_zfs_dataset(args: argparse.Namespace) -> str | None:
    if args.zfs_dataset:
        return args.zfs_dataset
    if not is_local_host(args.host):
        return None

    try:
        payload = parse_json_output(run_command(doctor_cmd(args)).stdout, "cleanroom doctor")
    except Exception:
        return None

    snapshot = payload.get("snapshot")
    if not isinstance(snapshot, dict):
        return None
    if str(snapshot.get("driver", "")).strip().lower() != "zfs":
        return None

    checks = payload.get("checks")
    if not isinstance(checks, list):
        return None
    for check in checks:
        if not isinstance(check, dict):
            continue
        if check.get("name") != "snapshot_zfs_dataset":
            continue
        match = re.search(r'"([^"]+)"', str(check.get("message", "")))
        if match:
            return match.group(1)
    return None


def list_zfs_datasets(dataset_root: str) -> list[str]:
    proc = subprocess.run(
        ["zfs", "list", "-H", "-o", "name", "-r", dataset_root],
        capture_output=True,
        text=True,
    )
    if proc.returncode != 0:
        stderr = proc.stderr.strip()
        raise RuntimeError(f"zfs list failed for {dataset_root}: {stderr or proc.stdout.strip()}")
    return [line.strip() for line in proc.stdout.splitlines() if line.strip()]


def cleanup_report(
    args: argparse.Namespace,
    sandbox_ids: list[str],
    zfs_dataset: str | None,
) -> dict[str, Any]:
    listed_raw = run_command(sandbox_ls_cmd(args)).stdout
    listed_payload = parse_json_output(listed_raw, "cleanroom sandbox ls")
    if not isinstance(listed_payload, list):
        raise RuntimeError("cleanroom sandbox ls: expected a JSON array")

    by_id: dict[str, dict[str, Any]] = {}
    for item in listed_payload:
        if not isinstance(item, dict):
            continue
        sandbox_id = str(item.get("sandbox_id", "")).strip()
        if sandbox_id:
            by_id[sandbox_id] = item

    retained_stopped: list[str] = []
    control_plane_missing: list[str] = []
    active_leftovers: list[dict[str, str]] = []
    non_stopped_leftovers: list[dict[str, str]] = []
    for sandbox_id in sandbox_ids:
        item = by_id.get(sandbox_id)
        if item is None:
            control_plane_missing.append(sandbox_id)
            continue
        status = normalize_status(item.get("status"))
        if status == "stopped":
            retained_stopped.append(sandbox_id)
            continue
        leftover = {"sandbox_id": sandbox_id, "status": status}
        non_stopped_leftovers.append(leftover)
        if status not in {"failed", "unknown"}:
            active_leftovers.append(leftover)

    run_dirs_present = []
    if is_local_host(args.host):
        sandboxes_dir = pathlib.Path(args.state_dir) / "sandboxes"
        for sandbox_id in sandbox_ids:
            run_dir = sandboxes_dir / sandbox_id
            if run_dir.exists():
                run_dirs_present.append(str(run_dir))

    zfs_datasets_present: list[str] = []
    if zfs_dataset:
        descendants = list_zfs_datasets(f"{zfs_dataset}/sandboxes")
        sandbox_set = set(sandbox_ids)
        for ref in descendants:
            suffix = ref.rsplit("/", 1)[-1]
            if suffix in sandbox_set:
                zfs_datasets_present.append(ref)

    passed = not active_leftovers and not run_dirs_present and not zfs_datasets_present
    return {
        "passed": passed,
        "control_plane_retained_stopped_ids": retained_stopped,
        "control_plane_missing_ids": control_plane_missing,
        "control_plane_non_stopped_leftovers": non_stopped_leftovers,
        "active_leftovers": active_leftovers,
        "run_dirs_present": run_dirs_present,
        "zfs_dataset": zfs_dataset or "",
        "zfs_datasets_present": zfs_datasets_present,
    }


def terminate_best_effort(args: argparse.Namespace, sandbox_id: str) -> str:
    proc = subprocess.run(sandbox_rm_cmd(args, sandbox_id), capture_output=True, text=True)
    combined = "\n".join(part for part in [proc.stdout.strip(), proc.stderr.strip()] if part).strip()
    return combined or f"exit={proc.returncode}"


def ensure_cleanroom_bin(path: str) -> None:
    if os.sep in path or path.startswith("."):
        if not (os.path.isfile(path) and os.access(path, os.X_OK)):
            raise RuntimeError(f"cleanroom binary not found or not executable: {path}")
        return
    if shutil.which(path) is None:
        raise RuntimeError(f"cleanroom binary not found in PATH: {path}")


def print_summary(report: dict[str, Any]) -> None:
    print("Sequential sandbox create benchmark")
    print(f"- host: {report['host']}")
    print(f"- iterations: {report['config']['iterations']} (+ {report['config']['warmup']} warmup)")
    print(f"- requested backend: {report['config']['backend'] or 'default'}")
    print(f"- observed backends: {', '.join(report['observed_backends']) or 'unknown'}")
    print(f"- output: {report['output_path']}")
    warmup = report.get("warmup")
    if warmup:
        print(
            "- warmup: "
            f"create {warmup['create_ms']:.2f} ms, "
            f"terminate {warmup['terminate_ms']:.2f} ms"
        )
    for key, label in [
        ("create_ms", "create"),
        ("terminate_ms", "terminate"),
        ("total_ms", "create+terminate"),
    ]:
        stats = report["results"].get(key)
        if not stats:
            continue
        print(
            f"- {label}: "
            f"mean {stats['mean']:.2f} ms, "
            f"p50 {stats['p50']:.2f} ms, "
            f"p95 {stats['p95']:.2f} ms, "
            f"max {stats['max']:.2f} ms"
        )
    service_stats = report["results"].get("service_create_ms")
    if service_stats:
        print(
            "- service create window: "
            f"mean {service_stats['mean']:.2f} ms, "
            f"p50 {service_stats['p50']:.2f} ms, "
            f"p95 {service_stats['p95']:.2f} ms, "
            f"max {service_stats['max']:.2f} ms"
        )

    cleanup = report.get("cleanup", {})
    print(
        "- cleanup: "
        + ("pass" if cleanup.get("passed") else "fail")
        + f" (retained stopped={len(cleanup.get('control_plane_retained_stopped_ids', []))}, "
        + f"active leftovers={len(cleanup.get('active_leftovers', []))}, "
        + f"run dirs present={len(cleanup.get('run_dirs_present', []))}, "
        + f"zfs leftovers={len(cleanup.get('zfs_datasets_present', []))})"
    )
    if report.get("error"):
        print(f"- error: {report['error']}")


def main() -> int:
    args = parse_args()
    if args.iterations <= 0:
        raise SystemExit("iterations must be a positive integer")
    if args.warmup < 0:
        raise SystemExit("warmup must be a non-negative integer")

    ensure_cleanroom_bin(args.cleanroom_bin)
    if shutil.which("python3") is None and sys.executable.endswith("python"):
        raise SystemExit("python3 is required")

    output_dir = pathlib.Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    timestamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H-%M-%SZ")
    output_path = output_dir / f"{timestamp}-sandbox-create-sequential.json"

    zfs_dataset = detect_zfs_dataset(args)
    all_ids: list[str] = []
    live_ids: set[str] = set()
    observed_backends: set[str] = set()
    observed_statuses: set[str] = set()
    measured_runs: list[RunRecord] = []
    warmup_record: RunRecord | None = None
    error_message: str | None = None

    exit_code = 0
    try:
        total_runs = args.warmup + args.iterations
        for index in range(total_runs):
            kind = "warmup" if index < args.warmup else "measured"
            measured_iteration = 0 if kind == "warmup" else (index - args.warmup + 1)

            create_proc, create_ms = run_timed(sandbox_create_cmd(args))
            payload = parse_json_output(create_proc.stdout, "cleanroom sandbox create")
            if not isinstance(payload, dict):
                raise RuntimeError("cleanroom sandbox create: expected a JSON object")

            sandbox_id = str(payload.get("sandbox_id", "")).strip()
            if not sandbox_id:
                raise RuntimeError(f"cleanroom sandbox create: missing sandbox_id in {payload!r}")

            backend = str(payload.get("backend", "")).strip()
            status = normalize_status(payload.get("status"))
            observed_backends.add(backend or "unknown")
            observed_statuses.add(status or "unknown")
            all_ids.append(sandbox_id)
            live_ids.add(sandbox_id)

            _, terminate_ms = run_timed(sandbox_rm_cmd(args, sandbox_id))
            live_ids.discard(sandbox_id)

            record = RunRecord(
                kind=kind,
                iteration=measured_iteration,
                sandbox_id=sandbox_id,
                backend=backend,
                status=status,
                create_ms=create_ms,
                terminate_ms=terminate_ms,
                total_ms=create_ms + terminate_ms,
                service_create_ms=service_create_ms(payload),
            )
            if kind == "warmup":
                warmup_record = record
            else:
                measured_runs.append(record)
    except Exception as exc:
        error_message = str(exc)
    cleanup_errors: list[dict[str, str]] = []
    for sandbox_id in list(live_ids):
        message = terminate_best_effort(args, sandbox_id)
        cleanup_errors.append({"sandbox_id": sandbox_id, "message": message})
        live_ids.discard(sandbox_id)

    cleanup = {
        "passed": True,
        "control_plane_retained_stopped_ids": [],
        "control_plane_missing_ids": [],
        "control_plane_non_stopped_leftovers": [],
        "active_leftovers": [],
        "run_dirs_present": [],
        "zfs_dataset": zfs_dataset or "",
        "zfs_datasets_present": [],
    }
    if all_ids:
        try:
            cleanup = cleanup_report(args, all_ids, zfs_dataset)
        except Exception as exc:
            cleanup["passed"] = False
            cleanup["error"] = str(exc)
    if cleanup_errors:
        cleanup["best_effort_termination_messages"] = cleanup_errors
        cleanup["passed"] = False

    create_values = [run.create_ms for run in measured_runs]
    terminate_values = [run.terminate_ms for run in measured_runs]
    total_values = [run.total_ms for run in measured_runs]
    service_values = [run.service_create_ms for run in measured_runs if run.service_create_ms is not None]

    report: dict[str, Any] = {
        "benchmark": "sandbox-create-sequential",
        "timestamp": timestamp,
        "host": args.host,
        "output_path": str(output_path),
        "config": {
            "iterations": args.iterations,
            "warmup": args.warmup,
            "backend": args.backend,
            "image": args.image,
            "launch_seconds": args.launch_seconds,
            "state_dir": args.state_dir,
            "zfs_dataset_requested": args.zfs_dataset,
            "cleanroom_bin": args.cleanroom_bin,
        },
        "observed_backends": sorted(observed_backends),
        "observed_statuses": sorted(observed_statuses),
        "warmup": asdict(warmup_record) if warmup_record is not None else None,
        "runs": [asdict(run) for run in measured_runs],
        "results": {
            "create_ms": summarize(create_values) if create_values else None,
            "terminate_ms": summarize(terminate_values) if terminate_values else None,
            "total_ms": summarize(total_values) if total_values else None,
            "service_create_ms": summarize(service_values) if service_values else None,
        },
        "cleanup": cleanup,
        "error": error_message,
    }

    output_path.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print_summary(report)

    if error_message:
        print(f"Results written to {output_path}", file=sys.stderr)
        exit_code = 1
    elif not cleanup.get("passed", False):
        print(f"Cleanup verification failed. Results written to {output_path}", file=sys.stderr)
        exit_code = 1
    else:
        print(f"Results written to {output_path}")

    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
