package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func runConsoleWithCapture(cmd ConsoleCommand, stdinData string, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, &stdinData, ctx)
}

func TestConsoleIntegrationForwardsStdinAndStreamsOutput(t *testing.T) {
	started := make(chan struct{}, 1)
	var captured bytes.Buffer
	adapter := &integrationAdapter{
		runStreamFn: func(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
			if !req.TTY {
				return nil, errors.New("expected tty execution")
			}

			done := make(chan struct{})
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(data []byte) error {
						captured.Write(data)
						if stream.OnStdout != nil {
							stream.OnStdout(data)
						}
						if bytes.Contains(captured.Bytes(), []byte("exit\n")) {
							select {
							case <-done:
							default:
								close(done)
							}
						}
						return nil
					},
				})
			}
			select {
			case started <- struct{}{}:
			default:
			}

			select {
			case <-done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &backend.RunResult{
				RunID:    req.RunID,
				ExitCode: 0,
				Stdout:   captured.String(),
				Message:  "ok",
			}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()
	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		// Console defaults to host passthrough for this MVP.
		Command: []string{"sh"},
	}, "hello\nexit\n", runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ConsoleCommand.Run returned error: %v", outcome.err)
	}
	if !strings.Contains(captured.String(), "hello\nexit\n") {
		t.Fatalf("expected stdin to be forwarded to backend, got %q", captured.String())
	}
	if !strings.Contains(outcome.stdout, "hello\n") || !strings.Contains(outcome.stdout, "exit\n") {
		t.Fatalf("expected streamed output to include echoed stdin, got %q", outcome.stdout)
	}
	_ = mustReceiveWithin(t, started, 2*time.Second, "timed out waiting for console execution to start")
}

func TestConsoleIntegrationInterruptCancelsExecution(t *testing.T) {
	started := make(chan struct{}, 1)
	adapter := &integrationAdapter{
		runStreamFn: func(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(_ []byte) error { return nil },
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

	host, _ := startIntegrationServer(t, adapter)
	signalCh := withTestSignalChannel(t)
	cwd := t.TempDir()

	done := make(chan execOutcome, 1)
	go func() {
		done <- runConsoleWithCapture(ConsoleCommand{
			clientFlags: clientFlags{Host: host},
			Chdir:       cwd,
		}, "", runtimeContext{
			CWD:    cwd,
			Loader: integrationLoader{},
		})
	}()

	_ = mustReceiveWithin(t, started, 2*time.Second, "timed out waiting for console execution to start")
	signalCh <- os.Interrupt

	outcome := mustReceiveWithin(t, done, 2*time.Second, "timed out waiting for interrupted console to exit")
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected non-zero exit from interrupted console")
	}
	if got, want := ExitCode(outcome.err), 130; got != want {
		t.Fatalf("unexpected console exit code: got %d want %d (err=%v)", got, want, outcome.err)
	}
}

func TestConsoleIntegrationSecondInterruptForcesLocalExitWhenExecutionIgnoresCancel(t *testing.T) {
	started := make(chan struct{}, 1)
	releaseRun := make(chan struct{})
	t.Cleanup(func() {
		close(releaseRun)
	})
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(_ []byte) error { return nil },
				})
			}
			select {
			case started <- struct{}{}:
			default:
			}
			<-releaseRun
			return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "released"}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	signalCh := withTestSignalChannel(t)
	cwd := t.TempDir()

	done := make(chan execOutcome, 1)
	go func() {
		done <- runConsoleWithCapture(ConsoleCommand{
			clientFlags: clientFlags{Host: host},
			Chdir:       cwd,
		}, "", runtimeContext{
			CWD:    cwd,
			Loader: integrationLoader{},
		})
	}()

	_ = mustReceiveWithin(t, started, 2*time.Second, "timed out waiting for console execution to start")
	signalCh <- os.Interrupt
	signalCh <- os.Interrupt

	outcome := mustReceiveWithin(t, done, 2*time.Second, "timed out waiting for second interrupt to force local console exit")
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected non-zero exit from forced local interrupt")
	}
	if got, want := ExitCode(outcome.err), 130; got != want {
		t.Fatalf("unexpected console exit code: got %d want %d (err=%v)", got, want, outcome.err)
	}
}

func TestConsoleIntegrationSecondInterruptAfterWindowDoesNotForceLocalExit(t *testing.T) {
	started := make(chan struct{}, 1)
	releaseRun := make(chan struct{})
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{WriteStdin: func(_ []byte) error { return nil }})
			}
			select {
			case started <- struct{}{}:
			default:
			}
			<-releaseRun
			return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Message: "released"}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	signalCh := withTestSignalChannel(t)
	cwd := t.TempDir()

	done := make(chan execOutcome, 1)
	go func() {
		done <- runConsoleWithCapture(ConsoleCommand{
			clientFlags: clientFlags{Host: host},
			Chdir:       cwd,
		}, "", runtimeContext{CWD: cwd, Loader: integrationLoader{}})
	}()

	_ = mustReceiveWithin(t, started, 2*time.Second, "timed out waiting for console execution to start")
	signalCh <- os.Interrupt
	time.Sleep(2*interruptForceExitWindow + 100*time.Millisecond)
	signalCh <- os.Interrupt

	select {
	case outcome := <-done:
		t.Fatalf("unexpected forced local exit after interrupt window elapsed: err=%v cause=%v", outcome.err, outcome.cause)
	case <-time.After(300 * time.Millisecond):
	}

	close(releaseRun)
	outcome := mustReceiveWithin(t, done, 2*time.Second, "timed out waiting for console execution to finish after release")
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected canceled exit after interrupt")
	}
	if got, want := ExitCode(outcome.err), 130; got != want {
		t.Fatalf("unexpected console exit code: got %d want %d (err=%v)", got, want, outcome.err)
	}
}

func TestConsoleRejectsUnsupportedHostScheme(t *testing.T) {
	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags: clientFlags{Host: "tssvc://cleanroom"},
	}, "", runtimeContext{
		CWD: t.TempDir(),
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected host validation error")
	}
	if !strings.Contains(outcome.err.Error(), "unsupported endpoint") {
		t.Fatalf("expected unsupported endpoint error, got %v", outcome.err)
	}
}

func TestConsoleIntegrationDefaultTerminatesCreatedSandbox(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, _ backend.OutputStream) (*backend.RunResult, error) {
			return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Stdout: "ok\n", Message: "ok"}, nil
		},
	})

	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags:    clientFlags{Host: host},
		PrintSandboxID: true,
		Command:        []string{"sh"},
	}, "", runtimeContext{
		CWD:    t.TempDir(),
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ConsoleCommand.Run returned error: %v", outcome.err)
	}

	sandboxID := parseSandboxID(outcome.stderr)
	if sandboxID == "" {
		t.Fatalf("missing sandbox_id in stderr output: %q", outcome.stderr)
	}

	client := mustNewControlClient(t, host)
	requireSandboxStatus(t, client, sandboxID, cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED)
}

func TestConsoleIntegrationKeepPreservesCreatedSandbox(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, _ backend.OutputStream) (*backend.RunResult, error) {
			return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Stdout: "ok\n", Message: "ok"}, nil
		},
	})

	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags: clientFlags{Host: host},
		Keep:        true,
		Command:     []string{"sh"},
	}, "", runtimeContext{
		CWD:    t.TempDir(),
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ConsoleCommand.Run returned error: %v", outcome.err)
	}

	sandboxID := parseSandboxID(outcome.stderr)
	if sandboxID == "" {
		t.Fatalf("missing sandbox_id in stderr output: %q", outcome.stderr)
	}

	client := mustNewControlClient(t, host)
	requireSandboxStatus(t, client, sandboxID, cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY)
}

func TestConsoleIntegrationKeepWithPrintSandboxIDPrintsOnce(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, _ backend.OutputStream) (*backend.RunResult, error) {
			return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Stdout: "ok\n", Message: "ok"}, nil
		},
	})

	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags:    clientFlags{Host: host},
		Keep:           true,
		PrintSandboxID: true,
		Command:        []string{"sh"},
	}, "", runtimeContext{
		CWD:    t.TempDir(),
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ConsoleCommand.Run returned error: %v", outcome.err)
	}
	if got, want := strings.Count(outcome.stderr, "sandbox_id="), 1; got != want {
		t.Fatalf("expected one sandbox_id marker in stderr, got %d: %q", got, outcome.stderr)
	}
}

func TestConsoleIntegrationReuseSandboxSkipsPolicyCompile(t *testing.T) {
	started := make(chan struct{}, 1)
	host, _ := startIntegrationServer(t, &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
			done := make(chan struct{})
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(data []byte) error {
						if stream.OnStdout != nil {
							stream.OnStdout(data)
						}
						if bytes.Contains(data, []byte("exit\n")) {
							select {
							case <-done:
							default:
								close(done)
							}
						}
						return nil
					},
				})
			}
			select {
			case started <- struct{}{}:
			default:
			}
			<-done
			return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Stdout: "ok\n", Message: "ok"}, nil
		},
	})
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags: clientFlags{Host: host},
		In:          sandboxID,
		Command:     []string{"sh"},
	}, "exit\n", runtimeContext{
		CWD:    t.TempDir(),
		Loader: failingLoader{},
	})
	_ = mustReceiveWithin(t, started, 2*time.Second, "timed out waiting for console execution to start")
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ConsoleCommand.Run returned error: %v", outcome.err)
	}
	if !strings.Contains(outcome.stdout, "ok") {
		t.Fatalf("expected console output, got %q", outcome.stdout)
	}
}

func TestConsoleIntegrationRejectsKeepWhenReusingSandbox(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, _ backend.OutputStream) (*backend.RunResult, error) {
			return &backend.RunResult{RunID: req.RunID, ExitCode: 0, Stdout: "ok\n", Message: "ok"}, nil
		},
	})
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags: clientFlags{Host: host},
		In:          sandboxID,
		Keep:        true,
		Command:     []string{"sh"},
	}, "", runtimeContext{
		CWD:    t.TempDir(),
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected ConsoleCommand.Run to reject --keep with --in")
	}
	if got, want := outcome.err.Error(), "--keep cannot be used with --in"; !strings.Contains(got, want) {
		t.Fatalf("expected error to contain %q, got %q", want, got)
	}
}

func TestConsoleIntegrationRoutesBackendWarningsToStderr(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func([]byte) error { return nil },
				})
			}
			if stream.OnStderr != nil {
				stream.OnStderr([]byte("warning: backend warning\n"))
			}
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("/ # "))
			}
			return &backend.RunResult{
				RunID:    req.RunID,
				ExitCode: 0,
				Stdout:   "/ # ",
				Stderr:   "warning: backend warning\n",
			}, nil
		},
	})

	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags: clientFlags{Host: host},
		Command:     []string{"sh"},
	}, "", runtimeContext{
		CWD:    t.TempDir(),
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ConsoleCommand.Run returned error: %v", outcome.err)
	}
	if !strings.Contains(outcome.stderr, "warning: backend warning\n") {
		t.Fatalf("expected warning on stderr, got %q", outcome.stderr)
	}
	if strings.Contains(outcome.stdout, "warning: backend warning\n") {
		t.Fatalf("unexpected warning in stdout: %q", outcome.stdout)
	}
}
