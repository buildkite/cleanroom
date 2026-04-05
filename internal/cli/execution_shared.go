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

func ensureSandboxID(client *controlclient.Client, loader policyLoader, cwd, host, backendName, existingSandboxID, fromSnapshot, imageRefOverride string, launchSeconds int64, repository *resolvedRepositoryCheckout) (string, bool, error) {
	sandboxID := strings.TrimSpace(existingSandboxID)
	fromSnapshot = strings.TrimSpace(fromSnapshot)
	if sandboxID != "" {
		if fromSnapshot != "" {
			return "", false, errors.New("--from cannot be used with --in")
		}
		if strings.TrimSpace(imageRefOverride) != "" {
			return "", false, errors.New("--image cannot be used with --in")
		}
		return sandboxID, false, nil
	}
	if fromSnapshot != "" {
		if repository != nil {
			return "", false, errors.New("repository bootstrap cannot be used with --from")
		}
		if strings.TrimSpace(imageRefOverride) != "" {
			return "", false, errors.New("--image cannot be used with --from")
		}
		if strings.TrimSpace(backendName) != "" {
			return "", false, errors.New("--backend cannot be used with --from")
		}
		createSandboxResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
			Options: &cleanroomv1.SandboxOptions{
				LaunchSeconds: launchSeconds,
			},
			Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: fromSnapshot},
		})
		if err != nil {
			return "", false, fmt.Errorf("create sandbox: %w", err)
		}
		sandbox := createSandboxResp.GetSandbox()
		sandboxID := strings.TrimSpace(sandbox.GetSandboxId())
		if sandboxID == "" {
			return "", false, errors.New("create sandbox: response missing sandbox id")
		}
		return sandboxID, true, nil
	}

	sandboxID, _, err := createTopLevelSandbox(client, loader, cwd, host, backendName, imageRefOverride, launchSeconds, repository)
	if err != nil {
		return "", false, err
	}
	return sandboxID, true, nil
}

func validateExecutionSandboxArgs(chdir, existingSandboxID, fromSnapshot string, keep bool) error {
	sandboxID := strings.TrimSpace(existingSandboxID)
	snapshotID := strings.TrimSpace(fromSnapshot)
	hasChdir := strings.TrimSpace(chdir) != ""

	if sandboxID != "" && snapshotID != "" {
		return errors.New("--from cannot be used with --in")
	}
	if sandboxID != "" && keep {
		return errors.New("--keep cannot be used with --in")
	}
	if sandboxID != "" && hasChdir {
		return errors.New("--chdir cannot be used with --in")
	}
	if snapshotID != "" && hasChdir {
		return errors.New("--chdir cannot be used with --from")
	}

	return nil
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

func startExecutionStdinForwarder(
	client *controlclient.Client,
	sandboxID, executionID string,
	closeImmediately bool,
	cancel context.CancelFunc,
) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		if client == nil || strings.TrimSpace(sandboxID) == "" || strings.TrimSpace(executionID) == "" {
			return
		}

		reportErr := func(err error) {
			if err == nil {
				return
			}
			errCh <- err
			if cancel != nil {
				cancel()
			}
		}

		if closeImmediately {
			if err := closeExecutionStdin(client, sandboxID, executionID); err != nil {
				reportErr(err)
			}
			return
		}

		buf := make([]byte, 4096)
		sentInput := false
		for {
			n, readErr := os.Stdin.Read(buf)
			if n > 0 {
				sentInput = true
				if _, err := client.WriteExecutionStdin(context.Background(), &cleanroomv1.WriteExecutionStdinRequest{
					SandboxId:   sandboxID,
					ExecutionId: executionID,
					Data:        append([]byte(nil), buf[:n]...),
				}); err != nil {
					if isExecutionStdinUnsupportedErr(err) && !sentInput {
						return
					}
					if isBenignExecutionStdinErr(err) {
						return
					}
					reportErr(fmt.Errorf("write execution stdin: %w", err))
					return
				}
			}
			if readErr != nil {
				if err := closeExecutionStdin(client, sandboxID, executionID); err != nil &&
					!(isExecutionStdinUnsupportedErr(err) && !sentInput) {
					reportErr(err)
				}
				return
			}
		}
	}()
	return errCh
}

func resolveExecutionEnv(specs []string) ([]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		if strings.Contains(spec, "\x00") {
			return nil, fmt.Errorf("invalid --env %q: contains NUL", spec)
		}
		if key, _, ok := strings.Cut(spec, "="); ok {
			if key == "" {
				return nil, fmt.Errorf("invalid --env %q: missing variable name", spec)
			}
			out = append(out, spec)
			continue
		}

		if spec == "" {
			return nil, errors.New("invalid --env \"\": missing variable name")
		}
		value, ok := os.LookupEnv(spec)
		if !ok {
			return nil, fmt.Errorf("environment variable %q is not set", spec)
		}
		out = append(out, spec+"="+value)
	}
	return out, nil
}

func closeExecutionStdin(client *controlclient.Client, sandboxID, executionID string) error {
	_, err := client.CloseExecutionStdin(context.Background(), &cleanroomv1.CloseExecutionStdinRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
	})
	if isBenignExecutionStdinErr(err) || isClosedNetworkConnectionErr(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("close execution stdin: %w", err)
	}
	return nil
}

func pollExecutionStdinErr(errCh <-chan error) error {
	if errCh == nil {
		return nil
	}
	select {
	case err, ok := <-errCh:
		if !ok {
			return nil
		}
		return err
	default:
		return nil
	}
}

func waitExecutionStdinErr(errCh <-chan error, wait time.Duration) error {
	if err := pollExecutionStdinErr(errCh); err != nil || errCh == nil || wait <= 0 {
		return err
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case err, ok := <-errCh:
		if !ok {
			return nil
		}
		return err
	case <-timer.C:
		return nil
	}
}

func writeExecutionWarning(stderr io.Writer, message string) error {
	message = strings.TrimSpace(message)
	if stderr == nil || message == "" {
		return nil
	}
	palette := defaultTerminalPalette()
	_, err := io.WriteString(stderr, renderNoticeLine("warning", message, palette.warn, shouldUseANSI(stderr)))
	return err
}

func writeSandboxID(stderr io.Writer, sandboxID string) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if stderr == nil || sandboxID == "" {
		return nil
	}
	_, err := fmt.Fprintf(stderr, "sandbox_id=%s\n", sandboxID)
	return err
}

func writeExecutionID(stderr io.Writer, executionID string) error {
	executionID = strings.TrimSpace(executionID)
	if stderr == nil || executionID == "" {
		return nil
	}
	_, err := fmt.Fprintf(stderr, "execution_id=%s\n", executionID)
	return err
}

func writeArtifactsDir(stderr io.Writer, artifactsDir string) error {
	artifactsDir = strings.TrimSpace(artifactsDir)
	if stderr == nil || artifactsDir == "" {
		return nil
	}
	_, err := fmt.Fprintf(stderr, "artifacts_dir=%s\n", artifactsDir)
	return err
}

func writeExecutionInspectCommand(stderr io.Writer, sandboxID, executionID string) error {
	executionID = strings.TrimSpace(executionID)
	if stderr == nil || executionID == "" {
		return nil
	}
	_, err := fmt.Fprintf(stderr, "inspect_command=cleanroom execution inspect %s\n", executionID)
	return err
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
		case *cleanroomv1.ExecutionStreamEvent_Warning:
			if err := writeExecutionWarning(stderr, payload.Warning); err != nil {
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

func isExecutionNotRunningErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "execution is not running")
}

func isClosedNetworkConnectionErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "closed network connection")
}

func isExecutionStdinUnsupportedErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "stdin attach is not supported")
}

func isBenignExecutionStdinErr(err error) bool {
	return err == nil ||
		isCanceledStreamErr(err) ||
		isExecutionNoLongerActiveErr(err) ||
		isExecutionNotRunningErr(err)
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
