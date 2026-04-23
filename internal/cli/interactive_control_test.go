package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"context"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func TestPollInteractiveExitOrControlErrPrefersExitCode(t *testing.T) {
	t.Parallel()

	exitCodeCh := make(chan int, 1)
	exitCodeCh <- 17
	controlErrCh := make(chan error, 1)
	controlErrCh <- errors.New("control failed")

	gotExitCode, haveExitCode, err := pollInteractiveExitOrControlErr(exitCodeCh, &controlErrCh)
	if err != nil {
		t.Fatalf("expected no poll error, got %v", err)
	}
	if !haveExitCode {
		t.Fatal("expected exit code from poll")
	}
	if got, want := gotExitCode, 17; got != want {
		t.Fatalf("unexpected exit code from poll: got %d want %d", got, want)
	}
}

func TestPollInteractiveExitOrControlErrDisablesClosedControlChannel(t *testing.T) {
	t.Parallel()

	var exitCodeCh chan int
	controlErrCh := make(chan error)
	close(controlErrCh)

	gotExitCode, haveExitCode, err := pollInteractiveExitOrControlErr(exitCodeCh, &controlErrCh)
	if err != nil {
		t.Fatalf("expected no poll error, got %v", err)
	}
	if haveExitCode {
		t.Fatalf("unexpected exit code from poll: %d", gotExitCode)
	}
	if controlErrCh != nil {
		t.Fatal("expected poll to disable closed control error channel")
	}
}

func TestWaitForInteractiveExitOrControlErrPrefersExitWhenControlClosed(t *testing.T) {
	t.Parallel()

	exitCodeCh := make(chan int, 1)
	exitCodeCh <- 5
	controlErrCh := make(chan error)
	close(controlErrCh)

	gotExitCode, haveExitCode, err := waitForInteractiveExitOrControlErr(exitCodeCh, &controlErrCh, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("expected no wait error, got %v", err)
	}
	if !haveExitCode {
		t.Fatal("expected wait to return exit code")
	}
	if gotExitCode != 5 {
		t.Fatalf("unexpected exit code: got %d want 5", gotExitCode)
	}
}

func TestWaitForInteractiveExitOrControlErrReturnsControlError(t *testing.T) {
	t.Parallel()

	exitCodeCh := make(chan int)
	controlErrCh := make(chan error, 1)
	controlErrCh <- errors.New("control failed")

	_, haveExitCode, err := waitForInteractiveExitOrControlErr(exitCodeCh, &controlErrCh, 10*time.Millisecond)
	if haveExitCode {
		t.Fatal("expected no exit code when control error is returned")
	}
	if err == nil {
		t.Fatal("expected control error, got nil")
	}
	if got, want := err.Error(), "interactive control stream: control failed"; !strings.Contains(got, want) {
		t.Fatalf("unexpected control error message: got %q want substring %q", got, want)
	}
}

func TestWaitForInteractiveExitOrControlErrPrefersExitCodeWhenBothReady(t *testing.T) {
	t.Parallel()

	exitCodeCh := make(chan int, 1)
	exitCodeCh <- 9
	controlErrCh := make(chan error, 1)
	controlErrCh <- errors.New("control failed")

	gotExitCode, haveExitCode, err := waitForInteractiveExitOrControlErr(exitCodeCh, &controlErrCh, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("expected no wait error, got %v", err)
	}
	if !haveExitCode {
		t.Fatal("expected wait to return exit code")
	}
	if got, want := gotExitCode, 9; got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
}

func TestWaitForInteractiveExitOrControlErrIgnoresBenignControlErrorUntilExit(t *testing.T) {
	t.Parallel()

	exitCodeCh := make(chan int, 1)
	controlErrCh := make(chan error, 1)
	controlErrCh <- errors.New("execution is not running")

	go func() {
		time.Sleep(5 * time.Millisecond)
		exitCodeCh <- 3
	}()

	gotExitCode, haveExitCode, err := waitForInteractiveExitOrControlErr(exitCodeCh, &controlErrCh, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("expected no wait error, got %v", err)
	}
	if !haveExitCode {
		t.Fatal("expected wait to return exit code")
	}
	if got, want := gotExitCode, 3; got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}
}

func TestRequestInteractiveExecutionCancelAsyncReturnsImmediately(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	returned := make(chan struct{}, 1)
	t.Cleanup(func() {
		close(release)
	})

	go func() {
		requestInteractiveExecutionCancelAsync(context.Background(), nil, "sandbox-123", "exec-123", 2, func(context.Context, *cleanroomv1.CancelExecutionRequest) (*cleanroomv1.CancelExecutionResponse, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return &cleanroomv1.CancelExecutionResponse{Accepted: true}, nil
		})
		returned <- struct{}{}
	}()

	select {
	case <-returned:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for async cancel request dispatch")
	}

	select {
	case <-started:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for async cancel request to start")
	}
}
