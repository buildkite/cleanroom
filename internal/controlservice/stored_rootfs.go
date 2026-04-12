package controlservice

import (
	"context"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
)

type storedRootFSRecord struct {
	ID            string
	Kind          string
	SnapshotID    string
	Backend       string
	Policy        *policy.CompiledPolicy
	Repository    *repositorycheckout.Checkout
	StorageDriver string
	StorageRef    string
}

func storedRootFSRecordFromSnapshot(record snapshotstore.Record) (storedRootFSRecord, error) {
	compiled, err := policy.FromProto(record.Policy)
	if err != nil {
		return storedRootFSRecord{}, fmt.Errorf("invalid snapshot policy %q: %w", record.SnapshotID, err)
	}
	return storedRootFSRecord{
		ID:            strings.TrimSpace(record.SnapshotID),
		Kind:          "snapshot",
		SnapshotID:    strings.TrimSpace(record.SnapshotID),
		Backend:       strings.TrimSpace(record.Backend),
		Policy:        compiled,
		Repository:    repositorycheckout.FromProto(record.Repository),
		StorageDriver: strings.TrimSpace(record.StorageDriver),
		StorageRef:    strings.TrimSpace(record.StorageRef),
	}, nil
}

func storedRootFSRecordFromCacheEntry(record cachestore.Record) (storedRootFSRecord, error) {
	compiled, err := policy.FromProto(record.Policy)
	if err != nil {
		return storedRootFSRecord{}, fmt.Errorf("invalid cache policy %q/%q: %w", record.Stage, record.CacheKey, err)
	}
	sourceKind := strings.TrimSpace(record.Stage)
	if sourceKind == "" {
		sourceKind = "stage cache"
	} else {
		sourceKind += " stage cache"
	}
	return storedRootFSRecord{
		ID:            strings.TrimSpace(record.CacheKey),
		Kind:          sourceKind,
		SnapshotID:    strings.TrimSpace(record.BackingSnapshotID),
		Backend:       strings.TrimSpace(record.Backend),
		Policy:        compiled,
		Repository:    repositorycheckout.FromProto(record.Repository),
		StorageDriver: strings.TrimSpace(record.StorageDriver),
		StorageRef:    strings.TrimSpace(record.StorageRef),
	}, nil
}

func (s *Service) createSandboxFromStoredRootFS(ctx context.Context, req *cleanroomv1.CreateSandboxRequest, record storedRootFSRecord, overridePolicy *policy.CompiledPolicy) (*cleanroomv1.CreateSandboxResponse, error) {
	backendName := record.Backend
	if backendName == "" {
		return nil, fmt.Errorf("stored rootfs record %q missing backend", record.ID)
	}
	if requested := strings.TrimSpace(req.GetBackend()); requested != "" && requested != backendName {
		return nil, fmt.Errorf("%s %q requires backend %q, got %q", record.Kind, record.ID, backendName, requested)
	}
	effectivePolicy := record.Policy
	if overridePolicy != nil {
		effectivePolicy = overridePolicy
	}
	if effectivePolicy == nil {
		return nil, fmt.Errorf("stored rootfs record %q missing compiled policy", record.ID)
	}

	adapter, ok := s.Backends[backendName]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q", backendName)
	}
	snapshotAdapter, ok := adapter.(backend.SnapshottingAdapter)
	if !ok {
		return nil, fmt.Errorf("backend %q does not support snapshot-backed sandbox creation", backendName)
	}
	if !snapshotOperationsEnabledForBackend(backendName, s.Config) {
		return nil, fmt.Errorf("snapshots are not enabled for backend %q", backendName)
	}

	opts := req.GetOptions()
	execOpts := executionOptions{}
	if opts != nil {
		execOpts.LaunchSeconds = opts.GetLaunchSeconds()
	}
	firecrackerCfg := runtimeconfig.MergeBackendConfig(s.Config, backendName, execOpts.LaunchSeconds)
	firecrackerCfg.RunDir = ""
	firecrackerCfg = withSnapshotDriver(backendName, firecrackerCfg, record.StorageDriver)

	now := s.clock().Now()
	sandboxID := s.ids().NewSandboxID()
	provisionSnapshotID := strings.TrimSpace(record.SnapshotID)
	if provisionSnapshotID == "" {
		provisionSnapshotID = strings.TrimSpace(record.ID)
	}
	if err := snapshotAdapter.ProvisionSandboxFromSnapshot(ctx, backend.ProvisionFromSnapshotRequest{
		SandboxID:         sandboxID,
		SnapshotID:        provisionSnapshotID,
		StorageRef:        record.StorageRef,
		Policy:            effectivePolicy,
		FirecrackerConfig: firecrackerCfg,
	}); err != nil {
		return nil, fmt.Errorf("provision sandbox from snapshot: %w", err)
	}

	state := &sandboxState{
		ID:          sandboxID,
		Backend:     backendName,
		Policy:      effectivePolicy,
		Firecracker: firecrackerCfg,
		Repository:  cloneRepositoryCheckout(record.Repository),
		CreatedAt:   now,
		UpdatedAt:   now,
		Status:      cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY,
		events:      newEventFeed[*cleanroomv1.SandboxEvent](s.retention().maxRetainedSandboxEvents),
		Done:        make(chan struct{}),
	}

	sourceKind := strings.TrimSpace(record.Kind)
	if sourceKind == "" {
		sourceKind = "stored rootfs"
	}
	eventMessage := fmt.Sprintf("sandbox created from %s %s and ready", sourceKind, record.ID)
	responseMessage := fmt.Sprintf("sandbox created from %s and ready", sourceKind)

	s.mu.Lock()
	s.ensureMapsLocked()
	s.sandboxes[sandboxID] = state
	s.recordSandboxEventLocked(state, cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY, eventMessage)
	s.pruneStateLocked(now)
	resp := &cleanroomv1.CreateSandboxResponse{
		Sandbox: cloneSandboxLocked(state),
		Message: responseMessage,
	}
	s.mu.Unlock()

	if s.Logger != nil {
		s.Logger.Info("sandbox created from stored rootfs",
			"sandbox_id", sandboxID,
			"source_id", record.ID,
			"source_kind", sourceKind,
			"backend", backendName,
			"policy_hash", effectivePolicy.Hash,
		)
	}

	return resp, nil
}

func (s *Service) createSandboxFromCacheRecord(ctx context.Context, req *cleanroomv1.CreateSandboxRequest, compiled *policy.CompiledPolicy, record cachestore.Record) (*cleanroomv1.CreateSandboxResponse, error) {
	source, err := storedRootFSRecordFromCacheEntry(record)
	if err != nil {
		return nil, err
	}
	return s.createSandboxFromStoredRootFS(ctx, req, source, compiled)
}
