# DeepSec Remediation Plan

**Spec reference:** `docs/spec.md`; `docs/api.md`; `docs/tls.md`; `docs/plans/multi-principal-control-server.md`; `docs/plans/stage-scoped-egress.md`
**Status:** Slice 14 ready for review
**Last reviewed:** 2026-05-30

## Summary

DeepSec revalidated 26 findings against the Cleanroom checkout on 2026-05-27.
All 26 were treated as true positives that needed code, configuration, or CI
fixes. A post-merge re-check on 2026-05-30 scanned current `main`, processed one
new candidate, and force-revalidated all 27 findings. That pass marked 22
findings fixed and left 5 true positives. Slice 13 closed the 3 remaining
remote control-plane auth findings from that set, and Slice 14 closes the
cached Git pack authorization finding. One true positive remains: unbounded
dynamic Git content-cache handler creation.

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
fetch cache hits, and Go proxy cache hits were left for follow-up work and are
covered below.

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

Current `main` also includes owner-scoped gateway authorization for Git and OCI
cache routes. Auth-required Git cache requests now require an authenticated
sandbox owner and an authorized repository envelope before the gateway reaches
content-cache or host Git credentials. The 2026-05-30 DeepSec re-check showed
that this is only a partial fix for Git pack caching, because local/no-owner
cache hits and empty or non-Basic credential cases can still cross authorization
boundaries; Slice 14 tracks the remaining fix.

Slice 6b is implemented and ready for review. OCI registry handler creation is
now bounded by a small LRU cache, so request-controlled registry prefixes cannot
grow one handler per prefix for the lifetime of the daemon. Cache hits refresh
recency, evicted handlers are removed from the cache immediately, and handler
closers run only after outstanding requests release their leases.

Focused validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise exec -- go test ./internal/gateway
```

Result: passed.

Repository validation run on 2026-05-27:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise run check
```

Result: passed.

Slice 6c is implemented and ready for review. Fetch and Go proxy content-cache
metadata are now partitioned by compiled sandbox policy. Cache hits therefore
reuse data only within the same effective policy that authorized the original
upstream fetch, while misses use policy-pinned HTTP clients and per-policy
download singleflight groups for direct requests and redirects. The scoped
handler maps are LRU-bounded, evicted handlers close only after outstanding
requests release their leases, and evicted policy metadata is removed from the
shared metadata indexes after the evicted handler closes. If the same policy is
rebuilt while an old active handler is waiting to close, the stale handler
closes without deleting metadata owned by the replacement.

Focused validation run on 2026-05-28:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise exec -- go test ./internal/gateway
```

Result: passed.

Slice 7 is implemented and ready for review. Client-supplied policy protobufs
for sandbox creation can no longer set `network_default=allow`. The
`--dangerously-allow-all` path now sends an explicit sandbox creation option,
and the control service applies allow-all egress server-side after validating
the policy protobuf as deny-only. Stored compiled policies still round-trip
allow-default records so existing snapshots and caches created through the
explicit dangerous option remain readable.

Focused validation run on 2026-05-28:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise exec -- go test ./internal/policy ./internal/controlservice ./internal/cli
```

Result: passed.

Slice 8 is implemented and ready for review. OCI image rootfs materialization
now rejects archives before extraction when regular file payload exceeds 32 GiB
or archive entries exceed 1,000,000, preventing registry or import tar streams
from growing an unbounded temporary rootfs tree on the host. Managed kernel
asset cache paths now validate static specs and release-manifest `id` and asset
names as single safe path components before joining them under the Cleanroom
asset cache.

Focused validation run on 2026-05-28:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise exec -- go test ./internal/imagemgr ./internal/bootassets
```

Result: passed.

Repository validation run on 2026-05-28:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise run check
```

Result: passed.

Slice 9 is implemented and ready for review. The package-publishing
`.github/workflows/base-image.yml` workflow no longer runs third-party GitHub
Actions from mutable major tags while holding `packages: write`; each action ref
is pinned to the commit currently behind the referenced major tag, with the tag
kept as an inline note for reviewability.

Focused validation run on 2026-05-29:

```text
rg -n "uses:\s*[^@\s]+@v[0-9]+\b|uses:\s*[^@\s]+@main\b|uses:\s*[^@\s]+@master\b" .github/workflows -g'*.yml' -g'*.yaml'
```

Result: passed with no matches.

Repository validation run on 2026-05-29:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise run check
```

Result: passed.

Slice 10 is implemented and ready for review. darwin-vz snapshot creation now
tracks the stored snapshot rootfs once persistence succeeds and removes it if a
later error makes the API return failure. This specifically covers failed VM
resume after snapshot persistence, where the caller would otherwise not record
the snapshot while the stored rootfs remained on disk.

Focused validation run on 2026-05-29:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise exec -- go test ./internal/backend/darwinvz
```

Result: passed.

Repository validation run on 2026-05-29:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise run check
```

Result: passed.

Slice 11 is implemented and ready for review. Git file digest batching now
resolves requested paths to blob object IDs through NUL-delimited `ls-tree` and
`ls-files` output before streaming those object IDs through portable
`git cat-file --batch` reads. Repository paths containing newlines are handled
as single paths without depending on newer Git `cat-file -Z` support. Portable
dependency-stage validation also records the expanded dependency key-file path
list from the same host-side glob expansion used to build the cache key, then
validates those concrete paths inside the restored sandbox instead of treating
original glob patterns as literal files.

Focused validation run on 2026-05-29:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise exec -- go test ./internal/gitbatch ./internal/controlservice
```

Result: passed.

Focused CI-regression validation run on 2026-05-30 after Buildkite Linux
reported `git cat-file` EOFs:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise exec -- go test ./internal/gitbatch ./internal/controlservice ./internal/repositorychangeset
```

Result: passed.

Repository validation run on 2026-05-29:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise run check
```

Result: passed.

Repository validation rerun on 2026-05-30 after removing the newer Git
`cat-file -Z` dependency:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise run check
```

Result: passed.

Slice 12 is implemented and ready for review. Workspace copy-out now writes the
guest patch to a temporary file, applies it to a temporary index rooted at the
sandbox baseline, and derives the trusted changed-path list from that resulting
tree before conflict checks, forced obstacle handling, local patch generation,
or plan output use any target paths. The guest-reported `name-status` payload is
still parsed as part of the transport format, but it no longer decides which
local paths are safe to overwrite or report.

Focused validation run on 2026-05-30:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise exec -- go test ./internal/cli
```

Result: passed.

Repository validation run on 2026-05-30:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/deepsec-oci-handler-bounds/cleanroom/mise.toml mise run check
```

Result: passed.

Post-merge DeepSec re-check from the main checkout on 2026-05-30:

```text
cd /Users/lachlan/Develop/cleanroom/.deepsec
npm exec --package=pnpm@9.15.4 -- pnpm deepsec scan --project-id cleanroom --root /Users/lachlan/Develop/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec process --project-id cleanroom --concurrency 1
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom --force --concurrency 5 --root /Users/lachlan/Develop/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec report --project-id cleanroom
```

Result: scan tracked 63 files, the one new pending file produced the
`Unbounded dynamic Git cache handlers can exhaust gateway memory` medium
finding, and the force revalidation returned 5 true positives, 22 fixed,
0 false positives, and 0 uncertain verdicts.

Slice 13 is implemented and ready for review. Direct `cleanroom serve` now uses
the same listener/auth guard as daemon installs: Unix sockets and loopback
HTTP(S) listeners can remain unauthenticated, while non-loopback HTTP(S)
control-plane listeners require `auth.required=true`. Non-loopback plain HTTP
still remains invalid for bearer auth, so shared servers must use HTTPS plus
OIDC bearer authentication. The remote-access docs and API endpoint model now
state this requirement.

Focused validation run on 2026-05-30:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/Develop/cleanroom/mise.toml mise exec -- go test ./internal/cli -run 'TestValidate(ControlPlaneListenAuth|BearerAuthListenEndpoint)|TestDaemonInstall(RejectsNonLoopbackTCPWithoutAuth|AllowsNonLoopbackHTTPSWithAuth|RejectsNonLoopbackHTTPWithAuth)|TestServeCommandRunServerStartsAndStopsOnContextCancel'
```

Result: passed.

Focused validation run on 2026-05-30:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/Develop/cleanroom/mise.toml mise exec -- go test ./internal/cli
```

Result: passed.

Repository validation run on 2026-05-30:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/Develop/cleanroom/mise.toml mise run check
```

Result: passed.

Targeted DeepSec revalidation on 2026-05-30:

```text
cd /Users/lachlan/Develop/cleanroom/.deepsec
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom --force --filter internal/controlserver --min-severity HIGH --concurrency 2 --root /Users/lachlan/Develop/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom --force --filter proto/cleanroom/v1/control.proto --min-severity HIGH --concurrency 1 --root /Users/lachlan/Develop/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec report --project-id cleanroom
```

Result: the two `internal/controlserver` findings and the proto control-plane
surface finding revalidated as fixed. The report now has 2 true positives
remaining: cached Git pack authorization and unbounded dynamic Git handler
creation.

Slice 14 is implemented and ready for review. Git pack cache metadata is now
partitioned by host, effective policy key, owner authorization envelope, and
resolved upstream credential material. Empty credentials therefore cannot read
pack metadata written under host-side Basic credentials, and a changed owner
envelope or credential produces a separate pack index. `git-upload-pack`
requests with non-Basic upstream credentials bypass the embedded pack cache and
use the existing mirror-backed/direct Git fallback because the embedded
content-cache auth hook only re-checks Basic credentials on cache hits.

Focused validation run on 2026-05-30:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/Develop/cleanroom/mise.toml mise exec -- go test ./internal/gateway -run 'TestCachedGitHandler|TestGitHandlerForHost|TestContentCacheGitBasicAuthProvider|TestGitContentCacheUpstream'
```

Result: passed.

Focused validation run on 2026-05-30:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/Develop/cleanroom/mise.toml mise exec -- go test ./internal/gateway
```

Result: passed.

Targeted DeepSec revalidation on 2026-05-30:

```text
cd /Users/lachlan/Develop/cleanroom/.deepsec
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom --force --filter internal/gateway/git_cached.go --min-severity HIGH --concurrency 1 --root /Users/lachlan/Develop/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec report --project-id cleanroom
```

Result: the cached Git pack authorization finding revalidated as fixed. The
report now has 1 true positive remaining: unbounded dynamic Git handler
creation.

## Triage

| Finding | Severity | Decision | Remediation slice |
| --- | --- | --- | --- |
| File-handle gateway can host-dial private DNS answers | Critical | Needs fix | Slice 1: darwin-vz host-dial destination guard |
| Submodule mirroring bypasses network policy and accepts file URLs | Critical | Needs fix | Slice 2: host-side repository mirror policy enforcement |
| Submodule remotes are mirrored from the host without policy validation | Critical | Needs fix | Slice 2: host-side repository mirror policy enforcement |
| Control-plane RPC handlers are reachable without authentication | High | Fixed by re-check slice | Slice 13: require auth for non-loopback serve listeners |
| Remote control-plane RPCs are exposed without authentication | High | Fixed by re-check slice | Slice 13: require auth for non-loopback serve listeners |
| Sandbox port dial RPC lacks caller authentication and authorization | High | Fixed by re-check slice | Slice 13: require auth for non-loopback serve listeners |
| Network control-plane listeners do not enforce configured caller authentication | High | Needs fix | Slice 3: multi-principal control-server enforcement |
| Daemon install can expose the unauthenticated control plane on TCP | High | Needs fix | Slice 4a: daemon install listener and argument hardening |
| Newlines in daemon arguments can inject systemd unit directives | High | Needs fix | Slice 4a: daemon install listener and argument hardening |
| Privileged DNS install writes and chowns certificate paths in a user-controlled directory without symlink checks | High | Needs fix | Slice 4b: DNS and exposure certificate path hardening |
| Privileged certificate writes follow user-controlled symlinks | High | Needs fix | Slice 4b: DNS and exposure certificate path hardening |
| Remote URL path can inject fields into git credential fill lookup | High | Needs fix | Slice 6: gateway credential and cache authorization hardening |
| Cached Git pack responses are not scoped to current repo authorization | High | Fixed by Slice 14 | Slice 14: Git pack cache authorization binding |
| Policy protobufs can request allow-all sandbox egress | High | Needs fix | Slice 7: policy compile and protobuf validation hardening |
| Repository-controlled submodule URLs are mirrored by the host without policy validation | High | Needs fix | Slice 2: host-side repository mirror policy enforcement |
| Unbounded OCI handler creation from request-controlled registry prefixes | High bug | Needs fix | Slice 6: gateway credential and cache authorization hardening |
| Unbounded rootfs tar extraction can exhaust host disk | High bug | Needs fix | Slice 8: image and boot asset resource bounds |
| Submodule digesting conflates identical mirror paths at different commits | High bug | Needs fix | Slice 2: host-side repository mirror policy enforcement |
| Mutable GitHub Action refs run with package publish permission | Medium | Needs fix | Slice 9: CI supply-chain pinning |
| Release manifest fields are used as host cache path components | Medium | Needs fix | Slice 8: image and boot asset resource bounds |
| Fetch cache hits can bypass per-sandbox redirect policy | Medium | Needs fix | Slice 6: gateway credential and cache authorization hardening |
| Go proxy cache hits can bypass effective policy validation | Medium | Needs fix | Slice 6: gateway credential and cache authorization hardening |
| Unbounded dynamic Git cache handlers can exhaust gateway memory | Medium | New true positive from re-check | Slice 15: bound dynamic Git handler creation |
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
13. Require auth for direct non-loopback `cleanroom serve` listeners.
14. Finish Git pack cache authorization binding.
15. Bound dynamic Git content-cache handler creation.

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
