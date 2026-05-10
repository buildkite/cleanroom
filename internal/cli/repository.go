package cli

import (
	"encoding/hex"
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
	RemoteName     string
	RemoteURL      string
	CommitSHA      string
	DestinationDir string
	Submodules     bool
	Branch         string
	Dirty          bool
}

const defaultRepositoryOverridePath = "/workspace"
const defaultRepositoryOverrideRevision = "latest"

type repositoryOverrideFlags struct {
	RepoURL    string `name:"repo-url" help:"Bootstrap this repository URL instead of inheriting the current repository"`
	RepoCommit string `name:"repo-commit" help:"Bootstrap this repository commit SHA, tag, or latest; defaults to latest when --repo-url is set"`
}

func (f repositoryOverrideFlags) resolve(cwd string, loader policyLoader) (*resolvedRepositoryCheckout, error) {
	return f.resolveWithCommitResolver(cwd, loader, resolveRepositoryOverrideCommit)
}

type repositoryOverrideCommitResolver func(remoteURL, revision string) (string, error)

func (f repositoryOverrideFlags) resolveWithCommitResolver(cwd string, loader policyLoader, resolveCommit repositoryOverrideCommitResolver) (*resolvedRepositoryCheckout, error) {
	repoURL := strings.TrimSpace(f.RepoURL)
	if err := f.validate(); err != nil {
		return nil, err
	}
	if repoURL == "" {
		return nil, nil
	}
	repoCommit := f.revision()

	checkout := &repositorycheckout.Checkout{
		RemoteURL:      repoURL,
		DestinationDir: defaultRepositoryOverridePath,
	}
	remoteHost, err := checkout.NormalizeRemoteURL()
	if err != nil {
		return nil, err
	}
	if err := checkout.ValidateWorkdir(); err != nil {
		return nil, err
	}

	if loader != nil {
		compiled, _, err := loader.LoadAndCompile(cwd)
		if err != nil {
			return nil, err
		}
		if compiled != nil && !compiled.AllowsForStage(policy.NetworkStageWorkspace, remoteHost, 443) {
			return nil, fmt.Errorf("repository remote host %q is not allowed by workspace network policy", remoteHost)
		}
	}

	resolvedCommit, err := resolveCommit(checkout.RemoteURL, repoCommit)
	if err != nil {
		return nil, err
	}
	checkout.CommitSHA = resolvedCommit
	if err := checkout.ValidateBootstrap(); err != nil {
		return nil, err
	}

	return &resolvedRepositoryCheckout{
		RemoteURL:      checkout.RemoteURL,
		CommitSHA:      checkout.CommitSHA,
		DestinationDir: checkout.DestinationDir,
	}, nil
}

func (f repositoryOverrideFlags) validate() error {
	repoURL := strings.TrimSpace(f.RepoURL)
	repoCommit := strings.TrimSpace(f.RepoCommit)
	switch {
	case repoURL == "" && repoCommit == "":
		return nil
	case repoURL == "":
		return errors.New("--repo-commit requires --repo-url")
	default:
		return nil
	}
}

func (f repositoryOverrideFlags) revision() string {
	if revision := strings.TrimSpace(f.RepoCommit); revision != "" {
		return revision
	}
	return defaultRepositoryOverrideRevision
}

var resolveRepositoryOverrideCommit = resolveRepositoryOverrideCommitDefault

func resolveRepositoryOverrideCommitDefault(remoteURL, revision string) (string, error) {
	if commitSHA, ok := normalizeRepositoryCommitSHA(revision); ok {
		return commitSHA, nil
	}
	commitSHA, tagErr := resolveRemoteGitTagCommit(remoteURL, revision)
	if tagErr == nil {
		return commitSHA, nil
	}
	if strings.EqualFold(strings.TrimSpace(revision), defaultRepositoryOverrideRevision) {
		commitSHA, headErr := resolveRemoteGitHeadCommit(remoteURL)
		if headErr == nil {
			return commitSHA, nil
		}
		return "", errors.Join(tagErr, fmt.Errorf("resolve repository latest from remote HEAD: %w", headErr))
	}
	return "", tagErr
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

func resolveRemoteGitTagCommit(remoteURL, tag string) (string, error) {
	ref, err := repositoryTagRef(tag)
	if err != nil {
		return "", err
	}
	out, err := gitOutput("", "ls-remote", remoteURL, ref, ref+"^{}")
	if err != nil {
		return "", fmt.Errorf("resolve repository tag %q from remote: %w", strings.TrimSpace(tag), err)
	}
	commitSHA, err := selectRemoteGitTagCommit(out, ref)
	if err != nil {
		return "", err
	}
	return commitSHA, nil
}

func resolveRemoteGitHeadCommit(remoteURL string) (string, error) {
	out, err := gitOutput("", "ls-remote", "--exit-code", remoteURL, "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve repository HEAD from remote: %w", err)
	}
	commitSHA, err := selectRemoteGitRefCommit(out, "HEAD")
	if err != nil {
		return "", err
	}
	return commitSHA, nil
}

func repositoryTagRef(tag string) (string, error) {
	trimmed := strings.TrimSpace(tag)
	if trimmed == "" {
		return "", errors.New("repository commit_sha is required")
	}
	ref := "refs/tags/" + trimmed
	if strings.HasPrefix(trimmed, "refs/") {
		if !strings.HasPrefix(trimmed, "refs/tags/") {
			return "", fmt.Errorf("repository commit %q must be a full 40-character commit SHA or tag name", trimmed)
		}
		ref = trimmed
	}
	if _, err := gitOutput("", "check-ref-format", ref); err != nil {
		return "", fmt.Errorf("repository tag %q is not a valid tag name: %w", trimmed, err)
	}
	return ref, nil
}

func selectRemoteGitRefCommit(output, ref string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return "", fmt.Errorf("parse repository ref lookup: unexpected ls-remote output %q", line)
		}
		if fields[1] != ref {
			continue
		}
		if commitSHA, ok := normalizeRepositoryCommitSHA(fields[0]); ok {
			return commitSHA, nil
		}
		return "", fmt.Errorf("repository ref %q resolved to invalid commit %q", ref, fields[0])
	}
	return "", fmt.Errorf("repository ref %q was not found", ref)
}

func selectRemoteGitTagCommit(output, ref string) (string, error) {
	direct := ""
	peeled := ""
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return "", fmt.Errorf("parse repository tag lookup: unexpected ls-remote output %q", line)
		}
		switch fields[1] {
		case ref:
			direct = fields[0]
		case ref + "^{}":
			peeled = fields[0]
		}
	}
	if peeled != "" {
		if commitSHA, ok := normalizeRepositoryCommitSHA(peeled); ok {
			return commitSHA, nil
		}
		return "", fmt.Errorf("repository tag %q resolved to invalid commit %q", strings.TrimPrefix(ref, "refs/tags/"), peeled)
	}
	if direct != "" {
		if commitSHA, ok := normalizeRepositoryCommitSHA(direct); ok {
			return commitSHA, nil
		}
		return "", fmt.Errorf("repository tag %q resolved to invalid commit %q", strings.TrimPrefix(ref, "refs/tags/"), direct)
	}
	return "", fmt.Errorf("repository tag %q was not found", strings.TrimPrefix(ref, "refs/tags/"))
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
	return resolveRepositoryCheckoutFromConfig(cwd, loader, repository, true)
}

func resolveWorkspaceCopyRepositoryCheckout(cwd string, loader policyLoader) (*resolvedRepositoryCheckout, error) {
	repository, err := resolveRepositoryCheckout(cwd, loader)
	if err != nil || repository != nil {
		return repository, err
	}
	remoteName, err := resolveImplicitRepositoryRemote(cwd)
	if err != nil {
		if errors.Is(err, errSkipRepositoryCheckout) {
			return nil, nil
		}
		return nil, err
	}
	return resolveRepositoryCheckoutFromConfig(cwd, loader, policy.RepositoryConfig{
		Implicit: true,
		Mode:     "current-repo",
		Remote:   remoteName,
		Path:     defaultRepositoryOverridePath,
	}, false)
}

func resolveImplicitRepositoryRemote(cwd string) (string, error) {
	repoRoot, err := resolveRepositoryRoot(cwd, policy.RepositoryConfig{
		Implicit: true,
		Mode:     "current-repo",
		Path:     defaultRepositoryOverridePath,
	})
	if err != nil {
		return "", err
	}

	if upstream, err := gitOutput(repoRoot, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil {
		if remote, _, ok := strings.Cut(strings.TrimSpace(upstream), "/"); ok && strings.TrimSpace(remote) != "" {
			return strings.TrimSpace(remote), nil
		}
	}

	remoteOutput, err := gitOutput(repoRoot, "remote")
	if err != nil {
		return "", fmt.Errorf("list repository remotes: %w", err)
	}
	remotes := strings.Fields(remoteOutput)
	switch len(remotes) {
	case 0:
		return "", errors.New("workspace copy-in requires a repository remote")
	case 1:
		return remotes[0], nil
	}
	for _, remote := range remotes {
		if remote == "origin" {
			return remote, nil
		}
	}
	return "", fmt.Errorf("workspace copy-in found multiple repository remotes (%s); configure repository.remote in cleanroom.yaml", strings.Join(remotes, ", "))
}

func resolveRepositoryCheckoutFromConfig(cwd string, loader policyLoader, repository policy.RepositoryConfig, validatePolicy bool) (*resolvedRepositoryCheckout, error) {
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

	if validatePolicy && loader != nil {
		compiled, _, err := loader.LoadAndCompile(cwd)
		if err != nil {
			return nil, err
		}
		if compiled != nil && !compiled.AllowsForStage(policy.NetworkStageWorkspace, remoteHost, 443) {
			return nil, fmt.Errorf("repository remote host %q is not allowed by workspace network policy", remoteHost)
		}
	}

	return &resolvedRepositoryCheckout{
		RootDir:        repoRoot,
		RemoteName:     repository.Remote,
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
