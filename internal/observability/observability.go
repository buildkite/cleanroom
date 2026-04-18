package observability

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/zipkin"
	"go.opentelemetry.io/otel/propagation"
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
	interceptor    connect.Interceptor
	shutdown       func(context.Context) error
}

func Start(ctx context.Context, opts Options) (*Runtime, error) {
	propagator := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
	runtime := &Runtime{
		tracerProvider: tracenoop.NewTracerProvider(),
		shutdown:       func(context.Context) error { return nil },
	}

	if opts.Config.Enabled {
		provider, err := newTracerProvider(ctx, opts)
		if err != nil {
			return nil, err
		}
		runtime.tracerProvider = provider
		runtime.shutdown = provider.Shutdown
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

func (r *Runtime) Tracer(name string, options ...trace.TracerOption) trace.Tracer {
	if r == nil {
		return tracenoop.NewTracerProvider().Tracer(defaultTracerName(name), options...)
	}
	return r.tracerProvider.Tracer(defaultTracerName(name), options...)
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

func newTraceExporter(_ context.Context, cfg runtimeconfig.ObservabilityConfig) (sdktrace.SpanExporter, error) {
	exporter := strings.ToLower(strings.TrimSpace(cfg.Traces.Exporter))
	if exporter == "" {
		if strings.TrimSpace(cfg.OTLP.Endpoint) != "" {
			return nil, errors.New("observability.otlp is configured but this build only supports observability.traces.exporter=zipkin")
		}
		exporter = "zipkin"
	}

	switch exporter {
	case "zipkin":
		return newZipkinExporter(cfg.Traces.Zipkin)
	case "otlp", "otlp_http", "otlp/http", "http", "http/protobuf", "grpc":
		return nil, fmt.Errorf("trace exporter %q is not supported in this build; use observability.traces.exporter=zipkin", exporter)
	default:
		return nil, fmt.Errorf("unsupported observability.traces.exporter %q", cfg.Traces.Exporter)
	}
}

func newZipkinExporter(cfg runtimeconfig.ZipkinConfig) (*zipkin.Exporter, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil, errors.New("missing observability.traces.zipkin.endpoint")
	}

	options := make([]zipkin.Option, 0, 1)
	if headers := runtimeconfigHeaders(cfg.Headers); len(headers) > 0 {
		options = append(options, zipkin.WithHeaders(headers))
	}

	exporter, err := zipkin.New(endpoint, options...)
	if err != nil {
		return nil, fmt.Errorf("configure zipkin trace exporter: %w", err)
	}
	return exporter, nil
}

func newSampler(cfg runtimeconfig.TraceSamplingConfig) (sdktrace.Sampler, error) {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	ratio := 1.0
	if cfg.Ratio != nil {
		ratio = *cfg.Ratio
	}
	if cfg.Ratio == nil {
		ratio = 1
	}
	if ratio < 0 || ratio > 1 {
		return nil, fmt.Errorf("observability.traces.sampling.ratio must be between 0 and 1, got %v", ratio)
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

func defaultTracerName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return defaultTraceName
	}
	return trimmed
}
