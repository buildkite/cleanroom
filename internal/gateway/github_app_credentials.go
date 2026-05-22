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
	githubAppRepoPrefixesEnv   = "CLEANROOM_GITHUB_APP_REPO_PREFIXES"
)

// GitHubAppCredentialProvider resolves GitHub HTTPS Git credentials from a
// host-side GitHub App installation token source.
type GitHubAppCredentialProvider struct {
	auth         *ccgit.GitHubAppAuth
	repoPrefixes []string
}

// NewGitHubAppCredentialProvider creates a credential provider around a
// configured GitHub App auth source and explicit GitHub repository prefixes.
func NewGitHubAppCredentialProvider(auth *ccgit.GitHubAppAuth, repoPrefixes []string) (*GitHubAppCredentialProvider, error) {
	if auth == nil {
		return nil, nil
	}
	normalized, err := normalizeGitHubAppRepoPrefixes(repoPrefixes)
	if err != nil {
		return nil, err
	}
	if len(normalized) == 0 {
		return nil, fmt.Errorf("%s must contain at least one owner/ or owner/repo prefix", githubAppRepoPrefixesEnv)
	}
	return &GitHubAppCredentialProvider{auth: auth, repoPrefixes: normalized}, nil
}

// NewGitHubAppCredentialProviderFromEnv creates a GitHub App credential
// provider from Cleanroom's host runtime environment. If no GitHub App
// environment variables are set, it returns nil.
func NewGitHubAppCredentialProviderFromEnv() (CredentialProvider, error) {
	appID := strings.TrimSpace(os.Getenv(githubAppIDEnv))
	installationID := strings.TrimSpace(os.Getenv(githubAppInstallationIDEnv))
	privateKey := strings.TrimSpace(os.Getenv(githubAppPrivateKeyEnv))
	privateKeyFile := strings.TrimSpace(os.Getenv(githubAppPrivateKeyFileEnv))
	repoPrefixesRaw := strings.TrimSpace(os.Getenv(githubAppRepoPrefixesEnv))

	if appID == "" && installationID == "" && privateKey == "" && privateKeyFile == "" && repoPrefixesRaw == "" {
		return nil, nil
	}
	if appID == "" {
		return nil, fmt.Errorf("%s is required when GitHub App credentials are configured", githubAppIDEnv)
	}
	if installationID == "" {
		return nil, fmt.Errorf("%s is required when GitHub App credentials are configured", githubAppInstallationIDEnv)
	}
	repoPrefixes, err := parseGitHubAppRepoPrefixes(repoPrefixesRaw)
	if err != nil {
		return nil, err
	}
	if len(repoPrefixes) == 0 {
		return nil, fmt.Errorf("%s is required when GitHub App credentials are configured", githubAppRepoPrefixesEnv)
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
	return NewGitHubAppCredentialProvider(auth, repoPrefixes)
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

	repoPath, ok := githubAppRepoPath(parsed)
	if !ok {
		return "", nil
	}
	if !p.matchesRepo(repoPath) {
		return "", nil
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

func (p *GitHubAppCredentialProvider) matchesRepo(repoPath string) bool {
	repoPath = strings.ToLower(strings.TrimSpace(repoPath))
	for _, prefix := range p.repoPrefixes {
		if strings.HasSuffix(prefix, "/") {
			if strings.HasPrefix(repoPath, prefix) {
				return true
			}
			continue
		}
		if repoPath == prefix {
			return true
		}
	}
	return false
}

func githubAppRepoPath(parsed *url.URL) (string, bool) {
	path := strings.Trim(strings.TrimSpace(parsed.Path), "/")
	path = strings.TrimSuffix(path, ".git")
	owner, repo, ok := strings.Cut(path, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", false
	}
	return owner + "/" + repo, true
}

func parseGitHubAppRepoPrefixes(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	return normalizeGitHubAppRepoPrefixes(strings.Split(raw, ","))
}

func normalizeGitHubAppRepoPrefixes(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		normalized, err := normalizeGitHubAppRepoPrefix(value)
		if err != nil {
			return nil, err
		}
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out, nil
}

func normalizeGitHubAppRepoPrefix(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", fmt.Errorf("parse %s value %q: %w", githubAppRepoPrefixesEnv, value, err)
		}
		if !strings.EqualFold(parsed.Scheme, "https") || !strings.EqualFold(parsed.Hostname(), "github.com") {
			return "", fmt.Errorf("%s value %q must use https://github.com", githubAppRepoPrefixesEnv, value)
		}
		value = parsed.Path
	} else {
		value = strings.TrimPrefix(value, "github.com/")
	}

	ownerPrefix := strings.HasSuffix(value, "/")
	value = strings.Trim(value, "/")
	value = strings.TrimSuffix(value, ".git")
	if value == "" {
		return "", fmt.Errorf("%s value must identify owner/ or owner/repo", githubAppRepoPrefixesEnv)
	}
	parts := strings.Split(value, "/")
	switch {
	case ownerPrefix && len(parts) == 1 && parts[0] != "":
		return strings.ToLower(parts[0]) + "/", nil
	case !ownerPrefix && len(parts) == 2 && parts[0] != "" && parts[1] != "":
		return strings.ToLower(parts[0] + "/" + parts[1]), nil
	default:
		return "", fmt.Errorf("%s value %q must identify owner/ or owner/repo", githubAppRepoPrefixesEnv, value)
	}
}
