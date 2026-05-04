//go:build darwin

package darwinvz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/volumestore"
)

type darwinVZCacheOutputSnapshotTarget struct {
	volume     preparedDarwinVZCacheOutputVolume
	cfg        backend.FirecrackerConfig
	snapshotID string
}

type createdDarwinVZCacheOutputSnapshot struct {
	driver     volumestore.Driver
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

	volumes, err := selectDarwinVZCacheOutputVolumes(instance.cacheOutputVolumes, req.VolumeIDs)
	if err != nil {
		return nil, err
	}
	if len(volumes) == 0 {
		return &backend.SnapshotCacheOutputVolumesResult{}, nil
	}

	targets := make([]darwinVZCacheOutputSnapshotTarget, 0, len(volumes))
	for _, volume := range volumes {
		cfg := darwinVZCacheOutputSnapshotConfig(req.FirecrackerConfig, volume.Spec)
		if _, err := snapshotVolumeDriver(cfg); err != nil {
			return nil, err
		}
		targets = append(targets, darwinVZCacheOutputSnapshotTarget{
			volume:     volume,
			cfg:        cfg,
			snapshotID: darwinVZCacheOutputSnapshotID(snapshotIDPrefix, volume.Spec.VolumeID),
		})
	}

	executeInSandbox := a.executeInSandboxFn
	if executeInSandbox == nil {
		executeInSandbox = a.executeInSandbox
	}

	connectSeconds := req.FirecrackerConfig.LaunchSeconds
	if connectSeconds <= 0 {
		connectSeconds = instance.CommandTimeout
	}
	if connectSeconds <= 0 {
		connectSeconds = 30
	}
	syncCtx, cancel := context.WithTimeout(ctx, time.Duration(connectSeconds)*time.Second)
	defer cancel()
	syncResult, err := executeInSandbox(syncCtx, ctx, instance, backend.ExecutionRequest{
		SandboxID: sandboxID,
		Command:   []string{"sync"},
		Policy:    instance.Policy,
	}, backend.OutputStream{})
	if err != nil {
		return nil, fmt.Errorf("sync sandbox filesystem before cache output snapshot: %w", err)
	}
	if syncResult != nil && syncResult.ExitCode != 0 {
		guestErr := strings.TrimSpace(syncResult.Message)
		if guestErr != "" {
			return nil, fmt.Errorf("sync sandbox filesystem before cache output snapshot: guest sync command exited with code %d: %s", syncResult.ExitCode, guestErr)
		}
		return nil, fmt.Errorf("sync sandbox filesystem before cache output snapshot: guest sync command exited with code %d", syncResult.ExitCode)
	}

	helperRequest := a.helperRequestFn
	if helperRequest == nil {
		helperRequest = func(ctx context.Context, helper *helperSession, req helperControlRequest) (helperControlResponse, error) {
			return helper.request(ctx, req)
		}
	}
	if instance.Helper == nil {
		return nil, errors.New("darwin-vz sandbox helper is not available")
	}
	if strings.TrimSpace(instance.VMID) == "" {
		return nil, errors.New("darwin-vz sandbox vm id is empty")
	}

	result := &backend.SnapshotCacheOutputVolumesResult{Volumes: make([]backend.CacheOutputVolumeSnapshot, 0, len(targets))}
	createdSnapshots := make([]createdDarwinVZCacheOutputSnapshot, 0, len(targets))
	cleanupCreated := func() {
		for i := len(createdSnapshots) - 1; i >= 0; i-- {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = createdSnapshots[i].driver.DestroySnapshot(cleanupCtx, volumestore.DestroySnapshotRequest{SnapshotRef: createdSnapshots[i].storageRef})
			cancel()
		}
	}
	defer func() {
		if retErr != nil {
			cleanupCreated()
		}
	}()

	if _, err := helperRequest(ctx, instance.Helper, helperControlRequest{Op: "PauseVM", VMID: instance.VMID}); err != nil {
		return nil, fmt.Errorf("pause darwin-vz sandbox: %w", err)
	}
	defer func() {
		resumeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := helperRequest(resumeCtx, instance.Helper, helperControlRequest{Op: "ResumeVM", VMID: instance.VMID}); err != nil && retErr == nil {
			result = nil
			retErr = fmt.Errorf("resume darwin-vz sandbox after cache output snapshot: %w", err)
		}
	}()

	for _, target := range targets {
		driver, err := snapshotVolumeDriver(target.cfg)
		if err != nil {
			return nil, err
		}
		volume := target.volume
		snapshot, snapshotDriver, storageDriver, err := snapshotDarwinVZCacheOutputVolume(ctx, driver, volumestore.SnapshotVolumeRequest{
			SnapshotID: target.snapshotID,
			VolumeRef:  volume.Volume.Ref,
		})
		if err != nil {
			return nil, fmt.Errorf("snapshot cache output volume %q: %w", volume.Spec.VolumeID, err)
		}
		storageRef := strings.TrimSpace(snapshot.StorageRef)
		if storageRef == "" {
			return nil, fmt.Errorf("snapshot cache output volume %q returned empty storage ref", volume.Spec.VolumeID)
		}
		snapshotRef := strings.TrimSpace(snapshot.Ref)
		if snapshotRef == "" {
			snapshotRef = storageRef
		}
		createdSnapshots = append(createdSnapshots, createdDarwinVZCacheOutputSnapshot{
			driver:     snapshotDriver,
			storageRef: storageRef,
		})
		result.Volumes = append(result.Volumes, backend.CacheOutputVolumeSnapshot{
			Stage:              strings.TrimSpace(volume.Spec.Stage),
			BlockName:          strings.TrimSpace(volume.Spec.BlockName),
			CacheKey:           strings.TrimSpace(volume.Spec.CacheKey),
			VolumeID:           strings.TrimSpace(volume.Spec.VolumeID),
			StorageDriver:      storageDriver,
			StorageRef:         storageRef,
			SnapshotRef:        snapshotRef,
			StorageSizeBytes:   snapshot.StorageSizeBytes,
			ExclusiveSizeBytes: snapshot.ExclusiveSizeBytes,
			DriverMetadata:     strings.TrimSpace(snapshot.DriverMetadata),
			Outputs:            darwinVZCacheOutputVolumeSnapshotOutputs(volume.Spec),
		})
	}

	return result, nil
}

func snapshotDarwinVZCacheOutputVolume(ctx context.Context, driver volumestore.Driver, req volumestore.SnapshotVolumeRequest) (volumestore.Snapshot, volumestore.Driver, string, error) {
	if fallback, ok := driver.(*fallbackVolumeDriver); ok {
		snapshot, err := fallback.primary.SnapshotVolume(ctx, req)
		if err == nil || !fallback.shouldFallback(err) {
			return snapshot, fallback.primary, fallback.primary.Name(), err
		}
		snapshot, err = fallback.fallback.SnapshotVolume(ctx, req)
		return snapshot, fallback.fallback, fallback.fallback.Name(), err
	}
	snapshot, err := driver.SnapshotVolume(ctx, req)
	return snapshot, driver, driver.Name(), err
}

func selectDarwinVZCacheOutputVolumes(volumes []preparedDarwinVZCacheOutputVolume, volumeIDs []string) ([]preparedDarwinVZCacheOutputVolume, error) {
	if len(volumeIDs) == 0 {
		return append([]preparedDarwinVZCacheOutputVolume(nil), volumes...), nil
	}
	byID := make(map[string]preparedDarwinVZCacheOutputVolume, len(volumes))
	for _, volume := range volumes {
		byID[strings.TrimSpace(volume.Spec.VolumeID)] = volume
	}
	selected := make([]preparedDarwinVZCacheOutputVolume, 0, len(volumeIDs))
	seen := make(map[string]struct{}, len(volumeIDs))
	for _, id := range volumeIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, errors.New("cache output volume id cannot be empty")
		}
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("duplicate cache output volume id %q", id)
		}
		seen[id] = struct{}{}
		volume, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("unknown cache output volume id %q", id)
		}
		selected = append(selected, volume)
	}
	return selected, nil
}

func darwinVZCacheOutputSnapshotConfig(cfg backend.FirecrackerConfig, spec backend.CacheOutputVolumeSpec) backend.FirecrackerConfig {
	if driver := strings.TrimSpace(spec.StorageDriver); driver != "" {
		cfg.Snapshots.Driver = driver
	}
	return cfg
}

func darwinVZCacheOutputSnapshotID(prefix, volumeID string) string {
	hash := darwinVZCacheOutputSnapshotHash(strings.TrimSpace(volumeID))
	return strings.TrimSpace(prefix) + "-" + hash[:16]
}

func darwinVZCacheOutputVolumeSnapshotOutputs(spec backend.CacheOutputVolumeSpec) []backend.CacheOutputVolumeSnapshotOutput {
	out := make([]backend.CacheOutputVolumeSnapshotOutput, 0, len(spec.DirMappings)+len(spec.FileMappings))
	for _, mapping := range spec.DirMappings {
		out = append(out, backend.CacheOutputVolumeSnapshotOutput{
			Kind:          "dir",
			GuestPath:     strings.TrimSpace(mapping.GuestPath),
			VolumeSubpath: strings.TrimSpace(mapping.Subpath),
		})
	}
	for _, mapping := range spec.FileMappings {
		out = append(out, backend.CacheOutputVolumeSnapshotOutput{
			Kind:          "file",
			GuestPath:     strings.TrimSpace(mapping.GuestPath),
			VolumeSubpath: strings.TrimSpace(mapping.Subpath),
			Mode:          mapping.Mode.Perm(),
		})
	}
	return out
}

func darwinVZCacheOutputSnapshotHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
