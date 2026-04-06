package controlservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/snapshotstore"
)

const workspaceSeedSnapshotNamePrefix = "workspace-seed:"

func workspaceSeedSnapshotName(backendName, policyHash string, repository *repositorycheckout.Checkout) string {
	if repository == nil {
		return ""
	}
	sum := sha256.New()
	for _, part := range []string{
		strings.TrimSpace(backendName),
		strings.TrimSpace(policyHash),
		strings.TrimSpace(repository.RemoteURL),
		strings.TrimSpace(repository.CommitSHA),
		strings.TrimSpace(repository.DestinationDir),
		fmt.Sprintf("%t", repository.Submodules),
		strings.TrimSpace(repository.Branch),
	} {
		_, _ = sum.Write([]byte(part))
		_, _ = sum.Write([]byte{0})
	}
	return workspaceSeedSnapshotNamePrefix + hex.EncodeToString(sum.Sum(nil))
}

func workspaceSeedSnapshotRecord(records []snapshotstore.Record, backendName, policyHash string, repository *repositorycheckout.Checkout) (snapshotstore.Record, bool) {
	expectedName := workspaceSeedSnapshotName(backendName, policyHash, repository)
	if expectedName == "" {
		return snapshotstore.Record{}, false
	}

	var (
		best  snapshotstore.Record
		found bool
	)
	for _, record := range records {
		if strings.TrimSpace(record.Name) != expectedName {
			continue
		}
		if strings.TrimSpace(record.Backend) != strings.TrimSpace(backendName) {
			continue
		}
		if strings.TrimSpace(record.PolicyHash) != strings.TrimSpace(policyHash) {
			continue
		}
		if !repositoryCheckoutsEqual(repositorycheckout.FromProto(record.Repository), repository) {
			continue
		}
		if !found || record.CreatedAt.After(best.CreatedAt) {
			best = record
			found = true
		}
	}
	return best, found
}

func (s *Service) lookupWorkspaceSeedSnapshot(ctx context.Context, backendName string, compiled *policy.CompiledPolicy, repository *repositorycheckout.Checkout) (snapshotstore.Record, bool, error) {
	if compiled == nil || repository == nil {
		return snapshotstore.Record{}, false, nil
	}
	store, err := s.snapshotStoreOrErr()
	if err != nil {
		return snapshotstore.Record{}, false, nil
	}
	records, err := store.List(ctx)
	if err != nil {
		return snapshotstore.Record{}, false, err
	}
	record, ok := workspaceSeedSnapshotRecord(records, backendName, compiled.Hash, repository)
	return record, ok, nil
}

func (s *Service) maybePublishWorkspaceSeedSnapshot(
	ctx context.Context,
	adapter backend.SnapshottingAdapter,
	sandboxID, backendName string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
) {
	if adapter == nil || compiled == nil || repository == nil {
		return
	}
	if !snapshotOperationsEnabledForBackend(backendName, s.Config) {
		return
	}

	store, err := s.snapshotStoreOrErr()
	if err != nil {
		return
	}

	if _, ok, err := s.lookupWorkspaceSeedSnapshot(ctx, backendName, compiled, repository); err == nil && ok {
		return
	} else if err != nil {
		s.logWorkspaceSeedWarning("lookup workspace seed snapshot", sandboxID, err)
		return
	}

	snapshotID := newSnapshotID()
	snapshotCfg := withSnapshotDriver(backendName, firecrackerCfg, firecrackerCfg.Snapshots.Driver)
	result, err := adapter.CreateSnapshot(ctx, backend.SnapshotRequest{
		SandboxID:         sandboxID,
		SnapshotID:        snapshotID,
		FirecrackerConfig: snapshotCfg,
	})
	if err != nil {
		s.logWorkspaceSeedWarning("publish workspace seed snapshot", sandboxID, err)
		return
	}

	record := snapshotstore.Record{
		SnapshotID:      snapshotID,
		SourceSandboxID: sandboxID,
		Backend:         backendName,
		Name:            workspaceSeedSnapshotName(backendName, compiled.Hash, repository),
		PolicyHash:      compiled.Hash,
		Policy:          compiled.ToProto(),
		Repository:      cloneRepositoryCheckout(repository).ToProto(),
		StorageDriver:   snapshotCfg.Snapshots.Driver,
		StorageRef:      strings.TrimSpace(result.StorageRef),
		CreatedAt:       time.Now().UTC(),
	}
	if err := store.Create(ctx, record); err != nil {
		deleteErr := adapter.DeleteSnapshot(ctx, backend.DeleteSnapshotRequest{
			SnapshotID:        snapshotID,
			StorageRef:        record.StorageRef,
			FirecrackerConfig: snapshotCfg,
		})
		if deleteErr != nil {
			s.logWorkspaceSeedWarning("rollback workspace seed snapshot after metadata failure", sandboxID, fmt.Errorf("%w (rollback failed: %v)", err, deleteErr))
			return
		}
		s.logWorkspaceSeedWarning("persist workspace seed snapshot metadata", sandboxID, err)
	}
}

func (s *Service) logWorkspaceSeedWarning(message, sandboxID string, err error) {
	if s == nil || s.Logger == nil || err == nil {
		return
	}
	s.Logger.Warn(message, "sandbox_id", sandboxID, "error", err)
}
