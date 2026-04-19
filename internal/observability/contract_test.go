package observability

import (
	"testing"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func TestGatewayRequestSpanName(t *testing.T) {
	t.Parallel()

	if got, want := GatewayRequestSpanName("git"), "cleanroom.gateway.git.request"; got != want {
		t.Fatalf("GatewayRequestSpanName returned %q, want %q", got, want)
	}
}

func TestExecutionOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status cleanroomv1.ExecutionStatus
		want   string
	}{
		{name: "succeeded", status: cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED, want: OutcomeSucceeded},
		{name: "failed", status: cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED, want: OutcomeFailed},
		{name: "canceled", status: cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED, want: OutcomeCanceled},
		{name: "timed out", status: cleanroomv1.ExecutionStatus_EXECUTION_STATUS_TIMED_OUT, want: OutcomeTimedOut},
		{name: "queued falls back to enum name", status: cleanroomv1.ExecutionStatus_EXECUTION_STATUS_QUEUED, want: "queued"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ExecutionOutcome(tt.status); got != tt.want {
				t.Fatalf("ExecutionOutcome(%s) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}
