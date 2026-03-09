package interactivequic

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/controlservice"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

type integrationInteractiveService struct {
	expectedSessionID string
	expectedToken     string
	session           *controlservice.InteractiveSession
	history           []*cleanroomv1.ExecutionStreamEvent
	updates           chan *cleanroomv1.ExecutionStreamEvent
	done              chan struct{}

	mu       sync.Mutex
	released []string

	stdinCh   chan string
	resizeCh  chan testResizeCall
	cancelCh  chan int32
	releaseCh chan string
}

func newIntegrationInteractiveService() *integrationInteractiveService {
	return &integrationInteractiveService{
		expectedSessionID: "sess-123",
		expectedToken:     "token-456",
		session: &controlservice.InteractiveSession{
			SessionID:   "sess-123",
			SandboxID:   "sandbox-123",
			ExecutionID: "exec-123",
			InitialCols: 100,
			InitialRows: 40,
		},
		history: []*cleanroomv1.ExecutionStreamEvent{
			{
				SandboxId:   "sandbox-123",
				ExecutionId: "exec-123",
				Status:      cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
				Payload: &cleanroomv1.ExecutionStreamEvent_Stdout{
					Stdout: []byte("history-stdout\n"),
				},
			},
		},
		updates:   make(chan *cleanroomv1.ExecutionStreamEvent, 4),
		done:      make(chan struct{}),
		stdinCh:   make(chan string, 4),
		resizeCh:  make(chan testResizeCall, 4),
		cancelCh:  make(chan int32, 4),
		releaseCh: make(chan string, 4),
	}
}

func (s *integrationInteractiveService) ConsumeInteractiveSession(sessionID, token string) (*controlservice.InteractiveSession, error) {
	if sessionID != s.expectedSessionID {
		return nil, fmt.Errorf("unexpected session id %q", sessionID)
	}
	if token != s.expectedToken {
		return nil, fmt.Errorf("unexpected session token %q", token)
	}
	return s.session, nil
}

func (s *integrationInteractiveService) ReleaseInteractiveExecution(sandboxID, executionID string) {
	key := sandboxID + "/" + executionID
	s.mu.Lock()
	s.released = append(s.released, key)
	s.mu.Unlock()
	select {
	case s.releaseCh <- key:
	default:
	}
}

func (s *integrationInteractiveService) WriteExecutionStdin(sandboxID, executionID string, data []byte) error {
	if sandboxID != s.session.SandboxID || executionID != s.session.ExecutionID {
		return fmt.Errorf("unexpected stdin target %q/%q", sandboxID, executionID)
	}
	select {
	case s.stdinCh <- string(data):
	default:
	}
	return nil
}

func (s *integrationInteractiveService) ResizeExecutionTTY(sandboxID, executionID string, cols, rows uint32) error {
	call := testResizeCall{
		sandboxID:   sandboxID,
		executionID: executionID,
		cols:        cols,
		rows:        rows,
	}
	select {
	case s.resizeCh <- call:
	default:
	}
	return nil
}

func (s *integrationInteractiveService) CancelExecution(_ context.Context, req *cleanroomv1.CancelExecutionRequest) (*cleanroomv1.CancelExecutionResponse, error) {
	if req.GetSandboxId() != s.session.SandboxID || req.GetExecutionId() != s.session.ExecutionID {
		return nil, fmt.Errorf("unexpected cancel target %q/%q", req.GetSandboxId(), req.GetExecutionId())
	}
	select {
	case s.cancelCh <- req.GetSignal():
	default:
	}
	return &cleanroomv1.CancelExecutionResponse{
		SandboxId:   req.GetSandboxId(),
		ExecutionId: req.GetExecutionId(),
		Accepted:    true,
		Status:      cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
	}, nil
}

func (s *integrationInteractiveService) SubscribeExecutionEvents(sandboxID, executionID string) ([]*cleanroomv1.ExecutionStreamEvent, <-chan *cleanroomv1.ExecutionStreamEvent, <-chan struct{}, func(), error) {
	if sandboxID != s.session.SandboxID || executionID != s.session.ExecutionID {
		return nil, nil, nil, nil, fmt.Errorf("unexpected subscribe target %q/%q", sandboxID, executionID)
	}
	return s.history, s.updates, s.done, func() {}, nil
}

func TestDialRoundTrip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	service := newIntegrationInteractiveService()
	server, err := Start(ctx, "127.0.0.1:0", service, nil)
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	defer func() {
		_ = server.Close()
	}()

	session, err := Dial(
		context.Background(),
		server.Addr().String(),
		server.ALPN(),
		"SHA256:"+strings.ToUpper(server.CertPinSHA256()),
		service.expectedSessionID,
		service.expectedToken,
	)
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}
	defer session.Close()

	initialResize := waitForResizeCall(t, service.resizeCh)
	if got, want := initialResize.sandboxID, service.session.SandboxID; got != want {
		t.Fatalf("initial resize sandbox mismatch: got %q want %q", got, want)
	}
	if got, want := initialResize.executionID, service.session.ExecutionID; got != want {
		t.Fatalf("initial resize execution mismatch: got %q want %q", got, want)
	}
	if got, want := initialResize.cols, uint32(100); got != want {
		t.Fatalf("initial resize cols mismatch: got %d want %d", got, want)
	}
	if got, want := initialResize.rows, uint32(40); got != want {
		t.Fatalf("initial resize rows mismatch: got %d want %d", got, want)
	}

	historyChunk := readPTYChunk(t, session)
	if got, want := historyChunk, "history-stdout\n"; got != want {
		t.Fatalf("unexpected history PTY chunk: got %q want %q", got, want)
	}

	if err := session.WriteStdin([]byte("stdin-data")); err != nil {
		t.Fatalf("WriteStdin returned error: %v", err)
	}
	if got, want := waitForString(t, service.stdinCh), "stdin-data"; got != want {
		t.Fatalf("unexpected stdin payload: got %q want %q", got, want)
	}

	if err := session.SendResize(120, 50); err != nil {
		t.Fatalf("SendResize returned error: %v", err)
	}
	resize := waitForResizeCall(t, service.resizeCh)
	if got, want := resize.cols, uint32(120); got != want {
		t.Fatalf("resize cols mismatch: got %d want %d", got, want)
	}
	if got, want := resize.rows, uint32(50); got != want {
		t.Fatalf("resize rows mismatch: got %d want %d", got, want)
	}

	service.updates <- &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   service.session.SandboxID,
		ExecutionId: service.session.ExecutionID,
		Status:      cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
		Payload: &cleanroomv1.ExecutionStreamEvent_Stderr{
			Stderr: []byte("update-stderr\n"),
		},
	}
	if got, want := readPTYChunk(t, session), "update-stderr\n"; got != want {
		t.Fatalf("unexpected update PTY chunk: got %q want %q", got, want)
	}

	if err := session.SendSignal(15); err != nil {
		t.Fatalf("SendSignal returned error: %v", err)
	}
	if got, want := waitForSignal(t, service.cancelCh), int32(15); got != want {
		t.Fatalf("unexpected cancel signal: got %d want %d", got, want)
	}

	service.updates <- &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   service.session.SandboxID,
		ExecutionId: service.session.ExecutionID,
		Status:      cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED,
		Payload: &cleanroomv1.ExecutionStreamEvent_Exit{
			Exit: &cleanroomv1.ExecutionExit{
				ExitCode: 7,
				Status:   cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED,
				Message:  "boom",
			},
		},
	}
	close(service.done)
	close(service.updates)

	msg := waitForControlMessage(t, session.Events())
	if got, want := msg.Type, controlTypeExit; got != want {
		t.Fatalf("unexpected control message type: got %q want %q", got, want)
	}
	if got, want := msg.ExitCode, int32(7); got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
	if got, want := msg.Status, cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED.String(); got != want {
		t.Fatalf("unexpected exit status: got %q want %q", got, want)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if got, want := waitForString(t, service.releaseCh), "sandbox-123/exec-123"; got != want {
		t.Fatalf("unexpected release target: got %q want %q", got, want)
	}
}

func TestDialRejectsWrongCertPin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	service := newIntegrationInteractiveService()
	server, err := Start(ctx, "127.0.0.1:0", service, nil)
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	defer func() {
		_ = server.Close()
	}()

	_, err = Dial(
		context.Background(),
		server.Addr().String(),
		server.ALPN(),
		"sha256:0000000000000000000000000000000000000000000000000000000000000000",
		service.expectedSessionID,
		service.expectedToken,
	)
	if err == nil {
		t.Fatal("expected cert pin validation error")
	}
	if !strings.Contains(err.Error(), "interactive cert pin mismatch") {
		t.Fatalf("unexpected cert pin error: %v", err)
	}
}

func readPTYChunk(t *testing.T, session *Session) string {
	t.Helper()

	type result struct {
		data string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 1024)
		n, err := session.ReadPTY(buf)
		ch <- result{data: string(buf[:n]), err: err}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("ReadPTY returned error: %v", res.err)
		}
		return res.data
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for PTY chunk")
		return ""
	}
}

func waitForResizeCall(t *testing.T, ch <-chan testResizeCall) testResizeCall {
	t.Helper()

	select {
	case call := <-ch:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for resize call")
		return testResizeCall{}
	}
}

func waitForString(t *testing.T, ch <-chan string) string {
	t.Helper()

	select {
	case value := <-ch:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for string value")
		return ""
	}
}

func waitForSignal(t *testing.T, ch <-chan int32) int32 {
	t.Helper()

	select {
	case signal := <-ch:
		return signal
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for signal")
		return 0
	}
}

func waitForControlMessage(t *testing.T, ch <-chan controlMessage) controlMessage {
	t.Helper()

	select {
	case msg, ok := <-ch:
		if !ok {
			t.Fatal("control message channel closed before exit event")
		}
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for control message")
		return controlMessage{}
	}
}

func TestSessionCloseIsIdempotentAfterRemoteEOF(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	service := newIntegrationInteractiveService()
	server, err := Start(ctx, "127.0.0.1:0", service, nil)
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	defer func() {
		_ = server.Close()
	}()

	session, err := Dial(context.Background(), server.Addr().String(), server.ALPN(), server.CertPinSHA256(), service.expectedSessionID, service.expectedToken)
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}

	service.updates <- &cleanroomv1.ExecutionStreamEvent{
		SandboxId:   service.session.SandboxID,
		ExecutionId: service.session.ExecutionID,
		Status:      cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED,
		Payload: &cleanroomv1.ExecutionStreamEvent_Exit{
			Exit: &cleanroomv1.ExecutionExit{
				ExitCode: 0,
				Status:   cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED,
			},
		},
	}
	close(service.done)
	close(service.updates)

	msg := waitForControlMessage(t, session.Events())
	if got, want := msg.Type, controlTypeExit; got != want {
		t.Fatalf("unexpected control message type: got %q want %q", got, want)
	}

	if err := session.Close(); err != nil && err != io.EOF {
		t.Fatalf("first Close returned error: %v", err)
	}
	if err := session.Close(); err != nil && err != io.EOF {
		t.Fatalf("second Close returned error: %v", err)
	}
}
