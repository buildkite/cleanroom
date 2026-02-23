# Benchmarks

This document describes the lightweight benchmark methodology we use for:

- regression detection over time
- comparing different host classes for cleanroom workloads

The goal is consistency, not perfect synthetic accuracy.

## Scope

We currently track two benchmark categories:

- TTI (time-to-interactive): sandbox create -> first successful command
- in-sandbox workloads: IOPS-like file throughput, git clone latency, CPU hashing throughput

## Methodology

### 1) TTI benchmark

Script: `scripts/benchmark-tti.sh`

- Starts from `cleanroom exec -- echo benchmark`
- Uses `hyperfine` for repeated runs
- Excludes sandbox teardown from timed command
- Performs termination in `--prepare/--cleanup` steps

Example:

```bash
./scripts/benchmark-tti.sh --host unix:///tmp/cleanroom.sock --iterations 10
```

Output:

- `benchmarks/results/<timestamp>.json`

### 2) Sandbox workload benchmark

Script: `scripts/benchmark-sandbox-workloads.sh`

- Creates one sandbox and reuses it across all workload iterations
- Workload A (storage):
  - `dd` write/read with `4 KiB` blocks (default), reports elapsed + estimated IOPS + MiB/s
- Workload B (git clone):
  - shallow clone (`--depth 1`) of a configured repository
  - reports elapsed seconds + cloned size
- Workload C (CPU):
  - hashes a fixed byte count using `sha256sum`
  - reports elapsed seconds + MiB/s
- Sandbox teardown happens outside measured workload sections

Example:

```bash
./scripts/benchmark-sandbox-workloads.sh \
  --host unix:///tmp/cleanroom.sock \
  --iterations 5 \
  --repo-url https://github.com/hashicorp/terraform.git \
  --repo-depth 1
```

Output:

- `benchmarks/results/<timestamp>-sandbox-workloads.json`

## Host Baseline (Latest Run)

Recorded on: `2026-02-22T00-07-09Z`

- Instance type: `m8i.xlarge`
- Region/AZ: `ap-southeast-2` / `ap-southeast-2b`
- CPU (host): `4 vCPU` (`Intel Xeon 6975P-C`)
- Memory (host): `~15 GiB`
- Root disk: `200 GiB` EBS (`Amazon Elastic Block Store`)
- Architecture: `x86_64`

Source file:

- `benchmarks/results/2026-02-22T00-07-09Z-sandbox-workloads.json`

### Latest workload results

- IOPS write mean: `670,476 ops/s` (`2619.05 MiB/s`)
- IOPS read mean: `1,400,000 ops/s` (`5468.75 MiB/s`)
- Git clone mean: `1.516 s` (`46 MiB`, repo: `hashicorp/terraform`, depth `1`)
- CPU hash mean: `239.11 MiB/s`

## Notes for Comparison

- Keep scripts, flags, and repo URL/depth identical across hosts.
- Run on quiet hosts (avoid concurrent heavy jobs).
- Compare medians/means across repeated runs, not single-shot values.
- Treat absolute IOPS values as directional; regression deltas are the primary signal.
