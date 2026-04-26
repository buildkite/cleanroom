package cli

import (
	"archive/tar"
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
	"github.com/buildkite/cleanroom/internal/controlclient"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func runAgentWithCapture(cmd AgentCommand, stdinData string, ctx runtimeContext) execOutcome {
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

func TestAgentIntegrationStartsPersistentSandbox(t *testing.T) {
	var gotCommand []string
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if !req.TTY {
				return nil, errors.New("expected tty execution")
			}
			gotCommand = append([]string(nil), req.Command...)
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("codex-ready\n"))
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
	outcome := runAgentWithCapture(AgentCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Agent:       "amp",
	}, "", runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("AgentCommand.Run returned error: %v", outcome.err)
	}
	assertAgentShellCommand(t, gotCommand, "amp", "exec amp")

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

func TestAgentIntegrationUsesPolicyImageForCreatedSandbox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	imageRefCh := make(chan string, 1)
	var gotCommand []string
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
			if req.Policy == nil {
				return nil, errors.New("expected policy on run request")
			}
			imageRefCh <- req.Policy.ImageRef
			gotCommand = append([]string(nil), req.Command...)
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				Message:     "ok",
			}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()
	outcome := runAgentWithCapture(AgentCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Agent:       "codex",
	}, "", runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("AgentCommand.Run returned error: %v", outcome.err)
	}
	assertAgentShellCommand(t, gotCommand, "codex", "exec sh -lc 'if command -v codex >/dev/null 2>&1; then exec codex \"$@\"; fi; exec env MISE_YES=1 MISE_TRUSTED_CONFIG_PATHS=/workspace mise --no-config exec -y nodejs@lts -- npm exec --yes --package @openai/codex@latest -- codex \"$@\"' sh")

	gotImageRef := mustReceiveWithin(t, imageRefCh, 2*time.Second, "timed out waiting for run request policy")
	if got, want := gotImageRef, integrationPolicyImageRef; got != want {
		t.Fatalf("unexpected image ref: got %q want %q", got, want)
	}
}

func TestAgentIntegrationDangerouslyAllowAllSetsAllowNetworkDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	adapter := &snapshotIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()

	outcome := runAgentWithCapture(AgentCommand{
		clientFlags:         clientFlags{Host: host},
		Chdir:               cwd,
		DangerouslyAllowAll: true,
		Agent:               "codex",
	}, "", runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("AgentCommand.Run returned error: %v", outcome.err)
	}
	if adapter.provisionReq.Policy == nil {
		t.Fatal("expected provisioned policy")
	}
	if got, want := adapter.provisionReq.Policy.NetworkDefault, "allow"; got != want {
		t.Fatalf("unexpected provisioned network default: got %q want %q", got, want)
	}
}

func TestAgentIntegrationPassesArgsToCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var gotCommand []string
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			gotCommand = append([]string(nil), req.Command...)
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				Message:     "ok",
			}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()
	outcome := runAgentWithCapture(AgentCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Agent:       "codex",
		Args:        []string{"--", "exec", "--yolo", "fix lint failures"},
	}, "", runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("AgentCommand.Run returned error: %v", outcome.err)
	}
	assertAgentShellCommand(t, gotCommand, "codex", "exec sh -lc 'if command -v codex >/dev/null 2>&1; then exec codex \"$@\"; fi; exec env MISE_YES=1 MISE_TRUSTED_CONFIG_PATHS=/workspace mise --no-config exec -y nodejs@lts -- npm exec --yes --package @openai/codex@latest -- codex \"$@\"' sh 'exec' '--yolo' 'fix lint failures'")
}

func TestAgentIntegrationCopiesConfiguredCredentialsBeforeAttach(t *testing.T) {
	hostHome := t.TempDir()
	sourcePath := filepath.Join(hostHome, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		t.Fatalf("mkdir credentials: %v", err)
	}
	if err := os.WriteFile(sourcePath, []byte(`{"token":"redacted"}`), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}

	adapter := &agentCredentialCopyAdapter{
		integrationAdapter: &integrationAdapter{
			runStreamFn: func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
				if !req.TTY {
					return nil, errors.New("expected agent execution to use tty")
				}
				return &backend.ExecutionResult{
					ExecutionID: req.ExecutionID,
					ExitCode:    0,
					Message:     "ok",
				}, nil
			},
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()
	outcome := runAgentWithCapture(AgentCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Agent:       "codex",
	}, "", runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
		Config: runtimeconfig.Config{
			Agents: map[string]runtimeconfig.Agent{
				"codex": {
					Command: "codex",
					Test:    "command -v codex >/dev/null 2>&1",
					Credentials: []runtimeconfig.AgentCredential{
						{Source: sourcePath, Target: "~/.codex/auth.json"},
					},
				},
			},
		},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("AgentCommand.Run returned error: %v", outcome.err)
	}
	if got, want := adapter.archiveDestination, "/"; got != want {
		t.Fatalf("unexpected credential archive destination: got %q want %q", got, want)
	}
	assertAgentShellCommand(t, adapter.runReq.Command, "codex", "exec codex")

	tr := tar.NewReader(bytes.NewReader(adapter.archive.Bytes()))
	header, err := tr.Next()
	if err != nil {
		t.Fatalf("read credential archive header: %v", err)
	}
	if got, want := header.Name, "root/.codex/auth.json"; got != want {
		t.Fatalf("unexpected credential archive path: got %q want %q", got, want)
	}
	body, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("read credential archive body: %v", err)
	}
	if got, want := string(body), `{"token":"redacted"}`; got != want {
		t.Fatalf("unexpected credential archive body: got %q want %q", got, want)
	}
}

type agentCredentialCopyAdapter struct {
	*integrationAdapter

	archiveDestination string
	archive            bytes.Buffer
}

func (a *agentCredentialCopyAdapter) ExtractSandboxArchive(_ context.Context, _ string, destination string, r io.Reader) (int64, error) {
	a.archiveDestination = destination
	return io.Copy(&a.archive, r)
}

func TestAgentIntegrationReusesProvidedSandboxWithoutLoadingPolicy(t *testing.T) {
	var gotCommand []string
	host, _ := startIntegrationServer(t, &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
			gotCommand = append([]string(nil), req.Command...)
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				Message:     "ok",
			}, nil
		},
	})
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	outcome := runAgentWithCapture(AgentCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sandboxID,
		Agent:       "amp",
	}, "", runtimeContext{
		CWD:    t.TempDir(),
		Loader: failingLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("AgentCommand.Run returned error: %v", outcome.err)
	}
	assertAgentShellCommand(t, gotCommand, "amp", "exec amp")
}

func assertAgentShellCommand(t *testing.T, got []string, testName, execLine string) {
	t.Helper()
	if len(got) != 3 || got[0] != "sh" || got[1] != "-lc" {
		t.Fatalf("unexpected command wrapper: got %v", got)
	}
	script := got[2]
	if !strings.Contains(script, "command -v '"+testName+"' >/dev/null 2>&1") &&
		!strings.Contains(script, "command -v "+testName+" >/dev/null 2>&1") &&
		!strings.Contains(script, "mise exec -- "+testName+" --version >/dev/null 2>&1") &&
		!strings.Contains(script, "mise --no-config exec -y nodejs@lts -- npm exec --yes --package ") {
		t.Fatalf("expected agent test for %q in script:\n%s", testName, script)
	}
	if !strings.Contains(script, execLine) {
		t.Fatalf("expected exec line %q in script:\n%s", execLine, script)
	}
}
