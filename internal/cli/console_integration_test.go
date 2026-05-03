package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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

func TestConsoleIntegrationCopyOutAppliesSandboxChangesAfterSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	resolvedLocalRoot, err := gitOutput(localRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("resolve local repository root: %v", err)
	}
	baseCommit, copyOutPayload := prepareReadmeWorkspaceCopyOutPayload(t, localRoot, "console sandbox\n")

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", baseCommit, "main")

	var (
		mu       sync.Mutex
		commands []string
	)
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		command := strings.Join(req.Command, " ")
		mu.Lock()
		commands = append(commands, command)
		mu.Unlock()
		if strings.Contains(command, "cleanroom-copy-out-v1") {
			if stream.OnStdout != nil {
				stream.OnStdout(copyOutPayload)
			}
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
		}
		if stream.OnStdout != nil {
			stream.OnStdout([]byte("console ran\n"))
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags:        clientFlags{Host: host},
		In:                 sandboxID,
		workspaceCopyFlags: workspaceCopyFlags{CopyOut: true},
		Command:            []string{"sh"},
	}, "exit\n", runtimeContext{
		CWD:    localRoot,
		Loader: workspaceCopyRepositoryLoader(),
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ConsoleCommand.Run returned error: %v", outcome.err)
	}
	if got := mustReadFile(t, filepath.Join(localRoot, "README.md")); got != "console sandbox\n" {
		t.Fatalf("unexpected copied-out README: got %q", got)
	}
	if !strings.Contains(outcome.stdout, "console ran\n") {
		t.Fatalf("expected console stdout, got %q", outcome.stdout)
	}
	if strings.Contains(outcome.stdout, "write\t"+filepath.Join(resolvedLocalRoot, "README.md")+"\n") {
		t.Fatalf("copy-out plan should not be written to stdout, got %q", outcome.stdout)
	}
	if !strings.Contains(outcome.stderr, "write\t"+filepath.Join(resolvedLocalRoot, "README.md")+"\n") {
		t.Fatalf("expected copy-out plan in stderr, got %q", outcome.stderr)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := len(commands), 2; got != want {
		t.Fatalf("expected console command plus workspace copy-out command, got %d: %v", got, commands)
	}
	if !strings.Contains(commands[1], "cleanroom-copy-out-v1") {
		t.Fatalf("expected copy-out command second, got %q", commands[1])
	}
}

func TestConsoleIntegrationCopyOutFailurePreservesCreatedSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")

	adapter := &snapshotIntegrationAdapter{}
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		command := strings.Join(req.Command, " ")
		if strings.Contains(command, "cleanroom-copy-out-v1") {
			return nil, errors.New("copy-out unavailable")
		}
		if stream.OnStdout != nil {
			stream.OnStdout([]byte("console ran\n"))
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}
	host, _ := startIntegrationServer(t, adapter)

	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags:        clientFlags{Host: host},
		workspaceCopyFlags: workspaceCopyFlags{CopyOut: true},
		Command:            []string{"sh"},
	}, "exit\n", runtimeContext{
		CWD:    localRoot,
		Loader: workspaceCopyRepositoryLoader(),
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected copy-out failure")
	}
	if !strings.Contains(outcome.err.Error(), "workspace copy-out") {
		t.Fatalf("expected workspace copy-out error, got %v", outcome.err)
	}
	if !strings.Contains(outcome.stdout, "console ran\n") {
		t.Fatalf("expected console stdout, got %q", outcome.stdout)
	}
	sandboxID := parseSandboxID(outcome.stderr)
	if sandboxID == "" {
		t.Fatalf("expected sandbox id in stderr when preserving sandbox, got %q", outcome.stderr)
	}
	client := mustNewControlClient(t, host)
	requireSandboxStatus(t, client, sandboxID, cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY)

	adapter.mu.Lock()
	terminateCalls := adapter.terminateCalls
	adapter.mu.Unlock()
	if terminateCalls != 0 {
		t.Fatalf("expected copy-out failure to preserve created sandbox, got %d terminate calls", terminateCalls)
	}
}

func TestConsoleIntegrationForwardsStdinAndStreamsOutput(t *testing.T) {
	started := make(chan struct{}, 1)
	var captured bytes.Buffer
	adapter := &integrationAdapter{
		runStreamFn: func(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
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
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				Message:     "ok",
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

func TestConsoleIntegrationPassesResolvedEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "host-secret")

	var captured backend.ExecutionRequest
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			captured = req
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				Message:     "ok",
			}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()
	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Env:         []string{"OPENAI_API_KEY", "DEBUG=1", "EMPTY="},
		Command:     []string{"sh"},
	}, "", runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ConsoleCommand.Run returned error: %v", outcome.err)
	}
	if got, want := captured.Env, []string{"OPENAI_API_KEY=host-secret", "DEBUG=1", "EMPTY="}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected console env: got %v want %v", got, want)
	}
}

func TestConsoleIntegrationInterruptCancelsExecution(t *testing.T) {
	started := make(chan struct{}, 1)
	adapter := &integrationAdapter{
		runStreamFn: func(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
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
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
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
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "released"}, nil
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
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{WriteStdin: func(_ []byte) error { return nil }})
			}
			select {
			case started <- struct{}{}:
			default:
			}
			<-releaseRun
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "released"}, nil
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
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("ok\n"))
			}
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
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
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("ok\n"))
			}
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
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
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("ok\n"))
			}
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
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
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
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
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("ok\n"))
			}
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
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
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("ok\n"))
			}
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
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

func TestConsoleIntegrationRejectsChdirWhenReusingSandbox(t *testing.T) {
	cwd := t.TempDir()
	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags: clientFlags{Host: "tssvc://cleanroom"},
		Chdir:       cwd,
		In:          "cr_123",
		Command:     []string{"sh"},
	}, "", runtimeContext{
		CWD:    cwd,
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected ConsoleCommand.Run to reject --chdir with --in")
	}
	if got, want := outcome.err.Error(), "--chdir cannot be used with --in"; !strings.Contains(got, want) {
		t.Fatalf("expected error to contain %q, got %q", want, got)
	}
}

func TestConsoleIntegrationRejectsChdirWhenCreatingFromSnapshot(t *testing.T) {
	cwd := t.TempDir()
	outcome := runConsoleWithCapture(ConsoleCommand{
		clientFlags: clientFlags{Host: "tssvc://cleanroom"},
		Chdir:       cwd,
		From:        "snap_123",
		Command:     []string{"sh"},
	}, "", runtimeContext{
		CWD:    cwd,
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected ConsoleCommand.Run to reject --chdir with --from")
	}
	if got, want := outcome.err.Error(), "--chdir cannot be used with --from"; !strings.Contains(got, want) {
		t.Fatalf("expected error to contain %q, got %q", want, got)
	}
}

func TestConsoleIntegrationRoutesBackendWarningsToStderr(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
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
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
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

func TestConsoleIntegrationOmitsFailureFooter(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    7,
				Message:     "failed",
				RunDir:      "/tmp/console-failed",
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
	if outcome.err == nil {
		t.Fatal("expected non-zero exit from failed console execution")
	}
	if got, want := ExitCode(outcome.err), 7; got != want {
		t.Fatalf("unexpected console exit code: got %d want %d (err=%v)", got, want, outcome.err)
	}
	if strings.Contains(outcome.stderr, "sandbox_id=") {
		t.Fatalf("did not expect sandbox_id footer in failure output, got %q", outcome.stderr)
	}
	if strings.Contains(outcome.stderr, "execution_id=") {
		t.Fatalf("did not expect execution_id footer in failure output, got %q", outcome.stderr)
	}
	if strings.Contains(outcome.stderr, "inspect_command=cleanroom execution inspect ") {
		t.Fatalf("did not expect inspect_command footer in failure output, got %q", outcome.stderr)
	}
	if strings.Contains(outcome.stderr, "artifacts_dir=/tmp/console-failed") {
		t.Fatalf("did not expect artifacts_dir footer in failure output, got %q", outcome.stderr)
	}
}
