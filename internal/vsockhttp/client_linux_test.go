//go:build linux

package vsockhttp

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/mdlayher/vsock"
)

func TestNewHostTransportDialsHypervisorWithDefaultPort(t *testing.T) {
	t.Parallel()

	originalDial := dialVsock
	t.Cleanup(func() { dialVsock = originalDial })

	called := false
	dialVsock = func(contextID, port uint32, cfg *vsock.Config) (net.Conn, error) {
		called = true
		if contextID != vsock.Hypervisor {
			t.Fatalf("unexpected context ID: got %d want %d", contextID, vsock.Hypervisor)
		}
		if port != DefaultGatewayPort {
			t.Fatalf("unexpected port: got %d want %d", port, DefaultGatewayPort)
		}
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}

	transport := NewHostTransport(0)
	conn, err := transport.DialContext(context.Background(), "tcp", "ignored")
	if err != nil {
		t.Fatalf("DialContext returned error: %v", err)
	}
	if !called {
		t.Fatal("expected dialVsock to be called")
	}
	_ = conn.Close()
}

func TestNewHostTransportHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	originalDial := dialVsock
	t.Cleanup(func() { dialVsock = originalDial })

	dialVsock = func(contextID, port uint32, cfg *vsock.Config) (net.Conn, error) {
		t.Fatal("dialVsock should not be called for canceled contexts")
		return nil, errors.New("unexpected")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	transport := NewHostTransport(DefaultGatewayPort)
	_, err := transport.DialContext(ctx, "tcp", "ignored")
	if err == nil {
		t.Fatal("expected DialContext to fail for canceled context")
	}
}

func TestNewHostClientAppliesDefaultTimeout(t *testing.T) {
	t.Parallel()

	client := NewHostClient(DefaultGatewayPort, 0)
	if got, want := client.Timeout, 30*time.Second; got != want {
		t.Fatalf("unexpected default timeout: got %s want %s", got, want)
	}
}
