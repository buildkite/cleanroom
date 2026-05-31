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
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/repositorystore"
)

func (s *Service) resolveCreateSandboxPolicy(ctx context.Context, pb *cleanroomv1.Policy, repository *repositorycheckout.Checkout) (*policy.CompiledPolicy, string, *repositorycheckout.Checkout, error) {
	if pb != nil {
		compiled, err := policy.FromCreateRequestProto(pb)
		if err != nil {
			return nil, "", nil, fmt.Errorf("invalid policy: %w", err)
		}
		return compiled, "", repository, nil
	}
	if repository == nil {
		return nil, "", nil, errors.New("missing policy")
	}
	return s.resolveRepositoryCreateSandboxPolicy(ctx, repository)
}

func (s *Service) resolveRepositoryCreateSandboxPolicy(ctx context.Context, repository *repositorycheckout.Checkout) (*policy.CompiledPolicy, string, *repositorycheckout.Checkout, error) {
	if s.RepositoryStore == nil {
		return nil, "", nil, errors.New("repository checkout without policy requires repository store")
	}
	resolvedRepository := cloneRepositoryCheckout(repository)
	if _, err := resolvedRepository.NormalizeRemoteURL(); err != nil {
		return nil, "", nil, err
	}
	if err := s.resolveRepositoryCheckoutCommit(ctx, resolvedRepository); err != nil {
		return nil, "", nil, err
	}

	compiled, source, repositoryConfig, err := s.loadRepositoryPolicyAtCommit(ctx, resolvedRepository)
	if err != nil {
		return nil, "", nil, err
	}
	if repositoryConfig.Enabled() {
		if strings.TrimSpace(repositoryConfig.Path) != "" {
			resolvedRepository.DestinationDir = repositoryConfig.Path
		}
		resolvedRepository.Submodules = repositoryConfig.Submodules
	} else {
		resolvedRepository = nil
	}
	return compiled, "repository:" + source, resolvedRepository, nil
}

func (s *Service) resolveRepositoryCheckoutCommit(ctx context.Context, repository *repositorycheckout.Checkout) error {
	if commitSHA, ok := normalizeRepositoryCommitSHA(repository.CommitSHA); ok {
		repository.CommitSHA = commitSHA
		return nil
	}
	if strings.TrimSpace(repository.CommitSHA) != "" {
		return fmt.Errorf("repository commit_sha %q must be a full 40-character hexadecimal commit SHA", strings.TrimSpace(repository.CommitSHA))
	}
	return s.RepositoryStore.WithRepository(ctx, repository.RemoteURL, "", repositorystore.FetchHints{}, func(repoDir string) error {
		branch := strings.TrimSpace(repository.Branch)
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

func (s *Service) loadRepositoryPolicyAtCommit(ctx context.Context, repository *repositorycheckout.Checkout) (*policy.CompiledPolicy, string, policy.RepositoryConfig, error) {
	for _, path := range []string{policy.PrimaryPolicyPath, policy.FallbackPolicyPath} {
		content, err := s.RepositoryStore.ReadFileAtCommit(ctx, repository.RemoteURL, repository.CommitSHA, path)
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
	return nil, "", policy.RepositoryConfig{}, fmt.Errorf("%w: expected %s or %s in repository %s at %s", policy.ErrPolicyNotFound, policy.PrimaryPolicyPath, policy.FallbackPolicyPath, repository.RemoteURL, repository.CommitSHA)
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
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "does not exist") ||
		strings.Contains(message, "exists on disk, but not in") ||
		strings.Contains(message, "pathspec")
}
