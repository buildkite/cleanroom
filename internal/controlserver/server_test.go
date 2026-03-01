package controlserver

import (
	"net"
	"testing"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/endpoint"
)

func TestStreamSubscriberDroppedErrWhileExecutionStillRunning(t *testing.T) {
	done := make(chan struct{})

	err := streamSubscriberDroppedErr(done, "execution")
	if err == nil {
		t.Fatal("expected error when stream subscriber is dropped before done")
	}
	if got, want := connect.CodeOf(err), connect.CodeResourceExhausted; got != want {
		t.Fatalf("unexpected connect code: got %v want %v", got, want)
	}
}

func TestStreamSubscriberDroppedErrAfterExecutionDone(t *testing.T) {
	done := make(chan struct{})
	close(done)

	if err := streamSubscriberDroppedErr(done, "execution"); err != nil {
		t.Fatalf("expected nil when stream closes after done, got %v", err)
	}
}

func TestListenHTTPAcceptsHTTPPrefix(t *testing.T) {
	t.Parallel()

	ep := endpoint.Endpoint{
		Scheme:  "http",
		Address: "http://127.0.0.1:0",
	}
	ln, cleanup, err := listen(ep, nil)
	if err != nil {
		t.Fatalf("listen http endpoint: %v", err)
	}
	if cleanup != nil {
		t.Fatal("expected no cleanup callback for tcp/http listener")
	}
	t.Cleanup(func() { _ = ln.Close() })
	if _, ok := ln.Addr().(*net.TCPAddr); !ok {
		t.Fatalf("expected tcp listener, got %T", ln.Addr())
	}
}

func TestListenRejectsUnsupportedScheme(t *testing.T) {
	t.Parallel()

	_, _, err := listen(endpoint.Endpoint{Scheme: "tssvc", Address: "127.0.0.1:0"}, nil)
	if err == nil {
		t.Fatal("expected unsupported scheme error")
	}
}
