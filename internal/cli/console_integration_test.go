package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
)

func runConsoleWithCapture(cmd ConsoleCommand, stdinData string, ctx runtimeContext) execOutcome {
	tmpDir, err := os.MkdirTemp("", "cleanroom-console-test-*")
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

	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		return execOutcome{cause: fmt.Errorf("create stdin pipe: %w", err)}
	}
	if stdinData != "" {
		if _, err := io.WriteString(stdinWriter, stdinData); err != nil {
			return execOutcome{cause: fmt.Errorf("write stdin payload: %w", err)}
		}
	}
	_ = stdinWriter.Close()
	defer stdinReader.Close()

	oldStdin := os.Stdin
	oldStderr := os.Stderr
	os.Stdin = stdinReader
	os.Stderr = stderrFile
	defer func() {
		os.Stdin = oldStdin
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
		Host:  host,
		Chdir: cwd,
		// Console defaults to host passthrough for this MVP.
		Command: []string{"sh"},
	}, "hello\nexit\n", runtimeContext{
		CWD: cwd,
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
			Host:  host,
			Chdir: cwd,
		}, "", runtimeContext{
			CWD: cwd,
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
