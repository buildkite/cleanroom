package client

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type integrationAdapter struct{}

func (integrationAdapter) Name() string { return "firecracker" }

func (integrationAdapter) Run(_ context.Context, req backend.RunRequest) (*backend.RunResult, error) {
	return &backend.RunResult{
		RunID:    req.RunID,
		ExitCode: 0,
		Stdout:   "hello from cleanroom\n",
		Message:  "ok",
	}, nil
}

func (a integrationAdapter) RunStream(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
	result, err := a.Run(ctx, req)
	if err != nil {
		return nil, err
	}
	if stream.OnStdout != nil {
		stream.OnStdout([]byte(result.Stdout))
	}
	return result, nil
}

func startIntegrationServer(t *testing.T) string {
	t.Helper()

	svc := &controlservice.Service{
		Config: runtimeconfig.Config{DefaultBackend: "firecracker"},
		Backends: map[string]backend.Adapter{
			"firecracker": integrationAdapter{},
		},
	}

	httpServer := httptest.NewServer(controlserver.New(svc, nil).Handler())
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

	terminateResp, err := client.TerminateSandbox(ctx, &TerminateSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	if !terminateResp.GetTerminated() {
		t.Fatal("expected terminated=true")
	}
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
