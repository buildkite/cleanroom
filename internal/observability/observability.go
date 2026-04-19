package observability

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const (
	serviceNamespace = "cleanroom"
	defaultTraceName = "github.com/buildkite/cleanroom"
)

type Options struct {
	Config         runtimeconfig.ObservabilityConfig
	ServiceName    string
	ServiceVersion string
}

type Runtime struct {
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
	interceptor    connect.Interceptor
	shutdown       func(context.Context) error
}

// NewWithTracerProvider builds an observability runtime around an existing tracer provider.
func NewWithTracerProvider(provider trace.TracerProvider) (*Runtime, error) {
	return NewWithProviders(provider, nil)
}

// NewWithProviders builds an observability runtime around existing tracer and meter providers.
func NewWithProviders(tracerProvider trace.TracerProvider, meterProvider metric.MeterProvider) (*Runtime, error) {
	if tracerProvider == nil {
		tracerProvider = tracenoop.NewTracerProvider()
	}
	if meterProvider == nil {
		meterProvider = metricnoop.NewMeterProvider()
	}
	return newRuntime(tracerProvider, meterProvider, func(context.Context) error { return nil })
}

func Start(ctx context.Context, opts Options) (*Runtime, error) {
	tracerProvider := trace.TracerProvider(tracenoop.NewTracerProvider())
	meterProvider := metric.MeterProvider(metricnoop.NewMeterProvider())
	shutdown := func(context.Context) error { return nil }

	if opts.Config.Enabled {
		provider, err := newTracerProvider(ctx, opts)
		if err != nil {
			return nil, err
		}
		metricsProvider, err := newMeterProvider(ctx, opts)
		if err != nil {
			shutdownCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_ = provider.Shutdown(shutdownCtx)
			return nil, err
		}
		tracerProvider = provider
		meterProvider = metricsProvider
		shutdown = func(shutdownCtx context.Context) error {
			metricErr := metricsProvider.Shutdown(shutdownCtx)
			traceErr := provider.Shutdown(shutdownCtx)
			return errors.Join(traceErr, metricErr)
		}
	}

	return newRuntime(tracerProvider, meterProvider, shutdown)
}

func (r *Runtime) Tracer(name string, options ...trace.TracerOption) trace.Tracer {
	if r == nil {
		return tracenoop.NewTracerProvider().Tracer(defaultTracerName(name), options...)
	}
	return r.tracerProvider.Tracer(defaultTracerName(name), options...)
}

func (r *Runtime) TracerProvider() trace.TracerProvider {
	if r == nil {
		return tracenoop.NewTracerProvider()
	}
	return r.tracerProvider
}

func (r *Runtime) Meter(name string, options ...metric.MeterOption) metric.Meter {
	if r == nil {
		return metricnoop.NewMeterProvider().Meter(defaultTracerName(name), options...)
	}
	return r.meterProvider.Meter(defaultTracerName(name), options...)
}

func (r *Runtime) MeterProvider() metric.MeterProvider {
	if r == nil {
		return metricnoop.NewMeterProvider()
	}
	return r.meterProvider
}

func (r *Runtime) ConnectInterceptor() connect.Interceptor {
	if r == nil {
		return nil
	}
	return r.interceptor
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil || r.shutdown == nil {
		return nil
	}
	return r.shutdown(ctx)
}

func newRuntime(tracerProvider trace.TracerProvider, meterProvider metric.MeterProvider, shutdown func(context.Context) error) (*Runtime, error) {
	propagator := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
	runtime := &Runtime{
		tracerProvider: tracerProvider,
		meterProvider:  meterProvider,
		shutdown:       shutdown,
	}

	interceptor, err := otelconnect.NewInterceptor(
		otelconnect.WithPropagator(propagator),
		otelconnect.WithTracerProvider(runtime.tracerProvider),
		otelconnect.WithTrustRemote(),
		otelconnect.WithoutMetrics(),
		otelconnect.WithoutServerPeerAttributes(),
		otelconnect.WithoutTraceEvents(),
	)
	if err != nil {
		return nil, fmt.Errorf("create connect interceptor: %w", err)
	}
	runtime.interceptor = interceptor
	return runtime, nil
}

func newTracerProvider(ctx context.Context, opts Options) (*sdktrace.TracerProvider, error) {
	exporter, err := newTraceExporter(ctx, opts.Config)
	if err != nil {
		return nil, err
	}

	sampler, err := newSampler(opts.Config.Traces.Sampling)
	if err != nil {
		return nil, err
	}

	resource, err := newResource(opts)
	if err != nil {
		return nil, err
	}

	return sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource),
		sdktrace.WithSampler(sampler),
	), nil
}

func newMeterProvider(ctx context.Context, opts Options) (*sdkmetric.MeterProvider, error) {
	exporter, err := newMetricExporter(ctx, opts.Config)
	if err != nil {
		return nil, err
	}

	resource, err := newResource(opts)
	if err != nil {
		return nil, err
	}

	reader := sdkmetric.NewPeriodicReader(exporter)
	return sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(resource),
		sdkmetric.WithReader(reader),
	), nil
}

func newTraceExporter(ctx context.Context, cfg runtimeconfig.ObservabilityConfig) (sdktrace.SpanExporter, error) {
	otlpProtocol, err := runtimeconfig.ResolveOTLPTraceProtocol(cfg)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.OTLP.Endpoint) == "" {
		return nil, errors.New("missing observability.otlp.endpoint")
	}
	return newOTLPExporter(ctx, cfg.OTLP, otlpProtocol)
}

func newOTLPExporter(ctx context.Context, cfg runtimeconfig.OTLPConfig, protocol string) (sdktrace.SpanExporter, error) {
	switch protocol {
	case "grpc":
		return newOTLPGRPCExporter(ctx, cfg)
	case "http/protobuf":
		return newOTLPHTTPExporter(ctx, cfg)
	default:
		return nil, fmt.Errorf("unsupported observability.otlp.protocol %q", cfg.Protocol)
	}
}

func newMetricExporter(ctx context.Context, cfg runtimeconfig.ObservabilityConfig) (sdkmetric.Exporter, error) {
	otlpProtocol, err := runtimeconfig.ResolveOTLPTraceProtocol(cfg)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.OTLP.Endpoint) == "" {
		return nil, errors.New("missing observability.otlp.endpoint")
	}
	return newOTLPMetricExporter(ctx, cfg.OTLP, otlpProtocol)
}

func newOTLPMetricExporter(ctx context.Context, cfg runtimeconfig.OTLPConfig, protocol string) (sdkmetric.Exporter, error) {
	switch protocol {
	case "grpc":
		return newOTLPGRPCMetricExporter(ctx, cfg)
	case "http/protobuf":
		return newOTLPHTTPMetricExporter(ctx, cfg)
	default:
		return nil, fmt.Errorf("unsupported observability.otlp.protocol %q", cfg.Protocol)
	}
}

func newOTLPGRPCExporter(ctx context.Context, cfg runtimeconfig.OTLPConfig) (sdktrace.SpanExporter, error) {
	options := make([]otlptracegrpc.Option, 0, 3)
	if endpoint := strings.TrimSpace(cfg.Endpoint); endpoint != "" {
		if hasURLScheme(endpoint) {
			endpointURL, err := normalizeEndpointURL(endpoint)
			if err != nil {
				return nil, err
			}
			options = append(options, otlptracegrpc.WithEndpointURL(endpointURL))
		} else {
			options = append(options, otlptracegrpc.WithEndpoint(endpoint))
		}
	}
	if cfg.Insecure {
		options = append(options, otlptracegrpc.WithInsecure())
	}
	if headers := runtimeconfigHeaders(cfg.Headers); len(headers) > 0 {
		options = append(options, otlptracegrpc.WithHeaders(headers))
	}

	exporter, err := otlptracegrpc.New(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("configure otlp grpc trace exporter: %w", err)
	}
	return exporter, nil
}

func newOTLPHTTPExporter(ctx context.Context, cfg runtimeconfig.OTLPConfig) (sdktrace.SpanExporter, error) {
	options := make([]otlptracehttp.Option, 0, 3)
	if endpoint := strings.TrimSpace(cfg.Endpoint); endpoint != "" {
		if hasURLScheme(endpoint) {
			endpointURL, err := normalizeOTLPHTTPTraceEndpointURL(endpoint)
			if err != nil {
				return nil, err
			}
			options = append(options, otlptracehttp.WithEndpointURL(endpointURL))
		} else {
			options = append(options, otlptracehttp.WithEndpoint(endpoint))
		}
	}
	if cfg.Insecure {
		options = append(options, otlptracehttp.WithInsecure())
	}
	if headers := runtimeconfigHeaders(cfg.Headers); len(headers) > 0 {
		options = append(options, otlptracehttp.WithHeaders(headers))
	}

	exporter, err := otlptracehttp.New(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("configure otlp http trace exporter: %w", err)
	}
	return exporter, nil
}

func newOTLPGRPCMetricExporter(ctx context.Context, cfg runtimeconfig.OTLPConfig) (sdkmetric.Exporter, error) {
	options := make([]otlpmetricgrpc.Option, 0, 3)
	if endpoint := strings.TrimSpace(cfg.Endpoint); endpoint != "" {
		if hasURLScheme(endpoint) {
			endpointURL, err := normalizeEndpointURL(endpoint)
			if err != nil {
				return nil, err
			}
			options = append(options, otlpmetricgrpc.WithEndpointURL(endpointURL))
		} else {
			options = append(options, otlpmetricgrpc.WithEndpoint(endpoint))
		}
	}
	if cfg.Insecure {
		options = append(options, otlpmetricgrpc.WithInsecure())
	}
	if headers := runtimeconfigHeaders(cfg.Headers); len(headers) > 0 {
		options = append(options, otlpmetricgrpc.WithHeaders(headers))
	}

	exporter, err := otlpmetricgrpc.New(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("configure otlp grpc metric exporter: %w", err)
	}
	return exporter, nil
}

func newOTLPHTTPMetricExporter(ctx context.Context, cfg runtimeconfig.OTLPConfig) (sdkmetric.Exporter, error) {
	options := make([]otlpmetrichttp.Option, 0, 3)
	if endpoint := strings.TrimSpace(cfg.Endpoint); endpoint != "" {
		if hasURLScheme(endpoint) {
			endpointURL, err := normalizeOTLPHTTPMetricEndpointURL(endpoint)
			if err != nil {
				return nil, err
			}
			options = append(options, otlpmetrichttp.WithEndpointURL(endpointURL))
		} else {
			options = append(options, otlpmetrichttp.WithEndpoint(endpoint))
		}
	}
	if cfg.Insecure {
		options = append(options, otlpmetrichttp.WithInsecure())
	}
	if headers := runtimeconfigHeaders(cfg.Headers); len(headers) > 0 {
		options = append(options, otlpmetrichttp.WithHeaders(headers))
	}

	exporter, err := otlpmetrichttp.New(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("configure otlp http metric exporter: %w", err)
	}
	return exporter, nil
}

func newSampler(cfg runtimeconfig.TraceSamplingConfig) (sdktrace.Sampler, error) {
	if err := runtimeconfig.ValidateTraceSamplingConfig(cfg); err != nil {
		return nil, err
	}

	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	ratio := 1.0
	if cfg.Ratio != nil {
		ratio = *cfg.Ratio
	}

	switch mode {
	case "", "parentbased_traceidratio":
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio)), nil
	case "traceidratio":
		return sdktrace.TraceIDRatioBased(ratio), nil
	case "always_on":
		return sdktrace.AlwaysSample(), nil
	case "always_off":
		return sdktrace.NeverSample(), nil
	case "parentbased_always_on":
		return sdktrace.ParentBased(sdktrace.AlwaysSample()), nil
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample()), nil
	default:
		return nil, fmt.Errorf("unsupported observability.traces.sampling.mode %q", cfg.Mode)
	}
}

func newResource(opts Options) (*sdkresource.Resource, error) {
	attributes := []attribute.KeyValue{
		attribute.String("service.namespace", serviceNamespace),
		attribute.String("service.name", strings.TrimSpace(opts.ServiceName)),
	}
	if version := strings.TrimSpace(opts.ServiceVersion); version != "" {
		attributes = append(attributes, attribute.String("service.version", version))
	}
	if environment := strings.TrimSpace(opts.Config.DeploymentEnvironment); environment != "" {
		attributes = append(attributes, attribute.String("deployment.environment.name", environment))
	}

	resource, err := sdkresource.Merge(
		sdkresource.Default(),
		sdkresource.NewSchemaless(attributes...),
	)
	if err != nil {
		return nil, fmt.Errorf("build telemetry resource: %w", err)
	}
	return resource, nil
}

func runtimeconfigHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}

	out := make(map[string]string, len(headers))
	for key, value := range headers {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		out[trimmedKey] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func hasURLScheme(endpoint string) bool {
	return strings.Contains(strings.TrimSpace(endpoint), "://")
}

func normalizeEndpointURL(endpoint string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", fmt.Errorf("invalid observability.otlp.endpoint %q: %w", endpoint, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid observability.otlp.endpoint %q", endpoint)
	}
	return parsed.String(), nil
}

func normalizeOTLPHTTPTraceEndpointURL(endpoint string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", fmt.Errorf("invalid observability.otlp.endpoint %q: %w", endpoint, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid observability.otlp.endpoint %q", endpoint)
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = "/v1/traces"
	}
	return parsed.String(), nil
}

func normalizeOTLPHTTPMetricEndpointURL(endpoint string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", fmt.Errorf("invalid observability.otlp.endpoint %q: %w", endpoint, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid observability.otlp.endpoint %q", endpoint)
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = "/v1/metrics"
	}
	return parsed.String(), nil
}

func defaultTracerName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return defaultTraceName
	}
	return trimmed
}

func TraceIDFromContext(ctx context.Context) string {
	return TraceIDFromSpanContext(trace.SpanContextFromContext(ctx))
}

func TraceIDFromSpanContext(spanContext trace.SpanContext) string {
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}
