//go:build darwin

package darwinvz

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestDarwinVZTimingSummaryIncludesHelperTimings(t *testing.T) {
	t.Parallel()

	got := darwinVZTimingSummary(map[string]int64{
		"vm_ready":    321,
		"proxy_ready": 45,
	})

	want := "timings proxy_ready=45ms vm_ready=321ms"
	if got != want {
		t.Fatalf("unexpected timing summary: got %q want %q", got, want)
	}
}

func TestWriteDarwinVZRunObservationIncludesHelperTimings(t *testing.T) {
	t.Parallel()

	runDir := t.TempDir()
	obs := darwinVZRunObservation{
		ExecutionID:  "run-123",
		Backend:      "darwin-vz",
		LaunchedVM:   true,
		RunDir:       runDir,
		RootFSCopyMS: 27,
		HelperTimingMS: map[string]int64{
			"vm_ready":    321,
			"proxy_ready": 45,
		},
	}
	applyDarwinVZHelperTimings(&obs, obs.HelperTimingMS)

	if err := writeDarwinVZRunObservation(runDir, &obs, 1500); err != nil {
		t.Fatalf("writeDarwinVZRunObservation returned error: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(runDir, runObservabilityFile))
	if err != nil {
		t.Fatalf("read observability file: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatalf("parse observability file: %v", err)
	}

	if got, want := payload["execution_id"], "run-123"; got != want {
		t.Fatalf("unexpected execution_id: got %v want %v", got, want)
	}
	if got, want := payload["vm_ready_ms"], float64(321); got != want {
		t.Fatalf("unexpected vm_ready_ms: got %v want %v", got, want)
	}
	if got, want := payload["rootfs_copy_ms"], float64(27); got != want {
		t.Fatalf("unexpected rootfs_copy_ms: got %v want %v", got, want)
	}
	if got, want := payload["total_ms"], float64(1500); got != want {
		t.Fatalf("unexpected total_ms: got %v want %v", got, want)
	}

	helperTimingPayload, ok := payload["helper_timing_ms"].(map[string]any)
	if !ok {
		t.Fatalf("expected helper_timing_ms object, got %T", payload["helper_timing_ms"])
	}
	if got, want := helperTimingPayload["proxy_ready"], float64(45); got != want {
		t.Fatalf("unexpected helper_timing_ms.proxy_ready: got %v want %v", got, want)
	}
	if got, want := helperTimingPayload["vm_ready"], float64(321); got != want {
		t.Fatalf("unexpected helper_timing_ms.vm_ready: got %v want %v", got, want)
	}
}

func TestRunInSandboxWritesObservabilityWithPendingLaunchTimings(t *testing.T) {
	t.Parallel()

	runDir := t.TempDir()
	adapter := &Adapter{
		executeInSandboxFn: func(_ context.Context, _ context.Context, instance *sandboxInstance, req backend.ExecutionRequest, _ backend.OutputStream) (*backend.ExecutionResult, error) {
			if instance == nil || instance.SandboxID != "cr-test" {
				t.Fatalf("unexpected sandbox instance: %#v", instance)
			}
			return &backend.ExecutionResult{
				ExecutionID: req.ExecutionID,
				ExitCode:    0,
				LaunchedVM:  false,
				PlanPath:    "/tmp/fake-plan.json",
				RunDir:      runDir,
				ImageRef:    "image-ref",
				ImageDigest: "image-digest",
			}, nil
		},
		sandboxes: map[string]*sandboxInstance{
			"cr-test": {
				SandboxID: "cr-test",
				Policy:    &policy.CompiledPolicy{NetworkDefault: "deny"},
				LaunchObservability: &darwinVZLaunchObservability{
					RootFSCopyMS: 27,
					HelperTimingMS: map[string]int64{
						"vm_ready": 321,
					},
				},
			},
		},
	}

	_, err := adapter.RunInSandbox(context.Background(), backend.ExecutionRequest{
		SandboxID:   "cr-test",
		ExecutionID: "run-123",
		Command:     []string{"echo", "hello"},
		Policy:      &policy.CompiledPolicy{NetworkDefault: "deny"},
		FirecrackerConfig: backend.FirecrackerConfig{
			RunDir: runDir,
		},
	}, backend.OutputStream{})
	if err != nil {
		t.Fatalf("RunInSandbox returned error: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(runDir, runObservabilityFile))
	if err != nil {
		t.Fatalf("read observability file: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatalf("parse observability file: %v", err)
	}
	if got, want := payload["vm_ready_ms"], float64(321); got != want {
		t.Fatalf("unexpected vm_ready_ms: got %v want %v", got, want)
	}
	if got, want := payload["rootfs_copy_ms"], float64(27); got != want {
		t.Fatalf("unexpected rootfs_copy_ms: got %v want %v", got, want)
	}
}

func TestRunWritesObservabilityErrorWhenRequestedCommandWriteFails(t *testing.T) {
	t.Parallel()

	runDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(runDir, "requested-command.json"), 0o755); err != nil {
		t.Fatalf("mkdir requested-command.json: %v", err)
	}

	adapter := &Adapter{}
	_, err := adapter.run(context.Background(), backend.ExecutionRequest{
		ExecutionID: "run-write-fail",
		Command:     []string{"echo", "hello"},
		Policy:      &policy.CompiledPolicy{NetworkDefault: "deny"},
		FirecrackerConfig: backend.FirecrackerConfig{
			RunDir: runDir,
			Launch: false,
		},
	}, backend.OutputStream{})
	if err == nil {
		t.Fatal("expected run to fail")
	}

	b, readErr := os.ReadFile(filepath.Join(runDir, runObservabilityFile))
	if readErr != nil {
		t.Fatalf("read observability file: %v", readErr)
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatalf("parse observability file: %v", err)
	}
	if got := payload["error"]; got == nil || got == "" {
		t.Fatalf("expected error in observability payload, got %v", got)
	}
}
