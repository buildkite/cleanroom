package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	ccgit "github.com/buildkite/content-cache/protocol/git"
)

const githubAppRepoPrefixesConfigField = "repo_prefixes"

// GitHubAppCredentialConfig configures host-side GitHub App credentials without
// embedding private key material.
type GitHubAppCredentialConfig struct {
	AppID          string
	InstallationID string
	PrivateKeyFile string
	RepoPrefixes   []string
}

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
		return nil, fmt.Errorf("%s must contain at least one owner/ or owner/repo prefix", githubAppRepoPrefixesConfigField)
	}
	return &GitHubAppCredentialProvider{auth: auth, repoPrefixes: normalized}, nil
}

// NewGitHubAppCredentialProviderFromConfig creates a GitHub App credential
// provider from runtime configuration. If no fields are set, it returns nil.
func NewGitHubAppCredentialProviderFromConfig(cfg GitHubAppCredentialConfig) (CredentialProvider, error) {
	appID := strings.TrimSpace(cfg.AppID)
	installationID := strings.TrimSpace(cfg.InstallationID)
	privateKeyFile := strings.TrimSpace(cfg.PrivateKeyFile)
	repoPrefixes := trimStringSlice(cfg.RepoPrefixes)

	if appID == "" && installationID == "" && privateKeyFile == "" && len(repoPrefixes) == 0 {
		return nil, nil
	}
	if appID == "" {
		return nil, errors.New("app_id is required when GitHub App credentials are configured")
	}
	if installationID == "" {
		return nil, errors.New("installation_id is required when GitHub App credentials are configured")
	}
	if privateKeyFile == "" {
		return nil, errors.New("private_key_file is required when GitHub App credentials are configured")
	}
	if len(repoPrefixes) == 0 {
		return nil, errors.New("repo_prefixes must contain at least one owner/ or owner/repo prefix")
	}

	data, err := os.ReadFile(privateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read private_key_file: %w", err)
	}
	auth, err := ccgit.NewGitHubAppAuth(ccgit.GitHubAppAuthConfig{
		AppID:          appID,
		InstallationID: installationID,
		PrivateKey:     strings.TrimSpace(string(data)),
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

func trimStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeGitHubAppRepoPrefix(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", fmt.Errorf("parse %s value %q: %w", githubAppRepoPrefixesConfigField, value, err)
		}
		if !strings.EqualFold(parsed.Scheme, "https") || !strings.EqualFold(parsed.Hostname(), "github.com") {
			return "", fmt.Errorf("%s value %q must use https://github.com", githubAppRepoPrefixesConfigField, value)
		}
		value = parsed.Path
	} else {
		value = strings.TrimPrefix(value, "github.com/")
	}

	ownerPrefix := strings.HasSuffix(value, "/")
	value = strings.Trim(value, "/")
	value = strings.TrimSuffix(value, ".git")
	if value == "" {
		return "", fmt.Errorf("%s value must identify owner/ or owner/repo", githubAppRepoPrefixesConfigField)
	}
	parts := strings.Split(value, "/")
	switch {
	case ownerPrefix && len(parts) == 1 && parts[0] != "":
		return strings.ToLower(parts[0]) + "/", nil
	case !ownerPrefix && len(parts) == 2 && parts[0] != "" && parts[1] != "":
		return strings.ToLower(parts[0] + "/" + parts[1]), nil
	default:
		return "", fmt.Errorf("%s value %q must identify owner/ or owner/repo", githubAppRepoPrefixesConfigField, value)
	}
}
