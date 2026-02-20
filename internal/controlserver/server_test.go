package controlserver

import (
	"errors"
	"net"
	"path/filepath"
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

type stubTSNetServer struct {
	listener  net.Listener
	listenErr error

	listenNetwork string
	listenAddr    string
	closeCalls    int
}

func (s *stubTSNetServer) Listen(network, addr string) (net.Listener, error) {
	s.listenNetwork = network
	s.listenAddr = addr
	if s.listenErr != nil {
		return nil, s.listenErr
	}
	if s.listener == nil {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		s.listener = ln
	}
	return s.listener, nil
}

func (s *stubTSNetServer) Close() error {
	s.closeCalls++
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func TestListenTSNetUsesStateDirAndCleanup(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	stub := &stubTSNetServer{}
	var gotStateDir string

	originalNewTSNetServer := newTSNetServer
	newTSNetServer = func(_ endpoint.Endpoint, stateDir string) tsnetServer {
		gotStateDir = stateDir
		return stub
	}
	t.Cleanup(func() {
		newTSNetServer = originalNewTSNetServer
	})

	ep := endpoint.Endpoint{
		Scheme:        "tsnet",
		Address:       ":7777",
		TSNetHostname: "cleanroom",
	}

	ln, cleanup, err := listen(ep)
	if err != nil {
		t.Fatalf("listen tsnet: %v", err)
	}
	if ln == nil {
		t.Fatal("expected listener")
	}
	if cleanup == nil {
		t.Fatal("expected cleanup callback")
	}
	if stub.listenNetwork != "tcp" {
		t.Fatalf("expected tcp network, got %q", stub.listenNetwork)
	}
	if stub.listenAddr != ":7777" {
		t.Fatalf("expected tsnet listen addr :7777, got %q", stub.listenAddr)
	}

	expectedStateDir := filepath.Join(stateHome, "cleanroom", "tsnet")
	if gotStateDir != expectedStateDir {
		t.Fatalf("expected tsnet state dir %q, got %q", expectedStateDir, gotStateDir)
	}

	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if stub.closeCalls != 1 {
		t.Fatalf("expected cleanup to close tsnet server once, got %d", stub.closeCalls)
	}
}

func TestListenTSNetClosesServerOnListenError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	stub := &stubTSNetServer{listenErr: errors.New("boom")}

	originalNewTSNetServer := newTSNetServer
	newTSNetServer = func(_ endpoint.Endpoint, _ string) tsnetServer {
		return stub
	}
	t.Cleanup(func() {
		newTSNetServer = originalNewTSNetServer
	})

	_, cleanup, err := listen(endpoint.Endpoint{
		Scheme:        "tsnet",
		Address:       ":7777",
		TSNetHostname: "cleanroom",
	})
	if err == nil {
		t.Fatal("expected listen error")
	}
	if cleanup != nil {
		t.Fatal("expected no cleanup callback on listen failure")
	}
	if stub.closeCalls != 1 {
		t.Fatalf("expected listen failure to close tsnet server, got %d", stub.closeCalls)
	}
}
