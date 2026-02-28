package darwinvz

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

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
