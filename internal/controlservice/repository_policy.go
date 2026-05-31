package controlservice

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorybundle"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/repositorystore"
)

func (s *Service) resolveCreateSandboxPolicy(ctx context.Context, pb *cleanroomv1.Policy, repository *repositorycheckout.Checkout, commitBundle *repositorybundle.Bundle) (*policy.CompiledPolicy, string, *repositorycheckout.Checkout, *repositorycheckout.Checkout, error) {
	if pb != nil {
		compiled, err := policy.FromCreateRequestProto(pb)
		if err != nil {
			return nil, "", nil, nil, fmt.Errorf("invalid policy: %w", err)
		}
		return compiled, "", repository, repository, nil
	}
	if repository == nil {
		return nil, "", nil, nil, errors.New("missing policy")
	}
	return s.resolveRepositoryCreateSandboxPolicy(ctx, repository, commitBundle)
}

func (s *Service) resolveRepositoryCreateSandboxPolicy(ctx context.Context, repository *repositorycheckout.Checkout, commitBundle *repositorybundle.Bundle) (*policy.CompiledPolicy, string, *repositorycheckout.Checkout, *repositorycheckout.Checkout, error) {
	if s.RepositoryStore == nil {
		return nil, "", nil, nil, errors.New("repository checkout without policy requires repository store")
	}
	resolvedRepository := cloneRepositoryCheckout(repository)
	if _, err := resolvedRepository.NormalizeRemoteURL(); err != nil {
		return nil, "", nil, nil, err
	}
	if commitBundle != nil && strings.TrimSpace(resolvedRepository.CommitSHA) == "" {
		resolvedRepository.CommitSHA = strings.ToLower(strings.TrimSpace(commitBundle.TargetCommitSHA))
	}
	if err := s.resolveRepositoryCheckoutCommit(ctx, resolvedRepository, commitBundle); err != nil {
		return nil, "", nil, nil, err
	}
	if err := validateRepositoryCommitBundleForCheckout(resolvedRepository, commitBundle); err != nil {
		return nil, "", nil, nil, err
	}

	compiled, source, repositoryConfig, err := s.loadRepositoryPolicy(ctx, resolvedRepository, commitBundle)
	if err != nil {
		return nil, "", nil, nil, err
	}
	authorizationRepository := cloneRepositoryCheckout(resolvedRepository)
	if repositoryConfig.Enabled() {
		if strings.TrimSpace(repositoryConfig.Path) != "" {
			resolvedRepository.DestinationDir = repositoryConfig.Path
		}
		resolvedRepository.Submodules = repositoryConfig.Submodules
	} else {
		resolvedRepository = nil
	}
	return compiled, "repository:" + source, resolvedRepository, authorizationRepository, nil
}

func (s *Service) resolveRepositoryCheckoutCommit(ctx context.Context, repository *repositorycheckout.Checkout, commitBundle *repositorybundle.Bundle) error {
	if commitSHA, ok := normalizeRepositoryCommitSHA(repository.CommitSHA); ok {
		repository.CommitSHA = commitSHA
		branch := strings.TrimSpace(repository.Branch)
		if branch == "" {
			return nil
		}
		branchValidationCommits, err := repositoryBranchValidationCommits(commitSHA, commitBundle)
		if err != nil {
			return err
		}
		hints := repositorystore.FetchHints{Branches: []string{branch}}
		if err := s.RepositoryStore.Refresh(ctx, repository.RemoteURL, hints); err != nil {
			return err
		}
		for _, branchValidationCommit := range branchValidationCommits {
			if err := s.RepositoryStore.EnsureCommit(ctx, repository.RemoteURL, branchValidationCommit, hints); err != nil {
				return err
			}
		}
		return s.RepositoryStore.WithRepository(ctx, repository.RemoteURL, branchValidationCommits[0], hints, func(repoDir string) error {
			if err := validateRepositoryBranchContainsAnyCommit(ctx, repoDir, branch, branchValidationCommits); err != nil {
				return err
			}
			repository.Branch = branch
			return nil
		})
	}
	if strings.TrimSpace(repository.CommitSHA) != "" {
		return fmt.Errorf("repository commit_sha %q must be a full 40-character hexadecimal commit SHA", strings.TrimSpace(repository.CommitSHA))
	}
	branch := strings.TrimSpace(repository.Branch)
	hints := repositorystore.FetchHints{}
	if branch != "" {
		hints.Branches = []string{branch}
	}
	if err := s.RepositoryStore.Refresh(ctx, repository.RemoteURL, hints); err != nil {
		return err
	}
	return s.RepositoryStore.WithRepository(ctx, repository.RemoteURL, "", hints, func(repoDir string) error {
		if branch != "" {
			if err := validateRepositoryBranch(ctx, repoDir, branch); err != nil {
				return err
			}
			commitSHA, err := gitOutputContext(ctx, repoDir, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
			if err != nil {
				return fmt.Errorf("resolve repository branch %q: %w", branch, err)
			}
			repository.CommitSHA = commitSHA
			repository.Branch = branch
			return nil
		}

		if headBranch, err := gitOutputContext(ctx, repoDir, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
			repository.Branch = strings.TrimSpace(headBranch)
		}
		commitSHA, err := gitOutputContext(ctx, repoDir, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil {
			return fmt.Errorf("resolve repository HEAD: %w", err)
		}
		repository.CommitSHA = commitSHA
		return nil
	})
}

func repositoryBranchValidationCommits(commitSHA string, commitBundle *repositorybundle.Bundle) ([]string, error) {
	commitSHA = strings.ToLower(strings.TrimSpace(commitSHA))
	if commitBundle == nil || !strings.EqualFold(strings.TrimSpace(commitBundle.TargetCommitSHA), commitSHA) {
		return []string{commitSHA}, nil
	}
	prerequisites, err := commitBundle.PrerequisiteCommits()
	if err != nil {
		return nil, err
	}
	if len(prerequisites) == 0 {
		return []string{commitSHA}, nil
	}
	return prerequisites, nil
}

func (s *Service) loadRepositoryPolicy(ctx context.Context, repository *repositorycheckout.Checkout, commitBundle *repositorybundle.Bundle) (*policy.CompiledPolicy, string, policy.RepositoryConfig, error) {
	if commitBundle != nil {
		return s.loadRepositoryPolicyFromCommitBundle(ctx, repository, commitBundle)
	}
	return s.loadRepositoryPolicyAtCommit(ctx, repository)
}

func (s *Service) loadRepositoryPolicyAtCommit(ctx context.Context, repository *repositorycheckout.Checkout) (*policy.CompiledPolicy, string, policy.RepositoryConfig, error) {
	return loadRepositoryPolicyFiles(func(path string) ([]byte, error) {
		return s.RepositoryStore.ReadFileAtCommit(ctx, repository.RemoteURL, repository.CommitSHA, path)
	}, repositoryPolicyNotFoundError(repository))
}

func (s *Service) loadRepositoryPolicyFromCommitBundle(ctx context.Context, repository *repositorycheckout.Checkout, commitBundle *repositorybundle.Bundle) (*policy.CompiledPolicy, string, policy.RepositoryConfig, error) {
	prerequisiteCommit, err := s.ensureRepositoryCommitBundlePrerequisites(ctx, repository, commitBundle)
	if err != nil {
		return nil, "", policy.RepositoryConfig{}, err
	}

	var (
		compiled         *policy.CompiledPolicy
		source           string
		repositoryConfig policy.RepositoryConfig
	)
	err = s.RepositoryStore.WithRepository(ctx, repository.RemoteURL, prerequisiteCommit, repositorystore.FetchHints{}, func(repoDir string) error {
		return commitBundle.WithRepository(ctx, repoDir, func(bundleRepoDir string) error {
			var err error
			compiled, source, repositoryConfig, err = loadRepositoryPolicyFiles(func(path string) ([]byte, error) {
				return gitShowFileAtCommit(ctx, bundleRepoDir, commitBundle.TargetCommitSHA, path)
			}, repositoryPolicyNotFoundError(repository))
			return err
		})
	})
	if err != nil {
		return nil, "", policy.RepositoryConfig{}, err
	}
	return compiled, source, repositoryConfig, nil
}

func loadRepositoryPolicyFiles(readFile func(path string) ([]byte, error), notFound error) (*policy.CompiledPolicy, string, policy.RepositoryConfig, error) {
	for _, path := range []string{policy.PrimaryPolicyPath, policy.FallbackPolicyPath} {
		content, err := readFile(path)
		if err != nil {
			if isRepositoryFileMissingError(err) {
				continue
			}
			return nil, "", policy.RepositoryConfig{}, fmt.Errorf("read repository policy %s: %w", path, err)
		}
		compiled, repositoryConfig, err := policy.CompileBytesWithRepositoryConfig(content, path)
		if err != nil {
			return nil, path, policy.RepositoryConfig{}, err
		}
		return compiled, path, repositoryConfig, nil
	}
	return nil, "", policy.RepositoryConfig{}, notFound
}

func repositoryPolicyNotFoundError(repository *repositorycheckout.Checkout) error {
	return fmt.Errorf("%w: expected %s or %s in repository %s at %s", policy.ErrPolicyNotFound, policy.PrimaryPolicyPath, policy.FallbackPolicyPath, repository.RemoteURL, repository.CommitSHA)
}

func normalizeRepositoryCommitSHA(revision string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(revision))
	if len(normalized) != 40 {
		return "", false
	}
	if _, err := hex.DecodeString(normalized); err != nil {
		return "", false
	}
	return normalized, true
}

func validateRepositoryBranch(ctx context.Context, repoDir, branch string) error {
	if _, err := gitOutputContext(ctx, repoDir, "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("invalid repository branch %q: %w", branch, err)
	}
	return nil
}

func validateRepositoryBranchContainsCommit(ctx context.Context, repoDir, branch, commitSHA string) error {
	return validateRepositoryBranchContainsAnyCommit(ctx, repoDir, branch, []string{commitSHA})
}

func validateRepositoryBranchContainsAnyCommit(ctx context.Context, repoDir, branch string, commitSHAs []string) error {
	if err := validateRepositoryBranch(ctx, repoDir, branch); err != nil {
		return err
	}
	branchCommit, err := gitOutputContext(ctx, repoDir, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve repository branch %q: %w", branch, err)
	}
	for _, commitSHA := range commitSHAs {
		if _, err := gitOutputContext(ctx, repoDir, "merge-base", "--is-ancestor", commitSHA, branchCommit); err == nil {
			return nil
		}
	}
	return fmt.Errorf("repository branch %q does not contain any candidate commit", branch)
}

func gitShowFileAtCommit(ctx context.Context, repoDir, commitSHA, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "show", strings.TrimSpace(commitSHA)+":"+path)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		if isGitShowFileMissingError(message, path) {
			return nil, repositorystore.NewFileNotFoundError(commitSHA, path)
		}
		return nil, fmt.Errorf("git show %s:%s: %s", strings.TrimSpace(commitSHA), path, message)
	}
	return output, nil
}

func gitOutputContext(ctx context.Context, repoDir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoDir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return "", errors.New(message)
	}
	return strings.TrimSpace(string(output)), nil
}

func isRepositoryFileMissingError(err error) bool {
	return errors.Is(err, repositorystore.ErrFileNotFound)
}

func isGitShowFileMissingError(message, path string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	path = strings.ToLower(strings.TrimSpace(path))
	if path == "" {
		return false
	}
	quotedPath := "'" + path + "'"
	return strings.Contains(message, "path "+quotedPath+" does not exist") ||
		strings.Contains(message, "path "+quotedPath+" exists on disk, but not in") ||
		strings.Contains(message, "pathspec "+quotedPath)
}
