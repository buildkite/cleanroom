package interactivequic

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/buildkite/cleanroom/internal/controlservice"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/quic-go/quic-go"
)

type testResizeCall struct {
	sandboxID   string
	executionID string
	cols        uint32
	rows        uint32
}

type testInteractiveService struct {
	resizeErr   error
	resizeCalls []testResizeCall
}

func (s *testInteractiveService) ConsumeInteractiveSession(sessionID, token string) (*controlservice.InteractiveSession, error) {
	return nil, errors.New("not implemented")
}

func (s *testInteractiveService) WriteExecutionStdin(sandboxID, executionID string, data []byte) error {
	return nil
}

func (s *testInteractiveService) ResizeExecutionTTY(sandboxID, executionID string, cols, rows uint32) error {
	s.resizeCalls = append(s.resizeCalls, testResizeCall{
		sandboxID:   sandboxID,
		executionID: executionID,
		cols:        cols,
		rows:        rows,
	})
	return s.resizeErr
}

func (s *testInteractiveService) CancelExecution(ctx context.Context, req *cleanroomv1.CancelExecutionRequest) (*cleanroomv1.CancelExecutionResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *testInteractiveService) SubscribeExecutionEvents(sandboxID, executionID string) ([]*cleanroomv1.ExecutionStreamEvent, <-chan *cleanroomv1.ExecutionStreamEvent, <-chan struct{}, func(), error) {
	return nil, nil, nil, func() {}, errors.New("not implemented")
}

func TestApplyInitialTTYSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		initialCols      uint32
		initialRows      uint32
		resizeErr        error
		wantErr          bool
		wantResizeCalled bool
	}{
		{
			name:             "applies non-zero initial size",
			initialCols:      160,
			initialRows:      48,
			wantResizeCalled: true,
		},
		{
			name:             "skips when cols are zero",
			initialCols:      0,
			initialRows:      48,
			wantResizeCalled: false,
		},
		{
			name:             "skips when rows are zero",
			initialCols:      160,
			initialRows:      0,
			wantResizeCalled: false,
		},
		{
			name:             "ignores unsupported resize errors",
			initialCols:      160,
			initialRows:      48,
			resizeErr:        controlservice.ErrExecutionResizeUnsupported,
			wantResizeCalled: true,
		},
		{
			name:             "returns non-ignorable resize errors",
			initialCols:      160,
			initialRows:      48,
			resizeErr:        errors.New("backend resize failed"),
			wantErr:          true,
			wantResizeCalled: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc := &testInteractiveService{resizeErr: tc.resizeErr}
			server := &Server{service: svc}
			session := &controlservice.InteractiveSession{
				SessionID:   "sess-123",
				SandboxID:   "sbx-123",
				ExecutionID: "exec-123",
				InitialCols: tc.initialCols,
				InitialRows: tc.initialRows,
			}

			err := server.applyInitialTTYSize(session)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tc.wantResizeCalled && len(svc.resizeCalls) != 1 {
				t.Fatalf("expected one resize call, got %d", len(svc.resizeCalls))
			}
			if !tc.wantResizeCalled && len(svc.resizeCalls) != 0 {
				t.Fatalf("expected no resize calls, got %d", len(svc.resizeCalls))
			}
			if len(svc.resizeCalls) == 1 {
				call := svc.resizeCalls[0]
				if call.sandboxID != "sbx-123" || call.executionID != "exec-123" {
					t.Fatalf("unexpected resize target: sandbox=%q execution=%q", call.sandboxID, call.executionID)
				}
				if call.cols != tc.initialCols || call.rows != tc.initialRows {
					t.Fatalf("unexpected resize dimensions: got %dx%d want %dx%d", call.cols, call.rows, tc.initialCols, tc.initialRows)
				}
			}
		})
	}
}

func TestShouldFailInteractiveOnStdinErr(t *testing.T) {
	t.Parallel()

	if shouldFailInteractiveOnStdinErr(nil) {
		t.Fatal("expected nil stdin error not to fail session")
	}
	if shouldFailInteractiveOnStdinErr(io.EOF) {
		t.Fatal("expected io.EOF stdin error not to fail session")
	}
	if !shouldFailInteractiveOnStdinErr(errors.New("stdin write failed")) {
		t.Fatal("expected non-EOF stdin error to fail session")
	}
}

func TestIsInteractiveAcceptClosedErr(t *testing.T) {
	t.Parallel()

	if isInteractiveAcceptClosedErr(nil) {
		t.Fatal("expected nil not to be treated as closed-listener accept error")
	}
	if !isInteractiveAcceptClosedErr(context.Canceled) {
		t.Fatal("expected context.Canceled to be treated as closed-listener accept error")
	}
	if !isInteractiveAcceptClosedErr(quic.ErrServerClosed) {
		t.Fatal("expected quic.ErrServerClosed to be treated as closed-listener accept error")
	}
	if !isInteractiveAcceptClosedErr(net.ErrClosed) {
		t.Fatal("expected net.ErrClosed to be treated as closed-listener accept error")
	}
	if isInteractiveAcceptClosedErr(errors.New("accept failed")) {
		t.Fatal("expected generic accept error not to be treated as closed-listener accept error")
	}
}
