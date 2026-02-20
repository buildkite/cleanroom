package controlservice

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlapi"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
)

type stubAdapter struct {
	result      *backend.RunResult
	runFn       func(context.Context, backend.RunRequest) (*backend.RunResult, error)
	runStreamFn func(context.Context, backend.RunRequest, backend.OutputStream) (*backend.RunResult, error)
	req         backend.RunRequest
	runCalls    int
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

type stubLoader struct {
	compiled *policy.CompiledPolicy
	source   string
}

func (l stubLoader) LoadAndCompile(_ string) (*policy.CompiledPolicy, string, error) {
	return l.compiled, l.source, nil
}

func newTestService(adapter backend.Adapter) *Service {
	return &Service{
		Loader: stubLoader{
			compiled: &policy.CompiledPolicy{Hash: "hash-1", NetworkDefault: "deny"},
			source:   "/repo/cleanroom.yaml",
		},
		Backends: map[string]backend.Adapter{"firecracker": adapter},
	}
}

func TestExecForwardsBackendOutput(t *testing.T) {
	adapter := &stubAdapter{result: &backend.RunResult{
		RunID:      "run-1",
		ExitCode:   0,
		LaunchedVM: true,
		PlanPath:   "/tmp/plan",
		RunDir:     "/tmp/run",
		Message:    "ok",
		Stdout:     "hello stdout\n",
		Stderr:     "hello stderr\n",
	}}

	svc := &Service{
		Loader: stubLoader{
			compiled: &policy.CompiledPolicy{Hash: "hash-1", NetworkDefault: "deny"},
			source:   "/repo/cleanroom.yaml",
		},
		Backends: map[string]backend.Adapter{"firecracker": adapter},
	}

	resp, err := svc.Exec(context.Background(), controlapi.ExecRequest{
		CWD:     "/repo",
		Command: []string{"--", "echo", "hi"},
	})
	if err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}

	if adapter.req.Command[0] != "echo" {
		t.Fatalf("expected normalized command to start with echo, got %q", adapter.req.Command)
	}
	if resp.Stdout != "hello stdout\n" {
		t.Fatalf("expected stdout to be forwarded, got %q", resp.Stdout)
	}
	if resp.Stderr != "hello stderr\n" {
		t.Fatalf("expected stderr to be forwarded, got %q", resp.Stderr)
	}
}

func TestLaunchRunTerminateLifecycle(t *testing.T) {
	adapter := &stubAdapter{}
	svc := newTestService(adapter)

	launchResp, err := svc.LaunchCleanroom(context.Background(), controlapi.LaunchCleanroomRequest{
		CWD: "/repo",
		Options: controlapi.LaunchCleanroomOptions{
			RunDir: "/tmp/cleanrooms",
		},
	})
	if err != nil {
		t.Fatalf("LaunchCleanroom returned error: %v", err)
	}
	if launchResp.CleanroomID == "" {
		t.Fatal("expected cleanroom_id to be set")
	}
	if launchResp.PolicyHash != "hash-1" {
		t.Fatalf("unexpected policy hash: %q", launchResp.PolicyHash)
	}

	runResp, err := svc.RunCleanroom(context.Background(), controlapi.RunCleanroomRequest{
		CleanroomID: launchResp.CleanroomID,
		Command:     []string{"--", "pi", "run", "fix tests"},
	})
	if err != nil {
		t.Fatalf("RunCleanroom returned error: %v", err)
	}
	if runResp.CleanroomID != launchResp.CleanroomID {
		t.Fatalf("unexpected cleanroom id: got %q want %q", runResp.CleanroomID, launchResp.CleanroomID)
	}
	if got := adapter.req.Command[0]; got != "pi" {
		t.Fatalf("expected normalized command to start with pi, got %q", got)
	}
	if !strings.HasPrefix(adapter.req.RunDir, "/tmp/cleanrooms/") {
		t.Fatalf("expected run dir under run root, got %q", adapter.req.RunDir)
	}
	if adapter.runCalls != 1 {
		t.Fatalf("expected exactly one run call, got %d", adapter.runCalls)
	}

	terminateResp, err := svc.TerminateCleanroom(context.Background(), controlapi.TerminateCleanroomRequest{
		CleanroomID: launchResp.CleanroomID,
	})
	if err != nil {
		t.Fatalf("TerminateCleanroom returned error: %v", err)
	}
	if !terminateResp.Terminated {
		t.Fatal("expected terminated=true")
	}

	_, err = svc.RunCleanroom(context.Background(), controlapi.RunCleanroomRequest{
		CleanroomID: launchResp.CleanroomID,
		Command:     []string{"echo", "should fail"},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown cleanroom") {
		t.Fatalf("expected unknown cleanroom error after terminate, got %v", err)
	}
}

func TestRunCleanroomCancellationCancelsRunningExecution(t *testing.T) {
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	adapter := &stubAdapter{
		runFn: func(ctx context.Context, _ backend.RunRequest) (*backend.RunResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			select {
			case canceled <- struct{}{}:
			default:
			}
			return nil, ctx.Err()
		},
	}
	svc := newTestService(adapter)

	launchResp, err := svc.LaunchCleanroom(context.Background(), controlapi.LaunchCleanroomRequest{
		CWD: "/repo",
	})
	if err != nil {
		t.Fatalf("LaunchCleanroom returned error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = svc.TerminateCleanroom(context.Background(), controlapi.TerminateCleanroomRequest{
			CleanroomID: launchResp.CleanroomID,
		})
	})

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		_, err := svc.RunCleanroom(ctx, controlapi.RunCleanroomRequest{
			CleanroomID: launchResp.CleanroomID,
			Command:     []string{"sleep", "300"},
		})
		runErr <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for backend execution start")
	}

	cancel()

	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context canceled error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RunCleanroom to return after cancellation")
	}

	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for running execution to be canceled")
	}
}

func TestExecutionStreamIncludesExitEvent(t *testing.T) {
	adapter := &stubAdapter{
		runFn: func(_ context.Context, req backend.RunRequest) (*backend.RunResult, error) {
			return &backend.RunResult{
				RunID:      req.RunID,
				ExitCode:   7,
				LaunchedVM: true,
				PlanPath:   "/tmp/plan",
				RunDir:     "/tmp/run",
				Message:    "done",
				Stdout:     "hello stdout\n",
				Stderr:     "hello stderr\n",
			}, nil
		},
	}
	svc := newTestService(adapter)

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Cwd: "/repo"})
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

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Cwd: "/repo"})
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

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Cwd: "/repo"})
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

	createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Cwd: "/repo"})
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

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Cwd: "/repo",
	})
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

	createResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Cwd: "/repo",
	})
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
		CWD:              "/repo",
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
		createSandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
			Cwd: "/repo",
		})
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

	sandboxResp, err := svc.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Cwd: "/repo"})
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
