# Stage-Scoped Network Egress Plan

**Spec reference:** `docs/spec.md` sections 5.2, 5.4, 6.1, 6.2
**Status:** Active implementation
**Last reviewed:** 2026-04-28

## Summary

Allow repository policy to grant outbound network egress to specific sandbox
lifecycle stages, so dependency and service preparation can fetch external
artifacts while workload execution remains offline by default.

## Problem

The current policy model has one deny-by-default network allowlist for the whole
sandbox. That is simple, but it makes setup-time access and workload-time access
inseparable:

- dependency bootstrap often needs package registries, module proxies, or git
  hosts
- service bootstrap may need OCI registries or setup endpoints
- the final workload should often run without any external egress

With one global `sandbox.network.allow`, any host needed during setup remains
reachable during `sandbox.run.before` and the requested command. Adding stage
metadata to each global allow rule would solve the enforcement problem, but it
would make policy authoring harder to read because lifecycle information would be
attached to every destination instead of the stage that needs it.

## Stage Model

Stage-scoped egress should use the same conceptual lifecycle as the stage-cache
pipeline:

- `workspace`: repository checkout plus explicit repository changeset
  application
- `dependencies`: `sandbox.dependencies.command`
- `services`: `sandbox.services.command` and service preparation state
- `execution`: `sandbox.run.before` and the requested command

`sandbox.run.before` belongs to `execution`. It runs immediately before user
code, so treating it as a setup stage would create an easy way to regain
setup-time egress at workload time.

Internal file-transfer and control-plane helper commands should not inherit
setup-stage egress by accident. They should either run with the `execution`
effective policy or a dedicated no-egress internal policy.

## Policy Shape

Prefer stage-local `network` blocks over global allow rules with per-rule stage
metadata:

```yaml
repository:
  network:
    allow:
      - host: github.com
        ports: [443]

sandbox:
  network:
    default: deny

  dependencies:
    network:
      allow:
        - host: proxy.golang.org
          ports: [443]
        - host: sum.golang.org
          ports: [443]
        - host: registry.npmjs.org
          ports: [443, 80]
    command: mise exec -- go mod download

  services:
    network:
      allow:
        - host: ghcr.io
          ports: [443]
    command: docker compose pull

  run:
    network:
      allow: []
    before: mise exec -- go test ./...
```

Semantics:

- `sandbox.network.default` remains `deny` for repository policy.
- `repository.network.allow` is the workspace-stage allowlist for repository
  checkout and explicit changeset application.
- `sandbox.dependencies.network.allow` applies only to
  `sandbox.dependencies.command`.
- `sandbox.services.network.allow` applies only to `sandbox.services.command`.
- `sandbox.run.network.allow` applies only to `sandbox.run.before` and the
  requested command.
- Stage allowlists do not inherit from earlier stages.
- Omitted stage `network.allow` means no external egress for that stage.
- `allow: []` is an explicit empty allowlist and is useful for making offline
  execution visually obvious.

The compiled policy should remain immutable. Stage-scoped egress is derived from
that immutable policy by computing an effective network policy for the active
stage.

## Effective Policy

Add a small policy helper rather than spreading filtering logic through the
control service and backends:

```go
type NetworkStage string

const (
    NetworkStageWorkspace    NetworkStage = "workspace"
    NetworkStageDependencies NetworkStage = "dependencies"
    NetworkStageServices     NetworkStage = "services"
    NetworkStageExecution    NetworkStage = "execution"
)

func (p *CompiledPolicy) NetworkPolicyForStage(stage NetworkStage) *CompiledPolicy
func (p *CompiledPolicy) AllowsForStage(stage NetworkStage, host string, port int) bool
```

The effective policy keeps `NetworkDefault` deny and uses the allow rules from
the active stage. The original compiled policy hash continues to identify the
complete repository policy.

## Control-Service Changes

Add a stage field to `backend.ExecutionRequest`:

```go
type ExecutionRequest struct {
    ...
    NetworkStage policy.NetworkStage
}
```

The control service should set it at every persistent sandbox command boundary:

- repository bootstrap: `workspace`
- repository changeset application: `workspace`
- dependency bootstrap: `dependencies`
- services bootstrap: `services`
- `sandbox.run.before`: `execution`
- requested command: `execution`
- file-transfer helpers: `execution` or a dedicated no-egress internal stage

`ProvisionSandbox` should still receive the full compiled policy, because the
sandbox is created from immutable policy. `RunInSandbox` should receive both the
full policy and the active stage, or a precomputed effective policy plus enough
metadata for observability. The important invariant is that adapters must never
silently enforce the union of all stage allowlists for a stage-scoped policy.

## Backend Enforcement

Backends need an active-stage egress mechanism. If a backend cannot enforce
stage-local network policy precisely, it should fail closed for policies that
use stage-local network blocks rather than falling back to global allowlist
behavior.

### Firecracker

Firecracker currently installs host-side TAP, DNS, and FORWARD rules when the
sandbox VM is launched. Stage-scoped egress requires those rules to become
stage-aware:

- provision the TAP, anti-spoof rules, gateway reachability, DNS redirection,
  and terminal default-deny rules once
- keep per-sandbox active egress chains that can be flushed and repopulated
  before each command
- teach the trusted DNS runtime to swap the active effective policy for the
  sandbox
- populate DNS-observed destination rules from the active effective policy
- populate direct literal-IP rules from the active effective policy
- flush active egress rules when the command finishes
- delete or expire conntrack state for the guest IP when moving from a wider
  stage to a narrower stage

The last point matters. Existing established-connection rules can otherwise
allow a setup-stage TCP connection to remain usable during execution.

Gateway routing and injected environment variables must also use the effective
stage policy. If `github.com` is allowed only during `workspace`, execution
should not receive a git rewrite for `github.com`, and the gateway should reject
execution-stage attempts even if the process constructs a gateway URL manually.

### Darwin VZ

The `darwin-vz` file-handle gateway already mediates TCP connections in process,
which is a good fit for active-stage policy. It still needs explicit active
policy plumbing:

- add a policy update path to the `dnsproxy.Runtime` used by the file-handle
  network
- apply the effective stage policy before each guest command
- register gateway scope tokens against the effective stage policy
- inject gateway environment variables from the effective stage policy
- close active file-handle TCP proxies when leaving a stage whose policy allowed
  destinations that the next stage denies

As with Firecracker, existing connections must be revoked when the stage policy
narrows. Blocking only new DNS answers or new TCP dials is not enough for "no
egress at execution time".

## Cache Implications

Stage-cache metadata must account for the network policy used to produce a
stage output.

The safest initial implementation can keep using the full compiled policy hash
in workspace, dependency, and services stage keys. That is conservative: changing
execution-only egress invalidates setup-stage caches even when their effective
egress did not change.

A more precise follow-up is to add per-stage network policy hashes:

- workspace cache includes the `workspace` effective network policy hash
- dependency cache includes the `dependencies` effective network policy hash
- services cache includes the `services` effective network policy hash

Do not restore a dependency or services cache produced under a broader effective
stage policy into a sandbox whose current stage policy is narrower unless there
is a deliberate compatibility proof. Exact effective-policy hash matching is the
right first rule.

## Observability

Stage-scoped egress should make policy decisions visible:

- include `cleanroom.network.stage` on execution spans and run observations
- include the effective policy hash or stage-policy hash where available
- include the stage in DNS-deny and blocked-connection warnings
- include the stage in gateway allow or deny audit logs

This keeps setup-time network failures distinguishable from workload-time policy
denials.

## Implementation Plan

### Merged prerequisite: `network.allow` syntactic sugar

The shorthand `host:port` and single-scalar `allow` forms are handled outside
this plan. Stage-scoped egress reuses the same normalized allow-rule parser for
global and stage-local allowlists.

### Current PR: Stage-scoped policy and darwin-vz active egress

Scope:

- Add stage-local network config to repository, dependencies, services, and run
  policy sections.
- Add effective-policy helpers and unit tests.
- Add `NetworkStage` to `backend.ExecutionRequest`.
- Set the stage at repository, dependency, services, pre-run, and workload
  command boundaries.
- Add unit tests proving each bootstrap path sends the expected stage.
- Treat helper/file-transfer commands explicitly rather than letting them inherit
  the previous stage.
- Darwin VZ: add active policy updates to the file-handle DNS runtime.
- Darwin VZ: register per-command gateway scope using the effective stage
  policy.
- Darwin VZ: revoke active file-handle TCP proxies when the stage narrows.
- Use effective stage policy for gateway env vars and gateway authorization.
- Firecracker: fail closed for stage-scoped policies until it has an active
  egress-rule update path.
- Update documentation to describe stage-local network blocks and backend
  support boundaries.

Definition of done: policy validation accepts stage-local network blocks, every
backend command receives an intentional network stage, darwin-vz file-handle
enforces the active effective policy, and unsupported backends reject
stage-scoped policies instead of widening egress.

### Follow-up PR: Firecracker active egress

Scope:

- Refactor host networking into stable baseline rules plus active egress rules.
- Add a policy update path for trusted DNS runtime and direct-IP rules.
- Flush active egress rules and revoke established flows at stage boundaries.
- Use effective stage policy for gateway env vars and gateway authorization.

Definition of done: Firecracker can fetch during dependency or service
bootstrap and fail closed for the same host during execution.

### Follow-up PR: Cache Metadata And Runtime Proof

Scope:

- Add exact effective-policy hash metadata to stage-cache records, or document
  the full-policy-hash conservative behavior for the first implementation slice.
- Add e2e coverage for cold and warm paths.
- Prove a warm dependency or services cache does not re-enable execution egress.
- Add examples that allow dependency or services egress while keeping execution
  offline.

Definition of done: stage-scoped egress is covered by docs, unit tests, and at
least one backend e2e proof.

## Resolved Decisions

- Retain global `sandbox.network.allow` as the legacy all-stage fallback when no
  stage-local network block is configured.
- Reject policies that combine global `sandbox.network.allow` with stage-local
  network blocks.
- Treat omitted stage-local network blocks as empty allowlists once any
  stage-local network block is configured.
- Reuse `execution` for helper/file-transfer commands instead of adding a public
  `internal` stage.

## Open Questions

- Is exact stage-policy hash matching enough for cache reuse, or do we eventually
  want a subset/superset proof for narrower policies?
