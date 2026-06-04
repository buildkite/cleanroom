package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/storagegc"
)

func TestParseSystemPruneDuration(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{input: "", want: 0},
		{input: "24h", want: 24 * time.Hour},
		{input: "7d", want: 7 * 24 * time.Hour},
		{input: "1.5d", want: 36 * time.Hour},
	}
	for _, tt := range tests {
		got, err := parseSystemPruneDuration(tt.input)
		if err != nil {
			t.Fatalf("parseSystemPruneDuration(%q) returned error: %v", tt.input, err)
		}
		if got != tt.want {
			t.Fatalf("parseSystemPruneDuration(%q) = %s, want %s", tt.input, got, tt.want)
		}
	}
}

func TestParseSystemPruneDurationRejectsInvalidValue(t *testing.T) {
	tests := []string{"soon", "-1h", "NaNd", "Infd", "1e100d"}
	for _, input := range tests {
		if _, err := parseSystemPruneDuration(input); err == nil {
			t.Fatalf("expected %q to fail", input)
		}
	}
}

func TestSystemPruneOlderThanDoesNotOverrideExecutionRetention(t *testing.T) {
	previousListSandboxIDs := systemListSandboxIDs
	previousInventory := systemInventory
	previousPlanPrune := systemPlanPrune
	t.Cleanup(func() {
		systemListSandboxIDs = previousListSandboxIDs
		systemInventory = previousInventory
		systemPlanPrune = previousPlanPrune
	})

	systemListSandboxIDs = func(context.Context, *runtimeContext, clientFlags) ([]string, bool, error) {
		return nil, true, nil
	}
	var gotExecutionMaxAge time.Duration
	systemInventory = func(_ context.Context, opts storagegc.InventoryOptions) (storagegc.Report, error) {
		gotExecutionMaxAge = opts.ExecutionMaxAge
		return storagegc.Report{GeneratedAt: time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)}, nil
	}
	var gotOlderThan time.Duration
	systemPlanPrune = func(_ storagegc.Report, opts storagegc.PruneOptions) storagegc.Plan {
		gotOlderThan = opts.OlderThan
		return storagegc.Plan{}
	}

	stdout, _ := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := (&SystemPruneCommand{DryRun: true, OlderThan: "7d"}).Run(&runtimeContext{Stdout: stdout})
	if err != nil {
		t.Fatalf("SystemPruneCommand.Run returned error: %v", err)
	}
	if got, want := gotExecutionMaxAge, storagegc.DefaultExecutionMaxAge; got != want {
		t.Fatalf("unexpected execution retention: got %s want %s", got, want)
	}
	if got, want := gotOlderThan, 7*24*time.Hour; got != want {
		t.Fatalf("unexpected prune older-than: got %s want %s", got, want)
	}
}

type testSystemZFSImportDatasetStore struct{}

func (testSystemZFSImportDatasetStore) ListZFSImportDatasets(context.Context) ([]string, error) {
	return nil, nil
}

func (testSystemZFSImportDatasetStore) DestroyZFSImportDataset(context.Context, string) error {
	return nil
}

func writeSystemTestFile(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	return path
}

func TestSystemPruneThreadsZFSImportDatasetStore(t *testing.T) {
	previousListSandboxIDs := systemListSandboxIDs
	previousInventory := systemInventory
	previousPlanPrune := systemPlanPrune
	previousExecutePrune := systemExecutePrune
	previousZFSImportDatasetStore := systemZFSImportDatasetStore
	t.Cleanup(func() {
		systemListSandboxIDs = previousListSandboxIDs
		systemInventory = previousInventory
		systemPlanPrune = previousPlanPrune
		systemExecutePrune = previousExecutePrune
		systemZFSImportDatasetStore = previousZFSImportDatasetStore
	})

	store := testSystemZFSImportDatasetStore{}
	systemZFSImportDatasetStore = func(runtimeconfig.Config) storagegc.ZFSImportDatasetStore {
		return store
	}
	systemListSandboxIDs = func(context.Context, *runtimeContext, clientFlags) ([]string, bool, error) {
		return nil, true, nil
	}
	systemInventory = func(_ context.Context, opts storagegc.InventoryOptions) (storagegc.Report, error) {
		if opts.ZFSImportDatasetStore != store {
			t.Fatalf("inventory zfs import dataset store = %#v, want %#v", opts.ZFSImportDatasetStore, store)
		}
		return storagegc.Report{GeneratedAt: time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)}, nil
	}
	systemPlanPrune = func(storagegc.Report, storagegc.PruneOptions) storagegc.Plan {
		return storagegc.Plan{Actions: []storagegc.Action{{
			Kind: storagegc.KindZFSImportDataset,
			ID:   "stale",
		}}}
	}
	systemExecutePrune = func(_ context.Context, _ storagegc.Report, _ storagegc.Plan, opts storagegc.ExecuteOptions) (storagegc.Result, error) {
		if opts.ZFSImportDatasetStore != store {
			t.Fatalf("execute zfs import dataset store = %#v, want %#v", opts.ZFSImportDatasetStore, store)
		}
		return storagegc.Result{DeletedEntries: 1}, nil
	}

	stdout, _ := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	if err := (&SystemPruneCommand{Force: true}).Run(&runtimeContext{Stdout: stdout}); err != nil {
		t.Fatalf("SystemPruneCommand.Run returned error: %v", err)
	}
}

type systemWarmupTestAdapter struct {
	name    string
	warmup  func(context.Context, backend.WarmupRequest) (*backend.WarmupResult, error)
	warmups int
}

func (a *systemWarmupTestAdapter) Name() string {
	if a.name != "" {
		return a.name
	}
	return "test"
}

func (a *systemWarmupTestAdapter) ProvisionSandbox(context.Context, backend.ProvisionRequest) error {
	return nil
}

func (a *systemWarmupTestAdapter) RunInSandbox(context.Context, backend.ExecutionRequest, backend.OutputStream) (*backend.ExecutionResult, error) {
	return &backend.ExecutionResult{}, nil
}

func (a *systemWarmupTestAdapter) TerminateSandbox(context.Context, string) error {
	return nil
}

func (a *systemWarmupTestAdapter) Warmup(ctx context.Context, req backend.WarmupRequest) (*backend.WarmupResult, error) {
	a.warmups++
	if a.warmup != nil {
		return a.warmup(ctx, req)
	}
	return &backend.WarmupResult{Backend: a.Name()}, nil
}

type systemWarmupUnsupportedAdapter struct{}

func (systemWarmupUnsupportedAdapter) Name() string { return "unsupported" }

func (systemWarmupUnsupportedAdapter) ProvisionSandbox(context.Context, backend.ProvisionRequest) error {
	return nil
}

func (systemWarmupUnsupportedAdapter) RunInSandbox(context.Context, backend.ExecutionRequest, backend.OutputStream) (*backend.ExecutionResult, error) {
	return &backend.ExecutionResult{}, nil
}

func (systemWarmupUnsupportedAdapter) TerminateSandbox(context.Context, string) error {
	return nil
}

func TestSystemWarmupUsesConfiguredDefaultBackend(t *testing.T) {
	rootFS := writeSystemTestFile(t, "rootfs.ext4")
	adapter := &systemWarmupTestAdapter{name: "darwin-vz"}
	adapter.warmup = func(_ context.Context, req backend.WarmupRequest) (*backend.WarmupResult, error) {
		if got, want := req.FirecrackerConfig.RootFSPath, rootFS; got != want {
			t.Fatalf("unexpected warmup rootfs config: got %q want %q", got, want)
		}
		if req.ImageRef != "" {
			t.Fatalf("expected no image ref for configured rootfs, got %q", req.ImageRef)
		}
		return &backend.WarmupResult{
			Backend:          "darwin-vz",
			KernelPath:       "/tmp/kernel",
			KernelStatus:     backend.WarmupStatusConfigured,
			RootFSPath:       rootFS,
			RootFSStatus:     backend.WarmupStatusConfigured,
			BaseRootFSRef:    rootFS,
			BaseRootFSStatus: backend.WarmupStatusReady,
		}, nil
	}

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := (&SystemWarmupCommand{}).Run(&runtimeContext{
		Stdout: stdout,
		Config: runtimeconfig.Config{
			DefaultBackend: "darwin-vz",
			Backends: runtimeconfig.Backends{
				DarwinVZ: runtimeconfig.DarwinVZConfig{RootFS: rootFS},
			},
		},
		Backends: map[string]backend.Adapter{
			"darwin-vz": adapter,
		},
	})
	if err != nil {
		t.Fatalf("SystemWarmupCommand.Run returned error: %v", err)
	}
	if adapter.warmups != 1 {
		t.Fatalf("expected one warmup call, got %d", adapter.warmups)
	}
	assertContainsAll(t, readStdout(), "warmed system", "backend=darwin-vz", "kernel_path=/tmp/kernel", "rootfs_status=configured", "base_rootfs_status=ready")
}

func TestSystemWarmupResolvesDefaultImageWhenRootFSMissing(t *testing.T) {
	previousResolve := resolveReferenceForPolicyUpdate
	t.Cleanup(func() {
		resolveReferenceForPolicyUpdate = previousResolve
	})

	const resolvedRef = "ghcr.io/buildkite/cleanroom-base/alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var gotSource string
	resolveReferenceForPolicyUpdate = func(_ context.Context, source string) (string, error) {
		gotSource = source
		return resolvedRef, nil
	}

	adapter := &systemWarmupTestAdapter{name: "darwin-vz"}
	adapter.warmup = func(_ context.Context, req backend.WarmupRequest) (*backend.WarmupResult, error) {
		if got, want := req.ImageRef, resolvedRef; got != want {
			t.Fatalf("unexpected warmup image ref: got %q want %q", got, want)
		}
		return &backend.WarmupResult{
			Backend:      "darwin-vz",
			RootFSPath:   "/tmp/prepared.ext4",
			RootFSStatus: backend.WarmupStatusPrepared,
			ImageRef:     req.ImageRef,
			ImageDigest:  "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}, nil
	}

	stdout, _ := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := (&SystemWarmupCommand{Backend: "darwin-vz"}).Run(&runtimeContext{
		Stdout: stdout,
		Config: runtimeconfig.Config{
			Backends: runtimeconfig.Backends{
				DarwinVZ: runtimeconfig.DarwinVZConfig{RootFS: "/tmp/missing-cleanroom-rootfs.ext4"},
			},
		},
		Backends: map[string]backend.Adapter{
			"darwin-vz": adapter,
		},
	})
	if err != nil {
		t.Fatalf("SystemWarmupCommand.Run returned error: %v", err)
	}
	if got, want := gotSource, defaultBumpRefSource; got != want {
		t.Fatalf("unexpected default image source: got %q want %q", got, want)
	}
}

func TestSystemWarmupJSONOutput(t *testing.T) {
	rootFS := writeSystemTestFile(t, "rootfs.ext4")
	adapter := &systemWarmupTestAdapter{name: "darwin-vz"}
	adapter.warmup = func(context.Context, backend.WarmupRequest) (*backend.WarmupResult, error) {
		return &backend.WarmupResult{
			Backend:       "darwin-vz",
			KernelPath:    "/tmp/kernel",
			KernelStatus:  backend.WarmupStatusConfigured,
			RootFSPath:    rootFS,
			RootFSStatus:  backend.WarmupStatusConfigured,
			BaseRootFSRef: rootFS,
			ImageRef:      "ghcr.io/buildkite/cleanroom-base/alpine@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			ImageDigest:   "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}, nil
	}

	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := (&SystemWarmupCommand{Backend: "darwin-vz", JSON: true}).Run(&runtimeContext{
		Stdout: stdout,
		Config: runtimeconfig.Config{
			Backends: runtimeconfig.Backends{
				DarwinVZ: runtimeconfig.DarwinVZConfig{RootFS: rootFS},
			},
		},
		Backends: map[string]backend.Adapter{
			"darwin-vz": adapter,
		},
	})
	if err != nil {
		t.Fatalf("SystemWarmupCommand.Run returned error: %v", err)
	}

	var payload backend.WarmupResult
	if err := json.Unmarshal([]byte(readStdout()), &payload); err != nil {
		t.Fatalf("unmarshal warmup JSON: %v", err)
	}
	if got, want := payload.Backend, "darwin-vz"; got != want {
		t.Fatalf("unexpected backend: got %q want %q", got, want)
	}
	if got, want := payload.RootFSStatus, backend.WarmupStatusConfigured; got != want {
		t.Fatalf("unexpected rootfs status: got %q want %q", got, want)
	}
}

func TestSystemWarmupRejectsUnsupportedBackend(t *testing.T) {
	stdout, _ := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := (&SystemWarmupCommand{Backend: "firecracker"}).Run(&runtimeContext{
		Stdout: stdout,
		Backends: map[string]backend.Adapter{
			"firecracker": systemWarmupUnsupportedAdapter{},
		},
	})
	if err == nil {
		t.Fatal("expected unsupported backend error")
	}
	if !strings.Contains(err.Error(), `backend "firecracker" does not support system warmup`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWriteSystemDFPrintsTotals(t *testing.T) {
	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := writeSystemDF(stdout, storagegc.Report{
		StateBaseDir: "/state/cleanroom",
		CacheBaseDir: "/cache/cleanroom",
		Entries: []storagegc.Entry{
			{Kind: storagegc.KindSandboxRuntime, ID: "orphan", SizeBytes: 2048, Reclaimable: true},
		},
		Totals: map[string]storagegc.CategoryTotal{
			storagegc.KindSandboxRuntime: {
				Count:            1,
				TotalBytes:       2048,
				ReclaimableBytes: 2048,
			},
		},
	})
	if err != nil {
		t.Fatalf("writeSystemDF returned error: %v", err)
	}
	output := readStdout()
	assertContainsAll(t, output, "state: /state/cleanroom", "cache: /cache/cleanroom", "sandbox-runtime", "2.0 KiB")
}

func TestWriteSystemPruneSummaryPrintsGroupedActions(t *testing.T) {
	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := writeSystemPruneSummary(stdout, storagegc.Plan{
		ReclaimableBytes: 8192,
		Actions: []storagegc.Action{
			{
				Kind:      storagegc.KindOrphanSnapshot,
				ID:        "snap-orphan",
				Path:      "/state/cleanroom/snapshots/darwin-vz/snap-orphan",
				SizeBytes: 4096,
				Reason:    "not referenced",
			},
			{
				Kind:      storagegc.KindOrphanSnapshot,
				ID:        "snap-other",
				Path:      "/state/cleanroom/snapshots/darwin-vz/snap-other",
				SizeBytes: 2048,
				Reason:    "not referenced",
			},
			{
				Kind:      storagegc.KindSandboxRuntime,
				ID:        "cr-orphan",
				Path:      "/state/cleanroom/sandboxes/cr-orphan",
				SizeBytes: 2048,
				Reason:    "not present",
			},
		},
	}, true)
	if err != nil {
		t.Fatalf("writeSystemPruneSummary returned error: %v", err)
	}
	output := readStdout()
	assertContainsAll(t, output, "would reclaim 8.0 KiB", "KIND", "ITEMS", "RECLAIMABLE", "orphan-snapshot", "2", "6.0 KiB", "sandbox-runtime", "1", "2.0 KiB", "Use --table")
	if strings.Contains(output, "snap-orphan") {
		t.Fatalf("summary output should not include per-action IDs, got %q", output)
	}
	if strings.Contains(output, "will reclaim") {
		t.Fatalf("dry-run output should say would reclaim, got %q", output)
	}
}

func TestWriteSystemPruneTablePrintsActions(t *testing.T) {
	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := writeSystemPruneTable(stdout, storagegc.Plan{
		ReclaimableBytes: 4096,
		Actions: []storagegc.Action{
			{
				Kind:      storagegc.KindOrphanSnapshot,
				ID:        "snap-orphan",
				Path:      "/state/cleanroom/snapshots/darwin-vz/snap-orphan",
				SizeBytes: 4096,
				Reason:    "not referenced",
			},
		},
	}, "would reclaim")
	if err != nil {
		t.Fatalf("writeSystemPruneTable returned error: %v", err)
	}
	output := readStdout()
	assertContainsAll(t, output, "would reclaim 4.0 KiB", "orphan-snapshot", "snap-orphan", "not referenced")
}

func TestWriteSystemPruneJSONIncludesSummaryAndActions(t *testing.T) {
	stdout, readStdout := makeStdoutCapture(t)
	t.Cleanup(func() { _ = stdout.Close() })

	err := writeSystemPruneJSON(stdout, storagegc.Plan{
		ReclaimableBytes: 4096,
		Actions: []storagegc.Action{
			{
				Kind:      storagegc.KindOrphanSnapshot,
				ID:        "snap-orphan",
				Path:      "/state/cleanroom/snapshots/darwin-vz/snap-orphan",
				SizeBytes: 4096,
				Reason:    "not referenced",
			},
		},
	}, true, nil)
	if err != nil {
		t.Fatalf("writeSystemPruneJSON returned error: %v", err)
	}
	var payload systemPruneJSONOutput
	if err := json.Unmarshal([]byte(readStdout()), &payload); err != nil {
		t.Fatalf("unmarshal prune JSON: %v", err)
	}
	if !payload.DryRun {
		t.Fatal("expected dry_run to be true")
	}
	if got, want := payload.Entries, 1; got != want {
		t.Fatalf("unexpected entries: got %d want %d", got, want)
	}
	if got, want := len(payload.Summary), 1; got != want {
		t.Fatalf("unexpected summary rows: got %d want %d", got, want)
	}
	if got, want := len(payload.Actions), 1; got != want {
		t.Fatalf("unexpected actions: got %d want %d", got, want)
	}
}
