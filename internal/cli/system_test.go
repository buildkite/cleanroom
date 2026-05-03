package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
