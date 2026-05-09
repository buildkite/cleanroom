//go:build darwin

package darwinvz

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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
	helperTimingMS := map[string]int64{
		"vm_ready":    321,
		"proxy_ready": 45,
	}
	helperTimingMS[darwinVZTimingRootFSBaseVolumePrepare] = 11
	helperTimingMS[darwinVZTimingRootFSWritableVolumeCreateClone] = 12
	helperTimingMS[darwinVZTimingRootFSMinimumSizeResize] = 13
	helperTimingMS[darwinVZTimingRootFSInspectValidate] = 14

	obs := darwinVZRunObservation{
		ExecutionID:    "run-123",
		Backend:        "darwin-vz",
		LaunchedVM:     true,
		RunDir:         runDir,
		RootFSCopyMS:   27,
		HelperTimingMS: helperTimingMS,
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
	if got, want := helperTimingPayload[darwinVZTimingRootFSBaseVolumePrepare], float64(11); got != want {
		t.Fatalf("unexpected helper_timing_ms.%s: got %v want %v", darwinVZTimingRootFSBaseVolumePrepare, got, want)
	}
	if got, want := helperTimingPayload[darwinVZTimingRootFSWritableVolumeCreateClone], float64(12); got != want {
		t.Fatalf("unexpected helper_timing_ms.%s: got %v want %v", darwinVZTimingRootFSWritableVolumeCreateClone, got, want)
	}
	if got, want := helperTimingPayload[darwinVZTimingRootFSMinimumSizeResize], float64(13); got != want {
		t.Fatalf("unexpected helper_timing_ms.%s: got %v want %v", darwinVZTimingRootFSMinimumSizeResize, got, want)
	}
	if got, want := helperTimingPayload[darwinVZTimingRootFSInspectValidate], float64(14); got != want {
		t.Fatalf("unexpected helper_timing_ms.%s: got %v want %v", darwinVZTimingRootFSInspectValidate, got, want)
	}
}

func TestApplyDarwinVZHelperTimingsMergesRootFSPhaseTimings(t *testing.T) {
	t.Parallel()

	obs := darwinVZRunObservation{}
	rootFSTimingMS := map[string]int64{
		darwinVZTimingRootFSBaseVolumePrepare: 11,
	}
	applyDarwinVZHelperTimings(&obs, rootFSTimingMS)

	helperTimingMS := map[string]int64{
		"vm_ready": 321,
	}
	applyDarwinVZHelperTimings(&obs, helperTimingMS)
	helperTimingMS["vm_ready"] = 1

	if got, want := obs.HelperTimingMS[darwinVZTimingRootFSBaseVolumePrepare], int64(11); got != want {
		t.Fatalf("unexpected rootfs phase timing: got %v want %v", got, want)
	}
	if got, want := obs.HelperTimingMS["vm_ready"], int64(321); got != want {
		t.Fatalf("unexpected vm_ready helper timing: got %v want %v", got, want)
	}
	if got, want := obs.VMReadyMS, int64(321); got != want {
		t.Fatalf("unexpected VMReadyMS: got %v want %v", got, want)
	}
}

func TestRecordLaunchPhaseObservabilityAddsTraceEvents(t *testing.T) {
	t.Parallel()

	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(recorder)
	defer func() { _ = tracerProvider.Shutdown(context.Background()) }()

	ctx, span := tracerProvider.Tracer("test").Start(context.Background(), "cleanroom.test")
	adapter := &Adapter{}
	adapter.recordLaunchPhaseObservability(ctx, darwinVZRunObservation{
		RootFSCopyMS: 27,
		VMReadyMS:    321,
		HelperTimingMS: map[string]int64{
			darwinVZTimingGuestExecReadyProbe: 401,
			"zero":                            0,
		},
	})
	span.End()

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected one ended span, got %d", len(spans))
	}
	requireLaunchPhaseEvent(t, spans[0], "rootfs_prepare", "27")
	requireLaunchPhaseEvent(t, spans[0], "guest_wait_ready", "321")
	requireLaunchPhaseEvent(t, spans[0], "helper_"+darwinVZTimingGuestExecReadyProbe, "401")
	requireNoLaunchPhaseEvent(t, spans[0], "helper_zero")
}

func TestWriteDarwinVZRunObservationIncludesNetworkMetadata(t *testing.T) {
	t.Parallel()

	runDir := t.TempDir()
	obs := darwinVZRunObservation{
		ExecutionID:       "run-123",
		Backend:           "darwin-vz",
		LaunchedVM:        true,
		RunDir:            runDir,
		NetworkMode:       darwinVZNetworkModeFileHandle,
		NetworkSubnetCIDR: "10.233.0.0/24",
		NetworkGuestIP:    "10.233.0.2",
		NetworkGatewayIP:  "10.233.0.1",
		NetworkPrefixLen:  24,
	}

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
	if got, want := payload["network_mode"], darwinVZNetworkModeFileHandle; got != want {
		t.Fatalf("unexpected network_mode: got %v want %v", got, want)
	}
	if got, want := payload["network_subnet_cidr"], "10.233.0.0/24"; got != want {
		t.Fatalf("unexpected network_subnet_cidr: got %v want %v", got, want)
	}
	if got, want := payload["network_guest_ip"], "10.233.0.2"; got != want {
		t.Fatalf("unexpected network_guest_ip: got %v want %v", got, want)
	}
	if got, want := payload["network_gateway_ip"], "10.233.0.1"; got != want {
		t.Fatalf("unexpected network_gateway_ip: got %v want %v", got, want)
	}
	if got, want := payload["network_prefix_len"], float64(24); got != want {
		t.Fatalf("unexpected network_prefix_len: got %v want %v", got, want)
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
						"vm_ready":                            321,
						darwinVZTimingRootFSBaseVolumePrepare: 11,
					},
					Network: &darwinVZNetworkMetadata{
						Mode:       darwinVZNetworkModeFileHandle,
						SubnetCIDR: "10.233.0.0/24",
						GuestIP:    "10.233.0.2",
						GatewayIP:  "10.233.0.1",
						PrefixLen:  24,
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
	helperTimingPayload, ok := payload["helper_timing_ms"].(map[string]any)
	if !ok {
		t.Fatalf("expected helper_timing_ms object, got %T", payload["helper_timing_ms"])
	}
	if got, want := helperTimingPayload[darwinVZTimingRootFSBaseVolumePrepare], float64(11); got != want {
		t.Fatalf("unexpected helper_timing_ms.%s: got %v want %v", darwinVZTimingRootFSBaseVolumePrepare, got, want)
	}
	if got, want := payload["network_guest_ip"], "10.233.0.2"; got != want {
		t.Fatalf("unexpected network_guest_ip: got %v want %v", got, want)
	}
}

func requireLaunchPhaseEvent(t *testing.T, span sdktrace.ReadOnlySpan, phase, durationMS string) {
	t.Helper()
	for _, event := range span.Events() {
		if event.Name != observability.EventLaunchPhase {
			continue
		}
		if eventAttributeValue(event, observability.AttrLaunchPhase) == phase &&
			eventAttributeValue(event, observability.AttrLaunchPhaseDurationMS) == durationMS &&
			eventAttributeValue(event, observability.AttrBackend) == "darwin-vz" {
			return
		}
	}
	t.Fatalf("expected launch phase event phase=%q duration_ms=%q, got events %#v", phase, durationMS, span.Events())
}

func requireNoLaunchPhaseEvent(t *testing.T, span sdktrace.ReadOnlySpan, phase string) {
	t.Helper()
	for _, event := range span.Events() {
		if event.Name == observability.EventLaunchPhase && eventAttributeValue(event, observability.AttrLaunchPhase) == phase {
			t.Fatalf("unexpected launch phase event phase=%q", phase)
		}
	}
}

func eventAttributeValue(event sdktrace.Event, key string) string {
	for _, attr := range event.Attributes {
		if string(attr.Key) == key {
			return fmt.Sprint(attr.Value.AsInterface())
		}
	}
	return ""
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
