// Package gatewayauth contains the owner and resource envelope that Cleanroom
// registers with gateway routes for a sandbox.
package gatewayauth

import (
	"fmt"
	"net/url"
	"strings"
)

// Authorization describes upstream resources a sandbox owner may access through
// privileged gateway handlers.
type Authorization struct {
	GitRepoPrefixes []string
	OCIRepoPrefixes []string
	FetchURLs       []string
}

// Owner identifies the control-plane principal that created a sandbox.
type Owner struct {
	PrincipalID string
	Scope       string
}

// ScopeMetadata is attached to a registered sandbox gateway scope.
type ScopeMetadata struct {
	Owner         Owner
	Authorization Authorization
}

// Clone returns a detached copy of scope metadata.
func (m ScopeMetadata) Clone() ScopeMetadata {
	return ScopeMetadata{
		Owner: m.Owner,
		Authorization: Authorization{
			GitRepoPrefixes: append([]string(nil), m.Authorization.GitRepoPrefixes...),
			OCIRepoPrefixes: append([]string(nil), m.Authorization.OCIRepoPrefixes...),
			FetchURLs:       append([]string(nil), m.Authorization.FetchURLs...),
		},
	}
}

// HasOwner reports whether the scope came from an authenticated resource owner.
func (m ScopeMetadata) HasOwner() bool {
	return strings.TrimSpace(m.Owner.PrincipalID) != ""
}

// GitRepoPrefixFromURL returns the normalized host/path key used to authorize
// Git gateway requests.
func GitRepoPrefixFromURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse git remote URL: %w", err)
	}
	host := strings.TrimSpace(strings.ToLower(parsed.Hostname()))
	if host == "" {
		return "", fmt.Errorf("git remote URL has no host")
	}
	if parsed.Port() != "" {
		host = host + ":" + parsed.Port()
	}
	return NormalizeGitRepoPrefix(host + "/" + strings.TrimPrefix(parsed.Path, "/")), nil
}

// GitRepoKeyFromRequest returns the normalized host/path key for a gateway Git
// request after the operation suffix has been removed.
func GitRepoKeyFromRequest(upstreamHost, repoPath string) (string, error) {
	repositoryPath := repoPath
	switch {
	case strings.HasSuffix(repositoryPath, "/info/refs"):
		repositoryPath = strings.TrimSuffix(repositoryPath, "/info/refs")
	case strings.HasSuffix(repositoryPath, "/git-upload-pack"):
		repositoryPath = strings.TrimSuffix(repositoryPath, "/git-upload-pack")
	case strings.HasSuffix(repositoryPath, "/git-receive-pack"):
		repositoryPath = strings.TrimSuffix(repositoryPath, "/git-receive-pack")
	default:
		return "", fmt.Errorf("unsupported git request path %q", repoPath)
	}
	return NormalizeGitRepoPrefix(strings.TrimSpace(upstreamHost) + "/" + strings.TrimPrefix(repositoryPath, "/")), nil
}

// NormalizeGitRepoPrefix normalizes host/path Git repository keys. Repository
// suffixes are stored without .git so remotes with and without the suffix match.
func NormalizeGitRepoPrefix(value string) string {
	normalized := strings.Trim(strings.ToLower(strings.TrimSpace(value)), "/")
	if strings.HasSuffix(normalized, ".git") {
		normalized = strings.TrimSuffix(normalized, ".git")
	}
	return normalized
}

// AllowsGitRepo reports whether a Git repo key is covered by the authorization
// envelope.
func AllowsGitRepo(prefixes []string, repoKey string) bool {
	return allowsPrefix(prefixes, NormalizeGitRepoPrefix(repoKey), NormalizeGitRepoPrefix)
}

// OCIRepoPrefixFromImageRef returns the normalized registry/repository key from
// an OCI image reference, ignoring tags and digests.
func OCIRepoPrefixFromImageRef(imageRef string) (string, error) {
	repo := strings.TrimSpace(imageRef)
	if repo == "" {
		return "", fmt.Errorf("image reference is empty")
	}
	if beforeDigest, _, ok := strings.Cut(repo, "@"); ok {
		repo = beforeDigest
	}
	lastSlash := strings.LastIndex(repo, "/")
	if colon := strings.LastIndex(repo, ":"); colon > lastSlash {
		repo = repo[:colon]
	}
	return NormalizeOCIRepoPrefix(repo), nil
}

// OCIRepoKeyFromPath returns the registry/repository key from a gateway OCI
// request path. Bare /v2/ version probes return ok=false because they do not
// identify a repository.
func OCIRepoKeyFromPath(prefix, rest string) (repoKey string, ok bool, err error) {
	prefix = strings.Trim(strings.ToLower(strings.TrimSpace(prefix)), "/")
	rest = strings.Trim(strings.TrimSpace(rest), "/")
	rest = strings.TrimPrefix(rest, "v2/")
	rest = strings.Trim(rest, "/")
	if rest == "" || rest == "v2" {
		return "", false, nil
	}
	parts := strings.Split(rest, "/")
	for i := len(parts) - 2; i >= 1; i-- {
		switch parts[i] {
		case "manifests", "blobs", "referrers":
			return NormalizeOCIRepoPrefix(prefix + "/" + strings.Join(parts[:i], "/")), true, nil
		}
	}
	for i := len(parts) - 2; i >= 1; i-- {
		if parts[i] == "tags" && parts[i+1] == "list" {
			return NormalizeOCIRepoPrefix(prefix + "/" + strings.Join(parts[:i], "/")), true, nil
		}
	}
	return "", false, fmt.Errorf("missing OCI repository in request path")
}

// NormalizeOCIRepoPrefix normalizes an OCI registry/repository key. References
// without an explicit registry use Docker Hub's canonical repository shape.
func NormalizeOCIRepoPrefix(value string) string {
	repo := strings.Trim(strings.ToLower(strings.TrimSpace(value)), "/")
	if repo == "" {
		return ""
	}
	parts := strings.Split(repo, "/")
	first := parts[0]
	hasRegistry := strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost"
	if !hasRegistry {
		if len(parts) == 1 {
			return "docker.io/library/" + repo
		}
		return "docker.io/" + repo
	}
	if first == "index.docker.io" || first == "registry-1.docker.io" {
		parts[0] = "docker.io"
		return strings.Join(parts, "/")
	}
	return repo
}

// AllowsOCIRepo reports whether an OCI repo key is covered by the authorization
// envelope.
func AllowsOCIRepo(prefixes []string, repoKey string) bool {
	return allowsPrefix(prefixes, NormalizeOCIRepoPrefix(repoKey), NormalizeOCIRepoPrefix)
}

func allowsPrefix(prefixes []string, key string, normalize func(string) string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	for _, prefix := range prefixes {
		trimmed := strings.TrimSpace(prefix)
		prefixMatch := strings.HasSuffix(trimmed, "/")
		normalized := normalize(trimmed)
		if normalized == "" {
			continue
		}
		if key == normalized {
			return true
		}
		if prefixMatch && strings.HasPrefix(key, normalized+"/") {
			return true
		}
	}
	return false
}
