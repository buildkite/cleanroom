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
	sandboxCreateDuration metric.Float64Histogram
	executionTotal        metric.Int64Counter
	executionDuration     metric.Float64Histogram
}

func NewServiceMetrics(provider metric.MeterProvider) (*ServiceMetrics, error) {
	meter := meterProviderOrNoop(provider).Meter("github.com/buildkite/cleanroom/internal/controlservice")
	sandboxCreateDuration, err := meter.Float64Histogram(
		"cleanroom_sandbox_create_duration_seconds",
		metric.WithDescription("Time spent creating a sandbox"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	executionTotal, err := meter.Int64Counter(
		"cleanroom_execution_total",
		metric.WithDescription("Completed executions by backend, kind, and outcome"),
	)
	if err != nil {
		return nil, err
	}
	executionDuration, err := meter.Float64Histogram(
		"cleanroom_execution_duration_seconds",
		metric.WithDescription("Execution duration by backend, kind, and outcome"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	return &ServiceMetrics{
		sandboxCreateDuration: sandboxCreateDuration,
		executionTotal:        executionTotal,
		executionDuration:     executionDuration,
	}, nil
}

func (m *ServiceMetrics) RecordSandboxCreate(ctx context.Context, backendName, source, outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	m.sandboxCreateDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(
		attribute.String("backend", normalizeMetricValue(backendName, "unknown")),
		attribute.String("source", normalizeMetricValue(source, "unknown")),
		attribute.String("outcome", normalizeMetricValue(outcome, "unknown")),
	))
}

func (m *ServiceMetrics) RecordExecution(ctx context.Context, backendName, kind, outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("backend", normalizeMetricValue(backendName, "unknown")),
		attribute.String("kind", normalizeMetricValue(kind, "unknown")),
		attribute.String("outcome", normalizeMetricValue(outcome, "unknown")),
	)
	m.executionTotal.Add(ctx, 1, attrs)
	m.executionDuration.Record(ctx, duration.Seconds(), attrs)
}

type GatewayMetrics struct {
	requestTotal    metric.Int64Counter
	requestDuration metric.Float64Histogram
}

func NewGatewayMetrics(provider metric.MeterProvider) (*GatewayMetrics, error) {
	meter := meterProviderOrNoop(provider).Meter("github.com/buildkite/cleanroom/internal/gateway")
	requestTotal, err := meter.Int64Counter(
		"cleanroom_gateway_requests_total",
		metric.WithDescription("Gateway requests by service, action, reason code, and status class"),
	)
	if err != nil {
		return nil, err
	}
	requestDuration, err := meter.Float64Histogram(
		"cleanroom_gateway_request_duration_seconds",
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
		attribute.String("service", service),
		attribute.String("action", action),
		attribute.String("reason_code", reasonCode),
		attribute.String("status_class", statusCodeClass(statusCode)),
	))
	m.requestDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(
		attribute.String("service", service),
		attribute.String("action", action),
	))
}

type BackendMetrics struct {
	launchPhaseDuration metric.Float64Histogram
}

func NewBackendMetrics(provider metric.MeterProvider, meterName string) (*BackendMetrics, error) {
	meter := meterProviderOrNoop(provider).Meter(defaultTracerName(meterName))
	launchPhaseDuration, err := meter.Float64Histogram(
		"cleanroom_launch_phase_duration_seconds",
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
		attribute.String("backend", normalizeMetricValue(backendName, "unknown")),
		attribute.String("phase", normalizeMetricValue(phase, "unknown")),
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
