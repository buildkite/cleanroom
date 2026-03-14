package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/buildkite/cleanroom/internal/controlclient"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
)

type resolvedRepositoryCheckout struct {
	RemoteURL      string
	CommitSHA      string
	DestinationDir string
	Submodules     bool
	Branch         string
	Dirty          bool
}

func resolveRepositoryCheckout(cwd string, loader policyLoader) (*resolvedRepositoryCheckout, error) {
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
	canonicalURL, remoteHost, err := canonicalizeGitRemoteURL(remoteURL)
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
		RemoteURL:      canonicalURL,
		CommitSHA:      strings.TrimSpace(commitSHA),
		DestinationDir: repository.Path,
		Submodules:     repository.Submodules,
		Branch:         strings.TrimSpace(branch),
		Dirty:          strings.TrimSpace(dirty) != "",
	}, nil
}

func maybeResolveRepositoryCheckout(cwd string, loader policyLoader, _ string, _ bool) (*resolvedRepositoryCheckout, error) {
	return resolveRepositoryCheckout(cwd, loader)
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

func canonicalizeGitRemoteURL(raw string) (string, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", errors.New("repository remote URL is empty")
	}

	if strings.HasPrefix(trimmed, "https://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", "", fmt.Errorf("parse repository remote URL %q: %w", trimmed, err)
		}
		host := strings.TrimSpace(strings.ToLower(parsed.Hostname()))
		if host == "" {
			return "", "", fmt.Errorf("repository remote URL %q has no host", trimmed)
		}
		if parsed.Port() != "" && parsed.Port() != "443" {
			return "", "", fmt.Errorf("repository remote URL %q uses unsupported non-default HTTPS port", trimmed)
		}
		parsed.User = nil
		parsed.Host = normalizedURLHost(host, parsed.Port())
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String(), host, nil
	}

	if strings.HasPrefix(trimmed, "ssh://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", "", fmt.Errorf("parse repository remote URL %q: %w", trimmed, err)
		}
		host := strings.TrimSpace(strings.ToLower(parsed.Hostname()))
		if host == "" {
			return "", "", fmt.Errorf("repository remote URL %q has no host", trimmed)
		}
		if parsed.Port() != "" && parsed.Port() != "22" {
			return "", "", fmt.Errorf("repository remote URL %q uses unsupported non-default SSH port", trimmed)
		}
		path := strings.TrimPrefix(parsed.Path, "/")
		if path == "" {
			return "", "", fmt.Errorf("repository remote URL %q has no path", trimmed)
		}
		return "https://" + host + "/" + path, host, nil
	}

	if at := strings.Index(trimmed, "@"); at >= 0 {
		hostAndPath := trimmed[at+1:]
		host, path, ok := strings.Cut(hostAndPath, ":")
		host = strings.TrimSpace(strings.ToLower(host))
		path = strings.TrimPrefix(strings.TrimSpace(path), "/")
		if ok && host != "" && path != "" {
			return "https://" + host + "/" + path, host, nil
		}
	}

	return "", "", fmt.Errorf("repository remote URL %q must use https or a canonicalizable ssh form", trimmed)
}

func normalizedURLHost(host, port string) string {
	if strings.Contains(host, ":") {
		if port != "" {
			return "[" + host + "]:" + port
		}
		return "[" + host + "]"
	}
	if port != "" {
		return host + ":" + port
	}
	return host
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

func createTopLevelSandbox(
	client *controlclient.Client,
	loader policyLoader,
	cwd, host, backendName, imageRefOverride string,
	launchSeconds int64,
	repository *resolvedRepositoryCheckout,
) (string, *cleanroomv1.Sandbox, error) {
	compiled, _, err := loader.LoadAndCompile(cwd)
	if err != nil {
		return "", nil, err
	}
	allowLocalImageOverride, err := isLocalControlPlaneEndpoint(host)
	if err != nil {
		return "", nil, err
	}
	compiled, err = overrideCompiledPolicyImage(compiled, imageRefOverride, allowLocalImageOverride)
	if err != nil {
		return "", nil, err
	}

	createSandboxResp, err := client.CreateSandbox(context.Background(), &cleanroomv1.CreateSandboxRequest{
		Backend: backendName,
		Options: &cleanroomv1.SandboxOptions{
			LaunchSeconds: launchSeconds,
		},
		Policy:             compiled.ToProto(),
		RepositoryCheckout: repositoryCheckoutProto(repository),
	})
	if err != nil {
		return "", nil, fmt.Errorf("create sandbox: %w", err)
	}

	sandbox := createSandboxResp.GetSandbox()
	sandboxID := strings.TrimSpace(sandbox.GetSandboxId())
	if sandboxID == "" {
		return "", nil, errors.New("create sandbox: response missing sandbox id")
	}

	return sandboxID, sandbox, nil
}

func normalizePassthroughCommand(command []string) []string {
	return repositorycheckout.NormalizeCommand(command)
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

func warnDirtyRepositoryCheckout(repository *resolvedRepositoryCheckout) {
	if repository == nil || !repository.Dirty {
		return
	}
	_, _ = fmt.Fprintf(
		os.Stderr,
		"warning: repository has local modifications; sandbox will use HEAD %s and ignore local changes\n",
		repository.CommitSHA,
	)
}
