package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	backendfirecracker "github.com/buildkite/cleanroom/internal/backend/firecracker"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/storagegc"
	"github.com/buildkite/cleanroom/internal/volumestore"
	"golang.org/x/term"
)

type SystemCommand struct {
	DF     SystemDFCommand     `name:"df" cmd:"" help:"Show cleanroom host storage usage"`
	Prune  SystemPruneCommand  `name:"prune" cmd:"" help:"Remove reclaimable cleanroom host storage"`
	Warmup SystemWarmupCommand `name:"warmup" cmd:"" help:"Prepare backend assets used before the first sandbox"`
}

type SystemDFCommand struct {
	clientFlags
	JSON bool `help:"Print storage inventory as JSON"`
}

type SystemPruneCommand struct {
	clientFlags
	DryRun    bool   `name:"dry-run" help:"Show what would be removed without deleting anything"`
	Force     bool   `help:"Do not prompt before deleting"`
	All       bool   `help:"Include system-managed caches such as stage caches, runtime rootfs caches, and image caches"`
	Table     bool   `help:"Print every prune action as a table"`
	JSON      bool   `help:"Print prune plan and result as JSON"`
	OlderThan string `name:"older-than" help:"Include eligible cache entries older than this duration (for example 24h or 7d)"`
}

type SystemWarmupCommand struct {
	Backend string `help:"Execution backend to warm (defaults to runtime config or host default)"`
	JSON    bool   `help:"Print warmup result as JSON"`
}

var systemInventory = storagegc.Inventory
var systemPlanPrune = storagegc.PlanPrune
var systemExecutePrune = storagegc.ExecutePrune
var systemListSandboxIDs = listSystemSandboxIDs
var systemZFSImportDatasetStore = defaultSystemZFSImportDatasetStore
var systemCachePeerTransferDriver = defaultSystemCachePeerTransferDriver
var systemIsTerminal = func(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

func (c *SystemDFCommand) Run(ctx *runtimeContext) error {
	zfsImportDatasetStore := systemZFSImportDatasetStore(ctx.Config)
	report, warning, err := loadSystemInventory(context.Background(), ctx, c.clientFlags, false, 0, zfsImportDatasetStore)
	if err != nil {
		return err
	}
	if warning != nil {
		_, _ = fmt.Fprintf(ctx.stderr(), "warning: %v\n", warning)
	}
	if c.JSON {
		enc := json.NewEncoder(stdout(ctx))
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	return writeSystemDF(stdout(ctx), report)
}

func (c *SystemPruneCommand) Run(ctx *runtimeContext) error {
	olderThan, err := parseSystemPruneDuration(c.OlderThan)
	if err != nil {
		return err
	}
	zfsImportDatasetStore := systemZFSImportDatasetStore(ctx.Config)
	report, _, err := loadSystemInventory(context.Background(), ctx, c.clientFlags, true, 0, zfsImportDatasetStore)
	if err != nil {
		return err
	}
	plan := systemPlanPrune(report, storagegc.PruneOptions{
		All:       c.All,
		OlderThan: olderThan,
		Now:       report.GeneratedAt,
	})
	if c.JSON && c.DryRun {
		return writeSystemPruneJSON(stdout(ctx), plan, true, nil)
	}
	if len(plan.Actions) == 0 {
		if c.JSON {
			return writeSystemPruneJSON(stdout(ctx), plan, c.DryRun, nil)
		}
		_, err := fmt.Fprintln(stdout(ctx), "nothing to prune")
		return err
	}

	if c.DryRun {
		if c.Table {
			return writeSystemPruneTable(stdout(ctx), plan, "would reclaim")
		}
		return writeSystemPruneSummary(stdout(ctx), plan, true)
	}
	if !c.Force {
		if err := confirmSystemPrune(ctx.stderr(), os.Stdin, len(plan.Actions), plan.ReclaimableBytes); err != nil {
			return err
		}
	}

	result, err := systemExecutePrune(context.Background(), report, plan, storagegc.ExecuteOptions{
		ZFSImportDatasetStore: zfsImportDatasetStore,
	})
	if err != nil {
		return err
	}
	if c.JSON {
		return writeSystemPruneJSON(stdout(ctx), plan, false, &result)
	}
	if c.Table {
		return writeSystemPruneTable(stdout(ctx), plan, "reclaimed")
	}
	_, err = fmt.Fprintf(stdout(ctx), "reclaimed %s from %d entries\n", formatStorageBytes(result.ReclaimedBytes), result.DeletedEntries)
	return err
}

func (c *SystemWarmupCommand) Run(ctx *runtimeContext) error {
	backendName := resolveBackendName(c.Backend, ctx.Config.DefaultBackend)
	adapter, ok := ctx.Backends[backendName]
	if !ok {
		return fmt.Errorf("unknown backend %q", backendName)
	}
	warmer, ok := adapter.(backend.WarmupAdapter)
	if !ok {
		return fmt.Errorf("backend %q does not support system warmup", backendName)
	}

	cfg := runtimeconfig.MergeBackendConfig(ctx.Config, backendName, 0)
	imageRef, err := resolveSystemWarmupImageRef(context.Background(), cfg)
	if err != nil {
		return err
	}
	result, err := warmer.Warmup(context.Background(), backend.WarmupRequest{
		FirecrackerConfig: cfg,
		ImageRef:          imageRef,
	})
	if err != nil {
		return err
	}
	if result == nil {
		return errors.New("backend warmup returned empty result")
	}
	if c.JSON {
		enc := json.NewEncoder(stdout(ctx))
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	return writeSystemWarmup(stdout(ctx), *result)
}

func resolveSystemWarmupImageRef(ctx context.Context, cfg backend.FirecrackerConfig) (string, error) {
	configuredRootFS := strings.TrimSpace(cfg.RootFSPath)
	if configuredRootFS != "" {
		if _, err := os.Stat(configuredRootFS); err == nil {
			return "", nil
		}
	}
	ref, err := resolveReferenceForPolicyUpdate(ctx, defaultBumpRefSource)
	if err != nil {
		return "", fmt.Errorf("resolve default sandbox image %q: %w", defaultBumpRefSource, err)
	}
	return ref, nil
}

func writeSystemWarmup(w io.Writer, result backend.WarmupResult) error {
	fields := []startupField{
		{Key: "backend", Value: result.Backend},
		{Key: "kernel_path", Value: result.KernelPath},
		{Key: "kernel_status", Value: result.KernelStatus},
		{Key: "rootfs_path", Value: result.RootFSPath},
		{Key: "rootfs_status", Value: result.RootFSStatus},
		{Key: "base_rootfs_ref", Value: result.BaseRootFSRef},
		{Key: "base_rootfs_status", Value: result.BaseRootFSStatus},
		{Key: "image_ref", Value: result.ImageRef},
		{Key: "image_digest", Value: result.ImageDigest},
	}
	if result.MinimumRootFSBytes > 0 {
		fields = append(fields, startupField{Key: "minimum_rootfs_bytes", Value: strconv.FormatInt(result.MinimumRootFSBytes, 10)})
	}
	_, err := fmt.Fprint(w, renderSummaryBlock(summaryBlock{
		Title:      "warmed system",
		TitleStyle: defaultTerminalPalette().info,
		Fields:     fields,
	}, false))
	return err
}

func loadSystemInventory(ctx context.Context, runtimeCtx *runtimeContext, flags clientFlags, requireSandboxState bool, executionMaxAge time.Duration, zfsImportDatasetStore storagegc.ZFSImportDatasetStore) (storagegc.Report, error, error) {
	sandboxIDs, stateKnown, listErr := systemListSandboxIDs(ctx, runtimeCtx, flags)
	if listErr != nil && requireSandboxState {
		return storagegc.Report{}, nil, fmt.Errorf("list sandboxes before prune: %w", listErr)
	}
	if executionMaxAge <= 0 {
		executionMaxAge = storagegc.DefaultExecutionMaxAge
	}
	report, err := systemInventory(ctx, storagegc.InventoryOptions{
		Config:                runtimeCtx.Config,
		ActiveSandboxIDs:      sandboxIDs,
		SandboxStateKnown:     stateKnown,
		ExecutionMaxAge:       executionMaxAge,
		ZFSImportDatasetStore: zfsImportDatasetStore,
	})
	if err != nil {
		return storagegc.Report{}, nil, err
	}
	return report, listErr, nil
}

func defaultSystemZFSImportDatasetStore(cfg runtimeconfig.Config) storagegc.ZFSImportDatasetStore {
	store := backendfirecracker.NewZFSImportDatasetStore(runtimeconfig.MergeBackendConfig(cfg, "firecracker", 0))
	if store == nil {
		return nil
	}
	return store
}

func defaultSystemCachePeerTransferDriver(cfg runtimeconfig.Config) volumestore.IncrementalSnapshotTransferDriver {
	driver := backendfirecracker.NewZFSIncrementalTransferDriver(runtimeconfig.MergeBackendConfig(cfg, "firecracker", 0))
	if driver == nil {
		return nil
	}
	return driver
}

func listSystemSandboxIDs(ctx context.Context, runtimeCtx *runtimeContext, flags clientFlags) ([]string, bool, error) {
	client, err := flags.connect(runtimeCtx)
	if err != nil {
		return nil, false, err
	}
	resp, err := client.ListSandboxes(ctx, &cleanroomv1.ListSandboxesRequest{})
	if err != nil {
		return nil, false, err
	}
	ids := make([]string, 0, len(resp.GetSandboxes()))
	for _, sandbox := range resp.GetSandboxes() {
		id := strings.TrimSpace(sandbox.GetSandboxId())
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, true, nil
}

func writeSystemDF(w io.Writer, report storagegc.Report) error {
	if len(report.Entries) == 0 {
		_, err := fmt.Fprintln(w, "no cleanroom storage found")
		return err
	}
	if _, err := fmt.Fprintf(w, "state: %s\ncache: %s\n", report.StateBaseDir, report.CacheBaseDir); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "CATEGORY\tITEMS\tTOTAL\tRECLAIMABLE\tPROTECTED"); err != nil {
		return err
	}
	for _, kind := range sortedTotalKinds(report.Totals) {
		total := report.Totals[kind]
		if _, err := fmt.Fprintf(
			tw,
			"%s\t%d\t%s\t%s\t%s\n",
			kind,
			total.Count,
			formatStorageBytes(total.TotalBytes),
			formatStorageBytes(total.ReclaimableBytes),
			formatStorageBytes(total.ProtectedBytes),
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

type systemPruneSummaryRow struct {
	Kind             string `json:"kind"`
	Count            int    `json:"count"`
	ReclaimableBytes int64  `json:"reclaimable_bytes"`
}

type systemPruneJSONOutput struct {
	DryRun           bool                    `json:"dry_run"`
	ReclaimableBytes int64                   `json:"reclaimable_bytes"`
	Entries          int                     `json:"entries"`
	Summary          []systemPruneSummaryRow `json:"summary"`
	Actions          []storagegc.Action      `json:"actions"`
	Result           *storagegc.Result       `json:"result,omitempty"`
}

func writeSystemPruneSummary(w io.Writer, plan storagegc.Plan, dryRun bool) error {
	verb := "will reclaim"
	if dryRun {
		verb = "would reclaim"
	}
	if _, err := fmt.Fprintf(w, "%s %s from %d entries\n", verb, formatStorageBytes(plan.ReclaimableBytes), len(plan.Actions)); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "KIND\tITEMS\tRECLAIMABLE"); err != nil {
		return err
	}
	for _, row := range summarizePrunePlan(plan) {
		if _, err := fmt.Fprintf(tw, "%s\t%d\t%s\n", row.Kind, row.Count, formatStorageBytes(row.ReclaimableBytes)); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Use --table to show every path, or --json for machine-readable details."); err != nil {
		return err
	}
	return nil
}

func writeSystemPruneTable(w io.Writer, plan storagegc.Plan, verb string) error {
	if _, err := fmt.Fprintf(w, "%s %s from %d entries\n", verb, formatStorageBytes(plan.ReclaimableBytes), len(plan.Actions)); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "KIND\tID\tSIZE\tREASON\tPATH"); err != nil {
		return err
	}
	for _, action := range sortedPruneActions(plan.Actions) {
		if _, err := fmt.Fprintf(
			tw,
			"%s\t%s\t%s\t%s\t%s\n",
			action.Kind,
			action.ID,
			formatStorageBytes(action.SizeBytes),
			action.Reason,
			action.Path,
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func writeSystemPruneJSON(w io.Writer, plan storagegc.Plan, dryRun bool, result *storagegc.Result) error {
	actions := plan.Actions
	if actions == nil {
		actions = []storagegc.Action{}
	}
	summary := summarizePrunePlan(plan)
	if summary == nil {
		summary = []systemPruneSummaryRow{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(systemPruneJSONOutput{
		DryRun:           dryRun,
		ReclaimableBytes: plan.ReclaimableBytes,
		Entries:          len(actions),
		Summary:          summary,
		Actions:          actions,
		Result:           result,
	})
}

func summarizePrunePlan(plan storagegc.Plan) []systemPruneSummaryRow {
	rowsByKind := map[string]systemPruneSummaryRow{}
	for _, action := range plan.Actions {
		row := rowsByKind[action.Kind]
		row.Kind = action.Kind
		row.Count++
		row.ReclaimableBytes += action.SizeBytes
		rowsByKind[action.Kind] = row
	}
	rows := make([]systemPruneSummaryRow, 0, len(rowsByKind))
	for _, row := range rowsByKind {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ReclaimableBytes == rows[j].ReclaimableBytes {
			return rows[i].Kind < rows[j].Kind
		}
		return rows[i].ReclaimableBytes > rows[j].ReclaimableBytes
	})
	return rows
}

func sortedPruneActions(actions []storagegc.Action) []storagegc.Action {
	out := append([]storagegc.Action(nil), actions...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].SizeBytes == out[j].SizeBytes {
			if out[i].Kind == out[j].Kind {
				return out[i].ID < out[j].ID
			}
			return out[i].Kind < out[j].Kind
		}
		return out[i].SizeBytes > out[j].SizeBytes
	})
	return out
}

func confirmSystemPrune(stderr *os.File, stdin *os.File, count int, bytes int64) error {
	if !systemIsTerminal(stdin) {
		return errors.New("refusing to prune without --force in non-interactive mode")
	}
	if _, err := fmt.Fprintf(stderr, "Prune %d cleanroom storage entries and reclaim %s? [y/N] ", count, formatStorageBytes(bytes)); err != nil {
		return err
	}
	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer != "y" && answer != "yes" {
		return errors.New("prune cancelled")
	}
	return nil
}

func parseSystemPruneDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	if d, err := time.ParseDuration(value); err == nil {
		if d < 0 {
			return 0, errors.New("--older-than must be non-negative")
		}
		return d, nil
	}
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(value, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("parse --older-than %q: %w", value, err)
		}
		if days < 0 {
			return 0, errors.New("--older-than must be non-negative")
		}
		duration := days * float64(24*time.Hour)
		if math.IsNaN(duration) || math.IsInf(duration, 0) || duration > float64(1<<63-1) {
			return 0, fmt.Errorf("parse --older-than %q: duration is too large", value)
		}
		return time.Duration(duration), nil
	}
	return 0, fmt.Errorf("parse --older-than %q: use Go duration syntax such as 24h or day syntax such as 7d", value)
}

func sortedTotalKinds(totals map[string]storagegc.CategoryTotal) []string {
	kinds := make([]string, 0, len(totals))
	for kind := range totals {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

func formatStorageBytes(bytes int64) string {
	if bytes < 0 {
		return "-" + formatStorageBytes(-bytes)
	}
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f EiB", value/unit)
}

func stdout(ctx *runtimeContext) *os.File {
	if ctx != nil && ctx.Stdout != nil {
		return ctx.Stdout
	}
	return os.Stdout
}
