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
