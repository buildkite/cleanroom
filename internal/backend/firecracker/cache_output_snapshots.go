package firecracker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/backend/cacheoutput"
	"github.com/buildkite/cleanroom/internal/observability"
)

type cacheOutputSnapshotTarget struct {
	volume     preparedCacheOutputVolume
	cfg        backend.FirecrackerConfig
	snapshotID string
}

type createdCacheOutputSnapshot struct {
	cfg        backend.FirecrackerConfig
	storageRef string
}

func (a *Adapter) SnapshotCacheOutputVolumes(ctx context.Context, req backend.SnapshotCacheOutputVolumesRequest) (_ *backend.SnapshotCacheOutputVolumesResult, retErr error) {
	sandboxID := strings.TrimSpace(req.SandboxID)
	if sandboxID == "" {
		return nil, errors.New("missing sandbox_id")
	}
	snapshotIDPrefix := strings.TrimSpace(req.SnapshotIDPrefix)
	if snapshotIDPrefix == "" {
		return nil, errors.New("missing snapshot_id_prefix")
	}

	a.sandboxMu.Lock()
	instance, ok := a.sandboxes[sandboxID]
	a.sandboxMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown sandbox %q", sandboxID)
	}
	if err := instance.exitedErrOrNil(); err != nil {
		return nil, fmt.Errorf("sandbox %q is not running: %w", sandboxID, err)
	}

	volumes, err := selectCacheOutputVolumes(instance.cacheOutputVolumes, req.VolumeIDs)
	if err != nil {
		return nil, err
	}
	if len(volumes) == 0 {
		return &backend.SnapshotCacheOutputVolumesResult{}, nil
	}

	targets := make([]cacheOutputSnapshotTarget, 0, len(volumes))
	needsHostSync := false
	for _, volume := range volumes {
		cfg, err := cacheOutputSnapshotConfig(req.FirecrackerConfig, volume.Spec, volume.Volume.Ref)
		if err != nil {
			return nil, err
		}
		if err := validateSnapshotStorageConfig(cfg); err != nil {
			return nil, err
		}
		if strings.EqualFold(strings.TrimSpace(cfg.Snapshots.Driver), "zfs") {
			needsHostSync = true
		}
		targets = append(targets, cacheOutputSnapshotTarget{
			volume:     volume,
			cfg:        cfg,
			snapshotID: cacheOutputSnapshotID(snapshotIDPrefix, volume.Spec.VolumeID),
		})
	}

	syncResp, _, err := a.executeInSandbox(ctx, instance, snapshotSyncTimeoutSeconds, []string{"sync"}, "", nil, false, nil, nil, nil, false, backend.OutputStream{})
	if err != nil {
		return nil, fmt.Errorf("sync sandbox filesystem before cache output snapshot: %w", err)
	}
	if syncResp.ExitCode != 0 {
		guestErr := strings.TrimSpace(syncResp.Error)
		if guestErr != "" {
			return nil, fmt.Errorf("sync sandbox filesystem before cache output snapshot: guest sync command exited with code %d: %s", syncResp.ExitCode, guestErr)
		}
		return nil, fmt.Errorf("sync sandbox filesystem before cache output snapshot: guest sync command exited with code %d", syncResp.ExitCode)
	}

	logger := observability.WithLoggerFields(observability.WithTraceContext(baseFirecrackerLogger(a.Logger), ctx), observability.LogFieldSandboxID, sandboxID)
	result := &backend.SnapshotCacheOutputVolumesResult{Volumes: make([]backend.CacheOutputVolumeSnapshot, 0, len(targets))}
	createdSnapshots := make([]createdCacheOutputSnapshot, 0, len(targets))
	cleanupCreated := func() {
		for i := len(createdSnapshots) - 1; i >= 0; i-- {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = destroySnapshotStorage(cleanupCtx, logger, createdSnapshots[i].cfg, createdSnapshots[i].storageRef)
			cancel()
		}
	}
	defer func() {
		if retErr != nil {
			cleanupCreated()
		}
	}()

	if err := pauseSandboxProcess(instance); err != nil {
		return nil, err
	}
	defer func() {
		if err := resumeSandboxProcess(instance); err != nil && retErr == nil {
			result = nil
			retErr = fmt.Errorf("resume firecracker sandbox after cache output snapshot: %w", err)
		}
	}()

	if needsHostSync {
		if err := flushSnapshotHostFilesystem(ctx, "zfs"); err != nil {
			return nil, err
		}
	}

	for _, target := range targets {
		volume := target.volume
		snapshot, err := createSnapshotStorage(ctx, logger, target.cfg, target.snapshotID, volume.Volume.Ref)
		if err != nil {
			return nil, fmt.Errorf("snapshot cache output volume %q: %w", volume.Spec.VolumeID, err)
		}
		if snapshot == nil {
			return nil, fmt.Errorf("snapshot cache output volume %q returned no result", volume.Spec.VolumeID)
		}
		storageRef := strings.TrimSpace(snapshot.StorageRef)
		if storageRef == "" {
			return nil, fmt.Errorf("snapshot cache output volume %q returned empty storage ref", volume.Spec.VolumeID)
		}
		createdSnapshots = append(createdSnapshots, createdCacheOutputSnapshot{
			cfg:        target.cfg,
			storageRef: storageRef,
		})
		result.Volumes = append(result.Volumes, backend.CacheOutputVolumeSnapshot{
			Stage:              strings.TrimSpace(volume.Spec.Stage),
			BlockName:          strings.TrimSpace(volume.Spec.BlockName),
			CacheKey:           strings.TrimSpace(volume.Spec.CacheKey),
			VolumeID:           strings.TrimSpace(volume.Spec.VolumeID),
			SnapshotID:         strings.TrimSpace(target.snapshotID),
			StorageDriver:      effectiveSnapshotDriver(target.cfg),
			StorageRef:         storageRef,
			SnapshotRef:        storageRef,
			StorageSizeBytes:   snapshot.StorageSizeBytes,
			ExclusiveSizeBytes: snapshot.ExclusiveSizeBytes,
			DriverMetadata:     strings.TrimSpace(snapshot.DriverMetadata),
			Outputs:            cacheOutputVolumeSnapshotOutputs(volume.Spec),
		})
	}
	return result, nil
}

func selectCacheOutputVolumes(volumes []preparedCacheOutputVolume, volumeIDs []string) ([]preparedCacheOutputVolume, error) {
	return cacheoutput.SelectByVolumeID(volumes, volumeIDs, func(volume preparedCacheOutputVolume) string {
		return volume.Spec.VolumeID
	})
}

func cacheOutputSnapshotID(prefix, volumeID string) string {
	return cacheoutput.SnapshotID(prefix, volumeID)
}

func cacheOutputSnapshotConfig(cfg backend.FirecrackerConfig, spec backend.CacheOutputVolumeSpec, volumeRef string) (backend.FirecrackerConfig, error) {
	if driver := strings.TrimSpace(spec.StorageDriver); driver != "" {
		cfg.Snapshots.Driver = driver
	}
	return snapshotConfigForStorageRef(cfg, volumeRef)
}

func cacheOutputVolumeSnapshotOutputs(spec backend.CacheOutputVolumeSpec) []backend.CacheOutputVolumeSnapshotOutput {
	return cacheoutput.SnapshotOutputs(spec)
}

func effectiveSnapshotDriver(cfg backend.FirecrackerConfig) string {
	driver := strings.ToLower(strings.TrimSpace(cfg.Snapshots.Driver))
	if driver == "" {
		return "file"
	}
	return driver
}
