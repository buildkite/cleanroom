package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

type resolvedRepositoryCheckout struct {
	RootDir        string
	RemoteURL      string
	CommitSHA      string
	DestinationDir string
	Submodules     bool
	Branch         string
	Dirty          bool
}

const defaultRepositoryOverridePath = "/workspace"

type repositoryOverrideFlags struct {
	RepoURL    string `name:"repo-url" help:"Bootstrap this repository URL instead of inheriting the current repository"`
	RepoCommit string `name:"repo-commit" help:"Bootstrap this exact repository commit SHA; requires --repo-url"`
}

func (f repositoryOverrideFlags) resolve(cwd string, loader policyLoader) (*resolvedRepositoryCheckout, error) {
	repoURL := strings.TrimSpace(f.RepoURL)
	repoCommit := strings.TrimSpace(f.RepoCommit)
	switch {
	case repoURL == "" && repoCommit == "":
		return nil, nil
	case repoURL == "" || repoCommit == "":
		return nil, errors.New("--repo-url and --repo-commit must be used together")
	}

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      repoURL,
		CommitSHA:      repoCommit,
		DestinationDir: defaultRepositoryOverridePath,
	}
	if err := checkout.ValidateBootstrap(); err != nil {
		return nil, err
	}
	remoteHost, err := checkout.NormalizeRemoteURL()
	if err != nil {
		return nil, err
	}

	if loader != nil {
		compiled, _, err := loader.LoadAndCompile(cwd)
		if err != nil {
			return nil, err
		}
		if compiled != nil && !compiled.Allows(remoteHost, 443) {
			return nil, fmt.Errorf("repository remote host %q is not allowed by sandbox policy", remoteHost)
		}
	}

	return &resolvedRepositoryCheckout{
		RemoteURL:      checkout.RemoteURL,
		CommitSHA:      checkout.CommitSHA,
		DestinationDir: checkout.DestinationDir,
	}, nil
}

func (f repositoryOverrideFlags) hasRepositoryOverride() bool {
	return strings.TrimSpace(f.RepoURL) != "" || strings.TrimSpace(f.RepoCommit) != ""
}

func resolveRepositoryCheckout(cwd string, loader policyLoader) (*resolvedRepositoryCheckout, error) {
	return resolveRepositoryCheckoutWithOverride(cwd, loader, repositoryOverrideFlags{})
}

func resolveRepositoryCheckoutWithOverride(cwd string, loader policyLoader, override repositoryOverrideFlags) (*resolvedRepositoryCheckout, error) {
	if checkout, err := override.resolve(cwd, loader); err != nil || checkout != nil {
		return checkout, err
	}
	repository, err := loadRepositoryConfig(cwd, loader)
	if err != nil {
		if errors.Is(err, errSkipRepositoryCheckout) {
			return nil, nil
		}
		return nil, err
	}
	repoRoot, err := resolveRepositoryRoot(cwd, repository)
	if err != nil {
		if errors.Is(err, errSkipRepositoryCheckout) {
			return nil, nil
		}
		return nil, err
	}
	dirty, err := gitOutput(repoRoot, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("inspect repository status: %w", err)
	}

	commitSHA, err := gitOutput(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve repository HEAD: %w", err)
	}
	branch, err := gitOutput(repoRoot, "branch", "--show-current")
	if err != nil {
		return nil, fmt.Errorf("resolve repository branch: %w", err)
	}
	remoteURL, err := gitOutput(repoRoot, "remote", "get-url", repository.Remote)
	if err != nil {
		return nil, fmt.Errorf("resolve repository remote %q: %w", repository.Remote, err)
	}
	canonicalURL, remoteHost, err := repositorycheckout.CanonicalizeRemoteURL(remoteURL)
	if err != nil {
		return nil, err
	}

	compiled, _, err := loader.LoadAndCompile(cwd)
	if err != nil {
		return nil, err
	}
	if compiled != nil && !compiled.Allows(remoteHost, 443) {
		return nil, fmt.Errorf("repository remote host %q is not allowed by sandbox policy", remoteHost)
	}

	return &resolvedRepositoryCheckout{
		RootDir:        repoRoot,
		RemoteURL:      canonicalURL,
		CommitSHA:      strings.TrimSpace(commitSHA),
		DestinationDir: repository.Path,
		Submodules:     repository.Submodules,
		Branch:         strings.TrimSpace(branch),
		Dirty:          strings.TrimSpace(dirty) != "",
	}, nil
}

var errSkipRepositoryCheckout = errors.New("skip repository checkout")

func loadRepositoryConfig(cwd string, loader policyLoader) (policy.RepositoryConfig, error) {
	if loader == nil {
		return policy.RepositoryConfig{}, errSkipRepositoryCheckout
	}

	repository, _, err := loader.LoadRepository(cwd)
	if err != nil {
		if errors.Is(err, policy.ErrPolicyNotFound) {
			return policy.RepositoryConfig{}, errSkipRepositoryCheckout
		}
		return policy.RepositoryConfig{}, err
	}
	if !repository.Enabled() {
		return policy.RepositoryConfig{}, errSkipRepositoryCheckout
	}
	switch repository.Mode {
	case "current-repo":
		return repository, nil
	default:
		return policy.RepositoryConfig{}, fmt.Errorf("unsupported repository.mode %q", repository.Mode)
	}
}

func resolveRepositoryRoot(cwd string, repository policy.RepositoryConfig) (string, error) {
	repoRoot, err := gitOutput(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		if shouldSkipImplicitRepository(repository, err) {
			return "", errSkipRepositoryCheckout
		}
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	return repoRoot, nil
}

func shouldSkipImplicitRepository(repository policy.RepositoryConfig, err error) bool {
	if !repository.Implicit || err == nil {
		return false
	}
	return isNotAGitRepositoryErr(err)
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return strings.TrimSpace(string(out)), nil
}

func isNotAGitRepositoryErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "not a git repository")
}

func repositoryCheckoutProto(repository *resolvedRepositoryCheckout) *cleanroomv1.RepositoryCheckout {
	return toRepositoryCheckout(repository).ToProto()
}

func toRepositoryCheckout(repository *resolvedRepositoryCheckout) *repositorycheckout.Checkout {
	if repository == nil {
		return nil
	}
	return &repositorycheckout.Checkout{
		RemoteURL:      repository.RemoteURL,
		CommitSHA:      repository.CommitSHA,
		DestinationDir: repository.DestinationDir,
		Submodules:     repository.Submodules,
		Branch:         repository.Branch,
	}
}

func warnDirtyRepositoryCheckout(repository *resolvedRepositoryCheckout, includingLocalChanges bool) {
	if repository == nil || !repository.Dirty || includingLocalChanges {
		return
	}
	_, _ = fmt.Fprint(
		os.Stderr,
		renderNoticeLine(
			"warning",
			fmt.Sprintf("repository has local modifications; sandbox will use HEAD %s and ignore local changes", repository.CommitSHA),
			defaultTerminalPalette().warn,
			shouldUseANSI(os.Stderr),
		),
	)
}
