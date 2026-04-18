package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func runExecutionInspectWithCapture(cmd ExecutionInspectCommand, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, nil, ctx)
}

func TestExecutionInspectIntegrationShowsExecutionDetails(t *testing.T) {
	t.Helper()

	runDir := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(runDir, "execution-observability.json"),
		[]byte(`{"total_ms":12,"guest_exec_ms":7}`),
		0o644,
	); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	adapter := &integrationAdapter{
		runFn: func(_ context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				Stdout:      "hello\n",
				Stderr:      "warn\n",
				RunDir:      runDir,
				PlanPath:    "/tmp/plan.json",
				ImageRef:    "ghcr.io/buildkite/cleanroom-base/alpine@sha256:abc",
				ImageDigest: "sha256:abc",
				Message:     "command completed",
			}, nil
		},
	}

	host, svc := startIntegrationServer(t, adapter)
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	createResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "ok"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createResp.GetExecution().GetExecutionId()
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	outcome := runExecutionInspectWithCapture(ExecutionInspectCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sandboxID,
		ExecutionID: executionID,
	}, runtimeContext{CWD: t.TempDir()})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecutionInspectCommand.Run returned error: %v", outcome.err)
	}

	assertContainsAll(
		t,
		outcome.stdout,
		"execution: "+executionID,
		"sandbox: "+sandboxID,
		"status: succeeded",
		"kind: batch",
		"exit_code: 0",
		"message: command completed",
		"artifacts_dir: "+runDir,
		"plan_path: /tmp/plan.json",
		"image_ref: ghcr.io/buildkite/cleanroom-base/alpine@sha256:abc",
		"image_digest: sha256:abc",
		"\nstderr:\nwarn\n",
		"\nstdout:\nhello\n",
		"\nobservability:\n",
		`"total_ms": 12`,
		`"guest_exec_ms": 7`,
	)
}

func TestExecutionInspectIntegrationSupportsLastAndJSON(t *testing.T) {
	t.Helper()

	adapter := &integrationAdapter{
		runFn: func(_ context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				Stdout:      "hello\n",
				Message:     "done",
			}, nil
		},
	}

	host, svc := startIntegrationServer(t, adapter)
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	createResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "ok"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createResp.GetExecution().GetExecutionId()
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	outcome := runExecutionInspectWithCapture(ExecutionInspectCommand{
		clientFlags: clientFlags{Host: host},
		SandboxID:   sandboxID,
		Last:        true,
		JSON:        true,
	}, runtimeContext{CWD: t.TempDir()})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecutionInspectCommand.Run returned error: %v", outcome.err)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(outcome.stdout), &payload); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}

	execution, ok := payload["execution"].(map[string]any)
	if !ok {
		t.Fatalf("expected execution object, got %#v", payload["execution"])
	}
	if got, want := strings.TrimSpace(execution["execution_id"].(string)), executionID; got != want {
		t.Fatalf("unexpected execution id: got %q want %q", got, want)
	}
	if got, want := strings.TrimSpace(execution["sandbox_id"].(string)), sandboxID; got != want {
		t.Fatalf("unexpected sandbox id: got %q want %q", got, want)
	}
}

func TestExecutionInspectIntegrationSupportsGlobalExecutionID(t *testing.T) {
	t.Helper()

	adapter := &integrationAdapter{
		runFn: func(_ context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				Stdout:      "hello\n",
				Message:     "done",
			}, nil
		},
	}

	host, svc := startIntegrationServer(t, adapter)
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	createResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "ok"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createResp.GetExecution().GetExecutionId()
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	outcome := runExecutionInspectWithCapture(ExecutionInspectCommand{
		clientFlags: clientFlags{Host: host},
		ExecutionID: executionID,
	}, runtimeContext{CWD: t.TempDir()})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecutionInspectCommand.Run returned error: %v", outcome.err)
	}

	assertContainsAll(
		t,
		outcome.stdout,
		"execution: "+executionID,
		"sandbox: "+sandboxID,
		"status: succeeded",
	)
}

func TestExecutionInspectIntegrationShowsTraceMetadata(t *testing.T) {
	t.Helper()

	runDir := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(runDir, "execution-observability.json"),
		[]byte(`{"trace_id":"0123456789abcdef0123456789abcdef","total_ms":12}`),
		0o644,
	); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	adapter := &integrationAdapter{
		runFn: func(_ context.Context, req backend.ExecutionRequest) (*backend.ExecutionResult, error) {
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				RunDir:      runDir,
			}, nil
		},
	}

	host, svc := startIntegrationServerWithConfig(t, adapter, runtimeconfig.Config{
		DefaultBackend: "firecracker",
		Observability: runtimeconfig.ObservabilityConfig{
			Enabled: true,
			OTLP:    runtimeconfig.OTLPConfig{Endpoint: "http://localhost:4318"},
			Traces: runtimeconfig.TraceConfig{
				URLTemplate: "https://jaeger.example.test/trace/{{.TraceID}}?sandbox={{.SandboxID}}",
			},
		},
	})
	client := mustNewControlClient(t, host)
	sandboxID := mustCreateSandbox(t, client)

	createResp, err := client.CreateExecution(context.Background(), &cleanroomv1.CreateExecutionRequest{
		SandboxId: sandboxID,
		Command:   []string{"echo", "ok"},
		Kind:      cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH,
	})
	if err != nil {
		t.Fatalf("CreateExecution returned error: %v", err)
	}
	executionID := createResp.GetExecution().GetExecutionId()
	if _, err := svc.WaitExecution(context.Background(), sandboxID, executionID); err != nil {
		t.Fatalf("WaitExecution returned error: %v", err)
	}

	outcome := runExecutionInspectWithCapture(ExecutionInspectCommand{
		clientFlags: clientFlags{Host: host},
		ExecutionID: executionID,
	}, runtimeContext{CWD: t.TempDir()})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("ExecutionInspectCommand.Run returned error: %v", outcome.err)
	}

	assertContainsAll(
		t,
		outcome.stdout,
		"trace_id: 0123456789abcdef0123456789abcdef",
		"trace_url: https://jaeger.example.test/trace/0123456789abcdef0123456789abcdef?sandbox="+sandboxID,
	)
}

func TestExecutionInspectCommandRejectsMutuallyExclusiveFlags(t *testing.T) {
	t.Helper()

	outcome := runExecutionInspectWithCapture(ExecutionInspectCommand{
		SandboxID:   "sbx_123",
		ExecutionID: "exe_123",
		Last:        true,
	}, runtimeContext{CWD: t.TempDir()})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(outcome.err.Error(), "choose either <execution-id> or --last") {
		t.Fatalf("unexpected validation error: %v", outcome.err)
	}
}
