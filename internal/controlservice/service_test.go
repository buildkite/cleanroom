package controlservice

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
)

type stubAdapter struct {
	result         *backend.RunResult
	runFn          func(context.Context, backend.RunRequest) (*backend.RunResult, error)
	runStreamFn    func(context.Context, backend.RunRequest, backend.OutputStream) (*backend.RunResult, error)
	provisionFn    func(context.Context, backend.ProvisionRequest) error
	terminateFn    func(context.Context, string) error
	req            backend.RunRequest
	runCalls       int
	provisionCalls int
	terminateCalls int
}

func (s *stubAdapter) Name() string { return "stub" }

func (s *stubAdapter) Run(ctx context.Context, req backend.RunRequest) (*backend.RunResult, error) {
	s.req = req
	s.runCalls++
	if s.runFn != nil {
		return s.runFn(ctx, req)
	}
	if s.result != nil {
		return s.result, nil
	}
	return &backend.RunResult{
		RunID:      req.RunID,
		ExitCode:   0,
		LaunchedVM: true,
		PlanPath:   "/tmp/plan",
		RunDir:     "/tmp/run",
		Message:    "ok",
	}, nil
}

func (s *stubAdapter) RunStream(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
	s.req = req
	s.runCalls++
	if s.runStreamFn != nil {
		return s.runStreamFn(ctx, req, stream)
	}
	var (
		result *backend.RunResult
		err    error
	)
	if s.runFn != nil {
		result, err = s.runFn(ctx, req)
		if err != nil {
			return nil, err
		}
	} else {
		result = s.result
	}
	if result == nil {
		result = &backend.RunResult{
			RunID:      req.RunID,
			ExitCode:   0,
			LaunchedVM: true,
			PlanPath:   "/tmp/plan",
			RunDir:     "/tmp/run",
			Message:    "ok",
		}
	}
	if stream.OnStdout != nil && result.Stdout != "" {
		stream.OnStdout([]byte(result.Stdout))
	}
	if stream.OnStderr != nil && result.Stderr != "" {
		stream.OnStderr([]byte(result.Stderr))
	}
	return result, nil
}

func (s *stubAdapter) ProvisionSandbox(ctx context.Context, req backend.ProvisionRequest) error {
	s.provisionCalls++
	if s.provisionFn != nil {
		return s.provisionFn(ctx, req)
	}
	return nil
}

func (s *stubAdapter) RunInSandbox(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
	return s.RunStream(ctx, req, stream)
}

func (s *stubAdapter) TerminateSandbox(ctx context.Context, sandboxID string) error {
	s.terminateCalls++
	if s.terminateFn != nil {
		return s.terminateFn(ctx, sandboxID)
	}
	return nil
}

type stubLoader struct {
	compiled *policy.CompiledPolicy
	source   string
}

func (l stubLoader) LoadAndCompile(_ string) (*policy.CompiledPolicy, string, error) {
	return l.compiled, l.source, nil
}

func testPolicy() *cleanroomv1.Policy {
	return &cleanroomv1.Policy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
	}
}

func newTestService(adapter backend.Adapter) *Service {
	return &Service{
		Loader: stubLoader{
			compiled: &policy.CompiledPolicy{
				Version:        1,
				NetworkDefault: "deny",
				ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			},
			source: "/repo/cleanroom.yaml",
		},
		Backends: map[string]backend.Adapter{"firecracker": adapter},
	}
}

func TestExecutionStreamIncludesExitEvent(t *testing.T) {
	adapter := &stubAdapter{
		runFn: func(_ context.Context, req backend.RunRequest) (*backend.RunResult, error) {
			return &backend.RunResult{
				RunID:       req.RunID,
				ExitCode:    7,
				LaunchedVM:  true,
				PlanPath:    "/tmp/plan",
				RunDir:      "/tmp/run",
				ImageRef:    "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ImageDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				Message:     "done",
				Stdout:      "hello stdout\n",
				Stderr:      "hello stderr\n",
			}, nil
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"--", "echo", "hi"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	history, updates, done, unsubscribe, err := svc.SubscribeExecutionEvents(sandboxID, executionID)
	if err != nil {
		t.Fatalf("SubscribeExecutionEvents returned error: %v", err)
	}
	defer unsubscribe()

	events := collectExecutionEvents(t, history, updates, done)
	var sawStdout bool
	var sawStderr bool
	var exit *cleanroomv1.ExecutionExit
	for _, event := range events {
		if got, want := event.GetImageDigest(), "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"; got != want {
			t.Fatalf("expected image digest on event, got %q want %q", got, want)
		}
		switch payload := event.Payload.(type) {
		case *cleanroomv1.ExecutionStreamEvent_Stdout:
			if strings.Contains(string(payload.Stdout), "hello stdout") {
				sawStdout = true
			}
		case *cleanroomv1.ExecutionStreamEvent_Stderr:
			if strings.Contains(string(payload.Stderr), "hello stderr") {
				sawStderr = true
			}
		case *cleanroomv1.ExecutionStreamEvent_Exit:
			exit = payload.Exit
		}
	}
	if !sawStdout {
		t.Fatalf("expected stdout event in stream, events=%d", len(events))
	}
	if !sawStderr {
		t.Fatalf("expected stderr event in stream, events=%d", len(events))
	}
	if exit == nil {
		t.Fatalf("expected exit event in stream, events=%d", len(events))
	}
	if got, want := exit.GetExitCode(), int32(7); got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
	if got, want := exit.GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED; got != want {
		t.Fatalf("unexpected exit status: got %v want %v", got, want)
	}
}

func TestCancelExecutionTransitionsToCanceled(t *testing.T) {
	started := make(chan struct{}, 1)
	adapter := &stubAdapter{
		runFn: func(ctx context.Context, _ backend.RunRequest) (*backend.RunResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sleep", "10"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	history, updates, done, unsubscribe, err := svc.SubscribeExecutionEvents(sandboxID, executionID)
	if err != nil {
		t.Fatalf("SubscribeExecutionEvents returned error: %v", err)
	}
	defer unsubscribe()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for execution to start")
	}

	cancelResp, err := svc.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Signal:      15,
	})
	if err != nil {
		t.Fatalf("CancelExecution returned error: %v", err)
	}
	if !cancelResp.GetAccepted() {
		t.Fatal("expected cancel request to be accepted")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled execution to finish")
	}

	getResp, err := svc.GetExecution(context.Background(), &cleanroomv1.GetExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
	})
	if err != nil {
		t.Fatalf("GetExecution returned error: %v", err)
	}
	if got, want := getResp.GetExecution().GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED; got != want {
		t.Fatalf("unexpected execution status: got %v want %v", got, want)
	}

	events := collectExecutionEvents(t, history, updates, done)
	var sawCancelMessage bool
	var exit *cleanroomv1.ExecutionExit
	for _, event := range events {
		if payload, ok := event.Payload.(*cleanroomv1.ExecutionStreamEvent_Message); ok && strings.Contains(payload.Message, "cancel requested") {
			sawCancelMessage = true
		}
		if payload, ok := event.Payload.(*cleanroomv1.ExecutionStreamEvent_Exit); ok {
			exit = payload.Exit
		}
	}
	if !sawCancelMessage {
		t.Fatalf("expected cancel message event, events=%d", len(events))
	}
	if exit == nil {
		t.Fatalf("expected exit event after cancel, events=%d", len(events))
	}
	if got, want := exit.GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED; got != want {
		t.Fatalf("unexpected exit status: got %v want %v", got, want)
	}
	if got, want := exit.GetExitCode(), int32(143); got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
}

func TestCreateSandboxProvisionsPersistentBackend(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	if createResp.GetSandbox().GetSandboxId() == "" {
		t.Fatal("expected sandbox id")
	}
	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("unexpected provision call count: got %d want %d", got, want)
	}

	if _, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: createResp.GetSandbox().GetSandboxId()}); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	if got, want := adapter.terminateCalls, 1; got != want {
		t.Fatalf("unexpected terminate call count: got %d want %d", got, want)
	}
}

func TestCreateExecutionRejectsWhenSandboxBusy(t *testing.T) {
	started := make(chan struct{}, 1)
	adapter := &stubAdapter{
		runFn: func(ctx context.Context, req backend.RunRequest) (*backend.RunResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	firstExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	firstExecutionID := firstExecutionResp.GetExecution().GetExecutionId()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first execution to start")
	}

	_, err = svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "second"},
	})
	if err == nil {
		t.Fatal("expected sandbox_busy error")
	}
	if !strings.Contains(err.Error(), "sandbox_busy") {
		t.Fatalf("expected sandbox_busy error, got: %v", err)
	}

	if _, err := svc.CancelExecution(context.Background(), &cleanroomv1.CancelExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: firstExecutionID,
		Signal:      15,
	}); err != nil {
		t.Fatalf("CancelExecution returned error: %v", err)
	}
	if _, err := svc.WaitExecution(context.Background(), sandboxID, firstExecutionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}
}

func TestTerminateSandboxReturnsBackendTerminateError(t *testing.T) {
	adapter := &stubAdapter{
		terminateFn: func(context.Context, string) error {
			return errors.New("boom")
		},
	}
	svc := newTestService(adapter)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	_, err = svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{SandboxId: createResp.GetSandbox().GetSandboxId()})
	if err == nil {
		t.Fatal("expected terminate backend error")
	}
	if !strings.Contains(err.Error(), "terminate backend sandbox") {
		t.Fatalf("unexpected terminate error: %v", err)
	}
}

func TestExecutionAttachIOForwarding(t *testing.T) {
	started := make(chan struct{}, 1)
	stdinChunks := make(chan string, 1)
	resizes := make(chan [2]uint32, 1)
	adapter := &stubAdapter{
		runStreamFn: func(ctx context.Context, _ backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(data []byte) error {
						stdinChunks <- string(data)
						return nil
					},
					ResizeTTY: func(cols, rows uint32) error {
						resizes <- [2]uint32{cols, rows}
						return nil
					},
				})
			}
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh"},
		Options: &cleanroomv1.ExecutionOptions{
			Tty: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for execution to start")
	}

	if err := svc.WriteExecutionStdin(sandboxID, executionID, []byte("hello\n")); err != nil {
		t.Fatalf("WriteExecutionStdin returned error: %v", err)
	}
	if err := svc.ResizeExecutionTTY(sandboxID, executionID, 120, 40); err != nil {
		t.Fatalf("ResizeExecutionTTY returned error: %v", err)
	}

	select {
	case got := <-stdinChunks:
		if got != "hello\n" {
			t.Fatalf("unexpected stdin payload: got %q want %q", got, "hello\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stdin callback")
	}

	select {
	case got := <-resizes:
		if got != [2]uint32{120, 40} {
			t.Fatalf("unexpected resize payload: got %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for resize callback")
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

func TestExecutionAttachIOWaitsForDelayedAttachRegistration(t *testing.T) {
	started := make(chan struct{}, 1)
	stdinChunks := make(chan string, 1)
	resizes := make(chan [2]uint32, 1)
	adapter := &stubAdapter{
		runStreamFn: func(ctx context.Context, _ backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			time.Sleep(100 * time.Millisecond)
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(data []byte) error {
						stdinChunks <- string(data)
						return nil
					},
					ResizeTTY: func(cols, rows uint32) error {
						resizes <- [2]uint32{cols, rows}
						return nil
					},
				})
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh"},
		Options: &cleanroomv1.ExecutionOptions{
			Tty: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for execution to start")
	}

	if err := svc.WriteExecutionStdin(sandboxID, executionID, []byte("hello\n")); err != nil {
		t.Fatalf("WriteExecutionStdin returned error: %v", err)
	}
	if err := svc.ResizeExecutionTTY(sandboxID, executionID, 120, 40); err != nil {
		t.Fatalf("ResizeExecutionTTY returned error: %v", err)
	}

	select {
	case got := <-stdinChunks:
		if got != "hello\n" {
			t.Fatalf("unexpected stdin payload: got %q want %q", got, "hello\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delayed stdin callback")
	}

	select {
	case got := <-resizes:
		if got != [2]uint32{120, 40} {
			t.Fatalf("unexpected resize payload: got %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delayed resize callback")
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

func TestExecutionAttachIOUnsupportedWhenBackendDoesNotExposeHandlers(t *testing.T) {
	started := make(chan struct{}, 1)
	adapter := &stubAdapter{
		runStreamFn: func(ctx context.Context, _ backend.RunRequest, _ backend.OutputStream) (*backend.RunResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

	createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"sh"},
		Options: &cleanroomv1.ExecutionOptions{
			Tty: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.GetExecution().GetExecutionId()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for execution to start")
	}

	if err := svc.WriteExecutionStdin(sandboxID, executionID, []byte("hello\n")); !errors.Is(err, ErrExecutionStdinUnsupported) {
		t.Fatalf("expected ErrExecutionStdinUnsupported, got %v", err)
	}
	if err := svc.ResizeExecutionTTY(sandboxID, executionID, 80, 24); !errors.Is(err, ErrExecutionResizeUnsupported) {
		t.Fatalf("expected ErrExecutionResizeUnsupported, got %v", err)
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

func TestTerminateRetainsStoppedSandboxState(t *testing.T) {
	svc := newTestService(&stubAdapter{})

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	terminateResp, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
	if !terminateResp.GetTerminated() {
		t.Fatal("expected terminated=true")
	}

	getResp, err := svc.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED; got != want {
		t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
	}
}

func TestRunExecutionSkipsAlreadyFinalExecution(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.GetSandbox().GetSandboxId()

	finished := time.Now().UTC()
	executionID := "exec-final"
	key := executionKey(sandboxID, executionID)

	svc.mu.Lock()
	sb := svc.sandboxes[sandboxID]
	sb.Status = cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED

	ex := &executionState{
		ID:               executionID,
		SandboxID:        sandboxID,
		Command:          []string{"echo", "stale"},
		Status:           cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED,
		ExitCode:         143,
		FinishedAt:       &finished,
		EventSubscribers: map[int]chan *cleanroomv1.ExecutionStreamEvent{},
		Done:             make(chan struct{}),
	}
	svc.recordExecutionEventLocked(ex, &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Status:      ex.Status,
		Payload: &cleanroomv1.ExecutionStreamEvent_Exit{Exit: &cleanroomv1.ExecutionExit{
			ExitCode: ex.ExitCode,
			Status:   ex.Status,
			Message:  "already canceled",
		}},
	})
	closeExecutionDoneLocked(ex)
	svc.executions[key] = ex
	initialEvents := len(ex.EventHistory)
	svc.mu.Unlock()

	svc.runExecution(sandboxID, executionID)

	svc.mu.RLock()
	gotEx := svc.executions[key]
	svc.mu.RUnlock()

	if gotEx == nil {
		t.Fatal("expected execution state to exist")
	}
	if got, want := len(gotEx.EventHistory), initialEvents; got != want {
		t.Fatalf("expected no additional events, got %d want %d", got, want)
	}
	if got, want := gotEx.Status, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED; got != want {
		t.Fatalf("unexpected status: got %v want %v", got, want)
	}
	if got, want := adapter.runCalls, 0; got != want {
		t.Fatalf("adapter should not run for finalized execution: got %d want %d", got, want)
	}
}

func TestStatePruningBoundsRetainedTerminalState(t *testing.T) {
	origSandboxes := maxRetainedStoppedSandboxes
	origExecutions := maxRetainedFinishedExecutions
	origAge := retainedStateMaxAge
	maxRetainedStoppedSandboxes = 1
	maxRetainedFinishedExecutions = 2
	retainedStateMaxAge = 24 * time.Hour
	defer func() {
		maxRetainedStoppedSandboxes = origSandboxes
		maxRetainedFinishedExecutions = origExecutions
		retainedStateMaxAge = origAge
	}()

	svc := newTestService(&stubAdapter{})

	runOnce := func() (string, string) {
		createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
		if err != nil {
			t.Fatalf("CreateSandbox returned error: %v", err)
		}
		sandboxID := createSandboxResp.GetSandbox().GetSandboxId()

		createExecutionResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
			SandboxId: sandboxID,
			Command:   []string{"echo", "ok"},
		})
		if err != nil {
			t.Fatalf("CreateExecution returned error: %v", err)
		}
		executionID := createExecutionResp.GetExecution().GetExecutionId()

		if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
			t.Fatalf("WaitExecution returned error: %v", err)
		}

		if _, err := svc.TerminateSandbox(context.Background(), &cleanroomv1.TerminateSandboxRequest{
			SandboxId: sandboxID,
		}); err != nil {
			t.Fatalf("TerminateSandbox returned error: %v", err)
		}
		return sandboxID, executionID
	}

	firstSandboxID, firstExecutionID := runOnce()
	_, _ = runOnce()
	lastSandboxID, lastExecutionID := runOnce()

	svc.mu.RLock()
	defer svc.mu.RUnlock()

	stoppedSandboxes := 0
	for _, sb := range svc.sandboxes {
		if sb.Status == cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED {
			stoppedSandboxes++
		}
	}
	if got, want := stoppedSandboxes, 1; got != want {
		t.Fatalf("unexpected retained stopped sandboxes: got %d want %d", got, want)
	}
	if got, want := len(svc.executions), 2; got != want {
		t.Fatalf("unexpected retained finished executions: got %d want %d", got, want)
	}
	if _, ok := svc.sandboxes[firstSandboxID]; ok {
		t.Fatalf("expected oldest stopped sandbox %q to be pruned", firstSandboxID)
	}
	if _, ok := svc.executions[executionKey(firstSandboxID, firstExecutionID)]; ok {
		t.Fatalf("expected oldest finished execution %q to be pruned", firstExecutionID)
	}
	if _, ok := svc.sandboxes[lastSandboxID]; !ok {
		t.Fatalf("expected newest stopped sandbox %q to be retained", lastSandboxID)
	}
	if _, ok := svc.executions[executionKey(lastSandboxID, lastExecutionID)]; !ok {
		t.Fatalf("expected newest finished execution %q to be retained", lastExecutionID)
	}
}

func TestStreamedOutputArrivesBeforeExecutionExit(t *testing.T) {
	adapter := &stubAdapter{
		runStreamFn: func(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("chunk-1\n"))
			}
			time.Sleep(50 * time.Millisecond)
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("chunk-2\n"))
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			return &backend.RunResult{
				RunID:      req.RunID,
				ExitCode:   0,
				LaunchedVM: false,
				PlanPath:   "/tmp/plan",
				RunDir:     "/tmp/run",
				Message:    "ok",
				Stdout:     "chunk-1\nchunk-2\n",
			}, nil
		},
	}
	svc := newTestService(adapter)

	sandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := sandboxResp.GetSandbox().GetSandboxId()

	execResp, err := svc.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "stream"},
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := execResp.GetExecution().GetExecutionId()

	_, updates, done, unsubscribe, err := svc.SubscribeExecutionEvents(sandboxID, executionID)
	if err != nil {
		t.Fatalf("SubscribeExecutionEvents returned error: %v", err)
	}
	defer unsubscribe()

	sawStdoutBeforeDone := false
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for !sawStdoutBeforeDone {
		select {
		case event, ok := <-updates:
			if !ok {
				t.Fatal("stream closed before stdout event")
			}
			if _, ok := event.Payload.(*cleanroomv1.ExecutionStreamEvent_Stdout); ok {
				sawStdoutBeforeDone = true
			}
		case <-done:
			t.Fatal("execution finished before any streamed stdout event")
		case <-timeout.C:
			t.Fatal("timed out waiting for streamed stdout event")
		}
	}
}

func collectExecutionEvents(t *testing.T, history []*cleanroomv1.ExecutionStreamEvent, updates <-chan *cleanroomv1.ExecutionStreamEvent, done <-chan struct{}) []*cleanroomv1.ExecutionStreamEvent {
	t.Helper()
	events := append([]*cleanroomv1.ExecutionStreamEvent(nil), history...)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()

	for {
		select {
		case event, ok := <-updates:
			if ok {
				events = append(events, event)
			}
		case <-done:
			for {
				select {
				case event, ok := <-updates:
					if !ok {
						return events
					}
					events = append(events, event)
				default:
					return events
				}
			}
		case <-timer.C:
			t.Fatalf("timed out collecting execution events")
		}
	}
}
