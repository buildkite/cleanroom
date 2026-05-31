# DeepSec Remediation Plan

**Spec reference:** `docs/spec.md`; `docs/api.md`; `docs/tls.md`; `docs/plans/multi-principal-control-server.md`; `docs/plans/stage-scoped-egress.md`
**Status:** Slice 25 ready for review
**Last reviewed:** 2026-05-31

## Summary

DeepSec revalidated 26 findings against the Cleanroom checkout on 2026-05-27.
All 26 were treated as true positives that needed code, configuration, or CI
fixes. A post-merge re-check on 2026-05-30 scanned current `main`, processed one
new candidate, and force-revalidated all 27 findings. That pass marked 22
findings fixed and left 5 true positives. Slice 13 closed the 3 remaining
remote control-plane auth findings from that set, and Slice 14 closes the
cached Git pack authorization finding. Slice 15 closes the remaining dynamic
Git content-cache handler growth finding. The 2026-05-30 DeepSec status now
shows all 27 findings revalidated as fixed.

DeepSec was rerun against the v0.10.0 release on 2026-05-31. That run tracked
64 files, found 12 issues, and revalidated all 12 as true positives. The first
v0.10.0 fixing slice is scoped to the two critical Git gateway port-smuggling
findings because they share one root cause: route hosts were authorized as
host:443 while outbound URL construction could preserve an embedded port.
Slice 16 closed both critical findings in PR #491. Slice 17 closed the HIGH
darwin-vz helper resolution finding in PR #492. Slice 18 closed the HIGH
execution-scoped gateway credential cleanup finding in PR #493. Slice 19 closed
the HIGH DNS exfiltration finding in PR #494. Slice 20 closed the HIGH OCI
digest cache authorization finding in PR #495. Slice 21 closed the unknown
OIDC `kid` JWKS fetch amplification finding in PR #496. Slice 22 closed the
Git proxy environment port-preservation finding in PR #497. Slice 23 closes the
retained execution output UTF-8 handling bug. Slice 24 closes the persistent
darwin-vz file-handle network policy lifetime finding. Slice 25 closes the Git
mirror upload-pack response buffering finding.

After Slice 25 revalidation, the live `cleanroom-v0.10.0` DeepSec status shows
1 unresolved true positive: Git mirror command-output buffering.

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

Slice 15 is implemented and ready for review. Scoped Git content-cache handlers
now use the same lease-and-LRU pattern as OCI, fetch, and Go proxy handlers.
The cache keeps a bounded set of scoped handlers, refreshes recency on reuse,
evicts least-recently-used scopes when the bound is exceeded, and closes evicted
handlers after in-flight requests release their leases. The non-Basic
`git-upload-pack` fallback path now uses a direct Git proxy instead of the
mirror-backed fallback, so bearer-token requests do not reuse mirror contents
keyed only by repository URL.

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
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom --force --filter internal/gateway/contentcache.go --min-severity MEDIUM --concurrency 1 --root /Users/lachlan/Develop/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec report --project-id cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec status --project-id cleanroom
```

Result: the dynamic Git handler creation finding revalidated as fixed. DeepSec
status now reports 27/27 findings revalidated, with 0 true positives and 27
fixed findings.

Slice 16 is implemented and ready for review. The direct Git route in
`internal/gateway/git.go` and the cached route in
`internal/gateway/git_cached.go` now normalize the route host before policy
checks and outbound URL construction. Explicit ports, userinfo, slashes,
malformed escapes, and other URL authority syntax are rejected before either
the direct proxy, content-cache, or fallback paths can construct an outbound
URL.

Codex review on PR #491 flagged that an escaped route-host slash could be
decoded by `net/http` before host validation. The slice now validates the
escaped `/git/<host>/` segment before splitting the decoded path, so encoded
host separators are rejected before owner, cache, or fallback decisions.

Focused validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/gateway
```

Result: passed.

Repository validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise run check
```

Result: passed.

Targeted DeepSec revalidation on 2026-05-31:

```text
cd /Users/lachlan/Develop/cleanroom/.deepsec
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom-v0.10.0 --force --filter internal/gateway/git.go --concurrency 1 --root /Users/lachlan/.codex/worktrees/28db/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom-v0.10.0 --force --filter internal/gateway/git_cached.go --concurrency 1 --root /Users/lachlan/.codex/worktrees/28db/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec report --project-id cleanroom-v0.10.0
```

Result: both critical Git gateway port-smuggling findings revalidated as fixed.
The `internal/gateway/git.go` run also rechecked the existing medium mirror
buffering finding, which remains a true positive outside this slice. DeepSec
status now reports 12/12 findings revalidated, with 10 true positives and
2 fixed findings.

Slice 17 is implemented and ready for review. The darwin-vz helper resolver
now keeps explicit `CLEANROOM_DARWIN_VZ_HELPER`, installed sibling helper, and
PATH discovery, but no longer searches the current working directory or
ancestor `dist/` directories unless `CLEANROOM_DARWIN_VZ_HELPER_ALLOW_CWD=1`
is explicitly set for local development. This keeps normal installed helper
resolution working while removing the default repo-local helper execution path
from untrusted working directories.

Focused validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/backend/darwinvz -run 'TestResolveHelperBinaryPath|TestHelperWorkdirLookupAllowed|TestResolvePrebuiltBinaryPathFromWorkdirUsesAncestorDist'
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/backend/darwinvz
```

Result: passed.

Repository validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise run check
```

Result: passed.

Targeted DeepSec revalidation on 2026-05-31:

```text
cd /Users/lachlan/Develop/cleanroom/.deepsec
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom-v0.10.0 --force --filter internal/backend/darwinvz/helper_client.go --concurrency 1 --root /Users/lachlan/.codex/worktrees/28db/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec status --project-id cleanroom-v0.10.0
```

Result: the helper resolution finding revalidated as fixed. DeepSec status now
reports 12/12 findings revalidated, with 9 true positives and 3 fixed findings.

Slice 18 is implemented and ready for review. Firecracker execution cleanup now
clears both the active execution trace and the execution-scoped gateway
authorization metadata. The gateway registry keeps the registered sandbox scope
as the fallback authorization and restores it when the matching execution ends,
while preserving the existing execution-ID guard so an older execution cannot
clear a newer active scope. The trace-only cleanup path remains available for
darwin-vz scope-token flows that release the token after each execution.

Focused validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/gateway -run 'TestRegistry.*ExecutionScope'
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/backend/firecracker -run 'TestRunInSandbox.*GatewayScope'
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/gateway
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/backend/firecracker
```

Result: passed.

Repository validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise run check
```

Result: passed.

Targeted DeepSec revalidation on 2026-05-31:

```text
cd /Users/lachlan/Develop/cleanroom/.deepsec
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom-v0.10.0 --force --filter internal/backend/firecracker/backend.go --concurrency 1 --root /Users/lachlan/.codex/worktrees/28db/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec status --project-id cleanroom-v0.10.0
```

Result: the execution-scoped gateway credential cleanup finding revalidated as
fixed. DeepSec status now reports 12/12 findings revalidated, with 8 true
positives and 4 fixed findings.

Slice 19 is implemented and ready for review. The shared DNS forwarder can now
block disallowed queries before contacting upstream resolvers. When this gate
is enabled, only query names allowed by the active sandbox policy or static
gateway records are answered; denied names get `REFUSED`, invoke the deny hook,
and do not create DNS observations. Policy-gated upstream requests also strip
answer and authority sections from guest-supplied DNS messages so allowed
question names cannot carry extra records upstream. For deny-by-default
policies, they rebuild allowed questions into canonical `IN` address, CNAME,
HTTPS, or SVCB lookups so header fields, mixed-case QNAME bytes, QCLASS, and
unsupported QTYPE values do not become covert channels. Allow-default policies
still permit broader `IN` lookup types, but the forwarder supplies its own DNS
ID and EDNS capacity before restoring the guest's ID and question casing in the
response. The gate is enabled for both darwin-vz file-handle DNS and
Firecracker trusted DNS, and remains disabled when file-handle networking runs
without a policy runtime.

Focused validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/dnsproxy -run 'TestForwarder(BlocksDisallowedQueryBeforeUpstream|AllowsPolicyQueryWhenBlockingDisallowedQueries|SanitizesAllowedQueryBeforeUpstream|BlocksUnsupportedQueryShapeBeforeUpstream|AllowsAnyINQueryTypeWhenPolicyAllowsAll|BlocksAliasWhenOnlyCNAMETargetAllowed|ServesStaticRecordsWithoutUpstreamObservationOrDeny)'
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/backend/darwinvz -run 'TestStartFileHandleGatewayDoesNotResolveAllowRulesAtStartup|TestNewFileHandleDNSRuntime|TestFileHandleGateway'
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/backend/firecracker -run 'TestSetupHostNetworkWithTrustedDNSFactory|TestTrustedDNS'
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/dnsproxy
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/backend/darwinvz
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/backend/firecracker
```

Result: passed.

Repository validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise run check
```

Result: passed.

Targeted DeepSec revalidation on 2026-05-31:

```text
cd /Users/lachlan/Develop/cleanroom/.deepsec
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom-v0.10.0 --force --filter internal/backend/darwinvz/filehandle_gateway.go --concurrency 1 --root /Users/lachlan/.codex/worktrees/28db/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec status --project-id cleanroom-v0.10.0
```

Result: the DNS query exfiltration finding revalidated as fixed. DeepSec status
now reports 12/12 findings revalidated, with 7 true positives and 5 fixed
findings.

Slice 20 is implemented and ready for review. OCI content-cache handlers are
now leased by registry prefix plus an authorization cache scope derived from
the requested OCI repository and authenticated sandbox owner when available.
Each scoped handler builds its own OCI tag, manifest, and blob indexes and its
own singleflight downloader. This preserves cache reuse inside the same
authorized repo scope while preventing digest manifest, blob, and in-flight
download hits from crossing into another repo or owner envelope.

Focused validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/gateway -run 'Test(CachedRegistryHandler|DockerHubMirrorHandler|OCIHandlerForPrefix|ScopedOCIIndex)'
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/gateway
```

Result: passed.

Repository validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise run check
```

Result: passed.

Targeted DeepSec revalidation on 2026-05-31:

```text
cd /Users/lachlan/Develop/cleanroom/.deepsec
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom-v0.10.0 --force --filter internal/gateway/oci_cached.go --concurrency 1 --root /Users/lachlan/.codex/worktrees/28db/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec status --project-id cleanroom-v0.10.0
```

Result: the OCI digest cache authorization finding revalidated as fixed.
DeepSec status now reports 12/12 findings revalidated, with 6 true positives
and 6 fixed findings.

Slice 21 is implemented and ready for review. OIDC JWKS cache lookups now
distinguish an unloaded cache from a loaded fresh cache. Fresh cache misses for
unknown `kid` values return a local key-not-found error without fetching JWKS
again, while the first lookup still fetches keys, stale caches still refresh,
and reused-`kid` key rotation still uses the existing signature-failure refresh
path.

Focused validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/authz -run 'TestOIDCValidator(DoesNotRefreshFreshJWKSForUnknownKid|RefreshesJWKSOnReusedKidSignatureFailure|ExpiresJWKSCache)'
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/authz
```

Result: passed.

Repository validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise run check
```

Result: passed.

Targeted DeepSec revalidation on 2026-05-31:

```text
cd /Users/lachlan/Develop/cleanroom/.deepsec
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom-v0.10.0 --force --filter internal/authz/oidc.go --concurrency 1 --root /Users/lachlan/.codex/worktrees/28db/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec status --project-id cleanroom-v0.10.0
```

Result: the unknown OIDC `kid` JWKS fetch amplification finding revalidated as
fixed. DeepSec status now reports 12/12 findings revalidated, with 5 true
positives and 7 fixed findings.

Slice 22 is implemented and ready for review. Policy allow-rule hosts now must
be bare hostnames or valid IP literals when loaded from YAML or proto state, so
mapping-form rules cannot smuggle ports, userinfo, schemes, paths, percent
escapes, bracketed IPv6 literals, or control characters into host-based policy
checks. Host validation rejects non-ASCII input before lowercasing so Unicode
case folding cannot silently rewrite policy hosts. Git proxy environment
generation also reuses the normalized Git route-host validator before writing
`insteadOf` rewrites, so manually constructed compiled policies cannot produce
a gateway rewrite for an authority-shaped host.

Focused validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/gateway ./internal/policy -run 'Test(GitProxyEnvVarsSkipsMalformedAllowRuleHosts|ProxyEnvVarsStillIncludesGitWhenRubyGemsRouteIsUnavailable|CompileRejectsNetworkAllowHostAuthoritySyntax|CompilePreservesNetworkAllowIPLiteralHosts|CompileRejectsNetworkAllowNonASCIIBeforeLowercase|FromProtoRejectsNetworkAllowHostAuthoritySyntax|FromProtoPreservesNetworkAllowIPv6LiteralHost|CompileNormalizesNetworkAllowShorthand|FromProtoCanonicalisesAllowRules)'
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/gateway ./internal/policy
```

Result: passed.

Repository validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise run check
```

Result: passed.

Targeted DeepSec revalidation on 2026-05-31:

```text
cd /Users/lachlan/Develop/cleanroom/.deepsec
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom-v0.10.0 --force --filter internal/gateway/git_proxy_env.go --concurrency 1 --root /Users/lachlan/.codex/worktrees/28db/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec status --project-id cleanroom-v0.10.0
```

Result: the Git proxy environment port-preservation finding revalidated as
fixed. DeepSec status now reports 12/12 findings revalidated, with 4 true
positives and 8 fixed findings.

Slice 23 is implemented and ready for review. Retained execution stdout and
stderr now carry a tiny per-stream pending UTF-8 tail between chunks, store only
valid UTF-8 in snapshot strings, and flush any incomplete final sequence as a
replacement character when execution finishes. Tail truncation advances to a
UTF-8 rune boundary before cloning the retained bytes. This keeps
`InspectExecutionResponse` protobuf string fields valid even when a workload
emits invalid bytes, a valid multi-byte rune is split across output chunks, or a
byte limit lands in the middle of a multi-byte rune. Raw stream events still
carry the original bytes.

Focused validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/controlservice -run 'Test(AppendRetainedOutput(ClonesTailSlice|SanitizesInvalidUTF8|TruncatesOnUTF8Boundary)|AppendRetainedOutputBytesPreservesSplitUTF8|FlushRetainedOutputPendingSanitizesIncompleteUTF8|ExecutionRetention(BoundsOutput|SanitizesInvalidUTF8Output))'
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/controlservice
```

Result: passed.

Repository validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise run check
```

Result: passed.

Targeted DeepSec revalidation on 2026-05-31:

```text
cd /Users/lachlan/Develop/cleanroom/.deepsec
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom-v0.10.0 --force --filter internal/controlservice/state_helpers.go --concurrency 1 --root /Users/lachlan/.codex/worktrees/28db/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec status --project-id cleanroom-v0.10.0
```

Result: the retained execution output UTF-8 handling bug revalidated as fixed.
DeepSec status now reports 12/12 findings revalidated, with 3 true positives
and 9 fixed findings.

Slice 24 is implemented and ready for review. Persistent darwin-vz executions
now clear the file-handle network policy when the execution returns. The clear
path drains active TCP proxy connections, removes DNS policy state for the
sandbox, and lets the next execution re-register its stage policy before it
runs. This closes the gap where detached guest children could keep using the
last execution's egress policy after the foreground command exited.

Focused validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/backend/darwinvz -run 'TestFileHandle(Gateway(SetPolicyUpdatesDNSRuntime|ClearPolicyRemovesDNSRuntimePolicy)|VirtualNetwork(SetPolicyClosesActiveTCPProxyConnections|SetPolicyCancelsPendingTCPProxyDial|SetPolicySerializesWithTCPAdmission|ClearPolicyClosesActiveTCPProxyConnections))'
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/backend/darwinvz
```

Result: passed.

Repository validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise run check
```

Result: passed.

Targeted DeepSec revalidation on 2026-05-31:

```text
cd /Users/lachlan/Develop/cleanroom/.deepsec
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom-v0.10.0 --force --filter internal/backend/darwinvz/backend_darwin.go --concurrency 1 --root /Users/lachlan/.codex/worktrees/28db/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec status --project-id cleanroom-v0.10.0
```

Result: the persistent darwin-vz file-handle network policy lifetime finding
revalidated as fixed. DeepSec status now reports 12/12 findings revalidated,
with 2 true positives and 10 fixed findings.

Slice 25 is implemented and ready for review. Mirror-backed `git-upload-pack`
responses now stream command stdout directly to the HTTP response writer instead
of buffering the full packfile in memory before replying. The response writer
flushes after writes when the server supports it, and upload-pack stderr is
captured through a bounded error buffer so failure messages cannot grow without
limit.

Focused validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/gateway -run 'Test(ServeMirrorUploadPackWritesCommandOutput|LimitedOutputBuffer|GitHandlerServesMirrorToRealGitClient)'
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise exec -- go test ./internal/gateway
```

Result: passed.

Repository validation run on 2026-05-31:

```text
MISE_TRUSTED_CONFIG_PATHS=/Users/lachlan/.codex/worktrees/28db/cleanroom/mise.toml mise run check
```

Result: passed.

Targeted DeepSec revalidation on 2026-05-31:

```text
cd /Users/lachlan/Develop/cleanroom/.deepsec
npm exec --package=pnpm@9.15.4 -- pnpm deepsec revalidate --project-id cleanroom-v0.10.0 --force --filter internal/gateway/git.go --concurrency 1 --root /Users/lachlan/.codex/worktrees/28db/cleanroom
npm exec --package=pnpm@9.15.4 -- pnpm deepsec status --project-id cleanroom-v0.10.0
```

Result: the mirror upload-pack buffering finding revalidated as fixed. The
forced `internal/gateway/git.go` run also rechecked the already-fixed critical
port-smuggling finding in that file. DeepSec status now reports 12/12 findings
revalidated, with 1 true positive and 11 fixed findings.

## Triage

| Finding | Severity | Decision | Remediation slice |
| --- | --- | --- | --- |
| Direct Git proxy allows SSRF via embedded port in upstream host | Critical | Fixed by Slice 16 | Slice 16: Git gateway route authority normalization |
| Git cache route allows policy port bypass via port smuggling in upstream host | Critical | Fixed by Slice 16 | Slice 16: Git gateway route authority normalization |
| Helper resolution can execute a repo-local binary on the host | High | Fixed by Slice 17 | Slice 17: gate darwin-vz CWD helper discovery |
| Execution-scoped gateway credential authorization persists after execution | High | Fixed by Slice 18 | Slice 18: restore gateway scope after execution |
| Denied workloads can still exfiltrate data through DNS queries | High | Fixed by Slice 19 | Slice 19: DNS pre-query policy gate |
| OCI digest cache can bypass repository-level authorization | High | Fixed by Slice 20 | Slice 20: OCI cache scope binding |
| Unknown JWT key IDs force repeated JWKS fetches | Medium | Fixed by Slice 21 | Slice 21: OIDC JWKS fresh-miss guard |
| Git proxy rewrites can preserve embedded ports from policy hosts | Medium | Fixed by Slice 22 | Slice 22: policy host authority validation |
| Retained execution output can become invalid UTF-8 | Bug | Fixed by Slice 23 | Slice 23: retained output UTF-8 sanitization |
| Persistent VM network policy remains active after an execution exits | Medium | Fixed by Slice 24 | Slice 24: darwin-vz file-handle policy cleanup |
| Mirror upload-pack buffers unbounded pack responses in memory | Medium | Fixed by Slice 25 | Slice 25: stream Git upload-pack responses |
| Git mirror operations buffer unbounded command output | Medium | Needs fix | Slice 26: bound Git mirror command output |
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
| Unbounded dynamic Git cache handlers can exhaust gateway memory | Medium | Fixed by Slice 15 | Slice 15: bound dynamic Git handler creation |
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
16. Normalize Git gateway route authorities before policy checks or outbound URL construction.
17. Gate darwin-vz current-working-directory helper discovery behind explicit local-development opt-in.
18. Restore registered gateway authorization when Firecracker execution scopes end.
19. Gate DNS queries against sandbox policy before forwarding to upstream resolvers.
20. Bind OCI digest cache entries to the authorized repo and owner scope.
21. Return local OIDC JWKS key-not-found errors for fresh unknown `kid` cache misses.
22. Reject authority-shaped policy hosts before generating Git proxy rewrites.
23. Sanitize retained execution output before exposing protobuf strings.
24. Clear persistent darwin-vz file-handle network policy when execution exits.
25. Stream or bound Git upload-pack responses.
26. Bound Git mirror command output captured for errors.

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
