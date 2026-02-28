package controlserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlservice"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

type resizeUnsupportedAdapter struct {
	started chan struct{}
}

func (a *resizeUnsupportedAdapter) Name() string {
	return "resize-unsupported"
}

func (a *resizeUnsupportedAdapter) Run(context.Context, backend.RunRequest) (*backend.RunResult, error) {
	return nil, errors.New("unexpected Run call")
}

func (a *resizeUnsupportedAdapter) RunStream(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
	if stream.OnAttach != nil {
		stream.OnAttach(backend.AttachIO{
			WriteStdin: func([]byte) error { return nil },
		})
	}
	select {
	case a.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestApplyAttachInputIgnoresUnsupportedResize(t *testing.T) {
	adapter := &resizeUnsupportedAdapter{started: make(chan struct{}, 1)}
	svc := &controlservice.Service{
		Backends: map[string]backend.Adapter{"firecracker": adapter},
	}
	server := New(svc, nil)

	policy := &cleanroomv1.Policy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
	}
	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: policy})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh"},
		Options:   &cleanroomv1.ExecutionOptions{Tty: true},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	select {
	case <-adapter.started:
	case <-time.After(2 * time.Second):
		t.Fatal("execution did not start")
	}

	detached, err := server.applyAttachInput(context.Background(), sandboxID, executionID, &cleanroomv1.ExecutionAttachFrame{
		Payload: &cleanroomv1.ExecutionAttachFrame_Resize{
			Resize: &cleanroomv1.ExecutionResize{Cols: 120, Rows: 40},
		},
	})
	if err != nil {
		t.Fatalf("expected unsupported resize to be ignored, got error: %v", err)
	}
	if detached {
		t.Fatal("expected resize frame to keep attach session open")
	}

	if _, err := svc.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Signal:      2,
	}); err != nil {
		t.Fatalf("CancelExecution returned error: %v", err)
	}
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}
}
