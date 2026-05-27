package controlservice

import (
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/authz"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func TestInteractiveSessionBrokerOpenConsumeSingleUse(t *testing.T) {
	now := time.Date(2026, time.March, 15, 10, 0, 0, 0, time.UTC)
	broker := &interactiveSessionBroker{}
	broker.configureTransport("127.0.0.1:4433", "cleanroom-interactive-v1", "abc123")

	executions := map[string]*executionState{
		executionKey("sb-1", "exec-1"): {
			ID:        "exec-1",
			SandboxID: "sb-1",
			Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE,
			Status:    cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
		},
	}

	grant, err := broker.open(executions, now, 30*time.Second, "session-1", "token-1", "sb-1", "exec-1", authz.ResourceOwner{}, 80, 24)
	if err != nil {
		t.Fatalf("open returned error: %v", err)
	}
	if got, want := grant.SessionID, "session-1"; got != want {
		t.Fatalf("unexpected session id: got %q want %q", got, want)
	}
	if got, want := grant.SessionToken, "token-1"; got != want {
		t.Fatalf("unexpected session token: got %q want %q", got, want)
	}
	if got, want := grant.QuicEndpoint, "127.0.0.1:4433"; got != want {
		t.Fatalf("unexpected endpoint: got %q want %q", got, want)
	}

	session, err := broker.consume(executions, now, "session-1", "token-1")
	if err != nil {
		t.Fatalf("consume returned error: %v", err)
	}
	if got, want := session.ExecutionID, "exec-1"; got != want {
		t.Fatalf("unexpected execution id: got %q want %q", got, want)
	}
	if _, err := broker.consume(executions, now, "session-1", "token-1"); err == nil {
		t.Fatal("expected interactive session to be single use")
	}
}

func TestInteractiveSessionBrokerPrunesExpiredSessions(t *testing.T) {
	now := time.Date(2026, time.March, 15, 10, 0, 0, 0, time.UTC)
	broker := &interactiveSessionBroker{}
	broker.configureTransport("127.0.0.1:4433", "cleanroom-interactive-v1", "abc123")

	executions := map[string]*executionState{
		executionKey("sb-1", "exec-1"): {
			ID:        "exec-1",
			SandboxID: "sb-1",
			Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE,
			Status:    cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
		},
	}

	if _, err := broker.open(executions, now, time.Second, "session-1", "token-1", "sb-1", "exec-1", authz.ResourceOwner{}, 80, 24); err != nil {
		t.Fatalf("open returned error: %v", err)
	}
	if _, err := broker.consume(executions, now.Add(2*time.Second), "session-1", "token-1"); err == nil {
		t.Fatal("expected expired interactive session to be pruned")
	}
}
