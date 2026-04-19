# Observability Stack Plan

**Spec reference:** `spec.md` sections 8.1, 9; `api.md` section 9.3
**Status:** in progress
**Last reviewed:** 2026-04-19

## Summary

Move Cleanroom from per-execution JSON files plus Buildkite annotations to a
standard telemetry stack built around:

- OpenTelemetry for traces and metrics instrumentation
- structured JSON logs with trace correlation
- an OTLP collector layer on hosts and CI workers
- Grafana plus Tempo plus a Prometheus-compatible metrics backend plus Loki for
  storage and query

Current implementation note:

- the first shipped tracing slice added OpenTelemetry spans, trace IDs, direct
  trace links, and serve startup/status output
- OTLP trace export is now the only supported transport so the same Cleanroom
  trace configuration works with local Jaeger, an OpenTelemetry Collector or
  Grafana Alloy, and production observability backends

## Immediate delivery priorities

The first operator-focused observability slice should optimise for ease of use,
not breadth.

We want a user to be able to:

1. point Cleanroom at Jaeger, a local Collector, Grafana Alloy, or a production
   OTLP ingress and immediately get useful traces
2. follow one execution from CLI to control service to gateway to backend work
3. use the CLI as the jump-off point into the trace UI rather than manually
   reconstructing trace identifiers

That means the immediate priorities are:

- OTLP trace export support in runtime config and the observability runtime,
  with gRPC and HTTP/protobuf transport support
- backend-neutral trace coverage across CLI, control plane, gateway, and
  backend adapters
- CLI integration that surfaces `trace_id` and, when configured, direct trace
  links from failure output and `cleanroom execution inspect`
- explicit startup/status output so operators can see where traces are being
  exported

Metrics, structured log correlation, dashboards, and collector-side enrichment
remain important, but they should follow the initial tracing UX rather than
block it.

The implementation should stay backend-agnostic at the product boundary:

- CLI and control-plane APIs expose stable execution, sandbox, and reason-code
  semantics
- internal telemetry shape is shared across `firecracker`, `darwin-vz`, and
  future backends
- backend-specific timings and runtime details stay inside adapter internals
  and telemetry attributes

Buildkite annotations and retained artifacts remain useful, but only as the
last-mile summary and failure-bundle surface. They should not remain the system
of record for longitudinal analysis.

## Why change

Current observability is useful for one execution at a time, but weak for
operating the system over time.

Today we have:

- retained `execution-observability.json` payloads under local state
- `cleanroom execution inspect` and `cleanroom status` for per-execution views
- ad hoc Buildkite annotations for some E2E steps
- structured deny/failure messages in several subsystems

Current gaps:

- no trace view for following one execution across CLI, control plane, gateway,
  backend launch, and guest execution phases
- no aggregated metrics for p50/p95/p99 launch latency, failure rates, retry
  behavior, cache hit rates, or deny rates
- no log/trace/metric correlation
- no fleet-wide query surface across many builds or hosts
- CI summaries can select the wrong retained execution when a job performs
  multiple runs

## Goals

- Make one execution easy to trace end to end.
- Make backend performance and reliability easy to compare over time.
- Preserve stable reason codes across logs, metrics, traces, CLI, and API.
- Support CI, local development, and long-running hosted control planes.
- Keep metric cardinality bounded and defensible.
- Keep runtime/exporter details configurable through runtime config rather than
  backend-specific CLI flags.
- Make Buildkite output link to the canonical telemetry systems instead of
  duplicating them.

## Non-goals

- Building a custom telemetry protocol or storage backend.
- Exposing backend-specific observability flags in normal CLI UX.
- Using per-execution IDs, sandbox IDs, image digests, or host IPs as metric
  labels.
- Replacing retained local execution artifacts; they remain useful for local
  debugging and offline inspection.
- Solving every possible dashboard, SLO, or alert in the first slice.

## Recommended stack

Default recommendation:

- instrumentation: OpenTelemetry SDKs
- transport/processing: OTLP to Grafana Alloy or OpenTelemetry Collector
- traces: Tempo
- metrics: Mimir or Prometheus-compatible remote write backend
- logs: Loki
- UI/query: Grafana

Deployment options:

- managed-first: Grafana Cloud with OTLP or Alloy agents on CI and server hosts
- self-hosted: Grafana plus Alloy/Collector plus Tempo plus Loki plus Mimir

Reasons to prefer this stack:

- OpenTelemetry is the vendor-neutral standard for traces and metrics
- collector pipelines let us batch, enrich, sample, and route without
  hard-wiring a backend into the application
- Grafana provides first-class correlation between traces, metrics, and logs
- Tempo, Loki, and Prometheus-style metrics are a common operational default
  with broad ecosystem support

## Current product model to preserve

The current product semantics should remain intact:

- `cleanroom execution inspect` stays the canonical per-execution diagnostic
  view
- `cleanroom status` stays the retained local artifact view
- stable error and deny reason codes remain part of the CLI and API contract
- backend adapters continue to own backend-specific launch/runtime details

The new stack should enrich these semantics, not replace them.

## Design principles

### 1. Use one telemetry contract across the product

Every execution path should emit the same core identifiers and outcome model:

- execution ID
- sandbox ID when applicable
- backend
- execution kind
- outcome
- stable reason code

This keeps dashboards and queries backend-neutral.

### 2. Put high-cardinality fields in traces and logs, not metrics

Metric labels should stay low-cardinality:

- good: backend, execution kind, outcome, phase, reason code
- bad: execution ID, sandbox ID, build number, image digest, guest IP, host IP

High-cardinality identifiers belong in:

- trace/span attributes
- structured logs
- retained local artifacts

### 3. Use traces as the canonical single-run view

For one execution, traces should be the best answer to:

- where time went
- which subsystem failed
- whether retries occurred
- which deny reason or transport error ended the run

### 4. Use metrics for aggregation and alerting

Metrics should answer:

- what is slow right now
- what is regressing over time
- which backend or queue is unhealthy
- whether retries, guest-agent timeouts, and deny events are spiking

### 5. Keep logs structured and correlated

Structured logs should include trace correlation so a user can pivot from a log
line to the trace and from a trace to the supporting logs.

### 6. Keep collector/exporter choices in runtime config

Exporter endpoints, auth headers, sampling, and deployment environment belong
in runtime config under XDG-managed config, not hard-coded in backend logic and
not spread across ad hoc environment variables.

## Target architecture

### 1. Instrumentation points

Add telemetry to these layers:

- CLI client
- control server and control service
- host gateway
- backend adapters
- CI wrappers and job summaries

### 2. Data flow

Preferred data path:

1. Cleanroom components emit OTLP traces and metrics plus structured JSON logs.
2. A local Alloy or Collector instance receives telemetry on the host.
3. The collector enriches telemetry with host, environment, and CI metadata.
4. The collector exports traces to Tempo, metrics to Mimir/Prometheus, and logs
   to Loki.
5. Grafana becomes the main operator UI.

For the first usable slice, the trace-only subset of this path is enough:

1. Cleanroom emits OTLP traces.
2. Those traces can be sent directly to a local Jaeger OTLP ingress or to a
   local/remote Collector.
3. The CLI exposes the trace identifier so the operator can pivot into the
   trace UI quickly.

### 3. Buildkite role

Buildkite should become the summary layer:

- annotate the job with a short execution summary
- attach direct links to trace, dashboard, and log views when available
- retain a small observability artifact bundle for offline debugging

## Telemetry model

### Resource attributes

Every process should emit stable resource metadata such as:

- `service.namespace=cleanroom`
- `service.name`
- `service.version`
- `deployment.environment.name`
- `host.name` when appropriate

CI contexts should add build metadata in one shared adapter layer, for example:

- pipeline slug
- job ID
- queue
- branch
- commit SHA

We should keep this mapping centralized because CI semantic conventions are
still evolving and we should avoid scattering provider-specific keys.

### Trace model

Create one root trace per top-level execution or sandbox lifecycle action.

Examples of root operations:

- `cleanroom.exec`
- `cleanroom.console`
- `cleanroom.sandbox.create`
- `cleanroom.sandbox.terminate`
- `cleanroom.snapshot.create`

Recommended child spans:

- `policy.resolve`
- `policy.compile`
- `repository.resolve`
- `repository.bootstrap`
- `image.ensure`
- `rootfs.prepare`
- `network.setup`
- `vm.launch`
- `guest.wait_ready`
- `guest.exec`
- `sandbox.file_download`
- `sandbox.snapshot`
- `cleanup`

Gateway requests should emit their own spans with attributes for:

- service kind (`git`, `registry`, `secrets`, `meta`)
- destination host
- allow/deny action
- stable reason code

Backend adapters may add spans for backend-specific internals, but they should
roll up into the shared execution model above.

### Metric model

Start with a small set of counters and histograms:

- `cleanroom_execution_total{backend,kind,outcome}`
- `cleanroom_execution_duration_seconds{backend,kind,outcome}`
- `cleanroom_launch_phase_duration_seconds{backend,phase}`
- `cleanroom_guest_agent_timeout_total{backend}`
- `cleanroom_retry_total{backend,test}`
- `cleanroom_gateway_requests_total{service,action,reason_code}`
- `cleanroom_gateway_request_duration_seconds{service,action}`
- `cleanroom_policy_deny_total{backend,reason_code}`
- `cleanroom_image_cache_result_total{backend,result}`
- `cleanroom_snapshot_operation_total{backend,operation,outcome}`

Use histograms rather than summaries for latency so percentiles can be
aggregated across jobs and hosts.

Where supported, native histograms and exemplars should be enabled so slow
metric points can link back to representative traces.

### Log model

Emit JSON logs with fields such as:

- timestamp
- level
- message
- trace ID
- span ID
- execution ID
- sandbox ID
- backend
- reason code
- job metadata in CI

For Loki:

- keep labels minimal and stable
- do not promote execution ID, sandbox ID, or image digest to labels
- use derived fields or equivalent correlation to jump from logs to traces

### Audit model

The audit requirements in `spec.md` section 9 should be met through the same
telemetry program, not a second bespoke pipeline.

Audit events should remain structured around stable reason codes such as:

- `policy_invalid`
- `policy_conflict`
- `backend_unavailable`
- `backend_capability_mismatch`
- `host_not_allowed`
- `registry_not_allowed`
- `lockfile_violation`
- `secret_scope_violation`
- `runtime_launch_failed`

These codes should appear consistently in:

- CLI errors
- API errors
- structured logs
- trace/span attributes
- metric labels where cardinality remains safe

### Runtime configuration

Add one backend-neutral runtime config section for observability, for example:

```yaml
observability:
  enabled: true
  service_namespace: cleanroom
  deployment_environment: ci
  otlp:
    endpoint: https://otel.example.com:4317
    protocol: grpc
    headers:
      authorization: Bearer ${TOKEN}
  traces:
    sampling:
      mode: parentbased_traceidratio
      ratio: 1.0
    url_template: https://jaeger.example.com/trace/{{.TraceID}}?execution={{.ExecutionID}}
  metrics:
    export_interval_seconds: 30
  logs:
    format: json
```

Notes:

- config keys should remain backend-neutral
- exporter auth and endpoint details belong here, not in backend adapters
- CI hosts can still layer environment-variable overrides on top when needed
- OTLP is the only supported transport
- `traces.url_template` is optional and gives the CLI a backend-neutral way to
  print direct trace links in failure footers and `cleanroom execution inspect`

## Delivery strategy

This should land in phases, with useful operator value after each slice.

### Phase 0.5: Usable traces first

Before broad metrics/logging work, make tracing easy to adopt and easy to use.

Add:

- OTLP trace export support in the runtime and config layer
- clear config validation for OTLP trace settings
- CLI output of `trace_id` and optional `trace_url` on failures and other
  trace jump-off points
- gateway and backend spans needed for a useful single-execution trace
- `cleanroom serve` startup/status output that shows where traces are being
  exported

Definition of done:

- one execution can be exported to Jaeger or an OTLP-compatible production
  stack without transport-specific product quirks
- the CLI gives the operator enough information to find the trace quickly
- tracing support feels like a coherent product feature rather than raw SDK
  plumbing

### Phase 0: Taxonomy and contracts

Define and document:

- span names
- metric names
- common attributes
- stable outcome model
- stable reason-code mapping

Definition of done:

- one written schema shared by CLI, control plane, gateway, and backends
- no ad hoc metric names or inconsistent reason-code spellings

### Phase 1: Core traces and structured logs

Instrument:

- CLI top-level commands
- control service execution lifecycle
- gateway request allow/deny paths

Add:

- trace propagation through internal calls
- JSON logs with trace and execution correlation

Definition of done:

- one execution can be followed across CLI, control plane, gateway, and backend
- logs can be pivoted to traces

### Phase 2: Backend timing and metric emission

Instrument backend-neutral metrics plus backend adapter phase timings.

Add:

- counters and histograms for launch, execution, retries, denies, and cache
  behavior
- exemplars where supported

Definition of done:

- backend performance can be compared over time in Grafana
- p50/p95/p99 launch and execution latency are queryable

### Phase 3: Collector deployment

Deploy Alloy or Collector on:

- CI hosts
- long-running server hosts
- optional local developer setup

Add:

- OTLP receiver
- batching
- resource enrichment
- routing to Tempo, metrics backend, and Loki

Definition of done:

- telemetry leaves hosts through a single supported collector pipeline
- application code no longer needs backend-specific exporter logic

### Phase 4: Buildkite integration

Update CI summary behavior to:

- link to canonical trace and dashboard views
- keep artifact bundles as fallback evidence
- stop parsing local observability files as the primary operator view

Definition of done:

- Buildkite remains a concise summary surface
- deeper analysis happens in Grafana, Tempo, and Loki

### Phase 5: Sampling, retention, and alerts

Add:

- head or parent-based sampling defaults
- optional collector-side tail sampling where justified
- retention policies by environment
- first alert rules and SLO dashboards

Definition of done:

- telemetry cost is bounded
- high-value traces are retained
- alerts cover the first meaningful operational regressions

## Recommended initial defaults

For CI:

- trace sampling: 100% initially
- metric export: enabled
- JSON logs: enabled
- retention: shorter than production but long enough for regression analysis

For long-running shared environments:

- start with parent-based probabilistic sampling
- retain all error traces and key E2E traces where feasible
- keep gateway deny logs and security-relevant audit events at full fidelity

For local development:

- allow easy disablement
- keep local retained artifacts as the simplest fallback debugging path

## Dashboard set

Initial Grafana dashboards should answer:

- execution success rate by backend and kind
- execution latency by backend and phase
- guest-agent timeout trend
- gateway deny rate by reason code
- cache hit rate and cache latency
- snapshot operation outcomes
- E2E Buildkite queue health and duration trend

## Risks and mitigations

### Risk: metric cardinality explosion

Mitigation:

- review every metric label before merge
- ban execution ID and sandbox ID labels
- keep image digests and host IPs out of labels

### Risk: vendor lock-in through direct SDK exporters

Mitigation:

- standardize on OTLP
- keep exporter details in collector/runtime config

### Risk: tail sampling complexity

Mitigation:

- start with simple head sampling
- only introduce collector-side tail sampling after the routing topology is in
  place

### Risk: logs become expensive and noisy

Mitigation:

- enforce structured fields
- keep Loki labels low-cardinality
- ship debug-heavy fields as structured payload rather than indexed labels

## Open questions

- Should managed Grafana Cloud be the default for the first slice, or do we
  want self-hosted Tempo/Loki/Mimir from the start?
- How much CI/build metadata should become resource attributes versus
  query-time enrichment in the collector?
- Should we emit a first-class audit event stream in addition to logs, or is
  structured correlated logging sufficient for v1?

## Concrete implementation areas

Likely code areas for the first slices:

- `internal/cli`
- `internal/controlservice`
- `internal/controlserver`
- `internal/gateway`
- `internal/backend/firecracker`
- `internal/backend/darwinvz`
- `internal/runtimeconfig`

Likely new internal packages:

- `internal/observability` for tracer, meter, logger, and resource setup
- optional `internal/observability/ci` for CI metadata enrichment

## External references

- [OpenTelemetry docs](https://opentelemetry.io/docs/)
- [OpenTelemetry Go](https://opentelemetry.io/docs/languages/go/)
- [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/)
- [Prometheus and OpenTelemetry guide](https://prometheus.io/docs/guides/opentelemetry/)
- [Grafana Tempo docs](https://grafana.com/docs/tempo/latest/)
- [Grafana Loki docs](https://grafana.com/docs/loki/latest/)
- [Grafana Mimir docs](https://grafana.com/docs/mimir/latest/)
- [Grafana Alloy docs](https://grafana.com/docs/alloy/latest/)
