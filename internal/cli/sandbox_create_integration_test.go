package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

func stubPolicyUpdateResolver(t *testing.T, fn func(context.Context, string) (string, error)) func() {
	t.Helper()
	prev := resolveReferenceForPolicyUpdate
	resolveReferenceForPolicyUpdate = fn
	return func() {
		resolveReferenceForPolicyUpdate = prev
	}
}

func runSandboxCreateWithCapture(cmd SandboxCreateCommand, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, nil, ctx)
}

func runCreateAliasWithCapture(cmd CreateCommand, ctx runtimeContext) execOutcome {
	return runWithCapture(func(runCtx *runtimeContext) error {
		return cmd.Run(runCtx)
	}, nil, ctx)
}

func TestSandboxCreateIntegrationPrintsSandboxID(t *testing.T) {
	restore := stubPolicyUpdateResolver(t, func(_ context.Context, source string) (string, error) {
		if got, want := source, defaultBumpRefSource; got != want {
			t.Fatalf("unexpected default sandbox image source: got %q want %q", got, want)
		}
		return testImageOverrideRef, nil
	})
	defer restore()

	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()

	outcome := runSandboxCreateWithCapture(SandboxCreateCommand{
		clientFlags: clientFlags{Host: host},
	}, runtimeContext{
		CWD:    cwd,
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("SandboxCreateCommand.Run returned error: %v", outcome.err)
	}

	id := strings.TrimSpace(outcome.stdout)
	if id == "" {
		t.Fatalf("expected sandbox id output, got %q", outcome.stdout)
	}

	client := mustNewControlClient(t, host)
	requireSandboxStatus(t, client, id, cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY)
}

func TestSandboxCreateIntegrationJSONOutput(t *testing.T) {
	restore := stubPolicyUpdateResolver(t, func(_ context.Context, source string) (string, error) {
		if got, want := source, defaultBumpRefSource; got != want {
			t.Fatalf("unexpected default sandbox image source: got %q want %q", got, want)
		}
		return testImageOverrideRef, nil
	})
	defer restore()

	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()

	outcome := runSandboxCreateWithCapture(SandboxCreateCommand{
		clientFlags: clientFlags{Host: host},
		JSON:        true,
	}, runtimeContext{
		CWD:    cwd,
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("SandboxCreateCommand.Run returned error: %v", outcome.err)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(outcome.stdout), &payload); err != nil {
		t.Fatalf("expected json output, got parse error: %v (output=%q)", err, outcome.stdout)
	}
	rawID, ok := payload["sandbox_id"].(string)
	if !ok || strings.TrimSpace(rawID) == "" {
		t.Fatalf("expected sandbox_id in JSON output, got %v", payload)
	}
}

func TestTopLevelCreateIntegrationReportsExposureSetupFailureAfterCreate(t *testing.T) {
	adapter := &snapshotIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)
	cwd := t.TempDir()

	outcome := runCreateAliasWithCapture(CreateCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		Expose:      []string{"3000"},
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected CreateCommand.Run to fail during exposure setup")
	}
	if !strings.Contains(outcome.err.Error(), "does not support sandbox port dialing") {
		t.Fatalf("unexpected CreateCommand.Run error: %v", outcome.err)
	}
	if strings.TrimSpace(outcome.stdout) == "" {
		t.Fatalf("expected sandbox id before exposure setup error, got %q", outcome.stdout)
	}

	adapter.mu.Lock()
	provisionCalls := adapter.provisionCalls
	terminateCalls := adapter.terminateCalls
	adapter.mu.Unlock()
	if got, want := provisionCalls, 1; got != want {
		t.Fatalf("expected one sandbox provision, got %d", got)
	}
	if got, want := terminateCalls, 0; got != want {
		t.Fatalf("expected create to leave the created sandbox for inspection, got %d terminations", got)
	}
}

func TestTopLevelCreatePrevalidatesConfiguredExposureBeforeCreate(t *testing.T) {
	cwd := t.TempDir()

	outcome := runCreateAliasWithCapture(CreateCommand{
		Chdir:       cwd,
		ExposeHTTPS: []string{""},
	}, runtimeContext{
		CWD:    cwd,
		Loader: &configuredExposureLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected CreateCommand.Run to fail before creating a sandbox")
	}
	if !strings.Contains(outcome.err.Error(), "requires expose.https") {
		t.Fatalf("unexpected CreateCommand.Run error: %v", outcome.err)
	}
	if strings.TrimSpace(outcome.stdout) != "" {
		t.Fatalf("expected no sandbox id on stdout, got %q", outcome.stdout)
	}
	if strings.Contains(outcome.stderr, "sandbox_id=") {
		t.Fatalf("expected no sandbox id on stderr before create, got %q", outcome.stderr)
	}
}

func TestSandboxCreateIntegrationDangerouslyAllowAllSetsAllowNetworkDefault(t *testing.T) {
	restore := stubPolicyUpdateResolver(t, func(_ context.Context, source string) (string, error) {
		if got, want := source, defaultBumpRefSource; got != want {
			t.Fatalf("unexpected default sandbox image source: got %q want %q", got, want)
		}
		return testImageOverrideRef, nil
	})
	defer restore()

	adapter := &snapshotIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)

	outcome := runSandboxCreateWithCapture(SandboxCreateCommand{
		clientFlags:         clientFlags{Host: host},
		DangerouslyAllowAll: true,
	}, runtimeContext{
		CWD:    t.TempDir(),
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("SandboxCreateCommand.Run returned error: %v", outcome.err)
	}
	if got := strings.TrimSpace(outcome.stdout); got == "" {
		t.Fatalf("expected sandbox id output, got %q", outcome.stdout)
	}
	if got, want := adapter.provisionReq.Policy.NetworkDefault, "allow"; got != want {
		t.Fatalf("unexpected provisioned network default: got %q want %q", got, want)
	}
}

func TestSandboxCreateIntegrationDockerRequiresGuestService(t *testing.T) {
	restore := stubPolicyUpdateResolver(t, func(_ context.Context, source string) (string, error) {
		if got, want := source, defaultBumpRefSource; got != want {
			t.Fatalf("unexpected default sandbox image source: got %q want %q", got, want)
		}
		return testImageOverrideRef, nil
	})
	defer restore()

	adapter := &snapshotIntegrationAdapter{}
	host, _ := startIntegrationServer(t, adapter)

	outcome := runSandboxCreateWithCapture(SandboxCreateCommand{
		clientFlags: clientFlags{Host: host},
		Docker:      true,
	}, runtimeContext{
		CWD:    t.TempDir(),
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("SandboxCreateCommand.Run returned error: %v", outcome.err)
	}
	if got := strings.TrimSpace(outcome.stdout); got == "" {
		t.Fatalf("expected sandbox id output, got %q", outcome.stdout)
	}
	if adapter.provisionReq.Policy == nil {
		t.Fatal("expected provisioned policy")
	}
	if !adapter.provisionReq.Policy.Docker.Required {
		t.Fatal("expected provisioned docker service to be required")
	}
}

func TestSandboxCreateIntegrationRejectsDangerouslyAllowAllWithSnapshot(t *testing.T) {
	outcome := runSandboxCreateWithCapture(SandboxCreateCommand{
		From:                "snap_123",
		DangerouslyAllowAll: true,
	}, runtimeContext{
		CWD:    t.TempDir(),
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected SandboxCreateCommand.Run to fail when both --from and --dangerously-allow-all are set")
	}
	if got, want := outcome.err.Error(), "--dangerously-allow-all cannot be used with --from"; !strings.Contains(got, want) {
		t.Fatalf("expected error to contain %q, got %q", want, got)
	}
}

func TestSandboxCreateIntegrationRejectsDockerWithSnapshot(t *testing.T) {
	outcome := runSandboxCreateWithCapture(SandboxCreateCommand{
		From:   "snap_123",
		Docker: true,
	}, runtimeContext{
		CWD:    t.TempDir(),
		Loader: failingLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err == nil {
		t.Fatal("expected SandboxCreateCommand.Run to fail when both --from and --docker are set")
	}
	if got, want := outcome.err.Error(), "--docker cannot be used with --from"; !strings.Contains(got, want) {
		t.Fatalf("expected error to contain %q, got %q", want, got)
	}
}

func TestCreateAliasIntegrationPrintsSandboxID(t *testing.T) {
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()

	outcome := runCreateAliasWithCapture(CreateCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
	})
	if outcome.cause != nil {
		t.Fatalf("capture failure: %v", outcome.cause)
	}
	if outcome.err != nil {
		t.Fatalf("CreateCommand.Run returned error: %v", outcome.err)
	}
	if strings.TrimSpace(outcome.stdout) == "" {
		t.Fatalf("expected sandbox id output, got %q", outcome.stdout)
	}
}
