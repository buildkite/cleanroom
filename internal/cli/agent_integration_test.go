package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func runAgentCodexWithCapture(cmd AgentCodexCommand, stdinData string, ctx runtimeContext) execOutcome {
	tmpDir, err := os.MkdirTemp("", "cleanroom-agent-codex-test-*")
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

func TestAgentCodexIntegrationStartsPersistentSandbox(t *testing.T) {
	var gotCommand []string
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
			if !req.TTY {
				return nil, errors.New("expected tty execution")
			}
			gotCommand = append([]string(nil), req.Command...)
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("codex-ready\n"))
			}
			return &backend.RunResult{
				RunID:    req.RunID,
				ExitCode: 0,
				Stdout:   "codex-ready\n",
				Message:  "ok",
			}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()
	outcome := runAgentCodexWithCapture(AgentCodexCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
	}, "", runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("AgentCodexCommand.Run returned error: %v", outcome.err)
	}
	if got, want := gotCommand, []string{"codex"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected command: got %v want %v", got, want)
	}

	ep, err := endpoint.Resolve(host)
	if err != nil {
		t.Fatalf("resolve endpoint: %v", err)
	}
	client, err := controlclient.New(ep)
	if err != nil {
		t.Fatalf("create control client: %v", err)
	}
	listResp, err := client.ListSandboxes(context.Background(), &cleanroomv1.ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("ListSandboxes returned error: %v", err)
	}
	if got, want := len(listResp.GetSandboxes()), 1; got != want {
		t.Fatalf("unexpected sandbox count: got %d want %d", got, want)
	}
	if got, want := listResp.GetSandboxes()[0].GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY; got != want {
		t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
	}
}

func TestAgentCodexIntegrationPassesArgsToCodex(t *testing.T) {
	var gotCommand []string
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
			gotCommand = append([]string(nil), req.Command...)
			return &backend.RunResult{
				RunID:    req.RunID,
				ExitCode: 0,
				Message:  "ok",
			}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()
	outcome := runAgentCodexWithCapture(AgentCodexCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Args:        []string{"exec", "--yolo", "fix lint failures"},
	}, "", runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("AgentCodexCommand.Run returned error: %v", outcome.err)
	}
	if got, want := gotCommand, []string{"codex", "exec", "--yolo", "fix lint failures"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected command: got %v want %v", got, want)
	}
}
