package cli

import (
	"encoding/json"
	"strings"
	"testing"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

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
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()

	outcome := runSandboxCreateWithCapture(SandboxCreateCommand{
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
	host, _ := startIntegrationServer(t, &integrationAdapter{})
	cwd := t.TempDir()

	outcome := runSandboxCreateWithCapture(SandboxCreateCommand{
		clientFlags: clientFlags{Host: host},
		Chdir:       cwd,
		JSON:        true,
	}, runtimeContext{
		CWD:    cwd,
		Loader: integrationLoader{},
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
