package controlserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlservice"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/gen/cleanroom/v1/cleanroomv1connect"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type handlerTestAdapter struct {
	started     chan struct{}
	allowStdout chan struct{}
	allowFinish chan struct{}
}

func newHandlerTestAdapter() *handlerTestAdapter {
	return &handlerTestAdapter{
		started:     make(chan struct{}, 1),
		allowStdout: make(chan struct{}),
		allowFinish: make(chan struct{}),
	}
}

func (a *handlerTestAdapter) Name() string { return "firecracker" }

func (a *handlerTestAdapter) ProvisionSandbox(context.Context, backend.ProvisionRequest) error {
	return nil
}

func (a *handlerTestAdapter) RunInSandbox(ctx context.Context, req backend.ExecutionRequest, stream backend.OutputStream) (*backend.ExecutionResult, error) {
	select {
	case a.started <- struct{}{}:
	default:
	}

	select {
	case <-a.allowStdout:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if stream.OnStdout != nil {
		stream.OnStdout([]byte("hello from handler\n"))
	}

	select {
	case <-a.allowFinish:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &backend.ExecutionResult{
		ExecutionID: req.ExecutionID,
		ExitCode:    0,
		Message:     "ok",
	}, nil
}

func (a *handlerTestAdapter) TerminateSandbox(context.Context, string) error {
	return nil
}

func newHandlerTestService(adapter backend.Adapter) *controlservice.Service {
	return &controlservice.Service{
		Config: runtimeconfig.Config{DefaultBackend: "firecracker"},
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
	}
}

func handlerTestPolicy() *cleanroomv1.Policy {
	return &cleanroomv1.Policy{
		Version:        1,
		ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		NetworkDefault: "deny",
	}
}

func handlerTestRepositoryPolicy() *cleanroomv1.Policy {
	policy := handlerTestPolicy()
	policy.Allow = []*cleanroomv1.PolicyAllowRule{{
		Host:  "github.com",
		Ports: []int32{443},
	}}
	return policy
}

func TestSandboxEventStreamReturnsHistoryThenFollowUpdates(t *testing.T) {
	service := newHandlerTestService(newHandlerTestAdapter())
	httpServer := httptest.NewServer(New(service, nil).Handler())
	defer httpServer.Close()

	sandboxClient := cleanroomv1connect.NewSandboxServiceClient(http.DefaultClient, httpServer.URL)

	createResp, err := sandboxClient.CreateSandbox(context.Background(), connect.NewRequest(&cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  handlerTestPolicy(),
	}))
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createResp.Msg.GetSandbox().GetSandboxId()

	stream, err := sandboxClient.StreamSandboxEvents(context.Background(), connect.NewRequest(&cleanroomv1.StreamSandboxEventsRequest{
		SandboxId: sandboxID,
		Follow:    true,
	}))
	if err != nil {
		t.Fatalf("StreamSandboxEvents returned error: %v", err)
	}

	if !stream.Receive() {
		t.Fatal("expected ready history event")
	}
	readyEvent := stream.Msg()
	if got, want := readyEvent.GetStatus(), cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY; got != want {
		t.Fatalf("unexpected history status: got %v want %v", got, want)
	}
	if got := readyEvent.GetMessage(); !strings.Contains(got, "created and ready") {
		t.Fatalf("unexpected ready history message: %q", got)
	}

	terminateDone := make(chan error, 1)
	go func() {
		_, err := sandboxClient.TerminateSandbox(context.Background(), connect.NewRequest(&cleanroomv1.TerminateSandboxRequest{
			SandboxId: sandboxID,
		}))
		terminateDone <- err
	}()

	var statuses []cleanroomv1.SandboxStatus
	for stream.Receive() {
		statuses = append(statuses, stream.Msg().GetStatus())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamSandboxEvents stream error: %v", err)
	}
	if err := <-terminateDone; err != nil {
		t.Fatalf("TerminateSandbox returned error: %v", err)
	}

	wantStatuses := []cleanroomv1.SandboxStatus{
		cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPING,
		cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED,
	}
	if len(statuses) != len(wantStatuses) {
		t.Fatalf("unexpected sandbox update count: got %d want %d (%v)", len(statuses), len(wantStatuses), statuses)
	}
	for i, want := range wantStatuses {
		if got := statuses[i]; got != want {
			t.Fatalf("sandbox event %d status mismatch: got %v want %v", i, got, want)
		}
	}
}

func TestCreateSandboxStreamShowsBootstrapOutputThenResponse(t *testing.T) {
	adapter := newHandlerTestAdapter()
	service := newHandlerTestService(adapter)
	httpServer := httptest.NewServer(New(service, nil).Handler())
	defer httpServer.Close()

	sandboxClient := cleanroomv1connect.NewSandboxServiceClient(http.DefaultClient, httpServer.URL)

	stream, err := sandboxClient.CreateSandboxStream(context.Background(), connect.NewRequest(&cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  handlerTestRepositoryPolicy(),
		RepositoryCheckout: &cleanroomv1.RepositoryCheckout{
			RemoteUrl:      "https://github.com/buildkite/cleanroom.git",
			CommitSha:      "0123456789abcdef0123456789abcdef01234567",
			DestinationDir: "/workspace",
		},
	}))
	if err != nil {
		t.Fatalf("CreateSandboxStream returned error: %v", err)
	}

	if !stream.Receive() {
		t.Fatalf("expected provisioning event, stream err=%v", stream.Err())
	}
	if got := stream.Msg().GetMessage(); !strings.Contains(got, "provisioning sandbox") {
		t.Fatalf("unexpected first create event message: %q", got)
	}

	if !stream.Receive() {
		t.Fatalf("expected repository bootstrap event, stream err=%v", stream.Err())
	}
	if got := stream.Msg().GetMessage(); !strings.Contains(got, "bootstrapping repository checkout") {
		t.Fatalf("unexpected repository bootstrap message: %q", got)
	}

	close(adapter.allowStdout)
	if !stream.Receive() {
		t.Fatalf("expected bootstrap stdout event, stream err=%v", stream.Err())
	}
	if got, want := string(stream.Msg().GetStdout()), "hello from handler\n"; got != want {
		t.Fatalf("unexpected bootstrap stdout event: got %q want %q", got, want)
	}

	close(adapter.allowFinish)

	var sawResponse bool
	for stream.Receive() {
		if response := stream.Msg().GetResponse(); response != nil {
			sawResponse = true
			if response.GetSandbox().GetStatus() != cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY {
				t.Fatalf("unexpected streamed sandbox status: %v", response.GetSandbox().GetStatus())
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("CreateSandboxStream stream error: %v", err)
	}
	if !sawResponse {
		t.Fatal("expected final create response event")
	}
}

func TestExecutionEventStreamReturnsHistoryThenFollowUpdates(t *testing.T) {
	adapter := newHandlerTestAdapter()
	service := newHandlerTestService(adapter)
	httpServer := httptest.NewServer(New(service, nil).Handler())
	defer httpServer.Close()

	sandboxClient := cleanroomv1connect.NewSandboxServiceClient(http.DefaultClient, httpServer.URL)
	executionClient := cleanroomv1connect.NewExecutionServiceClient(http.DefaultClient, httpServer.URL)

	createSandboxResp, err := sandboxClient.CreateSandbox(context.Background(), connect.NewRequest(&cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy:  handlerTestPolicy(),
	}))
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	sandboxID := createSandboxResp.Msg.GetSandbox().GetSandboxId()

	createExecutionResp, err := executionClient.CreateExecution(context.Background(), connect.NewRequest(&cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "hello"},
	}))
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createExecutionResp.Msg.GetExecution().GetExecutionId()

	select {
	case <-adapter.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for execution to start")
	}

	stream, err := executionClient.StreamExecution(context.Background(), connect.NewRequest(&cleanroomv1.StreamExecutionRequest{
		SandboxId:   sandboxID,
		ExecutionId: executionID,
		Follow:      true,
	}))
	if err != nil {
		t.Fatalf("StreamExecution returned error: %v", err)
	}

	if !stream.Receive() {
		t.Fatal("expected queued history event")
	}
	queuedEvent := stream.Msg()
	if got, want := queuedEvent.GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_QUEUED; got != want {
		t.Fatalf("unexpected queued history status: got %v want %v", got, want)
	}
	if got := queuedEvent.GetMessage(); !strings.Contains(got, "queued") {
		t.Fatalf("unexpected queued history message: %q", got)
	}

	if !stream.Receive() {
		t.Fatal("expected started history event")
	}
	startedEvent := stream.Msg()
	if got, want := startedEvent.GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING; got != want {
		t.Fatalf("unexpected started history status: got %v want %v", got, want)
	}
	if got := startedEvent.GetMessage(); !strings.Contains(got, "started") {
		t.Fatalf("unexpected started history message: %q", got)
	}

	close(adapter.allowStdout)
	if !stream.Receive() {
		t.Fatal("expected stdout follow event")
	}
	stdoutEvent := stream.Msg()
	if got, want := string(stdoutEvent.GetStdout()), "hello from handler\n"; got != want {
		t.Fatalf("unexpected stdout follow event: got %q want %q", got, want)
	}

	close(adapter.allowFinish)
	if !stream.Receive() {
		t.Fatal("expected exit follow event")
	}
	exitEvent := stream.Msg().GetExit()
	if exitEvent == nil {
		t.Fatal("expected exit payload")
	}
	if got, want := exitEvent.GetStatus(), cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED; got != want {
		t.Fatalf("unexpected exit status: got %v want %v", got, want)
	}
	if got, want := exitEvent.GetExitCode(), int32(0); got != want {
		t.Fatalf("unexpected exit code: got %d want %d", got, want)
	}

	if stream.Receive() {
		t.Fatal("did not expect extra execution events after exit")
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamExecution stream error: %v", err)
	}
}

func TestCreateExecutionRejectsSpoofedInternalWorkspaceCopyInHeader(t *testing.T) {
	service := newHandlerTestService(newHandlerTestAdapter())
	httpServer := httptest.NewServer(New(service, nil).Handler())
	defer httpServer.Close()

	executionClient := cleanroomv1connect.NewExecutionServiceClient(http.DefaultClient, httpServer.URL)
	req := connect.NewRequest(&cleanroomv1.CreateExecutionRequest{
		SandboxId: "sbx_spoofed",
		Command:   []string{"sh", "-lc", "true"},
		Options: &cleanroomv1.ExecutionOptions{
			SkipRunBefore: true,
		},
	})
	req.Header().Set(internalWorkspaceCopyInHeader, "1")

	_, err := executionClient.CreateExecution(context.Background(), req)
	if err == nil {
		t.Fatal("expected CreateExecution to reject spoofed internal workspace copy-in header")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Fatalf("unexpected error code: got %v want %v (err=%v)", code, connect.CodeUnauthenticated, err)
	}
}

func TestCreateExecutionRejectsInternalWorkspaceCopyInHeaderWhenLoopbackTrustUnmarked(t *testing.T) {
	service := newHandlerTestService(newHandlerTestAdapter())
	server := New(service, nil).TrustInternalWorkspaceCopyInRequestsFromLoopback()

	req := connect.NewRequest(&cleanroomv1.CreateExecutionRequest{
		SandboxId: "sbx_unmarked",
		Command:   []string{"sh", "-lc", "true"},
		Options: &cleanroomv1.ExecutionOptions{
			SkipRunBefore: true,
		},
	})
	req.Header().Set(internalWorkspaceCopyInHeader, "1")

	_, err := server.CreateExecution(context.Background(), req)
	if err == nil {
		t.Fatal("expected CreateExecution to reject unmarked internal workspace copy-in request")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Fatalf("unexpected error code: got %v want %v (err=%v)", code, connect.CodeUnauthenticated, err)
	}
}

func TestCreateExecutionAllowsInternalWorkspaceCopyInHeaderFromLoopbackTrustedEndpoint(t *testing.T) {
	service := newHandlerTestService(newHandlerTestAdapter())
	httpServer := httptest.NewServer(New(service, nil).TrustInternalWorkspaceCopyInRequestsFromLoopback().Handler())
	defer httpServer.Close()

	executionClient := cleanroomv1connect.NewExecutionServiceClient(http.DefaultClient, httpServer.URL)
	req := connect.NewRequest(&cleanroomv1.CreateExecutionRequest{
		SandboxId: "sbx_loopback_trusted",
		Command:   []string{"sh", "-lc", "true"},
		Options: &cleanroomv1.ExecutionOptions{
			SkipRunBefore: true,
		},
	})
	req.Header().Set(internalWorkspaceCopyInHeader, "1")

	_, err := executionClient.CreateExecution(context.Background(), req)
	if err == nil {
		t.Fatal("expected CreateExecution to return unknown sandbox")
	}
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Fatalf("unexpected error code: got %v want %v (err=%v)", code, connect.CodeNotFound, err)
	}
}

func TestCreateExecutionAllowsInternalWorkspaceCopyInHeaderWhenTrusted(t *testing.T) {
	service := newHandlerTestService(newHandlerTestAdapter())
	httpServer := httptest.NewServer(New(service, nil).TrustInternalWorkspaceCopyInRequests().Handler())
	defer httpServer.Close()

	executionClient := cleanroomv1connect.NewExecutionServiceClient(http.DefaultClient, httpServer.URL)
	req := connect.NewRequest(&cleanroomv1.CreateExecutionRequest{
		SandboxId: "sbx_trusted",
		Command:   []string{"sh", "-lc", "true"},
		Options: &cleanroomv1.ExecutionOptions{
			SkipRunBefore: true,
		},
	})
	req.Header().Set(internalWorkspaceCopyInHeader, "1")

	_, err := executionClient.CreateExecution(context.Background(), req)
	if err == nil {
		t.Fatal("expected CreateExecution to return unknown sandbox")
	}
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Fatalf("unexpected error code: got %v want %v (err=%v)", code, connect.CodeNotFound, err)
	}
}

func TestMarkLoopbackInternalWorkspaceCopyInRequestsStripsSpoofedMarkerFromRemote(t *testing.T) {
	var gotTrusted string
	handler := markLoopbackInternalWorkspaceCopyInRequests(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotTrusted = r.Header.Get(internalWorkspaceCopyInTrustedHeader)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "203.0.113.10:12345"
	req.Header.Set(internalWorkspaceCopyInHeader, "1")
	req.Header.Set(internalWorkspaceCopyInTrustedHeader, "1")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotTrusted != "" {
		t.Fatalf("expected spoofed trust marker to be stripped for remote caller, got %q", gotTrusted)
	}
}

func TestMarkLoopbackInternalWorkspaceCopyInRequestsMarksLoopback(t *testing.T) {
	var gotTrusted string
	handler := markLoopbackInternalWorkspaceCopyInRequests(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotTrusted = r.Header.Get(internalWorkspaceCopyInTrustedHeader)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set(internalWorkspaceCopyInHeader, "1")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotTrusted != "1" {
		t.Fatalf("expected loopback copy-in request to be marked trusted, got %q", gotTrusted)
	}
}

func TestIsLoopbackRemoteAddr(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		want       bool
	}{
		{
			name:       "ipv4 loopback",
			remoteAddr: "127.0.0.1:12345",
			want:       true,
		},
		{
			name:       "ipv6 loopback",
			remoteAddr: "[::1]:12345",
			want:       true,
		},
		{
			name:       "remote ipv4",
			remoteAddr: "203.0.113.10:12345",
			want:       false,
		},
		{
			name:       "private network is still remote",
			remoteAddr: "10.0.0.2:12345",
			want:       false,
		},
		{
			name:       "invalid",
			remoteAddr: "not-an-address",
			want:       false,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := isLoopbackRemoteAddr(tc.remoteAddr); got != tc.want {
				t.Fatalf("isLoopbackRemoteAddr(%q) = %v, want %v", tc.remoteAddr, got, tc.want)
			}
		})
	}
}

func TestToConnectErrorMapsExpectedCodes(t *testing.T) {
	t.Parallel()

	connectErr := connect.NewError(connect.CodeUnauthenticated, errors.New("bad auth"))
	tests := []struct {
		name string
		err  error
		want connect.Code
	}{
		{
			name: "canceled",
			err:  context.Canceled,
			want: connect.CodeCanceled,
		},
		{
			name: "deadline exceeded",
			err:  context.DeadlineExceeded,
			want: connect.CodeDeadlineExceeded,
		},
		{
			name: "missing maps to invalid argument",
			err:  errors.New("missing sandbox_id"),
			want: connect.CodeInvalidArgument,
		},
		{
			name: "unknown maps to not found",
			err:  errors.New("unknown sandbox \"sbx-123\""),
			want: connect.CodeNotFound,
		},
		{
			name: "sandbox path not found maps to not found",
			err:  backend.NewSandboxPathNotFoundError("/tmp/missing"),
			want: connect.CodeNotFound,
		},
		{
			name: "not ready maps to failed precondition",
			err:  errors.New("sandbox \"sbx-123\" is not ready"),
			want: connect.CodeFailedPrecondition,
		},
		{
			name: "unsupported suspend maps to failed precondition",
			err:  errors.New("backend \"firecracker\" does not support sandbox suspend"),
			want: connect.CodeFailedPrecondition,
		},
		{
			name: "existing connect error passes through",
			err:  connectErr,
			want: connect.CodeUnauthenticated,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := toConnectError(tc.err)
			if got == nil {
				t.Fatal("expected non-nil connect error")
			}
			if code := connect.CodeOf(got); code != tc.want {
				t.Fatalf("unexpected connect code: got %v want %v", code, tc.want)
			}
		})
	}
}
