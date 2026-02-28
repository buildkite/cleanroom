package client

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
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

func startIntegrationServerWithAdapter(t *testing.T, adapter backend.Adapter) string {
	t.Helper()

	svc := &controlservice.Service{
		Config: runtimeconfig.Config{DefaultBackend: "firecracker"},
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
	}

	httpServer := httptest.NewServer(controlserver.New(svc, nil).Handler())
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
	host := startIntegrationServerWithAdapter(t, adapter)
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
	case <-adapter.runCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected execution run context to be canceled after timeout")
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
