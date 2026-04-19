# Local Observability Stack

This example runs a local Grafana + Prometheus + Tempo stack with an OpenTelemetry Collector in front.

The data flow is:

- Cleanroom sends OTLP traces and metrics to the collector on `localhost:4317` or `localhost:4318`.
- The collector forwards traces to Tempo.
- The collector exposes OTLP metrics as a Prometheus scrape endpoint.
- Tempo generates trace-derived service graph and span metrics and remote-writes them to Prometheus.
- Grafana queries Prometheus and Tempo.

## Start the stack

From this directory:

```bash
docker compose up -d
```

The local endpoints are:

- Grafana: `http://localhost:3000`
- Prometheus: `http://localhost:9090`
- Tempo HTTP API: `http://localhost:3200`
- OTLP gRPC ingest: `localhost:14317`
- OTLP HTTP ingest: `http://localhost:14318`

Grafana is configured for anonymous local access.

The collector uses `14317` and `14318` on the host so it can coexist with other local OTLP or Jaeger containers already using the default ports.

## Point Cleanroom at the collector

Set your Cleanroom runtime config to send OTLP to the collector rather than directly to Tempo or Jaeger:

```yaml
observability:
  enabled: true
  deployment_environment: local
  otlp:
    endpoint: http://localhost:14318
    protocol: http/protobuf
  traces:
    exporter: otlp
    sampling:
      mode: parentbased_traceidratio
      ratio: 1.0
```

If you prefer gRPC, set `endpoint: localhost:14317` and `protocol: grpc` instead.

## Explore the data

- In Grafana Explore, use the `Tempo` datasource for traces.
- Use the `Prometheus` datasource for `cleanroom_*` metrics exported from the collector.
- Tempo service graph and span metrics also land in Prometheus.
- Grafana also provisions a `Cleanroom Observability` dashboard automatically under the `Cleanroom` folder.

## Stop the stack

```bash
docker compose down -v
```
