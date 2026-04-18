package controlservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachekey"
	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

const (
	dependencyStageName            = "dependency"
	dependencyStageProducerVersion = "cleanroom/dependency-stage-v1"
)

type dependencyStagePlan struct {
	CacheKey                string
	ParentWorkspaceCacheKey string
	BootstrapCommand        []string
	BootstrapRecipeDigest   string
	KeyFiles                []string
	KeyFilesDigest          string
}

type dependencyKeyFileDigest struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

func dependencyStagePlanForRepository(compiled *policy.CompiledPolicy, repository *repositorycheckout.Checkout) (dependencyStagePlan, bool) {
	if compiled == nil || repository == nil || !compiled.Dependencies.Enabled() {
		return dependencyStagePlan{}, false
	}

	bootstrapCommand := repositorycheckout.WrapCommandInWorkdir(compiled.Dependencies.Command, repository)
	bootstrapRecipeDigest := repositorycheckout.WorkdirRecipeDigest(compiled.Dependencies.Command, repository)
	if len(bootstrapCommand) == 0 || strings.TrimSpace(bootstrapRecipeDigest) == "" {
		return dependencyStagePlan{}, false
	}

	return dependencyStagePlan{
		BootstrapCommand:      bootstrapCommand,
		BootstrapRecipeDigest: bootstrapRecipeDigest,
		KeyFiles:              append([]string(nil), compiled.Dependencies.KeyFiles...),
	}, true
}

func (s *Service) finalizeDependencyStagePlan(
	ctx context.Context,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	workspaceStageKey string,
	plan dependencyStagePlan,
) (dependencyStagePlan, bool, error) {
	if compiled == nil || repository == nil || strings.TrimSpace(workspaceStageKey) == "" {
		return plan, false, nil
	}

	keyFilesDigest, err := s.dependencyStageKeyFilesDigest(ctx, repository, changeset, plan.KeyFiles)
	if err != nil {
		return plan, false, err
	}

	cacheKey := cachekey.DependencyStageKey(cachekey.DependencyStageInputs{
		WorkspaceKey:          strings.TrimSpace(workspaceStageKey),
		CompiledPolicyHash:    strings.TrimSpace(compiled.Hash),
		KeyFilesDigest:        strings.TrimSpace(keyFilesDigest),
		BootstrapRecipeDigest: strings.TrimSpace(plan.BootstrapRecipeDigest),
	})
	if strings.TrimSpace(cacheKey) == "" {
		return plan, false, nil
	}

	plan.CacheKey = cacheKey
	plan.ParentWorkspaceCacheKey = strings.TrimSpace(workspaceStageKey)
	plan.KeyFilesDigest = strings.TrimSpace(keyFilesDigest)
	return plan, true, nil
}

func (s *Service) dependencyStageKeyFilesDigest(ctx context.Context, repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset, files []string) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	if repository == nil {
		return "", fmt.Errorf("dependency key files require a repository checkout")
	}
	if s.RepositoryMirrors == nil {
		return "", fmt.Errorf("dependency key files require repository mirrors")
	}
	mirrorPath, err := s.RepositoryMirrors.MirrorPath(repository.RemoteURL)
	if err != nil {
		mirrorPath, err = s.RepositoryMirrors.EnsureMirror(ctx, repository.RemoteURL)
		if err != nil {
			return "", err
		}
	}
	digest, err := dependencyStageKeyFilesDigestFromMirror(ctx, mirrorPath, repository.CommitSHA, changeset, files)
	if err == nil {
		return digest, nil
	}
	if ensureErr := s.RepositoryMirrors.EnsureMirrorContains(ctx, repository.RemoteURL, repository.CommitSHA); ensureErr != nil {
		return "", ensureErr
	}
	mirrorPath, err = s.RepositoryMirrors.MirrorPath(repository.RemoteURL)
	if err != nil {
		return "", err
	}
	return dependencyStageKeyFilesDigestFromMirror(ctx, mirrorPath, repository.CommitSHA, changeset, files)
}

func dependencyStageKeyFilesDigestFromMirror(ctx context.Context, mirrorPath, commitSHA string, changeset *repositorychangeset.Changeset, files []string) (string, error) {
	trimmedMirrorPath := strings.TrimSpace(mirrorPath)
	trimmedCommitSHA := strings.TrimSpace(commitSHA)
	if trimmedCommitSHA == "" {
		return "", fmt.Errorf("dependency key file commit SHA is empty")
	}

	manifest := make([]dependencyKeyFileDigest, 0, len(files))
	if changeset != nil {
		digests, err := changeset.DigestPathsFromBase(trimmedMirrorPath, files)
		if err != nil {
			return "", fmt.Errorf("read dependency key files from repository changeset: %w", err)
		}
		for _, file := range digests {
			manifest = append(manifest, dependencyKeyFileDigest{
				Path:    file.Path,
				SHA256:  file.SHA256,
				Deleted: file.Deleted,
			})
		}
		payload, err := json.Marshal(manifest)
		if err != nil {
			return "", fmt.Errorf("marshal dependency key file manifest: %w", err)
		}

		sum := sha256.Sum256(payload)
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	for _, file := range files {
		if trimmedMirrorPath == "" {
			return "", fmt.Errorf("dependency key file mirror path is empty")
		}
		digest, err := gitFileDigestAtCommit(ctx, trimmedMirrorPath, trimmedCommitSHA, file)
		if err != nil {
			return "", fmt.Errorf("read dependency key file %q: %w", file, err)
		}
		manifest = append(manifest, dependencyKeyFileDigest{
			Path:   file,
			SHA256: digest,
		})
	}

	payload, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal dependency key file manifest: %w", err)
	}

	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func gitFileDigestAtCommit(ctx context.Context, repoPath, commitSHA, file string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "show", commitSHA+":"+file)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("%s", message)
	}

	sum := sha256.Sum256(output)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (s *Service) lookupDependencyStageCache(ctx context.Context, backendName string, compiled *policy.CompiledPolicy, repository *repositorycheckout.Checkout, plan dependencyStagePlan) (cachestore.Record, bool, error) {
	if compiled == nil || repository == nil || strings.TrimSpace(plan.CacheKey) == "" {
		return cachestore.Record{}, false, nil
	}
	store, err := s.cacheStoreOrErr()
	if err != nil {
		return cachestore.Record{}, false, nil
	}

	record, ok, err := store.GetReady(ctx, dependencyStageName, plan.CacheKey)
	if err != nil {
		return cachestore.Record{}, false, err
	}
	if !ok {
		return cachestore.Record{}, false, nil
	}
	if strings.TrimSpace(record.Backend) != strings.TrimSpace(backendName) {
		return cachestore.Record{}, false, nil
	}
	if strings.TrimSpace(record.PolicyHash) != strings.TrimSpace(compiled.Hash) {
		return cachestore.Record{}, false, nil
	}
	if strings.TrimSpace(record.ParentCacheKey) != strings.TrimSpace(plan.ParentWorkspaceCacheKey) {
		return cachestore.Record{}, false, nil
	}
	if !repositoryCheckoutsEqual(repositorycheckout.FromProto(record.Repository), repository) {
		return cachestore.Record{}, false, nil
	}
	return record, true, nil
}

func (s *Service) maybePublishDependencyStageCache(
	ctx context.Context,
	adapter backend.SnapshottingAdapter,
	sandboxID, backendName string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	plan dependencyStagePlan,
	replacedRecord *cachestore.Record,
) {
	if adapter == nil || compiled == nil || repository == nil || strings.TrimSpace(plan.CacheKey) == "" {
		return
	}
	if !snapshotOperationsEnabledForBackend(backendName, s.Config) {
		return
	}

	store, err := s.cacheStoreOrErr()
	if err != nil {
		return
	}

	if record, ok, err := s.lookupDependencyStageCache(ctx, backendName, compiled, repository, plan); err == nil && ok {
		if replacedRecord == nil || strings.TrimSpace(record.CacheKey) != strings.TrimSpace(replacedRecord.CacheKey) {
			s.logDependencyStageAlreadyPublished(record)
			return
		}
	} else if err != nil {
		s.logDependencyStageWarning("lookup dependency stage cache", sandboxID, err)
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
		s.logDependencyStageWarning("publish dependency stage cache", sandboxID, err)
		return
	}

	record := cachestore.Record{
		CacheKey:          plan.CacheKey,
		Stage:             dependencyStageName,
		State:             cacheStateReady,
		BackingSnapshotID: strings.TrimSpace(snapshotID),
		Backend:           backendName,
		PolicyHash:        compiled.Hash,
		Policy:            compiled.ToProto(),
		Repository:        cloneRepositoryCheckout(normalizeRepositoryCheckoutForComparison(repository)).ToProto(),
		ParentCacheKey:    plan.ParentWorkspaceCacheKey,
		StorageDriver:     snapshotCfg.Snapshots.Driver,
		StorageRef:        strings.TrimSpace(result.StorageRef),
		CreatedAt:         s.clock().Now(),
		LastUsedAt:        s.clock().Now(),
		ProducerVersion:   dependencyStageProducerVersion,
	}

	persist := store.Create
	if replacedRecord != nil && strings.TrimSpace(replacedRecord.CacheKey) == plan.CacheKey {
		persist = store.Upsert
	}

	if err := persist(ctx, record); err != nil {
		deleteErr := adapter.DeleteSnapshot(ctx, backend.DeleteSnapshotRequest{
			SnapshotID:        snapshotID,
			StorageRef:        record.StorageRef,
			FirecrackerConfig: snapshotCfg,
		})
		if deleteErr != nil {
			s.logDependencyStageWarning("rollback dependency stage cache after metadata failure", sandboxID, fmt.Errorf("%w (rollback failed: %v)", err, deleteErr))
			return
		}
		s.logDependencyStageWarning("persist dependency stage cache metadata", sandboxID, err)
		return
	}

	s.logDependencyStagePublished(record, sandboxID, replacedRecord != nil && strings.TrimSpace(replacedRecord.CacheKey) == plan.CacheKey)

	if replacedRecord != nil && strings.TrimSpace(replacedRecord.CacheKey) == plan.CacheKey {
		if err := s.deleteWorkspaceStageCacheSnapshot(ctx, adapter, backendName, firecrackerCfg, *replacedRecord); err != nil {
			s.logDependencyStageWarning("delete replaced dependency stage cache snapshot", sandboxID, err)
		}
	}
}

func (s *Service) bootstrapDependencyStageInPersistentSandbox(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	plan dependencyStagePlan,
	reporter CreateSandboxReporter,
) error {
	if adapter == nil || compiled == nil || strings.TrimSpace(sandboxID) == "" || len(plan.BootstrapCommand) == 0 {
		return nil
	}
	if s.Logger != nil {
		s.Logger.Debug("starting dependency stage bootstrap",
			"sandbox_id", sandboxID,
			"cache_key", strings.TrimSpace(plan.CacheKey),
		)
	}

	bootstrapExecutionID, result, stdout, stderr, err := s.runPersistentBootstrapCommand(
		ctx,
		adapter,
		sandboxID,
		compiled,
		firecrackerCfg,
		cleanroomv1.CreateSandboxPhase_CREATE_SANDBOX_PHASE_BOOTSTRAP_DEPENDENCIES,
		plan.BootstrapCommand,
		nil,
		reporter,
	)
	if s.Logger != nil {
		s.Logger.Debug("dependency stage bootstrap execution finished",
			"sandbox_id", sandboxID,
			"execution_id", bootstrapExecutionID,
			"exit_code", func() int {
				if result == nil {
					return -1
				}
				return result.ExitCode
			}(),
			"error", err,
		)
	}
	return persistentBootstrapCommandError(result, stdout, stderr, err, "dependency stage bootstrap returned no result", "dependency stage bootstrap failed with exit code %d")
}

func (s *Service) logDependencyStageCacheHit(record cachestore.Record) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug("dependency stage cache hit",
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
	)
}

func (s *Service) logDependencyStageCacheMiss(backendName, cacheKey string) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug("dependency stage cache miss",
		"cache_key", strings.TrimSpace(cacheKey),
		"backend", strings.TrimSpace(backendName),
	)
}

func (s *Service) logDependencyStageAlreadyPublished(record cachestore.Record) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Debug("dependency stage cache already published",
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
	)
}

func (s *Service) logDependencyStagePublished(record cachestore.Record, sandboxID string, replaced bool) {
	if s == nil || s.Logger == nil {
		return
	}
	message := "dependency stage cache published"
	if replaced {
		message = "dependency stage cache replaced"
	}
	s.Logger.Info(message,
		"sandbox_id", strings.TrimSpace(sandboxID),
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
	)
}

func (s *Service) logDependencyStageRestore(record cachestore.Record, sandboxID string) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Info("dependency stage cache restored",
		"sandbox_id", strings.TrimSpace(sandboxID),
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
	)
}

func (s *Service) logDependencyStageRestoreWarning(record cachestore.Record, err error) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Warn("restore dependency stage cache",
		"cache_key", strings.TrimSpace(record.CacheKey),
		"backing_snapshot_id", strings.TrimSpace(record.BackingSnapshotID),
		"backend", strings.TrimSpace(record.Backend),
		"error", err,
	)
}

func (s *Service) logDependencyStageWarning(operation, sandboxID string, err error) {
	if s == nil || s.Logger == nil || err == nil {
		return
	}
	s.Logger.Warn(operation,
		"sandbox_id", strings.TrimSpace(sandboxID),
		"error", err,
	)
}
