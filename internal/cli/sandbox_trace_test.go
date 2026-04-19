package cli

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracetest "go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestCreateSandboxWithProgressClosesClientStreamForTracing(t *testing.T) {
	t.Parallel()

	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(recorder)
	t.Cleanup(func() {
		_ = tracerProvider.Shutdown(context.Background())
	})

	clientObs, err := observability.NewWithTracerProvider(tracerProvider)
	if err != nil {
		t.Fatalf("NewWithTracerProvider for client returned error: %v", err)
	}
	serverObs, err := observability.NewWithTracerProvider(tracerProvider)
	if err != nil {
		t.Fatalf("NewWithTracerProvider for server returned error: %v", err)
	}

	service := &controlservice.Service{
		Config:        runtimeconfig.Config{DefaultBackend: "firecracker"},
		Backends:      map[string]backend.Adapter{"firecracker": &integrationAdapter{}},
		Observability: serverObs,
	}
	httpServer := httptest.NewServer(controlserver.New(service, nil, serverObs.ConnectInterceptor()).Handler())
	defer httpServer.Close()

	ep, err := endpoint.Resolve(httpServer.URL)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	client, err := controlclient.New(ep, controlclient.WithConnectInterceptors(clientObs.ConnectInterceptor()))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	rootCtx, rootSpan := clientObs.Tracer("github.com/buildkite/cleanroom/internal/cli_test").Start(context.Background(), "cleanroom.exec")
	_, sandboxID, err := createSandboxWithProgress(rootCtx, nil, nil, client, &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  sandboxTraceTestPolicy(),
	})
	rootSpan.End()
	if err != nil {
		t.Fatalf("createSandboxWithProgress returned error: %v", err)
	}
	if sandboxID == "" {
		t.Fatal("expected sandbox id")
	}

	clientSpan := findEndedSpan(recorder.Ended(), "cleanroom.v1.SandboxService/CreateSandboxStream", trace.SpanKindClient)
	if clientSpan == nil {
		t.Fatalf("expected ended client CreateSandboxStream span, got %#v", recorder.Ended())
	}
	if got, want := clientSpan.Parent().SpanID(), rootSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("unexpected client stream parent span id: got %s want %s", got, want)
	}

	serverSpan := findEndedSpan(recorder.Ended(), "cleanroom.v1.SandboxService/CreateSandboxStream", trace.SpanKindServer)
	if serverSpan == nil {
		t.Fatalf("expected ended server CreateSandboxStream span, got %#v", recorder.Ended())
	}
	if got, want := serverSpan.Parent().SpanID(), clientSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("unexpected server stream parent span id: got %s want %s", got, want)
	}
	if got, want := serverSpan.SpanContext().TraceID(), rootSpan.SpanContext().TraceID(); got != want {
		t.Fatalf("unexpected server stream trace id: got %s want %s", got, want)
	}
}

func sandboxTraceTestPolicy() *cleanroomv1.Policy {
	return &cleanroomv1.Policy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
	}
}

func findEndedSpan(spans []sdktrace.ReadOnlySpan, name string, kind trace.SpanKind) sdktrace.ReadOnlySpan {
	for _, span := range spans {
		if span.Name() == name && span.SpanKind() == kind {
			return span
		}
	}
	return nil
}
