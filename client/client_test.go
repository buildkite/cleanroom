package client

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type integrationAdapter struct{}

func (integrationAdapter) Name() string { return "firecracker" }

func (integrationAdapter) ProvisionSandbox(context.Context, backend.ProvisionRequest) error {
	return nil
}

func (integrationAdapter) RunInSandbox(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
	if stream.OnStdout != nil {
		stream.OnStdout([]byte("hello from cleanroom\n"))
	}
	result := &backend.ExecutionResult{
		ExecutionID: req.ExecutionID,
		ExitCode:    0,
		Message:     "ok",
	}
	return result, nil
}

func (integrationAdapter) TerminateSandbox(context.Context, string) error { return nil }

type snapshotIntegrationAdapter struct {
	integrationAdapter
}

func (snapshotIntegrationAdapter) CreateSnapshot(_ context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
	return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
}

func (snapshotIntegrationAdapter) ProvisionSandboxFromSnapshot(context.Context, backend.ProvisionFromSnapshotRequest) error {
	return nil
}

func (snapshotIntegrationAdapter) DeleteSnapshot(context.Context, backend.DeleteSnapshotRequest) error {
	return nil
}

func startIntegrationServer(t *testing.T) string {
	t.Helper()

	return startSnapshotTestServer(t, integrationAdapter{})
}

func startSnapshotTestServer(t *testing.T, adapter backend.Adapter) string {
	t.Helper()

	return startSnapshotTestServerWithObservability(t, adapter, nil)
}

func startSnapshotTestServerWithObservability(t *testing.T, adapter backend.Adapter, obs *observability.Runtime, interceptors ...connect.Interceptor) string {
	t.Helper()

	store, err := snapshotstore.New(snapshotstore.Options{
		MetadataDBPath: t.TempDir() + "/snapshots.db",
	})
	if err != nil {
		t.Fatalf("create snapshot store: %v", err)
	}

	svc := &controlservice.Service{
		Config: runtimeconfig.Config{
			DefaultBackend: "firecracker",
			Backends: runtimeconfig.Backends{
				Firecracker: runtimeconfig.FirecrackerConfig{
					Snapshots: runtimeconfig.SnapshotConfig{
						Enabled: true,
						Driver:  "file",
					},
				},
			},
		},
		SnapshotStore: store,
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
		Observability: obs,
	}

	httpServer := httptest.NewServer(controlserver.New(svc, nil, interceptors...).Handler())
	t.Cleanup(httpServer.Close)
	return httpServer.URL
}

func testPolicy() *Policy {
	return &Policy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
	}
}

func TestClientLifecycle(t *testing.T) {
	host := startIntegrationServer(t)

	client, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	createSandboxResp, err := client.CreateSandbox(ctx, &CreateSandboxRequest{
		Policy:  testPolicy(),
		Backend: "firecracker",
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if createSandboxResp.GetSandbox().GetStatus() != SandboxStatus_SANDBOX_STATUS_READY {
		t.Fatalf("unexpected sandbox status: %v", createSandboxResp.GetSandbox().GetStatus())
	}

	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	if sandboxID == "" {
		t.Fatal("expected sandbox_id")
	}

	getSandboxResp, err := client.GetSandbox(ctx, &GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if getSandboxResp.GetSandbox().GetSandboxId() != sandboxID {
		t.Fatalf("GetSandbox sandbox id mismatch: got %q want %q", getSandboxResp.GetSandbox().GetSandboxId(), sandboxID)
	}

	listResp, err := client.ListSandboxes(ctx, &ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("ListSandboxes returned error: %v", err)
	}
	if len(listResp.GetSandboxes()) == 0 {
		t.Fatal("expected at least one sandbox")
	}

	createExecutionResp, err := client.CreateExecution(ctx, &CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "hello"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()
	if executionID == "" {
		t.Fatal("expected execution_id")
	}

	stream, err := client.StreamExecution(ctx, &StreamExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Follow:      true,
	})
	if err != nil {
		t.Fatalf("StreamExecution returned error: %v", err)
	}

	sawStdout := false
	sawExit := false
	for stream.Receive() {
		event := stream.Msg()
		if strings.Contains(string(event.GetStdout()), "hello from cleanroom") {
			sawStdout = true
		}
		if exit := event.GetExit(); exit != nil {
			sawExit = true
			if exit.GetStatus() != ExecutionStatus_EXECUTION_STATUS_SUCCEEDED {
				t.Fatalf("unexpected exit status: %v", exit.GetStatus())
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamExecution stream error: %v", err)
	}
	if !sawStdout {
		t.Fatal("expected stdout event")
	}
	if !sawExit {
		t.Fatal("expected exit event")
	}

	getExecutionResp, err := client.GetExecution(ctx, &GetExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
	})
	if err != nil {
		t.Fatalf("GetExecution returned error: %v", err)
	}
	if got := getExecutionResp.GetExecution().GetStatus(); got != ExecutionStatus_EXECUTION_STATUS_SUCCEEDED {
		t.Fatalf("unexpected execution status: %v", got)
	}

	listExecutionsResp, err := client.ListExecutions(ctx, &ListExecutionsRequest{All: true})
	if err != nil {
		t.Fatalf("ListExecutions returned error: %v", err)
	}
	foundExecution := false
	for _, execution := range listExecutionsResp.GetExecutions() {
		if execution.GetExecutionId() != executionID {
			continue
		}
		foundExecution = true
		if got := execution.GetSandboxId(); got != sandboxID {
			t.Fatalf("ListExecutions sandbox id mismatch: got %q want %q", got, sandboxID)
		}
	}
	if !foundExecution {
		t.Fatalf("expected ListExecutions to include %q", executionID)
	}

	terminateResp, err := client.TerminateSandbox(ctx, &TerminateSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	if !terminateResp.GetTerminated() {
		t.Fatal("expected terminated=true")
	}
}

func TestWithConnectInterceptorsPropagatesTraceContext(t *testing.T) {
	t.Parallel()

	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(recorder)
	t.Cleanup(func() {
		_ = tracerProvider.Shutdown(context.Background())
	})

	serverObs, err := observability.NewWithTracerProvider(tracerProvider)
	if err != nil {
		t.Fatalf("NewWithTracerProvider returned error: %v", err)
	}
	host := startSnapshotTestServerWithObservability(t, integrationAdapter{}, serverObs, serverObs.ConnectInterceptor())

	clientInterceptor, err := otelconnect.NewInterceptor(
		otelconnect.WithTracerProvider(tracerProvider),
		otelconnect.WithPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		)),
		otelconnect.WithoutMetrics(),
		otelconnect.WithoutServerPeerAttributes(),
		otelconnect.WithoutTraceEvents(),
	)
	if err != nil {
		t.Fatalf("create client otelconnect interceptor: %v", err)
	}

	client, err := New(host, WithConnectInterceptors(clientInterceptor))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	createSandboxResp, err := client.CreateSandbox(ctx, &CreateSandboxRequest{
		Policy:  testPolicy(),
		Backend: "firecracker",
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	if sandboxID == "" {
		t.Fatal("expected sandbox_id")
	}

	rootCtx, rootSpan := tracerProvider.Tracer("github.com/buildkite/cleanroom/client_test").Start(ctx, "bot.turn")
	rootSpanContext := rootSpan.SpanContext()
	createExecutionResp, err := client.CreateExecution(rootCtx, &CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "hello"},
	})
	rootSpan.End()
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()
	if executionID == "" {
		t.Fatal("expected execution_id")
	}

	stream, err := client.StreamExecution(ctx, &StreamExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Follow:      true,
	})
	if err != nil {
		t.Fatalf("StreamExecution returned error: %v", err)
	}
	sawExit := false
	for stream.Receive() {
		if stream.Msg().GetExit() != nil {
			sawExit = true
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamExecution stream error: %v", err)
	}
	if !sawExit {
		t.Fatal("expected exit event")
	}

	clientSpan := waitForEndedSpan(t, recorder, "cleanroom.v1.ExecutionService/CreateExecution", trace.SpanKindClient)
	serverSpan := waitForEndedSpan(t, recorder, "cleanroom.v1.ExecutionService/CreateExecution", trace.SpanKindServer)
	createSpan := waitForEndedSpan(t, recorder, observability.SpanExecutionCreate, trace.SpanKindInternal)
	runSpan := waitForEndedSpan(t, recorder, observability.SpanExecutionRun, trace.SpanKindInternal)

	if got, want := clientSpan.Parent().SpanID(), rootSpanContext.SpanID(); got != want {
		t.Fatalf("unexpected client span parent: got %s want %s", got, want)
	}
	if got, want := serverSpan.Parent().SpanID(), clientSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("unexpected server span parent: got %s want %s", got, want)
	}
	if got, want := createSpan.Parent().SpanID(), serverSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("unexpected execution create span parent: got %s want %s", got, want)
	}
	if got, want := runSpan.Parent().SpanID(), createSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("unexpected execution run span parent: got %s want %s", got, want)
	}
	for _, span := range []struct {
		name string
		span sdktrace.ReadOnlySpan
	}{
		{name: "client RPC", span: clientSpan},
		{name: "server RPC", span: serverSpan},
		{name: "execution create", span: createSpan},
		{name: "execution run", span: runSpan},
	} {
		if got, want := span.span.SpanContext().TraceID(), rootSpanContext.TraceID(); got != want {
			t.Fatalf("unexpected %s trace id: got %s want %s", span.name, got, want)
		}
	}
}

func TestClientSnapshotLifecycle(t *testing.T) {
	host := startSnapshotTestServer(t, snapshotIntegrationAdapter{})

	client, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	createSandboxResp, err := client.CreateSandbox(ctx, &CreateSandboxRequest{
		Policy:  testPolicy(),
		Backend: "firecracker",
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	if sandboxID == "" {
		t.Fatal("expected sandbox_id")
	}

	createSnapshotResp, err := client.CreateSnapshot(ctx, &CreateSnapshotRequest{
		SandboxId: sandboxID,
		Name:      "golden",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	snapshotID := createSnapshotResp.GetSnapshot().GetSnapshotId()
	if snapshotID == "" {
		t.Fatal("expected snapshot_id")
	}

	getSnapshotResp, err := client.GetSnapshot(ctx, &GetSnapshotRequest{SnapshotId: snapshotID})
	if err != nil {
		t.Fatalf("GetSnapshot returned error: %v", err)
	}
	if got, want := getSnapshotResp.GetSnapshot().GetName(), "golden"; got != want {
		t.Fatalf("unexpected snapshot name: got %q want %q", got, want)
	}

	listSnapshotsResp, err := client.ListSnapshots(ctx, &ListSnapshotsRequest{})
	if err != nil {
		t.Fatalf("ListSnapshots returned error: %v", err)
	}
	if got, want := len(listSnapshotsResp.GetSnapshots()), 1; got != want {
		t.Fatalf("unexpected snapshot count: got %d want %d", got, want)
	}

	fromSnapshotResp, err := client.CreateSandbox(ctx, &CreateSandboxRequest{
		Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: snapshotID},
	})
	if err != nil {
		t.Fatalf("CreateSandbox from snapshot returned error: %v", err)
	}
	if got := fromSnapshotResp.GetSandbox().GetSandboxId(); got == "" {
		t.Fatal("expected snapshot-backed sandbox_id")
	}

	deleteSnapshotResp, err := client.DeleteSnapshot(ctx, &DeleteSnapshotRequest{SnapshotId: snapshotID})
	if err != nil {
		t.Fatalf("DeleteSnapshot returned error: %v", err)
	}
	if !deleteSnapshotResp.GetDeleted() {
		t.Fatal("expected deleted=true")
	}

	for _, id := range []string{fromSnapshotResp.GetSandbox().GetSandboxId(), sandboxID} {
		terminateResp, terminateErr := client.TerminateSandbox(ctx, &TerminateSandboxRequest{SandboxId: id})
		if terminateErr != nil {
			t.Fatalf("TerminateSandbox returned error for %q: %v", id, terminateErr)
		}
		if !terminateResp.GetTerminated() {
			t.Fatalf("expected terminated=true for %q", id)
		}
	}
}

func waitForEndedSpan(t *testing.T, recorder *tracetest.SpanRecorder, name string, kind trace.SpanKind) sdktrace.ReadOnlySpan {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		spans := recorder.Ended()
		if span := findEndedSpan(spans, name, kind); span != nil {
			return span
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected ended %s span %q, got %s", kind, name, endedSpanSummary(spans))
		}
		time.Sleep(10 * time.Millisecond)
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

func endedSpanSummary(spans []sdktrace.ReadOnlySpan) string {
	if len(spans) == 0 {
		return "<none>"
	}
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name())
	}
	return strings.Join(names, ", ")
}

func TestNewRejectsUnsupportedEndpointScheme(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
	}{
		{
			name: "tsnet",
			host: "tsnet://cleanroom:7777",
			want: "unsupported endpoint",
		},
		{
			name: "tssvc",
			host: "tssvc://cleanroom",
			want: "unsupported endpoint",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.host)
			if err == nil {
				t.Fatalf("expected error for endpoint %q", tc.host)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
