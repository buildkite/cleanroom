# Observability

Cleanroom observability is OTLP-only. When enabled, `cleanroom serve` prints
startup status for trace export, sampling, and whether direct trace links are
configured.

## Runtime config

Configure observability in `~/.config/cleanroom/config.yaml`:

```yaml
observability:
  enabled: true
  deployment_environment: local
  otlp:
    endpoint: http://localhost:14318
    protocol: http/protobuf
  traces:
    sampling:
      mode: parentbased_traceidratio
      ratio: 1.0
```

If you prefer OTLP gRPC, set `endpoint: localhost:14317` and `protocol: grpc`
instead.

`observability.traces.url_template` is optional. When set, Cleanroom prints
`trace_url=...` in failure footers and exposes the same URL from
`cleanroom execution inspect`.

## Local stack

For local development, start the collector-based stack in
[`examples/observability`](../examples/observability/README.md):

```bash
docker compose -f examples/observability/docker-compose.yaml up -d
```

That stack provides:

- Grafana at `http://localhost:3000`
- Prometheus at `http://localhost:9090`
- Tempo at `http://localhost:3200`
- an OpenTelemetry Collector listening on `localhost:14317` and `http://localhost:14318`

Grafana also provisions a `Cleanroom Observability` dashboard automatically
under the `Cleanroom` folder.

## Working with traces

Use the local stack or your configured OTLP backend to inspect traces and
execution diagnostics.

- `cleanroom exec` and `cleanroom console` print `sandbox_id`, `execution_id`,
  and `trace_id` on failure when available.
- `cleanroom exec --print-trace-id` also prints `trace_id` after a successful
  execution when available.
- When `url_template` is configured, failure footers also print `trace_url`.
- `cleanroom execution inspect <execution-id>` shows execution status,
  retained stdout and stderr, `trace_id`, optional `trace_url`, and retained
  observability payload.
- `cleanroom status --execution-id <execution-id>` shows the retained local
  artefacts, including `execution-observability.json`.

In the local Grafana stack:

- use the `Tempo` datasource for traces
- use the `Prometheus` datasource for `cleanroom_*` metrics
- use the provisioned dashboard for a quick overview

## Related docs

- [examples/observability/README.md](../examples/observability/README.md) for
  the local Docker Compose stack
- [docs/api.md](api.md) for execution inspection behaviour
- [docs/isolation.md](isolation.md) for retained execution observability files

## Telemetry contract

This section is the canonical observability contract for names and fields that
Cleanroom emits today. When we rename or remove any of these, update the
emitters and this document in the same change.

### Resource attributes

Every process should emit these stable OpenTelemetry resource attributes:

- `service.namespace=cleanroom`
- `service.name`
- `service.version` when available
- `deployment.environment.name` when configured

### Root spans

The current top-level product spans are:

- `cleanroom.exec`
- `cleanroom.console`
- `cleanroom.sandbox.create`
- `cleanroom.execution.create`
- `cleanroom.execution.run`
- `cleanroom.gateway.<service>.request`

`<service>` is one of `git`, `registry`, `rubygems`, `secrets`, or `meta`.

Sandbox creation may emit additional child spans under the
`cleanroom.sandbox.*` prefix for cache lookup, restore, bootstrap, and publish
phases.

### Metrics

The current metric names are:

- `cleanroom_sandbox_create_duration_seconds{backend,source,outcome}`
- `cleanroom_execution_total{backend,kind,outcome}`
- `cleanroom_execution_duration_seconds{backend,kind,outcome}`
- `cleanroom_gateway_requests_total{service,action,reason_code,status_class}`
- `cleanroom_gateway_request_duration_seconds{service,action}`
- `cleanroom_launch_phase_duration_seconds{backend,phase}`

Metric labels must stay low-cardinality. Execution IDs, sandbox IDs, image
digests, and host IPs belong in traces, logs, or retained artefacts, not in
metric labels.

### Common attributes

These attribute keys form the shared contract across traces, metrics, and
structured logs where applicable:

- `cleanroom.backend`
- `cleanroom.sandbox.id`
- `cleanroom.execution.id`
- `cleanroom.execution.kind`
- `cleanroom.reason_code`
- `cleanroom.gateway.service`
- `cleanroom.gateway.action`
- `cleanroom.command.argc`
- `cleanroom.command.name`
- `cleanroom.command.summary`

### Outcomes and reason codes

Execution and sandbox telemetry should use backend-neutral outcomes:

- `succeeded`
- `failed`
- `canceled`
- `timed_out`

Gateway request telemetry keeps the request decision as a separate axis:

- `action=allow`
- `action=deny`

Current gateway `reason_code` values include:

- `host_not_allowed`
- `method_not_allowed`
- `upstream_error`
- `unknown_registry_prefix`
- `proxied`
- `mirrored`
- `cached`

### Log correlation

When logs are emitted in structured form, they should carry the same core
correlation fields used elsewhere:

- `trace_id`
- `span_id`
- `execution_id`
- `sandbox_id`
- `backend`
- `reason_code`
- `component`
- `subsystem`
