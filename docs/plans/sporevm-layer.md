# Cleanroom As A SporeVM Layer

**Status:** Active
**Last reviewed:** 2026-07-02
**Related code:** `internal/cli/vm.go`, `internal/sporevm/`
**Related upstream:** `~/Develop/sporevm` `origin/main` libspore named lifecycle and networking APIs

## Summary

Cleanroom should become a small layer on top of libspore/sporevm, not a second
runtime, control plane, or fork/fan-out implementation.

The user-facing model should feel like a simple subset of `spore`:

```console
cleanroom create [--wait] <name> [dir]
cleanroom exec <name> -- cmd
cleanroom capture <name> --out ./test.spore
cleanroom resume ./test.spore --name <name>
cleanroom destroy <name>
```

Cleanroom adds value only where it translates project intent into SporeVM
runtime inputs: policy, workspace setup, gateway bindings, and provenance
metadata. Everything else should stay in SporeVM.

## Problem

The current Cleanroom codebase contains its own daemon/control-plane model,
backend adapters, sandbox ids, snapshot ids, workspace copy flows, gateway
plumbing, and execution inspection surfaces. Some of that was useful before
SporeVM exposed a stable libspore boundary, but it now risks duplicating the
runtime rather than composing it.

SporeVM already owns VM lifecycle, snapshots, named handles, fork/fan-out,
network enforcement primitives, rootfs/cache operations, and host capability
facts. Cleanroom should not reimplement those surfaces.

## Goals

- Make the public CLI a small named-VM subset over libspore.
- Use libspore as the runtime boundary, not the `spore` CLI.
- Translate Cleanroom policy into libspore create-time options.
- Keep unsupported policy fail-closed.
- Preserve Cleanroom's useful layer: workspace facts, gateway service bindings,
  and provenance.
- Delete or demote old control-plane UX once the new path covers it.

## Non-Goals

- Do not replicate SporeVM fork/fan-out. Users can call SporeVM for that.
- Do not preserve old top-level `create`/`exec` flags for compatibility.
- Do not keep Cleanroom backend adapters as the long-term runtime path.
- Do not introduce a broad capability framework before a concrete consumer needs
  it.
- Do not implement distributed scheduling here. Kubernetes or another scheduler
  can own distributed placement.

## Target Model

### Ownership Boundary

SporeVM owns:

- named VM lifecycle: create, exec, capture, destroy, resume, fork, list
- backend/runtime details
- snapshot format and restore semantics
- rootfs/cache/system operations
- network enforcement primitives and event capture

Cleanroom owns:

- CLI subset and policy loading
- policy-to-libspore option translation
- workspace planning and metadata
- gateway and bound host service declarations
- provenance facts attached to captured spores through SporeVM annotations

### CLI Contract

`cleanroom create` loads Cleanroom policy from `[dir]`, validates the subset that
can be represented by libspore, then calls `createNamed`.

`cleanroom exec` calls `execNamed` against the named VM and propagates guest
stdout, stderr, and exit code. `--json` prints the libspore response.

`cleanroom capture` calls `snapshotNamed` with `continue_after=true`.

`cleanroom resume` reads Cleanroom metadata from the spore, recreates the
Cleanroom setup that was active when it was captured, then calls `resumeNamed`.

`cleanroom destroy` calls `removeNamed`. `terminate` and `rm` can remain aliases.

### Policy Contract

First supported policy subset:

- `sandbox.image.ref`
- resources that map directly to libspore memory/vCPU options
- global exact host-plus-port `sandbox.network.allow`

Rejected until mapped safely:

- stage-scoped network policy
- Docker service
- dependency/service stages
- `sandbox.run.before`
- wildcard/CIDR/SNI/HTTP network policy
- live per-exec network policy updates

The first implementation may reject all network allow rules. The next slice
should translate exact host-plus-port rules after checking libspore
`networkCapabilities`.

### Workspace Contract

Keep this smaller than the old workspace system.

The initial workspace layer records facts:

- source directory
- git remote and commit when available
- dirty state
- policy source and hash

Actual file materialization should be added only when needed by a concrete flow.
Until then, `create <name> [dir]` can treat `[dir]` as policy/workspace context,
not as an implicit bidirectional sync system.

### Gateway Contract

Cleanroom gateway support should map to libspore bound Unix services:

- Cleanroom starts a host service when policy needs it.
- `createNamed` receives bound service declarations.
- Captures record service names and guest endpoints, not live host socket paths.
- Resume fails closed if required services are not provided.

### Provenance Contract

Cleanroom should write a small provenance record into SporeVM annotations. This
depends on SporeVM adding a Docker-style metadata extension; Cleanroom should not
invent a sidecar store for this:

- Cleanroom version
- libspore version and ABI version
- policy source and hash
- image ref and digest when known
- workspace facts
- network rules accepted by libspore
- gateway service declarations

This is factual metadata, not an abstract capability system.

### Resume Contract

Resume is part of the core Cleanroom lifecycle. A Cleanroom-created spore should
resume with the same Cleanroom setup it had when captured:

- same policy hash and accepted network rules
- same gateway service names and guest endpoints
- same workspace facts
- same image/rootfs identity as recorded by SporeVM

Resume should not silently re-read current repository policy and pretend it is
the original setup. If required Cleanroom metadata is missing, malformed, or
requires gateway services that cannot be provided, `cleanroom resume` should fail
closed before calling `resumeNamed`.

The CLI should stay close to SporeVM:

```console
cleanroom resume ./test.spore --name worker
```

## Current State

Slices 1 and 2 are implemented in this branch. Top-level `create`, `exec`,
`capture`, `resume`, and `destroy` point at a libspore-backed adapter under
`internal/sporevm/`, with `terminate` and `rm` kept as `destroy` aliases.

The adapter now consumes SporeVM's official Go binding instead of declaring
Cleanroom-owned C ABI structs.

## Delivery Strategy

### Slice 1: Lock The Simple CLI

Status: implemented in this branch.

- Kept only top-level `create`, `exec`, `capture`, `resume`, `destroy` for the
  named lifecycle subset.
- Removed the public `vm` command group.
- Updated parser tests and help text.
- Kept old daemon/control-plane command structs only where existing direct tests
  still need them.

### Slice 2: Use Official Go libspore Bindings

Status: implemented in this branch.

- Add named lifecycle wrappers to SporeVM's Go binding, or consume them once
  landed.
- Replace Cleanroom's cgo shim with that package.
- Keep one tiny Cleanroom adapter only if it adds policy/workspace naming.

Done when Cleanroom does not declare C ABI structs itself.

### Slice 3: Translate Network Policy

- Add `networkCapabilities` to the Go binding.
- Translate Cleanroom exact host-plus-port allow rules to libspore create-time
  network rules.
- Reject stage-scoped network until libspore supports live policy updates.

Done when a policy with `github.com:443` becomes a libspore network rule and an
unsupported policy fails before VM creation.

### Slice 4: Add Gateway Bindings

- Map Cleanroom gateway services to libspore bound Unix services.
- Record restore-time service requirements in provenance.
- Keep service startup local and explicit.

Done when a VM can reach a Cleanroom-provided gateway endpoint through a bound
service and capture does not store host socket paths.

### Slice 5: Add Resume From Cleanroom Provenance

- Add `cleanroom resume <spore-dir> --name <name>`.
- Read Cleanroom provenance from SporeVM annotations.
- Recreate required gateway bindings before `resumeNamed`.
- Fail closed when required metadata or services are missing.

Done when a captured Cleanroom spore resumes with the same Cleanroom setup and
does not depend on current working-directory policy. This slice requires the
SporeVM annotations extension to exist first.

### Slice 6: Add Minimal Workspace Provenance

- Record source directory, git commit, dirty state, and policy hash.
- Store it in SporeVM annotations at capture time.
- Do not add sync/copy behavior yet.

Done when `capture` writes enough facts to explain what was created.

### Slice 7: Delete Old Runtime Surface

- Remove or hide daemon-backed top-level UX.
- Decide whether `sandbox`, `workspace`, `execution`, and `serve` survive as
  separate advanced/internal surfaces.
- Delete backend adapter paths only after no command depends on them.

Done when Cleanroom no longer has two user-facing runtime models.

## Verification

- Parser tests for the target CLI contract.
- Policy tests for accepted and rejected subsets.
- Tagged Go build against libspore.
- Runtime smoke on a host with SporeVM support:

```console
cleanroom create cr-test .
cleanroom exec cr-test -- /bin/true
cleanroom capture cr-test --out ./cr-test.spore
cleanroom resume ./cr-test.spore --name cr-restored
cleanroom exec cr-restored -- /bin/true
cleanroom destroy cr-test
cleanroom destroy cr-restored
```

- Network smoke once policy translation lands:

```console
cleanroom create cr-net ./fixtures/net-policy
cleanroom exec cr-net -- curl https://github.com
cleanroom exec cr-net -- curl https://example.com
```

The second command should fail when `example.com` is not allowed.

## Resolved Decisions

- Cleanroom should use libspore, not shell out to `spore`.
- The public UX should be a simple subset of SporeVM's named lifecycle.
- `resume` is part of the core Cleanroom subset.
- Resume should restore the setup recorded at capture time, not the caller's
  current policy directory.
- Cleanroom provenance should live in SporeVM annotations. Do not add a sidecar
  fallback.
- Cleanroom should not wrap fork/fan-out initially.
- Unsupported policy should fail closed.
- Compatibility with old top-level flags is not required during this rethink.

## Open Questions

- Should old `serve` remain?
  Default: no for local named lifecycle. Revisit only for a concrete remote
  control-plane use case.

## Key Learnings From Pressure-Testing

- The biggest risk is rebuilding SporeVM under Cleanroom names. The plan avoids
  this by making every runtime operation map to a libspore call.
- Workspace sync can easily become the old product again. The first workspace
  slice records facts only.
- Networking must fail closed. A partial translation that silently drops rules
  is worse than rejecting the policy.
- Provenance should stay factual. A generic capability/provenance framework is
  unnecessary until a reader needs more than concrete facts.
