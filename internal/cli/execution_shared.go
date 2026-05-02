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
	"go.opentelemetry.io/otel/trace"
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
	executionStdinErrDrainTimeout = 100 * time.Millisecond
)

type executionSandbox struct {
	SandboxID      string
	CreatedSandbox bool
	Repository     *resolvedRepositoryCheckout
	WorkspaceRoot  string
}

// resolveExecutionSandbox keeps `exec` and `console` on the same sandbox
// lifecycle shape: resolve repository context, then either reuse the requested
// sandbox or create one up front with repository bootstrap attached.
func resolveExecutionSandbox(
	callCtx context.Context,
	client *controlclient.Client,
	ctx *runtimeContext,
	cwd, host, backendName, existingSandboxID, fromSnapshot, imageRefOverride string,
	launchSeconds int64,
	dangerouslyAllowAll bool,
	repositoryOverride repositoryOverrideFlags,
	copyFlags workspaceCopyFlags,
) (*executionSandbox, error) {
	existingSandboxID = strings.TrimSpace(existingSandboxID)
	fromSnapshot = strings.TrimSpace(fromSnapshot)

	var repository *resolvedRepositoryCheckout
	localChanges := repositoryLocalChanges{}
	if fromSnapshot == "" && (existingSandboxID == "" || copyFlags.CopyIn) {
		var err error
		if copyFlags.CopyIn && existingSandboxID != "" && !repositoryOverride.hasRepositoryOverride() {
			repository, err = resolveWorkspaceCopyRepositoryCheckout(cwd, ctx.Loader)
		} else {
			repository, err = resolveRepositoryCheckoutWithOverride(cwd, ctx.Loader, repositoryOverride)
		}
		if err != nil {
			return nil, err
		}
		if err := validateTopLevelWorkspaceCopyTransport(repository, copyFlags.CopyIn && existingSandboxID == ""); err != nil {
			return nil, err
		}
		if existingSandboxID == "" {
			localChanges, err = resolveRepositoryLocalChanges(repository, copyFlags.CopyIn)
			if err != nil {
				return nil, err
			}
		}
	}
	warnDirtyRepositoryCheckout(repository, copyFlags.CopyIn && repository != nil && fromSnapshot == "")

	sandboxID, createdSandbox, err := ensureSandboxID(
		callCtx,
		client,
		ctx.Loader,
		cwd,
		host,
		backendName,
		existingSandboxID,
		fromSnapshot,
		imageRefOverride,
		launchSeconds,
		dangerouslyAllowAll,
		repository,
		localChanges,
	)
	if err != nil {
		return nil, err
	}

	workspaceRoot := ""
	if copyFlags.CopyIn && fromSnapshot == "" {
		localChangesAttachedDuringCreate := existingSandboxID == "" && repository != nil && (localChanges.Changeset != nil || localChanges.CommitBundle != nil)
		copyRepository := repository
		if repository == nil {
			workspaceRoot, err = resolveSandboxWorkspaceDestination(callCtx, client, sandboxID)
			if err != nil {
				return nil, err
			}
		} else if existingSandboxID != "" {
			effectiveRepository, _, err := resolveGitWorkspaceCheckout(callCtx, client, workspaceCopyOptions{
				SandboxID:  sandboxID,
				Repository: repository,
			})
			if err != nil {
				return nil, err
			}
			copyRepository = effectiveRepository
			workspaceRoot = effectiveRepository.DestinationDir
		}
		if !localChangesAttachedDuringCreate {
			if err := copyWorkspaceToSandbox(callCtx, ctx, client, workspaceCopyOptions{
				CWD:           cwd,
				SandboxID:     sandboxID,
				Repository:    copyRepository,
				Destination:   workspaceRoot,
				ForceGitReset: existingSandboxID != "",
				LaunchSeconds: launchSeconds,
			}); err != nil {
				if createdSandbox {
					terminateSandboxBestEffort(callCtx, client, sandboxID, 0, nil, "")
				}
				return nil, err
			}
			if existingSandboxID != "" && copyRepository != nil {
				repository = nil
			}
		} else if repository != nil {
			warnWorkspaceBindingError(ctx, recordGitWorkspaceBinding(sandboxID, repository, toRepositoryCheckout(repository), repositoryLocalChangesFiles(localChanges), "copy-in"))
		}
	}

	return &executionSandbox{
		SandboxID:      sandboxID,
		CreatedSandbox: createdSandbox,
		Repository:     repository,
		WorkspaceRoot:  workspaceRoot,
	}, nil
}

func ensureSandboxID(callCtx context.Context, client *controlclient.Client, loader policyLoader, cwd, host, backendName, existingSandboxID, fromSnapshot, imageRefOverride string, launchSeconds int64, dangerouslyAllowAll bool, repository *resolvedRepositoryCheckout, localChanges repositoryLocalChanges) (string, bool, error) {
	sandboxID := strings.TrimSpace(existingSandboxID)
	fromSnapshot = strings.TrimSpace(fromSnapshot)
	if sandboxID != "" {
		if fromSnapshot != "" {
			return "", false, errors.New("--from cannot be used with --in")
		}
		if strings.TrimSpace(imageRefOverride) != "" {
			return "", false, errors.New("--image cannot be used with --in")
		}
		if dangerouslyAllowAll {
			return "", false, errors.New("--dangerously-allow-all cannot be used with --in")
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
		if dangerouslyAllowAll {
			return "", false, errors.New("--dangerously-allow-all cannot be used with --from")
		}
		_, sandboxID, err := createSandboxWithProgress(callCtx, os.Stderr, client, &cleanroomv1.CreateSandboxRequest{
			Options: &cleanroomv1.SandboxOptions{
				LaunchSeconds: launchSeconds,
			},
			Source: &cleanroomv1.CreateSandboxRequest_SnapshotId{SnapshotId: fromSnapshot},
		})
		if err != nil {
			return "", false, fmt.Errorf("create sandbox: %w", err)
		}
		return sandboxID, true, nil
	}

	sandboxID, _, err := createTopLevelSandbox(callCtx, client, loader, cwd, host, backendName, imageRefOverride, launchSeconds, dangerouslyAllowAll, repository, localChanges)
	if err != nil {
		return "", false, err
	}
	return sandboxID, true, nil
}

func tracePreservingContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

func traceIDFromContext(ctx context.Context) string {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}

func executionCommandArgs(command []string) []string {
	args := make([]string, 0, len(command))
	for _, arg := range command {
		trimmed := strings.TrimSpace(arg)
		if trimmed == "" {
			continue
		}
		if len(args) == 0 && trimmed == "--" {
			continue
		}
		args = append(args, trimmed)
	}
	return args
}

func executionCommandName(command []string) string {
	for _, arg := range executionCommandArgs(command) {
		if arg != "" {
			return arg
		}
	}
	return ""
}

func executionCommandSummary(command []string) string {
	args := executionCommandArgs(command)
	parts := make([]string, 0, min(3, len(args)))
	truncated := false
	for _, arg := range args {
		if len(parts) == 3 {
			truncated = true
			break
		}
		parts = append(parts, arg)
	}
	if len(parts) == 0 {
		return ""
	}
	summary := strings.Join(parts, " ")
	if truncated {
		summary += " ..."
	}
	return summary
}

func validateExecutionSandboxArgs(chdir, existingSandboxID, fromSnapshot string, keep, dangerouslyAllowAll bool, repositoryOverride repositoryOverrideFlags, copyFlags workspaceCopyFlags) error {
	sandboxID := strings.TrimSpace(existingSandboxID)
	snapshotID := strings.TrimSpace(fromSnapshot)
	hasChdir := strings.TrimSpace(chdir) != ""

	if _, err := repositoryOverride.resolve(".", nil); err != nil {
		return err
	}
	if err := copyFlags.validate(existingSandboxID, fromSnapshot, repositoryOverride); err != nil {
		return err
	}

	if sandboxID != "" && snapshotID != "" {
		return errors.New("--from cannot be used with --in")
	}
	if sandboxID != "" && keep {
		return errors.New("--keep cannot be used with --in")
	}
	if sandboxID != "" && dangerouslyAllowAll {
		return errors.New("--dangerously-allow-all cannot be used with --in")
	}
	if snapshotID != "" && dangerouslyAllowAll {
		return errors.New("--dangerously-allow-all cannot be used with --from")
	}
	if sandboxID != "" && hasChdir && !copyFlags.CopyIn {
		return errors.New("--chdir cannot be used with --in")
	}
	if snapshotID != "" && hasChdir {
		return errors.New("--chdir cannot be used with --from")
	}
	if sandboxID != "" && repositoryOverride.hasRepositoryOverride() {
		return errors.New("--repo-url cannot be used with --in")
	}
	if snapshotID != "" && repositoryOverride.hasRepositoryOverride() {
		return errors.New("--repo-url cannot be used with --from")
	}

	return nil
}

func getFinalExecutionExitCode(callCtx context.Context, client *controlclient.Client, sandboxID, executionID string) (int, bool) {
	getResp, err := client.GetExecution(tracePreservingContext(callCtx), &cleanroomv1.GetExecutionRequest{
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
	callCtx context.Context,
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
		rpcCtx := tracePreservingContext(callCtx)

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
			if err := closeExecutionStdin(rpcCtx, client, sandboxID, executionID); err != nil {
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
				if _, err := client.WriteExecutionStdin(rpcCtx, &cleanroomv1.WriteExecutionStdinRequest{
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
				if err := closeExecutionStdin(rpcCtx, client, sandboxID, executionID); err != nil &&
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

func closeExecutionStdin(callCtx context.Context, client *controlclient.Client, sandboxID, executionID string) error {
	_, err := client.CloseExecutionStdin(tracePreservingContext(callCtx), &cleanroomv1.CloseExecutionStdinRequest{
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

func writeTraceID(stderr io.Writer, traceID string) error {
	traceID = strings.TrimSpace(traceID)
	if stderr == nil || traceID == "" {
		return nil
	}
	_, err := fmt.Fprintf(stderr, "trace_id=%s\n", traceID)
	return err
}

func replayExecutionHistory(callCtx context.Context, client *controlclient.Client, sandboxID, executionID string, stdout, stderr io.Writer) (int, bool, error) {
	stream, err := client.StreamExecution(tracePreservingContext(callCtx), &cleanroomv1.StreamExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Follow:      false,
	})
	if err != nil {
		return 0, false, err
	}
	defer func() {
		_ = stream.Close()
	}()

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

func terminateSandboxBestEffort(callCtx context.Context, client *controlclient.Client, sandboxID string, timeout time.Duration, logger *log.Logger, warnMessage string) {
	if client == nil || strings.TrimSpace(sandboxID) == "" {
		return
	}

	terminateCtx := tracePreservingContext(callCtx)
	var terminateCancel context.CancelFunc
	if timeout > 0 {
		terminateCtx, terminateCancel = context.WithTimeout(terminateCtx, timeout)
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
