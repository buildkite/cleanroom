package controlservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"path"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
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
	"github.com/buildkite/cleanroom/internal/submodule"
)

const (
	dependencyStageName                    = "dependency"
	dependencyStageProducerVersion         = "cleanroom/dependency-stage-v1"
	portableDependencyStageProducerVersion = "cleanroom/portable-dependency-stage-v1"
	dependencyStageReuseExact              = "exact"
	dependencyStageReusePortable           = "portable"
	portableDependencyOutputMode           = "outside-workspace"
)

var dependencyStageToolchainInputFiles = []string{
	".mise.toml",
	".tool-versions",
	"mise.lock",
	"mise.toml",
}

type dependencyStagePlan struct {
	CacheKey                string
	PortableCacheKey        string
	ParentWorkspaceCacheKey string
	ParentRuntimeCacheKey   string
	BootstrapCommand        []string
	BootstrapRecipeDigest   string
	KeyFiles                []string
	ExpandedKeyFiles        []string
	KeyFilesDigest          string
	ToolchainInputFiles     []string
	ToolchainInputsDigest   string
	Portable                bool
}

type stageKeyFileDigest struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

type stageInputFileDigest struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
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
		ToolchainInputFiles:   normalizeStageOptionalKeyFiles(dependencyStageToolchainInputFiles),
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

	inputDigests, err := s.dependencyStagePlanInputDigestDetails(ctx, repository, changeset, commitBundle, plan.KeyFiles, plan.ToolchainInputFiles)
	if err != nil {
		return plan, false, err
	}
	keyFilesDigest := inputDigests.KeyFilesDigest
	toolchainInputsDigest := inputDigests.ToolchainInputsDigest

	cacheKey := cachekey.DependencyStageKey(cachekey.DependencyStageInputs{
		WorkspaceKey:          strings.TrimSpace(workspaceStageKey),
		CompiledPolicyHash:    strings.TrimSpace(compiled.Hash),
		KeyFilesDigest:        strings.TrimSpace(keyFilesDigest),
		ToolchainInputsDigest: strings.TrimSpace(toolchainInputsDigest),
		BootstrapRecipeDigest: strings.TrimSpace(plan.BootstrapRecipeDigest),
	})
	if strings.TrimSpace(cacheKey) == "" {
		return plan, false, nil
	}

	plan.CacheKey = cacheKey
	plan.ParentWorkspaceCacheKey = strings.TrimSpace(workspaceStageKey)
	plan.ParentRuntimeCacheKey = strings.TrimSpace(runtimeBaseKey)
	plan.KeyFilesDigest = strings.TrimSpace(keyFilesDigest)
	plan.ExpandedKeyFiles = append([]string(nil), inputDigests.ExpandedKeyFiles...)
	plan.ToolchainInputsDigest = strings.TrimSpace(toolchainInputsDigest)
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
			ToolchainInputsDigest:       strings.TrimSpace(toolchainInputsDigest),
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

func (s *Service) dependencyStagePlanInputDigests(
	ctx context.Context,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	commitBundle *repositorybundle.Bundle,
	keyFiles []string,
	toolchainInputFiles []string,
) (string, string, error) {
	details, err := s.dependencyStagePlanInputDigestDetails(ctx, repository, changeset, commitBundle, keyFiles, toolchainInputFiles)
	if err != nil {
		return "", "", err
	}
	return details.KeyFilesDigest, details.ToolchainInputsDigest, nil
}

type dependencyStagePlanInputDigestDetails struct {
	KeyFilesDigest        string
	ExpandedKeyFiles      []string
	ToolchainInputsDigest string
}

func (s *Service) dependencyStagePlanInputDigestDetails(
	ctx context.Context,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	commitBundle *repositorybundle.Bundle,
	keyFiles []string,
	toolchainInputFiles []string,
) (dependencyStagePlanInputDigestDetails, error) {
	if len(keyFiles) == 0 && len(toolchainInputFiles) == 0 {
		return dependencyStagePlanInputDigestDetails{}, nil
	}
	if repository == nil {
		return dependencyStagePlanInputDigestDetails{}, fmt.Errorf("%s input files require a repository checkout", dependencyStageName)
	}
	if s.RepositoryStore == nil {
		if len(keyFiles) == 0 {
			return dependencyStagePlanInputDigestDetails{}, nil
		}
		return dependencyStagePlanInputDigestDetails{}, fmt.Errorf("%s input files require repository store", dependencyStageName)
	}

	readDigests := func(repoDir string) (dependencyStagePlanInputDigestDetails, error) {
		var details dependencyStagePlanInputDigestDetails
		if changeset != nil {
			if len(keyFiles) > 0 {
				var err error
				details.KeyFilesDigest, details.ExpandedKeyFiles, err = stageKeyFilesDigestWithChangesetDetails(ctx, repoDir, changeset, keyFiles, dependencyStageName, repository.RemoteURL, repository.Submodules, s.RepositoryStore)
				if err != nil {
					return dependencyStagePlanInputDigestDetails{}, err
				}
			}
			if len(toolchainInputFiles) > 0 {
				var err error
				details.ToolchainInputsDigest, err = stageKeyFilesDigestWithChangeset(ctx, repoDir, changeset, toolchainInputFiles, dependencyStageName+" toolchain", repository.RemoteURL, repository.Submodules, s.RepositoryStore)
				if err != nil {
					return dependencyStagePlanInputDigestDetails{}, err
				}
			}
			return details, nil
		}
		if len(keyFiles) > 0 {
			var err error
			details.KeyFilesDigest, details.ExpandedKeyFiles, err = stageKeyFilesDigestAtCommitDetails(ctx, repoDir, repository.RemoteURL, repository.CommitSHA, keyFiles, dependencyStageName, repository.Submodules, s.RepositoryStore)
			if err != nil {
				return dependencyStagePlanInputDigestDetails{}, err
			}
		}
		if len(toolchainInputFiles) > 0 {
			var err error
			details.ToolchainInputsDigest, err = stageOptionalKeyFilesDigestAtCommit(ctx, repoDir, repository.CommitSHA, toolchainInputFiles, dependencyStageName+" toolchain")
			if err != nil {
				return dependencyStagePlanInputDigestDetails{}, err
			}
		}
		return details, nil
	}

	if commitBundle != nil {
		prerequisiteCommit, err := s.ensureRepositoryCommitBundlePrerequisites(ctx, repository, commitBundle)
		if err != nil {
			return dependencyStagePlanInputDigestDetails{}, err
		}
		var details dependencyStagePlanInputDigestDetails
		err = s.RepositoryStore.WithRepository(ctx, repository.RemoteURL, prerequisiteCommit, repositorystore.FetchHints{}, func(repoDir string) error {
			return commitBundle.WithRepository(ctx, repoDir, func(bundleRepoDir string) error {
				var err error
				details, err = readDigests(bundleRepoDir)
				return err
			})
		})
		if err != nil {
			return dependencyStagePlanInputDigestDetails{}, err
		}
		return details, nil
	}

	var details dependencyStagePlanInputDigestDetails
	err := s.RepositoryStore.WithRepository(ctx, repository.RemoteURL, repository.CommitSHA, repositorystore.FetchHints{}, func(repoDir string) error {
		var err error
		details, err = readDigests(repoDir)
		return err
	})
	if err != nil {
		return dependencyStagePlanInputDigestDetails{}, err
	}
	return details, nil
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
		prerequisiteCommit, err := s.ensureRepositoryCommitBundlePrerequisites(ctx, repository, commitBundle)
		if err != nil {
			return "", err
		}
		var digest string
		err = s.RepositoryStore.WithRepository(ctx, repository.RemoteURL, prerequisiteCommit, repositorystore.FetchHints{}, func(repoDir string) error {
			return commitBundle.WithRepository(ctx, repoDir, func(bundleRepoDir string) error {
				var err error
				if changeset != nil {
					digest, err = stageKeyFilesDigestWithChangeset(ctx, bundleRepoDir, changeset, files, stageName, repository.RemoteURL, repository.Submodules, s.RepositoryStore)
				} else {
					digest, err = stageKeyFilesDigestAtCommit(ctx, bundleRepoDir, repository.RemoteURL, repository.CommitSHA, files, stageName, repository.Submodules, s.RepositoryStore)
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
			digest, err = stageKeyFilesDigestWithChangeset(ctx, repoDir, changeset, files, stageName, repository.RemoteURL, repository.Submodules, s.RepositoryStore)
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
		digest, err = stageKeyFilesDigestAtCommit(ctx, repoDir, repository.RemoteURL, repository.CommitSHA, files, stageName, repository.Submodules, s.RepositoryStore)
		return err
	})
	if err != nil {
		return "", err
	}
	return digest, nil
}

func (s *Service) stageInputFilesDigest(ctx context.Context, repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset, commitBundle *repositorybundle.Bundle, files []string, stageName string) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	if repository == nil {
		return "", fmt.Errorf("%s input files require a repository checkout", stageName)
	}
	if s.RepositoryStore == nil {
		return "", fmt.Errorf("%s input files require repository store", stageName)
	}
	if commitBundle != nil {
		prerequisiteCommit, err := s.ensureRepositoryCommitBundlePrerequisites(ctx, repository, commitBundle)
		if err != nil {
			return "", err
		}
		var digest string
		err = s.RepositoryStore.WithRepository(ctx, repository.RemoteURL, prerequisiteCommit, repositorystore.FetchHints{}, func(repoDir string) error {
			return commitBundle.WithRepository(ctx, repoDir, func(bundleRepoDir string) error {
				var err error
				if changeset != nil {
					digest, err = stageInputFilesDigestWithChangeset(ctx, bundleRepoDir, changeset, files, stageName, repository.RemoteURL, repository.Submodules, s.RepositoryStore)
				} else {
					digest, err = stageInputFilesDigestAtCommit(ctx, bundleRepoDir, repository.RemoteURL, repository.CommitSHA, files, stageName, repository.Submodules, s.RepositoryStore)
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
			digest, err = stageInputFilesDigestWithChangeset(ctx, repoDir, changeset, files, stageName, repository.RemoteURL, repository.Submodules, s.RepositoryStore)
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
		digest, err = stageInputFilesDigestAtCommit(ctx, repoDir, repository.RemoteURL, repository.CommitSHA, files, stageName, repository.Submodules, s.RepositoryStore)
		return err
	})
	if err != nil {
		return "", err
	}
	return digest, nil
}

func stageKeyFilesDigestWithChangeset(ctx context.Context, repoDir string, changeset *repositorychangeset.Changeset, files []string, stageName string, parentRemoteURL string, submodules bool, store repositorystore.RepositoryStore) (string, error) {
	digest, _, err := stageKeyFilesDigestWithChangesetDetails(ctx, repoDir, changeset, files, stageName, parentRemoteURL, submodules, store)
	return digest, err
}

func stageKeyFilesDigestWithChangesetDetails(ctx context.Context, repoDir string, changeset *repositorychangeset.Changeset, files []string, stageName string, parentRemoteURL string, submodules bool, store repositorystore.RepositoryStore) (string, []string, error) {
	manifest := make([]stageKeyFileDigest, 0, len(files))
	opts := repositorychangeset.DigestPathsOptions{
		Submodules:      submodules,
		ParentRemoteURL: parentRemoteURL,
	}
	if submodules && store != nil {
		opts.EnsureSubmoduleMirror = func(ctx context.Context, url, sha string) (string, error) {
			return store.EnsureSubmoduleMirror(ctx, url, sha)
		}
	}
	digests, err := changeset.DigestPathsFromBaseWithOptions(strings.TrimSpace(repoDir), files, opts)
	if err != nil {
		return "", nil, fmt.Errorf("read %s key files from repository changeset: %w", stageName, err)
	}
	expandedFiles := make([]string, 0, len(digests))
	for _, file := range digests {
		manifest = append(manifest, stageKeyFileDigest{
			Path:    file.Path,
			SHA256:  file.SHA256,
			Deleted: file.Deleted,
		})
		expandedFiles = append(expandedFiles, file.Path)
	}
	digest, err := digestStageKeyFileManifest(manifest, stageName)
	if err != nil {
		return "", nil, err
	}
	return digest, expandedFiles, nil
}

func stageOptionalKeyFilesDigestAtCommit(ctx context.Context, repoDir, commitSHA string, files []string, stageName string) (string, error) {
	trimmedCommitSHA := strings.TrimSpace(commitSHA)
	if trimmedCommitSHA == "" {
		return "", fmt.Errorf("%s input file commit SHA is empty", stageName)
	}

	normalizedFiles := normalizeStageOptionalKeyFiles(files)
	if len(normalizedFiles) == 0 {
		return "", nil
	}

	entries, err := gitTreeEntriesForFiles(ctx, strings.TrimSpace(repoDir), trimmedCommitSHA, normalizedFiles)
	if err != nil {
		return "", fmt.Errorf("inspect %s input files: %w", stageName, err)
	}

	existingFiles := make([]string, 0, len(normalizedFiles))
	for _, file := range normalizedFiles {
		if entry, ok := entries[file]; ok {
			if entry.Mode == "160000" {
				return "", fmt.Errorf("%s input file %q is a gitlink; toolchain input files must be blobs", stageName, file)
			}
			existingFiles = append(existingFiles, file)
		}
	}

	digests := map[string]string{}
	if len(existingFiles) > 0 {
		digests, err = gitFileDigestsAtCommit(ctx, strings.TrimSpace(repoDir), trimmedCommitSHA, existingFiles)
		if err != nil {
			return "", fmt.Errorf("read %s input files: %w", stageName, err)
		}
	}

	manifest := make([]stageKeyFileDigest, 0, len(normalizedFiles))
	for _, file := range normalizedFiles {
		if _, ok := entries[file]; !ok {
			manifest = append(manifest, stageKeyFileDigest{Path: file, Deleted: true})
			continue
		}
		manifest = append(manifest, stageKeyFileDigest{
			Path:   file,
			SHA256: digests[file],
		})
	}
	return digestStageKeyFileManifest(manifest, stageName)
}

func normalizeStageOptionalKeyFiles(files []string) []string {
	seen := make(map[string]struct{}, len(files))
	out := make([]string, 0, len(files))
	for _, file := range files {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		if _, ok := seen[file]; ok {
			continue
		}
		seen[file] = struct{}{}
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}

func stageInputFilesDigestWithChangeset(ctx context.Context, repoDir string, changeset *repositorychangeset.Changeset, files []string, stageName string, parentRemoteURL string, submodules bool, store repositorystore.RepositoryStore) (string, error) {
	opts := repositorychangeset.DigestPathsOptions{
		Submodules:      submodules,
		ParentRemoteURL: parentRemoteURL,
	}
	if submodules && store != nil {
		opts.EnsureSubmoduleMirror = func(ctx context.Context, url, sha string) (string, error) {
			return store.EnsureSubmoduleMirror(ctx, url, sha)
		}
	}
	digests, err := changeset.DigestRegularFilesFromBaseWithOptions(strings.TrimSpace(repoDir), files, opts)
	if err != nil {
		return "", fmt.Errorf("read %s input files from repository changeset: %w", stageName, err)
	}
	manifest := make([]stageInputFileDigest, 0, len(digests))
	for _, file := range digests {
		manifest = append(manifest, stageInputFileDigest{
			Path:   file.Path,
			Mode:   file.Mode,
			SHA256: file.SHA256,
		})
	}
	return digestStageInputFileManifest(manifest, stageName)
}

func digestStageKeyFileManifest(manifest []stageKeyFileDigest, stageName string) (string, error) {
	payload, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal %s key file manifest: %w", stageName, err)
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func stageKeyFilesDigestAtCommit(ctx context.Context, repoDir, parentRemoteURL, commitSHA string, files []string, stageName string, submodules bool, store repositorystore.RepositoryStore) (string, error) {
	digest, _, err := stageKeyFilesDigestAtCommitDetails(ctx, repoDir, parentRemoteURL, commitSHA, files, stageName, submodules, store)
	return digest, err
}

func stageKeyFilesDigestAtCommitDetails(ctx context.Context, repoDir, parentRemoteURL, commitSHA string, files []string, stageName string, submodules bool, store repositorystore.RepositoryStore) (string, []string, error) {
	trimmedCommitSHA := strings.TrimSpace(commitSHA)
	if trimmedCommitSHA == "" {
		return "", nil, fmt.Errorf("%s key file commit SHA is empty", stageName)
	}

	var mirrorSubs []submodule.MirrorSubmodule
	if submodules && store != nil {
		var err error
		mirrorSubs, err = submodule.ListMirrorSubmodulesAtCommit(ctx, repoDir, parentRemoteURL, trimmedCommitSHA, func(ctx context.Context, url, sha string) (string, error) {
			return store.EnsureSubmoduleMirror(ctx, url, sha)
		})
		if err != nil {
			return "", nil, fmt.Errorf("load submodule mirrors: %w", err)
		}
	}

	expandedFiles, err := expandStageKeyFilesAtCommit(ctx, repoDir, trimmedCommitSHA, files, stageName, mirrorSubs)
	if err != nil {
		return "", nil, err
	}

	var parentFiles []string
	subFiles := map[string][]string{}
	for _, f := range expandedFiles {
		sm, ok := submodule.FindMirrorSubmoduleForPath(f, mirrorSubs)
		if !ok {
			parentFiles = append(parentFiles, f)
			continue
		}
		stripped := strings.TrimPrefix(f, sm.Path+"/")
		subFiles[sm.Path] = append(subFiles[sm.Path], stripped)
	}

	if len(parentFiles) > 0 {
		parentEntries, err := gitTreeEntriesForFiles(ctx, strings.TrimSpace(repoDir), trimmedCommitSHA, parentFiles)
		if err != nil {
			return "", nil, fmt.Errorf("inspect %s key files: %w", stageName, err)
		}
		for _, file := range parentFiles {
			if entry, ok := parentEntries[file]; ok && entry.Mode == "160000" {
				return "", nil, fmt.Errorf("%s key file %q is a gitlink; inputs.files must name regular files", stageName, file)
			}
		}
	}

	allDigests := make(map[string]string, len(expandedFiles))

	if len(parentFiles) > 0 {
		parentDigests, err := gitFileDigestsAtCommit(ctx, strings.TrimSpace(repoDir), trimmedCommitSHA, parentFiles)
		if err != nil {
			return "", nil, fmt.Errorf("read %s key files: %w", stageName, err)
		}
		for k, v := range parentDigests {
			allDigests[k] = v
		}
	}

	for _, sm := range mirrorSubs {
		stripped := subFiles[sm.Path]
		if len(stripped) == 0 {
			continue
		}
		subDigests, err := gitFileDigestsAtCommit(ctx, sm.MirrorDir, sm.GitlinkSHA, stripped)
		if err != nil {
			return "", nil, fmt.Errorf("read %s key files in submodule %q: %w", stageName, sm.Path, err)
		}
		for k, v := range subDigests {
			allDigests[sm.Path+"/"+k] = v
		}
	}

	manifest := make([]stageKeyFileDigest, 0, len(expandedFiles))
	for _, file := range expandedFiles {
		manifest = append(manifest, stageKeyFileDigest{
			Path:   file,
			SHA256: allDigests[file],
		})
	}
	digest, err := digestStageKeyFileManifest(manifest, stageName)
	if err != nil {
		return "", nil, err
	}
	return digest, expandedFiles, nil
}

func stageInputFilesDigestAtCommit(ctx context.Context, repoDir, parentRemoteURL, commitSHA string, files []string, stageName string, submodules bool, store repositorystore.RepositoryStore) (string, error) {
	trimmedCommitSHA := strings.TrimSpace(commitSHA)
	if trimmedCommitSHA == "" {
		return "", fmt.Errorf("%s input file commit SHA is empty", stageName)
	}

	var mirrorSubs []submodule.MirrorSubmodule
	if submodules && store != nil {
		var err error
		mirrorSubs, err = submodule.ListMirrorSubmodulesAtCommit(ctx, repoDir, parentRemoteURL, trimmedCommitSHA, func(ctx context.Context, url, sha string) (string, error) {
			return store.EnsureSubmoduleMirror(ctx, url, sha)
		})
		if err != nil {
			return "", fmt.Errorf("load submodule mirrors: %w", err)
		}
	}

	expandedFiles, err := expandStageKeyFilesAtCommit(ctx, repoDir, trimmedCommitSHA, files, stageName, mirrorSubs)
	if err != nil {
		return "", err
	}

	var parentFiles []string
	subFiles := map[string][]string{}
	for _, f := range expandedFiles {
		sm, ok := submodule.FindMirrorSubmoduleForPath(f, mirrorSubs)
		if !ok {
			parentFiles = append(parentFiles, f)
			continue
		}
		stripped := strings.TrimPrefix(f, sm.Path+"/")
		subFiles[sm.Path] = append(subFiles[sm.Path], stripped)
	}

	entryByFile := make(map[string]gitTreeEntry, len(expandedFiles))

	if len(parentFiles) > 0 {
		treeEntries, err := gitTreeEntriesForFiles(ctx, strings.TrimSpace(repoDir), trimmedCommitSHA, parentFiles)
		if err != nil {
			return "", fmt.Errorf("inspect %s input files: %w", stageName, err)
		}
		for _, file := range parentFiles {
			entry, ok := treeEntries[file]
			if !ok {
				return "", fmt.Errorf("%s input file %q does not exist", stageName, file)
			}
			if !isRegularGitTreeFile(entry) {
				return "", fmt.Errorf("%s input file %q is %s; inputs.files must name regular files", stageName, file, gitTreeEntryKind(entry))
			}
			entryByFile[file] = entry
		}
	}

	for _, sm := range mirrorSubs {
		stripped := subFiles[sm.Path]
		if len(stripped) == 0 {
			continue
		}
		smEntries, err := gitTreeEntriesForFiles(ctx, sm.MirrorDir, sm.GitlinkSHA, stripped)
		if err != nil {
			return "", fmt.Errorf("inspect %s input files in submodule %q: %w", stageName, sm.Path, err)
		}
		for _, s := range stripped {
			entry, ok := smEntries[s]
			if !ok {
				return "", fmt.Errorf("%s input file %q does not exist", stageName, sm.Path+"/"+s)
			}
			if !isRegularGitTreeFile(entry) {
				return "", fmt.Errorf("%s input file %q is %s; inputs.files must name regular files", stageName, sm.Path+"/"+s, gitTreeEntryKind(entry))
			}
			entryByFile[sm.Path+"/"+s] = entry
		}
	}

	allDigests := make(map[string]string, len(expandedFiles))

	if len(parentFiles) > 0 {
		parentDigests, err := gitFileDigestsAtCommit(ctx, strings.TrimSpace(repoDir), trimmedCommitSHA, parentFiles)
		if err != nil {
			return "", fmt.Errorf("read %s input files: %w", stageName, err)
		}
		for k, v := range parentDigests {
			allDigests[k] = v
		}
	}

	for _, sm := range mirrorSubs {
		stripped := subFiles[sm.Path]
		if len(stripped) == 0 {
			continue
		}
		subDigests, err := gitFileDigestsAtCommit(ctx, sm.MirrorDir, sm.GitlinkSHA, stripped)
		if err != nil {
			return "", fmt.Errorf("read %s input files in submodule %q: %w", stageName, sm.Path, err)
		}
		for k, v := range subDigests {
			allDigests[sm.Path+"/"+k] = v
		}
	}

	manifest := make([]stageInputFileDigest, 0, len(expandedFiles))
	for _, file := range expandedFiles {
		entry := entryByFile[file]
		manifest = append(manifest, stageInputFileDigest{
			Path:   file,
			Mode:   entry.Mode,
			SHA256: allDigests[file],
		})
	}
	return digestStageInputFileManifest(manifest, stageName)
}

func digestStageInputFileManifest(manifest []stageInputFileDigest, stageName string) (string, error) {
	payload, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal %s input file manifest: %w", stageName, err)
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func expandStageKeyFilesAtCommit(ctx context.Context, repoDir, commitSHA string, files []string, stageName string, mirrorSubs []submodule.MirrorSubmodule) ([]string, error) {
	seen := make(map[string]struct{}, len(files))
	var expanded []string
	var candidates []string
	for _, file := range files {
		file = strings.TrimSpace(file)
		if !strings.ContainsAny(file, "*?[") {
			if _, ok := seen[file]; ok {
				continue
			}
			seen[file] = struct{}{}
			expanded = append(expanded, file)
			continue
		}
		if !doublestar.ValidatePattern(file) {
			return nil, fmt.Errorf("%s key file glob %q is invalid: %w", stageName, file, path.ErrBadPattern)
		}
		if candidates == nil {
			parentFiles, err := gitFilesAtCommit(ctx, repoDir, commitSHA)
			if err != nil {
				return nil, fmt.Errorf("list %s key files at commit: %w", stageName, err)
			}
			gitlinkPaths := make(map[string]struct{}, len(mirrorSubs))
			for _, sm := range mirrorSubs {
				gitlinkPaths[sm.Path] = struct{}{}
			}
			candidates = make([]string, 0, len(parentFiles))
			for _, f := range parentFiles {
				if _, isGitlink := gitlinkPaths[f]; isGitlink {
					continue
				}
				candidates = append(candidates, f)
			}
			for _, sm := range mirrorSubs {
				smFiles, err := submodule.ListMirrorSubmoduleFilesAtSHA(ctx, sm)
				if err != nil {
					return nil, fmt.Errorf("list submodule %q files: %w", sm.Path, err)
				}
				candidates = append(candidates, smFiles...)
			}
			sort.Strings(candidates)
		}
		matches := 0
		for _, candidate := range candidates {
			matched, err := doublestar.Match(file, candidate)
			if err != nil {
				return nil, fmt.Errorf("%s key file glob %q is invalid: %w", stageName, file, err)
			}
			if !matched {
				continue
			}
			matches++
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			expanded = append(expanded, candidate)
		}
		if matches == 0 {
			if len(mirrorSubs) == 0 {
				if gitlinkPaths, glErr := gitGitlinkPathsAtCommit(ctx, repoDir, commitSHA); glErr == nil {
					for _, glPath := range gitlinkPaths {
						if strings.HasPrefix(file, glPath+"/") {
							return nil, fmt.Errorf("%s key file glob %q is inside submodule %q; enable repository.submodules to digest submodule contents", stageName, file, glPath)
						}
					}
				}
			}
			return nil, fmt.Errorf("%s key file glob %q matched no files", stageName, file)
		}
	}
	sort.Strings(expanded)
	return expanded, nil
}

func gitFilesAtCommit(ctx context.Context, repoDir, commitSHA string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "ls-tree", "-r", "--name-only", "-z", commitSHA)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("%s", message)
	}
	parts := bytes.Split(output, []byte{0})
	files := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		files = append(files, string(part))
	}
	sort.Strings(files)
	return files, nil
}

func gitGitlinkPathsAtCommit(ctx context.Context, repoDir, commitSHA string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "ls-tree", "-r", "-z", commitSHA)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("%s", message)
	}
	var paths []string
	for _, raw := range bytes.Split(output, []byte{0}) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		metadata, rawPath, ok := bytes.Cut(raw, []byte{'\t'})
		if !ok {
			continue
		}
		fields := strings.Fields(string(metadata))
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "160000" {
			paths = append(paths, string(rawPath))
		}
	}
	return paths, nil
}

type gitTreeEntry struct {
	Mode string
	Type string
}

func isRegularGitTreeFile(entry gitTreeEntry) bool {
	return strings.TrimSpace(entry.Type) == "blob" && (entry.Mode == "100644" || entry.Mode == "100755")
}

func gitTreeEntryKind(entry gitTreeEntry) string {
	switch strings.TrimSpace(entry.Mode) {
	case "040000":
		return "a directory"
	case "120000":
		return "a symlink"
	case "160000":
		return "a gitlink"
	default:
		return "not a regular file"
	}
}

func (s *Service) lookupDependencyStageCache(ctx context.Context, backendName string, compiled *policy.CompiledPolicy, repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset, plan dependencyStagePlan) (cachestore.Record, bool, string, error) {
	if compiled == nil || repository == nil || strings.TrimSpace(plan.CacheKey) == "" {
		return cachestore.Record{}, false, "", nil
	}
	store, err := s.cacheStoreOrErr()
	if err != nil {
		return cachestore.Record{}, false, "", nil
	}

	record, ok, err := lookupReadyCacheRecord(ctx, store, dependencyStageName, plan.CacheKey)
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

	record, ok, err := lookupReadyCacheRecord(ctx, store, dependencyStageName, plan.PortableCacheKey)
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
	plan dependencyStagePlan,
	record cachestore.Record,
	cacheOutputVolumes []backend.CacheOutputVolumeSpec,
	reporter CreateSandboxReporter,
) (*cleanroomv1.CreateSandboxResponse, error) {
	restoreReq := &cleanroomv1.CreateSandboxRequest{
		Backend: backendName,
		Options: options,
	}
	restoreResp, err := s.createSandboxFromCacheRecord(ctx, restoreReq, compiled, record, cacheOutputVolumes, reporter)
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
	if err := s.validatePortableDependencyStageKeyFiles(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, plan, record.DependencyKeyFilesDigest); err != nil {
		if cleanupErr := s.terminateCreatedSandbox(context.Background(), adapter, sandboxID); cleanupErr != nil {
			return nil, fmt.Errorf("validate portable dependency stage key files: %w; cleanup failed: %v", err, cleanupErr)
		}
		return nil, fmt.Errorf("validate portable dependency stage key files: %w", err)
	}
	if err := s.validatePortableDependencyStageToolchainInputs(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, plan); err != nil {
		if cleanupErr := s.terminateCreatedSandbox(context.Background(), adapter, sandboxID); cleanupErr != nil {
			return nil, fmt.Errorf("validate portable dependency stage toolchain inputs: %w; cleanup failed: %v", err, cleanupErr)
		}
		return nil, fmt.Errorf("validate portable dependency stage toolchain inputs: %w", err)
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

func (s *Service) retainRestoredSandboxRepositoryState(resp *cleanroomv1.CreateSandboxResponse, repository *repositorycheckout.Checkout, commitBundle *repositorybundle.Bundle, changeset *repositorychangeset.Changeset) {
	if resp == nil || resp.GetSandbox() == nil {
		return
	}
	if sandbox := s.markRestoredSandboxRepositoryReady(resp.GetSandbox().GetSandboxId(), repository, commitBundle, changeset != nil); sandbox != nil {
		resp.Sandbox = sandbox
	}
}

func (s *Service) validatePortableDependencyStageKeyFiles(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	plan dependencyStagePlan,
	expectedDigest string,
) error {
	expectedDigest = strings.TrimSpace(expectedDigest)
	if expectedDigest == "" {
		return fmt.Errorf("portable dependency stage is missing dependency key-file digest")
	}
	files := plan.ExpandedKeyFiles
	if len(files) == 0 {
		files = compiled.Dependencies.KeyFiles
	}
	return s.validatePortableDependencyStageFileDigest(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, files, expectedDigest, "portable dependency key-file")
}

func (s *Service) validatePortableDependencyStageToolchainInputs(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	plan dependencyStagePlan,
) error {
	expectedDigest := strings.TrimSpace(plan.ToolchainInputsDigest)
	files := normalizeStageOptionalKeyFiles(plan.ToolchainInputFiles)
	if expectedDigest == "" || len(files) == 0 {
		return nil
	}
	return s.validatePortableDependencyStageFileDigest(ctx, adapter, sandboxID, compiled, firecrackerCfg, repository, files, expectedDigest, "portable dependency toolchain input")
}

func (s *Service) validatePortableDependencyStageFileDigest(
	ctx context.Context,
	adapter backend.Adapter,
	sandboxID string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	files []string,
	expectedDigest string,
	label string,
) error {
	command, err := dependencyStageFileDigestCommand(repository, files, label+" validation")
	if err != nil {
		return err
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	result, err := s.runPersistentSandboxCommand(ctx, adapter, sandboxID, compiled, firecrackerCfg, policy.NetworkStageExecution, s.ids().NewExecutionID(), command, nil, backend.OutputStream{
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
		return fmt.Errorf("%s validation returned no result", label)
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message == "" {
			message = fmt.Sprintf("%s validation failed with exit code %d", label, result.ExitCode)
		}
		return fmt.Errorf("%s", message)
	}
	actualDigest := strings.TrimSpace(stdout.String())
	if actualDigest != expectedDigest {
		return fmt.Errorf("%s digest mismatch: expected %s got %s", label, expectedDigest, actualDigest)
	}
	return nil
}

func dependencyStageKeyFilesDigestCommand(repository *repositorycheckout.Checkout, files []string) ([]string, error) {
	return dependencyStageFileDigestCommand(repository, files, "portable dependency key-file validation")
}

func dependencyStageFileDigestCommand(repository *repositorycheckout.Checkout, files []string, label string) ([]string, error) {
	if repository == nil {
		return nil, fmt.Errorf("%s requires a repository checkout", label)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s requires input files", label)
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

	now := s.clock().Now()
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
		CreatedAt:                now,
		LastUsedAt:               now,
		ProducerVersion:          dependencyStageProducerVersion,
	}
	populateStageCacheRecordMetadata(&record, plan.ParentRuntimeCacheKey, result, now)
	stampCacheRecordOwner(ctx, &record)

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
		policy.NetworkStageDependencies,
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
