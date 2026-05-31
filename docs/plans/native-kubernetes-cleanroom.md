# Native Kubernetes Cleanroom Plan

**Status:** Proposed
**Last reviewed:** 2026-06-01
**Spec references:** `docs/api.md`, `docs/backends.md`, `docs/policy.md`, `docs/caching.md`, `docs/remote-access.md`, `docs/observability.md`
**Related plans:** `docs/plans/layered-caching.md`, `docs/plans/zfs-stage-cache-replication.md`, `docs/plans/multi-principal-control-server.md`, `docs/plans/sandbox-suspend-wake.md`

## Summary

Make Cleanroom a Kubernetes-native sandbox runtime instead of a daemon that
creates hidden VMs behind Kubernetes' scheduler.

Kubernetes should own desired state, scheduling, bin packing, coarse tenancy,
load balancing, lifecycle visibility, and cluster-level failure recovery.
Cleanroom should own the runtime semantics that are specific to secure
repository sandboxes: immutable repository policy, microVM isolation,
deny-by-default egress, host-side gateway and cache mediation, exact-principal
resource ownership, execution and file APIs, snapshots, and backend-specific
runtime setup.

The preferred northbound integration is the Kubernetes SIG Agent Sandbox API.
Cleanroom should fit underneath that API as a runtime implementation before
inventing a competing agent-facing object model. If Cleanroom later needs its
own CRDs, they should follow the same Kubernetes conventions and stay narrow.

KubeVirt is not a replacement for this split. It is a possible VM substrate
under Cleanroom for clusters that want Kubernetes-managed QEMU/KVM VMs.
Firecracker remains the first backend target because Cleanroom already has
Firecracker runtime, gateway, and cache behavior.

## Problem

Cleanroom's current control model is server-oriented. The server process is
authoritative for sandbox lifecycle and execution state, and clients create
sandboxes through the Cleanroom API. That model works for local hosts and
shared servers, but it is awkward in Kubernetes at large scale.

The main failure mode is hidden capacity. If a small Kubernetes pod or central
service can start arbitrary VMs on a node, the Kubernetes scheduler cannot make
correct placement decisions. CPU, memory, disk, KVM capacity, tap devices, ZFS
clone capacity, cache locality, and node-specific backend support become
invisible to bin packing, quotas, autoscaling, preemption, and disruption
handling.

A Kubernetes-native design should make every sandbox visible as desired state
and as a schedulable resource envelope before the VM starts. The node runtime
should reconcile only the sandboxes Kubernetes assigned to that node.

## Goals

- Represent each Cleanroom sandbox as one Kubernetes-schedulable unit.
- Let Kubernetes perform normal placement, bin packing, quota enforcement,
  topology spreading, priority, preemption, and autoscaler integration.
- Keep Cleanroom's policy, gateway, cache, execution, auth, and backend
  invariants intact.
- Prefer Agent Sandbox `Sandbox`, `SandboxTemplate`, `SandboxClaim`, and
  `SandboxWarmPool` as the user-facing Kubernetes API.
- Keep privileged host operations inside a node-local Cleanroom runtime.
- Keep user-facing pods unprivileged or narrowly scoped.
- Surface lifecycle through Kubernetes status conditions and events.
- Use Kubernetes `Service` and Gateway API patterns for routing instead of
  ad hoc per-host port allocation.
- Use Kubernetes namespace, RBAC, quota, admission, and NetworkPolicy as the
  coarse cluster boundary.
- Fail closed when a backend, node capability, policy, owner, cache lineage, or
  gateway scope cannot be validated.

## Non-goals

- Do not build a custom scheduler in the first version.
- Do not make a central `cleanroom serve` deployment start hidden VMs on
  arbitrary nodes.
- Do not replace Cleanroom repository policy with Kubernetes NetworkPolicy.
- Do not expose host privileged sockets or ZFS/KVM operations to user pods.
- Do not store large logs, stdout, filesystem blobs, policy blobs, or cache
  records in Kubernetes status.
- Do not make warm pools hand out pre-owned user sandboxes in the first version.
- Do not require KubeVirt for the first Kubernetes deployment.
- Do not make KubeVirt the public product API.
- Do not add compatibility paths for older Kubernetes-specific prototypes until
  a first production shape exists.

## Target Model

The target shape is a normal Kubernetes reconciliation stack:

```text
SandboxClaim or CleanroomSandbox
        |
Cleanroom Kubernetes controller
        |
scheduled sandbox workload
        |
node-local Cleanroom runtime
        |
Firecracker, KubeVirt, or another backend
        |
Cleanroom guest agent, gateway, cache, execution, files
```

The cluster controller observes declarative Kubernetes objects, validates the
request, compiles Cleanroom policy, creates child resources, and reports status.
The Kubernetes scheduler places the sandbox workload. The node-local runtime
starts and manages the VM only after Kubernetes has assigned the workload to a
node with the required capabilities.

### Ownership Boundaries

Kubernetes owns:

- desired state objects
- scheduling and bin packing
- namespace-level tenancy
- RBAC and admission
- resource quotas and limit ranges
- workload replacement after pod or node failures
- Services, Gateway routes, and outer NetworkPolicies
- status conditions and events visible to cluster operators

Cleanroom owns:

- repository policy compilation and policy hashes
- backend capability checks
- sandbox VM lifecycle on the assigned node
- guest execution and file operations
- exact-principal resource ownership
- gateway authorization envelopes
- dependency and service cache keys
- stage-cache lineage and import validation
- backend-specific storage, networking, and guest-agent behavior

## API Strategy

### Preferred Northbound API: Agent Sandbox

The first Kubernetes integration should target Agent Sandbox rather than define
a competing high-level API.

The mapping is:

| Agent Sandbox concept | Cleanroom responsibility |
| --- | --- |
| `SandboxTemplate` | Runtime image, adapter pod shape, backend class, coarse NetworkPolicy, service exposure defaults |
| `SandboxClaim` | Request for one policy-bound Cleanroom sandbox and its owner identity |
| `SandboxWarmPool` | Warm adapter capacity and backend/runtime cache warmup |
| `Sandbox` status | Readiness, suspension, failure, service identity, route metadata |
| SDK command/file operations | Cleanroom execution and file APIs |

Cleanroom-specific fields should start as annotations or template conventions
only if the Agent Sandbox API has no field for them. If a field becomes
durable, prefer upstreaming it or adding a small Cleanroom extension CRD rather
than depending on undocumented annotation contracts.

### Optional Cleanroom CRD

If a native Cleanroom object is needed, keep it close to Kubernetes API
conventions and avoid imperative verbs:

```yaml
apiVersion: cleanroom.buildkite.com/v1alpha1
kind: CleanroomSandbox
metadata:
  name: buildkite-test-abc123
spec:
  repository:
    url: https://github.com/buildkite/cleanroom.git
    commit: abc123
  policy:
    configMapRef:
      name: cleanroom-policy
      key: cleanroom.yaml
  backendClassName: firecracker-zfs
  resources:
    vcpus: 4
    memory: 8Gi
    disk: 32Gi
  operatingMode: Running
status:
  observedGeneration: 4
  sandboxID: sbx_01abc
  policyHash: sha256:...
  conditions:
    - type: PolicyCompiled
      status: "True"
      reason: Valid
    - type: Ready
      status: "True"
      reason: RuntimeReady
```

Rules:

- `spec` is desired state.
- `status` is observed state.
- lifecycle changes use declarative fields such as `operatingMode`, not
  imperative status writes.
- finalizers guard runtime cleanup.
- child resources use owner references.
- status contains IDs, hashes, condition summaries, and route metadata only.

## Scheduling And Capacity

Every sandbox must have a Kubernetes-visible resource envelope before
placement:

```yaml
resources:
  requests:
    cpu: "4"
    memory: 8Gi
    ephemeral-storage: 32Gi
    cleanroom.buildkite.com/kvm: "1"
    cleanroom.buildkite.com/vm-slot: "1"
```

The controller derives this from:

- `cleanroom.yaml` policy resource floors
- backend runtime defaults
- backend class minimums
- repository bootstrap requirements
- Docker service requirements
- cache-output volume floors where they imply local disk pressure

Use standard Kubernetes placement features first:

- node labels for backend capability:
  - `cleanroom.buildkite.com/backend.firecracker=true`
  - `cleanroom.buildkite.com/storage.zfs=true`
  - `cleanroom.buildkite.com/kvm=true`
- taints and tolerations for dedicated sandbox nodes
- node affinity for backend classes
- topology spread constraints for availability
- priority classes for interactive versus batch sandboxes
- namespace `ResourceQuota` and `LimitRange`
- Cluster Autoscaler or Karpenter around visible pending workloads

Use a device plugin for scarce node-local resources in the first version:

- KVM slot availability
- maximum VM concurrency
- optionally tap-device or gateway slot limits

Use Dynamic Resource Allocation later if allocation needs structured
parameters, such as storage driver, cache locality, or per-sandbox device
metadata that a simple extended resource cannot express.

## Controller Design

The cluster controller runs as a normal Kubernetes controller with leader
election, watches, work queues, finalizers, status updates, and events.

Responsibilities:

- Watch Agent Sandbox objects and optional Cleanroom CRDs.
- Resolve repository policy source.
- Validate and compile policy using Cleanroom's existing parser and compiler.
- Resolve effective resources and backend class.
- Create or patch the scheduled sandbox workload.
- Create or patch Services, Gateway routes, NetworkPolicies, and narrow
  Secrets or projected tokens.
- Stamp owner metadata and policy hash.
- Set status conditions with `observedGeneration`.
- Emit Kubernetes Events for material lifecycle transitions.
- Requeue for expiration, suspend/wake, retryable runtime failures, and cleanup.
- Run finalizer cleanup for runtime resources and route resources.

The controller should not:

- start VMs directly
- run privileged host setup
- stream command output through CRD status
- choose nodes outside Kubernetes scheduling
- mutate policy after sandbox creation
- grant broad host access to adapter pods

## Node Runtime

Run the node runtime as a DaemonSet on nodes that advertise Cleanroom
capability.

Responsibilities:

- expose a node-local, authenticated control endpoint
- validate that requested sandboxes are scheduled to the local node
- prepare Firecracker/KVM networking
- manage rootfs and cache-output volumes
- manage ZFS/file snapshot drivers
- start, suspend, resume, and terminate VMs
- register and release gateway scopes
- publish runtime metrics and health
- clean orphaned VMs, TAP devices, firewall rules, and temporary volumes

The runtime is the only component with host privileges required for KVM,
networking, firewall, and storage operations. The adapter pod should receive a
per-sandbox token or projected service account identity that authorizes only
that sandbox.

## Scheduled Workload Shape

The first version should use an adapter pod as the schedulable workload:

```text
Pod cleanroom-sandbox-abc123
  container cleanroom-adapter
    - exposes Agent Sandbox runtime HTTP surface
    - talks to local Cleanroom node runtime
    - maps run/read/write/list/tunnel operations to Cleanroom APIs
```

The adapter pod carries the resource requests for the underlying VM. That makes
Kubernetes bin packing approximately correct even though the untrusted workload
runs in a VM started by the node runtime.

The adapter pod must not be privileged. It should not have raw access to the
node runtime socket unless that socket enforces per-sandbox authorization.

## Networking And Load Balancing

Use Kubernetes networking for entry and routing:

- create one Service per sandbox for stable in-cluster identity when needed
- use a shared router plus Gateway API for external access
- route by sandbox ID, claim name, or stable service name
- set readiness based on sandbox `Ready` condition
- keep per-sandbox direct load balancers out of the default path

Use Kubernetes NetworkPolicy as the outer perimeter:

- default deny namespace policy
- allow router to adapter service
- allow adapter to node runtime on the same node where supported
- allow adapter/runtime to Cleanroom gateway services
- deny metadata service and cluster-internal ranges unless explicitly required

Use Cleanroom policy inside the runtime:

- exact host and port egress allowlists
- stage-scoped network rules
- gateway-mediated Git, OCI, Go, RubyGems, Docker Hub, and fetch traffic
- owner-aware gateway envelopes
- deny-by-default behavior on unsupported backend capabilities

Kubernetes NetworkPolicy should not be treated as a replacement for Cleanroom's
repository policy. It is too coarse for stage-scoped repository egress and
host-side credential mediation.

## Storage And Caching

Keep storage layers explicit:

- Kubernetes PVCs are for lifecycle-visible sandbox state that must survive pod
  restart, reschedule, or suspension.
- node-local disks are for backend runtime artifacts and hot caches.
- Cleanroom stage-cache metadata remains Cleanroom-owned because it is keyed by
  policy, repository, owner, backend, runtime lineage, and storage driver.
- CSI snapshots can be used where they match the backend's semantics, but they
  do not replace Cleanroom user snapshots or system stage caches by default.

Warm pools should start with:

- prewarmed adapter pods
- pulled adapter images
- prepared Cleanroom base images
- populated transport caches
- populated stage caches where owner and lineage are safe

Warm pools should not initially provide pre-owned user sandboxes. Adoption would
need exact principal stamping, gateway envelope replacement, cache partitioning,
and file state guarantees before it is safe.

## Security Model

Kubernetes handles coarse access:

- namespace isolation
- RBAC for claim/template operations
- admission policy for allowed backend classes and resource ceilings
- ResourceQuota and LimitRange
- NetworkPolicy and Gateway policy
- service account identity

Cleanroom handles sandbox access:

- exact owner principal for sandboxes, executions, snapshots, files, streams,
  and cache metadata
- request-time authorization before repository mirrors, host credentials,
  snapshots, stage caches, or backend work are used
- immutable policy hash per sandbox
- backend capability validation before provisioning
- owner-aware gateway scopes
- fail-closed cache import and gateway behavior

Admission should reject:

- unsupported backend classes
- resource requests above namespace or tenant limits
- unpinned or disallowed images when policy requires pinned images
- dangerous allow-all egress unless the namespace is explicitly permitted
- templates that mount privileged host paths into adapter pods
- adapter pod specs that request privileged mode outside the node runtime

## Backend Notes

### Firecracker

Firecracker is the first Kubernetes backend target.

Expected shape:

- adapter pod is the Kubernetes-scheduled unit
- node runtime starts Firecracker on the assigned node
- KVM and VM slots are represented through extended resources
- ZFS-backed hosts use clone-backed stage-cache materialization
- file-backed hosts remain functional but should report degraded cache
  materialization capability

### KubeVirt

KubeVirt is a later backend option, not the primary product API.

Expected shape:

- the controller creates a KubeVirt `VirtualMachine` or
  `VirtualMachineInstance` as the scheduled VM resource
- Cleanroom still compiles policy, provides execution/file APIs, controls the
  gateway, and owns cache semantics
- the KubeVirt backend adapter maps Cleanroom sandbox lifecycle to KubeVirt
  lifecycle and status

Use this path only if Kubernetes-managed QEMU/KVM lifecycle, storage, or live
migration materially beats the Firecracker node-runtime path for a target
deployment.

### Backend Neutrality

Public Kubernetes-facing resources should name backend classes, not internal
runtime knobs. Backend-specific fields belong in `CleanroomBackendClass` style
runtime config or adapter internals.

## Observability

Expose both Kubernetes-native and Cleanroom-native signals.

Kubernetes-native:

- `status.conditions` for `PolicyCompiled`, `Scheduled`, `RuntimeReady`,
  `Ready`, `Suspended`, `Failed`, and `Expired`
- Events for policy validation failure, scheduling failure, runtime launch,
  ready, suspend, wake, termination, and cleanup failure
- metrics for controller reconcile latency, queue depth, errors, and condition
  transitions

Cleanroom-native:

- existing OTLP spans and metrics for sandbox creation, execution, gateway
  requests, cache lookup/import/export, and launch phases
- structured logs with `sandbox_id`, `execution_id`, `backend`, `reason_code`,
  Kubernetes namespace/name/UID, and owner principal
- retained execution observability artifacts outside CRD status

Metric labels must stay low-cardinality. Kubernetes object UID, sandbox ID, and
execution ID belong in logs and traces, not high-cardinality Prometheus labels.

## Current State

Cleanroom already has several pieces this plan can reuse:

- backend-neutral policy and resource floors in repository config
- ConnectRPC sandbox and execution APIs
- exact-principal auth for shared control servers
- gateway routes for Git, OCI, Docker Hub, Go modules, RubyGems, and fetches
- layered cache and stage-cache metadata
- Firecracker backend with KVM, TAP devices, host firewalling, and file/ZFS
  snapshot drivers
- suspend/wake lifecycle states and RPCs
- observability contracts for traces, metrics, logs, and retained execution
  diagnostics

The missing Kubernetes-native pieces are:

- controller reconciliation around Kubernetes objects
- scheduler-visible sandbox resource envelopes
- node-local runtime authorization for scheduled workloads
- adapter pod runtime surface for Agent Sandbox
- device plugin or DRA capacity exposure
- Service/Gateway routing integration
- Kubernetes status conditions and events

## Delivery Strategy

### Phase 1: Agent Sandbox Adapter Prototype

Build the smallest end-to-end integration without new Cleanroom CRDs.

Scope:

- package a `cleanroom-agent-sandbox-adapter` image
- run Cleanroom node runtime as a DaemonSet on one Firecracker-capable node pool
- define an Agent Sandbox `SandboxTemplate` for the adapter pod
- map Agent Sandbox run and file operations to Cleanroom execution and file APIs
- create one Cleanroom sandbox per claimed adapter pod
- set realistic pod resource requests from static template values

Definition of done:

- a `SandboxClaim` becomes ready
- an SDK command runs inside a Cleanroom VM
- file read/write flows through Cleanroom APIs
- Kubernetes schedules the adapter pod based on declared CPU, memory, storage,
  and extended resources
- deleting the claim terminates the Cleanroom sandbox and releases runtime
  resources

### Phase 2: Policy And Resource Reconciliation

Move from static template resources to policy-derived resources.

Scope:

- add a controller that resolves repository policy for a claim
- compile and persist the immutable policy hash
- derive effective CPU, memory, disk, Docker, and backend capability
  requirements
- patch or create the scheduled workload with those requests before VM start
- expose `PolicyCompiled` and `Ready` conditions

Definition of done:

- invalid policy fails before scheduling runtime work
- resource requests match the compiled policy and backend floors
- policy hash and effective resources are visible in status or annotations
- backend capability mismatch fails closed with an explainable condition

### Phase 3: Scheduler-Visible Node Capacity

Expose scarce runtime capacity to Kubernetes.

Scope:

- add node labels for backend support and storage driver support
- add taints for dedicated sandbox nodes
- add a device plugin for KVM and VM slots
- optionally expose local cache/storage capacity as a coarse extended resource
- document namespace `ResourceQuota` examples

Definition of done:

- pending claims remain pending when no capable node has capacity
- Kubernetes bin packs multiple claims on a capable node without overcommitting
  hidden VM slots
- autoscaler can react to pending adapter pods

### Phase 4: Routing And Network Policy

Make ingress and egress cluster-native without weakening Cleanroom policy.

Scope:

- create per-sandbox Services or stable Service records
- route external traffic through a shared router and Gateway API
- generate or document outer NetworkPolicies
- keep Cleanroom gateway and egress policy as the inner enforcement layer

Definition of done:

- clients can reach a sandbox through a stable Kubernetes route
- router targets only ready sandboxes
- NetworkPolicy blocks direct unwanted paths
- Cleanroom still denies policy-disallowed egress inside the VM

### Phase 5: Warm Capacity

Use Kubernetes warm pools and autoscaling for latency without violating owner
isolation.

Scope:

- warm adapter pods
- pre-pull images
- pre-materialize base runtime artifacts
- pre-populate transport caches
- add cache warming jobs or hooks where cache ownership is safe

Definition of done:

- warm claims have lower time to first instruction
- no warm pool member contains another principal's retained files, credentials,
  gateway scopes, or owner-partitioned cache entries
- stale template updates roll through warm pools without claim disruption

### Phase 6: Optional KubeVirt Backend

Evaluate KubeVirt as a backend only after the Firecracker path is proven.

Scope:

- implement a backend adapter that creates and observes KubeVirt VM resources
- map Cleanroom lifecycle to KubeVirt lifecycle
- retain Cleanroom execution, file, policy, gateway, and cache semantics
- compare density, startup time, storage, networking, migration, and operator
  complexity against Firecracker

Definition of done:

- one policy-bound Cleanroom sandbox runs through KubeVirt
- the same Cleanroom API contract works across Firecracker and KubeVirt
- unsupported KubeVirt features fail closed with clear conditions

## Verification

Unit tests:

- policy-to-resource-envelope resolution
- backend class validation
- condition transitions
- owner and policy hash stamping
- finalizer cleanup planning
- adapter authorization checks

Controller integration tests:

- claim creates child workload and status
- invalid policy sets failure condition and creates no runtime workload
- deletion runs finalizers and removes child resources
- suspend and resume update desired state and conditions
- status updates use `observedGeneration`

Cluster smoke tests:

- kind or real-cluster adapter smoke for API behavior
- Firecracker-capable node-pool smoke for actual VM launch
- multiple concurrent claims prove scheduler-visible bin packing
- quota exhaustion leaves claims pending or denied predictably
- router/Gateway smoke for command and port traffic
- NetworkPolicy smoke proves direct disallowed paths fail

Performance tests:

- time to first instruction for cold and warm claims
- controller reconcile throughput under claim bursts
- node runtime launch concurrency
- cache hit and miss latency
- autoscaler behavior when node pools are empty

Security tests:

- one principal cannot read another principal's sandbox, execution, files,
  snapshots, or cache entries
- adapter token cannot control a sandbox other than its own
- dangerous egress override is denied by admission unless explicitly allowed
- gateway denies ownerless or mismatched scopes under auth
- unprivileged adapter pod cannot access host runtime operations directly

## Key Learnings From Pressure-Testing

- The main Kubernetes objection would be hidden capacity. The plan therefore
  makes one sandbox equal one scheduled workload and requires honest resource
  requests before VM launch.
- A custom scheduler would raise unnecessary complexity early. The plan starts
  with normal scheduler primitives, node labels, taints, quotas, and a device
  plugin before considering DRA or scheduler extensions.
- Warm pools are useful but dangerous if they contain principal-bound state.
  The first warm-pool slice warms adapter/runtime/cache capacity, not adopted
  user sandboxes.
- Kubernetes NetworkPolicy is useful as a perimeter, but it cannot express
  Cleanroom's stage-scoped repository and gateway policy. The plan keeps both
  layers separate.
- KubeVirt is valuable as a backend candidate, but making it the public API
  would bypass Agent Sandbox and would not provide Cleanroom policy, gateway,
  cache, or ownership semantics by itself.
- CRD status can become a data sink. The plan keeps large artifacts in
  Cleanroom stores and exposes only condition summaries, IDs, hashes, and
  routes through Kubernetes objects.

## Resolved Decisions

- Use Agent Sandbox as the preferred user-facing Kubernetes API.
- Keep Cleanroom as the runtime semantics layer.
- Start with Firecracker and a node-local runtime.
- Treat KubeVirt as a later backend option.
- Use adapter pods as the first schedulable unit.
- Make resource requests honest enough for Kubernetes bin packing.
- Use device plugins for first-slice scarce node-local capacity.
- Keep Cleanroom policy enforcement inside the runtime.
- Use Kubernetes Services and Gateway API for routing.

## Deferred Work

- Dynamic Resource Allocation for structured backend capacity.
- KubeVirt backend implementation.
- Controlled sharing across principals.
- Cross-node live migration or hibernation.
- Cross-host cache scheduler beyond configured cache peers.
- First-class UI for Kubernetes-native sandbox status.
- Multi-sandbox-per-pod density optimization.

## Open Questions

### Blocking The First Slice

- Should the first adapter target Agent Sandbox's existing runtime HTTP surface
  exactly, or should it expose a narrower compatibility shim first?

Recommended default: target the existing runtime surface for run and file
operations only. Defer tunnels and richer interactive behavior until the basic
claim lifecycle is proven.

- Should the first node runtime endpoint be a Unix socket mounted into the
  adapter pod or a localhost/node-local HTTPS endpoint?

Recommended default: use a node-local HTTPS endpoint with per-sandbox bearer
  tokens. Avoid host socket mounts into user-visible pods unless the
  authorization boundary is already strong.

### Needed Before Production

- Which Kubernetes versions and managed providers are in the support matrix?
- Which CNI implementations must pass NetworkPolicy and routing tests?
- Should cache storage be local PV, hostPath managed by the node runtime, or a
  CSI-backed volume class in the first production deployment?
- Should KubeVirt be evaluated for migration and storage reasons before any
  production launch, or only after Firecracker proves insufficient?

### Safe To Defer

- Whether DRA replaces the first device plugin.
- Whether warm pools can safely adopt pre-created Cleanroom sandboxes.
- Whether Cleanroom needs its own CRD after Agent Sandbox integration.
- Whether live migration belongs in Cleanroom, KubeVirt, or outside v1 scope.
