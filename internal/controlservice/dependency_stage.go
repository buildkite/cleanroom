package controlservice

import (
	"bytes"
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
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorybundle"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/repositorystore"
)

const (
	dependencyStageName                    = "dependency"
	dependencyStageProducerVersion         = "cleanroom/dependency-stage-v1"
	portableDependencyStageProducerVersion = "cleanroom/portable-dependency-stage-v1"
	dependencyStageReuseExact              = "exact"
	dependencyStageReusePortable           = "portable"
	portableDependencyOutputMode           = "outside-workspace"
)

type dependencyStagePlan struct {
	CacheKey                string
	PortableCacheKey        string
	ParentWorkspaceCacheKey string
	ParentRuntimeCacheKey   string
	BootstrapCommand        []string
	BootstrapRecipeDigest   string
	KeyFiles                []string
	KeyFilesDigest          string
	Portable                bool
}

type stageKeyFileDigest struct {
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
		Portable:              compiled.Dependencies.Reuse == policy.DependencyReusePortable,
	}, true
}

func (s *Service) finalizeDependencyStagePlan(
	ctx context.Context,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	commitBundle *repositorybundle.Bundle,
	backendName string,
	workspaceStageKey string,
	runtimeBaseKey string,
	plan dependencyStagePlan,
) (dependencyStagePlan, bool, error) {
	if compiled == nil || repository == nil || strings.TrimSpace(workspaceStageKey) == "" {
		return plan, false, nil
	}

	keyFilesDigest, err := s.dependencyStageKeyFilesDigest(ctx, repository, changeset, commitBundle, plan.KeyFiles)
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
	plan.ParentRuntimeCacheKey = strings.TrimSpace(runtimeBaseKey)
	plan.KeyFilesDigest = strings.TrimSpace(keyFilesDigest)
	if plan.Portable && strings.TrimSpace(runtimeBaseKey) != "" && strings.TrimSpace(keyFilesDigest) != "" {
		normalizedRepository := normalizeRepositoryCheckoutForComparison(repository)
		plan.PortableCacheKey = cachekey.PortableDependencyStageKey(cachekey.PortableDependencyStageInputs{
			Backend:                     strings.TrimSpace(backendName),
			RuntimeKey:                  strings.TrimSpace(runtimeBaseKey),
			CompiledPolicyHash:          strings.TrimSpace(compiled.Hash),
			CanonicalRemoteURL:          strings.TrimSpace(normalizedRepository.RemoteURL),
			SubmoduleMode:               workspaceStageSubmoduleMode(normalizedRepository),
			DestinationDir:              strings.TrimSpace(normalizedRepository.DestinationDir),
			CheckoutRefreshRecipeDigest: repositorycheckout.RefreshRecipeDigest(normalizedRepository),
			KeyFilesDigest:              strings.TrimSpace(keyFilesDigest),
			BootstrapRecipeDigest:       strings.TrimSpace(plan.BootstrapRecipeDigest),
			OutputMode:                  portableDependencyOutputMode,
			ProducerVersion:             portableDependencyStageProducerVersion,
		})
	}
	return plan, true, nil
}

func (s *Service) dependencyStageKeyFilesDigest(ctx context.Context, repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset, commitBundle *repositorybundle.Bundle, files []string) (string, error) {
	return s.stageKeyFilesDigest(ctx, repository, changeset, commitBundle, files, dependencyStageName)
}

func (s *Service) stageKeyFilesDigest(ctx context.Context, repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset, commitBundle *repositorybundle.Bundle, files []string, stageName string) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	if repository == nil {
		return "", fmt.Errorf("%s key files require a repository checkout", stageName)
	}
	if s.RepositoryStore == nil {
		return "", fmt.Errorf("%s key files require repository store", stageName)
	}
	if commitBundle != nil {
		var digest string
		err := s.RepositoryStore.WithRepository(ctx, repository.RemoteURL, "", repositorystore.FetchHints{}, func(repoDir string) error {
			return commitBundle.WithRepository(ctx, repoDir, func(bundleRepoDir string) error {
				var err error
				if changeset != nil {
					digest, err = stageKeyFilesDigestWithChangeset(bundleRepoDir, changeset, files, stageName)
				} else {
					digest, err = stageKeyFilesDigestAtCommit(ctx, bundleRepoDir, repository.CommitSHA, files, stageName)
				}
				return err
			})
		})
		if err != nil {
			return "", err
		}
		return digest, nil
	}
	if changeset != nil {
		var digest string
		err := s.RepositoryStore.WithRepository(ctx, repository.RemoteURL, repository.CommitSHA, repositorystore.FetchHints{}, func(repoDir string) error {
			var err error
			digest, err = stageKeyFilesDigestWithChangeset(repoDir, changeset, files, stageName)
			return err
		})
		if err != nil {
			return "", err
		}
		return digest, nil
	}
	var digest string
	err := s.RepositoryStore.WithRepository(ctx, repository.RemoteURL, repository.CommitSHA, repositorystore.FetchHints{}, func(repoDir string) error {
		var err error
		digest, err = stageKeyFilesDigestAtCommit(ctx, repoDir, repository.CommitSHA, files, stageName)
		return err
	})
	if err != nil {
		return "", err
	}
	return digest, nil
}

func stageKeyFilesDigestWithChangeset(repoDir string, changeset *repositorychangeset.Changeset, files []string, stageName string) (string, error) {
	manifest := make([]stageKeyFileDigest, 0, len(files))
	digests, err := changeset.DigestPathsFromBase(strings.TrimSpace(repoDir), files)
	if err != nil {
		return "", fmt.Errorf("read %s key files from repository changeset: %w", stageName, err)
	}
	for _, file := range digests {
		manifest = append(manifest, stageKeyFileDigest{
			Path:    file.Path,
			SHA256:  file.SHA256,
			Deleted: file.Deleted,
		})
	}

	payload, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal %s key file manifest: %w", stageName, err)
	}

	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func stageKeyFilesDigestAtCommit(ctx context.Context, repoDir, commitSHA string, files []string, stageName string) (string, error) {
	trimmedCommitSHA := strings.TrimSpace(commitSHA)
	if trimmedCommitSHA == "" {
		return "", fmt.Errorf("%s key file commit SHA is empty", stageName)
	}

	manifest := make([]stageKeyFileDigest, 0, len(files))
	for _, file := range files {
		digest, err := gitFileDigestAtCommit(ctx, strings.TrimSpace(repoDir), trimmedCommitSHA, file)
		if err != nil {
			return "", fmt.Errorf("read %s key file %q: %w", stageName, file, err)
		}
		manifest = append(manifest, stageKeyFileDigest{
			Path:   file,
			SHA256: digest,
		})
	}

	payload, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal %s key file manifest: %w", stageName, err)
	}

	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func gitFileDigestAtCommit(ctx context.Context, repoDir, commitSHA, file string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "show", commitSHA+":"+file)
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

func (s *Service) lookupDependencyStageCache(ctx context.Context, backendName string, compiled *policy.CompiledPolicy, repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset, plan dependencyStagePlan) (cachestore.Record, bool, string, error) {
	if compiled == nil || repository == nil || strings.TrimSpace(plan.CacheKey) == "" {
		return cachestore.Record{}, false, "", nil
	}
	store, err := s.cacheStoreOrErr()
	if err != nil {
		return cachestore.Record{}, false, "", nil
	}

	record, ok, err := store.GetReady(ctx, dependencyStageName, plan.CacheKey)
	if err != nil {
		return cachestore.Record{}, false, "", err
	}
	if !ok {
		return cachestore.Record{}, false, observability.CacheLookupReasonRecordNotFound, nil
	}
	if strings.TrimSpace(record.Backend) != strings.TrimSpace(backendName) {
		return cachestore.Record{}, false, observability.CacheLookupReasonBackendMismatch, nil
	}
	if reuseMode := strings.TrimSpace(record.ReuseMode); reuseMode != "" && reuseMode != dependencyStageReuseExact {
		return cachestore.Record{}, false, observability.CacheLookupReasonRecordNotFound, nil
	}
	if strings.TrimSpace(record.PolicyHash) != strings.TrimSpace(compiled.Hash) {
		return cachestore.Record{}, false, observability.CacheLookupReasonPolicyHashMismatch, nil
	}
	if strings.TrimSpace(record.ParentCacheKey) != strings.TrimSpace(plan.ParentWorkspaceCacheKey) {
		return cachestore.Record{}, false, observability.CacheLookupReasonParentStageChanged, nil
	}
	if !repositoryCheckoutsEqual(repositorycheckout.FromProto(record.Repository), repository) {
		return cachestore.Record{}, false, observability.CacheLookupReasonRepositoryChanged, nil
	}
	if !cacheRecordChangesetIDMatches(record.RepositoryChangesetID, repository, changeset) {
		return cachestore.Record{}, false, observability.CacheLookupReasonRepositoryChanged, nil
	}
	return record, true, "", nil
}

func (s *Service) lookupPortableDependencyStageCache(ctx context.Context, backendName string, compiled *policy.CompiledPolicy, repository *repositorycheckout.Checkout, plan dependencyStagePlan) (cachestore.Record, bool, string, error) {
	if compiled == nil || repository == nil || strings.TrimSpace(plan.PortableCacheKey) == "" {
		return cachestore.Record{}, false, "", nil
	}
	store, err := s.cacheStoreOrErr()
	if err != nil {
		return cachestore.Record{}, false, "", nil
	}

	record, ok, err := store.GetReady(ctx, dependencyStageName, plan.PortableCacheKey)
	if err != nil {
		return cachestore.Record{}, false, "", err
	}
	if !ok {
		return cachestore.Record{}, false, observability.CacheLookupReasonRecordNotFound, nil
	}
	if strings.TrimSpace(record.Backend) != strings.TrimSpace(backendName) {
		return cachestore.Record{}, false, observability.CacheLookupReasonBackendMismatch, nil
	}
	if strings.TrimSpace(record.ReuseMode) != dependencyStageReusePortable {
		return cachestore.Record{}, false, observability.CacheLookupReasonRecordNotFound, nil
	}
	if strings.TrimSpace(record.PolicyHash) != strings.TrimSpace(compiled.Hash) {
		return cachestore.Record{}, false, observability.CacheLookupReasonPolicyHashMismatch, nil
	}
	if strings.TrimSpace(record.ParentCacheKey) != strings.TrimSpace(plan.ParentRuntimeCacheKey) {
		return cachestore.Record{}, false, observability.CacheLookupReasonWorkspaceParentChanged, nil
	}
	if strings.TrimSpace(record.DependencyKeyFilesDigest) != strings.TrimSpace(plan.KeyFilesDigest) {
		return cachestore.Record{}, false, observability.CacheLookupReasonRecordNotFound, nil
	}
	if !portableDependencyRepositoriesCompatible(repositorycheckout.FromProto(record.Repository), repository) {
		return cachestore.Record{}, false, observability.CacheLookupReasonRepositoryChanged, nil
	}
	return record, true, "", nil
}

func portableDependencyRepositoriesCompatible(left, right *repositorycheckout.Checkout) bool {
	left = normalizeRepositoryCheckoutForComparison(left)
	right = normalizeRepositoryCheckoutForComparison(right)
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return strings.TrimSpace(left.RemoteURL) == strings.TrimSpace(right.RemoteURL) &&
		strings.TrimSpace(left.DestinationDir) == strings.TrimSpace(right.DestinationDir) &&
		left.Submodules == right.Submodules &&
		strings.TrimSpace(left.Branch) == strings.TrimSpace(right.Branch)
}

func (s *Service) restorePortableDependencyStageCache(
	ctx context.Context,
	adapter backend.Adapter,
	backendName string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	commitBundle *repositorybundle.Bundle,
	options *cleanroomv1.SandboxOptions,
	record cachestore.Record,
	reporter CreateSandboxReporter,
) (*cleanroomv1.CreateSandboxResponse, error) {
	restoreReq := &cleanroomv1.CreateSandboxRequest{
		Backend: backendName,
		Options: options,
	}
	restoreResp, err := s.createSandboxFromCacheRecord(ctx, restoreReq, compiled, record, reporter)
	if err != nil {
		return nil, err
	}

	sandboxID := restoreResp.GetSandbox().GetSandboxId()
	if err := s.bootstrapRepositoryInPersistentSandbox(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, commitBundle, true, reporter); err != nil {
		if cleanupErr := s.terminateCreatedSandbox(context.Background(), adapter, sandboxID); cleanupErr != nil {
			return nil, fmt.Errorf("refresh repository checkout after portable dependency stage restore: %w; cleanup failed: %v", err, cleanupErr)
		}
		return nil, fmt.Errorf("refresh repository checkout after portable dependency stage restore: %w", err)
	}
	if changeset != nil {
		if err := s.bootstrapRepositoryChangesetInPersistentSandbox(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, changeset, reporter); err != nil {
			if cleanupErr := s.terminateCreatedSandbox(context.Background(), adapter, sandboxID); cleanupErr != nil {
				return nil, fmt.Errorf("apply repository changeset after portable dependency stage restore: %w; cleanup failed: %v", err, cleanupErr)
			}
			return nil, fmt.Errorf("apply repository changeset after portable dependency stage restore: %w", err)
		}
	}
	if err := s.validatePortableDependencyStageKeyFiles(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, record.DependencyKeyFilesDigest); err != nil {
		if cleanupErr := s.terminateCreatedSandbox(context.Background(), adapter, sandboxID); cleanupErr != nil {
			return nil, fmt.Errorf("validate portable dependency stage key files: %w; cleanup failed: %v", err, cleanupErr)
		}
		return nil, fmt.Errorf("validate portable dependency stage key files: %w", err)
	}

	if sandbox := s.markRestoredSandboxRepositoryReady(sandboxID, repository, commitBundle, changeset != nil); sandbox != nil {
		restoreResp.Sandbox = sandbox
	}
	return restoreResp, nil
}

func (s *Service) markRestoredSandboxRepositoryReady(sandboxID string, repository *repositorycheckout.Checkout, commitBundle *repositorybundle.Bundle, hasChangeset bool) *cleanroomv1.Sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	sandbox, ok := s.sandboxes[sandboxID]
	if !ok {
		return nil
	}
	sandbox.Repository = cloneRepositoryCheckout(repository)
	sandbox.RepositoryCommitBundle = cloneRepositoryCommitBundle(commitBundle)
	sandbox.RepositoryHasChangeset = hasChangeset
	sandbox.RepositoryChangesetPendingExecution = hasChangeset
	sandbox.UpdatedAt = s.clock().Now()
	return cloneSandboxLocked(sandbox)
}

func (s *Service) validatePortableDependencyStageKeyFiles(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	expectedDigest string,
) error {
	expectedDigest = strings.TrimSpace(expectedDigest)
	if expectedDigest == "" {
		return fmt.Errorf("portable dependency stage is missing dependency key-file digest")
	}
	command, err := dependencyStageKeyFilesDigestCommand(repository, compiled.Dependencies.KeyFiles)
	if err != nil {
		return err
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	result, err := s.runPersistentSandboxCommand(ctx, adapter, sandboxID, compiled, firecrackerCfg, s.ids().NewExecutionID(), command, nil, backend.OutputStream{
		OnStdout: func(chunk []byte) {
			_, _ = stdout.Write(chunk)
		},
		OnStderr: func(chunk []byte) {
			_, _ = stderr.Write(chunk)
		},
	})
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("%s", message)
	}
	if result == nil {
		return fmt.Errorf("portable dependency key-file validation returned no result")
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message == "" {
			message = fmt.Sprintf("portable dependency key-file validation failed with exit code %d", result.ExitCode)
		}
		return fmt.Errorf("%s", message)
	}
	actualDigest := strings.TrimSpace(stdout.String())
	if actualDigest != expectedDigest {
		return fmt.Errorf("portable dependency key-file digest mismatch: expected %s got %s", expectedDigest, actualDigest)
	}
	return nil
}

func dependencyStageKeyFilesDigestCommand(repository *repositorycheckout.Checkout, files []string) ([]string, error) {
	if repository == nil {
		return nil, fmt.Errorf("portable dependency key-file validation requires a repository checkout")
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("portable dependency key-file validation requires dependency key files")
	}
	var script strings.Builder
	script.WriteString("set -eu\n")
	script.WriteString("dest=" + dependencyStageShellQuote(repository.DestinationDir) + "\n")
	script.WriteString(`manifest='['` + "\n")
	script.WriteString(`sep=''` + "\n")
	for _, file := range files {
		pathJSON, err := json.Marshal(file)
		if err != nil {
			return nil, fmt.Errorf("marshal dependency key-file path: %w", err)
		}
		script.WriteString("file=" + dependencyStageShellQuote(file) + "\n")
		script.WriteString("path_json=" + dependencyStageShellQuote(string(pathJSON)) + "\n")
		script.WriteString(`path="$dest/$file"` + "\n")
		script.WriteString(`if [ -L "$path" ]; then` + "\n")
		script.WriteString(`  target="$(readlink "$path")"` + "\n")
		script.WriteString(`  hex="$(printf '%s' "$target" | sha256sum | awk '{print $1}')"` + "\n")
		script.WriteString(`  entry="{\"path\":${path_json},\"sha256\":\"sha256:${hex}\"}"` + "\n")
		script.WriteString(`elif [ -e "$path" ]; then` + "\n")
		script.WriteString(`  hex="$(sha256sum "$path" | awk '{print $1}')"` + "\n")
		script.WriteString(`  entry="{\"path\":${path_json},\"sha256\":\"sha256:${hex}\"}"` + "\n")
		script.WriteString("else\n")
		script.WriteString(`  entry="{\"path\":${path_json},\"deleted\":true}"` + "\n")
		script.WriteString("fi\n")
		script.WriteString(`manifest="${manifest}${sep}${entry}"` + "\n")
		script.WriteString(`sep=','` + "\n")
	}
	script.WriteString(`manifest="${manifest}]"` + "\n")
	script.WriteString(`digest="$(printf '%s' "$manifest" | sha256sum | awk '{print $1}')"` + "\n")
	script.WriteString(`printf 'sha256:%s\n' "$digest"` + "\n")
	return []string{"sh", "-lc", script.String()}, nil
}

func dependencyStageShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func (s *Service) maybePublishDependencyStageCache(
	ctx context.Context,
	adapter backend.SnapshottingAdapter,
	sandboxID, backendName string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
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

	var exactRecord cachestore.Record
	exactPublished := false
	exactReusable := false
	if record, ok, _, err := s.lookupDependencyStageCache(ctx, backendName, compiled, repository, changeset, plan); err == nil && ok {
		exactRecord = record
		exactPublished = true
		if replacedRecord == nil || strings.TrimSpace(record.CacheKey) != strings.TrimSpace(replacedRecord.CacheKey) {
			exactReusable = true
			s.logDependencyStageAlreadyPublished(record)
			if !plan.Portable || strings.TrimSpace(plan.PortableCacheKey) == "" {
				return
			}
			if portableRecord, ok, _, portableErr := s.lookupPortableDependencyStageCache(ctx, backendName, compiled, repository, plan); portableErr == nil && ok {
				s.logDependencyStageAlreadyPublished(portableRecord)
				return
			} else if portableErr != nil {
				s.logDependencyStageWarning("lookup portable dependency stage cache", sandboxID, portableErr)
				return
			}
		}
	} else if err != nil {
		s.logDependencyStageWarning("lookup dependency stage cache", sandboxID, err)
		return
	}

	if exactPublished && exactReusable && strings.TrimSpace(plan.PortableCacheKey) != "" {
		portableRecord := portableDependencyStageRecordFromExactRecord(exactRecord, compiled, repository, changeset, plan)
		if err := store.Upsert(ctx, portableRecord); err != nil {
			s.logDependencyStageWarning("persist portable dependency stage cache metadata", sandboxID, err)
			return
		}
		s.logDependencyStagePublished(portableRecord, sandboxID, false)
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
		CacheKey:                 plan.CacheKey,
		Stage:                    dependencyStageName,
		ReuseMode:                dependencyStageReuseExact,
		State:                    cacheStateReady,
		BackingSnapshotID:        strings.TrimSpace(snapshotID),
		Backend:                  backendName,
		PolicyHash:               compiled.Hash,
		Policy:                   compiled.ToProto(),
		Repository:               cloneRepositoryCheckout(normalizeRepositoryCheckoutForComparison(repository)).ToProto(),
		RepositoryHasChangeset:   changeset != nil,
		RepositoryChangesetID:    repositoryChangesetID(repository, changeset),
		ParentCacheKey:           plan.ParentWorkspaceCacheKey,
		StorageDriver:            snapshotCfg.Snapshots.Driver,
		StorageRef:               strings.TrimSpace(result.StorageRef),
		DependencyKeyFilesDigest: plan.KeyFilesDigest,
		CreatedAt:                s.clock().Now(),
		LastUsedAt:               s.clock().Now(),
		ProducerVersion:          dependencyStageProducerVersion,
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

	if strings.TrimSpace(plan.PortableCacheKey) != "" {
		portableRecord := portableDependencyStageRecordFromExactRecord(record, compiled, repository, changeset, plan)
		if err := store.Upsert(ctx, portableRecord); err != nil {
			s.logDependencyStageWarning("persist portable dependency stage cache metadata", sandboxID, err)
		} else {
			s.logDependencyStagePublished(portableRecord, sandboxID, false)
		}
	}

	if replacedRecord != nil && strings.TrimSpace(replacedRecord.CacheKey) == plan.CacheKey {
		if err := s.deleteWorkspaceStageCacheSnapshot(ctx, adapter, backendName, firecrackerCfg, *replacedRecord); err != nil {
			s.logDependencyStageWarning("delete replaced dependency stage cache snapshot", sandboxID, err)
		}
	}
}

func portableDependencyStageRecordFromExactRecord(
	exactRecord cachestore.Record,
	compiled *policy.CompiledPolicy,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	plan dependencyStagePlan,
) cachestore.Record {
	record := exactRecord
	record.CacheKey = strings.TrimSpace(plan.PortableCacheKey)
	record.ReuseMode = dependencyStageReusePortable
	record.PolicyHash = compiled.Hash
	record.Policy = compiled.ToProto()
	record.Repository = cloneRepositoryCheckout(normalizeRepositoryCheckoutForComparison(repository)).ToProto()
	record.RepositoryHasChangeset = changeset != nil
	record.RepositoryChangesetID = repositoryChangesetID(repository, changeset)
	record.ParentCacheKey = strings.TrimSpace(plan.ParentRuntimeCacheKey)
	record.InputManifestDigest = strings.TrimSpace(plan.KeyFilesDigest)
	record.DependencyKeyFilesDigest = strings.TrimSpace(plan.KeyFilesDigest)
	record.CheckoutRefreshRequired = true
	record.ProducerVersion = portableDependencyStageProducerVersion
	return record
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
