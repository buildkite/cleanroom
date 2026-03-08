# Snapshot/Restore Capability Plan (Firecracker First)

**Thread reference:** `T-019cc2b1-6012-7478-a032-0b8f64f93174`

**Status:** Proposed

## Summary

This document captures what we learned while evaluating snapshot and restore as a first-class capability, and proposes a phased implementation that stays backend-neutral at the API/CLI level while delivering an initial Firecracker implementation.

Primary objective: reduce time-to-interactive (TTI) for first command execution by restoring from a prebuilt golden VM snapshot instead of cold booting on every sandbox create.

## Desired Outcomes

1. Snapshot and restore are explicit backend capabilities (discoverable and fail-closed).
2. Firecracker supports building and restoring golden snapshots safely.
3. `cleanroom exec` can opportunistically use restore for fast boot (`auto` mode) and reliably fall back to cold boot.
4. Observability and benchmark tooling can prove the TTI improvement.

## Non-Goals (Initial Rollout)

1. Cross-backend snapshot portability.
2. Remote/distributed snapshot artifact replication.
3. Diff snapshot chains and merge tooling.
4. Multi-tenant quota/isolation orchestration beyond local host constraints.

## Learning Inventory

## Current Codebase State

1. Backends already publish a machine-readable capability map, but there is no snapshot-related capability key yet.
2. Firecracker launch currently uses `--config-file` cold boot paths in both ephemeral and persistent flows.
3. Runtime rootfs preparation and per-run/per-sandbox rootfs cloning already exist and use reflink clone fallback where supported.
4. Persistent sandboxes are provisioned once and reused for execution, which is a natural place to introduce restore-based provisioning.
5. Run observability already captures phase timings (`rootfs_copy_ms`, `firecracker_start_ms`, `vm_ready_ms`, and more), so snapshot timings can be added with minimal format disruption.

## Gaps and Risks Found

1. No snapshot API surface exists in proto/control service/client yet.
2. No launch-time capability validation is currently wired in control service despite spec intent.
3. Host cleanup helper `runRootCommandBatch` intentionally swallows errors, which can hide failed network cleanup and leave state pollution behind.
4. Current launch flow has no Firecracker API client path for `pause`, `snapshot/create`, or `snapshot/load` operations.

## Firecracker Snapshot Constraints That Matter

1. `LoadSnapshot` is valid only before microVM boot; `Pause` and `CreateSnapshot` require a booted VM.
2. Snapshot restore requires compatible host resources (disk backing files, TAP devices, vsock path) to exist and be reachable by the new Firecracker process.
3. Cloning from the same snapshot can duplicate guest identity/state; source VM should not continue execution after snapshot when using a secure one-to-one flow.
4. Vsock connections open at snapshot time are reset on restore; listen sockets remain active, which aligns with cleanroom's host-dial guest-agent behavior.
5. Network state is not guaranteed to survive process boundaries; clone networking needs explicit handling.
6. Snapshot compatibility is tied to software/hardware details; strict fingerprinting is required for safe reuse.

## Proposed Architecture

## Capability Contract (Backend Neutral)

Add capability keys:

1. `snapshot.create`
2. `snapshot.restore`
3. `snapshot.golden_boot`

Policy and request-level requirements map to these keys. If a user requests snapshot-required behavior and backend capability is missing, fail with `backend_capability_mismatch`.

## Snapshot Artifact Model

A golden snapshot record contains:

1. `state.snap` (Firecracker VM state)
2. `mem.snap` (guest memory file)
3. Base rootfs artifact reference (prepared rootfs image used at snapshot creation)
4. Manifest metadata (`manifest.json`)

Suggested metadata fields:

1. Snapshot ID and profile name
2. Backend name/version
3. Fingerprint fields (see below)
4. Creation timestamps and toolchain versions
5. Size accounting for state/memory/disk artifacts

## Compatibility Fingerprint

A snapshot can be restored only when all fields match:

1. Backend + Firecracker binary version
2. Kernel path and SHA256
3. Guest agent SHA256
4. Prepared rootfs key and policy image digest
5. Machine config (`vcpus`, `memory_mib`, SMT)
6. Boot args template
7. Snapshot schema version (cleanroom-side)

Mismatch behavior:

1. `auto` mode: skip snapshot and cold boot, record skip reason.
2. `required` mode: fail before provisioning.

## Storage Layout

Use durable XDG data storage for snapshot artifacts:

`$XDG_DATA_HOME/cleanroom/snapshots/<backend>/<profile>/<snapshot-id>/...`

Keep large transient materializations in cache where possible.

## Firecracker Implementation Plan

## Phase 0: Hardening and Prerequisites

1. Fix `runRootCommandBatch` to return first cleanup failure (or aggregated failures) instead of swallowing all errors.
2. Add Firecracker API socket readiness checks before issuing API requests.
3. Ensure snapshot build path only snapshots after guest agent readiness to avoid early-boot restore instability.

## Phase 1: API/CLI and Capability Wiring

1. Add proto/service/client endpoints for snapshot lifecycle (build, list, delete).
2. Add launch options for snapshot mode (`off|auto|required`) and optional profile selection.
3. Add capability validation in control service create path and return `backend_capability_mismatch` when required.

## Phase 2: Firecracker Snapshot Build

Build flow:

1. Provision template VM using existing launch path.
2. Wait for guest agent readiness.
3. Optional warmup command(s) for profile-specific preloading.
4. Call Firecracker API: pause VM, create full snapshot.
5. Terminate source VM without resuming it.
6. Persist manifest + artifacts keyed by fingerprint.

## Phase 3: Firecracker Restore

Restore flow for sandbox provisioning:

1. Resolve matching snapshot by profile + fingerprint.
2. Prepare per-sandbox writable rootfs from the snapshot base rootfs (existing reflink-aware copy path).
3. Set up TAP/iptables and other host resources before restore.
4. Start Firecracker process with API socket only.
5. Call `snapshot/load` with memory/state files and explicit network overrides when needed.
6. Resume VM and run existing guest-agent readiness check.
7. Continue with existing `RunInSandbox` execution flow.

Fallback:

1. In `auto` mode, any restore failure falls back to cold boot and records reason.
2. In `required` mode, return explicit restore failure and stop.

## Phase 4: Golden Fast Boot Defaulting

1. Add `auto` snapshot mode to default runtime config for Firecracker once stable.
2. Keep `off` available for debugging and deterministic cold-boot comparisons.
3. Keep `required` for benchmark and strict CI validation scenarios.

## Networking Strategy for Restore/Clones

Short-term pragmatic strategy:

1. Keep one active VM per snapshot profile by default for secure, simple rollout.
2. Use per-sandbox TAP setup before restore.
3. Apply network interface override mapping at load time when host TAP names differ.

Future scale strategy:

1. Introduce network namespace per clone path for same-IP guest clones.
2. Add NAT/DNAT glue for ingress/egress where parallel clone density requires it.

## Security and Correctness Guardrails

1. Treat snapshot files as trusted local artifacts; do not expose remote import by default in initial rollout.
2. Include manifest hash validation for local corruption detection.
3. Document uniqueness caveats for cloned state and prefer one-to-one snapshot usage in initial release.
4. Keep `random.trust_cpu=on` and rely on modern kernel VMGenID behavior when available, but do not assume this solves all user-space uniqueness state.

## Observability and Benchmarking

Add run observability fields:

1. `snapshot_used` (bool)
2. `snapshot_load_ms`
3. `snapshot_skip_reason`
4. `snapshot_profile`

Benchmark contract:

1. Continue using existing TTI benchmark definition (`sandbox create -> first successful command`).
2. Compare median/p95 for `off`, `auto (hit)`, and `auto (miss)` modes.
3. Track restore hit-rate and fallback-rate trends.

## Test Plan

1. Unit tests for fingerprint computation and compatibility checks.
2. Unit tests for manifest read/write validation and corruption handling.
3. Adapter tests for restore-then-fallback behavior and mode semantics.
4. Integration tests for `snapshot build`, sandbox create from snapshot, and command execution.
5. Negative tests for fingerprint mismatch, missing files, API socket timeout, and host network cleanup failure.

## Milestone Order

1. Hardening + capability keys + validation wiring.
2. Snapshot build endpoint and Firecracker implementation.
3. Restore endpoint and provisioning integration with fallback.
4. Observability fields and benchmark automation updates.
5. Default-on `auto` mode after reliability targets are met.

## Open Decisions

1. Final protobuf shape for snapshot lifecycle APIs (new service vs extending SandboxService).
2. Exact snapshot profile model (single default profile vs named profiles with warmup commands).
3. Whether to keep snapshot artifacts under data dir only, or split metadata in state/cache for eviction policies.
4. Parallel clone networking strategy timeline (single-clone-first vs immediate namespace support).

## Definition of Done

1. Snapshot/restore capabilities are visible in doctor JSON and enforceable at launch.
2. Firecracker can build a golden snapshot and restore from it for sandbox provisioning.
3. `auto` mode is reliable and falls back safely on incompatibility.
4. TTI improvement is measurable and documented using the benchmark workflow.
