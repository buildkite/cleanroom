# Observability

Cleanroom observability is OTLP-only for traces and metrics. Server-side
operational logs stay local to the process and can be emitted as text or JSON.
When enabled, `cleanroom serve` prints startup status for log format, trace
export, sampling, and whether direct trace links are configured.

## Runtime config

Configure observability in `~/.config/cleanroom/config.yaml`:

```yaml
observability:
  enabled: true
  deployment_environment: local
  logs:
    format: json
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

`observability.logs.format` defaults to `text`. Set it to `json` when you want
structured logs with stable correlation fields such as `trace_id`, `span_id`,
`execution_id`, `sandbox_id`, `backend`, and `reason_code`.

`observability.traces.url_template` is optional. When set, Cleanroom exposes
`trace_url` from `cleanroom execution inspect`.

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

- `cleanroom exec` and `cleanroom console` leave failure stderr focused on
  streamed guest output instead of appending diagnostic footers.
- `cleanroom exec --print-trace-id` also prints `trace_id` after a successful
  execution when available.
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
- `cleanroom.repository.commit_sha`
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

### Cache observability

Layered-cache stage names are appropriate in observability because they help
operators understand cache lookup, restore, publish, and fallback behaviour.
They should remain telemetry vocabulary, not primary user-facing product nouns.

The cache-stage values are:

- `runtime`
- `workspace`
- `dependency`

Recommended cache telemetry attributes are:

- `cleanroom.cache.stage=runtime|workspace|dependency`
- `cleanroom.cache.operation=lookup|restore|publish|invalidate`
- `cleanroom.cache.result=hit|miss|restored|published|fallback|failed`
- `cleanroom.cache.lookup_reason=cache_record_not_found|backend_mismatch|policy_hash_mismatch|repository_changed|workspace_parent_changed`
- `cleanroom.repository.commit_sha=<git commit sha>`

Use these fields in traces and structured logs for cache-specific work. Use
them in metrics only for cache-specific series, not for generic execution
metrics.

Recommended cache-specific metric naming is:

- `cleanroom_cache_operation_total{stage,operation,result}`
- `cleanroom_cache_operation_duration_seconds{stage,operation}`

Do not put cache keys, storage refs, snapshot IDs, execution IDs, sandbox IDs,
or image digests into metric labels.
