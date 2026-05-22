package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"

	ccgit "github.com/buildkite/content-cache/protocol/git"
)

const (
	githubAppIDEnv             = "CLEANROOM_GITHUB_APP_ID"
	githubAppInstallationIDEnv = "CLEANROOM_GITHUB_APP_INSTALLATION_ID"
	githubAppPrivateKeyEnv     = "CLEANROOM_GITHUB_APP_PRIVATE_KEY"
	githubAppPrivateKeyFileEnv = "CLEANROOM_GITHUB_APP_PRIVATE_KEY_FILE"
)

// GitHubAppCredentialProvider resolves GitHub HTTPS Git credentials from a
// host-side GitHub App installation token source.
type GitHubAppCredentialProvider struct {
	auth *ccgit.GitHubAppAuth
}

// NewGitHubAppCredentialProvider creates a credential provider around a
// configured GitHub App auth source.
func NewGitHubAppCredentialProvider(auth *ccgit.GitHubAppAuth) *GitHubAppCredentialProvider {
	if auth == nil {
		return nil
	}
	return &GitHubAppCredentialProvider{auth: auth}
}

// NewGitHubAppCredentialProviderFromEnv creates a GitHub App credential
// provider from Cleanroom's host runtime environment. If no GitHub App
// environment variables are set, it returns nil.
func NewGitHubAppCredentialProviderFromEnv() (CredentialProvider, error) {
	appID := strings.TrimSpace(os.Getenv(githubAppIDEnv))
	installationID := strings.TrimSpace(os.Getenv(githubAppInstallationIDEnv))
	privateKey := strings.TrimSpace(os.Getenv(githubAppPrivateKeyEnv))
	privateKeyFile := strings.TrimSpace(os.Getenv(githubAppPrivateKeyFileEnv))

	if appID == "" && installationID == "" && privateKey == "" && privateKeyFile == "" {
		return nil, nil
	}
	if appID == "" {
		return nil, fmt.Errorf("%s is required when GitHub App credentials are configured", githubAppIDEnv)
	}
	if installationID == "" {
		return nil, fmt.Errorf("%s is required when GitHub App credentials are configured", githubAppInstallationIDEnv)
	}
	if privateKey != "" && privateKeyFile != "" {
		return nil, fmt.Errorf("%s and %s are mutually exclusive", githubAppPrivateKeyEnv, githubAppPrivateKeyFileEnv)
	}
	if privateKey == "" {
		if privateKeyFile == "" {
			return nil, fmt.Errorf("%s or %s is required when GitHub App credentials are configured", githubAppPrivateKeyEnv, githubAppPrivateKeyFileEnv)
		}
		data, err := os.ReadFile(privateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", githubAppPrivateKeyFileEnv, err)
		}
		privateKey = strings.TrimSpace(string(data))
	}

	auth, err := ccgit.NewGitHubAppAuth(ccgit.GitHubAppAuthConfig{
		AppID:          appID,
		InstallationID: installationID,
		PrivateKey:     privateKey,
		TokenScope:     ccgit.GitHubAppTokenScopeRequestedRepo,
	})
	if err != nil {
		return nil, err
	}
	return NewGitHubAppCredentialProvider(auth), nil
}

func (p *GitHubAppCredentialProvider) Resolve(ctx context.Context, remoteURL string) (string, error) {
	if p == nil || p.auth == nil {
		return "", nil
	}

	parsed, err := normalizeCredentialURL(remoteURL)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", nil
	}

	repoPath, err := githubAppRepoPath(parsed)
	if err != nil {
		return "", err
	}
	username, password, err := p.auth.BasicAuth(ctx, ccgit.RepoRef{Host: "github.com", RepoPath: repoPath})
	if err != nil {
		return "", fmt.Errorf("resolve GitHub App credentials: %w", err)
	}
	if username == "" {
		return "", fmt.Errorf("resolve GitHub App credentials: empty username")
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return "Basic " + encoded, nil
}

func githubAppRepoPath(parsed *url.URL) (string, error) {
	path := strings.Trim(strings.TrimSpace(parsed.Path), "/")
	path = strings.TrimSuffix(path, ".git")
	owner, repo, ok := strings.Cut(path, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", fmt.Errorf("github.com remote URL %q must identify owner/repo", parsed.Redacted())
	}
	return owner + "/" + repo, nil
}
