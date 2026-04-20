package observability

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func float64Ptr(v float64) *float64 {
	return &v
}

func TestStartDisabledReturnsRuntime(t *testing.T) {
	runtime, err := Start(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if runtime == nil {
		t.Fatal("Start returned nil runtime")
	}
	if runtime.ConnectInterceptor() == nil {
		t.Fatal("expected connect interceptor")
	}
}

func TestStartDisabledLeavesExistingOTelErrorHandlerUntouched(t *testing.T) {
	original := otel.GetErrorHandler()
	t.Cleanup(func() { otel.SetErrorHandler(original) })

	previousErrCh := make(chan error, 1)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		previousErrCh <- err
	}))

	reportedErrCh := make(chan error, 1)
	runtime, err := Start(context.Background(), Options{
		ReportError: func(err error) {
			reportedErrCh <- err
		},
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	otel.Handle(errors.New("disabled observability"))

	select {
	case err := <-previousErrCh:
		if got, want := err.Error(), "disabled observability"; got != want {
			t.Fatalf("unexpected restored handler error: got %q want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for existing error handler")
	}

	select {
	case err := <-reportedErrCh:
		t.Fatalf("expected disabled observability not to install report handler, got %v", err)
	default:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
}

func TestStartRejectsUnsupportedTraceExporterWhenEnabled(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			OTLP: runtimeconfig.OTLPConfig{
				Endpoint: "http://localhost:4318",
			},
			Traces: runtimeconfig.TraceConfig{
				Exporter: "zipkin",
			},
		},
	})
	if err == nil {
		t.Fatal("expected Start to reject unsupported observability.traces.exporter")
	}
	if !strings.Contains(err.Error(), "unsupported observability.traces.exporter") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStartRejectsMissingOTLPEndpointWhenEnabled(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
		},
	})
	if err == nil {
		t.Fatal("expected Start to reject missing observability.otlp.endpoint")
	}
	if !strings.Contains(err.Error(), "missing observability.otlp.endpoint") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStartRejectsUnsupportedOTLPProtocol(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			OTLP: runtimeconfig.OTLPConfig{
				Endpoint: "http://localhost:4318",
				Protocol: "banana",
			},
		},
	})
	if err == nil {
		t.Fatal("expected Start to reject unsupported OTLP protocol")
	}
	if !strings.Contains(err.Error(), "observability.otlp.protocol") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStartRejectsOTLPHTTPPath(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			OTLP: runtimeconfig.OTLPConfig{
				Endpoint: "https://collector.example.test/v1/traces",
				Protocol: "http/protobuf",
			},
		},
	})
	if err == nil {
		t.Fatal("expected Start to reject OTLP HTTP endpoints with a path")
	}
	if !strings.Contains(err.Error(), "must not include a path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewSamplerRejectsOutOfRangeRatio(t *testing.T) {
	_, err := newSampler(runtimeconfig.TraceSamplingConfig{
		Mode:  "parentbased_traceidratio",
		Ratio: float64Ptr(2),
	})
	if err == nil {
		t.Fatal("expected newSampler to reject ratio > 1")
	}
	if !strings.Contains(err.Error(), "ratio must be between 0 and 1") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewSamplerDefaultsUnsetRatioToOne(t *testing.T) {
	sampler, err := newSampler(runtimeconfig.TraceSamplingConfig{
		Mode: "parentbased_traceidratio",
	})
	if err != nil {
		t.Fatalf("newSampler returned error: %v", err)
	}

	result := sampler.ShouldSample(
		sdktrace.SamplingParameters{
			Name: "test-default-ratio",
		},
	)
	if result.Decision != sdktrace.RecordAndSample {
		t.Fatalf("expected unset ratio to sample, got %v", result.Decision)
	}
}

func TestNewSamplerPreservesExplicitZeroRatio(t *testing.T) {
	sampler, err := newSampler(runtimeconfig.TraceSamplingConfig{
		Mode:  "traceidratio",
		Ratio: float64Ptr(0),
	})
	if err != nil {
		t.Fatalf("newSampler returned error: %v", err)
	}

	result := sampler.ShouldSample(
		sdktrace.SamplingParameters{
			Name: "test-zero-ratio",
		},
	)
	if result.Decision != sdktrace.Drop {
		t.Fatalf("expected explicit zero ratio to drop, got %v", result.Decision)
	}
}

func TestStartRoutesOTelErrorsToConfiguredReporter(t *testing.T) {
	original := otel.GetErrorHandler()
	t.Cleanup(func() { otel.SetErrorHandler(original) })

	previousErrCh := make(chan error, 1)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		previousErrCh <- err
	}))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0x0a, 0x00})
	}))
	defer server.Close()

	reportedErrCh := make(chan error, 1)
	runtime, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			OTLP: runtimeconfig.OTLPConfig{
				Endpoint: server.URL,
				Protocol: "http/protobuf",
			},
		},
		ServiceName: "cleanroom-cli",
		ReportError: func(err error) {
			reportedErrCh <- err
		},
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	otel.Handle(errors.New("collector unavailable"))

	select {
	case err := <-reportedErrCh:
		if got, want := err.Error(), "collector unavailable"; got != want {
			t.Fatalf("unexpected reported error: got %q want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for configured error reporter")
	}

	select {
	case err := <-previousErrCh:
		t.Fatalf("expected configured error reporter to replace previous handler, got %v", err)
	default:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	otel.Handle(errors.New("after shutdown"))

	select {
	case err := <-previousErrCh:
		if got, want := err.Error(), "after shutdown"; got != want {
			t.Fatalf("unexpected restored handler error: got %q want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for restored error handler")
	}
}

func TestStartSuppressesConfiguredReporterDuringShutdown(t *testing.T) {
	original := otel.GetErrorHandler()
	t.Cleanup(func() { otel.SetErrorHandler(original) })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	endpoint := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	reportedErrCh := make(chan error, 2)
	runtime, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			OTLP: runtimeconfig.OTLPConfig{
				Endpoint: "http://" + endpoint,
				Protocol: "http/protobuf",
			},
		},
		ServiceName: "cleanroom-cli",
		ReportError: func(err error) {
			reportedErrCh <- err
		},
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	counter, err := runtime.Meter("github.com/buildkite/cleanroom/internal/observability_test").Int64Counter("cleanroom.test.shutdown.metric")
	if err != nil {
		t.Fatalf("Int64Counter returned error: %v", err)
	}
	counter.Add(context.Background(), 1)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = runtime.Shutdown(shutdownCtx)
	if err == nil {
		t.Fatal("expected Shutdown to return an export error")
	}
	if !strings.Contains(err.Error(), "failed to upload metrics") {
		t.Fatalf("expected shutdown error to mention metric upload failure, got %v", err)
	}

	select {
	case err := <-reportedErrCh:
		t.Fatalf("expected shutdown errors to be returned, not reported asynchronously, got %v", err)
	default:
	}
}

func TestStartPreservesNestedOTelErrorHandlersAcrossRuntimeShutdown(t *testing.T) {
	original := otel.GetErrorHandler()
	t.Cleanup(func() { otel.SetErrorHandler(original) })

	previousErrCh := make(chan error, 1)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		previousErrCh <- err
	}))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0x0a, 0x00})
	}))
	defer server.Close()

	reportedErrCh1 := make(chan error, 1)
	runtime1, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			OTLP: runtimeconfig.OTLPConfig{
				Endpoint: server.URL,
				Protocol: "http/protobuf",
			},
		},
		ServiceName: "cleanroom-cli-1",
		ReportError: func(err error) {
			reportedErrCh1 <- err
		},
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	reportedErrCh2 := make(chan error, 2)
	runtime2, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			OTLP: runtimeconfig.OTLPConfig{
				Endpoint: server.URL,
				Protocol: "http/protobuf",
			},
		},
		ServiceName: "cleanroom-cli-2",
		ReportError: func(err error) {
			reportedErrCh2 <- err
		},
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	otel.Handle(errors.New("before shutdown"))

	select {
	case err := <-reportedErrCh2:
		if got, want := err.Error(), "before shutdown"; got != want {
			t.Fatalf("unexpected nested reporter error: got %q want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for nested reporter")
	}

	select {
	case err := <-reportedErrCh1:
		t.Fatalf("expected newer runtime to own otel errors, got %v", err)
	default:
	}
	select {
	case err := <-previousErrCh:
		t.Fatalf("expected previous handler to be shadowed, got %v", err)
	default:
	}

	shutdownCtx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	if err := runtime1.Shutdown(shutdownCtx1); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	otel.Handle(errors.New("after first shutdown"))

	select {
	case err := <-reportedErrCh2:
		if got, want := err.Error(), "after first shutdown"; got != want {
			t.Fatalf("unexpected error after first shutdown: got %q want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for remaining runtime reporter")
	}

	select {
	case err := <-reportedErrCh1:
		t.Fatalf("expected shutdown runtime not to resume ownership, got %v", err)
	default:
	}
	select {
	case err := <-previousErrCh:
		t.Fatalf("expected previous handler to remain shadowed, got %v", err)
	default:
	}

	shutdownCtx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := runtime2.Shutdown(shutdownCtx2); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	otel.Handle(errors.New("after second shutdown"))

	select {
	case err := <-previousErrCh:
		if got, want := err.Error(), "after second shutdown"; got != want {
			t.Fatalf("unexpected restored handler error: got %q want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for previous handler restoration")
	}
}

func TestRuntimeErrorReporterSuppressWaitsForInFlightHandle(t *testing.T) {
	enteredReport := make(chan struct{})
	releaseReport := make(chan struct{})
	reporter := &runtimeErrorReporter{report: func(error) {
		close(enteredReport)
		<-releaseReport
	}}

	handleDone := make(chan struct{})
	go func() {
		defer close(handleDone)
		reporter.Handle(errors.New("collector unavailable"))
	}()

	select {
	case <-enteredReport:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for in-flight report")
	}

	suppressDone := make(chan struct{})
	go func() {
		reporter.suppress()
		close(suppressDone)
	}()

	select {
	case <-suppressDone:
		t.Fatal("expected suppress to wait for in-flight report")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseReport)

	select {
	case <-suppressDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for suppress to finish")
	}

	select {
	case <-handleDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for handle to finish")
	}
}

func TestStartExportsOTLPHTTPSpanOnShutdown(t *testing.T) {
	bodyCh := make(chan []byte, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/metrics" {
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{0x0a, 0x00})
			return
		}
		if got, want := r.URL.Path, "/v1/traces"; got != want {
			t.Errorf("unexpected request path: got %q want %q", got, want)
		}
		if got, want := r.Header.Get("X-Trace-Token"), "secret"; got != want {
			t.Errorf("unexpected request header: got %q want %q", got, want)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		select {
		case bodyCh <- body:
		default:
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0x0a, 0x00})
	}))
	defer server.Close()

	runtime, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			OTLP: runtimeconfig.OTLPConfig{
				Endpoint: server.URL,
				Protocol: "http/protobuf",
				Headers: map[string]string{
					"X-Trace-Token": "secret",
				},
			},
		},
		ServiceName: "cleanroom-cli",
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	_, span := runtime.Tracer("github.com/buildkite/cleanroom/internal/observability_test").Start(context.Background(), "cleanroom.test.otlp.http")
	span.End()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	select {
	case body := <-bodyCh:
		var req coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal OTLP HTTP request: %v", err)
		}
		if len(req.ResourceSpans) == 0 {
			t.Fatal("expected exported OTLP HTTP request to contain resource spans")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OTLP HTTP span export")
	}
}

func TestStartExportsOTLPHTTPMetricsOnShutdown(t *testing.T) {
	bodyCh := make(chan []byte, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" {
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{0x0a, 0x00})
			return
		}
		if got, want := r.Header.Get("X-Metric-Token"), "secret"; got != want {
			t.Errorf("unexpected request header: got %q want %q", got, want)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		select {
		case bodyCh <- body:
		default:
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0x0a, 0x00})
	}))
	defer server.Close()

	runtime, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			OTLP: runtimeconfig.OTLPConfig{
				Endpoint: server.URL,
				Protocol: "http/protobuf",
				Headers: map[string]string{
					"X-Metric-Token": "secret",
				},
			},
		},
		ServiceName: "cleanroom-cli",
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	counter, err := runtime.Meter("github.com/buildkite/cleanroom/internal/observability_test").Int64Counter("cleanroom.test.metric")
	if err != nil {
		t.Fatalf("Int64Counter returned error: %v", err)
	}
	counter.Add(context.Background(), 1, metric.WithAttributes())

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	select {
	case body := <-bodyCh:
		var req colmetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal OTLP HTTP metric request: %v", err)
		}
		if len(req.ResourceMetrics) == 0 {
			t.Fatal("expected exported OTLP HTTP metric request to contain resource metrics")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OTLP HTTP metric export")
	}
}

func TestStartExportsOTLPGRPCSpanOnShutdownWithDefaultProtocol(t *testing.T) {
	requestCh := make(chan *coltracepb.ExportTraceServiceRequest, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	grpcServer := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(grpcServer, &captureTraceService{requests: requestCh})
	colmetricspb.RegisterMetricsServiceServer(grpcServer, &captureMetricService{})
	defer grpcServer.Stop()
	go func() {
		_ = grpcServer.Serve(listener)
	}()

	runtime, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			OTLP: runtimeconfig.OTLPConfig{
				Endpoint: listener.Addr().String(),
				Insecure: true,
				Headers: map[string]string{
					"x-trace-token": "secret",
				},
			},
		},
		ServiceName: "cleanroom-cli",
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	_, span := runtime.Tracer("github.com/buildkite/cleanroom/internal/observability_test").Start(context.Background(), "cleanroom.test.otlp.grpc")
	span.End()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	select {
	case req := <-requestCh:
		if len(req.ResourceSpans) == 0 {
			t.Fatal("expected exported OTLP gRPC request to contain resource spans")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OTLP gRPC span export")
	}
}

func TestStartExportsOTLPGRPCMetricsOnShutdown(t *testing.T) {
	requestCh := make(chan *colmetricspb.ExportMetricsServiceRequest, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	grpcServer := grpc.NewServer()
	colmetricspb.RegisterMetricsServiceServer(grpcServer, &captureMetricService{requests: requestCh})
	defer grpcServer.Stop()
	go func() {
		_ = grpcServer.Serve(listener)
	}()

	runtime, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			OTLP: runtimeconfig.OTLPConfig{
				Endpoint: listener.Addr().String(),
				Insecure: true,
			},
		},
		ServiceName: "cleanroom-cli",
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	counter, err := runtime.Meter("github.com/buildkite/cleanroom/internal/observability_test").Int64Counter("cleanroom.test.grpc.metric")
	if err != nil {
		t.Fatalf("Int64Counter returned error: %v", err)
	}
	counter.Add(context.Background(), 1)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	select {
	case req := <-requestCh:
		if len(req.ResourceMetrics) == 0 {
			t.Fatal("expected exported OTLP gRPC request to contain resource metrics")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OTLP gRPC metric export")
	}
}

type captureTraceService struct {
	coltracepb.UnimplementedTraceServiceServer
	requests chan *coltracepb.ExportTraceServiceRequest
}

func (s *captureTraceService) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	if s != nil && s.requests != nil {
		s.requests <- req
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

type captureMetricService struct {
	colmetricspb.UnimplementedMetricsServiceServer
	requests chan *colmetricspb.ExportMetricsServiceRequest
}

func (s *captureMetricService) Export(_ context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	if s != nil && s.requests != nil {
		s.requests <- req
	}
	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}
