package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/controlclient"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/charmbracelet/log"
	"github.com/quic-go/quic-go"
)

var (
	newSignalChannel = func() chan os.Signal {
		return make(chan os.Signal, 2)
	}
	notifySignals = func(ch chan os.Signal, sig ...os.Signal) {
		signal.Notify(ch, sig...)
	}
	stopSignals = func(ch chan os.Signal) {
		signal.Stop(ch)
	}
)

func ensureSandboxID(client *controlclient.Client, loader policyLoader, cwd, host, backendName, existingSandboxID, imageRefOverride string, launchSeconds int64, repository *resolvedRepositoryCheckout) (string, error) {
	sandboxID := strings.TrimSpace(existingSandboxID)
	if sandboxID != "" {
		if strings.TrimSpace(imageRefOverride) != "" {
			return "", errors.New("--image cannot be used with --sandbox-id")
		}
		return sandboxID, nil
	}

	sandboxID, _, err := createTopLevelSandbox(client, loader, cwd, host, backendName, imageRefOverride, launchSeconds, repository)
	if err != nil {
		return "", err
	}
	return sandboxID, nil
}

func getFinalExecutionExitCode(client *controlclient.Client, sandboxID, executionID string) (int, bool) {
	getResp, err := client.GetExecution(context.Background(), &cleanroomv1.GetExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
	})
	if err != nil || getResp.GetExecution() == nil {
		return 0, false
	}
	execution := getResp.GetExecution()
	if !isFinalExecutionStatus(execution.GetStatus()) {
		return 0, false
	}
	return int(execution.GetExitCode()), true
}

func replayExecutionHistory(client *controlclient.Client, sandboxID, executionID string, stdout, stderr io.Writer) (int, bool, error) {
	stream, err := client.StreamExecution(context.Background(), &cleanroomv1.StreamExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Follow:      false,
	})
	if err != nil {
		return 0, false, err
	}

	exitCode := 0
	haveExitCode := false
	for stream.Receive() {
		event := stream.Msg()
		switch payload := event.Payload.(type) {
		case *cleanroomv1.ExecutionStreamEvent_Stdout:
			if _, err := stdout.Write(payload.Stdout); err != nil {
				return 0, false, err
			}
		case *cleanroomv1.ExecutionStreamEvent_Stderr:
			if _, err := stderr.Write(payload.Stderr); err != nil {
				return 0, false, err
			}
		case *cleanroomv1.ExecutionStreamEvent_Exit:
			exitCode = int(payload.Exit.GetExitCode())
			haveExitCode = true
		}
	}
	if err := stream.Err(); err != nil {
		return 0, false, err
	}
	return exitCode, haveExitCode, nil
}

func terminateSandboxBestEffort(client *controlclient.Client, sandboxID string, timeout time.Duration, logger *log.Logger, warnMessage string) {
	if client == nil || strings.TrimSpace(sandboxID) == "" {
		return
	}

	terminateCtx := context.Background()
	var terminateCancel context.CancelFunc
	if timeout > 0 {
		terminateCtx, terminateCancel = context.WithTimeout(context.Background(), timeout)
		defer terminateCancel()
	}

	_, err := client.TerminateSandbox(terminateCtx, &cleanroomv1.TerminateSandboxRequest{SandboxId: sandboxID})
	if err != nil && logger != nil {
		logger.Warn(warnMessage, "sandbox_id", sandboxID, "error", err)
	}
}

func isCanceledStreamErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) && connectErr.Code() == connect.CodeCanceled {
		return true
	}
	return false
}

func isExecutionNoLongerActiveErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "no longer active")
}

func isInteractiveStreamClosedErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var appErr *quic.ApplicationError
	if errors.As(err, &appErr) && appErr.ErrorCode == 0 {
		return true
	}
	return false
}

func isFinalExecutionStatus(status cleanroomv1.ExecutionStatus) bool {
	switch status {
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED,
		cleanroomv1.ExecutionStatus_EXECUTION_STATUS_TIMED_OUT:
		return true
	default:
		return false
	}
}

func resolveConsoleDialFailure(
	dialErr error,
	replayFn func() (int, bool, error),
	getFinalFn func() (int, bool),
) error {
	haveExitCode := false
	exitCode := 0
	if replayFn != nil {
		replayedExitCode, replayedHaveExitCode, replayErr := replayFn()
		if replayErr == nil {
			haveExitCode = replayedHaveExitCode
			exitCode = replayedExitCode
		}
	}

	if !haveExitCode && getFinalFn != nil {
		if fetchedExitCode, ok := getFinalFn(); ok {
			haveExitCode = true
			exitCode = fetchedExitCode
		}
	}

	if haveExitCode {
		if exitCode != 0 {
			return exitCodeError{code: exitCode}
		}
		return nil
	}
	return fmt.Errorf("dial interactive execution: %w", dialErr)
}
