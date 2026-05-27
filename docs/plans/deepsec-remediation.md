# DeepSec Remediation Plan

**Spec reference:** `docs/spec.md`; `docs/api.md`; `docs/tls.md`; `docs/plans/multi-principal-control-server.md`; `docs/plans/stage-scoped-egress.md`
**Status:** Slice 1 ready for review
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

Focused validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/backend/darwinvz
```

Result: passed.

Repository validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise run check
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
| Daemon install can expose the unauthenticated control plane on TCP | High | Needs fix | Slice 3: multi-principal control-server enforcement, plus daemon install guardrails |
| Newlines in daemon arguments can inject systemd unit directives | High | Needs fix | Slice 4: privileged installer argument and path hardening |
| Privileged DNS install writes and chowns certificate paths in a user-controlled directory without symlink checks | High | Needs fix | Slice 4: privileged installer argument and path hardening |
| Privileged certificate writes follow user-controlled symlinks | High | Needs fix | Slice 4: privileged installer argument and path hardening |
| Remote URL path can inject fields into git credential fill lookup | High | Needs fix | Slice 5: gateway credential and cache authorization hardening |
| Cached Git pack responses are not scoped to current repo authorization | High | Needs fix | Slice 5: gateway credential and cache authorization hardening |
| Policy protobufs can request allow-all sandbox egress | High | Needs fix | Slice 6: policy compile and protobuf validation hardening |
| Repository-controlled submodule URLs are mirrored by the host without policy validation | High | Needs fix | Slice 2: host-side repository mirror policy enforcement |
| Unbounded OCI handler creation from request-controlled registry prefixes | High bug | Needs fix | Slice 5: gateway credential and cache authorization hardening |
| Unbounded rootfs tar extraction can exhaust host disk | High bug | Needs fix | Slice 7: image and boot asset resource bounds |
| Submodule digesting conflates identical mirror paths at different commits | High bug | Needs fix | Slice 2: host-side repository mirror policy enforcement |
| Mutable GitHub Action refs run with package publish permission | Medium | Needs fix | Slice 8: CI supply-chain pinning |
| Release manifest fields are used as host cache path components | Medium | Needs fix | Slice 7: image and boot asset resource bounds |
| Fetch cache hits can bypass per-sandbox redirect policy | Medium | Needs fix | Slice 5: gateway credential and cache authorization hardening |
| Go proxy cache hits can bypass effective policy validation | Medium | Needs fix | Slice 5: gateway credential and cache authorization hardening |
| Snapshot storage can be orphaned when VM resume fails | Bug | Needs fix | Slice 9: darwin-vz lifecycle cleanup |
| Portable dependency validation treats glob key files as literals | Bug | Needs fix | Slice 10: dependency validation correctness |
| Newline-containing Git paths break batched digest calculation | Bug | Needs fix | Slice 10: dependency validation correctness |
| Workspace copy-out trusts guest-reported paths before applying the guest patch | High | Needs fix | Slice 11: workspace copy-out trust-boundary hardening |

## Slice Order

1. Block unsafe darwin-vz file-handle gateway host dials.
2. Enforce repository mirror policy for submodules, including `file://` and host-local targets, and repair submodule cache keys.
3. Land multi-principal control-server enforcement using the existing auth plan.
4. Harden privileged installer argument handling, certificate writes, and symlink behavior.
5. Bind gateway credentials and cache hits to the current authorization and effective policy.
6. Reject unsafe policy protobufs before backend execution.
7. Bound image/rootfs extraction and sanitize boot asset cache path components.
8. Pin publish-capable GitHub Actions workflows to immutable refs.
9. Clean up darwin-vz snapshot storage when resume fails.
10. Fix dependency and Git path validation edge cases.
11. Apply guest workspace patches before trusting copy-out paths.

## Key Learnings From Pressure-Testing

The file-handle gateway guard must be independent of DNS policy. A default-allow
network policy or a previously observed DNS answer cannot be allowed to widen
the host dial boundary, so the destination address check runs before DNS runtime
authorization and before the host opens a TCP connection.

The blocked set also needs to include non-obvious special-use ranges, not just
RFC1918 and loopback. The Slice 1 test matrix includes CGNAT, documentation,
benchmarking, reserved, NAT64, and IPv4-mapped private destinations to keep that
boundary explicit.

## Validation Standard

Each slice should include focused regression coverage for the affected trust
boundary, a targeted `go test` run for the touched packages, and a broader repo
check when the touched surface can affect shared behavior.
