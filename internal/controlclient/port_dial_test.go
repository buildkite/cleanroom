package controlclient

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/controlserver"
	"github.com/buildkite/cleanroom/internal/controlservice"
	"github.com/buildkite/cleanroom/internal/endpoint"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

type portDialTestAdapter struct {
	peer   chan net.Conn
	dialFn func(context.Context, string, int) (net.Conn, error)
}

func (a *portDialTestAdapter) Name() string { return "firecracker" }

func (a *portDialTestAdapter) ProvisionSandbox(context.Context, backend.ProvisionRequest) error {
	return nil
}

func (a *portDialTestAdapter) RunInSandbox(context.Context, backend.ExecutionRequest, backend.OutputStream) (*backend.ExecutionResult, error) {
	return nil, nil
}

func (a *portDialTestAdapter) TerminateSandbox(context.Context, string) error {
	return nil
}

func (a *portDialTestAdapter) DialSandboxPort(ctx context.Context, sandboxID string, port int) (net.Conn, error) {
	if a.dialFn != nil {
		return a.dialFn(ctx, sandboxID, port)
	}
	serverConn, peer := net.Pipe()
	a.peer <- peer
	return serverConn, nil
}

func newPortDialTestClient(t *testing.T, adapter *portDialTestAdapter) (*Client, string) {
	t.Helper()
	service := &controlservice.Service{
		Config: runtimeconfig.Config{DefaultBackend: "firecracker"},
		Backends: map[string]backend.Adapter{
			"firecracker": adapter,
		},
	}
	httpServer := httptest.NewServer(controlserver.New(service, nil).Handler())
	t.Cleanup(httpServer.Close)

	client, err := New(endpoint.Endpoint{Scheme: "http", BaseURL: httpServer.URL})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	createResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Backend: "firecracker",
		Policy: &cleanroomv1.Policy{
			Version:        1,
			ImageRef:       "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			NetworkDefault: "deny",
		},
	})
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	return client, createResp.GetSandbox().GetSandboxId()
}

func TestDialSandboxPortStreamOutlivesDialContext(t *testing.T) {
	adapter := &portDialTestAdapter{peer: make(chan net.Conn, 1)}
	client, sandboxID := newPortDialTestClient(t, adapter)

	dialCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	conn, err := client.DialSandboxPort(dialCtx, sandboxID, 3000)
	if err != nil {
		t.Fatalf("DialSandboxPort returned error: %v", err)
	}
	defer conn.Close()

	var peer net.Conn
	select {
	case peer = <-adapter.peer:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backend dial")
	}
	defer peer.Close()

	time.Sleep(50 * time.Millisecond)
	writeDone := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("ping"))
		writeDone <- err
	}()

	buf := make([]byte, 4)
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline returned error: %v", err)
	}
	n, err := peer.Read(buf)
	if err != nil {
		t.Fatalf("peer Read returned error after dial context deadline: %v", err)
	}
	if got, want := string(buf[:n]), "ping"; got != want {
		t.Fatalf("unexpected peer payload: got %q want %q", got, want)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("Write returned error after dial context deadline: %v", err)
	}
}

func TestDialSandboxPortWaitsForBackendDial(t *testing.T) {
	started := make(chan struct{})
	adapter := &portDialTestAdapter{
		dialFn: func(ctx context.Context, _ string, _ int) (net.Conn, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	client, sandboxID := newPortDialTestClient(t, adapter)

	dialCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	conn, err := client.DialSandboxPort(dialCtx, sandboxID, 3000)
	if err == nil {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatal("expected DialSandboxPort to fail when backend dial exceeds the caller deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded error, got %v", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("expected backend dial to start before the caller deadline")
	}
}

func TestDialSandboxPortReturnsBackendOpenError(t *testing.T) {
	adapter := &portDialTestAdapter{
		dialFn: func(context.Context, string, int) (net.Conn, error) {
			return nil, errors.New("backend refused")
		},
	}
	client, sandboxID := newPortDialTestClient(t, adapter)

	conn, err := client.DialSandboxPort(context.Background(), sandboxID, 3000)
	if err == nil {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatal("expected DialSandboxPort to return backend open error")
	}
	if !strings.Contains(err.Error(), "backend refused") {
		t.Fatalf("expected backend error, got %v", err)
	}
}

func TestDialSandboxPortCloseDoesNotWaitForBackendEOF(t *testing.T) {
	adapter := &portDialTestAdapter{peer: make(chan net.Conn, 1)}
	client, sandboxID := newPortDialTestClient(t, adapter)

	conn, err := client.DialSandboxPort(context.Background(), sandboxID, 3000)
	if err != nil {
		t.Fatalf("DialSandboxPort returned error: %v", err)
	}
	peer := <-adapter.peer
	defer peer.Close()

	done := make(chan error, 1)
	go func() {
		done <- conn.Close()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Close")
	}
}
