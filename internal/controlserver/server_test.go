package controlserver

import (
	"testing"

	"connectrpc.com/connect"
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
