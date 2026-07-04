# Cleanroom Bakes Repeatable Warm Spores

**Status:** Active
**Last reviewed:** 2026-07-03
**Related code:** `internal/cli/vm.go`, `internal/gateway/`, `internal/policy/`
**Related upstream:** `~/Develop/sporevm` `origin/main` spore CLI, spore format, named lifecycle

## Summary

Cleanroom bakes a repository's declared policy into a repeatable warm spore —
an environment with dependencies installed and caches hot, provenance
attached, and network policy enforceable wherever it runs. SporeVM consumes
the artifact with its native verbs; cleanroom owns no runtime lifecycle.

```console
cleanroom bake . --out repo.spore              # slow path, runs rarely

spore run --from repo.spore 'make test'        # fast path, runs constantly
spore fork repo.spore --count 100 --out agents/
```

Cleanroom is the build tool; spore is the run tool. The split gives each side
one job: cleanroom answers "what environment is this repository entitled to,
and where did this artifact come from"; SporeVM answers everything about
running it.

Cleanroom provides three capabilities:

- **bake** — compile repo policy into SporeVM configuration (failing closed on
  anything SporeVM cannot enforce), boot a builder VM, materialise the repo,
  run declared warmup steps, and capture the result as a spore with provenance
  annotations.
- **verify** — check a spore's provenance facts and report what a consumer
  needs to run it.
- **gateway** — a host-side mediation service bound into guests so warmup and
  runtime workloads reach credentials and APIs without secrets ever entering
  the sandbox or the artifact.

## Problem

Repositories need sandboxed environments that are fast to start, safe to run
untrusted work in, and cheap to multiply. SporeVM provides the runtime
primitives — warm checkpoints, copy-on-write fork and fan-out, network
enforcement baked into the spore manifest — but it has no opinion about
repositories: what image a project needs, what egress it is allowed, what
warmup makes it useful, or how to trust an artifact someone hands you.

Without a bake layer, every consumer re-derives that per-repo setup by hand:
boot, install dependencies, hope the network access matches what the project
actually needs, and lose all record of how the environment was produced. Agent
fan-out makes this acute — a hundred forked workers are only useful if the
parent checkpoint already has the repo present, dependencies installed, and
credentials reachable without being baked in.

Cleanroom fills exactly that gap and nothing more. Spores are always warm
checkpoints (the format has no config-only or template form), so the natural
artifact for a policy compiler to emit is the checkpoint itself.

## Goals

- One headline command: `cleanroom bake [dir] --out <spore>` producing a spore
  that `spore run --from`, `spore fork`, and `spore fanout` consume natively.
- Fail closed at bake time on any policy SporeVM cannot enforce.
- Repeatable artifacts: pinned inputs, recorded facts, idempotent rebake keyed
  on those facts.
- Secrets never enter the artifact: warmup reaches credentials only through
  the gateway; nothing secret is resident when the checkpoint is captured.
- Native fan-out stays native: forked children share one gateway per lineage
  with no per-child orchestration.
- `spore` is the only runtime interface, spoken as a subprocess. No libspore
  linkage, no cgo; releases stay `CGO_ENABLED=0`.

## Non-Goals

- No runtime lifecycle verbs. Cleanroom does not wrap `run`, `exec`,
  `resume`, `fork`, `fanout`, or `destroy`. The bake builder VM is ephemeral
  build infrastructure created and destroyed inside one `bake` invocation.
- No bidirectional workspace sync. Bake copies the repo in once; refresh
  strategies live in guest workflows, not a sync engine.
- No sidecar metadata store. Provenance lives in SporeVM annotations only.
- No Buildkite-opinionated policy pushed upstream. Upstream asks stay generic.
- No distributed scheduling or spore registry service. `spore pack/push/pull`
  already covers distribution.

## Target Model

### Ownership Boundary

SporeVM owns: VM lifecycle, the spore format and its restore semantics,
network enforcement, fork/fan-out, rootfs and cache operations, annotation
storage and snapshot merge semantics, distribution (`pack`/`push`/`pull`).

Cleanroom owns: the policy schema and its compilation, the bake pipeline,
provenance facts and verification, and the gateway service.

```
╭──────────────────────────────╮
│        cleanroom bake        │
│  compile policy (fail closed)│
│  spore create + copy-in      │──▶ repo.spore ──▶ spore run --from
│  warmup via gateway          │    (provenance    spore fork --count N
│  spore suspend --out         │     annotations)  spore fanout
╰──────────────┬───────────────╯
               │ binds during warmup          one socket per lineage
               ▼                                       ▼
╭──────────────────────────────╮        ╭──────────────────────────╮
│   cleanroom gateway serve    │◀───────│  forked children (1..N)  │
│   secrets stay host-side     │        │  attribution via         │
╰──────────────────────────────╯        │  generation identity     │
                                        ╰──────────────────────────╯
```

### Bake Contract

`cleanroom bake [dir] --out <spore>` runs one pipeline:

1. **Compile.** Load `cleanroom.yaml` from `[dir]`, validate the enforceable
   subset, translate to `spore create` inputs. Any policy SporeVM cannot
   enforce fails here with a specific error.
2. **Create.** Start a builder VM via `spore create` with the translated
   network rules, resources, image, and provenance annotations.
3. **Materialise.** Copy the repository into the guest via `spore copy-in`.
   One direction, once.
4. **Warm.** Run the policy's declared warmup steps via `spore exec`. Steps
   that need credentials reach them through the bake-time gateway binding;
   bake fails closed if warmup is configured with raw secret environment
   variables.
5. **Capture.** `spore suspend --out <spore>`, then destroy the builder VM.
   Create-time annotations merge into the snapshot manifest (SporeVM
   annotation semantics), so the artifact carries its provenance.
6. **Verify.** Run the verify checks against the produced spore before
   reporting success.

Bake invokes `spore` as subprocesses and may consume its `--json` output. It
must not supervise anything that outlives the bake invocation.

### Repeatability Contract

A warm spore is not bit-reproducible; it is repeatable in the lockfile sense:
pinned inputs, recorded facts, equivalent rebake.

The bake key is the hash of: policy hash, resolved image digest, declared
input files (lockfiles by default, or the repo commit), and the warmup step
definitions. All components are recorded as annotations. `bake` is idempotent:
when `--out` already contains a spore whose recorded key matches, it no-ops.
`verify` audits the same facts, so a spore someone hands you is checkable
against the repo that claims to have produced it.

Source staleness is handled by pinning, not syncing: the artifact records the
commit it was baked at. Fan-out consumers typically want that fixed base;
interactive consumers refresh inside the guest (`git fetch` is a small delta,
and the policy already allows the repo host). Keying on lockfiles rather than
HEAD keeps rebakes rare.

### Policy Contract

Supported at bake time:

- `sandbox.image.ref`
- resources that map directly to SporeVM memory/vCPU options
- global exact host-plus-port `sandbox.network.allow`
- warmup steps (dependency installation and cache-priming commands run before
  capture)
- mediation requests: which gateway services the repo needs and at what scope
  (for example `inference: anthropic`, `git-credentials: read-only`). These
  are requests, not grants; they compile into the policy hash and provenance
  so the artifact's asks are tamper-evident.

Rejected, failing closed at compile: stage-scoped network policy, Docker
service, wildcard/CIDR/SNI/HTTP network rules, live per-exec policy updates.
Guest commands use absolute paths; SporeVM exec does no PATH lookup.

### Provenance Contract

Facts live in SporeVM annotations under `dev.buildkite.cleanroom.*`,
provenance version `1`, written at builder-create time: cleanroom version,
policy source and hash, image ref and digest, bake key, repo commit, remote,
dirty state, gateway service requirements (stable service name and guest
endpoint only — never host socket paths), and whether warmup was
gateway-mediated.

`cleanroom verify` reads `spore --json inspect` output (stdin or by invoking
it), fails closed on missing or malformed provenance, and reports the gateway
bindings a consumer must provide — including the exact `spore run --from` or
`spore fork` invocation that satisfies them.

### Gateway Contract

The gateway is scoped to a **spore lineage** — a baked spore and every fork of
it — not to individual VMs:

- Authorization is per lineage. All forks of one baked spore share the same
  repo, policy, and entitlements; they are the same security principal. One
  `cleanroom gateway serve --socket <path>` serves the whole lineage, and
  every child binds the same socket. Different repos get different gateways.
- Attribution is per guest, presented by the guest. SporeVM's generation
  device injects per-child fan-out identity; workloads present it in requests
  (a header, not a socket). The gateway uses it for attribution, rate limits,
  and audit — never for authorization, which is decided by which socket the
  request arrived on.
- Secrets stay host-side. The gateway holds or fetches credentials on the
  host and mediates access; guests see responses, not keys. The same binding
  serves bake-time warmup and runtime consumers.

What a lineage may access is the intersection of three layers, because no
single party can be trusted to decide alone:

1. **The repo requests.** Mediation requests from the policy contract,
   recorded in provenance and hash-bound to the policy. A rebake that widens
   them changes the policy hash and bake key, making the widening visible.
2. **The host grants.** `gateway serve` reads operator-side runtime config
   (XDG) declaring which credential sources it holds and which lineages
   qualify, matched on verified provenance facts:

   ```yaml
   # ~/.config/cleanroom/gateway.yaml
   grants:
     - match: { remote: "https://github.com/example-org/*" }
       services: [anthropic-inference, github-token-readonly]
   ```

3. **The binding is the capability.** `gateway serve --for <spore>` verifies
   the spore's provenance, resolves requested ∩ granted, and serves exactly
   that on its socket. The operator's act of passing the socket to
   `spore run --from`/`fork` connects the lineage to the grant — the same
   trust move as mounting a secret into a container, protected host-side by
   socket file permissions. The model makes grants explicit and auditable;
   it does not pretend to prevent operator misuse.

On Kubernetes the same layers map onto platform primitives: the gateway runs
as a per-pod native sidecar sharing an `emptyDir` socket, grants become
ServiceAccount plus workload identity, and the requested-∩-granted check moves
to admission (signed spore artifacts paired with ServiceAccounts under
admission policy). The gateway binary is identical; only the trust wiring
around it changes. Lineage scoping is an authorization semantic, not a process
topology — per-pod sockets are fine because the scope derives from identity,
not the socket. See `docs/plans/native-kubernetes-cleanroom.md`.

```console
cleanroom gateway serve --socket gw.sock &
spore fork repo.spore --count 100 --out agents/ \
  --bind-service cleanroom-gateway=unix:gw.sock
```

The gateway knows nothing about VM lifecycle. Whoever runs VMs (a developer's
shell, a CI job, an agent coordinator) starts one gateway per lineage — one
background process, regardless of fan-out width.

## Upstream Prerequisites

All verified against `sporevm/sporevm` main: the library API supports each
item; the `spore` CLI does not yet expose them.

1. **Create-time inputs:** annotations, exact host-plus-port allow rules, and
   bound-service declarations on `spore create` — ideally one spec-file flag
   (`--options @file.json`) mapping to `CreateNamedOptions`, subsuming the
   individual flags and avoiding shell-quoting hazards.
2. **Bound-service bindings on consumers:** a flag on `spore run --from`,
   named `spore resume`, `spore fork`, and `spore fanout` to bind an existing
   Unix socket for a service the manifest requires.
3. **Annotations on capture:** expose snapshot-time annotations on
   `spore suspend` (library `SnapshotNamedOptions.Annotations`) so bake can
   record capture-time facts.
4. **Resume readiness fail-closed:** resume currently reports success even
   when the restored monitor never becomes ready; it must wait or fail, as
   create does.

Bake slices that depend on an item land after it; nothing degrades silently.

## Current State

The repo currently contains a libspore-linked adapter (`internal/sporevm/`)
behind top-level lifecycle commands (`internal/cli/vm.go`), plus hidden legacy
daemon/control-plane commands. Mapping into this plan:

- Policy validation and translation (`validateVMPolicy`, `vmNetworkRules`) and
  annotation construction (`vmCreateAnnotations`, git facts) become the
  compile and stamp stages of bake.
- Provenance parsing and gateway-requirement checks
  (`vmCleanroomProvenanceFromAnnotations`, `vmResumeGatewayBindings`) become
  verify.
- The libspore cgo client, its build tag, and the top-level
  `create`/`exec`/`capture`/`resume`/`destroy` commands are removed.
- `internal/gateway/` becomes the standalone gateway service.

## Delivery Strategy

### Slice 1: Compile And Stamp As Pure Stages

Status: implemented in this branch.

- Extracted policy compilation and provenance-fact collection into
  `internal/bake` (`Compile`, `Stamp`, `AnnotationArgs`, `QuoteArgs`), with
  `cleanroom compile` / `cleanroom stamp` plumbing commands and the legacy
  create path delegating to the same functions.
- Verified end to end: `spore create <name> $(cleanroom compile <dir>)` boots
  a VM with translated image, memory, and vCPUs; unsupported policy fails
  closed with the established messages.
- Implementation notes: cleanroom policy sizes are decimal (`1gb` = 10^9)
  while spore's `--memory` requires host-page alignment (16KiB on macOS
  arm64, 4KiB on Linux), so compile rounds memory up to the next 16KiB —
  portable to both. Network rules emit `--allow-host-port`, which today's
  spore rejects; that path lights up when Slice 2 lands (fails loudly, never
  widens). Stamp output contains shell-quoted values, so composed invocations
  go through `eval`; the Slice 3 spec file removes that requirement.

### Slice 2: Upstream CLI Parity

- Land prerequisites 1–4 in sporevm (small flag plumbing to existing library
  options, plus the resume readiness fix).

Done when annotations, exact host-port rules, and socket bindings flow through
`spore create`, `spore suspend`, `spore run --from`, `spore fork`.

### Slice 3: Bake

Status: done. Verified end to end against spore 0.4.0 (upstream #347/#348/#349
fixed the SPIO bulk transport, inspect's v1-manifest rejection, and the
socket-path panic): bake of a real git workspace with a dependency-installing
warmup, restore via `spore run --from` using the installed dependency,
idempotent rebake no-op, and `spore fork --count 3` of the baked artifact
with per-child generation identity visible in-guest.

Residual upstream items, tracked for later slices: netd public egress times
out (DNS proxies, TCP connect never completes; affects old `--allow-host`
too), so network-fetching warmup steps are blocked until the outbound proxy
works; `spore fork` supports only 1-vCPU spores, so fan-out-destined bakes
must not set `resources.vcpus` above 1.

- Implemented the bake pipeline (`internal/bake/bake.go`, `cleanroom bake`):
  compile → create builder → copy-in → warmup → suspend → verify, with the
  bake key (`internal/bake/key.go`), idempotent no-op for clean workspaces,
  dirty-workspace fail-closed rebake, ephemeral builder cleanup on failure,
  and a spore >= 0.3.1 version gate. `sandbox.warmup` (flat shell-command
  list) added to the policy schema and covered by the policy hash.
- Removed the libspore adapter (`internal/sporevm/`), the top-level
  lifecycle commands, the libspore build tag, and the Go bindings dependency.
- The three upstream bugs this slice surfaced (SPIO bulk transfer capped at
  ~3KB, inspect rejecting v1 multi-vCPU manifests, monitor panic on oversized
  socket paths) were fixed in sporevm #347/#348/#349.

Done when `cleanroom bake . --out repo.spore` then
`spore run --from repo.spore '/bin/sh -c "cd /workspace && make test"'`
works end to end on a repo with dependencies, and rebaking without input
changes is a no-op. Met on 2026-07-04 (network-fetching warmup variant
deferred behind the netd egress fix).

### Slice 4: Verify

- `cleanroom verify` against `spore --json inspect` output: provenance checks,
  bake-key audit, and required-binding reporting with runnable invocations.

Done when a baked spore verifies, a foreign spore fails closed, and the
reported `spore run --from` command works verbatim.

### Slice 5: Gateway Per Lineage

- `cleanroom gateway serve --for <spore> --socket <path>` hosting the
  mediation service: verify provenance, resolve requested ∩ granted from the
  grants runtime config, serve only that scope.
- Mediation requests in the policy schema and provenance.
- Bake-time binding for warmup credentials, fail-closed on raw secret env.
- Guest attribution via generation identity in requests.

Done when a fan-out batch of forked children all reach one gateway through a
shared bound socket, per-child attribution appears in gateway logs, and no
credential material appears in the captured spore (checked by grepping guest
disk and a memory-chunk scan for a known canary secret).

### Slice 6: Release And CI

- goreleaser stays `CGO_ENABLED=0`; drop the libspore build tag from
  `scripts/build-go.sh`.
- CI smoke: install pinned spore via mise; bake → run --from → fork → verify →
  destroy.

Done when a released binary bakes on a clean host with only spore installed
and CI exercises the full pipeline.

### Slice 7: Delete Old Runtime Surface

- Delete `internal/sporevm/` and the hidden daemon/control-plane commands and
  their backend adapter paths.

Done when cleanroom has one user-facing model and no dormant second runtime.

## Verification

- Unit: compile translation and fail-closed rejection; stamp facts; bake-key
  computation; verify parsing against recorded `spore --json inspect`
  fixtures (fixtures pin the annotation merge-through-snapshot behavior so an
  upstream semantic change fails loudly here).
- Runtime smoke (CI, Slice 6):

```console
cleanroom bake . --out cr-test.spore
spore run --from cr-test.spore '/bin/sh -c "cd /workspace && make test"'
spore fork cr-test.spore --count 3 --out forks/
spore --json inspect cr-test.spore | cleanroom verify
cleanroom bake . --out cr-test.spore   # must no-op
```

- Network smoke: a policy allowing `github.com:443` permits that fetch in a
  consumer VM and blocks `example.com`.
- Secret canary: bake with a gateway-mediated canary credential; assert the
  canary appears nowhere in the captured spore's disk or memory chunks.

## Resolved Decisions

- The artifact is a warm spore. Spores are always checkpoints; cleanroom emits
  the checkpoint, not configuration for someone else to boot.
- `spore` (the CLI) is cleanroom's only runtime interface, invoked as a
  subprocess. Machine output (`--json`) is the contract.
- Enforcement lives in the spore manifest and is applied by SporeVM on every
  resume and fork; cleanroom's verify is integrity and UX, not the security
  boundary.
- Gateway scope is the spore lineage. Authorization per lineage via the bound
  socket; attribution per guest via generation identity presented in
  requests.
- Provenance lives in SporeVM annotations; no sidecar store.
- Repeatable means pinned inputs and idempotent rebake, not bit-for-bit
  reproducibility, and the plan says so plainly.
- Bake owns an ephemeral builder VM inside one invocation and supervises
  nothing beyond it.

## Open Questions

- Warmup schema shape: a flat command list under `sandbox.warmup`, or named
  steps with per-step network scope? Default: flat list first; per-step scope
  only when a concrete policy needs it.
- Should bake refuse a dirty worktree by default (record-and-warn vs fail)?
  Default: warn and record `workspace.git.dirty=true`; `--require-clean` for
  CI.
- Single-VM convenience for gateway startup (a launcher hook in spore, or a
  small `cleanroom run` bridge): deferred until the lineage-scoped model
  proves insufficient for interactive use.
- Grant-rule matching semantics: which provenance facts are matchable
  (remote, org, policy hash), glob vs exact, and whether a dirty-worktree
  bake can ever qualify for grants. Default: match on remote and policy
  hash, exact-or-glob on remote only, dirty bakes excluded from grants.

## Key Learnings From Pressure-Testing

- Secrets in the checkpoint are the sharpest risk: anything resident during
  warmup can be captured into memory chunks and distributed. The gateway is
  load-bearing for bake, not an add-on; the canary check in Verification
  exists because of this.
- Fan-out breaks any design that scopes the gateway per VM. Scoping
  authorization to the lineage and moving attribution into guest-presented
  identity keeps native `spore fork --count N` intact with one host process.
- Annotation merge-through-snapshot is the load-bearing upstream fact for
  provenance; verify fixtures pin it.
- Baked source goes stale by design; pinning plus in-guest refresh is the
  contract. A sync engine here would regrow the product this plan deletes.
- Bake is where lifecycle ownership could creep back in. Confining it to an
  ephemeral builder VM inside one invocation keeps the boundary honest.
