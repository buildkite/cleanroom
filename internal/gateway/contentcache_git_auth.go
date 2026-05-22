package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	ccgit "github.com/buildkite/content-cache/protocol/git"
)

type contentCacheGitBasicAuthProvider struct {
	credentials CredentialProvider
}

func newContentCacheGitBasicAuthProvider(credentials CredentialProvider) ccgit.BasicAuthProvider {
	if credentials == nil {
		return nil
	}
	return contentCacheGitBasicAuthProvider{credentials: credentials}
}

func (p contentCacheGitBasicAuthProvider) BasicAuth(ctx context.Context, repo ccgit.RepoRef) (string, string, error) {
	header, err := p.credentials.Resolve(ctx, repo.UpstreamURL())
	if err != nil {
		return "", "", fmt.Errorf("resolve upstream credentials: %w", err)
	}
	username, password, ok, err := parseBasicAuthHeader(header)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", nil
	}
	return username, password, nil
}

func parseBasicAuthHeader(header string) (string, string, bool, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", "", false, nil
	}
	scheme, encoded, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", "", false, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", "", true, fmt.Errorf("decode Basic auth header: %w", err)
	}
	username, password, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", "", true, fmt.Errorf("Basic auth header missing username/password separator")
	}
	return username, password, true, nil
}
