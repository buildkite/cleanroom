package gateway

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// CredentialProvider resolves an HTTP Authorization header for an upstream git
// remote URL. It returns values such as "Bearer <token>" or "Basic <base64>".
type CredentialProvider interface {
	Resolve(ctx context.Context, remoteURL string) (authorizationHeader string, err error)
}

// EnvCredentialProvider reads credentials from environment variables at
// construction time. It maps upstream hosts to tokens and the HTTP
// authorization scheme expected by that host's git smart HTTP transport:
//
//	github.com -> CLEANROOM_GITHUB_TOKEN (Basic, username "x-access-token")
//	gitlab.com -> CLEANROOM_GITLAB_TOKEN (Bearer)
//
// GitHub's git HTTPS endpoint rejects "Authorization: Bearer <token>" and
// requires HTTP Basic with the token as the password, so each host carries
// its own scheme rather than assuming Bearer everywhere.
type EnvCredentialProvider struct {
	hostCreds map[string]envHostCredential
}

type envHostCredential struct {
	token  string
	scheme envAuthScheme
}

type envAuthScheme int

const (
	envAuthSchemeBearer envAuthScheme = iota
	envAuthSchemeBasic
)

type envHostConfig struct {
	envVar        string
	scheme        envAuthScheme
	basicUsername string
}

// NewEnvCredentialProvider creates a provider from the current environment.
func NewEnvCredentialProvider() *EnvCredentialProvider {
	p := &EnvCredentialProvider{
		hostCreds: make(map[string]envHostCredential),
	}
	hostConfigs := map[string]envHostConfig{
		"github.com": {envVar: "CLEANROOM_GITHUB_TOKEN", scheme: envAuthSchemeBasic, basicUsername: "x-access-token"},
		"gitlab.com": {envVar: "CLEANROOM_GITLAB_TOKEN", scheme: envAuthSchemeBearer},
	}
	for host, cfg := range hostConfigs {
		v := strings.TrimSpace(os.Getenv(cfg.envVar))
		if v == "" {
			continue
		}
		token := v
		if cfg.scheme == envAuthSchemeBasic {
			token = base64.StdEncoding.EncodeToString([]byte(cfg.basicUsername + ":" + v))
		}
		p.hostCreds[host] = envHostCredential{token: token, scheme: cfg.scheme}
	}
	return p
}

// Resolve returns the credential for the given upstream remote URL. Returns an
// empty string if no credential is configured.
func (p *EnvCredentialProvider) Resolve(_ context.Context, remoteURL string) (string, error) {
	host, err := remoteURLHost(remoteURL)
	if err != nil {
		return "", err
	}
	cred, ok := p.hostCreds[host]
	if !ok || cred.token == "" {
		return "", nil
	}
	switch cred.scheme {
	case envAuthSchemeBasic:
		return "Basic " + cred.token, nil
	default:
		return "Bearer " + cred.token, nil
	}
}

// ConfiguredHosts returns a sorted list of upstream hosts with configured tokens.
func (p *EnvCredentialProvider) ConfiguredHosts() []string {
	hosts := make([]string, 0, len(p.hostCreds))
	for host := range p.hostCreds {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

type gitCredentialFillFunc func(dir, input string) (string, error)

// GitCredentialFillProvider resolves path-scoped credentials from the host git
// credential helpers.
type GitCredentialFillProvider struct {
	dir  string
	fill gitCredentialFillFunc
}

func NewGitCredentialFillProvider(dir string, fill gitCredentialFillFunc) *GitCredentialFillProvider {
	if fill == nil {
		fill = gitCredentialFillFromHost
	}
	return &GitCredentialFillProvider{
		dir:  dir,
		fill: fill,
	}
}

func (p *GitCredentialFillProvider) Resolve(_ context.Context, remoteURL string) (string, error) {
	lookup, err := buildGitCredentialFillLookup(remoteURL)
	if err != nil {
		return "", err
	}
	output, err := p.fill(p.dir, lookup)
	if err != nil {
		return "", nil
	}
	username, password := parseGitCredentialFillOutput(output)
	if username == "" || password == "" {
		return "", nil
	}
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return "Basic " + auth, nil
}

type chainCredentialProvider struct {
	providers []CredentialProvider
}

func NewChainCredentialProvider(providers ...CredentialProvider) CredentialProvider {
	filtered := make([]CredentialProvider, 0, len(providers))
	for _, provider := range providers {
		if provider != nil {
			filtered = append(filtered, provider)
		}
	}
	return chainCredentialProvider{providers: filtered}
}

func (p chainCredentialProvider) Resolve(ctx context.Context, remoteURL string) (string, error) {
	for _, provider := range p.providers {
		header, err := provider.Resolve(ctx, remoteURL)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(header) != "" {
			return header, nil
		}
	}
	return "", nil
}

func gitCredentialFillFromHost(dir, input string) (string, error) {
	cmd := exec.Command("git", "credential", "fill")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	// GIT_TERMINAL_PROMPT=0 prevents `git credential fill` from blocking on an
	// interactive prompt when no credential helper is configured for the host.
	// This provider runs against many hosts (OCI registries, package indexes,
	// etc.); we want it to silently return no credentials rather than block
	// the cleanroom server on stdin.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func buildGitCredentialFillLookup(remoteURL string) (string, error) {
	parsed, err := normalizeCredentialURL(remoteURL)
	if err != nil {
		return "", err
	}

	var lookup strings.Builder
	lookup.WriteString("protocol=https\n")
	lookup.WriteString("host=")
	lookup.WriteString(parsed.Host)
	lookup.WriteString("\n")
	path := strings.TrimPrefix(strings.TrimSpace(parsed.Path), "/")
	if path != "" {
		lookup.WriteString("path=")
		lookup.WriteString(path)
		lookup.WriteString("\n")
	}
	lookup.WriteString("\n")
	return lookup.String(), nil
}

func parseGitCredentialFillOutput(raw string) (string, string) {
	var username string
	var password string

	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "username":
			username = strings.TrimSpace(value)
		case "password":
			password = strings.TrimSpace(value)
		}
	}
	return username, password
}

func remoteURLHost(remoteURL string) (string, error) {
	parsed, err := normalizeCredentialURL(remoteURL)
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(parsed.Hostname())), nil
}

func normalizeCredentialURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("empty remote URL")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("parse remote URL %q: %w", raw, err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, fmt.Errorf("remote URL %q must use https", raw)
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return nil, fmt.Errorf("remote URL %q missing host", raw)
	}
	parsed.User = nil
	return parsed, nil
}
