package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type integrationAdapter struct {
	mu sync.Mutex

	runFn       func(context.Context, backend.RunRequest) (*backend.RunResult, error)
	runStreamFn func(context.Context, backend.RunRequest, backend.OutputStream) (*backend.RunResult, error)
}

func (a *integrationAdapter) Name() string { return "firecracker" }

func (a *integrationAdapter) Run(ctx context.Context, req backend.RunRequest) (*backend.RunResult, error) {
	a.mu.Lock()
	fn := a.runFn
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "ok"}, nil
}

func (a *integrationAdapter) RunStream(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
	a.mu.Lock()
	fn := a.runStreamFn
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, req, stream)
	}
	result, err := a.Run(ctx, req)
	if err != nil {
		return nil, err
	}
	if stream.OnStdout != nil && result.Stdout != "" {
		stream.OnStdout([]byte(result.Stdout))
	}
	if stream.OnStderr != nil && result.Stderr != "" {
		stream.OnStderr([]byte(result.Stderr))
	}
	return result, nil
}

type integrationLoader struct{}

func (integrationLoader) LoadAndCompile(_ string) (*policy.CompiledPolicy, string, error) {
	return &policy.CompiledPolicy{
		Hash:           "policy-hash-test",
		NetworkDefault: "deny",
	}, "/repo/cleanroom.yaml", nil
}

type execOutcome struct {
	err    error
	stdout string
	stderr string
	cause  error
}

func startIntegrationServer(t *testing.T, adapter backend.Adapter) (string, *controlservice.Service) {
	t.Helper()

	svc := &controlservice.Service{
		Loader: integrationLoader{},
		Config: runtimeconfig.Config{
			DefaultBackend: "firecracker",
		},
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
	}

	httpServer := httptest.NewServer(controlserver.New(svc, nil).Handler())
	t.Cleanup(httpServer.Close)
	return httpServer.URL, svc
}

func runExecWithCapture(cmd ExecCommand, ctx runtimeContext) execOutcome {
	tmpDir, err := os.MkdirTemp("", "cleanroom-cli-test-*")
	if err != nil {
		return execOutcome{cause: fmt.Errorf("create temp dir: %w", err)}
	}
	defer os.RemoveAll(tmpDir)

	stdoutPath := filepath.Join(tmpDir, "stdout.log")
	stderrPath := filepath.Join(tmpDir, "stderr.log")

	stdoutFile, err := os.Create(stdoutPath)
	if err != nil {
		return execOutcome{cause: fmt.Errorf("create stdout capture file: %w", err)}
	}
	defer stdoutFile.Close()

	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		return execOutcome{cause: fmt.Errorf("create stderr capture file: %w", err)}
	}
	defer stderrFile.Close()

	oldStderr := os.Stderr
	os.Stderr = stderrFile
	defer func() {
		os.Stderr = oldStderr
	}()

	ctx.Stdout = stdoutFile
	runErr := cmd.Run(&ctx)

	if err := stdoutFile.Sync(); err != nil {
		return execOutcome{cause: fmt.Errorf("sync stdout capture: %w", err)}
	}
	if err := stderrFile.Sync(); err != nil {
		return execOutcome{cause: fmt.Errorf("sync stderr capture: %w", err)}
	}

	stdoutBytes, err := os.ReadFile(stdoutPath)
	if err != nil {
		return execOutcome{cause: fmt.Errorf("read stdout capture: %w", err)}
	}
	stderrBytes, err := os.ReadFile(stderrPath)
	if err != nil {
		return execOutcome{cause: fmt.Errorf("read stderr capture: %w", err)}
	}

	return execOutcome{
		err:    runErr,
		stdout: string(stdoutBytes),
		stderr: string(stderrBytes),
	}
}

func withTestSignalChannel(t *testing.T) chan os.Signal {
	t.Helper()

	signalCh := make(chan os.Signal, 8)
	oldNewSignalChannel := newSignalChannel
	oldNotifySignals := notifySignals
	oldStopSignals := stopSignals

	newSignalChannel = func() chan os.Signal { return signalCh }
	notifySignals = func(_ chan os.Signal, _ ...os.Signal) {}
	stopSignals = func(_ chan os.Signal) {}

	t.Cleanup(func() {
		newSignalChannel = oldNewSignalChannel
		notifySignals = oldNotifySignals
		stopSignals = oldStopSignals
	})

	return signalCh
}

func mustReceiveWithin[T any](t *testing.T, ch <-chan T, timeout time.Duration, msg string) T {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(timeout):
		t.Fatal(msg)
		var zero T
		return zero
	}
}

func parseSandboxID(stderr string) string {
	match := regexp.MustCompile(`sandbox_id=([^\s]+)\s+execution_id=`).FindStringSubmatch(stderr)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func TestExecIntegrationStreamsOutput(t *testing.T) {
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("one\n"))
			}
			time.Sleep(25 * time.Millisecond)
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("two\n"))
			}
			return &backend.RunResult{
				RunID:    req.RunID,
				ExitCode: 0,
				Stdout:   "one\ntwo\n",
				Message:  "ok",
			}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		Host:    host,
		Chdir:   cwd,
		Command: []string{"echo", "ignored-by-adapter"},
	}, runtimeContext{
		CWD: cwd,
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if !strings.Contains(outcome.stdout, "one\n") || !strings.Contains(outcome.stdout, "two\n") {
		t.Fatalf("expected streamed stdout chunks, got %q", outcome.stdout)
	}
	if strings.Index(outcome.stdout, "one\n") > strings.Index(outcome.stdout, "two\n") {
		t.Fatalf("expected ordered stdout chunks, got %q", outcome.stdout)
	}
}

func TestExecIntegrationPropagatesExitAndStderr(t *testing.T) {
	adapter := &integrationAdapter{
		runFn: func(_ context.Context, req backend.RunRequest) (*backend.RunResult, error) {
			return &backend.RunResult{
				RunID:    req.RunID,
				ExitCode: 7,
				Stdout:   "out\n",
				Stderr:   "err\n",
				Message:  "failed",
			}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		Host:    host,
		Chdir:   cwd,
		Command: []string{"echo", "ignored-by-adapter"},
	}, runtimeContext{
		CWD: cwd,
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if got, want := ExitCode(outcome.err), 7; got != want {
		t.Fatalf("unexpected cli exit code: got %d want %d", got, want)
	}
	if !strings.Contains(outcome.stdout, "out\n") {
		t.Fatalf("expected stdout in stream output, got %q", outcome.stdout)
	}
	if !strings.Contains(outcome.stderr, "err\n") {
		t.Fatalf("expected stderr in stream output, got %q", outcome.stderr)
	}
}

func TestExecIntegrationFirstInterruptCancelsExecution(t *testing.T) {
	started := make(chan struct{}, 1)
	adapter := &integrationAdapter{
		runFn: func(ctx context.Context, _ backend.RunRequest) (*backend.RunResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	signalCh := withTestSignalChannel(t)
	cwd := t.TempDir()

	done := make(chan execOutcome, 1)
	go func() {
		done <- runExecWithCapture(ExecCommand{
			Host:    host,
			Chdir:   cwd,
			Command: []string{"sleep", "300"},
		}, runtimeContext{
			CWD: cwd,
		})
	}()

	_ = mustReceiveWithin(t, started, 2*time.Second, "timed out waiting for execution to start")
	signalCh <- os.Interrupt
	outcome := mustReceiveWithin(t, done, 2*time.Second, "timed out waiting for interrupted execution to exit")

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected cancellation error")
	}
	if got, want := ExitCode(outcome.err), 130; got != want {
		t.Fatalf("unexpected cli exit code: got %d want %d (err=%v)", got, want, outcome.err)
	}
}

func TestExecIntegrationSecondInterruptTerminatesSandbox(t *testing.T) {
	started := make(chan struct{}, 1)
	releaseRun := make(chan struct{})
	runReturned := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseRun)
		})
	}
	t.Cleanup(release)
	adapter := &integrationAdapter{
		runFn: func(ctx context.Context, _ backend.RunRequest) (*backend.RunResult, error) {
			defer close(runReturned)
			select {
			case started <- struct{}{}:
			default:
			}
			<-releaseRun
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return &backend.RunResult{ExitCode: 0, Message: "unexpected success"}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	signalCh := withTestSignalChannel(t)
	cwd := t.TempDir()

	done := make(chan execOutcome, 1)
	go func() {
		done <- runExecWithCapture(ExecCommand{
			Host:     host,
			Chdir:    cwd,
			LogLevel: "debug",
			Command:  []string{"sleep", "300"},
		}, runtimeContext{
			CWD: cwd,
		})
	}()

	_ = mustReceiveWithin(t, started, 2*time.Second, "timed out waiting for execution to start")
	signalCh <- os.Interrupt
	signalCh <- os.Interrupt

	outcome := mustReceiveWithin(t, done, 2*time.Second, "timed out waiting for second-interrupt exit")
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected cancellation error")
	}
	if got, want := ExitCode(outcome.err), 130; got != want {
		t.Fatalf("unexpected cli exit code: got %d want %d (err=%v)", got, want, outcome.err)
	}

	sandboxID := parseSandboxID(outcome.stderr)
	if sandboxID == "" {
		t.Fatalf("missing sandbox_id in stderr output: %q", outcome.stderr)
	}

	ep, err := endpoint.Resolve(host)
	if err != nil {
		t.Fatalf("resolve endpoint: %v", err)
	}
	client := controlclient.New(ep)
	getResp, err := client.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got, want := getResp.GetSandbox().GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED; got != want {
		t.Fatalf("unexpected sandbox status after second interrupt: got %v want %v", got, want)
	}

	release()
	_ = mustReceiveWithin(t, runReturned, 2*time.Second, "timed out waiting for adapter run to return after release")
}

func TestExecIntegrationVmPathUsesShForGuestCompatibility(t *testing.T) {
	adapter := &integrationAdapter{
		runFn: func(_ context.Context, req backend.RunRequest) (*backend.RunResult, error) {
			if len(req.Command) >= 1 && req.Command[0] == "bash" {
				return nil, errors.New(`exec: "bash": executable file not found in $PATH`)
			}
			return &backend.RunResult{
				RunID:    req.RunID,
				ExitCode: 0,
				Stdout:   "guest-ok\n",
				Message:  "ok",
			}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		Host:    host,
		Chdir:   cwd,
		Command: []string{"sh", "-lc", "echo guest-ok"},
	}, runtimeContext{
		CWD: cwd,
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if !strings.Contains(outcome.stdout, "guest-ok\n") {
		t.Fatalf("expected guest output, got %q", outcome.stdout)
	}
}

func TestParseSandboxID(t *testing.T) {
	in := "sandbox_id=cr-123 execution_id=exec-456\n"
	if got, want := parseSandboxID(in), "cr-123"; got != want {
		t.Fatalf("unexpected sandbox id: got %q want %q", got, want)
	}
	if got := parseSandboxID("no id here"); got != "" {
		t.Fatalf("expected empty sandbox id for invalid input, got %q", got)
	}
}
