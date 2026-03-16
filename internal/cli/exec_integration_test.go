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
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
)

type integrationAdapter struct {
	mu sync.Mutex

	runFn                    func(context.Context, backend.RunRequest) (*backend.RunResult, error)
	runStreamFn              func(context.Context, backend.RunRequest, backend.OutputStream) (*backend.RunResult, error)
	provisionFn              func(context.Context, backend.ProvisionRequest) error
	provisionFromSnapshotFn  func(context.Context, backend.ProvisionFromSnapshotRequest) error
	createSnapshotFn         func(context.Context, backend.SnapshotRequest) (*backend.SnapshotResult, error)
	deleteSnapshotFn         func(context.Context, backend.DeleteSnapshotRequest) error
	terminateFn              func(context.Context, string) error
	provisionReq             backend.ProvisionRequest
	provisionFromSnapshotReq backend.ProvisionFromSnapshotRequest
	createSnapshotReq        backend.SnapshotRequest
	deleteSnapshotReq        backend.DeleteSnapshotRequest
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

type snapshotIntegrationAdapter struct {
	integrationAdapter
}

func (a *snapshotIntegrationAdapter) ProvisionSandbox(ctx context.Context, req backend.ProvisionRequest) error {
	a.mu.Lock()
	a.provisionReq = req
	fn := a.provisionFn
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return nil
}

func (a *snapshotIntegrationAdapter) RunInSandbox(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
	return a.RunStream(ctx, req, stream)
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
	fn := a.terminateFn
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, sandboxID)
	}
	return nil
}

type integrationLoader struct{}

func (integrationLoader) LoadAndCompile(_ string) (*policy.CompiledPolicy, string, error) {
	return &policy.CompiledPolicy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
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

	httpServer := httptest.NewServer(controlserver.New(svc, nil).Handler())
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
	server := &http.Server{Handler: controlserver.New(svc, nil).Handler()}
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

func TestExecIntegrationForwardsStdinByDefault(t *testing.T) {
	var (
		mu          sync.Mutex
		stdinBuffer strings.Builder
	)
	stdinClosed := make(chan struct{}, 1)
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
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
			return &backend.RunResult{
				RunID:    req.RunID,
				ExitCode: 0,
				Stdout:   output,
				Message:  "ok",
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

func TestExecIntegrationNoStdinClosesImmediately(t *testing.T) {
	stdinWrites := make(chan string, 1)
	stdinClosed := make(chan struct{}, 1)
	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
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
			return &backend.RunResult{
				RunID:    req.RunID,
				ExitCode: 0,
				Stdout:   "closed\n",
				Message:  "ok",
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

func TestExecIntegrationNoStdinFailureDoesNotHangWhileStreamBlocked(t *testing.T) {
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
}

func TestExecIntegrationRendersStructuredWarnings(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")

	const warningText = "darwin-vz guest networking is enabled without host-side egress filtering"

	adapter := &integrationAdapter{
		runStreamFn: func(_ context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
			if stream.OnWarning != nil {
				stream.OnWarning(warningText)
			}
			if stream.OnStdout != nil {
				stream.OnStdout([]byte("hello world\n"))
			}
			return &backend.RunResult{
				RunID:    req.RunID,
				ExitCode: 0,
				Stdout:   "hello world\n",
				Message:  "ok",
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
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	signalCh := withTestSignalChannel(t)
	cwd := t.TempDir()
	done := make(chan execOutcome, 1)
	go func() {
		done <- runExecWithCapture(ExecCommand{
			clientFlags: clientFlags{Host: host, LogLevel: "debug"},
			Chdir:       cwd,
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
	in := "DEBU execution started component=client sandbox_id=cr-123 execution_id=exec-456\n"
	if got, want := parseSandboxID(in), "cr-123"; got != want {
		t.Fatalf("unexpected sandbox id: got %q want %q", got, want)
	}
	in = "sandbox_id=cr_123\n"
	if got, want := parseSandboxID(in), "cr_123"; got != want {
		t.Fatalf("unexpected sandbox id from print output: got %q want %q", got, want)
	}
	if got := parseSandboxID("no id here"); got != "" {
		t.Fatalf("expected empty sandbox id for invalid input, got %q", got)
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

func TestExecIntegrationRejectsKeepWhenReusingSandbox(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	cwd := t.TempDir()
	outcome := runExecWithCapture(ExecCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
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
