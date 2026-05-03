package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	"github.com/buildkite/cleanroom/internal/interactivequic"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type integrationAdapter struct {
	mu sync.Mutex

	runFn                    func(context.Context, backend.ExecutionRequest) (*backend.ExecutionResult, error)
	runStreamFn              func(context.Context, backend.ExecutionRequest, backend.OutputStream) (*backend.ExecutionResult, error)
	provisionFn              func(context.Context, backend.ProvisionRequest) error
	provisionFromSnapshotFn  func(context.Context, backend.ProvisionFromSnapshotRequest) error
	createSnapshotFn         func(context.Context, backend.SnapshotRequest) (*backend.SnapshotResult, error)
	deleteSnapshotFn         func(context.Context, backend.DeleteSnapshotRequest) error
	terminateFn              func(context.Context, string) error
	runReq                   backend.ExecutionRequest
	provisionReq             backend.ProvisionRequest
	provisionFromSnapshotReq backend.ProvisionFromSnapshotRequest
	createSnapshotReq        backend.SnapshotRequest
	deleteSnapshotReq        backend.DeleteSnapshotRequest
	runCalls                 int
	runInSandboxCalls        int
	provisionCalls           int
	terminateCalls           int
}

func (a *integrationAdapter) Name() string { return "firecracker" }

func (a *integrationAdapter) ProvisionSandbox(context.Context, backend.ProvisionRequest) error {
	return nil
}

func (a *integrationAdapter) RunInSandbox(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
	a.mu.Lock()
	a.runReq = req
	a.runCalls++
	streamFn := a.runStreamFn
	fn := a.runFn
	a.mu.Unlock()
	if streamFn != nil {
		return streamFn(ctx, req, stream)
	}
	if fn != nil {
		return fn(ctx, req)
	}
	return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
}

func (a *integrationAdapter) TerminateSandbox(context.Context, string) error { return nil }

type snapshotIntegrationAdapter struct {
	integrationAdapter
}

func (a *snapshotIntegrationAdapter) ProvisionSandbox(ctx context.Context, req backend.ProvisionRequest) error {
	a.mu.Lock()
	a.provisionCalls++
	a.provisionReq = req
	fn := a.provisionFn
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return nil
}

func (a *snapshotIntegrationAdapter) RunInSandbox(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
	a.mu.Lock()
	a.runInSandboxCalls++
	a.mu.Unlock()
	return a.integrationAdapter.RunInSandbox(ctx, req, stream)
}

func (a *snapshotIntegrationAdapter) CreateSnapshot(ctx context.Context, req backend.SnapshotRequest) (*backend.SnapshotResult, error) {
	a.mu.Lock()
	a.createSnapshotReq = req
	fn := a.createSnapshotFn
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &backend.SnapshotResult{StorageRef: "/snapshots/" + req.SnapshotID + ".ext4"}, nil
}

func (a *snapshotIntegrationAdapter) ProvisionSandboxFromSnapshot(ctx context.Context, req backend.ProvisionFromSnapshotRequest) error {
	a.mu.Lock()
	a.provisionFromSnapshotReq = req
	fn := a.provisionFromSnapshotFn
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return nil
}

func (a *snapshotIntegrationAdapter) DeleteSnapshot(ctx context.Context, req backend.DeleteSnapshotRequest) error {
	a.mu.Lock()
	a.deleteSnapshotReq = req
	fn := a.deleteSnapshotFn
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return nil
}

func (a *snapshotIntegrationAdapter) TerminateSandbox(ctx context.Context, sandboxID string) error {
	a.mu.Lock()
	a.terminateCalls++
	fn := a.terminateFn
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, sandboxID)
	}
	return nil
}

type integrationLoader struct{}

const integrationPolicyImageRef = "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func (integrationLoader) LoadAndCompile(_ string) (*policy.CompiledPolicy, string, error) {
	return &policy.CompiledPolicy{
		Version:        1,
		ImageRef:       integrationPolicyImageRef,
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
	}, "/repo/cleanroom.yaml", nil
}

func (integrationLoader) LoadRepository(_ string) (policy.RepositoryConfig, string, error) {
	return policy.RepositoryConfig{}, "/repo/cleanroom.yaml", nil
}

type failingLoader struct{}

func (failingLoader) LoadAndCompile(_ string) (*policy.CompiledPolicy, string, error) {
	return nil, "", errors.New("loader should not be called")
}

func (failingLoader) LoadRepository(_ string) (policy.RepositoryConfig, string, error) {
	return policy.RepositoryConfig{}, "/repo/cleanroom.yaml", nil
}

type execOutcome struct {
	err    error
	stdout string
	stderr string
	cause  error
}

func startIntegrationServer(t *testing.T, adapter backend.Adapter) (string, *controlservice.Service) {
	t.Helper()

	return startIntegrationServerWithConfig(t, adapter, runtimeconfig.Config{
		DefaultBackend: "firecracker",
		Backends: runtimeconfig.Backends{
			Firecracker: runtimeconfig.FirecrackerConfig{
				Snapshots: runtimeconfig.SnapshotConfig{
					Enabled: true,
					Driver:  "file",
				},
			},
		},
	})
}

func startIntegrationServerWithConfig(t *testing.T, adapter backend.Adapter, cfg runtimeconfig.Config) (string, *controlservice.Service) {
	t.Helper()

	store, err := snapshotstore.New(snapshotstore.Options{
		MetadataDBPath: filepath.Join(t.TempDir(), "snapshots.db"),
	})
	if err != nil {
		t.Fatalf("create snapshot store: %v", err)
	}

	svc := &controlservice.Service{
		Loader:        integrationLoader{},
		Config:        cfg,
		SnapshotStore: store,
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
	}

	httpServer := httptest.NewServer(controlserver.New(svc, nil).TrustInternalWorkspaceCopyInRequests().Handler())
	t.Cleanup(httpServer.Close)

	quicCtx, quicCancel := context.WithCancel(context.Background())
	t.Cleanup(quicCancel)
	quicServer, err := interactivequic.Start(quicCtx, "127.0.0.1:0", svc, nil)
	if err != nil {
		t.Fatalf("start interactive quic server: %v", err)
	}
	t.Cleanup(func() {
		_ = quicServer.Close()
	})
	svc.ConfigureInteractiveTransport(
		quicServer.Addr().String(),
		quicServer.ALPN(),
		quicServer.CertPinSHA256(),
	)

	return httpServer.URL, svc
}

func startUnixIntegrationServer(t *testing.T, adapter backend.Adapter) (string, *controlservice.Service) {
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

	socketFile, err := os.CreateTemp("/tmp", "cleanroom-test-*.sock")
	if err != nil {
		t.Fatalf("create unix socket path: %v", err)
	}
	socketPath := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatalf("close unix socket temp file: %v", err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatalf("remove unix socket temp file: %v", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	server := &http.Server{Handler: controlserver.New(svc, nil).TrustInternalWorkspaceCopyInRequests().Handler()}
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})

	quicCtx, quicCancel := context.WithCancel(context.Background())
	t.Cleanup(quicCancel)
	quicServer, err := interactivequic.Start(quicCtx, "127.0.0.1:0", svc, nil)
	if err != nil {
		t.Fatalf("start interactive quic server: %v", err)
	}
	t.Cleanup(func() {
		_ = quicServer.Close()
	})
	svc.ConfigureInteractiveTransport(
		quicServer.Addr().String(),
		quicServer.ALPN(),
		quicServer.CertPinSHA256(),
	)

	return "unix://" + socketPath, svc
}

func runWithCapture(runFn func(*runtimeContext) error, stdinData *string, ctx runtimeContext) execOutcome {
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

	oldStdin := os.Stdin
	if stdinData != nil {
		stdinReader, stdinWriter, err := os.Pipe()
		if err != nil {
			return execOutcome{cause: fmt.Errorf("create stdin pipe: %w", err)}
		}
		defer stdinReader.Close()
		if _, err := io.WriteString(stdinWriter, *stdinData); err != nil {
			_ = stdinWriter.Close()
			return execOutcome{cause: fmt.Errorf("write stdin payload: %w", err)}
		}
		if err := stdinWriter.Close(); err != nil {
			return execOutcome{cause: fmt.Errorf("close stdin writer: %w", err)}
		}
		os.Stdin = stdinReader
		defer func() {
			os.Stdin = oldStdin
		}()
	}

	oldStderr := os.Stderr
	os.Stderr = stderrFile
	defer func() {
		os.Stderr = oldStderr
	}()

	ctx.Stdout = stdoutFile
	runErr := runFn(&ctx)

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

func runExecWithCapture(cmd ExecCommand, ctx runtimeContext) execOutcome {
	emptyStdin := ""
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, &emptyStdin, ctx)
}

func TestExecIntegrationCopyOutAppliesSandboxChangesAfterCommand(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	resolvedLocalRoot, err := gitOutput(localRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("resolve local repository root: %v", err)
	}
	baseCommit, copyOutPayload := prepareReadmeWorkspaceCopyOutPayload(t, localRoot, "sandbox\n")

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
		switch {
		case strings.Contains(command, "cleanroom-copy-out-v1"):
			if stream.OnStdout != nil {
				stream.OnStdout(copyOutPayload)
			}
		default:
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("command ran\n"))
			}
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runExecWithCapture(ExecCommand{
		clientFlags:        clientFlags{Host: host},
		In:                 sandboxID,
		workspaceCopyFlags: workspaceCopyFlags{CopyOut: true},
		Command:            []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    localRoot,
		Loader: workspaceCopyRepositoryLoader(),
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if got := mustReadFile(t, filepath.Join(localRoot, "README.md")); got != "sandbox\n" {
		t.Fatalf("unexpected copied-out README: got %q", got)
	}
	if !strings.Contains(outcome.stdout, "command ran\n") {
		t.Fatalf("expected command stdout, got %q", outcome.stdout)
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
		t.Fatalf("expected user command plus workspace copy-out command, got %d: %v", got, commands)
	}
	if strings.Contains(commands[0], "cleanroom-copy-out-v1") {
		t.Fatalf("expected user command before copy-out, got %q", commands[0])
	}
	if !strings.Contains(commands[1], "cleanroom-copy-out-v1") {
		t.Fatalf("expected copy-out command second, got %q", commands[1])
	}
}

func TestExecIntegrationCopyOutRunsAfterNonZeroExit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	resolvedLocalRoot, err := gitOutput(localRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("resolve local repository root: %v", err)
	}
	baseCommit, copyOutPayload := prepareReadmeWorkspaceCopyOutPayload(t, localRoot, "sandbox after failure\n")

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", baseCommit, "main")

	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		command := strings.Join(req.Command, " ")
		if strings.Contains(command, "cleanroom-copy-out-v1") {
			if stream.OnStdout != nil {
				stream.OnStdout(copyOutPayload)
			}
			return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
		}
		if stream.OnStderr != nil {
			stream.OnStderr([]byte("failed\n"))
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 7, Message: "failed"}, nil
	}

	outcome := runExecWithCapture(ExecCommand{
		clientFlags:        clientFlags{Host: host},
		In:                 sandboxID,
		workspaceCopyFlags: workspaceCopyFlags{CopyOut: true},
		Command:            []string{"false"},
	}, runtimeContext{
		CWD:    localRoot,
		Loader: workspaceCopyRepositoryLoader(),
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	var exitErr exitCodeError
	if !errors.As(outcome.err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("expected exit code 7 after copy-out, got %v", outcome.err)
	}
	if got := mustReadFile(t, filepath.Join(localRoot, "README.md")); got != "sandbox after failure\n" {
		t.Fatalf("unexpected copied-out README after failure: got %q", got)
	}
	if strings.Contains(outcome.stdout, "write\t"+filepath.Join(resolvedLocalRoot, "README.md")+"\n") {
		t.Fatalf("copy-out plan should not be written to stdout, got %q", outcome.stdout)
	}
	if !strings.Contains(outcome.stderr, "failed\n") {
		t.Fatalf("expected command stderr, got %q", outcome.stderr)
	}
	if !strings.Contains(outcome.stderr, "write\t"+filepath.Join(resolvedLocalRoot, "README.md")+"\n") {
		t.Fatalf("expected copy-out plan in stderr, got %q", outcome.stderr)
	}
}

func TestExecIntegrationCopyOutFailurePreservesCreatedSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")

	adapter := &snapshotIntegrationAdapter{}
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
		command := strings.Join(req.Command, " ")
		if strings.Contains(command, "cleanroom-copy-out-v1") {
			return nil, errors.New("copy-out unavailable")
		}
		if stream.OnStdout != nil {
			stream.OnStdout([]byte("command ran\n"))
		}
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}
	host, _ := startIntegrationServer(t, adapter)

	outcome := runExecWithCapture(ExecCommand{
		clientFlags:        clientFlags{Host: host},
		workspaceCopyFlags: workspaceCopyFlags{CopyOut: true},
		Command:            []string{"echo", "ok"},
	}, runtimeContext{
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
	if !strings.Contains(outcome.stdout, "command ran\n") {
		t.Fatalf("expected command stdout, got %q", outcome.stdout)
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

func TestExecIntegrationCopyOutRejectsBaselineMismatchBeforeCommand(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	baseCommit := headCommit(t, localRoot)
	commitFile(t, localRoot, "local.txt", "local\n", "local commit")

	adapter := &integrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", baseCommit, "main")

	var executionCalled bool
	adapter.runStreamFn = func(_ context.Context, _ backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		executionCalled = true
		return &backend.ExecutionResult{ExitCode: 0, Message: "ok"}, nil
	}

	outcome := runExecWithCapture(ExecCommand{
		clientFlags:        clientFlags{Host: host},
		In:                 sandboxID,
		workspaceCopyFlags: workspaceCopyFlags{CopyOut: true},
		Command:            []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    localRoot,
		Loader: repositoryNotFoundLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected baseline mismatch to be rejected")
	}
	if !strings.Contains(outcome.err.Error(), "requires local checkout HEAD") {
		t.Fatalf("expected baseline mismatch error, got %v", outcome.err)
	}
	if executionCalled {
		t.Fatal("expected copy-out validation to fail before running command")
	}
}

func TestWorkspaceCopyOutPrevalidationAllowsSyncBaselineMismatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	localRoot := initGitRepository(t, "https://github.com/buildkite/cleanroom.git")
	baseCommit := headCommit(t, localRoot)
	commitFile(t, localRoot, "local.txt", "local\n", "local commit")

	host, _ := startIntegrationServer(t, &integrationAdapter{})
	sandboxID := createWorkspaceCopyTestSandboxWithRepositoryCommitBranch(t, host, "/sandbox-workspace", baseCommit, "main")
	client := mustNewControlClient(t, host)

	err := validateWorkspaceCopyOutBeforeExecution(context.Background(), &runtimeContext{
		CWD:    localRoot,
		Loader: repositoryNotFoundLoader{},
	}, client, localRoot, "", sandboxID, 0, workspaceCopyFlags{Sync: true})
	if err != nil {
		t.Fatalf("expected --sync prevalidation to allow copy-in to establish the copy-out base, got %v", err)
	}
}

func prepareReadmeWorkspaceCopyOutPayload(t *testing.T, localRoot, content string) (string, []byte) {
	t.Helper()

	baseCommit := headCommit(t, localRoot)
	if err := os.WriteFile(filepath.Join(localRoot, "README.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write sandbox readme: %v", err)
	}
	runGitInDir(t, localRoot, "add", "README.md")
	nameStatus := gitOutputBytes(t, localRoot, "diff", "--cached", "--name-status", "--no-renames", "-z", baseCommit)
	patch := gitOutputBytes(t, localRoot, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-color", "--no-renames", baseCommit)
	runGitInDir(t, localRoot, "reset", "--hard", baseCommit)
	return baseCommit, workspaceCopyOutTestPayload(nameStatus, patch)
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func newTestObservability(t *testing.T) *observability.Runtime {
	t.Helper()

	tracerProvider := sdktrace.NewTracerProvider()
	t.Cleanup(func() {
		_ = tracerProvider.Shutdown(context.Background())
	})

	obs, err := observability.NewWithTracerProvider(tracerProvider)
	if err != nil {
		t.Fatalf("NewWithTracerProvider returned error: %v", err)
	}
	return obs
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
	match := regexp.MustCompile(`sandbox_id="?([^"\s]+)"?`).FindStringSubmatch(stderr)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func parseExecutionID(stderr string) string {
	match := regexp.MustCompile(`execution_id="?([^"\s]+)"?`).FindStringSubmatch(stderr)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func parseTraceID(stderr string) string {
	match := regexp.MustCompile(`trace_id="?([^"\s]+)"?`).FindStringSubmatch(stderr)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func mustNewControlClient(t *testing.T, host string) *controlclient.Client {
	t.Helper()

	ep, err := endpoint.Resolve(host)
	if err != nil {
		t.Fatalf("resolve endpoint: %v", err)
	}
	client, err := controlclient.New(ep)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	return client
}

func mustCreateSandbox(t *testing.T, client *controlclient.Client) string {
	t.Helper()

	compiled, _, err := integrationLoader{}.LoadAndCompile(t.TempDir())
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	createSandboxResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{Policy: compiled.ToProto()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.GetSandbox().GetSandboxId()
	if sandboxID == "" {
		t.Fatal("expected sandbox id from CreateSandbox")
	}
	return sandboxID
}

func requireSandboxStatus(t *testing.T, client *controlclient.Client, sandboxID string, want cleanroomv1.SandboxStatus) {
	t.Helper()

	getResp, err := client.GetSandbox(context.Background(), &cleanroomv1.GetSandboxRequest{SandboxId: sandboxID})
	if err != nil {
		t.Fatalf("GetSandbox returned error: %v", err)
	}
	if got := getResp.GetSandbox().GetStatus(); got != want {
		t.Fatalf("unexpected sandbox status: got %v want %v", got, want)
	}
}

func TestExecIntegrationStreamsOutput(t *testing.T) {
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("one\n"))
			}
			time.Sleep(25 * time.Millisecond)
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("two\n"))
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
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Command:     []string{"echo", "ignored-by-adapter"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
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

func TestExecIntegrationDangerouslyAllowAllSetsAllowNetworkDefault(t *testing.T) {
	adapter := &snapshotIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()

	outcome := runExecWithCapture(ExecCommand{
		clientFlags:         clientFlags{Host: host},
		Chdir:               cwd,
		DangerouslyAllowAll: true,
		Command:             []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if adapter.provisionReq.Policy == nil {
		t.Fatal("expected provisioned policy")
	}
	if got, want := adapter.provisionReq.Policy.NetworkDefault, "allow"; got != want {
		t.Fatalf("unexpected provisioned network default: got %q want %q", got, want)
	}
}

func TestExecIntegrationForwardsStdinByDefault(t *testing.T) {
	var (
		mu          sync.Mutex
		stdinBuffer strings.Builder
	)
	stdinClosed := make(chan struct{}, 1)
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(data []byte) error {
						mu.Lock()
						_, _ = stdinBuffer.Write(data)
						mu.Unlock()
						return nil
					},
					CloseStdin: func() error {
						select {
						case stdinClosed <- struct{}{}:
						default:
						}
						return nil
					},
				})
			}
			select {
			case <-stdinClosed:
			case <-time.After(2 * time.Second):
				return nil, errors.New("timed out waiting for stdin close")
			}

			mu.Lock()
			gotStdin := stdinBuffer.String()
			mu.Unlock()
			output := "stdin:" + gotStdin
			if stream.OnStdout != nil {
				stream.OnStdout([]byte(output))
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
	stdinData := "hello from stdin\n"
	cmd := ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Command:     []string{"cat"},
	}
	outcome := runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, &stdinData, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if got, want := outcome.stdout, "stdin:"+stdinData; got != want {
		t.Fatalf("unexpected stdout: got %q want %q", got, want)
	}
	mu.Lock()
	gotStdin := stdinBuffer.String()
	mu.Unlock()
	if gotStdin != stdinData {
		t.Fatalf("unexpected stdin forwarded to backend: got %q want %q", gotStdin, stdinData)
	}
}

func TestExecIntegrationTTYForwardsStdinAndStreamsOutput(t *testing.T) {
	started := make(chan struct{}, 1)
	var captured strings.Builder
	adapter := &integrationAdapter{
		runStreamFn: func(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if !req.TTY {
				return nil, errors.New("expected tty execution")
			}

			done := make(chan struct{})
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(data []byte) error {
						_, _ = captured.Write(data)
						if stream.OnStdout != nil {
							stream.OnStdout(data)
						}
						if strings.Contains(captured.String(), "exit\n") {
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
	cmd := ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		TTY:         true,
		Command:     []string{"sh"},
	}
	stdinData := "hello\nexit\n"
	outcome := runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, &stdinData, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if got, want := captured.String(), stdinData; got != want {
		t.Fatalf("unexpected stdin forwarded to tty exec: got %q want %q", got, want)
	}
	if got, want := outcome.stdout, stdinData; got != want {
		t.Fatalf("unexpected tty exec stdout: got %q want %q", got, want)
	}
	_ = mustReceiveWithin(t, started, 2*time.Second, "timed out waiting for tty exec to start")
}

func TestExecIntegrationTTYSurfacesStdinAttachFailures(t *testing.T) {
	firstWrite := make(chan struct{}, 1)
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if !req.TTY {
				return nil, errors.New("expected tty execution")
			}
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(data []byte) error {
						select {
						case firstWrite <- struct{}{}:
						default:
						}
						return errors.New("use of closed network connection")
					},
					CloseStdin: func() error {
						return nil
					},
				})
			}
			select {
			case <-firstWrite:
			case <-time.After(2 * time.Second):
				return nil, errors.New("timed out waiting for stdin write")
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
	stdinData := "input\n"
	cmd := ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		TTY:         true,
		Command:     []string{"cat"},
	}
	outcome := runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, &stdinData, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected stdin attach error")
	}
	if !strings.Contains(outcome.err.Error(), "closed network connection") {
		t.Fatalf("expected surfaced interactive stdin failure, got %v", outcome.err)
	}
}

func TestExecIntegrationPassesResolvedEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "host-secret")

	var captured backend.ExecutionRequest
	adapter := &integrationAdapter{
		runFn: func(_ context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
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
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Env:         []string{"OPENAI_API_KEY", "DEBUG=1", "EMPTY="},
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if got, want := captured.Env, []string{"OPENAI_API_KEY=host-secret", "DEBUG=1", "EMPTY="}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected exec env: got %v want %v", got, want)
	}
}

func TestExecIntegrationIgnoresLateBenignStdinWriteFailures(t *testing.T) {
	firstWrite := make(chan struct{}, 1)
	secondWrite := make(chan struct{}, 1)
	var (
		mu         sync.Mutex
		stdinCalls int
	)
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(data []byte) error {
						mu.Lock()
						stdinCalls++
						call := stdinCalls
						mu.Unlock()
						if call == 1 {
							select {
							case firstWrite <- struct{}{}:
							default:
							}
							return nil
						}
						select {
						case secondWrite <- struct{}{}:
						default:
						}
						return errors.New("execution is not running")
					},
					CloseStdin: func() error {
						return nil
					},
				})
			}
			select {
			case <-firstWrite:
			case <-time.After(2 * time.Second):
				return nil, errors.New("timed out waiting for first stdin write")
			}
			select {
			case <-secondWrite:
			case <-time.After(2 * time.Second):
				return nil, errors.New("timed out waiting for second stdin write")
			}
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("done\n"))
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
	stdinData := strings.Repeat("x", 8192)
	cmd := ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Command:     []string{"cat"},
	}
	outcome := runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, &stdinData, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if got := outcome.stdout; got != "done\n" {
		t.Fatalf("unexpected stdout: got %q want %q", got, "done\n")
	}
	mu.Lock()
	gotCalls := stdinCalls
	mu.Unlock()
	if gotCalls < 2 {
		t.Fatalf("expected multiple stdin write attempts, got %d", gotCalls)
	}
}

func TestExecIntegrationTTYIgnoresLateBenignStdinWriteFailures(t *testing.T) {
	firstWrite := make(chan struct{}, 1)
	secondWrite := make(chan struct{}, 1)
	var (
		mu         sync.Mutex
		stdinCalls int
	)
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if !req.TTY {
				return nil, errors.New("expected tty execution")
			}
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(data []byte) error {
						mu.Lock()
						stdinCalls++
						call := stdinCalls
						mu.Unlock()
						if call == 1 {
							select {
							case firstWrite <- struct{}{}:
							default:
							}
							return nil
						}
						select {
						case secondWrite <- struct{}{}:
						default:
						}
						return errors.New("execution is not running")
					},
					CloseStdin: func() error {
						return nil
					},
				})
			}
			select {
			case <-firstWrite:
			case <-time.After(2 * time.Second):
				return nil, errors.New("timed out waiting for first stdin write")
			}
			select {
			case <-secondWrite:
			case <-time.After(2 * time.Second):
				return nil, errors.New("timed out waiting for second stdin write")
			}
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("done\n"))
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
	stdinData := strings.Repeat("x", 8192)
	cmd := ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		TTY:         true,
		Command:     []string{"cat"},
	}
	outcome := runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, &stdinData, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
}

func TestExecIntegrationNoStdinClosesImmediately(t *testing.T) {
	stdinWrites := make(chan string, 1)
	stdinClosed := make(chan struct{}, 1)
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(data []byte) error {
						select {
						case stdinWrites <- string(data):
						default:
						}
						return nil
					},
					CloseStdin: func() error {
						select {
						case stdinClosed <- struct{}{}:
						default:
						}
						return nil
					},
				})
			}
			select {
			case <-stdinClosed:
			case <-time.After(2 * time.Second):
				return nil, errors.New("timed out waiting for stdin close")
			}
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("closed\n"))
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
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		NoStdin:     true,
		Command:     []string{"cat"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if got := outcome.stdout; got != "closed\n" {
		t.Fatalf("unexpected stdout: got %q want %q", got, "closed\n")
	}
	select {
	case got := <-stdinWrites:
		t.Fatalf("expected no stdin writes, got %q", got)
	default:
	}
}

func TestExecIntegrationRejectsNoStdinWithTTY(t *testing.T) {
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		Chdir:   cwd,
		TTY:     true,
		NoStdin: true,
		Command: []string{"cat"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected exec --tty -n to fail")
	}
	if !strings.Contains(outcome.err.Error(), "--no-stdin cannot be used with --tty") {
		t.Fatalf("unexpected error: %v", outcome.err)
	}
}

func TestExecIntegrationIgnoresBenignStdinCloseFailures(t *testing.T) {
	stdinClosed := make(chan struct{}, 1)
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(data []byte) error {
						return nil
					},
					CloseStdin: func() error {
						select {
						case stdinClosed <- struct{}{}:
						default:
						}
						return errors.New("use of closed network connection")
					},
				})
			}
			select {
			case <-stdinClosed:
			case <-time.After(2 * time.Second):
				return nil, errors.New("timed out waiting for stdin close")
			}
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("done\n"))
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
	stdinData := ""
	cmd := ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Command:     []string{"cat"},
	}
	outcome := runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, &stdinData, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if got := outcome.stdout; got != "done\n" {
		t.Fatalf("unexpected stdout: got %q want %q", got, "done\n")
	}
}

func TestExecIntegrationPropagatesStdinWriteClosedConnectionFailures(t *testing.T) {
	firstWrite := make(chan struct{}, 1)
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func(data []byte) error {
						select {
						case firstWrite <- struct{}{}:
						default:
						}
						return errors.New("use of closed network connection")
					},
					CloseStdin: func() error {
						return nil
					},
				})
			}
			select {
			case <-firstWrite:
			case <-time.After(2 * time.Second):
				return nil, errors.New("timed out waiting for stdin write")
			}
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("done\n"))
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
	stdinData := "input\n"
	cmd := ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Command:     []string{"cat"},
	}
	outcome := runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, &stdinData, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected stdin write error")
	}
	if !strings.Contains(outcome.err.Error(), "write execution stdin") {
		t.Fatalf("expected stdin write error, got %v", outcome.err)
	}
}

func TestExecIntegrationNoStdinFailureDoesNotHangWhileStreamBlocked(t *testing.T) {
	started := make(chan struct{}, 1)
	adapter := &integrationAdapter{
		runFn: func(ctx context.Context, _ backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()

	done := make(chan execOutcome, 1)
	go func() {
		done <- runExecWithCapture(ExecCommand{
			clientFlags: clientFlags{Host: host},
			Chdir:       cwd,
			NoStdin:     true,
			Command:     []string{"cat"},
		}, runtimeContext{
			CWD:    cwd,
			Loader: integrationLoader{},
		})
	}()

	_ = mustReceiveWithin(t, started, 2*time.Second, "timed out waiting for execution to start")
	outcome := mustReceiveWithin(t, done, 5*time.Second, "timed out waiting for stdin close failure")

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected stdin close error")
	}
	if !strings.Contains(outcome.err.Error(), "execution stdin attach is not supported") {
		t.Fatalf("expected stdin attach error, got %v", outcome.err)
	}
}

func TestExecIntegrationPropagatesExitWithoutFailureFooter(t *testing.T) {
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("out\n"))
			}
			if stream.OnStderr != nil {
				stream.OnStderr([]byte("err\n"))
			}
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    7,
				RunDir:      "/tmp/exec-failed",
				Message:     "failed",
			}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Command:     []string{"echo", "ignored-by-adapter"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
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
	if strings.Contains(outcome.stderr, "sandbox_id=") {
		t.Fatalf("did not expect sandbox_id footer in failure output, got %q", outcome.stderr)
	}
	if strings.Contains(outcome.stderr, "execution_id=") {
		t.Fatalf("did not expect execution_id footer in failure output, got %q", outcome.stderr)
	}
	if strings.Contains(outcome.stderr, "inspect_command=cleanroom execution inspect ") {
		t.Fatalf("did not expect inspect_command footer in failure output, got %q", outcome.stderr)
	}
	if strings.Contains(outcome.stderr, "artifacts_dir=/tmp/exec-failed") {
		t.Fatalf("did not expect artifacts_dir footer in failure output, got %q", outcome.stderr)
	}
}

func TestExecIntegrationRendersStructuredWarnings(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")

	const warningText = "darwin-vz guest networking is enabled without host-side egress filtering"

	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if stream.OnWarning != nil {
				stream.OnWarning(warningText)
			}
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("hello world\n"))
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
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Command:     []string{"echo", "ignored-by-adapter"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})

	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if !strings.Contains(stripANSI(outcome.stderr), "warning: "+warningText) {
		t.Fatalf("expected structured warning in stderr, got %q", stripANSI(outcome.stderr))
	}
	if !strings.Contains(outcome.stderr, "\x1b[") {
		t.Fatalf("expected ANSI styling in warning output, got %q", outcome.stderr)
	}
	if !strings.Contains(outcome.stdout, "hello world\n") {
		t.Fatalf("expected stdout output, got %q", outcome.stdout)
	}
	if strings.Contains(outcome.stdout, warningText) {
		t.Fatalf("expected warning to stay out of stdout, got %q", outcome.stdout)
	}
}

func TestExecIntegrationFirstInterruptCancelsExecution(t *testing.T) {
	started := make(chan struct{}, 1)
	adapter := &integrationAdapter{
		runFn: func(ctx context.Context, _ backend.ExecutionRequest) (*backend.ExecutionResult, error) {
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
			clientFlags: clientFlags{Host: host},
			Chdir:       cwd,
			Command:     []string{"sleep", "300"},
		}, runtimeContext{
			CWD:    cwd,
			Loader: integrationLoader{},
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

func TestExecIntegrationSecondInterruptTerminatesSandboxWithRemove(t *testing.T) {
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
		runFn: func(ctx context.Context, _ backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			defer close(runReturned)
			select {
			case started <- struct{}{}:
			default:
			}
			<-releaseRun
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return &backend.ExecutionResult{ExitCode: 0, Message: "unexpected success"}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	signalCh := withTestSignalChannel(t)
	cwd := t.TempDir()

	done := make(chan execOutcome, 1)
	go func() {
		done <- runExecWithCapture(ExecCommand{
			clientFlags:    clientFlags{Host: host},
			Chdir:          cwd,
			PrintSandboxID: true,
			Command:        []string{"sleep", "300"},
		}, runtimeContext{
			CWD:    cwd,
			Loader: integrationLoader{},
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

	client := mustNewControlClient(t, host)
	requireSandboxStatus(t, client, sandboxID, cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED)

	release()
	_ = mustReceiveWithin(t, runReturned, 2*time.Second, "timed out waiting for adapter run to return after release")
}

func TestExecIntegrationSecondInterruptKeepsSuppliedSandboxWithoutRemove(t *testing.T) {
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
		runFn: func(ctx context.Context, _ backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			defer close(runReturned)
			select {
			case started <- struct{}{}:
			default:
			}
			<-releaseRun
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return &backend.ExecutionResult{ExitCode: 0, Message: "unexpected success"}, nil
		},
	}

	host, _ := startIntegrationServer(t, adapter)
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	signalCh := withTestSignalChannel(t)
	cwd := t.TempDir()
	done := make(chan execOutcome, 1)
	go func() {
		done <- runExecWithCapture(ExecCommand{
			clientFlags: clientFlags{Host: host},
			In:          sandboxID,
			Command:     []string{"sleep", "300"},
		}, runtimeContext{
			CWD:    cwd,
			Loader: failingLoader{},
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

	requireSandboxStatus(t, client, sandboxID, cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY)

	release()
	_ = mustReceiveWithin(t, runReturned, 2*time.Second, "timed out waiting for adapter run to return after release")
}

func TestExecIntegrationTTYSecondInterruptTerminatesSandboxWithRemove(t *testing.T) {
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
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if !req.TTY {
				return nil, errors.New("expected tty execution")
			}
			if stream.OnAttach != nil {
				stream.OnAttach(backend.AttachIO{
					WriteStdin: func([]byte) error { return nil },
					CloseStdin: func() error { return nil },
				})
			}
			defer close(runReturned)
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
		done <- runExecWithCapture(ExecCommand{
			clientFlags:    clientFlags{Host: host},
			Chdir:          cwd,
			TTY:            true,
			PrintSandboxID: true,
			Command:        []string{"sleep", "300"},
		}, runtimeContext{
			CWD:    cwd,
			Loader: integrationLoader{},
		})
	}()

	_ = mustReceiveWithin(t, started, 2*time.Second, "timed out waiting for tty execution to start")
	signalCh <- os.Interrupt
	signalCh <- os.Interrupt

	outcome := mustReceiveWithin(t, done, 2*time.Second, "timed out waiting for second-interrupt tty exec exit")
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

	client := mustNewControlClient(t, host)
	requireSandboxStatus(t, client, sandboxID, cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED)

	release()
	_ = mustReceiveWithin(t, runReturned, 2*time.Second, "timed out waiting for adapter run to return after release")
}

func TestExecIntegrationVmPathUsesShForGuestCompatibility(t *testing.T) {
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
			if len(req.Command) >= 1 && req.Command[0] == "bash" {
				return nil, errors.New(`exec: "bash": executable file not found in $PATH`)
			}
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("guest-ok\n"))
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
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Command:     []string{"sh", "-lc", "echo guest-ok"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
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
	in := "sandbox_id=cr_123\n"
	if got, want := parseSandboxID(in), "cr_123"; got != want {
		t.Fatalf("unexpected sandbox id from print output: got %q want %q", got, want)
	}
	if got := parseSandboxID("no id here"); got != "" {
		t.Fatalf("expected empty sandbox id for invalid input, got %q", got)
	}
}

func TestParseExecutionID(t *testing.T) {
	in := "execution_id=exec_456\n"
	if got, want := parseExecutionID(in), "exec_456"; got != want {
		t.Fatalf("unexpected execution id from print output: got %q want %q", got, want)
	}
	if got := parseExecutionID("no id here"); got != "" {
		t.Fatalf("expected empty execution id for invalid input, got %q", got)
	}
}

func TestExecIntegrationPrintSandboxIDFlag(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		clientFlags:    clientFlags{Host: host},
		Chdir:          cwd,
		PrintSandboxID: true,
		Command:        []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	sandboxID := parseSandboxID(outcome.stderr)
	if sandboxID == "" {
		t.Fatalf("missing sandbox_id in stderr output: %q", outcome.stderr)
	}
	if strings.Contains(outcome.stdout, "sandbox_id=") {
		t.Fatalf("sandbox_id marker must not be written to stdout: %q", outcome.stdout)
	}
}

func TestExecIntegrationPrintTraceIDFlag(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		clientFlags:  clientFlags{Host: host},
		Chdir:        cwd,
		PrintTraceID: true,
		Command:      []string{"echo", "ok"},
	}, runtimeContext{
		CWD:           cwd,
		Loader:        integrationLoader{},
		Observability: newTestObservability(t),
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	traceID := parseTraceID(outcome.stderr)
	if traceID == "" {
		t.Fatalf("missing trace_id in stderr output: %q", outcome.stderr)
	}
	if got, want := strings.Count(outcome.stderr, "trace_id="), 1; got != want {
		t.Fatalf("expected one trace_id marker in stderr, got %d: %q", got, outcome.stderr)
	}
	if strings.Contains(outcome.stdout, "trace_id=") {
		t.Fatalf("trace_id marker must not be written to stdout: %q", outcome.stdout)
	}
	if !strings.HasSuffix(outcome.stderr, "trace_id="+traceID+"\n") {
		t.Fatalf("expected trace_id to be the final stderr line, got %q", outcome.stderr)
	}
}

func TestExecIntegrationOmitsTraceIDWithoutFlag(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD:           cwd,
		Loader:        integrationLoader{},
		Observability: newTestObservability(t),
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if strings.Contains(outcome.stderr, "trace_id=") {
		t.Fatalf("trace_id marker must not be printed without --print-trace-id: %q", outcome.stderr)
	}
}

func TestExecIntegrationKeepWithPrintSandboxIDPrintsOnce(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		clientFlags:    clientFlags{Host: host},
		Chdir:          cwd,
		Keep:           true,
		PrintSandboxID: true,
		Command:        []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if got, want := strings.Count(outcome.stderr, "sandbox_id="), 1; got != want {
		t.Fatalf("expected one sandbox_id marker in stderr, got %d: %q", got, outcome.stderr)
	}
}

func TestExecIntegrationDefaultTerminatesCreatedSandbox(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		clientFlags:    clientFlags{Host: host},
		Chdir:          cwd,
		PrintSandboxID: true,
		Command:        []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}

	sandboxID := parseSandboxID(outcome.stderr)
	if sandboxID == "" {
		t.Fatalf("missing sandbox_id in stderr output: %q", outcome.stderr)
	}

	client := mustNewControlClient(t, host)
	requireSandboxStatus(t, client, sandboxID, cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED)
}

func TestExecIntegrationKeepPreservesCreatedSandbox(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Keep:        true,
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}

	sandboxID := parseSandboxID(outcome.stderr)
	if sandboxID == "" {
		t.Fatalf("missing sandbox_id in stderr output: %q", outcome.stderr)
	}

	client := mustNewControlClient(t, host)
	requireSandboxStatus(t, client, sandboxID, cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY)
}

func TestExecIntegrationUsesCreateExecDestroyLifecycle(t *testing.T) {
	adapter := &snapshotIntegrationAdapter{}
	adapter.runStreamFn = func(_ context.Context, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
		return &backend.ExecutionResult{ExecutionID: req.ExecutionID, ExitCode: 0, Message: "ok"}, nil
	}
	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()

	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}
	if got, want := adapter.provisionCalls, 1; got != want {
		t.Fatalf("expected one sandbox provision, got %d", got)
	}
	if got, want := adapter.runInSandboxCalls, 1; got != want {
		t.Fatalf("expected one sandbox execution, got %d", got)
	}
	if got, want := adapter.terminateCalls, 1; got != want {
		t.Fatalf("expected one sandbox termination, got %d", got)
	}
	if got, want := adapter.runCalls, 1; got != want {
		t.Fatalf("expected one unified backend Run call, got %d", got)
	}
	if got := strings.TrimSpace(adapter.provisionReq.SandboxID); got == "" {
		t.Fatal("expected provisioned sandbox id to be recorded")
	}
	if got, want := adapter.runReq.SandboxID, adapter.provisionReq.SandboxID; got != want {
		t.Fatalf("expected execution to target provisioned sandbox: got %q want %q", got, want)
	}
}

func TestExecIntegrationTerminatesCreatedSandboxWhenExposureSetupFails(t *testing.T) {
	adapter := &snapshotIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()

	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Expose:      []string{"3000"},
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected ExecCommand.Run to fail during exposure setup")
	}
	if !strings.Contains(outcome.err.Error(), "does not support sandbox port dialing") {
		t.Fatalf("unexpected ExecCommand.Run error: %v", outcome.err)
	}

	adapter.mu.Lock()
	sandboxID := adapter.provisionReq.SandboxID
	terminateCalls := adapter.terminateCalls
	runInSandboxCalls := adapter.runInSandboxCalls
	adapter.mu.Unlock()
	if sandboxID == "" {
		t.Fatal("expected sandbox to be provisioned before exposure setup")
	}
	if got, want := terminateCalls, 1; got != want {
		t.Fatalf("expected created sandbox to be terminated after exposure setup failure: got %d want %d", got, want)
	}
	if got, want := runInSandboxCalls, 0; got != want {
		t.Fatalf("expected command not to start after exposure setup failure: got %d want %d", got, want)
	}

	client := mustNewControlClient(t, host)
	requireSandboxStatus(t, client, sandboxID, cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED)
}

func TestExecIntegrationRejectsKeepWhenReusingSandbox(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		In:          sandboxID,
		Keep:        true,
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected ExecCommand.Run to reject --keep with --in")
	}
	if got, want := outcome.err.Error(), "--keep cannot be used with --in"; !strings.Contains(got, want) {
		t.Fatalf("expected error to contain %q, got %q", want, got)
	}
}

func TestExecIntegrationRejectsChdirWhenReusingSandbox(t *testing.T) {
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: "tssvc://cleanroom"},
		Chdir:       cwd,
		In:          "cr_123",
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected ExecCommand.Run to reject --chdir with --in")
	}
	if got, want := outcome.err.Error(), "--chdir cannot be used with --in"; !strings.Contains(got, want) {
		t.Fatalf("expected error to contain %q, got %q", want, got)
	}
}

func TestExecIntegrationRejectsChdirWhenCopyingOutFromReusedSandbox(t *testing.T) {
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: "tssvc://cleanroom"},
		Chdir:       cwd,
		In:          "cr_123",
		workspaceCopyFlags: workspaceCopyFlags{
			CopyOut: true,
		},
		Command: []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected ExecCommand.Run to reject --chdir with --in --copy-out")
	}
	if got, want := outcome.err.Error(), "--chdir cannot be used with --in"; !strings.Contains(got, want) {
		t.Fatalf("expected error to contain %q, got %q", want, got)
	}
}

func TestExecIntegrationRejectsChdirWhenCreatingFromSnapshot(t *testing.T) {
	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: "tssvc://cleanroom"},
		Chdir:       cwd,
		From:        "snap_123",
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected ExecCommand.Run to reject --chdir with --from")
	}
	if got, want := outcome.err.Error(), "--chdir cannot be used with --from"; !strings.Contains(got, want) {
		t.Fatalf("expected error to contain %q, got %q", want, got)
	}
}

func TestExecIntegrationReuseSandboxSkipsPolicyCompile(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		In:          sandboxID,
		Command:     []string{"echo", "ok"},
	}, runtimeContext{
		CWD:    t.TempDir(),
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecCommand.Run returned error: %v", outcome.err)
	}

	requireSandboxStatus(t, client, sandboxID, cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY)
}

func TestExecRejectsUnsupportedHostScheme(t *testing.T) {
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: "tssvc://cleanroom"},
		Command:     []string{"echo", "hi"},
	}, runtimeContext{
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
