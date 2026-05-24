package darwinvz

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
)

func TestHelperRequestDecodeErrorIsLifecycleIndeterminate(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	session := &helperSession{
		conn: clientConn,
		enc:  json.NewEncoder(clientConn),
		dec:  json.NewDecoder(clientConn),
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := session.request(context.Background(), helperControlRequest{Op: "PauseVM", VMID: "vm-test"})
		errCh <- err
	}()

	var req helperControlRequest
	if err := json.NewDecoder(serverConn).Decode(&req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if req.Op != "PauseVM" {
		t.Fatalf("unexpected request op: got %q want PauseVM", req.Op)
	}
	if err := serverConn.Close(); err != nil {
		t.Fatalf("close server conn: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected request error")
		}
		if !errors.Is(err, backend.ErrSandboxLifecycleIndeterminate) {
			t.Fatalf("expected indeterminate lifecycle error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request error")
	}
}

func TestCloseProcessReturnsWhenDoneChannelIsDrained(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not available: %v", err)
	}

	cmd := exec.Command(sleepPath, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep process: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Process.Release()
		}
	})

	origInterruptWait := helperInterruptWait
	origKillWait := helperKillWait
	helperInterruptWait = 25 * time.Millisecond
	helperKillWait = 25 * time.Millisecond
	t.Cleanup(func() {
		helperInterruptWait = origInterruptWait
		helperKillWait = origKillWait
	})

	done := make(chan error, 1)
	done <- nil
	<-done // Simulate waitForHelperControlSocket consuming the only wait result.

	session := &helperSession{
		cmd:  cmd,
		done: done,
	}

	resultCh := make(chan error, 1)
	go func() {
		resultCh <- session.closeProcess()
	}()

	select {
	case closeErr := <-resultCh:
		if closeErr == nil {
			t.Fatal("expected timeout error when done channel has no further sender")
		}
		if !strings.Contains(closeErr.Error(), "timed out waiting for helper process exit") {
			t.Fatalf("unexpected error: %v", closeErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closeProcess blocked waiting on drained done channel")
	}
}
