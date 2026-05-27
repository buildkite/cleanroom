# DeepSec Remediation Plan

**Spec reference:** `docs/spec.md`; `docs/api.md`; `docs/tls.md`; `docs/plans/multi-principal-control-server.md`; `docs/plans/stage-scoped-egress.md`
**Status:** Slice 6b ready for review
**Last reviewed:** 2026-05-27

## Summary

DeepSec revalidated 26 findings against the Cleanroom checkout on 2026-05-27.
All 26 are treated as true positives that need code, configuration, or CI fixes.

This plan tracks each finding separately while keeping implementation in
reviewable slices. A slice may close several findings when they share the same
trust boundary or cache key, but every finding below needs an explicit fix or a
documented product decision before it can be marked done.

## Current Progress

Slice 1 is implemented and ready for review. The darwin-vz file-handle gateway
now blocks host-side TCP proxy dials to loopback, private, link-local,
multicast, unspecified, documentation, benchmarking, reserved, NAT64, and other
special-use addresses before DNS policy authorization and before the host dial.
IPv6 destinations must also fall inside currently allocated global-unicast
prefixes.

Focused validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/backend/darwinvz
```

Result: passed.

Slice 2 is implemented and ready for review. It rejects file/local submodule
mirror remotes, canonicalizes mirror submodule remotes with the same repository
remote rules used for parent checkouts, and allows repository-controlled
submodule mirroring only when the resolved submodule host matches the already
validated parent repository host. This intentionally rejects cross-host
submodules until Cleanroom has an explicit submodule allowlist or host-side
policy contract for them. Slice 2 also keys mirror-backed submodule digests by
submodule path, mirror path, and gitlink SHA so two submodules that share a
remote cannot reuse the wrong commit's digest data.

Focused validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/controlservice ./internal/submodule ./internal/repositorystore ./internal/repositorychangeset
```

Result: passed.

Repository validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise run check
```

Result: passed.

Repository validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise run check
```

Result: passed.

Slice 3 is implemented and ready for review. It wires configured OIDC bearer
authentication into HTTP(S) control servers, refuses bearer auth on non-loopback
plain HTTP listeners, prevents clients from sending bearer tokens to non-loopback
plain HTTP endpoints, stamps server-derived owners onto sandboxes, executions,
snapshots, and interactive sessions, filters list APIs by owner, and checks
ownership on sandbox, execution, snapshot, file, port-dial, and repository
operations. Automatic guest-operation wakeups are authorized by the requested
operation before the wake starts; direct `ResumeSandbox` still requires
`sandbox.resume`.

This slice preserves existing local and unauthenticated default behavior unless
`auth.required` is enabled. Daemon install guardrails for TCP listeners remain
in the privileged installer hardening slice.

Focused validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/eaa8/cleanroom/mise.toml mise exec -- go test ./internal/authz ./internal/controlserver ./internal/controlclient ./internal/controlservice ./internal/cli ./internal/snapshotstore
```

Result: passed.

Repository validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/eaa8/cleanroom/mise.toml mise run check
```

Result: passed.

Slice 4a is implemented and ready for review. Daemon install now rejects
newlines, carriage returns, and NUL bytes in the generated service command
before writing systemd or launchd service files. It also refuses non-loopback
TCP control-plane daemon listeners unless runtime `auth.required` is enabled.
When auth is enabled, non-loopback plain HTTP is still rejected because bearer
tokens are only allowed over HTTPS or loopback HTTP.

The DNS resolver install and exposure certificate symlink hardening findings are
covered in Slice 4b.

Focused validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/eaa8/cleanroom/mise.toml mise exec -- go test ./internal/cli
```

Result: passed.

Repository validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/eaa8/cleanroom/mise.toml mise run check
```

Result: passed.

Slice 4b is implemented and ready for review. Exposure certificate reads now
reject symlinked certificate/key paths and symlinked TLS path ancestors, verify
the opened file still matches the inspected regular file, and write refreshed
certificate material through a same-directory temporary file before rename. The
privileged DNS installer also refuses to trust, remove trust for, or chown
certificate paths when the invoking user's TLS directory path contains symlink
components from the user's home or configured XDG config root, and it validates
the existing certificate/key pair before changing trust-store state. During
sudo installs, `XDG_CONFIG_HOME` must stay inside the invoking user's home, and
`dns status` reports that configuration error instead of hiding it. The
ownership handoff uses `lchown`.

Focused validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/eaa8/cleanroom/mise.toml mise exec -- go test ./internal/exposure
```

Result: passed.

Focused validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/eaa8/cleanroom/mise.toml mise exec -- go test ./internal/cli
```

Result: passed.

Repository validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/eaa8/cleanroom/mise.toml mise run check
```

Result: passed.

Slice 6a is implemented and ready for review. Git credential fill lookup now
rejects NUL, CR, and LF characters in line-based credential protocol fields
before invoking the host `git credential fill` helper. This closes the remote
URL path field injection finding without changing normal HTTPS repository
credential lookup behavior.

The remaining Slice 6 cache authorization findings for git pack responses,
fetch cache hits, and Go proxy cache hits remain follow-up work.

Focused validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/eaa8/cleanroom/mise.toml mise exec -- go test ./internal/gateway
```

Result: passed.

Repository validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/eaa8/cleanroom/mise.toml mise run check
```

Result: passed.

Slice 6b is implemented and ready for review. OCI registry handler creation is
now bounded by a small LRU cache, so request-controlled registry prefixes cannot
grow one handler per prefix for the lifetime of the daemon. Cache hits refresh
recency, evicted handlers have their closers released outside the cache lock,
and normal registry prefix reuse keeps the existing handler.

Focused validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/eaa8/cleanroom/mise.toml mise exec -- go test ./internal/gateway
```

Result: passed.

## Triage

| Finding | Severity | Decision | Remediation slice |
| --- | --- | --- | --- |
| File-handle gateway can host-dial private DNS answers | Critical | Needs fix | Slice 1: darwin-vz host-dial destination guard |
| Submodule mirroring bypasses network policy and accepts file URLs | Critical | Needs fix | Slice 2: host-side repository mirror policy enforcement |
| Submodule remotes are mirrored from the host without policy validation | Critical | Needs fix | Slice 2: host-side repository mirror policy enforcement |
| Control-plane RPC handlers are reachable without authentication | High | Needs fix | Slice 3: multi-principal control-server enforcement |
| Remote control-plane RPCs are exposed without authentication | High | Needs fix | Slice 3: multi-principal control-server enforcement |
| Sandbox port dial RPC lacks caller authentication and authorization | High | Needs fix | Slice 3: multi-principal control-server enforcement |
| Network control-plane listeners do not enforce configured caller authentication | High | Needs fix | Slice 3: multi-principal control-server enforcement |
| Daemon install can expose the unauthenticated control plane on TCP | High | Needs fix | Slice 4a: daemon install listener and argument hardening |
| Newlines in daemon arguments can inject systemd unit directives | High | Needs fix | Slice 4a: daemon install listener and argument hardening |
| Privileged DNS install writes and chowns certificate paths in a user-controlled directory without symlink checks | High | Needs fix | Slice 4b: DNS and exposure certificate path hardening |
| Privileged certificate writes follow user-controlled symlinks | High | Needs fix | Slice 4b: DNS and exposure certificate path hardening |
| Remote URL path can inject fields into git credential fill lookup | High | Needs fix | Slice 6: gateway credential and cache authorization hardening |
| Cached Git pack responses are not scoped to current repo authorization | High | Needs fix | Slice 6: gateway credential and cache authorization hardening |
| Policy protobufs can request allow-all sandbox egress | High | Needs fix | Slice 7: policy compile and protobuf validation hardening |
| Repository-controlled submodule URLs are mirrored by the host without policy validation | High | Needs fix | Slice 2: host-side repository mirror policy enforcement |
| Unbounded OCI handler creation from request-controlled registry prefixes | High bug | Needs fix | Slice 6: gateway credential and cache authorization hardening |
| Unbounded rootfs tar extraction can exhaust host disk | High bug | Needs fix | Slice 8: image and boot asset resource bounds |
| Submodule digesting conflates identical mirror paths at different commits | High bug | Needs fix | Slice 2: host-side repository mirror policy enforcement |
| Mutable GitHub Action refs run with package publish permission | Medium | Needs fix | Slice 9: CI supply-chain pinning |
| Release manifest fields are used as host cache path components | Medium | Needs fix | Slice 8: image and boot asset resource bounds |
| Fetch cache hits can bypass per-sandbox redirect policy | Medium | Needs fix | Slice 6: gateway credential and cache authorization hardening |
| Go proxy cache hits can bypass effective policy validation | Medium | Needs fix | Slice 6: gateway credential and cache authorization hardening |
| Snapshot storage can be orphaned when VM resume fails | Bug | Needs fix | Slice 10: darwin-vz lifecycle cleanup |
| Portable dependency validation treats glob key files as literals | Bug | Needs fix | Slice 11: dependency validation correctness |
| Newline-containing Git paths break batched digest calculation | Bug | Needs fix | Slice 11: dependency validation correctness |
| Workspace copy-out trusts guest-reported paths before applying the guest patch | High | Needs fix | Slice 12: workspace copy-out trust-boundary hardening |

## Slice Order

1. Block unsafe darwin-vz file-handle gateway host dials.
2. Enforce repository mirror policy for submodules, including `file://` and host-local targets, and repair submodule cache keys.
3. Land configured multi-principal control-server enforcement using the existing auth plan.
4. Harden daemon install listener and argument handling.
5. Harden DNS installer and exposure certificate path behavior.
6. Bind gateway credentials and cache hits to the current authorization and effective policy.
7. Reject unsafe policy protobufs before backend execution.
8. Bound image/rootfs extraction and sanitize boot asset cache path components.
9. Pin publish-capable GitHub Actions workflows to immutable refs.
10. Clean up darwin-vz snapshot storage when resume fails.
11. Fix dependency and Git path validation edge cases.
12. Apply guest workspace patches before trusting copy-out paths.

## Key Learnings From Pressure-Testing

The file-handle gateway guard must be independent of DNS policy. A default-allow
network policy or a previously observed DNS answer cannot be allowed to widen
the host dial boundary, so the destination address check runs before DNS runtime
authorization and before the host opens a TCP connection.

The blocked set also needs to include non-obvious special-use ranges, not just
RFC1918 and loopback. The Slice 1 test matrix includes CGNAT, documentation,
benchmarking, reserved, NAT64, AS112, AMT, ORCHIDv2, SRv6 SID, unallocated IPv6,
and IPv4-mapped private destinations to keep that boundary explicit.

For Slice 2, same-host-only submodule mirroring is the narrow fix. It is stricter
than allowing any host listed in the workspace egress policy, but it avoids
turning repository content into a host-side network selector before a dedicated
submodule mirror policy exists.

## Validation Standard

Each slice should include focused regression coverage for the affected trust
boundary, a targeted `go test` run for the touched packages, and a broader repo
check when the touched surface can affect shared behavior.
