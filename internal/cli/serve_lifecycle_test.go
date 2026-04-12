package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/gateway"
)

func reserveLocalTCPAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local TCP address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close reserved listener: %v", err)
	}
	return addr
}

func waitForHTTPHealthz(t *testing.T, url string, timeout time.Duration) {
	t.Helper()

	client := &http.Client{Timeout: 200 * time.Millisecond}
	ctx, cancel := context.WithTimeoutCause(context.Background(), timeout, fmt.Errorf("timed out waiting for %s", url))
	defer cancel()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build health check request: %v", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(context.Cause(ctx))
		case <-ticker.C:
		}
	}
}

func stubServeNotifyContext(t *testing.T, fn func(context.Context, ...os.Signal) (context.Context, context.CancelFunc)) {
	t.Helper()

	prev := serveSignalNotifyContext
	serveSignalNotifyContext = fn
	t.Cleanup(func() {
		serveSignalNotifyContext = prev
	})
}

func TestServeCommandRunServerStartsAndStopsOnContextCancel(t *testing.T) {
	listenAddr := reserveLocalTCPAddr(t)
	var cancelRun context.CancelFunc
	stubServeNotifyContext(t, func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		runCtx, cancel := context.WithCancel(parent)
		cancelRun = cancel
		return runCtx, cancel
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- (&ServeCommand{
			Listen:        "http://" + listenAddr,
			GatewayListen: "127.0.0.1:0",
		}).Run(&runtimeContext{
			CWD:        t.TempDir(),
			ConfigPath: "/tmp/cleanroom-config.yaml",
			Backends:   map[string]backend.Adapter{},
		})
	}()

	waitForHTTPHealthz(t, fmt.Sprintf("http://%s/healthz", listenAddr), 5*time.Second)
	if cancelRun == nil {
		t.Fatal("expected serveSignalNotifyContext replacement to capture a cancel func")
	}
	cancelRun()

	waitCtx, cancel := context.WithTimeoutCause(context.Background(), 5*time.Second, fmt.Errorf("timed out waiting for ServeCommand.Run to exit after cancellation"))
	defer cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ServeCommand.Run returned error: %v", err)
		}
	case <-waitCtx.Done():
		t.Fatal(context.Cause(waitCtx))
	}
}

func TestServeCommandRunServerStartsWithoutContentCache(t *testing.T) {
	listenAddr := reserveLocalTCPAddr(t)
	var cancelRun context.CancelFunc
	stubServeNotifyContext(t, func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		runCtx, cancel := context.WithCancel(parent)
		cancelRun = cancel
		return runCtx, cancel
	})

	prevNewGatewayContentCache := newGatewayContentCache
	newGatewayContentCache = func(gateway.ContentCacheConfig) (*gateway.ContentCache, error) {
		return nil, errors.New("cache dir unavailable")
	}
	t.Cleanup(func() {
		newGatewayContentCache = prevNewGatewayContentCache
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- (&ServeCommand{
			Listen:        "http://" + listenAddr,
			GatewayListen: "127.0.0.1:0",
		}).Run(&runtimeContext{
			CWD:        t.TempDir(),
			ConfigPath: "/tmp/cleanroom-config.yaml",
			Backends:   map[string]backend.Adapter{},
		})
	}()

	waitForHTTPHealthz(t, fmt.Sprintf("http://%s/healthz", listenAddr), 5*time.Second)
	if cancelRun == nil {
		t.Fatal("expected serveSignalNotifyContext replacement to capture a cancel func")
	}
	cancelRun()

	waitCtx, cancel := context.WithTimeoutCause(context.Background(), 5*time.Second, fmt.Errorf("timed out waiting for ServeCommand.Run to exit after cancellation"))
	defer cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ServeCommand.Run returned error: %v", err)
		}
	case <-waitCtx.Done():
		t.Fatal(context.Cause(waitCtx))
	}
}
