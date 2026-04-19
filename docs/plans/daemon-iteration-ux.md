# Daemon Iteration UX Plan

**Status:** Proposed
**Scope:** Local daemon iteration, with macOS pain as the primary trigger

## Summary

Iterating on the Cleanroom service is currently more cumbersome than it needs to
be:

- runtime config bootstrap is a separate optional step (`cleanroom config init`)
- runtime config validation is implicit and command-dependent
- daemon reconciliation is split across config bootstrap, service install, and
  restart
- launchd bootstrap failures surface low-level `launchctl` output without a
  clean recovery path

This plan keeps the public CLI backend-neutral while making local iteration
more direct:

- add `cleanroom config validate` for explicit runtime-config validation
- extend `cleanroom daemon install` with `--restart` and `--init-config`
- make `daemon install --restart --init-config` the normal local iteration path
- make daemon install perform preflight validation before it touches launchd or
  systemd
- improve daemon-manager-specific diagnostics on bootstrap failures

The target macOS loop becomes:

```bash
mise run install:global
cleanroom daemon install --restart
```

First-run bootstrap becomes:

```bash
cleanroom daemon install --restart --init-config
```

Manual config edits become:

```bash
cleanroom config validate
cleanroom daemon install --restart
```

## Problems With The Current UX

### 1. Runtime-config validation has no explicit entry point

`runtimeconfig.Load()` already parses and normalises the config file, but that
validation only happens as a side effect of some other command. If a user edits
`config.yaml` directly, there is no obvious "check this before I restart the
daemon" command.

That is especially awkward on macOS where a bad daemon restart often turns into
"did launchd reject the service?" versus "did Cleanroom reject the config?"

### 2. Local daemon iteration is expressed as lifecycle primitives instead of intent

For local development, the common intent is not:

1. install a new service file
2. maybe overwrite an existing one
3. maybe restart a running daemon

The real intent is:

1. validate my config
2. reconcile the service definition with the current executable and flags
3. ensure the daemon is running with that definition

The current CLI makes the user translate that intent into lower-level steps.

### 3. launchd failures are actionable only if the user already understands launchd

The current failure mode:

```text
bootstrap launchd service com.buildkite.cleanroom: launchctl bootstrap gui/501 ...: exit status 5
```

is accurate, but it does not answer the next operator question:

- is the service already loaded?
- did the generated plist change?
- is the config invalid?
- should I stop, uninstall, or just retry?

## Goals

- Provide a one-command daemon reconciliation flow for local iteration.
- Provide an explicit runtime-config validation command.
- Keep the top-level CLI backend-neutral.
- Keep service-manager-specific details inside the daemon manager layer.
- Fail before touching launchd or systemd when the runtime config is invalid.
- Print enough state on daemon-manager failures that users can recover without
  manually learning launchd internals first.

## Non-goals

- Adding a macOS-only top-level command such as `cleanroom launchd ...`
- Replacing `doctor` as the host-capability diagnostic surface
- Adding an interactive config editor
- Introducing compatibility shims for old daemon semantics beyond what is needed
  for the current pre-1.0 CLI

## Proposed CLI Changes

### `cleanroom config validate`

Add an explicit runtime-config validation command:

```bash
cleanroom config validate
cleanroom config validate --path ./tmp/config.yaml
cleanroom config validate --json
```

Suggested behavior:

- parse YAML from the default runtime config path or the supplied `--path`
- normalise values the same way `runtimeconfig.Load()` does today
- validate backend-agnostic config
- validate backend-specific config that can be checked without starting the
  daemon
- print the resolved config path and selected default backend
- optionally emit machine-readable JSON

The initial semantic validation should cover at least:

- supported `default_backend`
- supported `darwin-vz` network settings
- unsupported observability exporter settings already enforced by
  `runtimeconfig.Load()`
- endpoint parsing for `control_host` when set

This command should be the obvious answer to "I patched the config by hand; is
it valid?"

### `cleanroom daemon install --restart --init-config`

Keep `install` as the entry point, but make it cover the common iteration flow:

```bash
cleanroom daemon install --restart
cleanroom daemon install --restart --init-config
cleanroom daemon install --restart --dry-run
```

Suggested behavior:

1. resolve the active daemon scope for the host platform
2. load and validate runtime config
3. optionally create default config when `--init-config` is set and the config
   file is missing
   - if config already exists, do not overwrite it
   - validate the resulting config before mutating the service manager
4. compute the desired daemon definition from:
   - current executable path
   - daemon listen flags
   - daemon gateway listen flags
   - TLS flags
5. compare desired definition with the installed service definition
6. rewrite the managed service file if needed
7. reconcile manager state so the daemon is running with the desired definition
8. print final daemon status

This makes `daemon install --restart` the intent-based reconciliation path,
while preserving the existing lifecycle-oriented command group:

- install owns the generated service file
- install rewrites its own managed service file by default when the desired
  definition changes
- it can no-op when nothing changed except a process restart is needed
- it gives one place for preflight checks and recovery hints

`start`, `stop`, `restart`, and `uninstall` can remain for direct lifecycle
control, but the docs should steer local iteration toward
`daemon install --restart`.

The important semantic change is that install needs to be idempotent for the
managed service definition. For a fixed service name and fixed managed path,
install should safely rewrite its own managed file when the desired definition
changes. `--force` should remain for user-owned files such as
`cleanroom config init --force`, not for tool-owned daemon artifacts. The
cleanest version is to remove `--force` from `daemon install` entirely, or make
it a deprecated no-op there.

## Proposed Output Shape

`daemon install --restart` should report what it actually did, not just whether
the last manager command succeeded. For example:

```text
daemon installed
  config:   /Users/lachlan/.config/cleanroom/config.yaml
  manager:  launchd
  service:  com.buildkite.cleanroom
  path:     /Users/lachlan/Library/LaunchAgents/com.buildkite.cleanroom.plist
  config:   valid
  service:  updated
  runtime:  restarted
  listen:   unix:///var/folders/.../cleanroom.sock
```

`--dry-run` should print the same decision summary without mutating anything.

## launchd Failure Diagnostics

When `launchctl bootstrap` or `kickstart` fails on macOS, the CLI should add
manager-specific context before returning. The first slice does not need a full
`daemon logs` command; it just needs enough evidence to unblock recovery.

Recommended failure output:

- the `launchctl` subcommand that failed
- the launchd domain and service target
- the service plist path
- whether the service was already loaded before the operation
- whether the service file changed in this run
- a recovery hint when the target appears stale:
  - `cleanroom daemon stop`
  - `cleanroom daemon uninstall`
  - `cleanroom daemon install --restart`

If cheap to gather, include one extra probe:

- `launchctl print <target>` output when the service is still loaded

That keeps the CLI responsible for translating a low-level launchd failure into
Cleanroom-specific next steps.

## Implementation Notes

### 1. Split config loading from config validation

Introduce a reusable validation path so commands can validate config explicitly
without depending on `Run()` startup side effects. A likely shape is:

- `runtimeconfig.LoadPath(path string) (Config, error)`
- `runtimeconfig.Validate(cfg Config) error`

`Run()` can still call the default-path loader, but `config validate` and
`daemon install --restart` should share the same validation code.

### 2. Add backend-specific semantic checks to validation

Some runtime-config errors are only rejected later today. Pull those checks into
shared validation where possible so:

- `config validate` catches them directly
- `daemon install --restart` fails before it touches the service manager

`darwin-vz` network-mode validation is the clearest existing example.

### 3. Model daemon reconciliation explicitly

The daemon code already has manager-specific install, restart, and status
helpers. `install --restart` should add one higher-level reconciliation layer
above them instead of making users script those primitives manually.

That layer should answer:

- does a managed service file exist?
- does it already match the desired definition?
- is the manager target loaded?
- is the daemon running?
- what action is needed: write, bootstrap, kickstart, or no-op?

### 4. Keep manager specifics internal

`daemon install --restart` should remain the user-facing iteration command on
both macOS and Linux. Differences such as launchd domains versus systemd units
should stay in the manager-specific implementation.

## Rejected Alternatives

### Only add `config validate`

That would solve the manual-edit problem but would leave the multi-step daemon
reconciliation flow intact.

### Add a new `daemon apply` verb

This would work, but it adds another top-level lifecycle shape when the current
command group can already express the right intent with a small semantic
expansion of install.

### Keep `daemon install` overwrite-sensitive behind `--force`

This preserves existing wording but keeps the main friction in place. The
daemon service definition is a generated, tool-owned file; requiring a
destructive-looking flag to reconcile it makes the normal path look riskier and
more manual than it really is.

### Add a macOS-only `daemon dev` command

That would improve the immediate pain but push platform details into the public
CLI. The better boundary is a backend-neutral intent command with launchd-aware
implementation under the hood.

## Recommended First Slice

Implement the smallest version that materially changes the day-to-day loop:

1. add `cleanroom config validate --path/--json`
2. add `cleanroom daemon install --restart --init-config/--dry-run`
3. make `daemon install` rewrite the managed service definition by default and
   call shared config validation before any launchd or systemd mutations
4. improve launchd bootstrap error output with target, path, and stale-service
   hints

That is enough to turn the current macOS workflow from:

```bash
mise run install:global
cleanroom config init --force   # optional
cleanroom daemon install --force
cleanroom daemon restart
```

into:

```bash
mise run install:global
cleanroom daemon install --restart --init-config
```

and, after first-run bootstrap:

```bash
mise run install:global
cleanroom daemon install --restart
```
