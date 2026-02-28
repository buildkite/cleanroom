package client

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/gen/cleanroom/v1/cleanroomv1connect"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type blockingPersistentAdapter struct {
	integrationAdapter
	provisionEntered chan struct{}
	provisionRelease chan struct{}
	terminateEntered chan struct{}
	terminateRelease chan struct{}
	provisionCalls   atomic.Int32
}

func (a *blockingPersistentAdapter) ProvisionSandbox(context.Context, backend.ProvisionRequest) error {
	a.provisionCalls.Add(1)
	if a.provisionEntered != nil {
		a.provisionEntered <- struct{}{}
	}
	if a.provisionRelease != nil {
		<-a.provisionRelease
	}
	return nil
}

func (a *blockingPersistentAdapter) RunInSandbox(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
	return a.RunStream(ctx, req, stream)
}

func (a *blockingPersistentAdapter) TerminateSandbox(context.Context, string) error {
	if a.terminateEntered != nil {
		a.terminateEntered <- struct{}{}
	}
	if a.terminateRelease != nil {
		<-a.terminateRelease
	}
	return nil
}

type cancelAwareStreamingAdapter struct {
	integrationAdapter
	runEntered  chan struct{}
	runCanceled chan struct{}
}

func (a *cancelAwareStreamingAdapter) RunStream(ctx context.Context, req backend.RunRequest, stream backend.OutputStream) (*backend.RunResult, error) {
	if a.runEntered != nil {
		a.runEntered <- struct{}{}
	}
	<-ctx.Done()
	if a.runCanceled != nil {
		a.runCanceled <- struct{}{}
	}
	return nil, ctx.Err()
}

type immediateStreamingAdapter struct {
	integrationAdapter
}

func (a *immediateStreamingAdapter) RunStream(context.Context, backend.RunRequest, backend.OutputStream) (*backend.RunResult, error) {
	return &backend.RunResult{
		ExitCode: 0,
		Message:  "done",
	}, nil
}

type integrationExecutionHooks struct {
	beforeStreamExecution func(context.Context, *StreamExecutionRequest) error
	overrideStream        func(context.Context, *connect.Request[StreamExecutionRequest], *connect.ServerStream[ExecutionStreamEvent]) error
	beforeGetExecution    func(context.Context, *GetExecutionRequest) error
	onCancelExecution     func(*CancelExecutionRequest)
}

type integrationHookedServer struct {
	*controlserver.Server
	hooks integrationExecutionHooks
}

func (s *integrationHookedServer) StreamExecution(ctx context.Context, req *connect.Request[StreamExecutionRequest], stream *connect.ServerStream[ExecutionStreamEvent]) error {
	if s.hooks.overrideStream != nil {
		return s.hooks.overrideStream(ctx, req, stream)
	}
	if s.hooks.beforeStreamExecution != nil {
		if err := s.hooks.beforeStreamExecution(ctx, req.Msg); err != nil {
			return err
		}
	}
	return s.Server.StreamExecution(ctx, req, stream)
}

func (s *integrationHookedServer) GetExecution(ctx context.Context, req *connect.Request[GetExecutionRequest]) (*connect.Response[GetExecutionResponse], error) {
	if s.hooks.beforeGetExecution != nil {
		if err := s.hooks.beforeGetExecution(ctx, req.Msg); err != nil {
			return nil, err
		}
	}
	return s.Server.GetExecution(ctx, req)
}

func (s *integrationHookedServer) CancelExecution(ctx context.Context, req *connect.Request[CancelExecutionRequest]) (*connect.Response[CancelExecutionResponse], error) {
	if s.hooks.onCancelExecution != nil && req != nil && req.Msg != nil {
		s.hooks.onCancelExecution(req.Msg)
	}
	return s.Server.CancelExecution(ctx, req)
}

func startIntegrationServerWithAdapter(t *testing.T, adapter backend.Adapter) string {
	return startIntegrationServerWithAdapterAndExecutionHooks(t, adapter, integrationExecutionHooks{})
}

func startIntegrationServerWithAdapterAndExecutionHooks(t *testing.T, adapter backend.Adapter, hooks integrationExecutionHooks) string {
	t.Helper()
	return startIntegrationServerWithBackendsAndExecutionHooks(t, map[string]backend.Adapter{"firecracker": adapter}, hooks)
}

func startIntegrationServerWithBackendsAndExecutionHooks(t *testing.T, backends map[string]backend.Adapter, hooks integrationExecutionHooks) string {
	t.Helper()

	if len(backends) == 0 {
		t.Fatal("expected at least one backend")
	}
	svc := &controlservice.Service{
		Config:   runtimeconfig.Config{DefaultBackend: "firecracker"},
		Backends: backends,
	}

	server := &integrationHookedServer{
		Server: controlserver.New(svc, nil),
		hooks:  hooks,
	}
	mux := http.NewServeMux()
	sandboxPath, sandboxHandler := cleanroomv1connect.NewSandboxServiceHandler(server)
	executionPath, executionHandler := cleanroomv1connect.NewExecutionServiceHandler(server)
	mux.Handle(sandboxPath, sandboxHandler)
	mux.Handle(executionPath, executionHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	handler := h2c.NewHandler(mux, &http2.Server{})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer.URL
}

func TestNewFromEnvUsesCLEANROOMHOST(t *testing.T) {
	host := startIntegrationServer(t)
	t.Setenv("CLEANROOM_HOST", host)

	c, err := NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv returned error: %v", err)
	}

	resp, err := c.ListSandboxes(context.Background(), &ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("ListSandboxes returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response")
	}
}

func TestMustPanicsOnError(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
	}()

	_ = Must((*Client)(nil), errors.New("boom"))
}

func TestPolicyFromAllowlist(t *testing.T) {
	policy := PolicyFromAllowlist(
		"ghcr.io/buildkite/cleanroom-base/alpine@sha256:abc",
		"sha256:abc",
		Allow("api.github.com", 443),
		Allow("registry.npmjs.org", 443, 80),
	)

	if policy.GetVersion() != 1 {
		t.Fatalf("unexpected version: got %d", policy.GetVersion())
	}
	if policy.GetNetworkDefault() != "deny" {
		t.Fatalf("unexpected network default: got %q", policy.GetNetworkDefault())
	}
	if len(policy.GetAllow()) != 2 {
		t.Fatalf("unexpected allow length: got %d", len(policy.GetAllow()))
	}
	if policy.GetAllow()[0].GetHost() != "api.github.com" {
		t.Fatalf("unexpected first allow host: %q", policy.GetAllow()[0].GetHost())
	}
}

func TestPolicyFromAllowlistSkipsEntriesWithoutPorts(t *testing.T) {
	policy := PolicyFromAllowlist(
		"ghcr.io/buildkite/cleanroom-base/alpine@sha256:abc",
		"sha256:abc",
		Allow("api.github.com"),
		Allow("registry.npmjs.org", 443),
	)

	if got := len(policy.GetAllow()); got != 1 {
		t.Fatalf("expected only one valid allow entry, got %d", got)
	}
	if got := policy.GetAllow()[0].GetHost(); got != "registry.npmjs.org" {
		t.Fatalf("unexpected allow entry host: got %q", got)
	}
	if got := policy.GetAllow()[0].GetPorts(); len(got) != 1 || got[0] != 443 {
		t.Fatalf("unexpected allow ports: %#v", got)
	}
}

func TestEnsureSandboxReusesKey(t *testing.T) {
	host := startIntegrationServer(t)
	c, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opts := EnsureSandboxOptions{
		Backend: "firecracker",
		Policy:  testPolicy(),
	}

	first, err := c.EnsureSandbox(ctx, "thread:abc", opts)
	if err != nil {
		t.Fatalf("first EnsureSandbox returned error: %v", err)
	}
	if !first.Created {
		t.Fatal("expected first call to create sandbox")
	}

	second, err := c.EnsureSandbox(ctx, "thread:abc", opts)
	if err != nil {
		t.Fatalf("second EnsureSandbox returned error: %v", err)
	}
	if second.Created {
		t.Fatal("expected second call to reuse sandbox")
	}
	if first.ID != second.ID {
		t.Fatalf("expected same sandbox id, got %q and %q", first.ID, second.ID)
	}
}

func TestEnsureSandboxBackendChangeCreatesNewSandbox(t *testing.T) {
	host := startIntegrationServerWithBackendsAndExecutionHooks(t, map[string]backend.Adapter{
		"firecracker": integrationAdapter{},
		"darwin-vz":   integrationAdapter{},
	}, integrationExecutionHooks{})
	c, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx := context.Background()
	first, err := c.EnsureSandbox(ctx, "thread:backend-switch", EnsureSandboxOptions{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("first EnsureSandbox returned error: %v", err)
	}
	if !first.Created {
		t.Fatal("expected first ensure call to create sandbox")
	}

	second, err := c.EnsureSandbox(ctx, "thread:backend-switch", EnsureSandboxOptions{
		Backend: "darwin-vz",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("second EnsureSandbox returned error: %v", err)
	}
	if !second.Created {
		t.Fatal("expected backend change to create a new sandbox")
	}
	if second.ID == first.ID {
		t.Fatalf("expected backend change to return a different sandbox id, got %q", second.ID)
	}
	if second.Backend != "darwin-vz" {
		t.Fatalf("expected replacement sandbox backend to be darwin-vz, got %q", second.Backend)
	}
}

func TestEnsureSandboxReplacesTerminalSandbox(t *testing.T) {
	host := startIntegrationServer(t)
	c, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx := context.Background()
	first, err := c.EnsureSandbox(ctx, "thread:terminal", EnsureSandboxOptions{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("first EnsureSandbox returned error: %v", err)
	}
	if _, err := c.TerminateSandbox(ctx, &TerminateSandboxRequest{SandboxId: first.ID}); err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}

	second, err := c.EnsureSandbox(ctx, "thread:terminal", EnsureSandboxOptions{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("second EnsureSandbox returned error: %v", err)
	}
	if second == nil {
		t.Fatal("expected second sandbox handle")
	}
	if !second.Created {
		t.Fatal("expected terminal sandbox to be replaced with a newly created sandbox")
	}
	if second.ID == first.ID {
		t.Fatalf("expected replacement sandbox id to differ: got %q", second.ID)
	}
}

func TestEnsureSandboxSerializesConcurrentCreatesByKey(t *testing.T) {
	adapter := &blockingPersistentAdapter{
		provisionEntered: make(chan struct{}, 2),
		provisionRelease: make(chan struct{}),
	}
	host := startIntegrationServerWithAdapter(t, adapter)

	c, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	type ensureResult struct {
		handle *SandboxHandle
		err    error
	}
	firstDone := make(chan ensureResult, 1)
	secondDone := make(chan ensureResult, 1)

	go func() {
		handle, runErr := c.EnsureSandbox(context.Background(), "thread:concurrent", EnsureSandboxOptions{
			Backend: "firecracker",
			Policy:  testPolicy(),
		})
		firstDone <- ensureResult{handle: handle, err: runErr}
	}()

	select {
	case <-adapter.provisionEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first EnsureSandbox did not reach provisioning")
	}

	go func() {
		handle, runErr := c.EnsureSandbox(context.Background(), "thread:concurrent", EnsureSandboxOptions{
			Backend: "firecracker",
			Policy:  testPolicy(),
		})
		secondDone <- ensureResult{handle: handle, err: runErr}
	}()

	select {
	case <-adapter.provisionEntered:
		t.Fatal("second EnsureSandbox should not start provisioning while first is in-flight")
	case <-time.After(250 * time.Millisecond):
		// expected: second call waits for first to complete and map the key
	}

	close(adapter.provisionRelease)

	first := <-firstDone
	if first.err != nil {
		t.Fatalf("first EnsureSandbox returned error: %v", first.err)
	}
	second := <-secondDone
	if second.err != nil {
		t.Fatalf("second EnsureSandbox returned error: %v", second.err)
	}
	if first.handle == nil || second.handle == nil {
		t.Fatal("expected both EnsureSandbox calls to return handles")
	}
	if first.handle.ID != second.handle.ID {
		t.Fatalf("expected concurrent calls to reuse one sandbox id, got %q and %q", first.handle.ID, second.handle.ID)
	}
	if got := adapter.provisionCalls.Load(); got != 1 {
		t.Fatalf("expected exactly one provision call, got %d", got)
	}
}

func TestEnsureSandboxKeyLockHonorsContextCancellation(t *testing.T) {
	adapter := &blockingPersistentAdapter{
		provisionEntered: make(chan struct{}, 1),
		provisionRelease: make(chan struct{}),
	}
	host := startIntegrationServerWithAdapter(t, adapter)

	c, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, runErr := c.EnsureSandbox(context.Background(), "thread:ctx-lock", EnsureSandboxOptions{
			Backend: "firecracker",
			Policy:  testPolicy(),
		})
		firstDone <- runErr
	}()

	select {
	case <-adapter.provisionEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first EnsureSandbox did not reach provisioning")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, secondErr := c.EnsureSandbox(waitCtx, "thread:ctx-lock", EnsureSandboxOptions{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if secondErr == nil {
		t.Fatal("expected second EnsureSandbox to fail on context timeout while lock held")
	}
	if !errors.Is(secondErr, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", secondErr)
	}
	if elapsed := time.Since(started); elapsed > 750*time.Millisecond {
		t.Fatalf("EnsureSandbox lock wait ignored context timeout: elapsed=%s", elapsed)
	}
	if got := adapter.provisionCalls.Load(); got != 1 {
		t.Fatalf("expected only first call to reach provisioning while second timed out, got %d", got)
	}

	close(adapter.provisionRelease)
	if err := <-firstDone; err != nil {
		t.Fatalf("first EnsureSandbox returned error: %v", err)
	}
}

func TestEnsureSandboxReplacesStoppingSandbox(t *testing.T) {
	adapter := &blockingPersistentAdapter{
		terminateEntered: make(chan struct{}, 1),
		terminateRelease: make(chan struct{}),
	}
	host := startIntegrationServerWithAdapter(t, adapter)

	c, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	ctx := context.Background()

	first, err := c.EnsureSandbox(ctx, "thread:stopping", EnsureSandboxOptions{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("first EnsureSandbox returned error: %v", err)
	}

	terminateDone := make(chan error, 1)
	go func() {
		_, terminateErr := c.TerminateSandbox(ctx, &TerminateSandboxRequest{SandboxId: first.ID})
		terminateDone <- terminateErr
	}()

	select {
	case <-adapter.terminateEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("TerminateSandbox did not reach backend terminate")
	}

	second, err := c.EnsureSandbox(ctx, "thread:stopping", EnsureSandboxOptions{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("EnsureSandbox while stopping returned error: %v", err)
	}
	if second == nil || !second.Created {
		t.Fatal("expected a newly created replacement sandbox while cached sandbox is stopping")
	}
	if second.ID == first.ID {
		t.Fatalf("expected replacement sandbox id to differ from stopping sandbox id %q", first.ID)
	}

	close(adapter.terminateRelease)
	if err := <-terminateDone; err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}
}

func TestEnsureSandboxAcceptsExplicitSandboxID(t *testing.T) {
	host := startIntegrationServer(t)
	c, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx := context.Background()
	created, err := c.CreateSandbox(ctx, &CreateSandboxRequest{Backend: "firecracker", Policy: testPolicy()})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	id := created.GetSandbox().GetSandboxId()

	ensured, err := c.EnsureSandbox(ctx, "thread:explicit", EnsureSandboxOptions{SandboxID: id})
	if err != nil {
		t.Fatalf("EnsureSandbox returned error: %v", err)
	}
	if ensured.ID != id {
		t.Fatalf("sandbox id mismatch: got %q want %q", ensured.ID, id)
	}
	if ensured.Created {
		t.Fatal("expected ensured sandbox to be marked reused")
	}
}

func TestExecAndWait(t *testing.T) {
	host := startIntegrationServer(t)
	c, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sb, err := c.EnsureSandbox(ctx, "thread:exec", EnsureSandboxOptions{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("EnsureSandbox returned error: %v", err)
	}

	var stdout bytes.Buffer
	result, err := c.ExecAndWait(ctx, sb.ID, []string{"echo", "hello"}, ExecOptions{Stdout: &stdout})
	if err != nil {
		t.Fatalf("ExecAndWait returned error: %v", err)
	}
	if result.Status != ExecutionStatus_EXECUTION_STATUS_SUCCEEDED {
		t.Fatalf("unexpected status: %v", result.Status)
	}
	if result.ExitCode != 0 {
		t.Fatalf("unexpected exit code: %d", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "hello from cleanroom") {
		t.Fatalf("expected stdout in result, got %q", result.Stdout)
	}
	if !strings.Contains(stdout.String(), "hello from cleanroom") {
		t.Fatalf("expected stdout writer to receive output, got %q", stdout.String())
	}
}

func TestExecAndWaitTimeoutCancelsExecution(t *testing.T) {
	adapter := &cancelAwareStreamingAdapter{
		runEntered:  make(chan struct{}, 1),
		runCanceled: make(chan struct{}, 1),
	}
	cancelCalled := make(chan struct{}, 1)
	host := startIntegrationServerWithAdapterAndExecutionHooks(t, adapter, integrationExecutionHooks{
		onCancelExecution: func(*CancelExecutionRequest) {
			select {
			case cancelCalled <- struct{}{}:
			default:
			}
		},
	})
	c, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx := context.Background()
	sb, err := c.EnsureSandbox(ctx, "thread:timeout-cancel", EnsureSandboxOptions{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("EnsureSandbox returned error: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, runErr := c.ExecAndWait(ctx, sb.ID, []string{"sleep", "999"}, ExecOptions{
			Timeout: 100 * time.Millisecond,
		})
		done <- runErr
	}()

	select {
	case <-adapter.runEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("execution never reached backend")
	}

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ExecAndWait did not return after timeout")
	}
	if runErr == nil {
		t.Fatal("expected timeout error from ExecAndWait")
	}

	select {
	case <-cancelCalled:
	case <-time.After(5 * time.Second):
		runCanceledObserved := false
		select {
		case <-adapter.runCanceled:
			runCanceledObserved = true
		default:
		}
		t.Fatalf("expected ExecAndWait to request cancellation after timeout (runErr=%v runCanceledObserved=%t)", runErr, runCanceledObserved)
	}

	select {
	case <-adapter.runCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("expected execution run context to be canceled after timeout")
	}
}

func TestExecAndWaitStreamSetupFailureCancelsExecution(t *testing.T) {
	adapter := &cancelAwareStreamingAdapter{
		runEntered:  make(chan struct{}, 1),
		runCanceled: make(chan struct{}, 1),
	}
	cancelCalled := make(chan struct{}, 1)
	host := startIntegrationServerWithAdapterAndExecutionHooks(t, adapter, integrationExecutionHooks{
		beforeStreamExecution: func(context.Context, *StreamExecutionRequest) error {
			select {
			case <-adapter.runEntered:
			case <-time.After(2 * time.Second):
				return errors.New("execution did not reach backend before stream delay")
			}
			time.Sleep(200 * time.Millisecond)
			return nil
		},
		onCancelExecution: func(*CancelExecutionRequest) {
			select {
			case cancelCalled <- struct{}{}:
			default:
			}
		},
	})
	c, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx := context.Background()
	sb, err := c.EnsureSandbox(ctx, "thread:stream-setup-fail-cancel", EnsureSandboxOptions{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("EnsureSandbox returned error: %v", err)
	}

	_, runErr := c.ExecAndWait(ctx, sb.ID, []string{"sleep", "999"}, ExecOptions{
		Timeout: 50 * time.Millisecond,
	})
	if runErr == nil {
		t.Fatal("expected stream setup timeout error from ExecAndWait")
	}

	select {
	case <-cancelCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("expected ExecAndWait to request cancellation when stream setup fails")
	}

	select {
	case <-adapter.runCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("expected execution to be canceled when stream setup fails")
	}
}

func TestExecAndWaitTimeoutAppliesToFallbackStatusFetch(t *testing.T) {
	adapter := &immediateStreamingAdapter{}
	host := startIntegrationServerWithAdapterAndExecutionHooks(t, adapter, integrationExecutionHooks{
		overrideStream: func(context.Context, *connect.Request[StreamExecutionRequest], *connect.ServerStream[ExecutionStreamEvent]) error {
			// Return a successful stream with no events so ExecAndWait uses GetExecution fallback.
			return nil
		},
		beforeGetExecution: func(ctx context.Context, _ *GetExecutionRequest) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
				return nil
			}
		},
	})
	c, err := New(host)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	ctx := context.Background()
	sb, err := c.EnsureSandbox(ctx, "thread:fallback-timeout", EnsureSandboxOptions{
		Backend: "firecracker",
		Policy:  testPolicy(),
	})
	if err != nil {
		t.Fatalf("EnsureSandbox returned error: %v", err)
	}

	started := time.Now()
	_, runErr := c.ExecAndWait(ctx, sb.ID, []string{"echo", "hello"}, ExecOptions{
		Timeout: 50 * time.Millisecond,
	})
	if runErr == nil {
		t.Fatal("expected timeout error from fallback status fetch")
	}
	if got := ErrCode(runErr); got != ErrorCodeDeadlineExceeded {
		t.Fatalf("expected deadline_exceeded from fallback status fetch, got %q (%v)", got, runErr)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("fallback status fetch exceeded timeout budget: elapsed=%s", elapsed)
	}
}

func TestErrCode(t *testing.T) {
	if got := ErrCode(connect.NewError(connect.CodeNotFound, errors.New("unknown sandbox \"x\""))); got != ErrorCodeNotFound {
		t.Fatalf("unexpected code for not found: %q", got)
	}

	if got := ErrCode(connect.NewError(connect.CodeInternal, errors.New("backend_capability_mismatch: denied"))); got != ErrorCodeBackendCapabilityMismatch {
		t.Fatalf("unexpected code for backend mismatch: %q", got)
	}
}
