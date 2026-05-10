# Operations

The installer creates runtime config, installs the helper where needed, and
starts the daemon.

```bash
curl -fsSL https://raw.githubusercontent.com/buildkite/cleanroom/main/scripts/install.sh | bash
```

Use daemon commands when running local builds, changing service options, or
debugging a host.

Pass `--no-daemon` or set `CLEANROOM_INSTALL_DAEMON=0` when you only want the
binaries.

## Daemon

```bash
cleanroom daemon status
cleanroom daemon status --json
cleanroom daemon start
cleanroom daemon stop
cleanroom daemon restart
```

Install or update the service definition explicitly after a binary-only install
or when you change service options:

```bash
cleanroom daemon install --init-config --restart
```

On macOS the daemon runs under launchd user scope. On Linux it runs under
systemd system scope.

## Runtime Config

Runtime config lives at `$XDG_CONFIG_HOME/cleanroom/config.yaml`, usually
`~/.config/cleanroom/config.yaml`.

```bash
cleanroom config init
cleanroom config validate
cleanroom config validate --json
```

Runtime config covers host-specific choices such as the default backend,
control endpoint, backend asset paths, gateway settings, cache peers,
observability, snapshot drivers, and Docker service defaults.

Repository behavior belongs in `cleanroom.yaml`; see [Policy](policy.md).

## Diagnostics

```bash
cleanroom doctor
cleanroom doctor --json
cleanroom sandbox ls
cleanroom sandbox inspect --last
cleanroom execution ls
cleanroom execution inspect --last
cleanroom status --last
cleanroom version
```

`cleanroom exec` and `cleanroom console` keep stderr focused on guest output.
Use `--print-sandbox-id`, `cleanroom status --last`, or
`cleanroom execution inspect ...` when you need retained control-plane details.

## Storage

```bash
cleanroom system df
cleanroom system df --json
cleanroom system prune --dry-run
cleanroom system prune --dry-run --table
cleanroom system prune --all --older-than 7d
```

`system prune` protects active sandboxes and does not delete explicit snapshots
by default. Use `cleanroom snapshot rm` for named snapshots.

## Observability

Cleanroom can emit OTLP traces and metrics, and can write structured logs:

```yaml
observability:
  enabled: true
  logs:
    format: json
  otlp:
    endpoint: http://localhost:4318
    protocol: http/protobuf
    insecure: true
```

See [Observability](observability.md) for the telemetry contract and local
example stack.

## Local DNS And HTTPS

Local HTTPS exposure hostnames use the `cleanroom.localhost` domain. On macOS,
install DNS and HTTPS trust separately:

```bash
sudo cleanroom dns install
cleanroom dns status
```

See [Networking](networking.md) for service exposure.
