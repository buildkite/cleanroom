# Transparent Sandbox Suspend And Wake Plan

**Status:** Landed
**Last reviewed:** 2026-05-31
**Spec references:** `docs/api.md`, `docs/snapshots.md`, `docs/backend/darwin-vz.md`
**Related plans:** `docs/plans/system-storage-prune.md`, `docs/plans/layered-caching.md`

## Summary

Allow idle persistent sandboxes to suspend on the same host and wake
transparently when a user performs an operation that needs the guest VM. The
first useful version should reduce idle CPU and memory pressure without changing
the normal `cleanroom exec --in`, file-transfer, or local exposure workflows.

This is not a replacement for snapshots. Existing snapshots save a sandbox
filesystem so future sandboxes can fresh-boot from that state. Transparent
suspend keeps the same sandbox identity and guest process state on the same
daemon while the VM is paused. Later live checkpoint or hibernation work can
build on the lifecycle surface, but it needs a separate backend capability and
stronger restore semantics.

The implementation started with `darwin-vz`, because the helper already exposes
`PauseVM` and `ResumeVM`, and the backend already uses those operations
internally while capturing filesystem snapshots. Firecracker now supports
same-host freeze/wake using its existing `SIGSTOP` / `SIGCONT` helpers; describe
that as a CPU freeze until we prove memory reclamation or durable checkpointing.

## Problem

Cleanroom persistent sandboxes stay fully running until explicit termination.
That is useful for fast repeated commands, long-lived service processes, and
local port exposures, but it makes idle sandboxes consume host resources even
when the user is not interacting with them.

Before this work, the lifecycle model had no suspended state. `SandboxStatus`
only distinguished provisioning, ready, stopping, stopped, and failed.
Control-service operations generally required `READY` before touching the guest,
and the backend adapter interface only exposed provision, run, and terminate.
That left no place to express "this sandbox still exists, but the VM must wake
before the next guest operation."

The project already has filesystem snapshot semantics, but those are fresh-boot
restore points. Reusing snapshot language for idle VM suspension would create
the wrong expectation around live processes, network identity, open sockets, and
daemon restart durability.

## Goals

- Add backend-neutral sandbox lifecycle states for suspended and waking
  sandboxes.
- Wake suspended sandboxes automatically for guest-touching operations.
- Keep pure control-plane inspection from waking a VM.
- Keep policy and repository behavior unchanged.
- Make host resource policy runtime config, not `cleanroom.yaml`.
- Fail closed when a backend or sandbox does not support suspend.
- Preserve existing busy-state invariants around active executions, repository
  preparation, and file transfers.
- Provide enough events and observability to explain why an operation waited.
- Land a small `darwin-vz` first slice before broader backend or hibernation
  work.

## Non-goals

- Do not add live VM memory checkpoint/restore in the first slice.
- Do not make user snapshots represent live process state.
- Do not promise suspended sandboxes survive daemon restart.
- Do not promise open TCP connections survive suspension.
- Do not add project policy fields for suspend behavior.
- Do not add a cross-machine VM fork or fan-out contract.
- Do not silently terminate unsupported backend sandboxes to mimic suspension.

## Starting State

This was the relevant behavior when the plan started; it is retained as context
for why the lifecycle surface exists:

- `docs/snapshots.md` says snapshots are filesystem savepoints, not live process
  or memory checkpoints.
- `proto/cleanroom/v1/control.proto` exposes `Sandbox.status` and the current
  `SandboxStatus` enum with `PROVISIONING`, `READY`, `STOPPING`, `STOPPED`, and
  `FAILED`.
- There are no suspend/resume RPCs, control-client methods, control-server
  handlers, or `cleanroom sandbox` subcommands today.
- `internal/backend/backend.go` has capability flags and small optional backend
  interfaces for snapshots, file transfer, and port dialing.
- `internal/controlservice/service.go` tracks each sandbox's backend,
  capabilities, policy, active execution, repository busy state, file-transfer
  state, status, and event feed.
- `beginSandboxFileTransfer`, `CreateExecution`, `DialSandboxPort`, and
  snapshot creation all reject non-ready sandboxes before touching the guest.
- `cmd/cleanroom-darwin-vz/main.swift` implements helper `PauseVM` and
  `ResumeVM`.
- `internal/backend/darwinvz/backend_darwin.go` uses helper pause/resume while
  creating filesystem snapshots.
- `internal/backend/firecracker/backend.go` has process-level pause/resume
  helpers for snapshot capture.

These are enough for a same-daemon, same-host suspend/wake feature without
changing guest bootstrap, repository bootstrap, or snapshot storage.

## Current Progress

Slice 1 has landed: lifecycle statuses, backend capability inference, explicit
suspend/resume RPCs, control-client and control-server plumbing, manual CLI
commands, `darwin-vz` helper calls, and focused unit and integration tests.

Slice 2 has landed: guest-touching operations wake suspended sandboxes before
execution, file transfer, snapshot creation, or sandbox port dialing.
Client-side local exposure registration also accepts a suspended sandbox when
the backend advertises suspend and port-dial support, so the incoming connection
wakes through `DialSandboxPort`. Active port dials hold a guest-interaction
lease so idle-suspend work has a concrete busy marker for open tunnels.

Slice 3 has landed: `sandbox_lifecycle` is runtime config, the daemon starts an
idle suspend worker when `idle_suspend_after_seconds` is positive, and
transparent wake is bounded by `wake_timeout_seconds` or the sandbox launch
timeout when unset.

Slice 4 has landed: service metrics and logs cover backend
suspend and wake duration and outcome, and the `darwin-vz` E2E path now smokes
manual suspend, transparent wake through command execution, file read, and local
port exposure before terminating the sandbox.

Slice 5 has landed: Firecracker implements same-host freeze/resume through its
existing process pause and resume helpers, publishes the backend-neutral suspend
capability, and the Firecracker E2E path smokes manual suspend, transparent wake
through command execution, file read, local port exposure, deny-by-default
egress after wake, and termination from a suspended state.

Live hibernation and daemon-restart restore remain follow-up work.

The feature shipped in Cleanroom `v0.10.0`, published on 2026-05-30.

## Post-Release Follow-Up

The implementation and GitHub release are complete. Remaining work is
operational hardening, not a release blocker:

- Prove installed-daemon behavior on rolled-out hosts, not only in-repo
  execution: `doctor`, daemon status, manual suspend/wake, and a short
  `sandbox_lifecycle.idle_suspend_after_seconds` smoke.
- Decide whether installers or ops-managed hosts should set an idle timeout by
  default. Until that decision is made, automatic idle suspend remains explicit
  host runtime config.
- Keep product and operator wording clear that Firecracker suspend is same-host
  freeze/resume, not memory reclamation or durable hibernation.

## Target Lifecycle Model

Extend sandbox status with explicit transient and stable suspended states:

```proto
enum SandboxStatus {
  SANDBOX_STATUS_UNSPECIFIED = 0;
  SANDBOX_STATUS_PROVISIONING = 1;
  SANDBOX_STATUS_READY = 2;
  SANDBOX_STATUS_STOPPING = 3;
  SANDBOX_STATUS_STOPPED = 4;
  SANDBOX_STATUS_FAILED = 5;
  SANDBOX_STATUS_SUSPENDING = 6;
  SANDBOX_STATUS_SUSPENDED = 7;
  SANDBOX_STATUS_WAKING = 8;
}
```

State transitions:

```text
READY -> SUSPENDING -> SUSPENDED -> WAKING -> READY
READY -> STOPPING -> STOPPED
SUSPENDED -> STOPPING -> STOPPED
WAKING -> FAILED
SUSPENDING -> READY
SUSPENDING -> SUSPENDED
WAKING -> SUSPENDED
```

`SUSPENDED` means the sandbox identity, metadata, rootfs, helper process, and
backend instance still exist, but guest execution is paused. The control service
must wake the sandbox before any operation that requires guest code or guest
network connectivity.

`SUSPENDING -> READY` is the normal failure recovery path for a suspend attempt,
especially automatic idle suspend. If the backend cannot pause a still-running
sandbox, the control service should publish a failed-suspend event and leave the
sandbox usable. It should mark the sandbox `FAILED` only when the backend reports
that the instance is gone, corrupt, or otherwise cannot safely continue.
Deadline, cancellation, or backend-signaled transport/response errors are
indeterminate because the backend may have already applied the lifecycle
operation before the response failed. In those cases, the control plane should
keep the sandbox in the conservative retryable state: `SUSPENDED` after
ambiguous suspend or wake results.

`STOPPED` remains terminal. Stopped sandboxes are not wakeable.

## Wake Semantics

Guest-touching operations should wake a suspended sandbox before continuing:

- `CreateExecution`, including `sandbox.run.before`.
- Internal workspace copy-in executions.
- `DownloadSandboxFile`, `UploadSandboxFile`, `StatSandboxPath`,
  `WalkSandboxTree`, `ReadSandboxFile`, `WriteSandboxFile`,
  `RemoveSandboxPath`, `ArchiveSandboxPaths`, and `ExtractSandboxArchive`.
- `DialSandboxPort`, including local exposure connections.
- Snapshot creation, because it runs guest `sync` before capturing storage.

Pure control-plane operations should not wake the sandbox:

- `GetSandbox`
- `ListSandboxes`
- `ListExecutions`
- `GetExecution`
- `InspectExecution`
- `StreamSandboxEvents`
- `StreamExecution`
- `TerminateSandbox`

Local exposures need special handling. Today client-side exposure setup checks
that the sandbox is `READY` before registering listeners. That should change to
allow `SUSPENDED` when the sandbox reports the new suspend capability. The
actual incoming connection should wake through `DialSandboxPort`, then dial the
guest port after the backend reports ready.

## Runtime Config

Suspend policy belongs in runtime config because it is host resource management.
The implementation adds one host-level section:

```yaml
sandbox_lifecycle:
  idle_suspend_after_seconds: 600
  wake_timeout_seconds: 30
```

Semantics:

- omitted or zero `idle_suspend_after_seconds` disables automatic idle suspend
  by default
- `wake_timeout_seconds` bounds transparent wake waits, defaulting to the
  backend launch timeout when unset
- the policy applies only to backends that report `sandbox.suspend=true`

The implementation added the manual suspend/resume API and CLI before the idle
worker. Automatic idle suspend is available by config, but remains disabled by
default.

Avoid per-project fields in `cleanroom.yaml`. A repository should not decide how
aggressively a developer or CI host reclaims idle VM resources.

## Adapter Contract

Add one capability and one optional interface:

```go
const CapabilitySandboxSuspend = "sandbox.suspend"

type SuspendableAdapter interface {
    Adapter
    SuspendSandbox(ctx context.Context, sandboxID string) error
    ResumeSandbox(ctx context.Context, sandboxID string) error
}
```

`CapabilitiesForAdapter` should infer `sandbox.suspend=true` when the adapter
implements `SuspendableAdapter`, while still allowing backend-specific
capability reporting to force it off if runtime config makes it unavailable.

Adapter requirements:

- `SuspendSandbox` must be idempotent for an already suspended backend instance
  when the backend can detect that state.
- `ResumeSandbox` must be idempotent for an already running backend instance
  when the backend can detect that state.
- Both operations must return an error for unknown sandboxes.
- The adapter must not mutate policy, repository state, snapshot metadata, or
  user-visible sandbox identity.
- Backends that cannot suspend must simply not implement the interface.

## Control-Service Design

Expose explicit control-plane methods for manual lifecycle testing and operator
use:

```proto
rpc SuspendSandbox(SuspendSandboxRequest) returns (SuspendSandboxResponse);
rpc ResumeSandbox(ResumeSandboxRequest) returns (ResumeSandboxResponse);

message SuspendSandboxRequest {
  string sandbox_id = 1;
}

message SuspendSandboxResponse {
  Sandbox sandbox = 1;
}

message ResumeSandboxRequest {
  string sandbox_id = 1;
}

message ResumeSandboxResponse {
  Sandbox sandbox = 1;
}
```

These RPCs should also be exposed through the control client, control server, and
`cleanroom sandbox suspend|resume` commands in the first slice. Transparent wake
for guest-touching operations remains implicit and does not require users to call
`ResumeSandbox` directly.

Add a small lifecycle gate around guest-touching operations. It should replace
the repeated "status must be `READY`" checks with a common helper that can wake
first, then claim the operation-specific busy marker.

The helper should:

1. Look up the sandbox and adapter.
2. Reject terminal, provisioning, stopping, or failed states.
3. If the sandbox is `SUSPENDED`, transition it to `WAKING`, publish a sandbox
   event, and call `ResumeSandbox` without holding `s.mu`.
4. Coalesce concurrent wake attempts so only one backend resume call runs.
5. On successful wake, transition to `READY` and continue.
6. On wake failure, transition to `FAILED` and return a clear error. Keep
   `FAILED` sandboxes in active-resource metrics until termination confirms
   resources are gone.
7. Claim the requested operation marker, such as active execution or file
   transfer, only after the sandbox is ready.

Add a generic guest-interaction lease or explicit counters for interaction types
that do not currently have a busy marker. `DialSandboxPort` is the important
case: an accepted local exposure connection should hold a lease until the
proxied connection closes, so the idle worker does not pause the VM underneath
an active tunnel.

The gate should keep the existing idle checks:

- no active non-final execution
- no repository preparation
- no file transfer unless the caller is claiming that transfer
- no suspension while another lifecycle operation is in flight
- no active guest-interaction leases such as port dials

The idle suspend worker should use the same checks before calling
`SuspendSandbox`. It must not suspend a sandbox while a file transfer, execution,
repository refresh, snapshot, or port dial is active.

For observability, publish sandbox events such as:

- `sandbox idle suspend requested`
- `sandbox suspended`
- `sandbox suspend failed: ...`
- `sandbox wake requested`
- `sandbox ready after wake`
- `sandbox wake failed: ...`

Metrics should include backend, outcome, and duration for suspend and wake. Do
not include sandbox ids in metric labels.

## Backend Notes

### darwin-vz

The first backend should call helper `PauseVM` and `ResumeVM` for the existing
helper session and VM id. That is the narrowest path because these operations
already exist and are used in snapshot capture.

The backend should keep the helper process, proxy socket, filehandle gateway,
rootfs attachment, cache-output volumes, and sandbox runtime directory alive
while suspended. Wake should use the same proxy socket and guest agent path that
normal executions already use.

A follow-up optimization can lower the memory balloon target before pausing and
restore it on wake. That should not be part of the first correctness slice
unless local measurements show pause alone does not reduce the resource pressure
we care about.

### Firecracker

Firecracker can support a later same-host freeze using the existing
`pauseSandboxProcess` and `resumeSandboxProcess` helpers. Treat that as a CPU
freeze and process scheduling control only. It does not release rootfs storage,
does not prove memory reclamation, and does not survive daemon restart.

Do not expose Firecracker support until a real host smoke proves:

- command execution succeeds after suspend and wake
- local port dialing wakes and connects
- gateway firewall state remains correct
- termination works from both suspended and waking states

### Hibernation And Checkpointing

Live checkpoint/restore should be a separate later capability, for example
`sandbox.hibernate`, not an implementation detail hidden behind
`sandbox.suspend`.

That design must handle:

- restoring network identity and gateway registration
- reopening vsock, serial, helper, or proxy transports
- invalidating stale local exposure connections
- guest clock and timer behavior after restore
- daemon restart and host reboot semantics
- whether memory image size makes the feature faster than fresh boot

Until then, suspended sandboxes are same-daemon and same-host only.

## Safety Invariants

- Unsupported backends fail closed and never pretend termination is suspension.
- Waking a sandbox must not widen network policy or skip stage-scoped egress
  selection.
- Suspension must not run while guest-mutating work is active.
- `TerminateSandbox` must work from `READY`, `SUSPENDING`, `SUSPENDED`, and
  `WAKING`.
- `CreateSnapshot` must either wake and run the existing guest `sync` path or
  return a clear error. It must not snapshot an unknown paused filesystem state
  without the existing sync boundary.
- Pure inspection should not wake a VM.
- Control-service locks must not be held while calling backend suspend or
  resume.
- Failed suspend must be non-terminal when the sandbox is still running: publish
  an event, revert to `READY`, and retry only after the next idle window. Mark
  `FAILED` only when the backend reports a lost or unusable instance.
- Ambiguous suspend or wake results, such as deadline, cancellation, and helper
  transport or response errors, must not leave the sandbox in `READY` unless the
  backend proves it is running. Use `SUSPENDED` as the retryable conservative
  state.
- Failed wake should be visible as a sandbox event and final `FAILED` state for
  definite backend errors, not an endless `WAKING` state. `FAILED` still counts
  in active-resource metrics until termination confirms cleanup.
- Idle timers must be disabled by default until real backend smoke tests prove
  the behavior.

## Delivery Strategy

### Slice 1: Lifecycle Surface And Manual darwin-vz Wake

Add the enum states, capability, `SuspendableAdapter`, and `darwin-vz`
implementation. Add explicit suspend/resume RPCs, control-client methods,
control-server handlers, service methods, CLI commands, and status formatting,
but keep automatic idle suspend disabled.

Definition of done:

- unit tests cover status transitions, capability reporting, unsupported
  backend errors, failed suspend recovery to `READY`, and wake failure
- `darwin-vz` unit tests prove `PauseVM` and `ResumeVM` are called
- `cleanroom sandbox suspend <sandbox-id>` pauses a ready `darwin-vz` sandbox
- `cleanroom sandbox resume <sandbox-id>` resumes a suspended `darwin-vz`
  sandbox
- `GetSandbox` and `ListSandboxes` show suspended state without waking
- CLI inspect/list output renders `SUSPENDING`, `SUSPENDED`, and `WAKING`
  explicitly rather than as `unknown`

The first CLI surface is intentionally narrow and manual. It exists to prove the
backend and control-plane lifecycle before transparent wake and idle timers are
enabled:

```console
cleanroom sandbox suspend <sandbox-id>
cleanroom sandbox resume <sandbox-id>
```

### Slice 2: Transparent Wake Gates

Wire wake into `CreateExecution`, file transfer operations, snapshot creation,
and `DialSandboxPort`.

Definition of done:

- suspended sandbox `CreateExecution` wakes and runs
- suspended sandbox file read/write wakes and completes
- suspended sandbox local exposure connection wakes through `DialSandboxPort`
- concurrent wake attempts coalesce
- unsupported backends still return clear capability errors

### Slice 3: Idle Suspend Runtime Policy

Add `sandbox_lifecycle` runtime config and an idle scheduler in the daemon.
Keep the default disabled. When enabled, suspend only sandboxes that remain idle
past the configured threshold.

Definition of done:

- config loading and validation tests cover zero, positive, and invalid values
- idle worker skips busy sandboxes
- idle worker publishes events for suspend attempts and results
- `cleanroom config validate` rejects invalid lifecycle values

### Slice 4: Observability And Real Host Smoke

Add metrics, logs, and a real `darwin-vz` smoke test path.

Definition of done:

- traces or metrics include suspend and wake duration and outcome
- smoke test creates a sandbox, suspends it, runs a command after wake, reads a
  file after wake, and terminates it
- local exposure smoke confirms an incoming connection wakes and reaches the
  guest service

### Slice 5: Firecracker Freeze Experiment

Implement Firecracker same-host freeze only after the `darwin-vz` path has
landed.

Definition of done:

- capability reports accurately on supported hosts
- host smoke proves execution and port dialing after wake
- docs describe it as freeze/resume, not memory hibernation

## Verification

Unit tests:

- capability inference for `SuspendableAdapter`
- status transition helpers
- wake coalescing
- idle eligibility
- unsupported backend errors
- terminal state rejection
- pure inspection does not wake
- file transfer and execution wake before claiming busy state

Integration tests:

- service-level fake adapter test for suspended `CreateExecution`
- service-level fake adapter test for suspended file transfer
- service-level fake adapter test for suspended `DialSandboxPort`
- concurrent wake test with multiple callers
- termination from suspended and waking states

Runtime smoke:

```bash
mise run build
CLEANROOM_DARWIN_VZ_E2E=1 mise exec -- go test ./internal/backend/darwinvz -run TestPersistentSandboxSuspendWakeE2E -v
```

Manual installed-daemon proof before recommending or enabling automatic idle
suspend by default:

```bash
/usr/local/bin/cleanroom doctor --backend darwin-vz --json
/usr/local/bin/cleanroom daemon status
/usr/local/bin/cleanroom sandbox create --image ghcr.io/buildkite/cleanroom-base/alpine:latest
/usr/local/bin/cleanroom sandbox suspend <sandbox-id>
/usr/local/bin/cleanroom exec --in <sandbox-id> -- uname -a
/usr/local/bin/cleanroom sandbox inspect <sandbox-id>
```

Installed-daemon idle-suspend smoke:

```bash
cleanroom config validate
# Set sandbox_lifecycle.idle_suspend_after_seconds to a small positive value
# on a disposable host or isolated runtime config, then restart the daemon.
cleanroom daemon restart
cleanroom sandbox create --image ghcr.io/buildkite/cleanroom-base/alpine:latest
sleep <idle threshold plus one poll interval>
cleanroom sandbox inspect --last # status should be suspended
cleanroom exec --in <sandbox-id> -- echo woke
```

## Key Learnings From Pressure-Testing

The main risk is overpromising. "Suspend" can mean pause, hibernate, checkpoint,
or stop depending on the system. The first Cleanroom contract should be explicit:
same-host, same-daemon VM pause with transparent wake for the next guest
operation. That avoids contaminating user snapshots with live memory semantics.

The second risk is hidden operation paths. File transfer, snapshots, and port
dialing all execute guest-sensitive work without looking like normal `exec`.
The control service should centralize wake gating instead of adding one-off
status exceptions.

The third risk is racing lifecycle transitions with active work. The plan keeps
busy checks and backend calls separate: decide and mark lifecycle transitions
under the service lock, perform backend work outside the lock, then re-check and
publish final state.

Automatic idle suspend is deliberately opt-in even though the worker has landed.
Manual suspend/resume plus transparent wake are the release-safe correctness
target; a daemon timer should become a default only after installed-daemon and
prod-host rollout proof shows the wake latency and support boundary are
acceptable.

## Resolved Decisions

- Use a new lifecycle surface, not snapshot semantics.
- Keep suspend policy in runtime config rather than repository policy.
- Start with `darwin-vz`.
- Make `SUSPENDED` visible in `GetSandbox`, `ListSandboxes`, and event streams.
- Do not wake for pure control-plane inspection.
- Expose manual `sandbox suspend` and `sandbox resume` commands in the first PR
  so the lifecycle can be proven against an installed daemon before idle timers
  exist.
- Firecracker support is same-host freeze/resume only; hibernation remains
  follow-up work.
- Active local port dials hold a guest-interaction lease so the idle worker does
  not suspend underneath an open tunnel.

## Deferred Questions

- What idle timeout should the installer eventually recommend for local
  developer machines?
- Should automatic idle suspend ever be enabled by default, or remain explicit
  host config?
- Can memory ballooning before `darwin-vz` pause materially reduce host memory
  pressure without adding surprising wake latency?
