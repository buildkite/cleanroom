package storagegc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/cachestore"
	"github.com/buildkite/cleanroom/internal/imagemgr"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
)

const (
	KindSandboxRuntime    = "sandbox-runtime"
	KindSnapshot          = "snapshot"
	KindOrphanSnapshot    = "orphan-snapshot"
	KindStageCache        = "stage-cache"
	KindImageCache        = "image-cache"
	KindRuntimeRootFS     = "runtime-rootfs-cache"
	KindRepositoryCache   = "repository-cache"
	KindContentCache      = "content-cache"
	KindExecutionArtifact = "execution-artifacts"
	KindChangesetStore    = "changesets"
	KindZFSImportDataset  = "zfs-import-dataset"
)

const DefaultExecutionMaxAge = 24 * time.Hour

type SnapshotLister interface {
	List(context.Context) ([]snapshotstore.Record, error)
}

type CacheStore interface {
	List(context.Context) ([]cachestore.Record, error)
	Delete(context.Context, string, string) error
}

type ownerCacheStore interface {
	DeleteForOwner(context.Context, string, string, string) error
}

type ImageManager interface {
	List(context.Context) ([]imagemgr.Record, error)
	Remove(context.Context, string) ([]imagemgr.Record, error)
}

type ZFSImportDatasetStore interface {
	ListZFSImportDatasets(context.Context) ([]string, error)
	DestroyZFSImportDataset(context.Context, string) error
}

type InventoryOptions struct {
	Config           runtimeconfig.Config
	ActiveSandboxIDs []string
	// SandboxRuntimeIDs limits sandbox-runtime scanning to specific sandbox ids.
	// Empty means scan every sandbox runtime directory.
	SandboxRuntimeIDs     []string
	SandboxStateKnown     bool
	SnapshotStore         SnapshotLister
	CacheStore            CacheStore
	ImageManager          ImageManager
	ZFSImportDatasetStore ZFSImportDatasetStore
	Now                   time.Time
	ExecutionMaxAge       time.Duration
	// SkipSize avoids recursive filesystem sizing. It is intended for
	// lifecycle cleanup paths where deletion is targeted and byte accounting is
	// not user-facing.
	SkipSize bool
	// Kinds limits emitted entries. Empty means all categories; selected kinds
	// may still read adjacent metadata when needed for protection checks.
	Kinds []string
}

type Entry struct {
	Kind             string    `json:"kind"`
	ID               string    `json:"id,omitempty"`
	Backend          string    `json:"backend,omitempty"`
	Stage            string    `json:"stage,omitempty"`
	CacheKey         string    `json:"cache_key,omitempty"`
	OwnerPrincipalID string    `json:"owner_principal_id,omitempty"`
	OwnerScope       string    `json:"owner_scope,omitempty"`
	Path             string    `json:"path,omitempty"`
	StorageRef       string    `json:"storage_ref,omitempty"`
	SizeBytes        int64     `json:"size_bytes"`
	Reclaimable      bool      `json:"reclaimable"`
	Reason           string    `json:"reason,omitempty"`
	ProtectedBy      []string  `json:"protected_by,omitempty"`
	CreatedAt        time.Time `json:"created_at,omitempty"`
	LastUsedAt       time.Time `json:"last_used_at,omitempty"`
}

type CategoryTotal struct {
	Count            int   `json:"count"`
	TotalBytes       int64 `json:"total_bytes"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	ProtectedBytes   int64 `json:"protected_bytes"`
}

type Report struct {
	GeneratedAt       time.Time                `json:"generated_at"`
	StateBaseDir      string                   `json:"state_base_dir"`
	CacheBaseDir      string                   `json:"cache_base_dir"`
	SnapshotRoots     []string                 `json:"snapshot_roots"`
	SandboxStateKnown bool                     `json:"sandbox_state_known"`
	Entries           []Entry                  `json:"entries"`
	Totals            map[string]CategoryTotal `json:"totals"`
}

type PruneOptions struct {
	All       bool
	OlderThan time.Duration
	Now       time.Time
}

// LifecycleOptions selects cleanup actions that are safe because the service
// just completed the owning lifecycle transition.
type LifecycleOptions struct {
	TerminatedSandboxIDs []string
	ZFSImportDatasets    bool
}

type Action struct {
	Kind      string `json:"kind"`
	ID        string `json:"id,omitempty"`
	Path      string `json:"path,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	Reason    string `json:"reason,omitempty"`
	Entry     Entry  `json:"entry"`
}

type Plan struct {
	Actions          []Action `json:"actions"`
	ReclaimableBytes int64    `json:"reclaimable_bytes"`
}

type ExecuteOptions struct {
	CacheStore            CacheStore
	ImageManager          ImageManager
	ZFSImportDatasetStore ZFSImportDatasetStore
}

type Result struct {
	DeletedEntries int   `json:"deleted_entries"`
	ReclaimedBytes int64 `json:"reclaimed_bytes"`
}

func Inventory(ctx context.Context, opts InventoryOptions) (Report, error) {
	now := opts.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	executionMaxAge := opts.ExecutionMaxAge
	if executionMaxAge <= 0 {
		executionMaxAge = DefaultExecutionMaxAge
	}

	stateBase, err := paths.StateBaseDir()
	if err != nil {
		return Report{}, fmt.Errorf("resolve state base dir: %w", err)
	}
	cacheBase, err := paths.CacheBaseDir()
	if err != nil {
		return Report{}, fmt.Errorf("resolve cache base dir: %w", err)
	}
	snapshotRoots, err := snapshotRoots(opts.Config)
	if err != nil {
		return Report{}, err
	}

	report := Report{
		GeneratedAt:       now,
		StateBaseDir:      stateBase,
		CacheBaseDir:      cacheBase,
		SnapshotRoots:     snapshotRoots,
		SandboxStateKnown: opts.SandboxStateKnown,
		Entries:           []Entry{},
		Totals:            map[string]CategoryTotal{},
	}

	includedKinds := stringSet(opts.Kinds)
	referencedSnapshotPaths := map[string][]string{}
	referencedZFSImportDatasets := map[string][]string{}
	if inventoryIncludes(includedKinds, KindSnapshot) || inventoryIncludes(includedKinds, KindStageCache) || inventoryIncludes(includedKinds, KindOrphanSnapshot) || inventoryIncludes(includedKinds, KindZFSImportDataset) {
		snapshotStore := opts.SnapshotStore
		if snapshotStore == nil {
			store, err := snapshotstore.New(snapshotstore.Options{})
			if err != nil {
				return Report{}, err
			}
			snapshotStore = store
		}
		snapshotRecords, err := snapshotStore.List(ctx)
		if err != nil {
			return Report{}, fmt.Errorf("list snapshot metadata: %w", err)
		}
		for _, record := range snapshotRecords {
			entry := entryFromSnapshotRecord(record)
			if entry.Path != "" {
				if size, err := maybePathSize(ctx, entry.Path, opts.SkipSize); err == nil {
					entry.SizeBytes = size
				} else {
					return Report{}, fmt.Errorf("size snapshot %q: %w", record.SnapshotID, err)
				}
				addPathReference(referencedSnapshotPaths, entry.Path, "snapshot metadata "+record.SnapshotID)
			}
			addZFSImportDatasetReference(referencedZFSImportDatasets, record.StorageRef, "snapshot metadata "+record.SnapshotID)
			if inventoryIncludes(includedKinds, KindSnapshot) {
				report.Entries = append(report.Entries, entry)
			}
		}
	}

	if inventoryIncludes(includedKinds, KindStageCache) || inventoryIncludes(includedKinds, KindOrphanSnapshot) || inventoryIncludes(includedKinds, KindZFSImportDataset) {
		cacheStore := opts.CacheStore
		if cacheStore == nil {
			store, err := cachestore.New(cachestore.Options{})
			if err != nil {
				return Report{}, err
			}
			cacheStore = store
		}
		cacheRecords, err := cacheStore.List(ctx)
		if err != nil {
			return Report{}, fmt.Errorf("list cache metadata: %w", err)
		}
		for _, record := range cacheRecords {
			entry := entryFromCacheRecord(record)
			if entry.Path != "" {
				if size, err := maybePathSize(ctx, entry.Path, opts.SkipSize); err == nil {
					entry.SizeBytes = size
				} else {
					return Report{}, fmt.Errorf("size cache %q/%q: %w", record.Stage, record.CacheKey, err)
				}
				if protections := referencedSnapshotPaths[cleanPath(entry.Path)]; len(protections) > 0 {
					entry.ProtectedBy = append(entry.ProtectedBy, protections...)
					entry.Reason = "stage-cache metadata; also referenced by explicit snapshot metadata"
				}
				addPathReference(referencedSnapshotPaths, entry.Path, "stage-cache metadata "+record.Stage+"/"+record.CacheKey)
			}
			addZFSImportDatasetReference(referencedZFSImportDatasets, record.StorageRef, "stage-cache metadata "+record.Stage+"/"+record.CacheKey)
			if inventoryIncludes(includedKinds, KindStageCache) {
				report.Entries = append(report.Entries, entry)
			}
		}
	}

	if inventoryIncludes(includedKinds, KindSandboxRuntime) {
		activeSandboxes := stringSet(opts.ActiveSandboxIDs)
		sandboxEntries, err := scanSandboxRuntimeDirs(ctx, filepath.Join(stateBase, "sandboxes"), activeSandboxes, stringSet(opts.SandboxRuntimeIDs), opts.SandboxStateKnown, opts.SkipSize)
		if err != nil {
			return Report{}, err
		}
		report.Entries = append(report.Entries, sandboxEntries...)
	}

	if inventoryIncludes(includedKinds, KindOrphanSnapshot) {
		orphanSnapshots, err := scanOrphanSnapshotDirs(ctx, snapshotRoots, referencedSnapshotPaths, opts.SkipSize)
		if err != nil {
			return Report{}, err
		}
		report.Entries = append(report.Entries, orphanSnapshots...)
	}

	if inventoryIncludes(includedKinds, KindZFSImportDataset) && opts.ZFSImportDatasetStore != nil {
		importDatasets, err := scanZFSImportDatasets(ctx, opts.ZFSImportDatasetStore, referencedZFSImportDatasets)
		if err != nil {
			return Report{}, err
		}
		report.Entries = append(report.Entries, importDatasets...)
	}

	if inventoryIncludes(includedKinds, KindRuntimeRootFS) {
		runtimeRootFS, err := scanRuntimeRootFS(cacheBase, opts.SkipSize)
		if err != nil {
			return Report{}, err
		}
		report.Entries = append(report.Entries, runtimeRootFS...)
	}

	if inventoryIncludes(includedKinds, KindImageCache) {
		imageManager := opts.ImageManager
		if imageManager == nil {
			manager, err := imagemgr.New(imagemgr.Options{})
			if err != nil {
				return Report{}, err
			}
			imageManager = manager
		}
		imageEntries, err := scanImageEntries(ctx, imageManager, opts.SkipSize)
		if err != nil {
			return Report{}, err
		}
		report.Entries = append(report.Entries, imageEntries...)
	}

	if inventoryIncludes(includedKinds, KindExecutionArtifact) {
		executionEntries, err := scanExecutionArtifacts(ctx, filepath.Join(stateBase, "executions"), now, executionMaxAge, opts.SkipSize)
		if err != nil {
			return Report{}, err
		}
		report.Entries = append(report.Entries, executionEntries...)
	}

	for _, root := range []struct {
		kind string
		path string
	}{
		{kind: KindRepositoryCache, path: filepath.Join(stateBase, "repos")},
		{kind: KindContentCache, path: filepath.Join(cacheBase, "content-cache")},
		{kind: KindChangesetStore, path: filepath.Join(stateBase, "changesets")},
	} {
		if !inventoryIncludes(includedKinds, root.kind) {
			continue
		}
		entry, ok, err := protectedRootEntry(ctx, root.kind, root.path, opts.SkipSize)
		if err != nil {
			return Report{}, err
		}
		if ok {
			report.Entries = append(report.Entries, entry)
		}
	}

	sortEntries(report.Entries)
	report.Totals = totalsByKind(report.Entries)
	return report, nil
}

func PlanPrune(report Report, opts PruneOptions) Plan {
	now := opts.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	plan := Plan{}
	for _, entry := range report.Entries {
		reclaimable := entry.Reclaimable
		reason := entry.Reason
		if !reclaimable && opts.OlderThan > 0 && olderThan(entry, now, opts.OlderThan) {
			switch entry.Kind {
			case KindRuntimeRootFS:
				reclaimable = true
				reason = fmt.Sprintf("older than %s", opts.OlderThan)
			case KindStageCache:
				if entry.Path != "" && stageCacheStorageExclusive(entry) && stageCacheStorageManaged(entry, report.SnapshotRoots) {
					reclaimable = true
					reason = fmt.Sprintf("stage cache older than %s", opts.OlderThan)
				}
			}
		}
		if !reclaimable && opts.All {
			switch entry.Kind {
			case KindRuntimeRootFS:
				reclaimable = true
				reason = "system cache selected by --all"
			case KindStageCache:
				if entry.Path != "" && stageCacheStorageExclusive(entry) && stageCacheStorageManaged(entry, report.SnapshotRoots) {
					reclaimable = true
					reason = "stage cache selected by --all"
				}
			case KindImageCache:
				reclaimable = true
				reason = "image cache selected by --all"
			}
		}
		if !reclaimable {
			continue
		}
		actionPath, ok := deletionPathForReport(report, entry)
		if !ok {
			continue
		}
		action := Action{
			Kind:      entry.Kind,
			ID:        entry.ID,
			Path:      actionPath,
			SizeBytes: entry.SizeBytes,
			Reason:    reason,
			Entry:     entry,
		}
		plan.Actions = append(plan.Actions, action)
		plan.ReclaimableBytes += action.SizeBytes
	}
	sort.Slice(plan.Actions, func(i, j int) bool {
		if plan.Actions[i].Kind == plan.Actions[j].Kind {
			return plan.Actions[i].ID < plan.Actions[j].ID
		}
		return plan.Actions[i].Kind < plan.Actions[j].Kind
	})
	return plan
}

// PlanLifecyclePrune plans targeted cleanup for resources whose owner just
// completed a lifecycle transition. It is intentionally narrower than
// PlanPrune and does not apply age or --all policy.
func PlanLifecyclePrune(report Report, opts LifecycleOptions) Plan {
	terminatedSandboxes := stringSet(opts.TerminatedSandboxIDs)
	if len(terminatedSandboxes) == 0 && !opts.ZFSImportDatasets {
		return Plan{}
	}

	plan := Plan{}
	for _, entry := range report.Entries {
		switch entry.Kind {
		case KindSandboxRuntime:
			if !report.SandboxStateKnown {
				continue
			}
			if _, ok := terminatedSandboxes[entry.ID]; !ok {
				continue
			}
			actionPath, ok := deletionPathForReport(report, entry)
			if !ok || actionPath == "" {
				continue
			}
			action := Action{
				Kind:      entry.Kind,
				ID:        entry.ID,
				Path:      actionPath,
				SizeBytes: entry.SizeBytes,
				Reason:    "terminated sandbox lifecycle cleanup",
				Entry:     entry,
			}
			plan.Actions = append(plan.Actions, action)
			plan.ReclaimableBytes += action.SizeBytes
		case KindZFSImportDataset:
			if !opts.ZFSImportDatasets || !entry.Reclaimable {
				continue
			}
			action := Action{
				Kind:      entry.Kind,
				ID:        entry.ID,
				SizeBytes: entry.SizeBytes,
				Reason:    "stale zfs import dataset lifecycle cleanup",
				Entry:     entry,
			}
			plan.Actions = append(plan.Actions, action)
			plan.ReclaimableBytes += action.SizeBytes
		default:
			continue
		}
	}
	sort.Slice(plan.Actions, func(i, j int) bool {
		if plan.Actions[i].Kind == plan.Actions[j].Kind {
			return plan.Actions[i].ID < plan.Actions[j].ID
		}
		return plan.Actions[i].Kind < plan.Actions[j].Kind
	})
	return plan
}

func stageCacheStorageExclusive(entry Entry) bool {
	for _, protection := range entry.ProtectedBy {
		if strings.HasPrefix(protection, "snapshot metadata ") {
			return false
		}
	}
	return true
}

func ExecutePrune(ctx context.Context, report Report, plan Plan, opts ExecuteOptions) (Result, error) {
	if len(plan.Actions) == 0 {
		return Result{}, nil
	}
	roots := append([]string{report.StateBaseDir, report.CacheBaseDir}, report.SnapshotRoots...)
	roots = uniqueCleanPaths(roots)

	cacheStore := opts.CacheStore
	imageManager := opts.ImageManager
	zfsImportDatasetStore := opts.ZFSImportDatasetStore
	for _, action := range plan.Actions {
		switch action.Kind {
		case KindStageCache:
			if cacheStore == nil {
				store, err := cachestore.New(cachestore.Options{})
				if err != nil {
					return Result{}, err
				}
				cacheStore = store
			}
		case KindImageCache:
			if imageManager == nil {
				manager, err := imagemgr.New(imagemgr.Options{})
				if err != nil {
					return Result{}, err
				}
				imageManager = manager
			}
		case KindZFSImportDataset:
			if zfsImportDatasetStore == nil {
				return Result{}, errors.New("zfs import dataset store is not configured")
			}
		}
	}

	result := Result{}
	for _, action := range plan.Actions {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := executeAction(ctx, report, action, roots, cacheStore, imageManager, zfsImportDatasetStore); err != nil {
			return result, err
		}
		result.DeletedEntries++
		result.ReclaimedBytes += action.SizeBytes
	}
	return result, nil
}

func executeAction(ctx context.Context, report Report, action Action, roots []string, cacheStore CacheStore, imageManager ImageManager, zfsImportDatasetStore ZFSImportDatasetStore) error {
	switch action.Kind {
	case KindImageCache:
		if imageManager == nil {
			return errors.New("image manager is not configured")
		}
		if _, err := imageManager.Remove(ctx, action.Entry.ID); err != nil {
			return fmt.Errorf("remove image cache %q: %w", action.Entry.ID, err)
		}
		return nil
	case KindStageCache:
		stageCachePath, ok := managedSnapshotDirectory(action.Entry.Path, report.SnapshotRoots, action.Entry.Backend)
		if !ok {
			return fmt.Errorf("refusing to remove stage-cache storage_ref %q outside managed snapshot storage", action.Entry.StorageRef)
		}
		if cacheStore == nil {
			return errors.New("cache metadata store is not configured")
		}
		owner := strings.TrimSpace(action.Entry.OwnerPrincipalID)
		if ownerStore, ok := cacheStore.(ownerCacheStore); ok {
			if err := ownerStore.DeleteForOwner(ctx, action.Entry.Stage, action.Entry.CacheKey, owner); err != nil {
				return fmt.Errorf("delete stage-cache metadata %q/%q for owner %q: %w", action.Entry.Stage, action.Entry.CacheKey, owner, err)
			}
		} else if owner != "" {
			return fmt.Errorf("cache metadata store cannot delete owner-scoped stage-cache metadata %q/%q for owner %q", action.Entry.Stage, action.Entry.CacheKey, owner)
		} else if err := cacheStore.Delete(ctx, action.Entry.Stage, action.Entry.CacheKey); err != nil {
			return fmt.Errorf("delete stage-cache metadata %q/%q: %w", action.Entry.Stage, action.Entry.CacheKey, err)
		}
		return removeOwnedPath(stageCachePath, roots)
	case KindZFSImportDataset:
		if zfsImportDatasetStore == nil {
			return errors.New("zfs import dataset store is not configured")
		}
		if err := zfsImportDatasetStore.DestroyZFSImportDataset(ctx, action.Entry.StorageRef); err != nil {
			return fmt.Errorf("destroy zfs import dataset %q: %w", action.Entry.StorageRef, err)
		}
		return nil
	default:
		return removeOwnedPath(action.Path, roots)
	}
}

func entryFromSnapshotRecord(record snapshotstore.Record) Entry {
	path := storageRefPath(record.StorageRef)
	return Entry{
		Kind:        KindSnapshot,
		ID:          record.SnapshotID,
		Backend:     record.Backend,
		Path:        path,
		StorageRef:  record.StorageRef,
		Reclaimable: false,
		Reason:      "explicit snapshot metadata",
		ProtectedBy: []string{"snapshot metadata"},
		CreatedAt:   record.CreatedAt,
		LastUsedAt:  record.CreatedAt,
	}
}

func entryFromCacheRecord(record cachestore.Record) Entry {
	path := storageRefPath(record.StorageRef)
	protectedBy := []string{"stage-cache metadata"}
	reason := "stage-cache metadata"
	if path == "" {
		reason = "stage-cache metadata uses non-filesystem storage"
	}
	return Entry{
		Kind:             KindStageCache,
		ID:               record.Stage + "/" + record.CacheKey,
		Backend:          record.Backend,
		Stage:            record.Stage,
		CacheKey:         record.CacheKey,
		OwnerPrincipalID: record.OwnerPrincipalID,
		OwnerScope:       record.OwnerScope,
		Path:             path,
		StorageRef:       record.StorageRef,
		Reclaimable:      false,
		Reason:           reason,
		ProtectedBy:      protectedBy,
		CreatedAt:        record.CreatedAt,
		LastUsedAt:       record.LastUsedAt,
	}
}

func scanSandboxRuntimeDirs(ctx context.Context, baseDir string, activeIDs, selectedIDs map[string]struct{}, stateKnown, skipSize bool) ([]Entry, error) {
	dirEntries, err := os.ReadDir(baseDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sandbox runtime directory %q: %w", baseDir, err)
	}
	out := make([]Entry, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		if !dirEntry.IsDir() {
			continue
		}
		if len(selectedIDs) > 0 {
			if _, ok := selectedIDs[dirEntry.Name()]; !ok {
				continue
			}
		}
		path := filepath.Join(baseDir, dirEntry.Name())
		size, err := maybePathSize(ctx, path, skipSize)
		if err != nil {
			return nil, fmt.Errorf("size sandbox runtime %q: %w", path, err)
		}
		info, err := dirEntry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect sandbox runtime %q: %w", path, err)
		}
		entry := Entry{
			Kind:       KindSandboxRuntime,
			ID:         dirEntry.Name(),
			Path:       path,
			SizeBytes:  size,
			CreatedAt:  info.ModTime().UTC(),
			LastUsedAt: info.ModTime().UTC(),
		}
		if !stateKnown {
			entry.Reason = "daemon sandbox state unavailable"
			entry.ProtectedBy = []string{"unknown daemon state"}
		} else if _, ok := activeIDs[dirEntry.Name()]; ok {
			entry.Reason = "known daemon sandbox"
			entry.ProtectedBy = []string{"daemon state"}
		} else {
			entry.Reclaimable = true
			entry.Reason = "not present in daemon state"
		}
		out = append(out, entry)
	}
	return out, nil
}

func scanOrphanSnapshotDirs(ctx context.Context, snapshotRoots []string, refs map[string][]string, skipSize bool) ([]Entry, error) {
	var out []Entry
	seen := map[string]struct{}{}
	for _, root := range snapshotRoots {
		for _, backendName := range []string{"firecracker", "darwin-vz"} {
			namespaceDir := filepath.Join(root, backendName)
			dirEntries, err := os.ReadDir(namespaceDir)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("read snapshot namespace %q: %w", namespaceDir, err)
			}
			for _, dirEntry := range dirEntries {
				if !dirEntry.IsDir() {
					continue
				}
				dirPath := filepath.Join(namespaceDir, dirEntry.Name())
				if _, ok := seen[cleanPath(dirPath)]; ok {
					continue
				}
				seen[cleanPath(dirPath)] = struct{}{}
				rootFSPath := filepath.Join(dirPath, "rootfs.ext4")
				if _, ok := refs[cleanPath(rootFSPath)]; ok {
					continue
				}
				if _, ok := refs[cleanPath(dirPath)]; ok {
					continue
				}
				size, err := maybePathSize(ctx, dirPath, skipSize)
				if err != nil {
					return nil, fmt.Errorf("size orphan snapshot %q: %w", dirPath, err)
				}
				info, err := dirEntry.Info()
				if err != nil {
					return nil, fmt.Errorf("inspect orphan snapshot %q: %w", dirPath, err)
				}
				out = append(out, Entry{
					Kind:        KindOrphanSnapshot,
					ID:          dirEntry.Name(),
					Backend:     backendName,
					Path:        dirPath,
					StorageRef:  rootFSPath,
					SizeBytes:   size,
					Reclaimable: true,
					Reason:      "not referenced by snapshot or cache metadata",
					CreatedAt:   info.ModTime().UTC(),
					LastUsedAt:  info.ModTime().UTC(),
				})
			}
		}
	}
	return out, nil
}

func scanZFSImportDatasets(ctx context.Context, store ZFSImportDatasetStore, refs map[string][]string) ([]Entry, error) {
	datasets, err := store.ListZFSImportDatasets(ctx)
	if err != nil {
		return nil, fmt.Errorf("list zfs import datasets: %w", err)
	}
	out := make([]Entry, 0, len(datasets))
	seen := map[string]struct{}{}
	for _, dataset := range datasets {
		dataset = strings.TrimSpace(dataset)
		if dataset == "" {
			continue
		}
		if _, ok := zfsImportDatasetFromRef(dataset); !ok {
			continue
		}
		if _, ok := seen[dataset]; ok {
			continue
		}
		seen[dataset] = struct{}{}
		entry := Entry{
			Kind:        KindZFSImportDataset,
			ID:          zfsImportDatasetID(dataset),
			StorageRef:  dataset,
			Reclaimable: true,
			Reason:      "unreferenced zfs import dataset",
		}
		if protections := refs[dataset]; len(protections) > 0 {
			entry.Reclaimable = false
			entry.Reason = "zfs import dataset referenced by metadata"
			entry.ProtectedBy = append(entry.ProtectedBy, protections...)
		}
		out = append(out, entry)
	}
	return out, nil
}

func scanRuntimeRootFS(cacheBase string, skipSize bool) ([]Entry, error) {
	var out []Entry
	for _, backendName := range []string{"firecracker", "darwin-vz"} {
		dir := filepath.Join(cacheBase, backendName, "runtime-rootfs")
		dirEntries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read runtime rootfs cache %q: %w", dir, err)
		}
		for _, dirEntry := range dirEntries {
			if dirEntry.IsDir() {
				continue
			}
			path := filepath.Join(dir, dirEntry.Name())
			info, err := dirEntry.Info()
			if err != nil {
				return nil, fmt.Errorf("inspect runtime rootfs cache %q: %w", path, err)
			}
			size := int64(0)
			if !skipSize {
				size = allocatedFileSize(info)
			}
			out = append(out, Entry{
				Kind:        KindRuntimeRootFS,
				ID:          strings.TrimSuffix(dirEntry.Name(), filepath.Ext(dirEntry.Name())),
				Backend:     backendName,
				Path:        path,
				SizeBytes:   size,
				Reclaimable: false,
				Reason:      "runtime rootfs cache preserved unless selected",
				ProtectedBy: []string{"runtime rootfs cache"},
				CreatedAt:   info.ModTime().UTC(),
				LastUsedAt:  info.ModTime().UTC(),
			})
		}
	}
	return out, nil
}

func scanImageEntries(ctx context.Context, imageManager ImageManager, skipSize bool) ([]Entry, error) {
	records, err := imageManager.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list image cache metadata: %w", err)
	}
	out := make([]Entry, 0, len(records))
	for _, record := range records {
		var size int64
		if path := strings.TrimSpace(record.RootFSPath); path != "" && !skipSize {
			if actualSize, err := pathSize(ctx, path); err == nil && actualSize > 0 {
				size = actualSize
			} else if err == nil {
				size = 0
			} else if err != nil {
				return nil, fmt.Errorf("size image cache %q: %w", record.Digest, err)
			}
		}
		out = append(out, Entry{
			Kind:        KindImageCache,
			ID:          record.Digest,
			Path:        record.RootFSPath,
			SizeBytes:   size,
			Reclaimable: false,
			Reason:      "image cache metadata",
			ProtectedBy: []string{"image metadata"},
			CreatedAt:   record.CreatedAt,
			LastUsedAt:  record.LastUsedAt,
		})
	}
	return out, nil
}

func scanExecutionArtifacts(ctx context.Context, baseDir string, now time.Time, maxAge time.Duration, skipSize bool) ([]Entry, error) {
	dirEntries, err := os.ReadDir(baseDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read execution artifacts directory %q: %w", baseDir, err)
	}
	out := make([]Entry, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		if !dirEntry.IsDir() {
			continue
		}
		path := filepath.Join(baseDir, dirEntry.Name())
		size, err := maybePathSize(ctx, path, skipSize)
		if err != nil {
			return nil, fmt.Errorf("size execution artifacts %q: %w", path, err)
		}
		info, err := dirEntry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect execution artifacts %q: %w", path, err)
		}
		modTime := info.ModTime().UTC()
		entry := Entry{
			Kind:       KindExecutionArtifact,
			ID:         dirEntry.Name(),
			Path:       path,
			SizeBytes:  size,
			CreatedAt:  modTime,
			LastUsedAt: modTime,
		}
		if now.Sub(modTime) > maxAge {
			entry.Reclaimable = true
			entry.Reason = fmt.Sprintf("older than %s", maxAge)
		} else {
			entry.Reason = "recent execution artifacts"
			entry.ProtectedBy = []string{"execution retention window"}
		}
		out = append(out, entry)
	}
	return out, nil
}

func protectedRootEntry(ctx context.Context, kind, path string, skipSize bool) (Entry, bool, error) {
	size, err := maybePathSize(ctx, path, skipSize)
	if err != nil {
		return Entry{}, false, fmt.Errorf("size %s root %q: %w", kind, path, err)
	}
	if size == 0 {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return Entry{}, false, nil
		}
	}
	return Entry{
		Kind:        kind,
		Path:        path,
		SizeBytes:   size,
		Reclaimable: false,
		Reason:      "preserved by default",
		ProtectedBy: []string{"explicit prune not implemented"},
	}, true, nil
}

func snapshotRoots(cfg runtimeconfig.Config) ([]string, error) {
	defaultRoot, err := paths.SnapshotDir()
	if err != nil {
		return nil, fmt.Errorf("resolve default snapshot dir: %w", err)
	}
	roots := []string{defaultRoot}
	for _, snapshotCfg := range []runtimeconfig.SnapshotConfig{
		cfg.Backends.Firecracker.Snapshots,
		cfg.Backends.DarwinVZ.Snapshots,
	} {
		if baseDir := strings.TrimSpace(snapshotCfg.BaseDir); baseDir != "" {
			roots = append(roots, baseDir)
		}
	}
	return uniqueCleanPaths(roots), nil
}

func storageRefPath(storageRef string) string {
	ref := strings.TrimSpace(storageRef)
	if ref == "" || !filepath.IsAbs(ref) {
		return ""
	}
	return cleanPath(ref)
}

func addZFSImportDatasetReference(refs map[string][]string, storageRef, reason string) {
	dataset, ok := zfsImportDatasetFromRef(storageRef)
	if !ok {
		return
	}
	refs[dataset] = append(refs[dataset], reason)
}

func zfsImportDatasetFromRef(ref string) (string, bool) {
	dataset := strings.TrimSpace(ref)
	if dataset == "" || filepath.IsAbs(dataset) {
		return "", false
	}
	if before, _, ok := strings.Cut(dataset, "@"); ok {
		dataset = before
	}
	components := strings.Split(dataset, "/")
	if len(components) < 4 {
		return "", false
	}
	if components[len(components)-3] != "snapshots" || components[len(components)-2] != "imports" || strings.TrimSpace(components[len(components)-1]) == "" {
		return "", false
	}
	for _, component := range components {
		if strings.TrimSpace(component) == "" {
			return "", false
		}
	}
	return dataset, true
}

func zfsImportDatasetID(dataset string) string {
	_, dataset = filepath.Split(strings.TrimSpace(dataset))
	return dataset
}

func deletionPathForReport(report Report, entry Entry) (string, bool) {
	switch entry.Kind {
	case KindStageCache:
		return managedSnapshotDirectory(entry.Path, report.SnapshotRoots, entry.Backend)
	default:
		return deletionPath(entry), true
	}
}

func deletionPath(entry Entry) string {
	switch entry.Kind {
	case KindOrphanSnapshot:
		return entry.Path
	case KindStageCache:
		if entry.Path == "" {
			return ""
		}
		return filepath.Dir(entry.Path)
	default:
		return entry.Path
	}
}

func stageCacheStorageManaged(entry Entry, snapshotRoots []string) bool {
	_, ok := managedSnapshotDirectory(entry.Path, snapshotRoots, entry.Backend)
	return ok
}

func managedSnapshotDirectory(path string, snapshotRoots []string, backendName string) (string, bool) {
	path = cleanPath(path)
	if path == "" || filepath.Base(path) != "rootfs.ext4" {
		return "", false
	}
	backendName = strings.TrimSpace(backendName)
	if backendName != "firecracker" && backendName != "darwin-vz" {
		return "", false
	}
	for _, root := range snapshotRoots {
		root = cleanPath(root)
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 3 {
			continue
		}
		if parts[0] != backendName || strings.TrimSpace(parts[1]) == "" || parts[2] != "rootfs.ext4" {
			continue
		}
		return filepath.Dir(path), true
	}
	return "", false
}

func removeOwnedPath(path string, roots []string) error {
	path = cleanPath(path)
	if path == "" {
		return errors.New("missing prune path")
	}
	if !pathWithinAnyRoot(path, roots) {
		return fmt.Errorf("refusing to remove %q outside cleanroom-owned roots", path)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove %q: %w", path, err)
	}
	return nil
}

func inventoryIncludes(includedKinds map[string]struct{}, kind string) bool {
	if len(includedKinds) == 0 {
		return true
	}
	_, ok := includedKinds[kind]
	return ok
}

func pathWithinAnyRoot(path string, roots []string) bool {
	path = cleanPath(path)
	for _, root := range roots {
		root = cleanPath(root)
		if root == "" || path == root {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		if rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
			return true
		}
	}
	return false
}

func maybePathSize(ctx context.Context, path string, skip bool) (int64, error) {
	if skip {
		return 0, nil
	}
	return pathSize(ctx, path)
}

func pathSize(ctx context.Context, path string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return 0, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if !info.IsDir() {
		return allocatedFileSize(info), nil
	}

	var total int64
	err = filepath.WalkDir(path, func(path string, entry os.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += allocatedFileSize(info)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

func addPathReference(refs map[string][]string, path, reason string) {
	path = cleanPath(path)
	if path == "" {
		return
	}
	refs[path] = append(refs[path], reason)
}

func olderThan(entry Entry, now time.Time, age time.Duration) bool {
	if age <= 0 {
		return false
	}
	t := entry.LastUsedAt
	if t.IsZero() {
		t = entry.CreatedAt
	}
	if t.IsZero() {
		return false
	}
	return now.Sub(t) > age
}

func totalsByKind(entries []Entry) map[string]CategoryTotal {
	totals := map[string]CategoryTotal{}
	for _, entry := range entries {
		total := totals[entry.Kind]
		total.Count++
		total.TotalBytes += entry.SizeBytes
		if entry.Reclaimable {
			total.ReclaimableBytes += entry.SizeBytes
		} else {
			total.ProtectedBytes += entry.SizeBytes
		}
		totals[entry.Kind] = total
	}
	return totals
}

func sortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Kind == entries[j].Kind {
			if entries[i].ID == entries[j].ID {
				return entries[i].Path < entries[j].Path
			}
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Kind < entries[j].Kind
	})
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out[value] = struct{}{}
		}
	}
	return out
}

func uniqueCleanPaths(paths []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = cleanPath(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func cleanPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}
