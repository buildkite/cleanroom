package observability

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
)

type ServiceMetrics struct {
	sandboxCreateDuration     metric.Float64Histogram
	executionTotal            metric.Int64Counter
	executionDuration         metric.Float64Histogram
	cachePeerLookupTotal      metric.Int64Counter
	cachePeerTransferBytes    metric.Int64Counter
	cachePeerTransferDuration metric.Float64Histogram
	cachePeerImportTotal      metric.Int64Counter
}

func NewServiceMetrics(provider metric.MeterProvider) (*ServiceMetrics, error) {
	meter := meterProviderOrNoop(provider).Meter("github.com/buildkite/cleanroom/internal/controlservice")
	sandboxCreateDuration, err := meter.Float64Histogram(
		MetricSandboxCreateDurationSeconds,
		metric.WithDescription("Time spent creating a sandbox"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	executionTotal, err := meter.Int64Counter(
		MetricExecutionTotal,
		metric.WithDescription("Completed executions by backend, kind, and outcome"),
	)
	if err != nil {
		return nil, err
	}
	executionDuration, err := meter.Float64Histogram(
		MetricExecutionDurationSeconds,
		metric.WithDescription("Execution duration by backend, kind, and outcome"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	cachePeerLookupTotal, err := meter.Int64Counter(
		MetricCachePeerLookupTotal,
		metric.WithDescription("Cache peer lookup attempts by stage, direction, and result"),
	)
	if err != nil {
		return nil, err
	}
	cachePeerTransferBytes, err := meter.Int64Counter(
		MetricCachePeerTransferBytesTotal,
		metric.WithDescription("Cache peer transfer bytes by stage, direction, and result"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}
	cachePeerTransferDuration, err := meter.Float64Histogram(
		MetricCachePeerTransferDuration,
		metric.WithDescription("Cache peer transfer duration by stage, direction, and result"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	cachePeerImportTotal, err := meter.Int64Counter(
		MetricCachePeerImportTotal,
		metric.WithDescription("Cache peer import attempts by stage and result"),
	)
	if err != nil {
		return nil, err
	}
	return &ServiceMetrics{
		sandboxCreateDuration:     sandboxCreateDuration,
		executionTotal:            executionTotal,
		executionDuration:         executionDuration,
		cachePeerLookupTotal:      cachePeerLookupTotal,
		cachePeerTransferBytes:    cachePeerTransferBytes,
		cachePeerTransferDuration: cachePeerTransferDuration,
		cachePeerImportTotal:      cachePeerImportTotal,
	}, nil
}

func (m *ServiceMetrics) RecordSandboxCreate(ctx context.Context, backendName, source, outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	m.sandboxCreateDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(
		attribute.String(MetricLabelBackend, normalizeMetricValue(backendName, "unknown")),
		attribute.String(MetricLabelSource, normalizeMetricValue(source, "unknown")),
		attribute.String(MetricLabelOutcome, normalizeMetricValue(outcome, "unknown")),
	))
}

func (m *ServiceMetrics) RecordExecution(ctx context.Context, backendName, kind, outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String(MetricLabelBackend, normalizeMetricValue(backendName, "unknown")),
		attribute.String(MetricLabelKind, normalizeMetricValue(kind, "unknown")),
		attribute.String(MetricLabelOutcome, normalizeMetricValue(outcome, "unknown")),
	)
	m.executionTotal.Add(ctx, 1, attrs)
	m.executionDuration.Record(ctx, duration.Seconds(), attrs)
}

func (m *ServiceMetrics) RecordCachePeerLookup(ctx context.Context, stage, direction, result string) {
	if m == nil {
		return
	}
	m.cachePeerLookupTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String(MetricLabelStage, normalizeMetricValue(stage, "unknown")),
		attribute.String(MetricLabelDirection, normalizeMetricValue(direction, "unknown")),
		attribute.String(MetricLabelResult, normalizeMetricValue(result, "unknown")),
	))
}

func (m *ServiceMetrics) RecordCachePeerTransfer(ctx context.Context, stage, direction, result string, bytes int64, duration time.Duration) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String(MetricLabelStage, normalizeMetricValue(stage, "unknown")),
		attribute.String(MetricLabelDirection, normalizeMetricValue(direction, "unknown")),
		attribute.String(MetricLabelResult, normalizeMetricValue(result, "unknown")),
	)
	if bytes > 0 {
		m.cachePeerTransferBytes.Add(ctx, bytes, attrs)
	}
	m.cachePeerTransferDuration.Record(ctx, duration.Seconds(), attrs)
}

func (m *ServiceMetrics) RecordCachePeerImport(ctx context.Context, stage, result string) {
	if m == nil {
		return
	}
	m.cachePeerImportTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String(MetricLabelStage, normalizeMetricValue(stage, "unknown")),
		attribute.String(MetricLabelResult, normalizeMetricValue(result, "unknown")),
	))
}

type GatewayMetrics struct {
	requestTotal    metric.Int64Counter
	requestDuration metric.Float64Histogram
}

func NewGatewayMetrics(provider metric.MeterProvider) (*GatewayMetrics, error) {
	meter := meterProviderOrNoop(provider).Meter("github.com/buildkite/cleanroom/internal/gateway")
	requestTotal, err := meter.Int64Counter(
		MetricGatewayRequestsTotal,
		metric.WithDescription("Gateway requests by service, action, reason code, and status class"),
	)
	if err != nil {
		return nil, err
	}
	requestDuration, err := meter.Float64Histogram(
		MetricGatewayRequestDuration,
		metric.WithDescription("Gateway request duration by service and action"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	return &GatewayMetrics{requestTotal: requestTotal, requestDuration: requestDuration}, nil
}

func (m *GatewayMetrics) RecordRequest(ctx context.Context, service, action, reasonCode string, statusCode int, duration time.Duration) {
	if m == nil {
		return
	}
	service = normalizeMetricValue(service, "unknown")
	action = normalizeMetricValue(action, "unknown")
	reasonCode = normalizeMetricValue(reasonCode, "none")
	m.requestTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String(MetricLabelService, service),
		attribute.String(MetricLabelAction, action),
		attribute.String(MetricLabelReasonCode, reasonCode),
		attribute.String(MetricLabelStatusClass, statusCodeClass(statusCode)),
	))
	m.requestDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(
		attribute.String(MetricLabelService, service),
		attribute.String(MetricLabelAction, action),
	))
}

type BackendMetrics struct {
	launchPhaseDuration metric.Float64Histogram
}

func NewBackendMetrics(provider metric.MeterProvider, meterName string) (*BackendMetrics, error) {
	meter := meterProviderOrNoop(provider).Meter(defaultTracerName(meterName))
	launchPhaseDuration, err := meter.Float64Histogram(
		MetricLaunchPhaseDurationSeconds,
		metric.WithDescription("Backend launch and guest execution phase duration"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	return &BackendMetrics{launchPhaseDuration: launchPhaseDuration}, nil
}

func (m *BackendMetrics) RecordLaunchPhase(ctx context.Context, backendName, phase string, duration time.Duration) {
	if m == nil {
		return
	}
	m.launchPhaseDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(
		attribute.String(MetricLabelBackend, normalizeMetricValue(backendName, "unknown")),
		attribute.String(MetricLabelPhase, normalizeMetricValue(phase, "unknown")),
	))
}

func meterProviderOrNoop(provider metric.MeterProvider) metric.MeterProvider {
	if provider == nil {
		return metricnoop.NewMeterProvider()
	}
	return provider
}

func normalizeMetricValue(value, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	return trimmed
}

func statusCodeClass(statusCode int) string {
	if statusCode <= 0 {
		return "0xx"
	}
	return fmt.Sprintf("%dxx", statusCode/100)
}
