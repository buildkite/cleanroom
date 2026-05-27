package controlservice

import (
	"context"
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type storedRootFSRecord struct {
	ID                                  string
	Kind                                string
	SnapshotID                          string
	Backend                             string
	Policy                              *policy.CompiledPolicy
	Repository                          *repositorycheckout.Checkout
	RepositoryHasChangeset              bool
	RepositoryChangesetPendingExecution bool
	StorageDriver                       string
	StorageRef                          string
}

func storedRootFSRecordFromSnapshot(record snapshotstore.Record) (storedRootFSRecord, error) {
	compiled, err := policy.FromProto(record.Policy)
	if err != nil {
		return storedRootFSRecord{}, fmt.Errorf("invalid snapshot policy %q: %w", record.SnapshotID, err)
	}
	return storedRootFSRecord{
		ID:                     strings.TrimSpace(record.SnapshotID),
		Kind:                   "snapshot",
		SnapshotID:             strings.TrimSpace(record.SnapshotID),
		Backend:                strings.TrimSpace(record.Backend),
		Policy:                 compiled,
		Repository:             repositorycheckout.FromProto(record.Repository),
		RepositoryHasChangeset: record.RepositoryHasChangeset,
		StorageDriver:          strings.TrimSpace(record.StorageDriver),
		StorageRef:             strings.TrimSpace(record.StorageRef),
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
	if strings.TrimSpace(record.ReuseMode) == dependencyStageReusePortable {
		sourceKind = "portable dependency stage cache"
	}
	return storedRootFSRecord{
		ID:                                  strings.TrimSpace(record.CacheKey),
		Kind:                                sourceKind,
		SnapshotID:                          strings.TrimSpace(record.BackingSnapshotID),
		Backend:                             strings.TrimSpace(record.Backend),
		Policy:                              compiled,
		Repository:                          repositorycheckout.FromProto(record.Repository),
		RepositoryHasChangeset:              record.RepositoryHasChangeset,
		RepositoryChangesetPendingExecution: record.RepositoryHasChangeset,
		StorageDriver:                       strings.TrimSpace(record.StorageDriver),
		StorageRef:                          strings.TrimSpace(record.StorageRef),
	}, nil
}

func (s *Service) createSandboxFromStoredRootFS(ctx context.Context, req *cleanroomv1.CreateSandboxRequest, record storedRootFSRecord, overridePolicy *policy.CompiledPolicy, cacheOutputVolumes []backend.CacheOutputVolumeSpec, reporter CreateSandboxReporter) (*cleanroomv1.CreateSandboxResponse, error) {
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
	firecrackerCfg = withPolicyResourceMinimums(firecrackerCfg, effectivePolicy.Resources)
	firecrackerCfg = withBackendLaunchResourceDefaults(firecrackerCfg)
	firecrackerCfg = withSnapshotDriver(backendName, firecrackerCfg, record.StorageDriver)
	rootFSAttrs := rootFSMinimumTraceAttributes(firecrackerCfg)
	trace.SpanFromContext(ctx).SetAttributes(rootFSAttrs...)

	now := s.clock().Now()
	sandboxID := s.ids().NewSandboxID()
	owner, _ := ownerForContext(ctx)
	provisionSnapshotID := strings.TrimSpace(record.SnapshotID)
	if provisionSnapshotID == "" {
		provisionSnapshotID = strings.TrimSpace(record.ID)
	}
	sourceKind := strings.TrimSpace(record.Kind)
	sourceID := strings.TrimSpace(record.ID)
	if sourceID == "" {
		sourceID = provisionSnapshotID
	}
	backingSnapshotID := strings.TrimSpace(record.SnapshotID)
	if backingSnapshotID == "" {
		backingSnapshotID = provisionSnapshotID
	}
	switch sourceKind {
	case "snapshot":
		emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RESTORE_SNAPSHOT, "restoring snapshot")
	case "dependency stage cache":
		emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RESTORE_DEPENDENCY_STAGE_CACHE, "restoring dependency stage cache")
	case "services stage cache":
		emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RESTORE_SERVICES_STAGE_CACHE, "restoring services stage cache")
	case "portable dependency stage cache":
		emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RESTORE_DEPENDENCY_STAGE_CACHE, "restoring portable dependency stage cache")
	case "workspace stage cache":
		emitCreateSandboxMessage(reporter, cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_RESTORE_WORKSPACE_STAGE_CACHE, "restoring workspace stage cache")
	}
	state := &sandboxState{
		ID:                                  sandboxID,
		Backend:                             backendName,
		Capabilities:                        backend.CapabilitiesForAdapter(adapter),
		Policy:                              effectivePolicy,
		Firecracker:                         firecrackerCfg,
		Repository:                          cloneRepositoryCheckout(record.Repository),
		RepositoryHasChangeset:              record.RepositoryHasChangeset,
		RepositoryChangesetPendingExecution: record.RepositoryChangesetPendingExecution,
		SourceKind:                          sourceKind,
		SourceID:                            sourceID,
		BackingSnapshotID:                   backingSnapshotID,
		Owner:                               owner,
		CreatedAt:                           now,
		UpdatedAt:                           now,
		Status:                              cleanroomv1.SandboxStatus_SANDBOX_STATUS_PROVISIONING,
		events:                              newEventFeed[*cleanroomv1.SandboxEvent](s.retention().maxRetainedSandboxEvents),
		Done:                                make(chan struct{}),
	}
	s.mu.Lock()
	s.ensureMapsLocked()
	s.sandboxes[sandboxID] = state
	s.mu.Unlock()
	restoreAttrs := append([]attribute.KeyValue{
		attribute.String(observability.AttrBackend, backendName),
		attribute.String(observability.AttrSandboxID, sandboxID),
		attribute.String("cleanroom.source_kind", sourceKind),
	}, rootFSAttrs...)
	if err := s.traceCreateSandboxPhase(ctx, "cleanroom.sandbox.restore_snapshot", restoreAttrs, func(ctx context.Context) error {
		return snapshotAdapter.ProvisionSandboxFromSnapshot(ctx, backend.ProvisionFromSnapshotRequest{
			SandboxID:          sandboxID,
			SnapshotID:         provisionSnapshotID,
			StorageRef:         record.StorageRef,
			Policy:             effectivePolicy,
			CacheOutputVolumes: cloneCacheOutputVolumeSpecs(cacheOutputVolumes),
			FirecrackerConfig:  firecrackerCfg,
		})
	}); err != nil {
		if stateErr := s.dropProvisioningSandboxAfterCreateError(sandboxID); stateErr != nil {
			return nil, stateErr
		}
		return nil, fmt.Errorf("provision sandbox from snapshot: %w", err)
	}
	if err := s.ensureSandboxCreateStillProvisioning(sandboxID); err != nil {
		return nil, err
	}

	if sourceKind == "" {
		sourceKind = "stored rootfs"
	}
	eventMessage := fmt.Sprintf("sandbox created from %s %s and ready", sourceKind, sourceID)
	responseMessage := fmt.Sprintf("sandbox created from %s and ready", sourceKind)

	s.mu.Lock()
	s.ensureMapsLocked()
	var stateErr error
	state, stateErr = s.sandboxCreateProvisioningStateLocked(sandboxID)
	if stateErr != nil {
		s.mu.Unlock()
		return nil, stateErr
	}
	s.recordSandboxEventLocked(state, cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY, eventMessage)
	s.pruneStateLocked(now)
	resp := &cleanroomv1.CreateSandboxResponse{
		Sandbox:           cloneSandboxLocked(state),
		Message:           responseMessage,
		SourceKind:        state.SourceKind,
		SourceId:          state.SourceID,
		BackingSnapshotId: state.BackingSnapshotID,
	}
	s.mu.Unlock()

	if s.Logger != nil {
		s.Logger.Info("sandbox created from stored rootfs",
			"sandbox_id", sandboxID,
			"source_id", sourceID,
			"source_kind", sourceKind,
			"backing_snapshot_id", backingSnapshotID,
			"backend", backendName,
			"policy_hash", effectivePolicy.Hash,
		)
	}

	return resp, nil
}

func (s *Service) createSandboxFromCacheRecord(ctx context.Context, req *cleanroomv1.CreateSandboxRequest, compiled *policy.CompiledPolicy, record cachestore.Record, cacheOutputVolumes []backend.CacheOutputVolumeSpec, reporter CreateSandboxReporter) (*cleanroomv1.CreateSandboxResponse, error) {
	source, err := storedRootFSRecordFromCacheEntry(record)
	if err != nil {
		return nil, err
	}
	backingSnapshotID := strings.TrimSpace(source.SnapshotID)
	if backingSnapshotID == "" {
		backingSnapshotID = strings.TrimSpace(source.ID)
	}
	if backingSnapshotID != "" {
		if err := s.beginSnapshotUse(backingSnapshotID); err != nil {
			return nil, err
		}
		defer s.finishSnapshotUse(backingSnapshotID)
	}
	return s.createSandboxFromStoredRootFS(ctx, req, source, compiled, cacheOutputVolumes, reporter)
}
