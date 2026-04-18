package observability

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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

func TestStartRejectsMissingEndpointWhenEnabled(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			Traces: runtimeconfig.TraceConfig{
				Exporter: "zipkin",
			},
		},
	})
	if err == nil {
		t.Fatal("expected Start to reject missing observability.traces.zipkin.endpoint")
	}
	if !strings.Contains(err.Error(), "missing observability.traces.zipkin.endpoint") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStartRejectsUnsupportedProtocol(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			Traces: runtimeconfig.TraceConfig{
				Exporter: "grpc",
			},
		},
	})
	if err == nil {
		t.Fatal("expected Start to reject unsupported protocol")
	}
	if !strings.Contains(err.Error(), "not supported in this build") {
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

func TestStartExportsZipkinSpanOnShutdown(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v2/spans"; got != want {
			t.Errorf("unexpected request path: got %q want %q", got, want)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		bodyCh <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	runtime, err := Start(context.Background(), Options{
		Config: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			Traces: runtimeconfig.TraceConfig{
				Exporter: "zipkin",
				Zipkin: runtimeconfig.ZipkinConfig{
					Endpoint: server.URL + "/api/v2/spans",
				},
			},
		},
		ServiceName: "cleanroom-cli",
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	_, span := runtime.Tracer("github.com/buildkite/cleanroom/internal/observability_test").Start(context.Background(), "cleanroom.test")
	span.End()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	select {
	case body := <-bodyCh:
		if !bytes.Contains(body, []byte(`"name":"cleanroom.test"`)) {
			t.Fatalf("expected exported span payload to contain span name, got %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for zipkin span export")
	}
}
