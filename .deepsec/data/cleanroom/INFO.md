# cleanroom

## What this codebase does

Cleanroom is a Go 1.26 single-binary CLI and provenance layer over the
external `spore` CLI / SporeVM. It does not run virtual machines itself.
The supported command surface is deliberately small:

- `policy validate`
- `compile`
- `stamp`
- `bake`
- `verify`
- `gateway serve`
- `version`

Cleanroom compiles `cleanroom.yaml` into SporeVM create inputs. Policy
compilation must fail closed when a requested policy cannot be enforced
by the SporeVM input model or by the host-side mediation gateway.

`bake` stages a workspace by copying only git-visible files into
`/workspace`. It excludes ignored files and `.git`, runs configured
warmup commands, captures a spore, and records provenance annotations
for later verification and audit.

Bake key v2 covers the compiled policy hash, image reference, git
commit, git remote, and dirty-workspace state. Dirty workspaces and
non-git workspaces are never considered cache-fresh.

## Auth and trust shape

`verify` and the gateway integrity checks have an important caveat: the
bake key is a public hash, not proof of origin. Gateway verification uses
`--dir` as the local trust root. Authorisation is the operator choosing
to bind and expose the Unix socket.

The mediation gateway serves only services requested in policy and
granted by host XDG configuration. It listens on a Unix socket with 0600
permissions. Host credentials are injected by Cleanroom on the host side;
guest-supplied `Authorization` and attribution headers are stripped and
replaced before proxying. Attribution is for audit only and must not be
used as an authentication boundary.

## Threat model

The primary attacker is untrusted code running inside the spore-created
workspace, trying to widen declared access, smuggle unstaged host files,
reuse stale cached outputs, exfiltrate host credentials, or forge
provenance. A secondary attacker can influence repository contents,
policy files, service requests, local gateway traffic, release metadata,
or archive downloads.

Security-sensitive behaviour should remain explicit and deny-by-default.
Policy parsing, staging, provenance, gateway grant matching, credential
injection, checksum verification, and cache freshness are the main
review areas.

## Highest impact paths

- Policy compile fail-closed behaviour and schema validation.
- Workspace staging: git-visible file selection, ignore handling, `.git`
  exclusion, path normalisation, and `/workspace` copy layout.
- Provenance parsing, annotation handling, and validation against the
  expected schema.
- Bake-key/idempotency logic, especially dirty and non-git freshness.
- Gateway service scope matching, host grant matching, Unix socket
  permissions, and reverse proxy credential replacement.
- Installer and release checksum verification for downloaded archives.

## Project-specific patterns to flag

- Any change that silently allows unenforceable policy to compile.
- Any staging path that includes ignored files, `.git`, absolute host
  paths, symlink escapes, or unstaged dirty content in a cache-fresh bake.
- Any provenance parser that accepts malformed annotations, ambiguous
  hashes, missing policy fields, or mismatched image/commit/remote data.
- Any gateway route that grants services not requested by policy or not
  present in the host XDG grant configuration.
- Any reverse proxy code path that forwards guest-supplied credentials or
  treats attribution headers as proof of identity.
- Any installer or release script change that skips archive digest checks
  or executes downloaded content before verification.

## Known false-positives

- Tests, docs, and examples contain placeholder tokens, fake sockets,
  shell snippets, inspect fixtures, sample policies, and local hostnames.
- `.spore`, fork, and output paths in examples are artifacts used to
  explain expected command output and are not live secrets.
- `scripts/install.sh` and release scripts intentionally download and
  verify archives as part of installation and packaging workflows.
- Sanitised examples may include values such as `Person Example`,
  `person@example.com`, placeholder bearer strings, and fake Unix socket
  paths.
